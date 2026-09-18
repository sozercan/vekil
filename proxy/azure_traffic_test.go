package proxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func azureTrafficTestClock(h *ProxyHandler) func(time.Duration) {
	var offset atomic.Int64
	base := time.Now()
	h.azureTraffic.now = func() time.Time { return base.Add(time.Duration(offset.Load())) }
	return func(delay time.Duration) {
		h.azureTraffic.mu.Lock()
		defer h.azureTraffic.mu.Unlock()
		offset.Add(int64(delay))
		for _, entry := range h.azureTraffic.cooldowns {
			entry.notify()
		}
	}
}

func azureTrafficTestRequest(t *testing.T, h *ProxyHandler, ctx context.Context, provider, origin, model string) *http.Request {
	t.Helper()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, origin+"/openai/v1/responses", strings.NewReader(`{"input":"test"}`))
	if err != nil {
		t.Fatal(err)
	}
	return h.withAzureRouteTraffic(req, targetBinding{
		provider: &providerRuntime{id: provider, kind: providerTypeAzureOpenAI}, upstreamModel: model,
	})
}

func waitForAzureTrafficWaiters(t *testing.T, h *ProxyHandler, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		h.azureTraffic.mu.Lock()
		got := h.azureTraffic.waiters
		h.azureTraffic.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("Azure traffic waiters did not reach %d", want)
}

type azureTrafficTestResult struct {
	permit  *azureTrafficPermit
	blocked *http.Response
	err     error
}

func acquireAzureTrafficAsync(h *ProxyHandler, req *http.Request) <-chan azureTrafficTestResult {
	result := make(chan azureTrafficTestResult, 1)
	go func() {
		permit, blocked, err := h.acquireAzureRouteInference(req, false)
		result <- azureTrafficTestResult{permit, blocked, err}
	}()
	return result
}

func receiveAzureTrafficResult(t *testing.T, result <-chan azureTrafficTestResult) azureTrafficTestResult {
	t.Helper()
	select {
	case value := <-result:
		return value
	case <-time.After(3 * time.Second):
		t.Fatal("Azure admission did not finish")
		return azureTrafficTestResult{}
	}
}

func TestAzureTrafficCooldownScopeAndExpiredProbe(t *testing.T) {
	h := &ProxyHandler{}
	t.Cleanup(h.BeginShutdown)
	advance := azureTrafficTestClock(h)
	primary := azureTrafficTestRequest(t, h, t.Context(), "east", "https://east.example", "deployment")
	if !azureRouteTrafficFromRequest(primary).observe(429, http.Header{"Retry-After": {"5"}}) {
		t.Fatal("rate limit was not recorded")
	}
	for _, tc := range []struct {
		name, provider, origin, model string
		blocked                       bool
	}{
		{"same deployment", "east", "https://east.example", "deployment", true},
		{"provider alias", "east-alias", "https://east.example", "deployment", true},
		{"different resource", "west", "https://west.example", "deployment", false},
		{"different deployment", "east", "https://east.example", "classifier", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := azureTrafficTestRequest(t, h, t.Context(), tc.provider, tc.origin, tc.model)
			permit, blocked, err := h.acquireAzureRouteInference(req, true)
			defer permit.release()
			if err != nil || (blocked != nil) != tc.blocked {
				t.Fatalf("admission: blocked=%v error=%v", blocked != nil, err)
			}
			if blocked != nil {
				defer func() { _ = blocked.Body.Close() }()
				if blocked.StatusCode != 429 || blocked.Header.Get("Retry-After") != "5" {
					t.Fatalf("cooldown response = %d, %v", blocked.StatusCode, blocked.Header)
				}
			}
		})
	}
	advance(5 * time.Second)
	probe, blocked, err := h.acquireAzureRouteInference(primary, true)
	if err != nil || blocked != nil || probe == nil {
		t.Fatalf("expired cooldown did not admit a probe: %v, %v", blocked, err)
	}
	probe.release()
	permit, blocked, err := h.acquireAzureRouteInference(primary, true)
	defer permit.release()
	if err != nil || blocked != nil || permit != nil {
		t.Fatalf("successful probe did not restore ordinary admission: %v, %v", blocked, err)
	}
}

