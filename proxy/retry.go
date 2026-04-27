package proxy

import (
	"context"
	"io"
	"math/rand/v2"
	"net/http"
	"strconv"
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

// parseRetryAfter extracts a delay from a Retry-After header value.
// It supports both delay-seconds ("120") and is intentionally simple —
// HTTP-date values are ignored and fall back to the caller's default.
func parseRetryAfter(value string) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	seconds, err := strconv.Atoi(value)
	if err != nil || seconds <= 0 {
		return 0, false
	}
	return time.Duration(seconds) * time.Second, true
}

// drainAndClose discards up to 4 KB from the body before closing it so that
// HTTP/2 streams are cleanly consumed and the underlying connection can be
// reused instead of being reset.
func drainAndClose(body io.ReadCloser) {
	_, _ = io.Copy(io.Discard, io.LimitReader(body, 4096))
	_ = body.Close()
}

// doWithRetry executes an HTTP request with retry on transient failures.
// The reqFactory is called on each attempt to produce a fresh request body.
func (h *ProxyHandler) doWithRetry(labels requestMetricLabels, reqFactory func() (*http.Request, error)) (*http.Response, error) {
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
				if h.metrics != nil {
					h.metrics.observeRetry(labels, metricReasonFromError(err))
				}
				if ctxErr := sleepWithContext(req.Context(), backoff(retryDelay, attempt)); ctxErr != nil {
					return nil, ctxErr
				}
			}
			continue
		}

		if !retryable(resp.StatusCode) {
			return resp, nil
		}

		retryAfterHeader := resp.Header.Get("Retry-After")

		// Drain and close body before retry to allow connection reuse.
		drainAndClose(resp.Body)
		lastErr = &upstreamError{statusCode: resp.StatusCode}

		if attempt < maxRetries-1 {
			if h.metrics != nil {
				h.metrics.observeRetry(labels, retryReasonFromStatus(resp.StatusCode))
			}
			delay := backoff(retryDelay, attempt)
			if ra, ok := parseRetryAfter(retryAfterHeader); ok && ra > delay {
				delay = ra
			}
			if ctxErr := sleepWithContext(req.Context(), delay); ctxErr != nil {
				return nil, ctxErr
			}
		}
	}
	return nil, lastErr
}

// sleepWithContext blocks for the given duration or until the context is done,
// whichever comes first. It returns the context error if cancelled.
func sleepWithContext(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

type upstreamError struct {
	statusCode int
}

func (e *upstreamError) Error() string {
	return http.StatusText(e.statusCode)
}
