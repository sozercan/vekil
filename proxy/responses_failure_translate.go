package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sozercan/vekil/logger"
)

const (
	responsesPrecommitPeekTimeout  = 750 * time.Millisecond
	responsesPrecommitMaxPeekBytes = 64 * 1024
	responsesPeekReadChunkSize     = 4 * 1024
	// responsesFailureTapMaxBuffer bounds how much of an in-flight SSE event the
	// failure tap buffers while waiting for its delimiter. It must be large
	// enough to hold a real response.completed event, which embeds the full
	// response output plus the usage object — long Codex turns routinely exceed
	// 64 KiB — so it is sized at 1 MiB. On overflow the tap still best-effort
	// sniffs usage from the buffered bytes before dropping them.
	responsesFailureTapMaxBuffer = 1 << 20
	// responsesFailureTapOverflowTail is the trailing window retained when a
	// single SSE event overflows the buffer. A streamed response.completed embeds
	// its (small) usage object near the end of the event, so retaining the tail
	// lets a later chunk complete the "usage":{...} object for sniffing instead of
	// dropping it. 64 KiB comfortably holds a usage object plus surrounding JSON.
	responsesFailureTapOverflowTail = 64 * 1024
	responsesFailureLogMessageLimit = 256
)

type responsesPeekDecision int

const (
	responsesPeekDecisionPassthrough responsesPeekDecision = iota
	responsesPeekDecisionTranslate
)

type peekResult struct {
	decision         responsesPeekDecision
	status           int
	errType          string
	message          string
	retryAfter       string
	retryAfterSource string
	failure          *responsesWebSocketStreamEvent
	bufferedBytes    int
	peekDuration     time.Duration
}

type responsesPeekChunk struct {
	data []byte
	err  error
}

type responsesSSEMessage struct {
	event    string
	data     string
	semantic bool
}

type responsesSSEParser struct {
	pending  []byte
	allowBOM bool
}

type responsesPreparedStream struct {
	resp     *http.Response
	pr       *io.PipeReader
	peekDone chan peekResult
	commitCh chan struct{}
	abortCh  chan struct{}
	commitFn func()
	abortFn  func()
}

type responsesPreparedBody struct {
	reader  *io.PipeReader
	closeFn func()
}

func (b *responsesPreparedBody) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *responsesPreparedBody) Close() error {
	if b.closeFn != nil {
		b.closeFn()
	}
	return nil
}

func newResponsesPreparedStream(resp *http.Response, maxPeekBytes int) *responsesPreparedStream {
	pr, pw := io.Pipe()
	peekDone := make(chan peekResult, 1)
	commitCh := make(chan struct{})
	abortCh := make(chan struct{})
	upstreamBody := resp.Body

	var commitOnce sync.Once
	commit := func() {
		commitOnce.Do(func() {
			close(commitCh)
		})
	}

	var abortOnce sync.Once
	abort := func() {
		abortOnce.Do(func() {
			close(abortCh)
			_ = upstreamBody.Close()
		})
	}

	go runResponsesPeekPump(upstreamBody, pw, resp.Header, peekDone, commitCh, abortCh, maxPeekBytes)

	return &responsesPreparedStream{
		resp:     resp,
		pr:       pr,
		peekDone: peekDone,
		commitCh: commitCh,
		abortCh:  abortCh,
		commitFn: commit,
		abortFn:  abort,
	}
}

func (s *responsesPreparedStream) await(waitCtx, retryCtx context.Context, peekTimeout time.Duration) (peekResult, bool, error) {
	timer := time.NewTimer(peekTimeout)
	defer timer.Stop()

	var waitDone <-chan struct{}
	if waitCtx != nil {
		waitDone = waitCtx.Done()
	}
	var retryDone <-chan struct{}
	if retryCtx != nil && retryCtx != waitCtx {
		retryDone = retryCtx.Done()
	}

	for {
		select {
		case result := <-s.peekDone:
			return result, true, nil
		case <-timer.C:
			select {
			case result := <-s.peekDone:
				return result, true, nil
			default:
			}
			return peekResult{}, false, nil
		case <-waitDone:
			return peekResult{}, false, waitCtx.Err()
		case <-retryDone:
			return peekResult{}, false, retryCtx.Err()
		}
	}
}

