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

func TestDoWithRetry_StalledIntermediateBodyIsTimeBounded(t *testing.T) {
	stalledBody := newBlockingRetryBodyReadCloserWithBody(`{"error":{"message":"retry later"}}`)
	var attempts atomic.Int32
	h := &ProxyHandler{
		client: &http.Client{Transport: retryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if attempts.Add(1) == 1 {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     make(http.Header),
					Body:       stalledBody,
					Request:    req,
				}, nil
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
				Request:    req,
			}, nil
		})},
		log:            logger.New(logger.LevelError),
		maxRetries:     2,
		retryBaseDelay: time.Nanosecond,
	}

	type result struct {
		resp    *http.Response
		err     error
		elapsed time.Duration
	}
	resultCh := make(chan result, 1)
	go func() {
		start := time.Now()
		resp, err := h.doWithRetry(func() (*http.Request, error) {
			return http.NewRequest(http.MethodPost, "http://upstream.test/retry", nil)
		})
		resultCh <- result{resp: resp, err: err, elapsed: time.Since(start)}
	}()

	var got result
	select {
	case got = <-resultCh:
	case <-time.After(upstreamErrorDetailDrainTimeout + 750*time.Millisecond):
		attemptsBeforeCleanup := attempts.Load()
		_ = stalledBody.Close()
		select {
		case <-resultCh:
		case <-time.After(time.Second):
		}
		t.Fatalf("retry remained blocked on stalled intermediate body; attempts before cleanup = %d, want 2", attemptsBeforeCleanup)
	}

	if got.err != nil {
		t.Fatalf("doWithRetry() error = %v, want success", got.err)
	}
	defer func() { _ = got.resp.Body.Close() }()
	if got.resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", got.resp.StatusCode)
	}
	if gotAttempts := attempts.Load(); gotAttempts != 2 {
		t.Fatalf("attempts = %d, want 2", gotAttempts)
	}
	if got.elapsed < upstreamErrorDetailDrainTimeout/2 || got.elapsed >= upstreamErrorDetailDrainTimeout+750*time.Millisecond {
		t.Fatalf("elapsed = %v, want bounded wait in [%v, %v)", got.elapsed, upstreamErrorDetailDrainTimeout/2, upstreamErrorDetailDrainTimeout+750*time.Millisecond)
	}
	waitForRetryBodyClose(t, stalledBody.closed)
}

func TestDoWithRetry_StalledFinalBodyIsTimeBoundedAndCaptured(t *testing.T) {
	stalledBody := newBlockingRetryBodyReadCloserWithBody(`{"error":{"message":"short stalled error"}}`)
	var attempts atomic.Int32
	h := &ProxyHandler{
		client: &http.Client{Transport: retryRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			attempts.Add(1)
			return &http.Response{
				StatusCode: http.StatusServiceUnavailable,
				Header:     make(http.Header),
				Body:       stalledBody,
				Request:    req,
			}, nil
		})},
		log:            logger.New(logger.LevelError),
		maxRetries:     1,
		retryBaseDelay: time.Nanosecond,
	}

	type result struct {
		err     error
		elapsed time.Duration
	}
	resultCh := make(chan result, 1)
	go func() {
		start := time.Now()
		_, err := h.doWithRetry(func() (*http.Request, error) {
			return http.NewRequest(http.MethodPost, "http://upstream.test/final", nil)
		})
		resultCh <- result{err: err, elapsed: time.Since(start)}
	}()

	var got result
	select {
	case got = <-resultCh:
	case <-time.After(upstreamErrorDetailDrainTimeout + 750*time.Millisecond):
		attemptsBeforeCleanup := attempts.Load()
		_ = stalledBody.Close()
		select {
		case <-resultCh:
		case <-time.After(time.Second):
		}
		t.Fatalf("terminal error remained blocked on stalled final body; attempts before cleanup = %d, want 1", attemptsBeforeCleanup)
	}

	if got.err == nil {
		t.Fatal("doWithRetry() error = nil, want exhausted retry error")
	}
	if !strings.Contains(got.err.Error(), "short stalled error") {
		t.Fatalf("error = %q, want captured short body detail", got.err)
	}
	if gotAttempts := attempts.Load(); gotAttempts != 1 {
		t.Fatalf("attempts = %d, want 1", gotAttempts)
	}
	if got.elapsed < upstreamErrorDetailDrainTimeout/2 || got.elapsed >= upstreamErrorDetailDrainTimeout+750*time.Millisecond {
		t.Fatalf("elapsed = %v, want bounded wait in [%v, %v)", got.elapsed, upstreamErrorDetailDrainTimeout/2, upstreamErrorDetailDrainTimeout+750*time.Millisecond)
	}
	waitForRetryBodyClose(t, stalledBody.closed)
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
	prefix    *strings.Reader
	closeOnce sync.Once
	closed    chan struct{}
}

func newBlockingRetryBodyReadCloser(prefixBytes int) *blockingRetryBodyReadCloser {
	return newBlockingRetryBodyReadCloserWithBody(strings.Repeat("a", prefixBytes))
}

func newBlockingRetryBodyReadCloserWithBody(prefix string) *blockingRetryBodyReadCloser {
	return &blockingRetryBodyReadCloser{
		prefix: strings.NewReader(prefix),
		closed: make(chan struct{}),
	}
}

func (r *blockingRetryBodyReadCloser) Read(p []byte) (int, error) {
	if r.prefix.Len() > 0 {
		return r.prefix.Read(p)
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

type retryRoundTripFunc func(*http.Request) (*http.Response, error)

func (f retryRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
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
