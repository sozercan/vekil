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
)

func TestCopilotChatStreamThrottleCreatesCooldown(t *testing.T) {
	for _, tc := range []struct {
		name      string
		code      string
		errorType string
	}{
		{"typed model throttle", "user_model_rate_limited", "rate_limit_error"},
		{"code-only model throttle", "user_model_rate_limited", ""},
		{"code-only global throttle", "user_global_rate_limited", ""},
		{"code-only weekly throttle", "user_weekly_rate_limited", ""},
		{"code-only integration throttle", "integration_rate_limited", ""},
	} {
		for _, streaming := range []bool{true, false} {
			t.Run(fmt.Sprintf("%s/client_stream=%t", tc.name, streaming), func(t *testing.T) {
				upstreamError := map[string]string{"code": tc.code, "message": "upstream reset"}
				if tc.errorType != "" {
					upstreamError["type"] = tc.errorType
				}
				errorBody, err := json.Marshal(map[string]any{"error": upstreamError})
				if err != nil {
					t.Fatal(err)
				}
				var sends atomic.Int32
				h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
					sends.Add(1)
					var request struct {
						Stream bool `json:"stream"`
					}
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
					}
					if !request.Stream {
						t.Error("Chat request did not stream upstream")
					}
					w.Header().Set("Content-Type", "text/event-stream")
					w.Header().Set("Retry-After", "86400")
					_, _ = fmt.Fprintf(w, "event: error\ndata: %s\n\n", errorBody)
				})
				h.stats = newStatsCollector()
				now := time.Now()
				h.copilotTraffic.now = func() time.Time { return now }
				body := fmt.Sprintf(`{"model":"chat-model","stream":%t,"messages":[{"role":"user","content":"lookup"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]}`, streaming)
				first := httptest.NewRecorder()
				h.HandleOpenAIChatCompletions(first, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
				wantStatus := http.StatusTooManyRequests
				if streaming {
					wantStatus = http.StatusOK
				}
				if first.Code != wantStatus || !strings.Contains(first.Body.String(), tc.code) {
					t.Fatalf("initial streamed error was lost: %d %s", first.Code, first.Body.String())
				}
				second := httptest.NewRecorder()
				h.HandleOpenAIChatCompletions(second, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
				if sends.Load() != 1 || second.Code != http.StatusTooManyRequests || second.Header().Get("Retry-After") != "86400" || !strings.Contains(second.Body.String(), "rate limit is still active") {
					t.Fatalf("stream throttle did not create shared cooldown: sends=%d status=%d headers=%v body=%s", sends.Load(), second.Code, second.Header(), second.Body.String())
				}
				usage := h.stats.taskUsage.snapshot()
				if usage.Inflight != 0 || usage.Totals.Sends != 1 || usage.Totals.Completed != 1 || usage.Totals.Throttled != 1 {
					t.Fatalf("local stream cooldown changed physical-send accounting: %+v", usage)
				}
			})
		}
	}
}

