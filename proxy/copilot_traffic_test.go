package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
)

func copilotTrafficTestRequest(t *testing.T, ctx context.Context, providerID, origin, credential, integration, model string, size int) *http.Request {
	t.Helper()
	body, err := json.Marshal(map[string]any{"model": model, "messages": []map[string]string{{"role": "user", "content": strings.Repeat("x", size)}}})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+providerEndpointChatCompletions, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+credential)
	req.Header.Set("Copilot-Integration-ID", integration)
	return withCopilotInferenceRequest(req, &providerRuntime{id: providerID, kind: providerTypeCopilot}, providerEndpointChatCompletions, body)
}

func copilotTrafficTestMetadata(t *testing.T, req *http.Request) copilotInferenceRequest {
	t.Helper()
	metadata, ok := req.Context().Value(copilotInferenceRequestContextKey{}).(copilotInferenceRequest)
	if !ok {
		t.Fatal("missing Copilot inference metadata")
	}
	return metadata
}

func waitForCopilotTrafficWaiters(t *testing.T, h *ProxyHandler, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		h.copilotTraffic.mu.Lock()
		got := h.copilotTraffic.waiters
		h.copilotTraffic.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("traffic waiters did not reach %d", want)
}

func TestDoWithRetryReturnsLongResetResponseWithoutWaiting(t *testing.T) {
	for _, reset := range []string{"86400", "604800", "10000000000", "9999999999999999999999999999999", time.Now().Add(7 * 24 * time.Hour).UTC().Format(http.TimeFormat)} {
		t.Run(reset, func(t *testing.T) {
			var sends atomic.Int32
			body := newRetryBodyReadCloser(`{"error":{"code":"user_weekly_rate_limited","message":"weekly reset"}}`)
			h := &ProxyHandler{maxRetries: 3, retryBaseDelay: time.Nanosecond, client: &http.Client{Transport: retryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				sends.Add(1)
				return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {reset}, "X-Copilot-Service-Request-Id": {"original-attempt"}}, Body: body, Request: req}, nil
			})}}
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			started := time.Now()
			resp, err := h.doWithRetry(func() (*http.Request, error) {
				return http.NewRequestWithContext(ctx, http.MethodPost, "http://upstream.example/chat/completions", nil)
			})
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = resp.Body.Close() }()
			if time.Since(started) > time.Second || sends.Load() != 1 {
				t.Fatalf("long reset waited or retried: elapsed=%s sends=%d", time.Since(started), sends.Load())
			}
			if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != reset || resp.Header.Get("X-Copilot-Service-Request-Id") != "original-attempt" {
				t.Fatalf("response metadata changed: status=%d headers=%v", resp.StatusCode, resp.Header)
			}
			select {
			case <-body.closed:
				t.Fatal("upstream body was consumed before returning the response")
			default:
			}
			got, err := io.ReadAll(resp.Body)
			if err != nil || string(got) != `{"error":{"code":"user_weekly_rate_limited","message":"weekly reset"}}` {
				t.Fatalf("response body changed: %s, %v", got, err)
			}
		})
	}
}

func TestRetryDelayFitsBudget(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if !retryDelayFitsBudget(ctx, time.Second) || retryDelayFitsBudget(ctx, time.Minute) || retryDelayFitsBudget(context.Background(), 24*time.Hour) {
		t.Fatal("retry eligibility did not respect the remaining operation budget")
	}
	cancel()
	if retryDelayFitsBudget(ctx, time.Millisecond) {
		t.Fatal("canceled operation admitted a retry")
	}
}