func (s *responsesPreparedStream) commitResponse() *http.Response {
	s.commitFn()
	s.resp.Body = &responsesPreparedBody{reader: s.pr, closeFn: s.abortFn}
	return s.resp
}

func (s *responsesPreparedStream) abort() {
	s.abortFn()
}

func peekAndForwardResponses(h *ProxyHandler, w http.ResponseWriter, r *http.Request, resp *http.Response, upstreamCancel context.CancelFunc, model string, toolScope string) {
	peekAndForwardResponsesWithConfig(h, w, r, resp, upstreamCancel, model, responsesPrecommitPeekTimeout, responsesPrecommitMaxPeekBytes, toolScope)
}

func peekAndForwardResponsesWithConfig(h *ProxyHandler, w http.ResponseWriter, r *http.Request, resp *http.Response, upstreamCancel context.CancelFunc, model string, peekTimeout time.Duration, maxPeekBytes int, toolScope string) {
	if upstreamCancel != nil {
		defer upstreamCancel()
	}

	prepared := newResponsesPreparedStream(resp, maxPeekBytes)
	result, hasResult, err := prepared.await(r.Context(), nil, peekTimeout)
	if err != nil {
		prepared.abort()
		return
	}
	if hasResult && result.decision == responsesPeekDecisionTranslate {
		logResponsesPrecommitTranslated(h, result, model, resp.Header)
		prepared.abort()
		writeOpenAIErrorWithRetryAfter(w, result.status, result.message, result.errType, result.retryAfter, resp.Header)
		return
	}
	if hasResult && result.failure != nil && result.decision == responsesPeekDecisionPassthrough {
		logResponsesPrecommitFailOpen(h, result.failure, model, resp.Header)
	}

	resp = prepared.commitResponse()
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(http.StatusOK)
	var store *ToolExecutionContextStore
	if h != nil {
		store = h.toolContexts
	}
	streamResponsesPipeWithFailureLog(r.Context(), h, w, resp.Body, resp.Header, store, toolScope)
}

func prepareResponsesStreamAttempt(waitCtx, streamCtx context.Context, request func() (*http.Response, error)) (*http.Response, *peekResult, http.Header, error) {
	resp, err := request()
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
		return resp, nil, nil, err
	}

	prepared := newResponsesPreparedStream(resp, responsesPrecommitMaxPeekBytes)
	result, hasResult, err := prepared.await(waitCtx, streamCtx, responsesPrecommitPeekTimeout)
	if err != nil {
		prepared.abort()
		return nil, nil, nil, err
	}
	if hasResult && result.decision == responsesPeekDecisionTranslate {
		prepared.abort()
		return nil, &result, resp.Header.Clone(), nil
	}
	if hasResult && result.failure != nil {
		return prepared.commitResponse(), &result, nil, nil
	}
	return prepared.commitResponse(), nil, nil, nil
}

func (h *ProxyHandler) prepareResponsesStream(waitCtx, streamCtx context.Context, model string, request func() (*http.Response, error)) (*http.Response, *peekResult, http.Header, error) {
	resp, result, translatedHeaders, err := prepareResponsesStreamAttempt(waitCtx, streamCtx, request)
	if err != nil || result == nil {
		return resp, nil, nil, err
	}
	if result.decision == responsesPeekDecisionTranslate {
		logResponsesPrecommitTranslated(h, *result, model, translatedHeaders)
		return nil, result, translatedHeaders, nil
	}
	if result.failure != nil && resp != nil {
		logResponsesPrecommitFailOpen(h, result.failure, model, resp.Header)
	}
	return resp, nil, nil, nil
}

