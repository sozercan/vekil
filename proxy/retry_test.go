package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
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
	var upstreamErr *upstreamError
	if !errors.As(err, &upstreamErr) || upstreamErr.statusCode != http.StatusBadGateway {
		t.Errorf("expected preserved 502 upstream error, got %v", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Errorf("retryable status was replaced by context.Canceled: %v", err)
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
		{"10000000000", maxRetryAfter, true},
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

func TestDurationSecondsCeil(t *testing.T) {
	tests := []struct {
		delay time.Duration
		want  int64
	}{
		{delay: 500 * time.Millisecond, want: 1},
		{delay: time.Second, want: 1},
		{delay: 1500 * time.Millisecond, want: 2},
		{delay: maxRetryAfter, want: 300},
	}
	for _, tt := range tests {
		if got := durationSecondsCeil(tt.delay); got != tt.want {
			t.Fatalf("durationSecondsCeil(%v) = %d, want %d", tt.delay, got, tt.want)
		}
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

func TestDoWithRetryLifecycleCancellationDoesNotCountUnmadeRetry(t *testing.T) {
	t.Run("shutdown cancellation", func(t *testing.T) {
		started := make(chan struct{})
		var logs strings.Builder
		h := &ProxyHandler{
			client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				close(started)
				<-req.Context().Done()
				return nil, req.Context().Err()
			})},
			log:            logger.NewWithWriter(logger.LevelDebug, &logs),
			stats:          newStatsCollector(),
			maxRetries:     2,
			retryBaseDelay: time.Millisecond,
		}
		ctx, cancel := h.newInferenceUpstreamContext(false)
		defer cancel()
		ctx = markRetryStatsTracked(ctx)

		done := make(chan error, 1)
		go func() {
			_, err := h.doWithRetry(func() (*http.Request, error) {
				return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/retry", nil)
			})
			done <- err
		}()
		<-started
		h.BeginShutdown()
		if err := <-done; err != context.Canceled {
			t.Fatalf("doWithRetry() error = %v, want context.Canceled", err)
		}
		snap := h.stats.snapshot()
		if snap.Retries != 0 || len(snap.RetriesByCode) != 0 {
			t.Fatalf("shutdown retry stats = retries:%d by_code:%v, want zero", snap.Retries, snap.RetriesByCode)
		}
		if strings.Contains(logs.String(), "retrying upstream request") {
			t.Fatalf("shutdown logged an unmade retry: %s", logs.String())
		}
	})

	for _, tt := range []struct {
		name       string
		transport  roundTripFunc
		wantStatus int
	}{
		{
			name: "transport cancellation during backoff",
			transport: func(req *http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("temporary transport failure")
			},
		},
		{
			name:       "status cancellation during backoff",
			wantStatus: http.StatusServiceUnavailable,
			transport: func(req *http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusServiceUnavailable,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"error":"retry"}`)),
					Request:    req,
				}, nil
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			firstAttempt := make(chan struct{})
			var calls atomic.Int32
			h := &ProxyHandler{
				client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if calls.Add(1) == 1 {
						close(firstAttempt)
					}
					return tt.transport(req)
				})},
				log:            logger.NewWithWriter(logger.LevelDebug, io.Discard),
				stats:          newStatsCollector(),
				maxRetries:     2,
				retryBaseDelay: 500 * time.Millisecond,
			}
			ctx, cancel := h.newInferenceUpstreamContext(false)
			defer cancel()
			ctx = markRetryStatsTracked(ctx)
			done := make(chan error, 1)
			go func() {
				_, err := h.doWithRetry(func() (*http.Request, error) {
					return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/retry", nil)
				})
				done <- err
			}()
			<-firstAttempt
			time.Sleep(20 * time.Millisecond)
			h.BeginShutdown()
			err := <-done
			if tt.wantStatus != 0 {
				var upstreamErr *upstreamError
				if !errors.As(err, &upstreamErr) || upstreamErr.statusCode != tt.wantStatus {
					t.Fatalf("doWithRetry() error = %v, want upstream status %d", err, tt.wantStatus)
				}
			} else if err != context.Canceled {
				t.Fatalf("doWithRetry() error = %v, want context.Canceled", err)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("attempts = %d, want 1", got)
			}
			snap := h.stats.snapshot()
			if snap.Retries != 0 || len(snap.RetriesByCode) != 0 {
				t.Fatalf("canceled backoff retry stats = retries:%d by_code:%v, want zero", snap.Retries, snap.RetriesByCode)
			}
		})
	}

	t.Run("next request factory failure", func(t *testing.T) {
		var transportCalls atomic.Int32
		var factoryCalls atomic.Int32
		h := &ProxyHandler{
			client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				transportCalls.Add(1)
				return nil, fmt.Errorf("temporary transport failure")
			})},
			log:            logger.NewWithWriter(logger.LevelDebug, io.Discard),
			stats:          newStatsCollector(),
			maxRetries:     2,
			retryBaseDelay: time.Nanosecond,
		}
		ctx, cancel := h.newInferenceUpstreamContext(false)
		defer cancel()
		ctx = markRetryStatsTracked(ctx)
		_, err := h.doWithRetry(func() (*http.Request, error) {
			if factoryCalls.Add(1) == 2 {
				return nil, fmt.Errorf("credential refresh failed")
			}
			return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/retry", nil)
		})
		if err == nil || !strings.Contains(err.Error(), "credential refresh failed") {
			t.Fatalf("doWithRetry() error = %v, want credential refresh failure", err)
		}
		if transportCalls.Load() != 1 {
			t.Fatalf("transport calls = %d, want 1", transportCalls.Load())
		}
		if snap := h.stats.snapshot(); snap.Retries != 0 || len(snap.RetriesByCode) != 0 {
			t.Fatalf("factory failure retry stats = retries:%d by_code:%v, want zero", snap.Retries, snap.RetriesByCode)
		}
	})

	t.Run("shutdown between backoff and next attempt", func(t *testing.T) {
		secondFactory := make(chan struct{})
		releaseFactory := make(chan struct{})
		var factoryCalls atomic.Int32
		h := &ProxyHandler{
			client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return nil, fmt.Errorf("temporary transport failure")
			})},
			log:            logger.NewWithWriter(logger.LevelDebug, io.Discard),
			stats:          newStatsCollector(),
			maxRetries:     2,
			retryBaseDelay: time.Nanosecond,
		}
		ctx, cancel := h.newInferenceUpstreamContext(false)
		defer cancel()
		ctx = markRetryStatsTracked(ctx)
		done := make(chan error, 1)
		go func() {
			_, err := h.doWithRetry(func() (*http.Request, error) {
				if factoryCalls.Add(1) == 2 {
					close(secondFactory)
					<-releaseFactory
				}
				return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/retry", nil)
			})
			done <- err
		}()
		<-secondFactory
		h.BeginShutdown()
		close(releaseFactory)
		if err := <-done; err != context.Canceled {
			t.Fatalf("doWithRetry() error = %v, want context.Canceled", err)
		}
		if snap := h.stats.snapshot(); snap.Retries != 0 || len(snap.RetriesByCode) != 0 {
			t.Fatalf("between-attempt cancellation retry stats = retries:%d by_code:%v, want zero", snap.Retries, snap.RetriesByCode)
		}
	})

	t.Run("real retry", func(t *testing.T) {
		var calls atomic.Int32
		h := &ProxyHandler{
			client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					return nil, fmt.Errorf("temporary transport failure")
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
					Request:    req,
				}, nil
			})},
			log:            logger.NewWithWriter(logger.LevelDebug, io.Discard),
			stats:          newStatsCollector(),
			maxRetries:     2,
			retryBaseDelay: time.Nanosecond,
		}
		ctx, cancel := h.newInferenceUpstreamContext(false)
		defer cancel()
		ctx = markRetryStatsTracked(ctx)
		resp, err := h.doWithRetry(func() (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/retry", nil)
		})
		if err != nil {
			t.Fatalf("doWithRetry() error = %v", err)
		}
		_ = resp.Body.Close()
		snap := h.stats.snapshot()
		if snap.Retries != 1 || len(snap.RetriesByCode) != 1 || snap.RetriesByCode[0].Label != "transport" || snap.RetriesByCode[0].Count != 1 {
			t.Fatalf("real retry stats = retries:%d by_code:%v, want one transport retry", snap.Retries, snap.RetriesByCode)
		}
	})
}

func TestDoWithRetryPreservesRetryableStatusWhenShutdownStopsRetry(t *testing.T) {
	for _, tt := range []struct {
		name       string
		status     int
		cancelWhen string
	}{
		{name: "429 before drain", status: http.StatusTooManyRequests, cancelWhen: "before-drain"},
		{name: "503 during backoff", status: http.StatusServiceUnavailable, cancelWhen: "backoff"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			responseReady := make(chan struct{})
			releaseResponse := make(chan struct{})
			body := newRetryBodyReadCloser(`{"error":{"message":"retry later"}}`)
			var calls atomic.Int32
			h := &ProxyHandler{
				client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls.Add(1)
					resp := &http.Response{
						StatusCode: tt.status,
						Header: http.Header{
							"Retry-After":       []string{"2"},
							"X-Upstream-Status": []string{"preserved"},
						},
						Body:    body,
						Request: req,
					}
					if tt.cancelWhen == "before-drain" {
						close(responseReady)
						<-releaseResponse
					}
					return resp, nil
				})},
				log:            logger.NewWithWriter(logger.LevelError, io.Discard),
				stats:          newStatsCollector(),
				maxRetries:     2,
				retryBaseDelay: time.Second,
			}
			ctx, cancel := h.newInferenceUpstreamContext(false)
			defer cancel()
			ctx = markRetryStatsTracked(ctx)
			done := make(chan error, 1)
			go func() {
				_, err := h.doWithRetry(func() (*http.Request, error) {
					return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/status-race", nil)
				})
				done <- err
			}()

			if tt.cancelWhen == "before-drain" {
				<-responseReady
				h.BeginShutdown()
				close(releaseResponse)
			} else {
				waitForRetryBodyClose(t, body.closed)
				h.BeginShutdown()
			}

			err := <-done
			var upstreamErr *upstreamError
			if !errors.As(err, &upstreamErr) {
				t.Fatalf("doWithRetry() error = %v, want upstreamError", err)
			}
			if upstreamErr.statusCode != tt.status {
				t.Fatalf("upstream status = %d, want %d", upstreamErr.statusCode, tt.status)
			}
			if upstreamErr.retryAfter != "2" || upstreamErr.headers.Get("X-Upstream-Status") != "preserved" {
				t.Fatalf("upstream metadata = retry-after:%q headers:%v", upstreamErr.retryAfter, upstreamErr.headers)
			}
			if errors.Is(err, context.Canceled) {
				t.Fatalf("retryable status was replaced by shutdown cancellation: %v", err)
			}
			if got := calls.Load(); got != 1 {
				t.Fatalf("Client.Do calls = %d, want 1", got)
			}
			waitForRetryBodyClose(t, body.closed)
			if snap := h.stats.snapshot(); snap.Retries != 0 || len(snap.RetriesByCode) != 0 {
				t.Fatalf("unmade retry stats = retries:%d by_code:%v", snap.Retries, snap.RetriesByCode)
			}

			req := httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
			requestCtx, summary := WithRequestSummary(req.Context())
			req = req.WithContext(requestCtx)
			if h.handleShutdownError(httptest.NewRecorder(), req, ctx, err) {
				t.Fatal("retryable upstream status was misclassified as local shutdown")
			}
			if summary.StatsSuppressed() {
				t.Fatal("retryable upstream status was stats-suppressed")
			}
		})
	}
}

func TestDoWithRetryPreservesPendingStatusWhenCanceledBeforeNextClientDo(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		status := status
		t.Run(strconv.Itoa(status), func(t *testing.T) {
			secondFactory := make(chan struct{})
			releaseFactory := make(chan struct{})
			var factoryCalls atomic.Int32
			var transportCalls atomic.Int32
			h := &ProxyHandler{
				client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					transportCalls.Add(1)
					return &http.Response{
						StatusCode: status,
						Header: http.Header{
							"X-Pending-Status": []string{"preserved"},
						},
						Body:    io.NopCloser(strings.NewReader(`{"error":"retry"}`)),
						Request: req,
					}, nil
				})},
				log:            logger.NewWithWriter(logger.LevelDebug, io.Discard),
				stats:          newStatsCollector(),
				maxRetries:     2,
				retryBaseDelay: time.Nanosecond,
			}
			ctx, cancel := h.newInferenceUpstreamContext(false)
			defer cancel()
			ctx = markRetryStatsTracked(ctx)
			done := make(chan error, 1)
			go func() {
				_, err := h.doWithRetry(func() (*http.Request, error) {
					if factoryCalls.Add(1) == 2 {
						close(secondFactory)
						<-releaseFactory
					}
					return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/pending-status", nil)
				})
				done <- err
			}()

			<-secondFactory
			h.BeginShutdown()
			close(releaseFactory)
			err := <-done
			var upstreamErr *upstreamError
			if !errors.As(err, &upstreamErr) || upstreamErr.statusCode != status {
				t.Fatalf("doWithRetry() error = %v, want upstream status %d", err, status)
			}
			if upstreamErr.headers.Get("X-Pending-Status") != "preserved" {
				t.Fatalf("pending upstream headers = %v", upstreamErr.headers)
			}
			if got := transportCalls.Load(); got != 1 {
				t.Fatalf("Client.Do calls = %d, want 1", got)
			}
			if snap := h.stats.snapshot(); snap.Retries != 0 || len(snap.RetriesByCode) != 0 {
				t.Fatalf("unmade retry stats = retries:%d by_code:%v", snap.Retries, snap.RetriesByCode)
			}
		})
	}
}

func TestDoWithRetryAccountsPendingOnlyAfterClientDo(t *testing.T) {
	t.Run("canceled retry discards pending accounting", func(t *testing.T) {
		writer := newBlockingRetryLogWriter()
		var calls atomic.Int32
		secondAttempt := make(chan struct{})
		h := &ProxyHandler{
			client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					return &http.Response{
						StatusCode: http.StatusServiceUnavailable,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"error":"retry"}`)),
						Request:    req,
					}, nil
				}
				close(secondAttempt)
				<-req.Context().Done()
				return nil, req.Context().Err()
			})},
			log:            logger.NewWithWriter(logger.LevelDebug, writer),
			stats:          newStatsCollector(),
			maxRetries:     2,
			retryBaseDelay: time.Nanosecond,
		}
		ctx, cancel := h.newInferenceUpstreamContext(false)
		defer cancel()
		ctx = markRetryStatsTracked(ctx)
		done := make(chan error, 1)
		go func() {
			_, err := h.doWithRetry(func() (*http.Request, error) {
				return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/accounting-race", nil)
			})
			done <- err
		}()

		select {
		case <-secondAttempt:
			h.BeginShutdown()
		case <-writer.started:
			// Old behavior logged before Client.Do. Release it after cancellation so
			// the test can observe the incorrect retry count instead of deadlocking.
			h.BeginShutdown()
			writer.releaseWrite()
		case <-time.After(time.Second):
			t.Fatal("neither retry Client.Do nor pending logger started")
		}
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("doWithRetry() error = %v, want context.Canceled", err)
		}
		writer.releaseWrite()
		if writer.wasStarted() {
			t.Fatal("pending retry was logged before a canceled Client.Do completed")
		}
		if snap := h.stats.snapshot(); snap.Retries != 0 || len(snap.RetriesByCode) != 0 {
			t.Fatalf("canceled retry stats = retries:%d by_code:%v, want zero", snap.Retries, snap.RetriesByCode)
		}
	})

	t.Run("genuine retry records once after Client.Do", func(t *testing.T) {
		var calls atomic.Int32
		var logs strings.Builder
		h := &ProxyHandler{
			client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					return &http.Response{
						StatusCode: http.StatusTooManyRequests,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"error":"retry"}`)),
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
			log:            logger.NewWithWriter(logger.LevelDebug, &logs),
			stats:          newStatsCollector(),
			maxRetries:     2,
			retryBaseDelay: time.Nanosecond,
		}
		ctx, cancel := h.newInferenceUpstreamContext(false)
		defer cancel()
		ctx = markRetryStatsTracked(ctx)
		resp, err := h.doWithRetry(func() (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/accounting-success", nil)
		})
		if err != nil {
			t.Fatalf("doWithRetry() error = %v", err)
		}
		_ = resp.Body.Close()
		if got := calls.Load(); got != 2 {
			t.Fatalf("Client.Do calls = %d, want 2", got)
		}
		snap := h.stats.snapshot()
		if snap.Retries != 1 || len(snap.RetriesByCode) != 1 || snap.RetriesByCode[0].Label != "429" || snap.RetriesByCode[0].Count != 1 {
			t.Fatalf("retry stats = retries:%d by_code:%v, want one 429", snap.Retries, snap.RetriesByCode)
		}
		if strings.Count(logs.String(), "retrying upstream request") != 1 {
			t.Fatalf("retry logs = %q, want exactly one retry log", logs.String())
		}
	})
}

type blockingRetryLogWriter struct {
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
}

func newBlockingRetryLogWriter() *blockingRetryLogWriter {
	return &blockingRetryLogWriter{started: make(chan struct{}), release: make(chan struct{})}
}

func (w *blockingRetryLogWriter) Write(p []byte) (int, error) {
	w.startedOnce.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

func (w *blockingRetryLogWriter) releaseWrite() {
	w.releaseOnce.Do(func() { close(w.release) })
}

func (w *blockingRetryLogWriter) wasStarted() bool {
	select {
	case <-w.started:
		return true
	default:
		return false
	}
}

func TestDoWithRetryClientDoErrorCancellationCausality(t *testing.T) {
	t.Run("transport owned deadline with live context retries", func(t *testing.T) {
		var calls atomic.Int32
		h := &ProxyHandler{
			client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					return nil, fmt.Errorf("dial timeout: %w", context.DeadlineExceeded)
				}
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     make(http.Header),
					Body:       io.NopCloser(strings.NewReader(`{"ok":true}`)),
					Request:    req,
				}, nil
			})},
			log:            logger.NewWithWriter(logger.LevelDebug, io.Discard),
			stats:          newStatsCollector(),
			maxRetries:     2,
			retryBaseDelay: time.Nanosecond,
		}
		ctx, cancel := h.newInferenceUpstreamContext(false)
		defer cancel()
		ctx = markRetryStatsTracked(ctx)
		resp, err := h.doWithRetry(func() (*http.Request, error) {
			return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/live-timeout", nil)
		})
		if err != nil {
			t.Fatalf("doWithRetry() error = %v", err)
		}
		_ = resp.Body.Close()
		if got := calls.Load(); got != 2 {
			t.Fatalf("Client.Do calls = %d, want 2", got)
		}
		snap := h.stats.snapshot()
		if snap.Retries != 1 || len(snap.RetriesByCode) != 1 || snap.RetriesByCode[0].Label != "transport" || snap.RetriesByCode[0].Count != 1 {
			t.Fatalf("retry stats = retries:%d by_code:%v, want one transport retry", snap.Retries, snap.RetriesByCode)
		}
	})

	t.Run("independent transport error survives later shutdown", func(t *testing.T) {
		transportErr := errors.New("independent connection reset")
		errorReady := make(chan struct{})
		releaseError := make(chan struct{})
		var calls atomic.Int32
		h := &ProxyHandler{
			client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
				calls.Add(1)
				close(errorReady)
				<-releaseError
				return nil, transportErr
			})},
			log:            logger.NewWithWriter(logger.LevelError, io.Discard),
			maxRetries:     2,
			retryBaseDelay: time.Second,
		}
		ctx, cancel := h.newInferenceUpstreamContext(false)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := h.doWithRetry(func() (*http.Request, error) {
				return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/race", nil)
			})
			done <- err
		}()

		<-errorReady
		h.BeginShutdown()
		close(releaseError)
		err := <-done
		if !errors.Is(err, transportErr) {
			t.Fatalf("doWithRetry() error = %v, want independent transport error", err)
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("independent transport error was replaced by context error: %v", err)
		}
		if got := calls.Load(); got != 1 {
			t.Fatalf("Client.Do calls = %d, want 1", got)
		}

		req := httptest.NewRequest(http.MethodGet, "/v1/chat/completions", nil)
		requestCtx, summary := WithRequestSummary(req.Context())
		req = req.WithContext(requestCtx)
		if h.handleShutdownError(httptest.NewRecorder(), req, ctx, err) {
			t.Fatal("independent provider error was misclassified as local shutdown")
		}
		if summary.StatsSuppressed() {
			t.Fatal("independent provider error was stats-suppressed")
		}
	})

	t.Run("transport cancellation returns context error", func(t *testing.T) {
		started := make(chan struct{})
		h := &ProxyHandler{
			client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				close(started)
				<-req.Context().Done()
				return nil, fmt.Errorf("request canceled: %w", req.Context().Err())
			})},
			log:        logger.NewWithWriter(logger.LevelError, io.Discard),
			maxRetries: 2,
		}
		ctx, cancel := h.newInferenceUpstreamContext(false)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := h.doWithRetry(func() (*http.Request, error) {
				return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/canceled", nil)
			})
			done <- err
		}()
		<-started
		h.BeginShutdown()
		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Fatalf("doWithRetry() error = %v, want context.Canceled", err)
		}
	})

	t.Run("transport deadline returns context error", func(t *testing.T) {
		started := make(chan struct{})
		h := &ProxyHandler{
			client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				close(started)
				<-req.Context().Done()
				return nil, fmt.Errorf("request deadline: %w", context.DeadlineExceeded)
			})},
			log:        logger.NewWithWriter(logger.LevelError, io.Discard),
			maxRetries: 2,
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		defer cancel()
		done := make(chan error, 1)
		go func() {
			_, err := h.doWithRetry(func() (*http.Request, error) {
				return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/deadline", nil)
			})
			done <- err
		}()
		<-started
		if err := <-done; !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("doWithRetry() error = %v, want context.DeadlineExceeded", err)
		}
	})
}

func TestDoWithRetryPendingStatusFactoryErrorCausality(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusServiceUnavailable} {
		status := status
		for _, tt := range []struct {
			name           string
			factoryErr     error
			wantPending    bool
			wantCredential bool
		}{
			{name: "lifecycle canceled auth", factoryErr: fmt.Errorf("auth refresh canceled: %w", context.Canceled), wantPending: true},
			{name: "independent credential failure", factoryErr: errors.New("credential refresh failed"), wantCredential: true},
		} {
			t.Run(fmt.Sprintf("%d/%s", status, tt.name), func(t *testing.T) {
				secondFactory := make(chan struct{})
				releaseFactory := make(chan struct{})
				var factoryCalls atomic.Int32
				var transportCalls atomic.Int32
				h := &ProxyHandler{
					client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
						transportCalls.Add(1)
						return &http.Response{
							StatusCode: status,
							Header:     http.Header{"X-Pending-Auth": []string{"preserved"}},
							Body:       io.NopCloser(strings.NewReader(`{"error":"retry"}`)),
							Request:    req,
						}, nil
					})},
					log:            logger.NewWithWriter(logger.LevelDebug, io.Discard),
					stats:          newStatsCollector(),
					maxRetries:     2,
					retryBaseDelay: time.Nanosecond,
				}
				ctx, cancel := h.newInferenceUpstreamContext(false)
				defer cancel()
				ctx = markRetryStatsTracked(ctx)
				done := make(chan error, 1)
				go func() {
					_, err := h.doWithRetry(func() (*http.Request, error) {
						if factoryCalls.Add(1) == 2 {
							close(secondFactory)
							<-releaseFactory
							return nil, tt.factoryErr
						}
						return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/auth-race", nil)
					})
					done <- err
				}()

				<-secondFactory
				h.BeginShutdown()
				close(releaseFactory)
				err := <-done
				if tt.wantPending {
					var upstreamErr *upstreamError
					if !errors.As(err, &upstreamErr) || upstreamErr.statusCode != status {
						t.Fatalf("doWithRetry() error = %v, want pending status %d", err, status)
					}
					if upstreamErr.headers.Get("X-Pending-Auth") != "preserved" {
						t.Fatalf("pending headers = %v", upstreamErr.headers)
					}
				} else if tt.wantCredential && (err == nil || !strings.Contains(err.Error(), "credential refresh failed")) {
					t.Fatalf("doWithRetry() error = %v, want credential failure", err)
				}
				if got := transportCalls.Load(); got != 1 {
					t.Fatalf("Client.Do calls = %d, want 1", got)
				}
				if snap := h.stats.snapshot(); snap.Retries != 0 || len(snap.RetriesByCode) != 0 {
					t.Fatalf("unmade retry stats = retries:%d by_code:%v", snap.Retries, snap.RetriesByCode)
				}
			})
		}
	}
}

func TestContextTerminationMatchesMatrix(t *testing.T) {
	lifecycleCtx, cancelLifecycle := context.WithCancelCause(context.Background())
	cancelLifecycle(errProxyLifecycleShutdown)
	deadlineCtx, cancelDeadline := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelDeadline()
	<-deadlineCtx.Done()
	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()

	for _, tt := range []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{name: "lifecycle canceled", ctx: lifecycleCtx, err: context.Canceled, want: true},
		{name: "lifecycle deadline mismatch", ctx: lifecycleCtx, err: context.DeadlineExceeded},
		{name: "deadline deadline", ctx: deadlineCtx, err: context.DeadlineExceeded, want: true},
		{name: "deadline canceled mismatch", ctx: deadlineCtx, err: context.Canceled},
		{name: "ordinary canceled", ctx: cancelCtx, err: context.Canceled, want: true},
		{name: "ordinary deadline mismatch", ctx: cancelCtx, err: context.DeadlineExceeded},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if got := contextTerminationMatches(tt.ctx, tt.err); got != tt.want {
				t.Fatalf("contextTerminationMatches() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDoWithRetryPendingAccountingTerminationMatrix(t *testing.T) {
	for _, tt := range []struct {
		name         string
		contextKind  string
		transportErr error
		wantRetry    int64
		wantCanceled bool
		wantDeadline bool
	}{
		{name: "lifecycle canceled preemption", contextKind: "lifecycle", transportErr: context.Canceled, wantCanceled: true},
		{name: "lifecycle transport deadline mismatch", contextKind: "lifecycle", transportErr: context.DeadlineExceeded, wantRetry: 1, wantDeadline: true},
		{name: "ordinary deadline", contextKind: "deadline", transportErr: context.DeadlineExceeded, wantRetry: 1, wantDeadline: true},
		{name: "deadline transport canceled mismatch", contextKind: "deadline", transportErr: context.Canceled, wantRetry: 1, wantCanceled: true},
		{name: "ordinary cancel", contextKind: "cancel", transportErr: context.Canceled, wantRetry: 1, wantCanceled: true},
		{name: "cancel transport deadline mismatch", contextKind: "cancel", transportErr: context.DeadlineExceeded, wantRetry: 1, wantDeadline: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var calls atomic.Int32
			secondStarted := make(chan struct{})
			var h *ProxyHandler
			var ctx context.Context
			var cancel context.CancelFunc
			h = &ProxyHandler{
				log:            logger.NewWithWriter(logger.LevelDebug, io.Discard),
				stats:          newStatsCollector(),
				maxRetries:     2,
				retryBaseDelay: time.Nanosecond,
			}
			switch tt.contextKind {
			case "lifecycle":
				ctx, cancel = h.newInferenceUpstreamContext(false)
			case "deadline":
				ctx, cancel = context.WithTimeout(context.Background(), 20*time.Millisecond)
			case "cancel":
				ctx, cancel = context.WithCancel(context.Background())
			default:
				t.Fatalf("unknown context kind %q", tt.contextKind)
			}
			defer cancel()
			ctx = markRetryStatsTracked(ctx)
			h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					return &http.Response{
						StatusCode: http.StatusServiceUnavailable,
						Header:     make(http.Header),
						Body:       io.NopCloser(strings.NewReader(`{"error":"retry"}`)),
						Request:    req,
					}, nil
				}
				close(secondStarted)
				switch tt.contextKind {
				case "lifecycle":
					h.BeginShutdown()
				case "deadline":
					<-req.Context().Done()
				case "cancel":
					cancel()
				}
				return nil, tt.transportErr
			})}

			_, err := h.doWithRetry(func() (*http.Request, error) {
				return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/termination-matrix", nil)
			})
			<-secondStarted
			if tt.wantCanceled && !errors.Is(err, context.Canceled) {
				t.Fatalf("doWithRetry() error = %v, want context.Canceled class", err)
			}
			if tt.wantDeadline && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("doWithRetry() error = %v, want DeadlineExceeded class", err)
			}
			snap := h.stats.snapshot()
			if snap.Retries != tt.wantRetry {
				t.Fatalf("Retries = %d, want %d", snap.Retries, tt.wantRetry)
			}
			if tt.wantRetry == 1 {
				if len(snap.RetriesByCode) != 1 || snap.RetriesByCode[0].Label != "503" || snap.RetriesByCode[0].Count != 1 {
					t.Fatalf("RetriesByCode = %v, want one 503", snap.RetriesByCode)
				}
			} else if len(snap.RetriesByCode) != 0 {
				t.Fatalf("RetriesByCode = %v, want none", snap.RetriesByCode)
			}
			if got := calls.Load(); got != 2 {
				t.Fatalf("Client.Do calls = %d, want 2", got)
			}
		})
	}
}