func TestCopilotCooldownUsesAlternateResetHeaders(t *testing.T) {
	for _, tc := range []struct {
		name    string
		headers http.Header
	}{
		{"milliseconds", http.Header{"Retry-After-Ms": {"86400000"}}},
		{"token reset", http.Header{"X-Ratelimit-Remaining-Tokens": {"0"}, "X-Ratelimit-Reset-Tokens": {"24h"}}},
		{"request reset", http.Header{"X-Ratelimit-Remaining-Requests": {"0"}, "X-Ratelimit-Reset-Requests": {"86400"}}},
		{"millisecond precedence", http.Header{"Retry-After": {"1"}, "Retry-After-Ms": {"86400000"}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var sends atomic.Int32
			h := &ProxyHandler{maxRetries: 3, retryBaseDelay: time.Nanosecond, stats: newStatsCollector(), client: &http.Client{Transport: retryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				sends.Add(1)
				return routeExecutorTestResponse(req, http.StatusTooManyRequests, tc.headers.Clone(), `{"error":{"code":"user_global_rate_limited"}}`), nil
			})}}
			now := time.Now()
			h.copilotTraffic.now = func() time.Time { return now }
			ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
			defer cancel()
			request := func() (*http.Request, error) {
				return copilotTrafficTestRequest(t, ctx, "copilot", "http://upstream.example", "credential", "editor", "model", 0), nil
			}
			for attempt := range 2 {
				resp, err := h.doInferenceWithRetry(request)
				if err != nil {
					t.Fatal(err)
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusTooManyRequests {
					t.Fatalf("status = %d, want 429", resp.StatusCode)
				}
				if attempt == 1 && resp.Header.Get("Retry-After") != "86400" {
					t.Fatalf("shared reset = %q, want 86400", resp.Header.Get("Retry-After"))
				}
			}
			if sends.Load() != 1 {
				t.Fatalf("sent %d upstream requests during the same cooldown", sends.Load())
			}
			if usage := h.stats.taskUsage.snapshot(); usage.Totals.Sends != 1 || usage.Inflight != 0 {
				t.Fatalf("alternate reset changed actual-send accounting: %+v", usage)
			}
		})
	}
}

func TestCopilotCooldownScopeIsolation(t *testing.T) {
	for _, code := range []string{"user_model_rate_limited", "user_global_rate_limited", "user_weekly_rate_limited", "integration_rate_limited"} {
		t.Run(code, func(t *testing.T) {
			h := &ProxyHandler{}
			now := time.Unix(1800000000, 0)
			h.copilotTraffic.now = func() time.Time { return now }
			original := copilotTrafficTestRequest(t, context.Background(), "copilot-one", "http://upstream.example", "credential-one", "editor-one", "model-one", 0)
			h.copilotTraffic.observeThrottle(copilotTrafficTestMetadata(t, original), http.StatusTooManyRequests, "604800", []byte(`{"error":{"code":"`+code+`"}}`))
			cases := []struct {
				name, provider, origin, credential, integration, model string
				blocked                                                bool
			}{
				{"same request", "copilot-one", "http://upstream.example", "credential-one", "editor-one", "model-one", true},
				{"other model", "copilot-one", "http://upstream.example", "credential-one", "editor-one", "model-two", code != "user_model_rate_limited"},
				{"other credential", "copilot-one", "http://upstream.example", "credential-two", "editor-one", "model-one", code == "integration_rate_limited"},
				{"other integration", "copilot-one", "http://upstream.example", "credential-one", "editor-two", "model-one", code != "integration_rate_limited"},
				{"other provider", "copilot-two", "http://upstream.example", "credential-one", "editor-one", "model-one", false},
				{"other origin", "copilot-one", "http://other.example", "credential-one", "editor-one", "model-one", false},
			}
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					req := copilotTrafficTestRequest(t, context.Background(), tc.provider, tc.origin, tc.credential, tc.integration, tc.model, 0)
					permit, response, err := h.acquireCopilotInference(req)
					permit.release()
					if err != nil || (response != nil) != tc.blocked {
						t.Fatalf("blocked=%v err=%v, want blocked=%v", response != nil, err, tc.blocked)
					}
					if response != nil {
						defer func() { _ = response.Body.Close() }()
						if response.StatusCode != http.StatusTooManyRequests || response.Header.Get("Retry-After") != "604800" {
							t.Fatalf("cooldown response = %d %v", response.StatusCode, response.Header)
						}
					}
				})
			}
		})
	}
}

func TestCopilotCooldownRequiresBoundedRecognizedEvidence(t *testing.T) {
	for _, tc := range []struct {
		name, reset, body string
		status            int
	}{
		{"missing reset", "", `{"error":{"code":"user_global_rate_limited"}}`, 429},
		{"invalid reset", "tomorrow", `{"error":{"code":"user_global_rate_limited"}}`, 429},
		{"negative reset", "-1", `{"error":{"code":"user_global_rate_limited"}}`, 429},
		{"unknown code", "60", `{"error":{"code":"rate_limit_exceeded"}}`, 429},
		{"non throttle status", "60", `{"error":{"code":"user_global_rate_limited"}}`, 503},
		{"duplicate code", "60", `{"error":{"code":"user_global_rate_limited","code":"user_model_rate_limited"}}`, 429},
		{"conflicting code", "60", `{"code":"user_global_rate_limited","error":{"code":"user_model_rate_limited"}}`, 429},
		{"oversized body", "60", `{"error":{"code":"user_global_rate_limited","message":"` + strings.Repeat("x", maxCopilotThrottleEvidenceBytes) + `"}}`, 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &ProxyHandler{}
			req := copilotTrafficTestRequest(t, context.Background(), "copilot", "http://upstream.example", "credential", "editor", "model", 0)
			h.copilotTraffic.observeThrottle(copilotTrafficTestMetadata(t, req), tc.status, tc.reset, []byte(tc.body))
			permit, response, err := h.acquireCopilotInference(req)
			permit.release()
			if err != nil || response != nil || len(h.copilotTraffic.cooldowns) != 0 {
				t.Fatalf("unsupported evidence affected dispatch: response=%v error=%v entries=%d", response != nil, err, len(h.copilotTraffic.cooldowns))
			}
		})
	}
}