func runResponsesPeekPump(body io.ReadCloser, pw *io.PipeWriter, headers http.Header, peekDone chan<- peekResult, commitCh, abortCh <-chan struct{}, maxPeekBytes int) {
	chunkCh := make(chan responsesPeekChunk, 1)
	go readResponsesPeekChunks(body, chunkCh, abortCh)

	parser := responsesSSEParser{allowBOM: true}
	var prefix bytes.Buffer
	start := time.Now()
	decisionSent := false
	streamEnded := false
	var streamErr error

	sendResult := func(result peekResult) {
		if decisionSent {
			return
		}
		result.bufferedBytes = prefix.Len()
		result.peekDuration = time.Since(start)
		decisionSent = true
		select {
		case peekDone <- result:
		default:
		}
	}

	for {
		readCh := (<-chan responsesPeekChunk)(nil)
		if !decisionSent && !streamEnded {
			readCh = chunkCh
		}

		select {
		case <-abortCh:
			_ = pw.CloseWithError(context.Canceled)
			return
		case <-commitCh:
			writePrefixAndDrainResponsesStream(pw, prefix.Bytes(), chunkCh, abortCh, streamEnded, streamErr)
			return
		case chunk, ok := <-readCh:
			if !ok {
				streamEnded = true
				if !decisionSent {
					sendResult(peekResult{decision: responsesPeekDecisionPassthrough})
				}
				continue
			}

			if len(chunk.data) > 0 {
				_, _ = prefix.Write(chunk.data)
				if !decisionSent {
					parser.push(chunk.data)
					if prefix.Len() >= maxPeekBytes {
						sendResult(peekResult{decision: responsesPeekDecisionPassthrough})
					} else if msg, ok := parser.nextSemantic(); ok {
						sendResult(classifyResponsesPeekMessage(msg, headers))
					}
				}
			}

			if chunk.err != nil {
				streamEnded = true
				if chunk.err != io.EOF {
					streamErr = chunk.err
				}
				if !decisionSent {
					sendResult(peekResult{decision: responsesPeekDecisionPassthrough})
				}
			}
		}
	}
}

func readResponsesPeekChunks(body io.ReadCloser, chunkCh chan<- responsesPeekChunk, abortCh <-chan struct{}) {
	defer close(chunkCh)

	buf := make([]byte, responsesPeekReadChunkSize)
	for {
		n, err := body.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			select {
			case chunkCh <- responsesPeekChunk{data: chunk}:
			case <-abortCh:
				return
			}
		}
		if err != nil {
			select {
			case chunkCh <- responsesPeekChunk{err: err}:
			case <-abortCh:
			}
			return
		}
	}
}

func writePrefixAndDrainResponsesStream(pw *io.PipeWriter, prefix []byte, chunkCh <-chan responsesPeekChunk, abortCh <-chan struct{}, streamEnded bool, streamErr error) {
	if len(prefix) > 0 {
		if _, err := pw.Write(prefix); err != nil {
			_ = pw.CloseWithError(err)
			return
		}
	}

	if streamEnded {
		if streamErr != nil {
			_ = pw.CloseWithError(streamErr)
			return
		}
		_ = pw.Close()
		return
	}

	for {
		select {
		case <-abortCh:
			_ = pw.CloseWithError(context.Canceled)
			return
		case chunk, ok := <-chunkCh:
			if !ok {
				_ = pw.Close()
				return
			}
			if len(chunk.data) > 0 {
				if _, err := pw.Write(chunk.data); err != nil {
					_ = pw.CloseWithError(err)
					return
				}
			}
			if chunk.err != nil {
				if chunk.err != io.EOF {
					_ = pw.CloseWithError(chunk.err)
					return
				}
				_ = pw.Close()
				return
			}
		}
	}
}

func classifyResponsesPeekMessage(msg responsesSSEMessage, headers http.Header) peekResult {
	eventName := strings.TrimSpace(msg.event)
	if eventName != "" && eventName != "response.failed" {
		return peekResult{decision: responsesPeekDecisionPassthrough}
	}

	event, err := parseResponsesStreamEvent(msg.data)
	if err != nil {
		return peekResult{decision: responsesPeekDecisionPassthrough}
	}

	if eventName == "" && event.Type != "response.failed" {
		return peekResult{decision: responsesPeekDecisionPassthrough}
	}
	if event.Type != "response.failed" {
		return peekResult{decision: responsesPeekDecisionPassthrough}
	}

	status, errType, ok := classifyPrecommitResponsesFailure(event)
	if !ok {
		return peekResult{
			decision: responsesPeekDecisionPassthrough,
			failure:  &event,
		}
	}

	retryAfter, source := "", ""
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout {
		retryAfter, source = selectResponsesRetryAfter(headers)
	}

	return peekResult{
		decision:         responsesPeekDecisionTranslate,
		status:           status,
		errType:          errType,
		message:          responsesPrecommitErrorMessage(event, status),
		retryAfter:       retryAfter,
		retryAfterSource: source,
		failure:          &event,
	}
}

