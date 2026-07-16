package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sozercan/vekil/logger"
)

type retryPublicModelContextKey struct{}

type retryPublicModelInfo struct {
	model string
	known bool
}

func withRetryPublicModel(ctx context.Context, model string, known bool) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, retryPublicModelContextKey{}, retryPublicModelInfo{model: boundStatLabel(strings.TrimSpace(model)), known: known})
}

func retryMetricLabelsFromContext(ctx context.Context) (provider, model string, modelKnown bool) {
	if ctx == nil {
		return "", "", false
	}
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		d := readSummaryForStats(summary)
		provider, model, modelKnown = d.provider, d.metricModel, d.modelKnown
	}
	if route, ok := ctx.Value(providerRouteContextKey{}).(providerRouteInfo); ok && route.id != "" {
		provider = route.id
	}
	if publicModel, _ := ctx.Value(retryPublicModelContextKey{}).(retryPublicModelInfo); publicModel.model != "" {
		model, modelKnown = publicModel.model, publicModel.known
	}
	return provider, model, modelKnown
}

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
	if seconds, ok := parsePositiveDecimalClamped(value, int64(maxRetryAfter/time.Second)); ok {
		return retryAfterDurationFromSeconds(seconds), true
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

func retryAfterDurationFromSeconds(seconds int64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	maxSeconds := int64(maxRetryAfter / time.Second)
	if seconds >= maxSeconds {
		return maxRetryAfter
	}
	return time.Duration(seconds) * time.Second
}

func parsePositiveDecimalClamped(value string, max int64) (int64, bool) {
	if value == "" || max <= 0 {
		return 0, false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return 0, false
		}
	}
	var parsed int64
	for i := 0; i < len(value); i++ {
		digit := int64(value[i] - '0')
		if parsed > (max-digit)/10 {
			return max, true
		}
		parsed = parsed*10 + digit
	}
	if parsed <= 0 {
		return 0, false
	}
	return parsed, true
}

func retryAfterDurationFromMilliseconds(milliseconds int64) time.Duration {
	if milliseconds <= 0 {
		return 0
	}
	maxMilliseconds := int64(maxRetryAfter / time.Millisecond)
	if milliseconds >= maxMilliseconds {
		return maxRetryAfter
	}
	return time.Duration(milliseconds) * time.Millisecond
}

func durationSecondsCeil(delay time.Duration) int64 {
	if delay <= 0 {
		return 0
	}
	seconds := int64(delay / time.Second)
	if delay%time.Second != 0 {
		seconds++
	}
	return seconds
}

// drainAndClose discards up to 4 KB from the body before closing it so that
// HTTP/2 streams are cleanly consumed and the underlying connection can be
// reused instead of being reset. The read is time-bounded because a response
// body can yield a short prefix and then stall indefinitely.
func drainAndClose(body io.ReadCloser) {
	_ = captureAndDrainResponseBody(body, upstreamErrorDetailMaxBodyBytes, 0)
}

// drainReaderAndClose consumes reader through EOF before closing body so an
// HTTP/1.x transport can reuse a normally completed response connection. If the
// reader stalls, the existing upstream-body timeout closes it and lets the
// caller return without waiting indefinitely. The reader may wrap body (for
// example, a bufio.Reader with already-buffered bytes), so callers must pass the
// reader they were actively consuming rather than body directly.
func drainReaderAndClose(reader io.Reader, body io.ReadCloser) {
	if body == nil {
		return
	}
	if reader == nil {
		_ = body.Close()
		return
	}

	var closeOnce sync.Once
	closeBody := func() {
		closeOnce.Do(func() { _ = body.Close() })
	}

	timedOut := make(chan struct{})
	timer := time.AfterFunc(upstreamErrorDetailDrainTimeout, func() {
		// Signal first so a pathological Close implementation cannot extend the
		// caller-visible timeout. net/http response bodies unblock Read on Close.
		close(timedOut)
		closeBody()
	})

	drained := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, reader)
		_ = timer.Stop()
		closeBody()
		close(drained)
	}()

	select {
	case <-drained:
	case <-timedOut:
	}
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

	type pendingRetryAttempt struct {
		attempt     int
		status      int
		retryAfter  string
		delay       time.Duration
		err         error
		upstreamErr *upstreamError
		requestCtx  context.Context
	}
	var lastErr error
	var pending *pendingRetryAttempt
	for attempt := range maxRetries {
		req, err := reqFactory()
		if err != nil {
			if pending != nil && pending.upstreamErr != nil && pending.requestCtx != nil &&
				errors.Is(err, context.Canceled) && errors.Is(context.Cause(pending.requestCtx), errProxyLifecycleShutdown) {
				return nil, pending.upstreamErr
			}
			return nil, err
		}
		if ctxErr := req.Context().Err(); ctxErr != nil {
			if pending != nil && pending.upstreamErr != nil {
				return nil, pending.upstreamErr
			}
			return nil, ctxErr
		}

		observeUpstreamAttempt(req.Context())
		resp, err := h.client.Do(req)
		lifecyclePreempted := errors.Is(context.Cause(req.Context()), errProxyLifecycleShutdown) &&
			contextTerminationMatches(req.Context(), err)
		if pending != nil && !lifecyclePreempted {
			retryCtx := pending.requestCtx
			if retryCtx == nil {
				retryCtx = req.Context()
			}
			h.logRetryAttempt(retryCtx, pending.attempt, pending.status, pending.retryAfter, pending.delay, pending.err)
		}
		pending = nil
		if err != nil {
			if contextTerminationMatches(req.Context(), err) {
				return nil, req.Context().Err()
			}
			lastErr = err
			if permanentTransportError(err) {
				return nil, err
			}
			if attempt < maxRetries-1 {
				delay := backoff(retryDelay, attempt)
				if req.Context().Err() != nil {
					return nil, err
				}
				if ctxErr := sleepWithContext(req.Context(), delay); ctxErr != nil {
					return nil, ctxErr
				}
				if ctxErr := req.Context().Err(); ctxErr != nil {
					return nil, ctxErr
				}
				pending = &pendingRetryAttempt{attempt: attempt, delay: delay, err: err, requestCtx: req.Context()}
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
			if req.Context().Err() != nil {
				drainAndClose(resp.Body)
				return nil, upstreamErr
			}
			// Drain and close body before retry to allow connection reuse.
			drainAndClose(resp.Body)
			if req.Context().Err() != nil {
				return nil, upstreamErr
			}
			delay := backoff(retryDelay, attempt)
			if ra, ok := parseRetryAfter(retryAfterHeader); ok && ra > delay {
				delay = ra
			}
			if req.Context().Err() != nil {
				return nil, upstreamErr
			}
			if ctxErr := sleepWithContext(req.Context(), delay); ctxErr != nil {
				return nil, upstreamErr
			}
			if req.Context().Err() != nil {
				return nil, upstreamErr
			}
			pending = &pendingRetryAttempt{
				attempt:     attempt,
				status:      resp.StatusCode,
				retryAfter:  retryAfterHeader,
				delay:       delay,
				upstreamErr: upstreamErr,
				requestCtx:  req.Context(),
			}
		} else {
			upstreamErr.body = readRetryableUpstreamErrorBody(resp.Body)
		}
	}
	return nil, lastErr
}