func TestCopilotChatStreamProbeRenewsBeforeRelease(t *testing.T) {
	for _, aggregate := range []bool{false, true} {
		t.Run(fmt.Sprintf("aggregate=%t", aggregate), func(t *testing.T) {
			h := &ProxyHandler{}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			h.copilotTraffic.now = func() time.Time { return time.Unix(0, clock.Load()) }
			req := copilotTrafficTestRequest(t, ctx, "copilot", "http://upstream.example", "credential", "editor", "model", 0)
			metadata := copilotTrafficTestMetadata(t, req)
			h.copilotTraffic.observeThrottle(metadata, http.StatusTooManyRequests, "10", []byte(`{"error":{"code":"user_model_rate_limited"}}`))
			clock.Add(int64(11 * time.Second))
			probe, blocked, err := h.acquireCopilotInference(req)
			if err != nil || probe == nil || blocked != nil {
				t.Fatalf("recovery probe = %v, response=%v error=%v", probe, blocked, err)
			}
			defer probe.release()
			type acquireResult struct {
				response *http.Response
				err      error
			}
			const concurrent = 8
			results := make(chan acquireResult, concurrent)
			for range concurrent {
				go func() {
					permit, response, acquireErr := h.acquireCopilotInference(req)
					permit.release()
					results <- acquireResult{response, acquireErr}
				}()
			}
			waitForCopilotTrafficWaiters(t, h, concurrent)
			reader, writer := io.Pipe()
			defer func() { _ = writer.Close(); _ = reader.Close() }()
			closed := make(chan bool, 1)
			resp := routeExecutorTestResponse(req, http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}, "Retry-After": {"30"}}, "")
			resp.Body = &copilotProbeCloseObserver{ReadCloser: reader, controller: &h.copilotTraffic, key: metadata.keys[0], observed: closed}
			h.finishCopilotInference(req, resp, nil, probe)
			defer func() { _ = resp.Body.Close() }()
			consumed := make(chan error, 1)
			go func() {
				var consumeErr error
				if aggregate {
					_, consumeErr = aggregateStreamToResponse(resp.Body)
				} else {
					_, consumeErr = consumeOpenAIStreamChunks(resp.Body, nil)
				}
				_ = resp.Body.Close()
				consumed <- consumeErr
			}()
			if _, err := io.WriteString(writer, "event: error\ndata: "+`{"error":{"type":"rate_limit_error","code":"user_model_rate_limited","message":"model reset"}}`+"\n\n"); err != nil {
				t.Fatal(err)
			}
			_ = writer.Close()
			select {
			case renewed := <-closed:
				if !renewed {
					t.Fatal("Chat response cleanup began before the renewed cooldown was installed")
				}
			case <-ctx.Done():
				t.Fatal("Chat throttle did not close the probe response")
			}
			if err := <-consumed; err == nil {
				t.Fatal("Chat consumer lost the stream failure")
			} else {
				var streamErr *openAIStreamError
				if !errors.As(err, &streamErr) || streamErr.Code != "user_model_rate_limited" {
					t.Fatalf("Chat stream error changed: %v", err)
				}
			}
			for range concurrent {
				select {
				case result := <-results:
					if result.err != nil || result.response == nil || result.response.StatusCode != http.StatusTooManyRequests || result.response.Header.Get("Retry-After") != "30" {
						t.Fatalf("queued request escaped renewed Chat cooldown: %+v", result)
					}
					_ = result.response.Body.Close()
				case <-ctx.Done():
					t.Fatal("queued request did not resume after the Chat probe finished")
				}
			}
			waitForCopilotTrafficWaiters(t, h, 0)
		})
	}
}

type copilotChatCloseUnblocksBody struct {
	body        string
	readStarted chan struct{}
	closed      chan struct{}
	readOnce    sync.Once
	closeOnce   sync.Once
}

func (b *copilotChatCloseUnblocksBody) Read(p []byte) (int, error) {
	b.readOnce.Do(func() { close(b.readStarted) })
	<-b.closed
	return copy(p, b.body), io.ErrClosedPipe
}

func (b *copilotChatCloseUnblocksBody) Close() error {
	b.closeOnce.Do(func() { close(b.closed) })
	return nil
}

func TestCopilotChatStreamCloseJoinsPendingRead(t *testing.T) {
	for _, throttle := range []bool{false, true} {
		t.Run(fmt.Sprintf("throttle=%t", throttle), func(t *testing.T) {
			h := &ProxyHandler{}
			WithCopilotLargeRequestConcurrency(1, 1)(h)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			var clock atomic.Int64
			clock.Store(time.Now().UnixNano())
			h.copilotTraffic.now = func() time.Time { return time.Unix(0, clock.Load()) }
			req := copilotTrafficTestRequest(t, ctx, "copilot", "http://upstream.example", "credential", "editor", "model", 0)
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
				inner.body = "data: " + `{"error":{"code":"user_model_rate_limited"}}` + "\n\n"
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
				t.Fatal("stream read did not start")
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
				t.Fatal("closing a Chat stream did not unblock its pending read")
			}
			if err := <-readFinished; !errors.Is(err, io.ErrClosedPipe) {
				t.Fatalf("stream read error changed: %v", err)
			}
			select {
			case result := <-queued:
				if result.err != nil || (result.response != nil) != throttle {
					t.Fatalf("queued outcome = %+v, want throttle=%t", result, throttle)
				}
				if result.response != nil {
					if result.response.StatusCode != http.StatusTooManyRequests || result.response.Header.Get("Retry-After") != "30" {
						t.Fatalf("queued request escaped renewed reset: %v", result.response)
					}
					_ = result.response.Body.Close()
				}
			case <-ctx.Done():
				t.Fatal("stream close did not release its queued request")
			}
			waitForCopilotTrafficWaiters(t, h, 0)
			if len(h.copilotTraffic.groups) != 0 {
				t.Fatal("closed stream retained admission state")
			}
		})
	}
}

type copilotChatThrottleChunkReader struct {
	io.Reader
	chunk int
}