func TestCopilotCooldownObservationPreventsDuplicateSends(t *testing.T) {
	var sends atomic.Int32
	h := &ProxyHandler{maxRetries: 1, client: &http.Client{Transport: retryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		return routeExecutorTestResponse(req, http.StatusTooManyRequests, http.Header{"Retry-After": {"604800"}}, `{"error":{"code":"user_weekly_rate_limited"}}`), nil
	})}}
	h.stats = newStatsCollector()
	request := func() (*http.Request, error) {
		return copilotTrafficTestRequest(t, context.Background(), "copilot", "http://upstream.example", "credential", "editor", "model", 0), nil
	}
	if _, err := h.doInferenceWithRetry(request); err == nil {
		t.Fatal("first upstream 429 did not retain exhausted retry error")
	}
	for range 8 {
		resp, err := h.doInferenceWithRetry(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "604800" {
			t.Fatalf("cooldown response = %d %v", resp.StatusCode, resp.Header)
		}
	}
	if got := sends.Load(); got != 1 {
		t.Fatalf("sent %d upstream requests during the same cooldown", got)
	}
	usage := h.stats.taskUsage.snapshot()
	if usage.Inflight != 0 || usage.Totals.Sends != 1 || usage.Totals.Completed != 1 || usage.Totals.Throttled != 1 || usage.Totals.Errors != 1 {
		t.Fatalf("local cooldown responses changed actual-send accounting: %+v", usage)
	}
}

func TestCopilotCooldownIgnoresIncompleteAndOverflowedResponseEvidence(t *testing.T) {
	code := `{"error":{"code":"user_global_rate_limited"}}`
	for _, tc := range []struct {
		name         string
		body         string
		prefixOnly   bool
		wantCooldown bool
	}{
		{name: "complete response", body: code, wantCooldown: true},
		{name: "valid prefix beyond bound", body: code + strings.Repeat(" ", maxCopilotThrottleEvidenceBytes-len(code)) + `{}`},
		{name: "closed before EOF", body: code + " ", prefixOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &ProxyHandler{}
			req := copilotTrafficTestRequest(t, context.Background(), "copilot", "http://upstream.example", "credential", "editor", "model", 0)
			resp := routeExecutorTestResponse(req, http.StatusTooManyRequests, http.Header{"Retry-After": {"60"}}, tc.body)
			h.finishCopilotInference(req, resp, nil, nil)
			if tc.prefixOnly {
				_, _ = io.CopyN(io.Discard, resp.Body, int64(len(code)))
			} else {
				_, _ = io.Copy(io.Discard, resp.Body)
			}
			_ = resp.Body.Close()
			permit, blocked, err := h.acquireCopilotInference(req)
			permit.release()
			if err != nil || (blocked != nil) != tc.wantCooldown {
				t.Fatalf("response evidence cooldown=%v error=%v, want %v", blocked != nil, err, tc.wantCooldown)
			}
			if blocked != nil {
				_ = blocked.Body.Close()
			}
		})
	}
}

