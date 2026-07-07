package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sozercan/vekil/logger"
)

const (
	upstreamErrorDetailDrainTimeout = 250 * time.Millisecond
	maxRetryBackoff                 = 30 * time.Second
	maxRetryAfter                   = 5 * time.Minute
)

// retryable returns true for status codes that warrant a retry.
func retryable(statusCode int) bool {
	switch statusCode {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		529, // Anthropic overloaded_error
		520, // Cloudflare: web server returned an unknown error
		521, // Cloudflare: web server is down
		522, // Cloudflare: connection timed out
		523, // Cloudflare: origin is unreachable
		524, // Cloudflare: a timeout occurred
		530: // Cloudflare: origin DNS / origin error
		return true
	}
	return false
}

// backoff returns the delay for the given attempt (0-indexed) with jitter.
func backoff(base time.Duration, attempt int) time.Duration {
	if base <= 0 {
		base = time.Nanosecond
	}
	if attempt < 0 {
		attempt = 0
	}

	delay := base
	for range attempt {
		if delay >= maxRetryBackoff || delay > maxRetryBackoff/2 {
			delay = maxRetryBackoff
			break
		}
		delay *= 2
	}
	if delay > maxRetryBackoff {
		delay = maxRetryBackoff
	}

	jitterBound := delay / 4
	if jitterBound <= 0 {
		return delay
	}
	return delay + time.Duration(rand.Int64N(int64(jitterBound)))
}

// parseRetryAfter extracts a delay from a Retry-After header value.
// It supports both delay-seconds ("120") and HTTP-date values.
func parseRetryAfter(value string) (time.Duration, bool) {
	if value == "" {
		return 0, false
	}
	seconds, err := strconv.Atoi(value)
	if err == nil {
		if seconds <= 0 {
			return 0, false
		}
		return clampRetryAfter(time.Duration(seconds) * time.Second), true
	}

	retryAt, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := time.Until(retryAt)
	if delay <= 0 {
		return 0, false
	}
	return clampRetryAfter(delay), true
}

func clampRetryAfter(delay time.Duration) time.Duration {
	if delay > maxRetryAfter {
		return maxRetryAfter
	}
	return delay
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
			if permanentTransportError(err) {
				return nil, err
			}
			if attempt < maxRetries-1 {
				delay := backoff(retryDelay, attempt)
				h.logRetryAttempt(req.Context(), attempt, 0, "", delay, err)
				if ctxErr := sleepWithContext(req.Context(), delay); ctxErr != nil {
					return nil, ctxErr
				}
			}
			continue
		}

		if !retryable(resp.StatusCode) {
			return resp, nil
		}

		retryAfterHeader := resp.Header.Get("Retry-After")
		upstreamErr := &upstreamError{
			statusCode: resp.StatusCode,
			retryAfter: retryAfterHeader,
			headers:    resp.Header.Clone(),
		}
		lastErr = upstreamErr

		if attempt < maxRetries-1 {
			// Drain and close body before retry to allow connection reuse.
			drainAndClose(resp.Body)
			delay := backoff(retryDelay, attempt)
			if ra, ok := parseRetryAfter(retryAfterHeader); ok && ra > delay {
				delay = ra
			}
			h.logRetryAttempt(req.Context(), attempt, resp.StatusCode, retryAfterHeader, delay, nil)
			if ctxErr := sleepWithContext(req.Context(), delay); ctxErr != nil {
				return nil, ctxErr
			}
		} else {
			upstreamErr.body = readRetryableUpstreamErrorBody(resp.Body)
		}
	}
	return nil, lastErr
}

// doWithRetryMultiEndpoint is like doWithRetry but picks a different endpoint
// on each retry attempt. The reqFactoryForEndpoint produces a reqFactory closure
// for the given endpoint. On success, the endpoint's health tracker is updated.
func (h *ProxyHandler) doWithRetryMultiEndpoint(provider *providerRuntime, reqFactoryForEndpoint func(ep *providerEndpoint) func() (*http.Request, error)) (*http.Response, error) {
	maxRetries := h.maxRetries
	if maxRetries == 0 {
		maxRetries = 3
	}
	retryDelay := h.retryBaseDelay
	if retryDelay == 0 {
		retryDelay = 1 * time.Second
	}

	var lastErr error
	var lastEndpoint *providerEndpoint

	for attempt := range maxRetries {
		var ep *providerEndpoint
		if attempt == 0 || lastEndpoint == nil {
			ep = provider.pickEndpoint()
		} else {
			ep = provider.pickEndpointExcluding(lastEndpoint)
		}
		if ep == nil {
			return nil, &providerRequestError{statusCode: http.StatusServiceUnavailable, err: fmt.Errorf("no endpoints available for provider %q", provider.id)}
		}
		lastEndpoint = ep

		reqFactory := reqFactoryForEndpoint(ep)
		req, err := reqFactory()
		if err != nil {
			return nil, err
		}

		startTime := time.Now()
		resp, err := h.client.Do(req)
		latency := time.Since(startTime)

		if err != nil {
			lastErr = err
			ep.health.RecordFailure()
			if h.log != nil {
				h.log.Debug("endpoint request failed", logger.F("endpoint", ep.name), logger.Err(err))
			}
			if permanentTransportError(err) {
				return nil, err
			}
			if attempt < maxRetries-1 {
				delay := backoff(retryDelay, attempt)
				h.logRetryAttemptWithEndpoint(req.Context(), attempt, 0, "", delay, err, ep.name)
				if ctxErr := sleepWithContext(req.Context(), delay); ctxErr != nil {
					return nil, ctxErr
				}
			}
			continue
		}

		if !retryable(resp.StatusCode) {
			ep.health.RecordSuccess()
			// Update EWMA latency on the selector endpoint.
			updateEndpointLatencyEWMA(ep, latency)
			return resp, nil
		}

		// Retryable status: record failure.
		ep.health.RecordFailure()

		retryAfterHeader := resp.Header.Get("Retry-After")
		upstreamErr := &upstreamError{
			statusCode: resp.StatusCode,
			retryAfter: retryAfterHeader,
			headers:    resp.Header.Clone(),
		}
		lastErr = upstreamErr

		if attempt < maxRetries-1 {
			drainAndClose(resp.Body)
			delay := backoff(retryDelay, attempt)
			if ra, ok := parseRetryAfter(retryAfterHeader); ok && ra > delay {
				delay = ra
			}
			h.logRetryAttemptWithEndpoint(req.Context(), attempt, resp.StatusCode, retryAfterHeader, delay, nil, ep.name)
			if ctxErr := sleepWithContext(req.Context(), delay); ctxErr != nil {
				return nil, ctxErr
			}
		} else {
			upstreamErr.body = readRetryableUpstreamErrorBody(resp.Body)
		}
	}
	return nil, lastErr
}

