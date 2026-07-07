package proxy

import (
	"context"
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
	"github.com/sozercan/vekil/proxy/selector"
)

func TestDoWithRetry_SuccessOnFirstTry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	h := &ProxyHandler{
		auth:           auth.NewTestAuthenticator("tok"),
		client:         server.Client(),
		copilotURL:     server.URL,
		log:            logger.New(logger.LevelError),
		retryBaseDelay: 1 * time.Millisecond,
	}

	resp, err := h.doWithRetry(func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, server.URL+"/test", nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 call, got %d", calls.Load())
	}
}

func TestDoWithRetry_RetriesOnServerError(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n < 3 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	h := &ProxyHandler{
		auth:           auth.NewTestAuthenticator("tok"),
		client:         server.Client(),
		copilotURL:     server.URL,
		log:            logger.New(logger.LevelError),
		retryBaseDelay: 1 * time.Millisecond,
	}

	resp, err := h.doWithRetry(func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, server.URL+"/test", nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 calls, got %d", calls.Load())
	}
}

func TestDoWithRetry_RetriesAnthropicOverloaded529(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.WriteHeader(529)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	h := &ProxyHandler{
		auth:           auth.NewTestAuthenticator("tok"),
		client:         server.Client(),
		copilotURL:     server.URL,
		log:            logger.New(logger.LevelError),
		retryBaseDelay: 1 * time.Millisecond,
	}

	resp, err := h.doWithRetry(func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, server.URL+"/test", nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestDoWithRetry_ExhaustsRetries(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	h := &ProxyHandler{
		auth:           auth.NewTestAuthenticator("tok"),
		client:         server.Client(),
		copilotURL:     server.URL,
		log:            logger.New(logger.LevelError),
		retryBaseDelay: 1 * time.Millisecond,
	}

	_, err := h.doWithRetry(func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, server.URL+"/test", nil)
	})
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	if calls.Load() != 3 {
		t.Errorf("expected 3 calls, got %d", calls.Load())
	}
}

func TestDoWithRetry_NoRetryOn4xx(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer server.Close()

	h := &ProxyHandler{
		auth:           auth.NewTestAuthenticator("tok"),
		client:         server.Client(),
		copilotURL:     server.URL,
		log:            logger.New(logger.LevelError),
		retryBaseDelay: 1 * time.Millisecond,
	}

	resp, err := h.doWithRetry(func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, server.URL+"/test", nil)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("expected 400, got %d", resp.StatusCode)
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 call (no retry for 400), got %d", calls.Load())
	}
}

func TestRetryable(t *testing.T) {
	tests := []struct {
		code     int
		expected bool
	}{
		{http.StatusOK, false},
		{http.StatusBadRequest, false},
		{http.StatusUnauthorized, false},
		{http.StatusRequestTimeout, true},
		{http.StatusTooManyRequests, true},
		{http.StatusInternalServerError, false},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
		{520, true},
		{521, true},
		{522, true},
		{523, true},
		{524, true},
		{529, true},
		{530, true},
	}

	for _, tt := range tests {
		if got := retryable(tt.code); got != tt.expected {
			t.Errorf("retryable(%d) = %v, want %v", tt.code, got, tt.expected)
		}
	}
}

func TestDoWithRetry_CancelledDuringBackoff(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	h := &ProxyHandler{
		auth:           auth.NewTestAuthenticator("tok"),
		client:         server.Client(),
		copilotURL:     server.URL,
		log:            logger.New(logger.LevelError),
		retryBaseDelay: 5 * time.Second, // long enough that cancel fires first
	}

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay to interrupt the backoff sleep.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	_, err := h.doWithRetry(func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/test", nil)
	})
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
	if calls.Load() != 1 {
		t.Errorf("expected 1 call before cancel, got %d", calls.Load())
	}
}

