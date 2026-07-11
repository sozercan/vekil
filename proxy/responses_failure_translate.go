package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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
	responsesPeekCancellationGrace = 10 * time.Millisecond
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

type responsesPreparedAwaitSource int

const (
	responsesPreparedAwaitNone responsesPreparedAwaitSource = iota
	responsesPreparedAwaitInbound
	responsesPreparedAwaitUpstream
)

type peekResult struct {
	decision         responsesPeekDecision
	status           int
	errType          string
	message          string
	retryAfter       string
	retryAfterSource string
	failure          *responsesWebSocketStreamEvent
	terminal         *responsesWebSocketStreamEvent
	bufferedBytes    int
	peekDuration     time.Duration
}

type responsesPeekChunk struct {
	data                       []byte
	err                        error
	lifecycleCanceledAtFailure bool
}

type responsesPeekReadOutcome struct {
	err                        error
	lifecycleCanceledAtFailure bool
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
	resp      *http.Response
	pr        *io.PipeReader
	peekDone  chan peekResult
	peekState *responsesPeekState
	commitCh  chan struct{}
	abortCh   chan struct{}
	commitFn  func()
	abortFn   func()
}

type responsesPeekState struct {
	mu           sync.Mutex
	terminal     peekResult
	hasTerminal  bool
	terminalDone chan struct{}
	doneOnce     sync.Once
	outcome      responsesPeekReadOutcome
	hasOutcome   bool
	outcomeDone  chan struct{}
	outcomeOnce  sync.Once
}

func newResponsesPeekState() *responsesPeekState {
	return &responsesPeekState{terminalDone: make(chan struct{}), outcomeDone: make(chan struct{})}
}

func (s *responsesPeekState) publishOutcome(outcome responsesPeekReadOutcome) {
	if s == nil || outcome.err == nil {
		return
	}
	s.mu.Lock()
	if !s.hasOutcome {
		s.outcome = outcome
		s.hasOutcome = true
	}
	s.mu.Unlock()
	s.outcomeOnce.Do(func() { close(s.outcomeDone) })
}

func (s *responsesPeekState) publishTerminal(result peekResult) {
	if s == nil || result.terminal == nil {
		return
	}
	s.mu.Lock()
	if !s.hasTerminal || (result.decision == responsesPeekDecisionTranslate && s.terminal.decision != responsesPeekDecisionTranslate) {
		s.terminal = result
		s.hasTerminal = true
	}
	s.mu.Unlock()
	s.doneOnce.Do(func() { close(s.terminalDone) })
}