func TestCopilotCooldownExpiredResetAdmitsSingleProbe(t *testing.T) {
	h := &ProxyHandler{}
	var clock atomic.Int64
	clock.Store(time.Now().UnixNano())
	h.copilotTraffic.now = func() time.Time { return time.Unix(0, clock.Load()) }
	req := copilotTrafficTestRequest(t, context.Background(), "copilot", "http://upstream.example", "credential", "editor", "model", 0)
	metadata := copilotTrafficTestMetadata(t, req)
	h.copilotTraffic.observeThrottle(metadata, 429, "10", []byte(`{"error":{"code":"user_model_rate_limited"}}`))
	clock.Add(int64(11 * time.Second))
	probe, blocked, err := h.acquireCopilotInference(req)
	if probe == nil || blocked != nil || err != nil {
		t.Fatalf("first expired-reset request = permit:%v response:%v error:%v", probe != nil, blocked != nil, err)
	}
	defer probe.release()
	const concurrent = 12
	results := make(chan *http.Response, concurrent)
	errorsCh := make(chan error, concurrent)
	for range concurrent {
		go func() {
			permit, response, acquireErr := h.acquireCopilotInference(req)
			permit.release()
			results <- response
			errorsCh <- acquireErr
		}()
	}
	waitForCopilotTrafficWaiters(t, h, concurrent)
	response := routeExecutorTestResponse(req, 429, http.Header{"Retry-After": {"30"}}, `{"error":{"code":"user_model_rate_limited"}}`)
	h.finishCopilotInference(req, response, nil, probe)
	_, _ = io.Copy(io.Discard, response.Body)
	_ = response.Body.Close()
	for range concurrent {
		select {
		case got := <-results:
			if err := <-errorsCh; err != nil || got == nil || got.StatusCode != 429 || got.Header.Get("Retry-After") != "30" {
				t.Fatalf("waiting request escaped renewed cooldown: response=%v err=%v", got, err)
			}
			_ = got.Body.Close()
		case <-time.After(time.Second):
			t.Fatal("probe completion did not wake waiting requests")
		}
	}
	waitForCopilotTrafficWaiters(t, h, 0)
}

func TestCopilotLargeRequestAdmissionHoldsUntilClose(t *testing.T) {
	var sends atomic.Int32
	h := &ProxyHandler{maxRetries: 1, client: &http.Client{Transport: retryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		return routeExecutorTestResponse(req, http.StatusOK, nil, `{"choices":[]}`), nil
	})}}
	WithCopilotLargeRequestConcurrency(1, 256)(h)
	request := func(size int) func() (*http.Request, error) {
		return func() (*http.Request, error) {
			return copilotTrafficTestRequest(t, context.Background(), "copilot", "http://upstream.example", "credential", "editor", "model", size), nil
		}
	}
	first, err := h.doInferenceWithRetry(request(1024))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = first.Body.Close() }()
	_, _ = io.Copy(io.Discard, first.Body)
	second := make(chan *http.Response, 1)
	errorsCh := make(chan error, 1)
	go func() {
		resp, sendErr := h.doInferenceWithRetry(request(1024))
		second <- resp
		errorsCh <- sendErr
	}()
	waitForCopilotTrafficWaiters(t, h, 1)
	small, err := h.doInferenceWithRetry(request(0))
	if err != nil {
		t.Fatal(err)
	}
	_ = small.Body.Close()
	if got := sends.Load(); got != 2 {
		t.Fatalf("sends before closing first large response = %d, want large + small only", got)
	}
	_ = first.Body.Close()
	select {
	case resp := <-second:
		if err := <-errorsCh; err != nil || resp == nil {
			t.Fatalf("queued send: response=%v err=%v", resp, err)
		}
		_ = resp.Body.Close()
	case <-time.After(time.Second):
		t.Fatal("closing active response did not release queued request")
	}
	if got := sends.Load(); got != 3 || len(h.copilotTraffic.groups) != 0 {
		t.Fatalf("admission cleanup: sends=%d groups=%d", got, len(h.copilotTraffic.groups))
	}
}

func TestCopilotAdmissionCancellationBeforeDispatch(t *testing.T) {
	for _, reason := range []string{"client disconnect", "request deadline", "shutdown"} {
		t.Run(reason, func(t *testing.T) {
			h := &ProxyHandler{}
			WithCopilotLargeRequestConcurrency(1, 1)(h)
			firstReq := copilotTrafficTestRequest(t, context.Background(), "copilot", "http://upstream.example", "credential", "editor", "model", 0)
			first, _, err := h.acquireCopilotInference(firstReq)
			if err != nil {
				t.Fatal(err)
			}
			defer first.release()
			inbound, disconnect := context.WithCancel(context.Background())
			defer disconnect()
			ctx, cancel := h.newInferenceUpstreamContextFrom(inbound, false)
			defer cancel()
			if reason == "request deadline" {
				var deadlineCancel context.CancelFunc
				ctx, deadlineCancel = context.WithTimeout(ctx, 25*time.Millisecond)
				defer deadlineCancel()
			}
			req := copilotTrafficTestRequest(t, ctx, "copilot", "http://upstream.example", "credential", "editor", "model", 0)
			result := make(chan error, 1)
			go func() {
				permit, _, acquireErr := h.acquireCopilotInference(req)
				permit.release()
				result <- acquireErr
			}()
			waitForCopilotTrafficWaiters(t, h, 1)
			switch reason {
			case "client disconnect":
				disconnect()
				if ctx.Err() != nil {
					t.Fatal("client disconnect canceled detached inference context")
				}
			case "shutdown":
				h.BeginShutdown()
			}
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("queued request cancellation = %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("queued request ignored cancellation")
			}
			waitForCopilotTrafficWaiters(t, h, 0)
			first.release()
			if len(h.copilotTraffic.groups) != 0 {
				t.Fatal("canceled waiter retained admission state")
			}
		})
	}
}