func TestAzureTrafficRecoveryQueueSerializesAndRenews(t *testing.T) {
	h := &ProxyHandler{}
	t.Cleanup(h.BeginShutdown)
	advance := azureTrafficTestClock(h)
	req := azureTrafficTestRequest(t, h, t.Context(), "east", "https://east.example", "deployment")
	metadata := azureRouteTrafficFromRequest(req)
	metadata.observe(429, http.Header{"Retry-After": {"5"}})
	first := acquireAzureTrafficAsync(h, req)
	waitForAzureTrafficWaiters(t, h, 1)
	second := acquireAzureTrafficAsync(h, req)
	waitForAzureTrafficWaiters(t, h, 2)
	advance(5 * time.Second)
	probe := receiveAzureTrafficResult(t, first)
	if probe.err != nil || probe.blocked != nil || probe.permit == nil {
		t.Fatalf("first probe = %+v", probe)
	}
	select {
	case <-second:
		t.Fatal("concurrent recovery send was admitted")
	default:
	}
	// HTTP 200 may still contain a streamed 429. Publish its reset before
	// releasing the probe so the next caller cannot race the renewed limit.
	metadata.observe(429, http.Header{"Retry-After": {"10"}})
	probe.permit.release()
	waitForAzureTrafficWaiters(t, h, 1)
	advance(9 * time.Second)
	select {
	case <-second:
		t.Fatal("renewed cooldown was ignored")
	default:
	}
	advance(time.Second)
	next := receiveAzureTrafficResult(t, second)
	if next.err != nil || next.blocked != nil || next.permit == nil {
		t.Fatalf("second probe = %+v", next)
	}
	next.permit.release()
	waitForAzureTrafficWaiters(t, h, 0)
}

func TestAzureTrafficWaitCancellationAndBounds(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "disconnect", true: "shutdown"}[shutdown], func(t *testing.T) {
			h := &ProxyHandler{}
			t.Cleanup(h.BeginShutdown)
			azureTrafficTestClock(h)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req := azureTrafficTestRequest(t, h, ctx, "east", "https://east.example", "deployment")
			azureRouteTrafficFromRequest(req).observe(429, http.Header{"Retry-After": {"60"}})
			results := make([]<-chan azureTrafficTestResult, maxAzureDeploymentWaiters)
			for i := range results {
				results[i] = acquireAzureTrafficAsync(h, req)
			}
			waitForAzureTrafficWaiters(t, h, maxAzureDeploymentWaiters)
			_, blocked, err := h.acquireAzureRouteInference(req, false)
			var queueErr *providerRequestError
			if blocked != nil || !errors.As(err, &queueErr) || queueErr.statusCode != 503 || queueErr.code != "rate_limit_queue_full" {
				t.Fatalf("queue bound was not enforced: %v, %v", blocked, err)
			}
			if shutdown {
				h.BeginShutdown()
			} else {
				cancel()
			}
			for _, result := range results {
				if got := receiveAzureTrafficResult(t, result); !errors.Is(got.err, context.Canceled) || got.permit != nil {
					t.Fatalf("canceled waiter = %+v", got)
				}
			}
			waitForAzureTrafficWaiters(t, h, 0)
			if h.azureTraffic.waitingBytes != 0 {
				t.Fatalf("retained waiting bytes = %d", h.azureTraffic.waitingBytes)
			}
		})
	}
	h := &ProxyHandler{}
	t.Cleanup(h.BeginShutdown)
	req := azureTrafficTestRequest(t, h, t.Context(), "east", "https://east.example", "deployment")
	azureRouteTrafficFromRequest(req).observe(429, http.Header{"Retry-After": {"5"}})
	req.ContentLength = maxAzureTrafficWaitingBytes + 1
	_, _, err := h.acquireAzureRouteInference(req, false)
	var queueErr *providerRequestError
	if !errors.As(err, &queueErr) || queueErr.code != "rate_limit_queue_full" {
		t.Fatalf("waiting byte bound was not enforced: %v", err)
	}
}