func (s *responsesPeekState) terminalResult() (peekResult, bool) {
	if s == nil {
		return peekResult{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.terminal, s.hasTerminal
}

func (s *responsesPeekState) readOutcome() (responsesPeekReadOutcome, bool) {
	if s == nil {
		return responsesPeekReadOutcome{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outcome, s.hasOutcome
}

type responsesPreparedBody struct {
	reader    *io.PipeReader
	closeFn   func()
	closeOnce sync.Once
	closeErr  error
}

func (b *responsesPreparedBody) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *responsesPreparedBody) Close() error {
	b.closeOnce.Do(func() {
		// Closing the reader is required in addition to aborting the upstream.
		// A committed pump may already be blocked inside pw.Write, where it cannot
		// observe abortCh until the reader side is closed.
		if b.reader != nil {
			b.closeErr = b.reader.CloseWithError(context.Canceled)
		}
		if b.closeFn != nil {
			b.closeFn()
		}
	})
	return b.closeErr
}

func newResponsesPreparedStream(resp *http.Response, maxPeekBytes int) *responsesPreparedStream {
	pr, pw := io.Pipe()
	peekDone := make(chan peekResult, 1)
	peekState := newResponsesPeekState()
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

	go runResponsesPeekPump(upstreamBody, pw, resp.Header, peekDone, peekState, commitCh, abortCh, maxPeekBytes)

	return &responsesPreparedStream{
		resp:      resp,
		pr:        pr,
		peekDone:  peekDone,
		peekState: peekState,
		commitCh:  commitCh,
		abortCh:   abortCh,
		commitFn:  commit,
		abortFn:   abort,
	}
}

func (s *responsesPreparedStream) terminalResult() (peekResult, bool) {
	if s == nil || s.peekState == nil {
		return peekResult{}, false
	}
	return s.peekState.terminalResult()
}

func (s *responsesPreparedStream) awaitCancellationResolution(grace time.Duration) (peekResult, bool, responsesPeekReadOutcome, bool) {
	if s == nil || s.peekState == nil {
		return peekResult{}, false, responsesPeekReadOutcome{}, false
	}
	if terminal, ok := s.terminalResult(); ok {
		return terminal, true, responsesPeekReadOutcome{}, false
	}
	if outcome, ok := s.peekState.readOutcome(); ok {
		return peekResult{}, false, outcome, true
	}
	if grace <= 0 {
		return peekResult{}, false, responsesPeekReadOutcome{}, false
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-s.peekState.terminalDone:
		terminal, ok := s.terminalResult()
		return terminal, ok, responsesPeekReadOutcome{}, false
	case <-s.peekState.outcomeDone:
		if terminal, ok := s.terminalResult(); ok {
			return terminal, true, responsesPeekReadOutcome{}, false
		}
		outcome, ok := s.peekState.readOutcome()
		return peekResult{}, false, outcome, ok
	case <-timer.C:
		return peekResult{}, false, responsesPeekReadOutcome{}, false
	}
}

func (s *responsesPreparedStream) await(waitCtx, retryCtx context.Context, peekTimeout time.Duration) (peekResult, bool, responsesPreparedAwaitSource, error) {
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
			return result, true, responsesPreparedAwaitNone, nil
		case <-timer.C:
			select {
			case result := <-s.peekDone:
				return result, true, responsesPreparedAwaitNone, nil
			default:
			}
			return peekResult{}, false, responsesPreparedAwaitNone, nil
		case <-waitDone:
			return peekResult{}, false, responsesPreparedAwaitInbound, waitCtx.Err()
		case <-retryDone:
			grace := time.NewTimer(responsesPeekCancellationGrace)
			select {
			case result := <-s.peekDone:
				grace.Stop()
				return result, true, responsesPreparedAwaitNone, nil
			case <-grace.C:
				return peekResult{}, false, responsesPreparedAwaitUpstream, retryCtx.Err()
			}
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

func peekAndForwardResponses(h *ProxyHandler, w http.ResponseWriter, r *http.Request, resp *http.Response, upstreamCtx context.Context, upstreamCancel context.CancelFunc, model string, toolScope string) {
	body := newLifecycleAwareReadCloser(resp.Body, upstreamCtx)
	resp.Body = body
	peekAndForwardResponsesWithConfig(h, w, r, resp, upstreamCtx, upstreamCancel, model, responsesPrecommitPeekTimeout, responsesPrecommitMaxPeekBytes, toolScope, h.lifecycleStreamHooks(r.Context(), body.canceledAtFailure, func() { h.WriteShutdownServiceUnavailable(w, r) }))
}

func peekAndForwardResponsesWithConfig(h *ProxyHandler, w http.ResponseWriter, r *http.Request, resp *http.Response, upstreamCtx context.Context, upstreamCancel context.CancelFunc, model string, peekTimeout time.Duration, maxPeekBytes int, toolScope string, lifecycleHooks ...streamLifecycleHooks) {
	if upstreamCancel != nil {
		defer upstreamCancel()
	}
	lifecycle := streamLifecycleHooks{}
	if len(lifecycleHooks) > 0 {
		lifecycle = lifecycleHooks[0]
	}

	prepared := newResponsesPreparedStream(resp, maxPeekBytes)
	result, hasResult, awaitSource, err := prepared.await(r.Context(), upstreamCtx, peekTimeout)
	lifecycleCanceled := awaitSource != responsesPreparedAwaitInbound && upstreamCtx != nil && upstreamCtx.Err() != nil && errors.Is(context.Cause(upstreamCtx), errProxyLifecycleShutdown) && (err == nil || errors.Is(err, context.Canceled))
	if err != nil && !lifecycleCanceled {
		prepared.abort()
		if awaitSource == responsesPreparedAwaitInbound {
			return
		}
		status := http.StatusBadGateway
		if awaitSource == responsesPreparedAwaitUpstream && errors.Is(err, context.DeadlineExceeded) {
			status = http.StatusGatewayTimeout
		}
		observeResponseFailureStatus(r.Context(), status)
		if h != nil && h.log != nil {
			h.log.Error("upstream request failed",
				logger.F("endpoint", "responses_precommit"),
				logger.F("status", status),
				logger.Err(err),
			)
		}
		writeOpenAIUpstreamRequestFailure(w, status, err)
		return
	}
	if lifecycleCanceled && !(hasResult && result.terminal != nil) {
		terminal, hasTerminal, outcome, hasOutcome := prepared.awaitCancellationResolution(responsesPeekCancellationGrace)
		switch {
		case hasTerminal:
			terminal.decision = responsesPeekDecisionPassthrough
			result, hasResult = terminal, true
		case hasOutcome && !outcome.lifecycleCanceledAtFailure:
			// The read ended independently before shutdown won the race. Commit the
			// prepared stream so EOF/reset and any semantic event retain provider
			// accounting instead of becoming a local 503.
		case hasOutcome && outcome.lifecycleCanceledAtFailure:
			prepared.abort()
			lifecycle.suppressKnownTransportCancellation(false)
			return
		default:
			prepared.abort()
			lifecycle.suppressKnownTransportCancellation(false)
			return
		}
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
	if !(hasResult && result.terminal != nil) && lifecycle.suppressTransportCancellation(false) {
		prepared.abort()
		return
	}

	resp = prepared.commitResponse()
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(http.StatusOK)
	var store *ToolExecutionContextStore
	if h != nil {
		store = h.toolContexts
	}
	streamResponsesPipeWithFailureLog(r.Context(), h, w, resp.Body, resp.Header, store, toolScope, lifecycleHooks...)
}

func prepareResponsesStreamAttempt(waitCtx, streamCtx context.Context, request func() (*http.Response, error)) (*http.Response, *peekResult, http.Header, error) {
	resp, err := request()
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
		return resp, nil, nil, err
	}

	prepared := newResponsesPreparedStream(resp, responsesPrecommitMaxPeekBytes)
	result, hasResult, _, err := prepared.await(waitCtx, streamCtx, responsesPrecommitPeekTimeout)
	if err != nil {
		prepared.abort()
		return nil, nil, nil, err
	}
	if streamCtx != nil && streamCtx.Err() != nil && errors.Is(context.Cause(streamCtx), errProxyLifecycleShutdown) && !(hasResult && result.terminal != nil) {
		terminal, hasTerminal, outcome, hasOutcome := prepared.awaitCancellationResolution(responsesPeekCancellationGrace)
		switch {
		case hasTerminal:
			terminal.decision = responsesPeekDecisionPassthrough
			result, hasResult = terminal, true
		case hasOutcome && !outcome.lifecycleCanceledAtFailure:
			// Preserve the independently-ended prepared stream; handleCreateRequest
			// will classify it after consuming the pipe.
		case hasOutcome && outcome.lifecycleCanceledAtFailure:
			prepared.abort()
			return nil, nil, nil, streamCtx.Err()
		default:
			prepared.abort()
			return nil, nil, nil, streamCtx.Err()
		}
	}
	if hasResult && result.decision == responsesPeekDecisionTranslate {
		prepared.abort()
		return nil, &result, resp.Header.Clone(), nil
	}
	if hasResult && result.terminal != nil {
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
	if result.terminal != nil && resp != nil {
		return resp, result, nil, nil
	}
	return resp, nil, nil, nil
}

func runResponsesPeekPump(body io.ReadCloser, pw *io.PipeWriter, headers http.Header, peekDone chan<- peekResult, peekState *responsesPeekState, commitCh, abortCh <-chan struct{}, maxPeekBytes int) {
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
		peekState.publishTerminal(result)
		decisionSent = true
		select {
		case peekDone <- result:
		default:
		}
	}

	for {
		readCh := (<-chan responsesPeekChunk)(nil)
		if !streamEnded && (!decisionSent || prefix.Len() < maxPeekBytes) {
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
				parser.push(chunk.data)
				result, sawSemantic := inspectResponsesPeekMessages(&parser, headers, peekState)
				result.bufferedBytes = prefix.Len()
				result.peekDuration = time.Since(start)
				if !decisionSent {
					if result.decision == responsesPeekDecisionTranslate {
						sendResult(result)
					} else if prefix.Len() >= maxPeekBytes {
						if sawSemantic {
							sendResult(result)
						} else {
							sendResult(peekResult{decision: responsesPeekDecisionPassthrough})
						}
					} else if sawSemantic {
						sendResult(result)
					}
				}
			}

			if chunk.err != nil {
				peekState.publishOutcome(responsesPeekReadOutcome{
					err:                        chunk.err,
					lifecycleCanceledAtFailure: chunk.lifecycleCanceledAtFailure,
				})
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

func inspectResponsesPeekMessages(parser *responsesSSEParser, headers http.Header, peekState *responsesPeekState) (peekResult, bool) {
	result := peekResult{decision: responsesPeekDecisionPassthrough}
	sawSemantic := false
	for {
		msg, ok := parser.nextSemantic()
		if !ok {
			break
		}
		classified := classifyResponsesPeekMessage(msg, headers)
		peekState.publishTerminal(classified)
		if !sawSemantic {
			result = classified
		}
		sawSemantic = true
	}
	return result, sawSemantic
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
			lifecycleCanceledAtFailure := false
			if observer, ok := body.(interface{ canceledAtFailure() bool }); ok {
				lifecycleCanceledAtFailure = observer.canceledAtFailure()
			}
			select {
			case chunkCh <- responsesPeekChunk{err: err, lifecycleCanceledAtFailure: lifecycleCanceledAtFailure}:
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
	result := peekResult{decision: responsesPeekDecisionPassthrough}
	eventName := strings.TrimSpace(msg.event)
	event, err := parseResponsesStreamEvent(msg.data)
	if err != nil {
		return result
	}
	terminalType := strings.TrimSpace(event.Type)
	if terminalType == "" {
		terminalType = eventName
	}
	switch terminalType {
	case "response.completed", "response.failed", "response.incomplete":
		terminal := event
		result.terminal = &terminal
		if terminalType == "response.failed" || terminalType == "response.incomplete" {
			result.failure = &terminal
		}
	}
	if terminalType != "response.failed" {
		return result
	}

	status, errType, ok := classifyPrecommitResponsesFailure(event)
	if !ok {
		return result
	}

	retryAfter, source := "", ""
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout {
		retryAfter, source = selectResponsesRetryAfter(headers)
	}

	result.decision = responsesPeekDecisionTranslate
	result.status = status
	result.errType = errType
	result.message = responsesPrecommitErrorMessage(event, status)
	result.retryAfter = retryAfter
	result.retryAfterSource = source
	return result
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

func streamResponsesPipeWithFailureLog(ctx context.Context, h *ProxyHandler, w http.ResponseWriter, r io.Reader, upstreamHeaders http.Header, store *ToolExecutionContextStore, scope string, lifecycleHooks ...streamLifecycleHooks) {
	if closer, ok := r.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}
	lifecycle := streamLifecycleHooks{}
	if len(lifecycleHooks) > 0 {
		lifecycle = lifecycleHooks[0]
	}

	fw := &flushWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		fw.flusher = f
	}

	tap := newResponsesFailureTap(ctx, h, upstreamHeaders, store, scope)
	if _, err := io.Copy(fw, io.TeeReader(r, tap)); err != nil {
		if tap.completedCleanly() {
			return
		}
		// The HTTP 200 was already committed, so the client receives a truncated
		// stream when the upstream SSE connection resets or the pipe closes with
		// an error before a response.failed/incomplete event. Only record an
		// upstream failure for an actual upstream/transport error — a client
		// disconnect or cancellation (ctx cancelled, or a write error forwarding
		// to a gone client) is not an upstream failure and must not pollute the
		// dashboard error rate.
		if !tap.completedCleanly() && lifecycle.suppressTransportCancellation(true) {
			return
		}
		if ctx.Err() == nil && !isClientWriteError(fw, err) {
			observeResponseFailureStatus(ctx, http.StatusBadGateway)
		}
		if h != nil && h.log != nil {
			h.log.Debug("responses stream copy failed after commit", logger.Err(err))
		}
		return
	}

	if ctx.Err() == nil && !tap.completedCleanly() {
		if lifecycle.suppressTransportCancellation(true) {
			return
		}
		observeResponseFailureStatus(ctx, http.StatusBadGateway)
		if h != nil && h.log != nil {
			h.log.Debug("responses stream ended before terminal event")
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
	// overflowActive remains true while forwarding the rest of an overflowed event.
	// It is cleared once that event's blank-line boundary is observed.
	overflowActive bool
	// overflowCompletedEvent is set when the overflowed event appears to be a
	// response.completed terminal event, either from its event/type prefix or from
	// the terminal usage object recovered near the tail.
	overflowCompletedEvent bool
	overflowBoundaryTail   []byte
	// overflowUsageRecorded is set once usage has been recovered from an
	// overflowed event, to avoid redundant re-sniffing on every subsequent write.
	overflowUsageRecorded bool
	// usageTail is a bounded rolling window of the most recent raw stream bytes,
	// used to recover usage from an oversized streamed response.completed event.
	usageTail []byte
	// terminalSeen is set after a parsed terminal Responses event. A clean EOF
	// before response.completed/failed/incomplete means the committed stream was
	// truncated and should not be counted as a successful 200.
	terminalSeen bool
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
	if t.overflowActive {
		t.observeOverflowBoundary(p)
	}
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
		t.overflowActive = true
		if responsesOverflowLooksCompleted(t.parser.pending) {
			t.overflowCompletedEvent = true
		}
		t.overflowBoundaryTail = trailingBytes(t.parser.pending, 3)
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
		t.overflowCompletedEvent = true
	}
}

func (t *responsesFailureTap) observeOverflowBoundary(p []byte) {
	if t == nil || !t.overflowActive {
		return
	}
	combined := append(append([]byte(nil), t.overflowBoundaryTail...), p...)
	if bytes.Contains(combined, []byte("\n\n")) || bytes.Contains(combined, []byte("\r\n\r\n")) {
		t.overflowActive = false
	}
	t.overflowBoundaryTail = trailingBytes(combined, 3)
}

func trailingBytes(buf []byte, n int) []byte {
	if n <= 0 || len(buf) == 0 {
		return nil
	}
	if len(buf) > n {
		buf = buf[len(buf)-n:]
	}
	return append([]byte(nil), buf...)
}

func responsesOverflowLooksCompleted(buf []byte) bool {
	return bytes.Contains(buf, []byte("event: response.completed")) ||
		bytes.Contains(buf, []byte("event:response.completed")) ||
		bytes.Contains(buf, []byte(`"type":"response.completed"`)) ||
		bytes.Contains(buf, []byte(`"type": "response.completed"`))
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
	switch eventName {
	case "response.completed", "response.failed", "response.incomplete":
		t.terminalSeen = true
	}
	if eventName == "response.completed" || eventName == "response.failed" || eventName == "response.incomplete" {
		// Record token usage from every terminal event. Failed and incomplete
		// responses can still carry billable partial usage. This is best-effort: a
		// response.completed larger than responsesFailureTapMaxBuffer is dropped
		// by the tap before its delimiter, in which case usage is simply not
		// recorded for that turn (it degrades to zero rather than erroring). The
		// usage payload is small in practice (~hundreds of bytes).
		observeResponsesUsage(t.ctx, event.Response.Usage)
	}
	t.maybeCaptureToolCommand(eventName, event)
	t.maybeLog(eventName, event)
}

func (t *responsesFailureTap) completedCleanly() bool {
	if t == nil {
		return true
	}
	// If a giant response.completed event overflowed the bounded parser, treat it
	// as complete only after that overflowed event's blank-line boundary is seen.
	// Non-overflowing streams still require a parsed terminal event.
	return t.terminalSeen || (t.overflowCompletedEvent && !t.overflowActive)
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