// updateEndpointLatencyEWMA updates the EWMA latency on the selector endpoint.
// Uses a smoothing factor of 0.3 (recent observations have ~30% weight).
func updateEndpointLatencyEWMA(ep *providerEndpoint, latency time.Duration) {
	const alpha = 0.3
	if ep.sel.LatencyEWMA == 0 {
		ep.sel.LatencyEWMA = latency
	} else {
		ep.sel.LatencyEWMA = time.Duration(alpha*float64(latency) + (1-alpha)*float64(ep.sel.LatencyEWMA))
	}
}

func (h *ProxyHandler) logRetryAttemptWithEndpoint(ctx context.Context, attempt int, status int, retryAfter string, delay time.Duration, err error, endpoint string) {
	if h != nil && h.stats != nil && isRetryStatsTracked(ctx) {
		h.stats.incRetry(status)
	}
	if h == nil || h.log == nil {
		return
	}
	fields := []logger.Field{
		logger.F("attempt", attempt),
		logger.F("delay", delay.String()),
		logger.F("endpoint", endpoint),
	}
	if status != 0 {
		fields = append(fields, logger.F("status", status))
	}
	if retryAfter != "" {
		fields = append(fields, logger.F("retry_after", retryAfter))
	}
	if err != nil {
		fields = append(fields, logger.Err(err))
	}
	h.log.Debug("retrying upstream request", fields...)
}

func (h *ProxyHandler) logRetryAttempt(ctx context.Context, attempt int, status int, retryAfter string, delay time.Duration, err error) {
	// Count a retry only when it was made on behalf of a tracked inference
	// request. The upstream request context carries the retry-stats marker iff
	// the middleware set it for a tracked route and newInferenceUpstreamContext
	// propagated it; non-tracked callers (the in-process /dashboard/insight call,
	// the GET /v1/models catalog fetch, count-token probes, the proxy shims)
	// build their upstream context without the marker, so their retries are not
	// folded into the dashboard's retry stats.
	if h != nil && h.stats != nil && isRetryStatsTracked(ctx) {
		h.stats.incRetry(status)
	}
	if h == nil || h.log == nil {
		return
	}
	fields := []logger.Field{
		logger.F("attempt", attempt),
		logger.F("delay", delay.String()),
	}
	if status != 0 {
		fields = append(fields, logger.F("status", status))
	}
	if retryAfter != "" {
		fields = append(fields, logger.F("retry_after", retryAfter))
	}
	if err != nil {
		fields = append(fields, logger.Err(err))
	}
	h.log.Debug("retrying upstream request", fields...)
}

func permanentTransportError(err error) bool {
	if err == nil {
		return false
	}

	var certVerifyErr *tls.CertificateVerificationError
	if errors.As(err, &certVerifyErr) {
		return true
	}
	var unknownAuthorityErr x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthorityErr) {
		return true
	}
	var hostnameErr x509.HostnameError
	if errors.As(err, &hostnameErr) {
		return true
	}
	var certInvalidErr x509.CertificateInvalidError
	if errors.As(err, &certInvalidErr) {
		return true
	}

	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) && dnsErr.IsNotFound {
		return true
	}

	return strings.Contains(strings.ToLower(err.Error()), "unsupported protocol scheme")
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
	body       []byte
	retryAfter string
	headers    http.Header
}

func (e *upstreamError) Error() string {
	if e == nil {
		return ""
	}
	return formatUpstreamErrorMessage(e.statusCode, e.body)
}

func readRetryableUpstreamErrorBody(body io.ReadCloser) []byte {
	if body == nil {
		return nil
	}
	bodyBytes, _ := io.ReadAll(io.LimitReader(body, upstreamErrorDetailMaxBodyBytes))
	drainRetryableUpstreamErrorBody(body)
	return bodyBytes
}

func drainRetryableUpstreamErrorBody(body io.ReadCloser) {
	// Drain a bounded remainder after the bounded capture. This preserves
	// connection reuse for normal upstream error bodies without letting a huge or
	// stalled body delay returning the synthesized error indefinitely.
	go func() {
		var closeOnce sync.Once
		closeBody := func() {
			closeOnce.Do(func() { _ = body.Close() })
		}

		timer := time.AfterFunc(upstreamErrorDetailDrainTimeout, closeBody)
		_, _ = io.Copy(io.Discard, io.LimitReader(body, upstreamErrorDetailDrainBytes))
		_ = timer.Stop()
		closeBody()
	}()
}