func TestAzureTrafficLongResetPreservesDelayWithoutWaiting(t *testing.T) {
	h := &ProxyHandler{}
	t.Cleanup(h.BeginShutdown)
	azureTrafficTestClock(h)
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	req := azureTrafficTestRequest(t, h, ctx, "east", "https://east.example", "deployment")
	azureRouteTrafficFromRequest(req).observe(429, http.Header{"Retry-After": {"5"}, "Retry-After-Ms": {"86400000"}})
	_, blocked, err := h.acquireAzureRouteInference(req, false)
	if err != nil || blocked == nil || blocked.Header.Get("Retry-After") != "86400" {
		t.Fatalf("long reset = %v, %v", blocked, err)
	}
	defer func() { _ = blocked.Body.Close() }()
	if h.azureTraffic.waiters != 0 {
		t.Fatal("long reset entered the recovery queue")
	}
}

func TestAzureTrafficBoundsAcrossDeployments(t *testing.T) {
	h := &ProxyHandler{}
	t.Cleanup(h.BeginShutdown)
	advance := azureTrafficTestClock(h)
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	for i := 0; i <= maxAzureTrafficEntries; i++ {
		req := azureTrafficTestRequest(t, h, ctx, "east", "https://east.example", fmt.Sprintf("deployment-%d", i))
		tracked := azureRouteTrafficFromRequest(req).observe(429, http.Header{"Retry-After": {"60"}})
		if tracked != (i < maxAzureTrafficEntries) {
			t.Fatalf("cooldown capacity: entry=%d tracked=%v", i, tracked)
		}
	}
	var waiting []<-chan azureTrafficTestResult
	for i := range maxAzureTrafficWaitingBytes / maxAzureDeploymentWaitingBytes {
		req := azureTrafficTestRequest(t, h, ctx, "east", "https://east.example", fmt.Sprintf("deployment-%d", i))
		req.ContentLength = maxAzureDeploymentWaitingBytes
		waiting = append(waiting, acquireAzureTrafficAsync(h, req))
	}
	waitForAzureTrafficWaiters(t, h, len(waiting))
	// Reuse an existing tracked deployment because the entry table is full.
	overflow := azureTrafficTestRequest(t, h, ctx, "east", "https://east.example", "deployment-10")
	overflow.ContentLength = 1
	_, _, err := h.acquireAzureRouteInference(overflow, false)
	var queueErr *providerRequestError
	if !errors.As(err, &queueErr) || queueErr.code != "rate_limit_queue_full" {
		t.Fatalf("aggregate waiting byte bound was not enforced: %v", err)
	}
	cancel()
	for _, result := range waiting {
		if result := receiveAzureTrafficResult(t, result); !errors.Is(result.err, context.Canceled) {
			t.Fatalf("canceled admission = %+v", result)
		}
	}
	advance(time.Minute)
	next := azureTrafficTestRequest(t, h, t.Context(), "east", "https://east.example", "new-deployment")
	if !azureRouteTrafficFromRequest(next).observe(429, http.Header{"Retry-After": {"5"}}) {
		t.Fatal("expired cooldown entries were not reclaimed")
	}
	h.azureTraffic.mu.Lock()
	defer h.azureTraffic.mu.Unlock()
	if len(h.azureTraffic.cooldowns) != 1 || h.azureTraffic.waitingBytes != 0 || h.azureTraffic.waiters != 0 {
		t.Fatal("expired records or canceled waiters were retained")
	}
}