func classifyPrecommitResponsesFailure(event responsesWebSocketStreamEvent) (int, string, bool) {
	code := strings.ToLower(strings.TrimSpace(event.Response.Error.Code))
	switch code {
	case "too_many_requests", "rate_limit_exceeded":
		return http.StatusTooManyRequests, "rate_limit_error", true
	case "model_overloaded", "engine_overloaded":
		return http.StatusServiceUnavailable, "server_error", true
	case "bad_gateway":
		return http.StatusBadGateway, "server_error", true
	case "timeout", "gateway_timeout":
		return http.StatusGatewayTimeout, "server_error", true
	}

	if code == "" && strings.EqualFold(strings.TrimSpace(event.Response.Error.Type), "rate_limit_error") {
		return http.StatusTooManyRequests, "rate_limit_error", true
	}

	return 0, "", false
}

func selectResponsesRetryAfter(headers http.Header) (string, string) {
	if headers == nil {
		return "", ""
	}

	if value := strings.TrimSpace(headerGetCI(headers, "retry-after-ms")); value != "" {
		ms, err := strconv.Atoi(value)
		if err == nil && ms > 0 {
			seconds := (ms + 999) / 1000
			if seconds > 0 {
				return strconv.Itoa(seconds), "retry-after-ms"
			}
		}
	}

	if delay, ok := parseRetryAfter(strings.TrimSpace(headerGetCI(headers, "Retry-After"))); ok && delay > 0 {
		return strconv.Itoa(int(delay / time.Second)), "Retry-After"
	}

	return "", ""
}

// isClientWriteError reports whether an io.Copy error originated from writing to
// the client (the flushWriter destination) rather than from reading the upstream
// source. A client-side write error (the client disconnected) is not an upstream
// failure and must not be recorded as one.
func isClientWriteError(fw *flushWriter, err error) bool {
	if fw == nil || err == nil {
		return false
	}
	return fw.writeErr != nil
}

func streamResponsesPipeWithFailureLog(ctx context.Context, h *ProxyHandler, w http.ResponseWriter, r io.Reader, upstreamHeaders http.Header, store *ToolExecutionContextStore, scope string) {
	if closer, ok := r.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}

	fw := &flushWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		fw.flusher = f
	}

	tap := newResponsesFailureTap(ctx, h, upstreamHeaders, store, scope)
	if _, err := io.Copy(fw, io.TeeReader(r, tap)); err != nil {
		// The HTTP 200 was already committed, so the client receives a truncated
		// stream when the upstream SSE connection resets or the pipe closes with
		// an error before a response.failed/incomplete event. Only record an
		// upstream failure for an actual upstream/transport error — a client
		// disconnect or cancellation (ctx cancelled, or a write error forwarding
		// to a gone client) is not an upstream failure and must not pollute the
		// dashboard error rate.
		if ctx.Err() == nil && !isClientWriteError(fw, err) {
			observeResponseFailureStatus(ctx, http.StatusBadGateway)
		}
		if h != nil && h.log != nil {
			h.log.Debug("responses stream copy failed after commit", logger.Err(err))
		}
	}
}

func parseResponsesStreamEvent(data string) (responsesWebSocketStreamEvent, error) {
	var event responsesWebSocketStreamEvent
	if err := json.Unmarshal([]byte(data), &event); err != nil {
		return responsesWebSocketStreamEvent{}, err
	}
	return event, nil
}

