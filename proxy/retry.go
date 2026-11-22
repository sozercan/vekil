package proxy

import (
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sozercan/vekil/logger"
)

const (
	upstreamErrorDetailDrainTimeout = 250 * time.Millisecond
	maxRetryBackoff                 = 30 * time.Second
	maxRetryWaitWithoutDeadline     = 5 * time.Minute
	maxRetryAfterDuration           = time.Duration(1<<63 - 1)
)

func retryable(statusCode int) bool {
	switch statusCode {
	case http.StatusRequestTimeout,
		http.StatusTooManyRequests,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
		529,
		520,
		521,
		522,
		523,
		524,
		530:
		return true
	}
	return false
}

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

func parseRetryAfter(value string) (time.Duration, bool) {
	return parseRetryAfterAt(value, time.Now())
}

func parseRetryAfterAt(value string, now time.Time) (time.Duration, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if seconds, ok := parsePositiveDecimalClamped(value, int64(maxRetryAfterDuration/time.Second)+1); ok {
		return retryAfterDurationFromSeconds(seconds), true
	}

	retryAt, err := http.ParseTime(value)
	if err != nil {
		return 0, false
	}
	delay := retryAt.Sub(now)
	if delay <= 0 {
		return 0, false
	}
	return delay, true
}

func retryAfterDurationFromSeconds(seconds int64) time.Duration {
	if seconds <= 0 {
		return 0
	}
	maxSeconds := int64(maxRetryAfterDuration / time.Second)
	if seconds > maxSeconds {
		return maxRetryAfterDuration
	}
	return time.Duration(seconds) * time.Second
}

func retryDelayFitsBudget(ctx context.Context, delay time.Duration) bool {
	if ctx.Err() != nil {
		return false
	}
	if deadline, ok := ctx.Deadline(); ok {
		return delay < time.Until(deadline)
	}
	return delay <= maxRetryWaitWithoutDeadline
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

func drainAndClose(body io.ReadCloser) {
	_ = captureAndDrainResponseBody(body, upstreamErrorDetailMaxBodyBytes, 0)
}

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

func (h *ProxyHandler) doWithRetry(reqFactory func() (*http.Request, error)) (*http.Response, error) {
	return h.doWithRetryMode(reqFactory, false)
}

func (h *ProxyHandler) doInferenceWithRetry(reqFactory func() (*http.Request, error)) (*http.Response, error) {
	return h.doWithRetryMode(reqFactory, true)
}

func (h *ProxyHandler) doWithRetryMode(reqFactory func() (*http.Request, error), inference bool) (*http.Response, error) {
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

		var permit *copilotInferencePermit
		if inference {
			if rejected := h.maybeRejectNativeResponsesRequest(req); rejected != nil {
				if req.Body != nil {
					_ = req.Body.Close()
				}
				return rejected, nil
			}
			var blocked *http.Response
			permit, blocked, err = h.acquireCopilotInference(req)
			if err != nil || blocked != nil {
				if req.Body != nil {
					_ = req.Body.Close()
				}
				if err != nil && pending != nil && pending.upstreamErr != nil &&
					(errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
					return nil, pending.upstreamErr
				}
				return blocked, err
			}
		}
		resp, err := h.sendRetryRequest(req, inference)
		h.finishCopilotInference(req, resp, err, permit)
		if err == nil {
			normalizeResponsesCitationMarkers(req, resp)
		}
		lifecyclePreempted := errors.Is(context.Cause(req.Context()), errProxyLifecycleShutdown) &&
			contextTerminationMatches(req.Context(), err)
		if pending != nil && !lifecyclePreempted {
			h.logRetryAttempt(req.Context(), pending.attempt, pending.status, pending.retryAfter, pending.delay, pending.err)
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
				if !retryDelayFitsBudget(req.Context(), delay) {
					return nil, err
				}
				if ctxErr := sleepWithContext(req.Context(), delay); ctxErr != nil {
					return nil, ctxErr
				}
				if ctxErr := req.Context().Err(); ctxErr != nil {
					return nil, ctxErr
				}
				pending = &pendingRetryAttempt{attempt: attempt, delay: delay, err: err}
			}
			continue
		}

		if !retryable(resp.StatusCode) {
			return resp, nil
		}

		retryAfterHeader := resp.Header.Get("Retry-After")
		retryAfterDelay, _ := selectResponsesRetryAfter(resp.Header)
		if _, valid := retryAfterHeaderSeconds(retryAfterHeader); !valid {
			retryAfterHeader = retryAfterDelay
			if retryAfterHeader != "" {
				resp.Header.Set("Retry-After", retryAfterHeader)
			}
		}
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
			delay := backoff(retryDelay, attempt)
			if ra, ok := parseRetryAfter(retryAfterDelay); ok && ra > delay {
				delay = ra
			}
			if !retryDelayFitsBudget(req.Context(), delay) {
				return resp, nil
			}
			drainAndClose(resp.Body)
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

func (h *ProxyHandler) sendRetryRequest(req *http.Request, inference bool) (result *http.Response, resultErr error) {
	defer func() {
		if resultErr == nil {
			restoreCopilotNamespaceResponse(req, result)
		}
	}()
	if inference {
		if resp, handled, err := h.maybeSendNativeResponses(req); handled {
			return resp, err
		}
	}
	client := h.client
	if client == nil {
		client = http.DefaultClient
	}
	autoDecompressGzip := providerRequestAutoDecompressGzip(req)
	exceptionalInferenceClient := client.Timeout != 0 || client.Jar != nil || client.CheckRedirect != nil || req.URL == nil || req.URL.User != nil
	if !inference || exceptionalInferenceClient {
		if inference {
			clone := *client
			clone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			}
			client = &clone
		}
		var receipt *taskInferenceSend
		if inference && req.URL != nil && req.RequestURI == "" {
			receipt = h.beginTaskInferenceSend(req)
		}
		resp, err := client.Do(req)
		if err != nil {
			receipt.finish(resp, err)
			return resp, err
		}
		maybeAutoDecompressProviderResponse(resp, autoDecompressGzip)
		receipt.finish(resp, nil)
		return resp, nil
	}
	if req.RequestURI != "" {
		if req.Body != nil {
			_ = req.Body.Close()
		}
		return nil, &url.Error{Op: strings.ToLower(req.Method), URL: req.URL.String(), Err: errors.New("http: Request.RequestURI can't be set in client requests")}
	}
	if req.Header == nil {
		req.Header = make(http.Header)
	}
	transport := client.Transport
	if transport == nil {
		transport = http.DefaultTransport
	}
	receipt := h.beginTaskInferenceSend(req)
	resp, err := transport.RoundTrip(req)
	if err != nil {
		receipt.finish(resp, err)
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		if tlsErr, ok := err.(tls.RecordHeaderError); ok && string(tlsErr.RecordHeader[:]) == "HTTP/" {
			err = http.ErrSchemeMismatch
		}
		return nil, &url.Error{Op: strings.ToLower(req.Method), URL: req.URL.String(), Err: err}
	}
	if resp == nil {
		err := &url.Error{Op: strings.ToLower(req.Method), URL: req.URL.String(), Err: fmt.Errorf("http: RoundTripper implementation (%T) returned a nil *Response with a nil error", transport)}
		receipt.finish(nil, err)
		return nil, err
	}
	if resp.Body == nil {
		if resp.ContentLength > 0 && req.Method != http.MethodHead {
			err := &url.Error{Op: strings.ToLower(req.Method), URL: req.URL.String(), Err: fmt.Errorf("http: RoundTripper implementation (%T) returned a *Response with content length %d but a nil Body", transport, resp.ContentLength)}
			receipt.finish(resp, err)
			return nil, err
		}
		resp.Body = http.NoBody
	}
	if resp.Request == nil {
		resp.Request = req
	}
	maybeAutoDecompressProviderResponse(resp, autoDecompressGzip)
	receipt.finish(resp, nil)
	return resp, nil
}

