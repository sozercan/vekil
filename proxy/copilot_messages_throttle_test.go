package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func newCopilotMessagesThrottleHandler(t *testing.T, messages http.HandlerFunc) *ProxyHandler {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api" + providerEndpointModels:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"data":[{"id":"messages-model","supported_endpoints":["/v1/messages"]}]}`)
		case "/api" + providerEndpointMessages:
			messages(w, r)
		default:
			t.Errorf("unexpected upstream request: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(upstream.Close)
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithCopilotBaseURL(upstream.URL+"/api"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	return h
}

func recordCopilotMessagesThrottleRequest(ctx context.Context, h *ProxyHandler) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{"model":"messages-model","stream":true,"max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	h.HandleAnthropicMessages(w, req)
	return w
}

func TestCopilotMessagesStreamThrottleCreatesCooldown(t *testing.T) {
	for _, code := range []string{"user_model_rate_limited", "user_global_rate_limited", "user_weekly_rate_limited", "integration_rate_limited"} {
		t.Run(code, func(t *testing.T) {
			var sends atomic.Int32
			frame := fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"rate_limit_error\",\"code\":%q,\"message\":\"upstream reset\"}}\n\n", code)
			h := newCopilotMessagesThrottleHandler(t, func(w http.ResponseWriter, r *http.Request) {
				sends.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Retry-After", "86400")
				_, _ = io.WriteString(w, frame)
			})
			now := time.Now()
			h.copilotTraffic.now = func() time.Time { return now }
			first := recordCopilotMessagesThrottleRequest(context.Background(), h)
			if first.Code != http.StatusOK || first.Body.String() != frame {
				t.Fatalf("native Messages stream changed: status=%d body=%s", first.Code, first.Body.String())
			}
			second := recordCopilotMessagesThrottleRequest(context.Background(), h)
			if sends.Load() != 1 || second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") != "86400" || !strings.Contains(second.Body.String(), "rate limit is still active") {
				t.Fatalf("native Messages throttle did not create a shared cooldown: sends=%d status=%d headers=%v body=%s", sends.Load(), second.Code, second.Header(), second.Body.String())
			}
			usage := h.stats.taskUsage.snapshot()
			if usage.Inflight != 0 || usage.Totals.Sends != 1 || usage.Totals.Completed != 1 || usage.Totals.Throttled != 1 {
				t.Fatalf("local Messages cooldown changed physical-send accounting: %+v", usage)
			}
		})
	}
}

func TestCopilotMessagesStreamCodeOnlyThrottleAccounting(t *testing.T) {
	for _, code := range []string{"user_model_rate_limited", "user_global_rate_limited", "user_weekly_rate_limited", "integration_rate_limited"} {
		t.Run(code, func(t *testing.T) {
			var sends atomic.Int32
			frame := fmt.Sprintf("event: error\ndata: {\"type\":\"error\",\"error\":{\"code\":%q,\"message\":\"upstream reset\"}}\n\n", code)
			h := newCopilotMessagesThrottleHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				sends.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("Retry-After", "86400")
				_, _ = io.WriteString(w, frame)
			})
			ctx, summary := WithRequestSummary(context.Background())
			response := recordCopilotMessagesThrottleRequest(ctx, h)
			if response.Code != http.StatusOK || response.Body.String() != frame {
				t.Fatalf("code-only throttle changed native output: %d %s", response.Code, response.Body.String())
			}
			if got := summary.FailureStatus(); got != http.StatusTooManyRequests {
				t.Errorf("recorded stream failure = %d, want 429", got)
			}
			usage := h.stats.taskUsage.snapshot()
			if usage.Inflight != 0 || usage.Totals.Sends != 1 || usage.Totals.Completed != 1 || usage.Totals.Throttled != 1 {
				t.Errorf("code-only throttle accounting = %+v", usage)
			}
			blocked := recordCopilotMessagesThrottleRequest(context.Background(), h)
			if sends.Load() != 1 || blocked.Code != http.StatusTooManyRequests {
				t.Errorf("code-only cooldown sent more work: sends=%d status=%d", sends.Load(), blocked.Code)
			}
		})
	}
}