func responsesPrecommitErrorMessage(event responsesWebSocketStreamEvent, status int) string {
	message := strings.TrimSpace(event.Response.Error.Message)
	if message != "" {
		return message
	}
	if code := strings.TrimSpace(event.Response.Error.Code); code != "" {
		return code
	}
	if errType := strings.TrimSpace(event.Response.Error.Type); errType != "" {
		return errType
	}
	if text := http.StatusText(status); text != "" {
		return text
	}
	return "upstream response.failed"
}

func responsesUpstreamRequestID(headers http.Header) string {
	for _, name := range []string{"X-Request-Id", "X-Azure-Request-Id", "Openai-Request-Id"} {
		if value := strings.TrimSpace(headerGetCI(headers, name)); value != "" {
			return value
		}
	}
	return ""
}

func logResponsesPrecommitTranslated(h *ProxyHandler, result peekResult, model string, headers http.Header) {
	if result.failure == nil {
		return
	}
	h.log.Info("translated responses stream failure before commit",
		logger.F("endpoint", "responses_precommit_translated"),
		logger.F("status", result.status),
		logger.F("error_code", strings.TrimSpace(result.failure.Response.Error.Code)),
		logger.F("error_type", strings.TrimSpace(result.failure.Response.Error.Type)),
		logger.F("error_message", truncateResponsesFailureLogMessage(result.failure.Response.Error.Message)),
		logger.F("retry_after_source", result.retryAfterSource),
		logger.F("retry_after_seconds", result.retryAfter),
		logger.F("upstream_request_id", responsesUpstreamRequestID(headers)),
		logger.F("model", model),
		logger.F("peek_bytes", result.bufferedBytes),
		logger.F("peek_duration_ms", result.peekDuration.Milliseconds()),
	)
}

func logResponsesPrecommitFailOpen(h *ProxyHandler, event *responsesWebSocketStreamEvent, model string, headers http.Header) {
	if event == nil {
		return
	}
	h.log.Info("left responses stream failure as passthrough",
		logger.F("endpoint", "responses_precommit_failopen"),
		logger.F("error_code", strings.TrimSpace(event.Response.Error.Code)),
		logger.F("error_type", strings.TrimSpace(event.Response.Error.Type)),
		logger.F("error_message", truncateResponsesFailureLogMessage(event.Response.Error.Message)),
		logger.F("model", model),
		logger.F("upstream_request_id", responsesUpstreamRequestID(headers)),
	)
}

func truncateResponsesFailureLogMessage(message string) string {
	message = strings.TrimSpace(message)
	if len(message) <= responsesFailureLogMessageLimit {
		return message
	}
	return message[:responsesFailureLogMessageLimit]
}

func headerGetCI(headers http.Header, name string) string {
	values := headerValuesCI(headers, name)
	if len(values) == 0 {
		return ""
	}
	return values[0]
}

func headerValuesCI(headers http.Header, name string) []string {
	if headers == nil {
		return nil
	}
	canonicalName := http.CanonicalHeaderKey(name)
	for key, values := range headers {
		if http.CanonicalHeaderKey(key) == canonicalName {
			return values
		}
	}
	return nil
}

func (p *responsesSSEParser) push(chunk []byte) {
	p.pending = append(p.pending, chunk...)
}

func (p *responsesSSEParser) nextSemantic() (responsesSSEMessage, bool) {
	for {
		msg, consumed, incomplete := nextResponsesSSEMessage(p.pending, p.allowBOM)
		if incomplete {
			return responsesSSEMessage{}, false
		}
		p.allowBOM = false
		p.pending = p.pending[consumed:]
		if msg.semantic {
			return msg, true
		}
	}
}

