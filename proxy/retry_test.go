package proxy

import (
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/copilot-proxy/auth"
	"github.com/sozercan/copilot-proxy/logger"
)

func TestDoWithRetry_SuccessOnFirstTry(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"ok":true}`))
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
	defer resp.Body.Close()

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
		w.Write([]byte(`{"ok":true}`))
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
	defer resp.Body.Close()

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
	defer resp.Body.Close()

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