func TestDoWithRetry_RespectsRetryAfter(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		if n == 1 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer server.Close()

	h := &ProxyHandler{
		auth:           auth.NewTestAuthenticator("tok"),
		client:         server.Client(),
		copilotURL:     server.URL,
		log:            logger.New(logger.LevelError),
		retryBaseDelay: 1 * time.Millisecond,
	}

	start := time.Now()
	resp, err := h.doWithRetry(func() (*http.Request, error) {
		return http.NewRequest(http.MethodPost, server.URL+"/test", nil)
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	if calls.Load() != 2 {
		t.Errorf("expected 2 calls, got %d", calls.Load())
	}
	// Retry-After: 1 means at least 1 second of delay.
	if elapsed < 900*time.Millisecond {
		t.Errorf("expected at least ~1s delay from Retry-After, got %v", elapsed)
	}
}

func TestReadRetryableUpstreamErrorBodyDrainsAfterBoundedCapture(t *testing.T) {
	body := strings.Repeat("a", upstreamErrorDetailMaxBodyBytes+123)
	reader := newRetryBodyReadCloser(body)

	got := readRetryableUpstreamErrorBody(reader)
	waitForRetryBodyClose(t, reader.closed)

	if string(got) != body[:upstreamErrorDetailMaxBodyBytes] {
		t.Fatalf("captured body = %q, want first %d bytes", string(got), upstreamErrorDetailMaxBodyBytes)
	}
	if unread := reader.Len(); unread != 0 {
		t.Fatalf("expected response body to be drained to EOF, %d bytes unread", unread)
	}
}

func TestReadRetryableUpstreamErrorBodyCapsDrainAfterCapture(t *testing.T) {
	body := strings.Repeat("a", upstreamErrorDetailMaxBodyBytes+upstreamErrorDetailDrainBytes+123)
	reader := newRetryBodyReadCloser(body)

	got := readRetryableUpstreamErrorBody(reader)
	waitForRetryBodyClose(t, reader.closed)

	if string(got) != body[:upstreamErrorDetailMaxBodyBytes] {
		t.Fatalf("captured body = %q, want first %d bytes", string(got), upstreamErrorDetailMaxBodyBytes)
	}
	if unread := reader.Len(); unread != 123 {
		t.Fatalf("unread bytes after bounded drain = %d, want 123", unread)
	}
}

func TestReadRetryableUpstreamErrorBodyDoesNotWaitForStalledDrain(t *testing.T) {
	reader := newBlockingRetryBodyReadCloser(upstreamErrorDetailMaxBodyBytes)

	start := time.Now()
	got := readRetryableUpstreamErrorBody(reader)
	elapsed := time.Since(start)

	if len(got) != upstreamErrorDetailMaxBodyBytes {
		t.Fatalf("captured body length = %d, want %d", len(got), upstreamErrorDetailMaxBodyBytes)
	}
	if elapsed >= upstreamErrorDetailDrainTimeout {
		t.Fatalf("readRetryableUpstreamErrorBody elapsed = %v, want less than drain timeout %v", elapsed, upstreamErrorDetailDrainTimeout)
	}
	select {
	case <-reader.closed:
		t.Fatal("expected stalled drain to close asynchronously after timeout, not before return")
	default:
	}
	waitForRetryBodyClose(t, reader.closed)
}

type retryBodyReadCloser struct {
	*strings.Reader
	closeOnce sync.Once
	closed    chan struct{}
}

func newRetryBodyReadCloser(body string) *retryBodyReadCloser {
	return &retryBodyReadCloser{
		Reader: strings.NewReader(body),
		closed: make(chan struct{}),
	}
}

func (r *retryBodyReadCloser) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

type blockingRetryBodyReadCloser struct {
	remaining int
	closeOnce sync.Once
	closed    chan struct{}
}

func newBlockingRetryBodyReadCloser(prefixBytes int) *blockingRetryBodyReadCloser {
	return &blockingRetryBodyReadCloser{
		remaining: prefixBytes,
		closed:    make(chan struct{}),
	}
}

func (r *blockingRetryBodyReadCloser) Read(p []byte) (int, error) {
	if r.remaining > 0 {
		n := min(len(p), r.remaining)
		for i := range n {
			p[i] = 'a'
		}
		r.remaining -= n
		return n, nil
	}
	<-r.closed
	return 0, io.ErrClosedPipe
}

func (r *blockingRetryBodyReadCloser) Close() error {
	r.closeOnce.Do(func() { close(r.closed) })
	return nil
}

func waitForRetryBodyClose(t *testing.T, closed <-chan struct{}) {
	t.Helper()
	select {
	case <-closed:
	case <-time.After(upstreamErrorDetailDrainTimeout + time.Second):
		t.Fatal("timed out waiting for response body to close")
	}
}

func TestParseRetryAfter(t *testing.T) {
	tests := []struct {
		value   string
		wantDur time.Duration
		wantOK  bool
	}{
		{"", 0, false},
		{"0", 0, false},
		{"-1", 0, false},
		{"abc", 0, false},
		{"5", 5 * time.Second, true},
		{"120", 120 * time.Second, true},
		{"999999", maxRetryAfter, true},
		{"Wed, 21 Oct 2015 07:28:00 GMT", 0, false},
	}

	for _, tt := range tests {
		t.Run(fmt.Sprintf("value=%q", tt.value), func(t *testing.T) {
			dur, ok := parseRetryAfter(tt.value)
			if ok != tt.wantOK || dur != tt.wantDur {
				t.Errorf("parseRetryAfter(%q) = (%v, %v), want (%v, %v)", tt.value, dur, ok, tt.wantDur, tt.wantOK)
			}
		})
	}
}

func TestParseRetryAfter_HTTPDateAndClamp(t *testing.T) {
	future := time.Now().Add(2 * time.Second).UTC().Format(http.TimeFormat)
	dur, ok := parseRetryAfter(future)
	if !ok {
		t.Fatalf("parseRetryAfter(future HTTP-date) ok = false, want true")
	}
	if dur <= 0 || dur > 3*time.Second {
		t.Fatalf("future HTTP-date duration = %v, want a small positive duration", dur)
	}

	farFuture := time.Now().Add(24 * time.Hour).UTC().Format(http.TimeFormat)
	dur, ok = parseRetryAfter(farFuture)
	if !ok || dur != maxRetryAfter {
		t.Fatalf("far future duration = (%v, %v), want (%v, true)", dur, ok, maxRetryAfter)
	}
}

func TestBackoffGuardsZeroBaseAndLargeAttempt(t *testing.T) {
	if got := backoff(0, -10); got <= 0 {
		t.Fatalf("backoff(0, -10) = %v, want positive duration", got)
	}
	if got := backoff(time.Second, 1000); got < maxRetryBackoff || got >= maxRetryBackoff+maxRetryBackoff/4 {
		t.Fatalf("backoff(time.Second, 1000) = %v, want capped delay plus bounded jitter", got)
	}
}

func TestDoWithRetry_DoesNotPenalizeCanceledRequest(t *testing.T) {
	endpoint := &providerEndpointRuntime{endpoint: selector.Endpoint{Name: "east", Healthy: true}, health: newEndpointHealthTracker(endpointHealthConfig{errorBudget: endpointErrorBudget{Limit: 1, Window: time.Minute}, cooldown: time.Hour})}
	h := &ProxyHandler{client: http.DefaultClient, log: logger.New(logger.LevelError), retryBaseDelay: time.Millisecond}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := h.doWithRetry(func() (*http.Request, error) {
		req, reqErr := http.NewRequestWithContext(contextWithProviderEndpoint(ctx, endpoint, 2), http.MethodGet, "http://127.0.0.1:1", nil)
		return req, reqErr
	})
	if err == nil {
		t.Fatal("doWithRetry() error = nil, want canceled request error")
	}
	if !endpoint.health.healthy(time.Now()) {
		t.Fatal("canceled request should not penalize endpoint health")
	}
}

func TestDoWithRetry_BypassesRetryAfterForMultiEndpointFailover(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	endpoint := &providerEndpointRuntime{endpoint: selector.Endpoint{Name: "east", Healthy: true}, health: newEndpointHealthTracker(defaultEndpointHealthConfig())}
	h := &ProxyHandler{client: server.Client(), log: logger.New(logger.LevelError), retryBaseDelay: time.Millisecond}
	start := time.Now()
	resp, err := h.doWithRetry(func() (*http.Request, error) {
		return http.NewRequestWithContext(contextWithProviderEndpoint(context.Background(), endpoint, 2), http.MethodGet, server.URL, nil)
	})
	if err != nil {
		t.Fatalf("doWithRetry() error = %v", err)
	}
	_ = resp.Body.Close()
	if time.Since(start) > time.Second {
		t.Fatalf("retry slept too long despite alternate endpoints: %v", time.Since(start))
	}
	if calls.Load() != 2 {
		t.Fatalf("calls = %d, want 2", calls.Load())
	}
}

func TestDoWithRetry_PenalizesAuthFailures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusUnauthorized) }))
	defer server.Close()
	endpoint := &providerEndpointRuntime{endpoint: selector.Endpoint{Name: "bad-key", Healthy: true}, health: newEndpointHealthTracker(endpointHealthConfig{errorBudget: endpointErrorBudget{Limit: 1, Window: time.Minute}, cooldown: time.Hour})}
	h := &ProxyHandler{client: server.Client(), log: logger.New(logger.LevelError)}
	resp, err := h.doWithRetry(func() (*http.Request, error) {
		return http.NewRequestWithContext(contextWithProviderEndpoint(context.Background(), endpoint, 2), http.MethodGet, server.URL, nil)
	})
	if err != nil {
		t.Fatalf("doWithRetry() error = %v", err)
	}
	_ = resp.Body.Close()
	if endpoint.health.healthy(time.Now()) {
		t.Fatal("401 endpoint should be penalized")
	}
}

func TestDoWithRetry_DoesNotRecordStreamingHeaderAsSuccess(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: hi\n\n"))
	}))
	defer server.Close()
	endpoint := &providerEndpointRuntime{endpoint: selector.Endpoint{Name: "stream", Healthy: true}, health: newEndpointHealthTracker(defaultEndpointHealthConfig())}
	h := &ProxyHandler{client: server.Client(), log: logger.New(logger.LevelError)}
	resp, err := h.doWithRetry(func() (*http.Request, error) {
		return http.NewRequestWithContext(contextWithProviderEndpoint(context.Background(), endpoint, 2), http.MethodGet, server.URL, nil)
	})
	if err != nil {
		t.Fatalf("doWithRetry() error = %v", err)
	}
	_ = resp.Body.Close()
	if got := endpoint.health.latency(); got != 0 {
		t.Fatalf("streaming header recorded latency = %v, want 0 until body completes", got)
	}
}