func TestCopilotAdmissionTransportFailureReleasesPermit(t *testing.T) {
	h := &ProxyHandler{maxRetries: 1, client: &http.Client{Transport: retryRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return nil, errors.New("connection failed")
	})}}
	WithCopilotLargeRequestConcurrency(1, 1)(h)
	_, err := h.doInferenceWithRetry(func() (*http.Request, error) {
		return copilotTrafficTestRequest(t, context.Background(), "copilot", "http://upstream.example", "credential", "editor", "model", 0), nil
	})
	if err == nil || len(h.copilotTraffic.groups) != 0 {
		t.Fatalf("failed send retained admission permit: error=%v groups=%d", err, len(h.copilotTraffic.groups))
	}
}

func TestCopilotAdmissionTaskUsageTracksOnlyActualDispatch(t *testing.T) {
	started, releaseHeaders := make(chan struct{}), make(chan struct{})
	h := &ProxyHandler{maxRetries: 1, stats: newStatsCollector(), client: &http.Client{Transport: retryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-releaseHeaders
		return routeExecutorTestResponse(req, http.StatusOK, nil, `{"choices":[],"usage":{"prompt_tokens":3,"completion_tokens":2,"total_tokens":5,"reasoning_tokens":2},"copilot_usage":{"total_nano_aiu":9,"compute_units":4}}`), nil
	})}}
	WithCopilotLargeRequestConcurrency(1, 1)(h)
	t.Cleanup(func() {
		select {
		case <-releaseHeaders:
		default:
			close(releaseHeaders)
		}
	})
	request := func(ctx context.Context) func() (*http.Request, error) {
		return func() (*http.Request, error) {
			return copilotTrafficTestRequest(t, ctx, "copilot", "http://upstream.example", "credential", "editor", "model", 0), nil
		}
	}
	responses := make(chan *http.Response, 1)
	firstErrors := make(chan error, 1)
	go func() {
		resp, err := h.doInferenceWithRetry(request(context.Background()))
		responses <- resp
		firstErrors <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("first request did not reach the transport")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	queued := make(chan error, 1)
	go func() {
		resp, err := h.doInferenceWithRetry(request(ctx))
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		queued <- err
	}()
	waitForCopilotTrafficWaiters(t, h, 1)
	usage := h.stats.taskUsage.snapshot()
	if usage.Inflight != 1 || usage.Totals.Sends != 1 || usage.Totals.Completed != 0 {
		t.Fatalf("waiting for headers or admission changed actual-send accounting: %+v", usage)
	}
	cancel()
	select {
	case err := <-queued:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("queued request returned %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("canceled request remained queued")
	}
	close(releaseHeaders)
	resp := <-responses
	if err := <-firstErrors; err != nil {
		t.Fatal(err)
	}
	if _, err := io.Copy(io.Discard, resp.Body); err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	usage = h.stats.taskUsage.snapshot()
	if usage.Inflight != 0 || usage.Totals.Sends != 1 || usage.Totals.Completed != 1 || usage.Totals.Errors != 0 || usage.Totals.ReportedUsageSends != 1 {
		t.Fatalf("completed-send accounting includes canceled admission: %+v", usage)
	}
	if usage.Totals.Usage.PromptTokens != 3 || usage.Totals.Usage.CompletionTokens != 2 || usage.Totals.Usage.ReasoningTokens != 2 || usage.Totals.CopilotUsage.TotalNanoAIU != 9 || usage.Totals.CopilotUsage.ComputeUnits != 4 {
		t.Fatalf("actual response usage was lost: %+v", usage.Totals)
	}
}

func TestCopilotTrafficStateIsBounded(t *testing.T) {
	h := &ProxyHandler{}
	for i := range maxCopilotTrafficEntries + 20 {
		req := copilotTrafficTestRequest(t, context.Background(), "copilot", "http://upstream.example", fmt.Sprint(i), "editor", "model", 0)
		h.copilotTraffic.observeThrottle(copilotTrafficTestMetadata(t, req), 429, "3600", []byte(`{"error":{"code":"user_global_rate_limited"}}`))
	}
	if len(h.copilotTraffic.cooldowns) != maxCopilotTrafficEntries {
		t.Fatalf("stored %d cooldowns, want bounded %d", len(h.copilotTraffic.cooldowns), maxCopilotTrafficEntries)
	}
}

func TestCopilotCooldownRouteAdmissionPreservesStateAndSendAccounting(t *testing.T) {
	for _, pinned := range []bool{false, true} {
		t.Run(fmt.Sprintf("pinned=%t", pinned), func(t *testing.T) {
			var sends atomic.Int32
			client := &http.Client{Transport: retryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				sends.Add(1)
				return routeExecutorTestResponse(req, 200, nil, `{"id":"response","status":"completed","output":[]}`), nil
			})}
			primary := explicitRouteTestProvider("primary", "http://primary.example", "unused")
			primary.kind = providerTypeCopilot
			primary.paths = providerEndpointPolicyFor(providerTypeCopilot).defaultEndpointPaths()
			secondary := explicitRouteTestProvider("secondary", "http://secondary.example", "unused")
			h, route := explicitRouteTestHandler(t, client, routeModePriorityFailover, 2, 2, primary, secondary)
			h.auth = auth.NewTestAuthenticator("credential")
			h.stats = newStatsCollector()
			upstreamReq, err := h.newProviderJSONInferenceRequest(context.Background(), primary, http.MethodPost, providerEndpointResponses, []byte(`{"model":"deployment-a","input":"hello"}`), nil, "")
			if err != nil {
				t.Fatal(err)
			}
			h.copilotTraffic.observeThrottle(copilotTrafficTestMetadata(t, upstreamReq), 429, "86400", []byte(`{"error":{"code":"user_model_rate_limited"}}`))
			inbound, _ := WithRequestSummary(context.Background())
			operation := newRouteOperation(route, inbound)
			if pinned {
				if err := operation.forcePinnedTarget(route.targets[0].id); err != nil {
					t.Fatal(err)
				}
			}
			resp, err := h.executeExplicitRouteRequest(withRouteOperation(inbound, operation), route, providerEndpointResponses, []byte(`{"model":"public-model","input":"hello"}`), nil, "public-model", false)
			if err != nil {
				t.Fatal(err)
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			wantSends, wantStatus := 1, http.StatusOK
			if pinned {
				wantSends, wantStatus = 0, http.StatusTooManyRequests
			}
			if resp.StatusCode != wantStatus || int(sends.Load()) != wantSends {
				t.Fatalf("route cooldown: status=%d sends=%d, want %d/%d", resp.StatusCode, sends.Load(), wantStatus, wantSends)
			}
			if counted, _, _ := operation.snapshot(); counted != wantSends {
				t.Fatalf("counted %d physical sends, want %d", counted, wantSends)
			}
			if attempts := len(h.stats.snapshot().RecentAttempts); attempts != wantSends {
				t.Fatalf("recorded %d attempts, want %d actual sends", attempts, wantSends)
			}
			if task := h.stats.taskUsage.snapshot(); task.Totals.Sends != int64(wantSends) || task.Inflight != 0 {
				t.Fatalf("task accounting includes local rejection: %+v", task)
			}
		})
	}
}

func TestCopilotTokenCountingBypassesInferenceAdmission(t *testing.T) {
	h := &ProxyHandler{}
	provider := &providerRuntime{id: "copilot", kind: providerTypeCopilot}
	body := []byte(`{"model":"model","messages":[]}`)
	req, _ := http.NewRequest(http.MethodPost, "http://upstream.example/messages/count_tokens", bytes.NewReader(body))
	req = withCopilotInferenceRequest(req, provider, providerEndpointMessagesCount, body)
	if _, present := req.Context().Value(copilotInferenceRequestContextKey{}).(copilotInferenceRequest); present {
		t.Fatal("token counting acquired inference quota scope")
	}
	permit, response, err := h.acquireCopilotInference(req)
	if permit != nil || response != nil || err != nil {
		t.Fatalf("token counting was subject to inference admission: %v %v %v", permit, response, err)
	}
}
