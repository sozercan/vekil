package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
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

	resp, err := h.doWithRetry(requestMetricLabels{}, func() (*http.Request, error) {
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

	resp, err := h.doWithRetry(requestMetricLabels{}, func() (*http.Request, error) {
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

	_, err := h.doWithRetry(requestMetricLabels{}, func() (*http.Request, error) {
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

	resp, err := h.doWithRetry(requestMetricLabels{}, func() (*http.Request, error) {
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
		{http.StatusTooManyRequests, true},
		{http.StatusBadGateway, true},
		{http.StatusServiceUnavailable, true},
		{http.StatusGatewayTimeout, true},
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

	_, err := h.doWithRetry(requestMetricLabels{}, func() (*http.Request, error) {
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
	resp, err := h.doWithRetry(requestMetricLabels{}, func() (*http.Request, error) {
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
		// HTTP-date is intentionally not supported.
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