func nextResponsesSSEMessage(buf []byte, allowBOM bool) (responsesSSEMessage, int, bool) {
	var msg responsesSSEMessage
	index := 0

	if allowBOM {
		bom := []byte{0xEF, 0xBB, 0xBF}
		switch {
		case len(buf) >= len(bom) && bytes.Equal(buf[:len(bom)], bom):
			index = len(bom)
		case len(buf) < len(bom) && bytes.Equal(buf, bom[:len(buf)]):
			return responsesSSEMessage{}, 0, true
		}
	}

	var dataLines []string
	for {
		lineStart := index
		newlineOffset := bytes.IndexByte(buf[index:], '\n')
		if newlineOffset < 0 {
			return responsesSSEMessage{}, 0, true
		}
		index += newlineOffset + 1
		line := buf[lineStart : index-1]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}

		if len(line) == 0 {
			msg.data = strings.Join(dataLines, "\n")
			return msg, index, false
		}

		if line[0] == ':' {
			continue
		}

		field := line
		value := ""
		if colon := bytes.IndexByte(line, ':'); colon >= 0 {
			field = line[:colon]
			value = string(line[colon+1:])
			value = strings.TrimPrefix(value, " ")
		}

		switch string(field) {
		case "event":
			msg.event = value
			msg.semantic = true
		case "data":
			dataLines = append(dataLines, value)
			msg.semantic = true
		}
	}
}

type responsesFailureTap struct {
	h               *ProxyHandler
	upstreamHeaders http.Header
	ctx             context.Context
	store           *ToolExecutionContextStore
	scope           string
	responseScope   string
	parser          responsesSSEParser
	// overflowed is set once a single SSE event has exceeded the buffer cap, so
	// later writes keep best-effort sniffing usage from the rolling tail (the
	// event's framing is unrecoverable once dropped, so the normal dispatch path
	// cannot parse it).
	overflowed bool
	// overflowUsageRecorded is set once usage has been recovered from an
	// overflowed event, to avoid redundant re-sniffing on every subsequent write.
	overflowUsageRecorded bool
	// usageTail is a bounded rolling window of the most recent raw stream bytes,
	// used to recover usage from an oversized streamed response.completed event.
	usageTail []byte
}

func newResponsesFailureTap(ctx context.Context, h *ProxyHandler, upstreamHeaders http.Header, store *ToolExecutionContextStore, scope string) *responsesFailureTap {
	if ctx == nil {
		ctx = context.Background()
	}
	return &responsesFailureTap{
		h:               h,
		upstreamHeaders: upstreamHeaders,
		ctx:             ctx,
		store:           store,
		scope:           strings.TrimSpace(scope),
		parser:          responsesSSEParser{allowBOM: true},
	}
}

func (t *responsesFailureTap) Write(p []byte) (int, error) {
	// Maintain a small rolling tail of the raw byte stream, independent of the SSE
	// parser's pending buffer, so usage can be recovered from a very large streamed
	// response.completed event even after the parser truncates/clears its buffer
	// (the giant event's framing is corrupted once it overflows, so the normal
	// dispatch path cannot parse it). usage sits near the end of the event, so a
	// bounded tail of recent bytes is enough to capture the "usage":{...} object.
	t.appendUsageTail(p)

	t.parser.push(p)
	for {
		msg, ok := t.parser.nextSemantic()
		if !ok {
			break
		}
		t.maybeProcess(msg)
	}
	if t.overflowed {
		t.sniffUsageFromOverflow(t.usageTail)
	}
	if len(t.parser.pending) > responsesFailureTapMaxBuffer {
		// The event exceeds the buffer (a very large response.completed whose
		// delimiter has not arrived). Sniff usage from the rolling tail, then drop
		// the parser buffer (its framing is unrecoverable) and flag overflow so
		// later writes keep sniffing the tail.
		t.overflowed = true
		t.sniffUsageFromOverflow(t.usageTail)
		t.parser.pending = nil
		t.parser.allowBOM = false
	}
	return len(p), nil
}

// appendUsageTail keeps the last responsesFailureTapOverflowTail bytes of the raw
// stream in t.usageTail for best-effort usage recovery from oversized events.
func (t *responsesFailureTap) appendUsageTail(p []byte) {
	if t.overflowUsageRecorded {
		return
	}
	t.usageTail = append(t.usageTail, p...)
	if len(t.usageTail) > responsesFailureTapOverflowTail {
		t.usageTail = t.usageTail[len(t.usageTail)-responsesFailureTapOverflowTail:]
	}
}