func (r *copilotChatThrottleChunkReader) Read(p []byte) (int, error) {
	return r.Reader.Read(p[:min(len(p), r.chunk)])
}

func TestCopilotChatStreamThrottleRequiresStructuredBoundedEvidence(t *testing.T) {
	throttle := `{"error":{"type":"rate_limit_error","code":"user_model_rate_limited","message":"model reset"}}`
	frame := "event: error\ndata: " + throttle + "\n\n"
	generated, err := json.Marshal(map[string]any{"choices": []any{map[string]any{"delta": map[string]string{"content": frame}}}})
	if err != nil {
		t.Fatal(err)
	}
	largeOutput := "data: " + `{"choices":[{"delta":{"content":"` + strings.Repeat("x", maxCopilotThrottleEvidenceBytes+64) + `"}}]}` + "\n\n"
	for _, tc := range []struct {
		name         string
		body         string
		reset        string
		wantCooldown bool
	}{
		{"nested error", frame, "60", true},
		{"flat error", "event: error\ndata: " + `{"code":"user_model_rate_limited","message":"model reset"}` + "\n\n", "60", true},
		{"error after oversized output", largeOutput + frame, "60", true},
		{"multi-line data", "event: error\ndata: {\ndata: \"error\":{\"code\":\"user_model_rate_limited\"}}\n\n", "60", true},
		{"CRLF data", strings.ReplaceAll(frame, "\n", "\r\n"), "60", true},
		{"CR data", strings.ReplaceAll(frame, "\n", "\r"), "60", true},
		{"EOF event", strings.TrimSuffix(frame, "\n\n"), "60", true},
		{"generated text", "data: " + string(generated) + "\n\ndata: [DONE]\n\n", "60", false},
		{"nested output error", "data: " + `{"choices":[{"delta":{"error":{"code":"user_model_rate_limited"}}}]}` + "\n\n", "60", false},
		{"plain error text", "event: error\ndata: user_model_rate_limited\n\n", "60", false},
		{"duplicate code", "event: error\ndata: " + `{"error":{"code":"user_model_rate_limited","code":"user_global_rate_limited"}}` + "\n\n", "60", false},
		{"conflicting code", "event: error\ndata: " + `{"code":"user_global_rate_limited","error":{"code":"user_model_rate_limited"}}` + "\n\n", "60", false},
		{"unselected root code", "event: error\ndata: " + `{"code":"user_model_rate_limited","error":{"type":"server_error"}}` + "\n\n", "60", false},
		{"unknown code", strings.ReplaceAll(frame, "user_model_rate_limited", "unknown_limit"), "60", false},
		{"invalid trailing JSON", "event: error\ndata: " + throttle + "{}\n\n", "60", false},
		{"oversized line", "event: error\ndata: " + throttle + strings.Repeat(" ", maxCopilotThrottleEvidenceBytes) + "\n\n", "60", false},
		{"oversized event", strings.Repeat(": padding\n", maxCopilotThrottleEvidenceBytes/5) + frame, "60", false},
		{"after terminal success", "data: [DONE]\n\n" + frame, "60", false},
		{"no reset", frame, "", false},
		{"invalid reset", frame, "tomorrow", false},
	} {
		for _, chunk := range []int{1, 17, 4096} {
			t.Run(fmt.Sprintf("%s/chunk=%d", tc.name, chunk), func(t *testing.T) {
				h := &ProxyHandler{}
				req := copilotTrafficTestRequest(t, context.Background(), "copilot", "http://upstream.example", "credential", "editor", "model", 0)
				resp := routeExecutorTestResponse(req, http.StatusOK, http.Header{"Content-Type": {"text/event-stream; charset=utf-8"}, "Retry-After": {tc.reset}}, "")
				resp.Body = io.NopCloser(&copilotChatThrottleChunkReader{Reader: strings.NewReader(tc.body), chunk: chunk})
				h.finishCopilotInference(req, resp, nil, nil)
				body, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil || string(body) != tc.body {
					t.Fatalf("throttle observation changed response bytes: error=%v body=%s", readErr, body)
				}
				permit, blocked, acquireErr := h.acquireCopilotInference(req)
				permit.release()
				if acquireErr != nil || (blocked != nil) != tc.wantCooldown {
					t.Fatalf("structured Chat cooldown = %v error=%v, want %v", blocked != nil, acquireErr, tc.wantCooldown)
				}
				if blocked != nil {
					_ = blocked.Body.Close()
				}
			})
		}
	}
}