func TestAzureTrafficSuccessfulExhaustionStartsCooldown(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    int
		headers   http.Header
		wantReset string
	}{
		{"negative tokens", 200, http.Header{"X-Ratelimit-Remaining-Tokens": {"-65538"}, "X-Ratelimit-Reset-Tokens": {"71"}}, "71"},
		{"minus one tokens with reset", 200, http.Header{"X-Ratelimit-Remaining-Tokens": {"-1"}, "X-Ratelimit-Reset-Tokens": {"2"}}, "2"},
		{"zero requests", 200, http.Header{"X-Ratelimit-Remaining-Requests": {"0"}, "X-Ratelimit-Reset-Requests": {"4s"}}, "4"},
		{"both dimensions", 200, http.Header{"X-Ratelimit-Remaining-Tokens": {"0"}, "X-Ratelimit-Reset-Tokens": {"4s"}, "X-Ratelimit-Remaining-Requests": {"0"}, "X-Ratelimit-Reset-Requests": {"10s"}}, "10"},
		{"capacity remains", 200, http.Header{"X-Ratelimit-Remaining-Tokens": {"100"}, "X-Ratelimit-Reset-Tokens": {"60"}, "Retry-After": {"30"}}, ""},
		{"missing reset", 200, http.Header{"X-Ratelimit-Remaining-Tokens": {"0"}}, ""},
		{"invalid remaining", 200, http.Header{"X-Ratelimit-Remaining-Tokens": {"unknown"}, "X-Ratelimit-Reset-Tokens": {"60"}}, ""},
		{"unrelated error", 400, http.Header{"X-Ratelimit-Remaining-Tokens": {"0"}, "X-Ratelimit-Reset-Tokens": {"60"}}, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &ProxyHandler{}
			t.Cleanup(h.BeginShutdown)
			azureTrafficTestClock(h)
			req := azureTrafficTestRequest(t, h, t.Context(), "east", "https://east.example", "deployment")
			azureRouteTrafficFromRequest(req).observe(tc.status, tc.headers)
			permit, blocked, err := h.acquireAzureRouteInference(req, true)
			defer permit.release()
			if err != nil || (blocked != nil) != (tc.wantReset != "") {
				t.Fatalf("admission: blocked=%v err=%v", blocked != nil, err)
			}
			if blocked != nil {
				defer func() { _ = blocked.Body.Close() }()
				if blocked.Header.Get("Retry-After") != tc.wantReset {
					t.Fatalf("reset=%s want=%s", blocked.Header.Get("Retry-After"), tc.wantReset)
				}
			}
		})
	}
}

func TestAzureTrafficQueueCapacityIsReservedAcrossDeployments(t *testing.T) {
	for _, byteBound := range []bool{false, true} {
		t.Run(map[bool]string{false: "waiters", true: "bytes"}[byteBound], func(t *testing.T) {
			h := &ProxyHandler{}
			t.Cleanup(h.BeginShutdown)
			azureTrafficTestClock(h)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			east := azureTrafficTestRequest(t, h, ctx, "east", "https://east.example", "deployment")
			west := azureTrafficTestRequest(t, h, ctx, "west", "https://west.example", "deployment")
			azureRouteTrafficFromRequest(east).observe(429, http.Header{"Retry-After": {"30"}})
			azureRouteTrafficFromRequest(west).observe(429, http.Header{"Retry-After": {"30"}})
			count := maxAzureDeploymentWaiters
			if byteBound {
				count = 1
				east.ContentLength = maxAzureDeploymentWaitingBytes
				west.ContentLength = maxAzureDeploymentWaitingBytes
			}
			var results []<-chan azureTrafficTestResult
			for range count {
				results = append(results, acquireAzureTrafficAsync(h, east))
			}
			waitForAzureTrafficWaiters(t, h, count)
			_, _, err := h.acquireAzureRouteInference(east, false)
			var full *providerRequestError
			if !errors.As(err, &full) || full.code != "rate_limit_queue_full" {
				t.Fatalf("hot deployment exceeded its queue share: %v", err)
			}
			results = append(results, acquireAzureTrafficAsync(h, west))
			waitForAzureTrafficWaiters(t, h, count+1)
			cancel()
			for _, result := range results {
				if result := receiveAzureTrafficResult(t, result); !errors.Is(result.err, context.Canceled) {
					t.Fatalf("queued result=%+v", result)
				}
			}
			waitForAzureTrafficWaiters(t, h, 0)
			h.azureTraffic.mu.Lock()
			defer h.azureTraffic.mu.Unlock()
			if h.azureTraffic.waitingBytes != 0 {
				t.Fatal("global queued bytes leaked")
			}
			for _, entry := range h.azureTraffic.cooldowns {
				if entry.waitingBytes != 0 {
					t.Fatal("deployment queued bytes leaked")
				}
			}
		})
	}
}