// sniffUsageFromOverflow extracts a usage object from a partially-buffered SSE
// event that is about to be dropped for exceeding the buffer cap. It scans for
// the last "usage":{...} object in the buffer (the completed event carries usage
// after its output) and records it. It runs once per turn — once usage is
// observed onto the summary, a later real terminal event would just re-observe
// the same value. Best-effort: any parse failure is ignored.
func (t *responsesFailureTap) sniffUsageFromOverflow(buf []byte) {
	if t.overflowUsageRecorded {
		return
	}
	if usage, ok := extractResponsesUsageObject(buf); ok {
		observeResponsesUsage(t.ctx, usage)
		t.overflowUsageRecorded = true
	}
}

func (t *responsesFailureTap) maybeProcess(msg responsesSSEMessage) {
	eventName := strings.TrimSpace(msg.event)
	if eventName != "" &&
		eventName != "response.created" &&
		eventName != "response.completed" &&
		eventName != "response.output_item.done" &&
		eventName != "response.failed" &&
		eventName != "response.incomplete" {
		return
	}

	event, err := parseResponsesStreamEvent(msg.data)
	if err != nil {
		return
	}

	eventType := strings.TrimSpace(event.Type)
	if eventName == "" {
		eventName = eventType
	}
	if eventName == "response.completed" {
		// Record token usage from the terminal event. This is best-effort: a
		// response.completed larger than responsesFailureTapMaxBuffer is dropped
		// by the tap before its delimiter, in which case usage is simply not
		// recorded for that turn (it degrades to zero rather than erroring). The
		// usage payload is small in practice (~hundreds of bytes).
		observeResponsesUsage(t.ctx, event.Response.Usage)
	}
	t.maybeCaptureToolCommand(eventName, event)
	t.maybeLog(eventName, event)
}

func (t *responsesFailureTap) maybeCaptureToolCommand(eventName string, event responsesWebSocketStreamEvent) {
	switch eventName {
	case "response.created", "response.completed":
		if t.responseScope == "" {
			t.responseScope = toolExecutionScopeFromResponseID(event.Response.ID)
		}
		return
	case "response.output_item.done":
	default:
		return
	}

	if t.h == nil || len(event.Item) == 0 {
		return
	}
	scopes := uniqueToolExecutionScopes(t.scope, t.responseScope)
	if len(scopes) == 0 {
		return
	}
	t.h.maybeRewriteOrCaptureToolCommandItemInScopes(t.ctx, event.Item, t.store, scopes, false)
}

func (t *responsesFailureTap) maybeLog(eventName string, event responsesWebSocketStreamEvent) {
	if t.h == nil {
		return
	}
	if eventName != "response.failed" && eventName != "response.incomplete" {
		return
	}

	// The HTTP 200 was already committed before this post-commit failure event,
	// so the stats middleware would otherwise record the turn as a success.
	// Record an out-of-band failure status on the request summary so the
	// dashboard reflects the failure, mirroring how the websocket bridge maps the
	// same response.failed/incomplete into an errored turn. Classify the event so
	// rate limits (429) and overloads (503) keep their exact status rather than
	// all collapsing to bad-gateway.
	failureStatus, _, _ := responsesWebSocketStreamFailureDetails(event)
	if failureStatus == 0 {
		failureStatus = http.StatusBadGateway
	}
	observeResponseFailureStatus(t.ctx, failureStatus)

	fields := []logger.Field{
		logger.F("endpoint", "responses_stream_failure"),
		logger.F("event_type", eventName),
		logger.F("upstream_request_id", responsesUpstreamRequestID(t.upstreamHeaders)),
	}
	switch eventName {
	case "response.failed":
		fields = append(fields,
			logger.F("error_code", strings.TrimSpace(event.Response.Error.Code)),
			logger.F("error_type", strings.TrimSpace(event.Response.Error.Type)),
			logger.F("error_message", truncateResponsesFailureLogMessage(event.Response.Error.Message)),
		)
	case "response.incomplete":
		fields = append(fields,
			logger.F("reason", strings.TrimSpace(event.Response.IncompleteDetails.Reason)),
		)
	}
	t.h.log.Info("responses stream reported failure after commit", fields...)
}