func contextTerminationMatches(ctx context.Context, err error) bool {
	if ctx == nil || ctx.Err() == nil || err == nil {
		return false
	}
	errCanceled := errors.Is(err, context.Canceled)
	errDeadline := errors.Is(err, context.DeadlineExceeded)
	if errors.Is(context.Cause(ctx), errProxyLifecycleShutdown) {
		return errCanceled && !errDeadline
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errDeadline
	}
	if errors.Is(ctx.Err(), context.Canceled) {
		return errCanceled && !errDeadline
	}
	return false
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
	if h != nil && h.metrics != nil && isRetryStatsTracked(ctx) {
		provider, model, modelKnown := retryMetricLabelsFromContext(ctx)
		h.metrics.recordRetry(provider, model, status, modelKnown)
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
	return captureAndDrainResponseBody(body, upstreamErrorDetailMaxBodyBytes, upstreamErrorDetailDrainBytes)
}

type responseBodyReadProgress struct {
	captured        []byte
	captureComplete bool
}

// captureAndDrainResponseBody captures up to captureBytes and then drains up to
// drainBytes more before closing the body. It returns as soon as capture is
// complete, EOF is reached, or the timeout expires; any remaining bounded drain
// continues asynchronously so normal short responses can still leave their
// connection reusable. Progress is sent as owned byte slices so a timed-out
// caller can safely return the short prefix already received while Close
// interrupts a stalled Read.
func captureAndDrainResponseBody(body io.ReadCloser, captureBytes, drainBytes int) []byte {
	if body == nil {
		return nil
	}
	if captureBytes <= 0 {
		_ = body.Close()
		return nil
	}
	if drainBytes < 0 {
		drainBytes = 0
	}

	var closeOnce sync.Once
	closeBody := func() {
		closeOnce.Do(func() { _ = body.Close() })
	}

	timedOut := make(chan struct{})
	timer := time.AfterFunc(upstreamErrorDetailDrainTimeout, func() {
		// Signal first so a pathological Close implementation cannot extend the
		// caller-visible bound. net/http response bodies unblock Read on Close.
		close(timedOut)
		closeBody()
	})

	progress := make(chan responseBodyReadProgress)
	callerDone := make(chan struct{})
	defer close(callerDone)

	go func() {
		defer func() {
			_ = timer.Stop()
			closeBody()
		}()

		captureRemaining := captureBytes
		totalRemaining := captureBytes + drainBytes
		buf := make([]byte, min(totalRemaining, 32*1024))
		captureComplete := false

		sendProgress := func(captured []byte, complete bool) {
			select {
			case progress <- responseBodyReadProgress{captured: captured, captureComplete: complete}:
			case <-callerDone:
			}
		}

		for totalRemaining > 0 {
			readSize := min(len(buf), totalRemaining)
			n, err := body.Read(buf[:readSize])
			if n > 0 {
				if n > totalRemaining {
					n = totalRemaining
				}
				totalRemaining -= n

				captureCount := min(n, captureRemaining)
				var captured []byte
				if captureCount > 0 {
					captured = append([]byte(nil), buf[:captureCount]...)
					captureRemaining -= captureCount
				}
				complete := captureRemaining == 0
				if complete && totalRemaining == 0 {
					closeBody()
				}
				if captureCount > 0 || (complete && !captureComplete) {
					sendProgress(captured, complete)
				}
				captureComplete = complete
			}

			if err != nil {
				closeBody()
				if !captureComplete {
					sendProgress(nil, true)
				}
				return
			}
		}

		if !captureComplete {
			closeBody()
			sendProgress(nil, true)
		}
	}()

	captured := make([]byte, 0, captureBytes)
	for {
		select {
		case update := <-progress:
			captured = append(captured, update.captured...)
			if update.captureComplete {
				return captured
			}
		case <-timedOut:
			return captured
		}
	}
}