func TestCopilotMessagesStreamProbeRenewsBeforeRelease(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	const frame = "event: error\ndata: " + `{"type":"error","error":{"type":"rate_limit_error","code":"user_model_rate_limited","message":"upstream reset"}}` + "\n\n"
	var sends atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	finishProbe := func() { releaseOnce.Do(func() { close(release) }) }
	defer finishProbe()
	h := newCopilotMessagesThrottleHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		switch sends.Add(1) {
		case 1:
			w.Header().Set("Retry-After", "10")
			_, _ = io.WriteString(w, frame)
		case 2:
			w.Header().Set("Retry-After", "30")
			_, _ = io.WriteString(w, "event: message_start\ndata: "+`{"type":"message_start","message":{"id":"msg-probe","type":"message","role":"assistant","model":"messages-model","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}`+"\n\n")
			w.(http.Flusher).Flush()
			close(started)
			select {
			case <-release:
				_, _ = io.WriteString(w, "event: content_block_start\ndata: "+`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`+"\n\nevent: content_block_stop\ndata: "+`{"type":"content_block_stop","index":0}`+"\n\n"+frame)
			case <-r.Context().Done():
			}
		default:
			http.Error(w, "unexpected dispatch during renewed reset", http.StatusInternalServerError)
		}
	})
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	h.copilotTraffic.now = func() time.Time { return time.Unix(0, clock.Load()) }
	closed := make(chan bool, 1)
	transport := h.client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	h.client.Transport = retryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp, err := transport.RoundTrip(req)
		if err == nil && resp.StatusCode == http.StatusOK && resp.Header.Get("Retry-After") == "30" {
			metadata := copilotTrafficTestMetadata(t, req)
			resp.Body = &copilotProbeCloseObserver{ReadCloser: resp.Body, controller: &h.copilotTraffic, key: metadata.keys[0], observed: closed}
		}
		return resp, err
	})
	first := recordCopilotMessagesThrottleRequest(ctx, h)
	if first.Code != http.StatusOK || first.Body.String() != frame {
		t.Fatalf("initial Messages error = %d %s", first.Code, first.Body.String())
	}
	clock.Add(int64(11 * time.Second))
	probe := make(chan *httptest.ResponseRecorder, 1)
	go func() { probe <- recordCopilotMessagesThrottleRequest(ctx, h) }()
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("native Messages recovery probe did not start")
	}
	const concurrent = 8
	waiters := make(chan *httptest.ResponseRecorder, concurrent)
	for range concurrent {
		go func() { waiters <- recordCopilotMessagesThrottleRequest(ctx, h) }()
	}
	waitForCopilotTrafficWaiters(t, h, concurrent)
	finishProbe()
	select {
	case renewed := <-closed:
		if !renewed {
			t.Fatal("native Messages response cleanup began before the renewed cooldown was installed")
		}
	case <-ctx.Done():
		t.Fatal("native Messages throttle did not close its recovery response")
	}
	select {
	case response := <-probe:
		if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), frame) {
			t.Fatalf("native Messages recovery error changed: %d %s", response.Code, response.Body.String())
		}
	case <-ctx.Done():
		t.Fatal("native Messages recovery probe did not finish")
	}
	for range concurrent {
		select {
		case response := <-waiters:
			if response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") != "30" || !strings.Contains(response.Body.String(), "rate limit is still active") {
				t.Fatalf("queued request escaped renewed Messages cooldown: %d %v %s", response.Code, response.Header(), response.Body.String())
			}
		case <-ctx.Done():
			t.Fatal("queued request did not resume after native Messages recovery")
		}
	}
	waitForCopilotTrafficWaiters(t, h, 0)
	if sends.Load() != 2 || h.stats.taskUsage.snapshot().Totals.Sends != 2 {
		t.Fatalf("native Messages recovery dispatched extra work: sends=%d task=%+v", sends.Load(), h.stats.taskUsage.snapshot())
	}
}

