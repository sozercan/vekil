package proxy

import (
	"math/rand/v2"
	"net/http"
	"time"
)

// retryable returns true for status codes that warrant a retry.
func retryable(statusCode int) bool {
	switch statusCode {
	case http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// backoff returns the delay for the given attempt (0-indexed) with jitter.
func backoff(base time.Duration, attempt int) time.Duration {
	delay := base << uint(attempt)
	jitter := time.Duration(rand.Int64N(int64(delay / 4)))
	return delay + jitter
}

// doWithRetry executes an HTTP request with retry on transient failures.
// The reqFactory is called on each attempt to produce a fresh request body.
func (h *ProxyHandler) doWithRetry(reqFactory func() (*http.Request, error)) (*http.Response, error) {
	maxRetries := h.maxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}
	retryDelay := h.retryBaseDelay
	if retryDelay == 0 {
		retryDelay = 1 * time.Second
	}

	var lastErr error
	for attempt := range maxRetries {
		req, err := reqFactory()
		if err != nil {
			return nil, err
		}

		resp, err := h.client.Do(req)
		if err != nil {
			lastErr = err
			if attempt < maxRetries-1 {
				time.Sleep(backoff(retryDelay, attempt))
			}
			continue
		}

		if !retryable(resp.StatusCode) {
			return resp, nil
		}

		// Drain and close body before retry
		resp.Body.Close()
		lastErr = &upstreamError{statusCode: resp.StatusCode}

		if attempt < maxRetries-1 {
			time.Sleep(backoff(retryDelay, attempt))
		}
	}
	return nil, lastErr
}

type upstreamError struct {
	statusCode int
}

func (e *upstreamError) Error() string {
	return http.StatusText(e.statusCode)
}
