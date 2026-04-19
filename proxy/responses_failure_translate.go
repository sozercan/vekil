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
	responsesPrecommitPeekTimeout   = 750 * time.Millisecond
	responsesPrecommitMaxPeekBytes  = 64 * 1024
	responsesPeekReadChunkSize      = 4 * 1024
	responsesFailureTapMaxBuffer    = 64 * 1024
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

func peekAndForwardResponses(h *ProxyHandler, w http.ResponseWriter, r *http.Request, resp *http.Response, upstreamCancel context.CancelFunc, model string) {
	peekAndForwardResponsesWithConfig(h, w, r, resp, upstreamCancel, model, responsesPrecommitPeekTimeout, responsesPrecommitMaxPeekBytes)
}

func peekAndForwardResponsesWithConfig(h *ProxyHandler, w http.ResponseWriter, r *http.Request, resp *http.Response, upstreamCancel context.CancelFunc, model string, peekTimeout time.Duration, maxPeekBytes int) {
	pr, pw := io.Pipe()
	peekDone := make(chan peekResult, 1)
	commitCh := make(chan struct{})
	abortCh := make(chan struct{})

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
			upstreamCancel()
			_ = resp.Body.Close()
			_ = pr.Close()
		})
	}

	go runResponsesPeekPump(resp.Body, pw, resp.Header, peekDone, commitCh, abortCh, maxPeekBytes)

	timer := time.NewTimer(peekTimeout)
	defer timer.Stop()

	handleCommit := func(result *peekResult) {
		if result != nil && result.failure != nil && result.decision == responsesPeekDecisionPassthrough {
			logResponsesPrecommitFailOpen(h, result.failure, model, resp.Header)
		}

		commit()
		copyPassthroughHeaders(w.Header(), resp.Header)
		w.WriteHeader(http.StatusOK)
		streamResponsesPipeWithFailureLog(h, w, pr, resp.Header)
		abort()
	}

	handleResult := func(result peekResult) {
		if result.decision == responsesPeekDecisionTranslate {
			logResponsesPrecommitTranslated(h, result, model, resp.Header)
			abort()
			writeOpenAIErrorWithRetryAfter(w, result.status, result.message, result.errType, result.retryAfter, resp.Header)
			return
		}
		handleCommit(&result)
	}

	for {
		select {
		case result := <-peekDone:
			handleResult(result)
			return
		case <-timer.C:
			select {
			case result := <-peekDone:
				handleResult(result)
				return
			default:
			}
			handleCommit(nil)
			return
		case <-r.Context().Done():
			abort()
			return
		}
	}
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

func streamResponsesPipeWithFailureLog(h *ProxyHandler, w http.ResponseWriter, r io.Reader, upstreamHeaders http.Header) {
	if closer, ok := r.(io.Closer); ok {
		defer func() { _ = closer.Close() }()
	}

	fw := &flushWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		fw.flusher = f
	}

	tap := newResponsesFailureTap(h, upstreamHeaders)
	_, _ = io.Copy(fw, io.TeeReader(r, tap))
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

		msg.semantic = true
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
		case "data":
			dataLines = append(dataLines, value)
		}
	}
}

type responsesFailureTap struct {
	h               *ProxyHandler
	upstreamHeaders http.Header
	parser          responsesSSEParser
}

func newResponsesFailureTap(h *ProxyHandler, upstreamHeaders http.Header) *responsesFailureTap {
	return &responsesFailureTap{
		h:               h,
		upstreamHeaders: upstreamHeaders,
		parser:          responsesSSEParser{allowBOM: true},
	}
}

func (t *responsesFailureTap) Write(p []byte) (int, error) {
	t.parser.push(p)
	for {
		msg, ok := t.parser.nextSemantic()
		if !ok {
			break
		}
		t.maybeLog(msg)
	}
	if len(t.parser.pending) > responsesFailureTapMaxBuffer {
		t.parser.pending = nil
		t.parser.allowBOM = false
	}
	return len(p), nil
}

func (t *responsesFailureTap) maybeLog(msg responsesSSEMessage) {
	eventName := strings.TrimSpace(msg.event)
	if eventName != "response.failed" && eventName != "response.incomplete" && eventName != "" {
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
	if eventName != "response.failed" && eventName != "response.incomplete" {
		return
	}

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