var (
	errProviderGzipReadInProgress = errors.New("concurrent read on gzip response body")
	errProviderGzipBodyClosed     = errors.New("read on closed gzip response body")
	providerGzipReaderPool        = sync.Pool{New: func() any { return new(gzip.Reader) }}
)

type providerGzipEOFReader struct{}

func (providerGzipEOFReader) Read([]byte) (int, error) { return 0, io.EOF }
func (providerGzipEOFReader) ReadByte() (byte, error)  { return 0, io.EOF }

func providerGzipReaderPoolGet(body io.Reader) (*gzip.Reader, error) {
	reader := providerGzipReaderPool.Get().(*gzip.Reader)
	if err := reader.Reset(body); err != nil {
		providerGzipReaderPoolPut(reader)
		return nil, err
	}
	return reader, nil
}

func providerGzipReaderPoolPut(reader *gzip.Reader) {
	var empty flate.Reader = providerGzipEOFReader{}
	_ = reader.Reset(empty)
	providerGzipReaderPool.Put(reader)
}

type providerAutoGzipResponseBody struct {
	body   io.ReadCloser
	mu     sync.Mutex
	reader *gzip.Reader
	state  error
}

func (b *providerAutoGzipResponseBody) acquire() (*gzip.Reader, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state != nil {
		return nil, b.state
	}
	if b.reader == nil {
		b.state = errProviderGzipReadInProgress
		b.mu.Unlock()
		reader, err := providerGzipReaderPoolGet(b.body)
		b.mu.Lock()
		if b.state != errProviderGzipReadInProgress {
			if reader != nil {
				providerGzipReaderPoolPut(reader)
			}
			return nil, b.state
		}
		b.reader = reader
		b.state = err
		if err != nil {
			return nil, err
		}
	}
	reader := b.reader
	b.reader = nil
	b.state = errProviderGzipReadInProgress
	return reader, nil
}

func (b *providerAutoGzipResponseBody) release(reader *gzip.Reader) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.state == errProviderGzipReadInProgress {
		b.reader = reader
		b.state = nil
		return
	}
	providerGzipReaderPoolPut(reader)
}

func (b *providerAutoGzipResponseBody) Read(p []byte) (int, error) {
	reader, err := b.acquire()
	if err != nil {
		return 0, err
	}
	defer b.release(reader)
	return reader.Read(p)
}

func (b *providerAutoGzipResponseBody) Close() error {
	if b == nil || b.body == nil {
		return nil
	}
	b.mu.Lock()
	if b.state == nil && b.reader != nil {
		providerGzipReaderPoolPut(b.reader)
		b.reader = nil
	}
	b.state = errProviderGzipBodyClosed
	b.mu.Unlock()
	return b.body.Close()
}

func maybeAutoDecompressProviderResponse(resp *http.Response, enabled bool) {
	if !enabled || resp == nil || resp.Body == nil || !strings.EqualFold(resp.Header.Get("Content-Encoding"), "gzip") {
		return
	}
	resp.Body = &providerAutoGzipResponseBody{body: resp.Body}
	resp.Header.Del("Content-Encoding")
	resp.Header.Del("Content-Length")
	resp.ContentLength = -1
	resp.Uncompressed = true
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