func copilotMessagesTrafficTestRequest(t *testing.T, ctx context.Context) *http.Request {
	t.Helper()
	const body = `{"model":"messages-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://upstream.example"+providerEndpointMessages, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer credential")
	req.Header.Set("Copilot-Integration-ID", "editor")
	return withCopilotInferenceRequest(req, &providerRuntime{id: "copilot", kind: providerTypeCopilot}, providerEndpointMessages, []byte(body))
}

func TestCopilotMessagesStreamThrottleRequiresStructuredBoundedEvidence(t *testing.T) {
	const throttle = `{"type":"error","error":{"type":"rate_limit_error","code":"user_model_rate_limited","message":"upstream reset"}}`
	const frame = "event: error\ndata: " + throttle + "\n\n"
	const stop = "event: message_stop\ndata: " + `{"type":"message_stop"}` + "\n\n"
	generated, err := json.Marshal(map[string]any{"type": "content_block_delta", "delta": map[string]string{"type": "text_delta", "text": frame}})
	if err != nil {
		t.Fatal(err)
	}
	largeOutput := "data: " + `{"type":"content_block_delta","delta":{"type":"text_delta","text":"` + strings.Repeat("x", maxCopilotThrottleEvidenceBytes+64) + `"}}` + "\n\n"
	reset := http.Header{"Retry-After": {"60"}}
	for _, tc := range []struct {
		name         string
		body         string
		headers      http.Header
		wantCooldown bool
	}{
		{"native error", frame, reset, true},
		{"payload event type", "data: " + throttle + "\n\n", reset, true},
		{"SSE event type and code only", "event: error\ndata: " + `{"error":{"code":"user_model_rate_limited"}}` + "\n\n", reset, true},
		{"content block ending", "data: " + `{"type":"content_block_stop","index":0}` + "\n\n" + frame, reset, true},
		{"message delta", "data: " + `{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}` + "\n\n" + frame, reset, true},
		{"after oversized output", largeOutput + frame, reset, true},
		{"multi-line data", "event: error\ndata: {\ndata: \"type\":\"error\",\ndata: \"error\":{\"code\":\"user_model_rate_limited\"}}\n\n", reset, true},
		{"CRLF data", strings.ReplaceAll(frame, "\n", "\r\n"), reset, true},
		{"CR data", strings.ReplaceAll(frame, "\n", "\r"), reset, true},
		{"EOF event", strings.TrimSuffix(frame, "\n\n"), reset, true},
		{"millisecond reset", frame, http.Header{"Retry-After-Ms": {"60000"}}, true},
		{"exhausted token reset", frame, http.Header{"X-Ratelimit-Remaining-Tokens": {"0"}, "X-Ratelimit-Reset-Tokens": {"60s"}}, true},
		{"generated text", "data: " + string(generated) + "\n\n" + stop, reset, false},
		{"nested output error", "data: " + `{"type":"content_block_delta","delta":{"type":"text_delta","error":{"code":"user_model_rate_limited"}}}` + "\n\n", reset, false},
		{"non-error native type", "event: error\ndata: " + `{"type":"content_block_delta","error":{"code":"user_model_rate_limited"}}` + "\n\n", reset, false},
		{"missing nested envelope", "event: error\ndata: " + `{"type":"error","code":"user_model_rate_limited"}` + "\n\n", reset, false},
		{"unselected root code", "event: error\ndata: " + `{"type":"error","code":"user_model_rate_limited","error":{"type":"api_error"}}` + "\n\n", reset, false},
		{"plain error text", "event: error\ndata: user_model_rate_limited\n\n", reset, false},
		{"duplicate code", "event: error\ndata: " + `{"type":"error","error":{"code":"user_model_rate_limited","code":"user_global_rate_limited"}}` + "\n\n", reset, false},
		{"duplicate type", "event: error\ndata: " + `{"type":"content_block_delta","type":"error","error":{"code":"user_model_rate_limited"}}` + "\n\n", reset, false},
		{"conflicting code", "event: error\ndata: " + `{"type":"error","code":"user_global_rate_limited","error":{"code":"user_model_rate_limited"}}` + "\n\n", reset, false},
		{"unknown code", strings.ReplaceAll(frame, "user_model_rate_limited", "unknown_limit"), reset, false},
		{"invalid trailing JSON", "event: error\ndata: " + throttle + "{}\n\n", reset, false},
		{"truncated JSON", "event: error\ndata: " + strings.TrimSuffix(throttle, "}"), reset, false},
		{"oversized line", "event: error\ndata: " + throttle + strings.Repeat(" ", maxCopilotThrottleEvidenceBytes) + "\n\n", reset, false},
		{"oversized event", strings.Repeat(": padding\n", maxCopilotThrottleEvidenceBytes/5) + frame, reset, false},
		{"after message stop", stop + frame, reset, false},
		{"malformed message stop", "data: " + `{"type":"ping","type":"message_stop"}` + "\n\n" + frame, reset, true},
		{"no reset", frame, nil, false},
		{"invalid reset", frame, http.Header{"Retry-After": {"tomorrow"}}, false},
		{"unexhausted reset", frame, http.Header{"X-Ratelimit-Remaining-Tokens": {"1"}, "X-Ratelimit-Reset-Tokens": {"60s"}}, false},
	} {
		for _, chunk := range []int{1, 17, 4096} {
			t.Run(fmt.Sprintf("%s/chunk=%d", tc.name, chunk), func(t *testing.T) {
				h := &ProxyHandler{}
				req := copilotMessagesTrafficTestRequest(t, context.Background())
				resp := routeExecutorTestResponse(req, http.StatusOK, tc.headers.Clone(), "")
				if resp.Header == nil {
					resp.Header = make(http.Header)
				}
				resp.Header.Set("Content-Type", "text/event-stream; charset=utf-8")
				resp.Body = io.NopCloser(&copilotChatThrottleChunkReader{Reader: strings.NewReader(tc.body), chunk: chunk})
				h.finishCopilotInference(req, resp, nil, nil)
				body, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil || string(body) != tc.body {
					t.Fatalf("Messages throttle observation changed bytes: error=%v body=%s", readErr, body)
				}
				permit, blocked, acquireErr := h.acquireCopilotInference(req)
				permit.release()
				if acquireErr != nil || (blocked != nil) != tc.wantCooldown {
					t.Fatalf("structured Messages cooldown = %v error=%v, want %v", blocked != nil, acquireErr, tc.wantCooldown)
				}
				if blocked != nil {
					_ = blocked.Body.Close()
				}
			})
		}
	}
}

func TestCopilotMessagesStreamCloseJoinsPendingRead(t *testing.T) {
	for _, throttle := range []bool{false, true} {
		t.Run(fmt.Sprintf("throttle=%t", throttle), func(t *testing.T) {
			h := &ProxyHandler{}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			h.copilotTraffic.now = func() time.Time { return time.Unix(0, clock.Load()) }
			req := copilotMessagesTrafficTestRequest(t, ctx)
			metadata := copilotTrafficTestMetadata(t, req)
			h.copilotTraffic.observeThrottle(metadata, http.StatusTooManyRequests, "10", []byte(`{"error":{"code":"user_model_rate_limited"}}`))
			clock.Add(int64(11 * time.Second))
			probe, blocked, err := h.acquireCopilotInference(req)
			if err != nil || probe == nil || blocked != nil {
				t.Fatalf("recovery probe = %v, response=%v error=%v", probe, blocked, err)
			}
			defer probe.release()
			inner := &copilotChatCloseUnblocksBody{readStarted: make(chan struct{}), closed: make(chan struct{})}
			if throttle {
				inner.body = "data: " + `{"type":"error","error":{"code":"user_model_rate_limited"}}` + "\n\n"
			}
			resp := routeExecutorTestResponse(req, http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}, "Retry-After": {"30"}}, "")
			resp.Body = inner
			h.finishCopilotInference(req, resp, nil, probe)
			defer func() { _ = resp.Body.Close() }()
			readFinished := make(chan error, 1)
			go func() {
				_, readErr := io.Copy(io.Discard, resp.Body)
				readFinished <- readErr
			}()
			select {
			case <-inner.readStarted:
			case <-ctx.Done():
				t.Fatal("Messages stream read did not start")
			}
			type acquireResult struct {
				response *http.Response
				err      error
			}
			queued := make(chan acquireResult, 1)
			go func() {
				permit, response, acquireErr := h.acquireCopilotInference(req)
				permit.release()
				queued <- acquireResult{response, acquireErr}
			}()
			waitForCopilotTrafficWaiters(t, h, 1)
			closed := make(chan error, 1)
			go func() { closed <- resp.Body.Close() }()
			select {
			case err := <-closed:
				if err != nil {
					t.Fatal(err)
				}
			case <-ctx.Done():
				t.Fatal("closing a Messages stream did not unblock its pending read")
			}
			if err := <-readFinished; !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("Messages stream read error changed: %v", err)
			}
			select {
			case result := <-queued:
				if result.err != nil || (result.response != nil) != throttle {
					t.Fatalf("queued outcome = %+v, want throttle=%t", result, throttle)
				}
				if result.response != nil {
					if result.response.StatusCode != http.StatusTooManyRequests || result.response.Header.Get("Retry-After") != "30" {
						t.Fatalf("queued request escaped renewed Messages reset: %v", result.response)
					}
					_ = result.response.Body.Close()
				}
			case <-ctx.Done():
				t.Fatal("Messages stream close did not release its queued request")
			}
			waitForCopilotTrafficWaiters(t, h, 0)
			for _, cooldown := range h.copilotTraffic.cooldowns {
				if cooldown.probe != nil {
					t.Fatal("closed Messages stream retained a recovery probe")
				}
			}
		})
	}
}
