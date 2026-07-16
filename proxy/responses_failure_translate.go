package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/sozercan/vekil/logger"
)

const (
	responsesPrecommitPeekTimeout          = 750 * time.Millisecond
	responsesPrecommitMaxPeekBytes         = 64 * 1024
	responsesPrecommitHeldPreambleMaxBytes = 512 * 1024
	responsesPeekReadChunkSize             = 4 * 1024
	responsesPeekCancellationGrace         = 10 * time.Millisecond
	// responsesFailureTapMaxBuffer bounds how much of an in-flight SSE event the
	// failure tap buffers while waiting for its delimiter. It matches the
	// supported Responses scanner limit so every accepted terminal event is fully
	// parsed before it can affect status. Larger events retain best-effort usage
	// only and are treated as over-limit/truncated rather than reconstructed from
	// unvalidated fragments.
	responsesFailureTapMaxBuffer = openAIStreamScannerMaxBuffer
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
	decision            responsesPeekDecision
	status              int
	errType             string
	message             string
	retryAfter          string
	retryAfterSource    string
	failure             *responsesWebSocketStreamEvent
	terminal            *responsesWebSocketStreamEvent
	bufferedBytes       int
	peekDuration        time.Duration
	preamble            bool
	precommitReplaySafe bool
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
	done     bool
}

// responsesTerminalObserver incrementally inspects committed Responses SSE
// without rescanning an incomplete event on every transport chunk. It buffers at
// at most the normal WebSocket stream parser's event limit and keeps a bounded
// tail so a larger response.completed can still publish terminal identity and
// usage without retaining the full model output.
type responsesTerminalObserver struct {
	headers       http.Header
	peekState     *responsesPeekState
	line          []byte
	data          []byte
	event         string
	usageTail     []byte
	overflowEvent responsesWebSocketStreamEvent
	lineOverflow  bool
	overflow      bool
	firstLine     bool
	done          bool
	skipEvent     bool
}

func newResponsesTerminalObserver(headers http.Header, state *responsesPeekState) *responsesTerminalObserver {
	return &responsesTerminalObserver{headers: headers, peekState: state, firstLine: true}
}

func (o *responsesTerminalObserver) Write(p []byte) {
	if o == nil || o.done {
		return
	}
	for _, b := range p {
		if o.done {
			return
		}
		if !o.skipEvent {
			o.usageTail = append(o.usageTail, b)
			if len(o.usageTail) > responsesFailureTapOverflowTail {
				o.usageTail = o.usageTail[len(o.usageTail)-responsesFailureTapOverflowTail:]
			}
		}
		if b == '\n' {
			o.finishLine()
			continue
		}
		if o.skipEvent {
			o.line = nil
			o.lineOverflow = true
			continue
		}
		if o.overflow {
			if b == '\r' && !o.lineOverflow && len(o.line) == 0 {
				o.line = append(o.line, b)
			} else {
				o.line = nil
				o.lineOverflow = true
			}
			continue
		}
		if o.lineOverflow {
			continue
		}
		if len(o.line)+len(o.data)+len(o.event) >= openAIStreamScannerMaxBuffer {
			o.rememberOverflowEvent(o.data)
			o.rememberOverflowEvent(o.line)
			o.line = nil
			o.data = nil
			o.lineOverflow = true
			o.overflow = true
			continue
		}
		o.line = append(o.line, b)
	}
}

func (o *responsesTerminalObserver) finishLine() {
	if o.lineOverflow {
		o.lineOverflow = false
		o.line = nil
		return
	}
	line := bytes.TrimSuffix(o.line, []byte{'\r'})
	o.line = nil
	if o.firstLine {
		line = bytes.TrimPrefix(line, []byte{0xEF, 0xBB, 0xBF})
		o.firstLine = false
	}
	if len(line) == 0 {
		o.finishEvent(true)
		return
	}
	if line[0] == ':' {
		return
	}
	field, value := line, []byte{}
	if colon := bytes.IndexByte(line, ':'); colon >= 0 {
		field = line[:colon]
		value = line[colon+1:]
		value = bytes.TrimPrefix(value, []byte{' '})
	}
	switch string(field) {
	case "event":
		if len(o.data)+len(value) > openAIStreamScannerMaxBuffer {
			o.rememberOverflowEvent(o.data)
			o.rememberOverflowEvent(value)
			o.data = nil
			o.overflow = true
			return
		}
		o.event = string(value)
		o.skipEvent = strings.TrimSpace(o.event) != "" && !isResponsesTerminalType(o.event)
		o.rememberOverflowEvent(value)
	case "data":
		if o.skipEvent {
			return
		}
		if o.overflow {
			return
		}
		separator := 0
		if len(o.data) > 0 {
			separator = 1
		}
		if len(o.data)+separator+len(value)+len(o.event) > openAIStreamScannerMaxBuffer {
			o.rememberOverflowEvent(o.data)
			o.rememberOverflowEvent(value)
			o.data = nil
			o.overflow = true
			return
		}
		if len(o.data) == 0 {
			o.data = value
			return
		}
		o.data = append(o.data, '\n')
		o.data = append(o.data, value...)
	}
}

func (o *responsesTerminalObserver) rememberOverflowEvent(buf []byte) {
	if o == nil {
		return
	}
	updateResponsesTerminalEvent(&o.overflowEvent, strings.TrimSpace(o.event), buf)
}

func (o *responsesTerminalObserver) finishEvent(explicitBoundary bool) {
	if o.skipEvent {
		o.data = nil
		o.event = ""
		o.usageTail = nil
		o.overflowEvent = responsesWebSocketStreamEvent{}
		o.overflow = false
		o.skipEvent = false
		return
	}
	if !o.overflow && strings.TrimSpace(string(o.data)) == "[DONE]" {
		o.data = nil
		o.event = ""
		o.usageTail = nil
		o.overflowEvent = responsesWebSocketStreamEvent{}
		o.done = true
		return
	}
	if o.overflow {
		// Fragment recovery is best-effort metadata only. Without the complete JSON
		// payload, even an explicit SSE boundary cannot make the event authoritative.
		// The normal websocket scanner will report an over-limit stream error. Usage
		// after a real boundary remains billable evidence and is retained separately.
		o.rememberOverflowEvent(o.usageTail)
		if isResponsesTerminalType(o.overflowEvent.Type) {
			// Status remains non-authoritative without the full JSON, but terminal
			// usage is still billable evidence whether EOF or a blank line ended it.
			o.peekState.publishRecoveredUsage(o.overflowEvent.Response.Usage)
		}
	} else if len(o.data) > 0 || strings.TrimSpace(o.event) != "" {
		result := classifyResponsesPeekMessage(responsesSSEMessage{event: o.event, data: string(o.data), semantic: true}, o.headers)
		o.peekState.publishTerminal(result)
	}
	o.data = nil
	o.event = ""
	o.usageTail = nil
	o.overflowEvent = responsesWebSocketStreamEvent{}
	o.overflow = false
	o.skipEvent = false
}

func (o *responsesTerminalObserver) FinalizeEOF() {
	if o == nil {
		return
	}
	if o.lineOverflow {
		o.lineOverflow = false
		o.line = nil
	} else if len(o.line) > 0 {
		o.finishLine()
	}
	if o.overflow || len(o.data) > 0 || strings.TrimSpace(o.event) != "" {
		o.finishEvent(false)
	}
}

func extractResponsesFailureObject(buf []byte) (responsesWebSocketStreamError, bool) {
	var result responsesWebSocketStreamError
	object, ok := extractResponsesNamedObject(buf, []byte(`"error"`))
	if !ok || json.Unmarshal(object, &result) != nil {
		return responsesWebSocketStreamError{}, false
	}
	return result, result.Code != "" || result.Type != "" || result.Message != ""
}

func extractResponsesIncompleteDetails(buf []byte) (responsesWebSocketStreamIncompleteDetails, bool) {
	var result responsesWebSocketStreamIncompleteDetails
	object, ok := extractResponsesNamedObject(buf, []byte(`"incomplete_details"`))
	if !ok || json.Unmarshal(object, &result) != nil {
		return responsesWebSocketStreamIncompleteDetails{}, false
	}
	return result, result.Reason != ""
}

func extractResponsesNamedObject(buf, key []byte) ([]byte, bool) {
	search := buf
	for {
		idx := bytes.LastIndex(search, key)
		if idx < 0 {
			return nil, false
		}
		cursor := idx + len(key)
		for cursor < len(search) && (search[cursor] == ' ' || search[cursor] == '\t' || search[cursor] == ':' || search[cursor] == '\r' || search[cursor] == '\n') {
			cursor++
		}
		if cursor < len(search) && search[cursor] == '{' {
			if object, end := balancedJSONObject(search[cursor:]); end > 0 {
				return object, true
			}
		}
		search = search[:idx]
	}
}

func isResponsesTerminalType(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.completed", "response.failed", "response.incomplete", "error":
		return true
	default:
		return false
	}
}

func extractResponsesTerminalType(buf []byte) string {
	lines := buf
	for len(lines) > 0 {
		lineEnd := bytes.IndexByte(lines, '\n')
		line := lines
		if lineEnd >= 0 {
			line = lines[:lineEnd]
		}
		line = bytes.TrimSuffix(line, []byte{'\r'})
		if colon := bytes.IndexByte(line, ':'); colon >= 0 && string(line[:colon]) == "event" {
			if eventType := strings.TrimSpace(string(line[colon+1:])); isResponsesTerminalType(eventType) {
				return eventType
			}
		}
		if lineEnd < 0 {
			break
		}
		lines = lines[lineEnd+1:]
	}

	const (
		responsesJSONExpectKey = iota
		responsesJSONExpectColon
		responsesJSONExpectValue
		responsesJSONAfterValue
	)
	const (
		responsesJSONStringIgnored = iota
		responsesJSONStringKey
		responsesJSONStringValue
	)

	depth := 0
	state := responsesJSONExpectKey
	keyIsType := false
	inString := false
	escaped := false
	stringEscaped := false
	stringRole := responsesJSONStringIgnored
	stringStart := 0

	for i, b := range buf {
		if inString {
			if escaped {
				escaped = false
				continue
			}
			if b == '\\' {
				escaped = true
				stringEscaped = true
				continue
			}
			if b != '"' {
				continue
			}

			value := buf[stringStart:i]
			switch stringRole {
			case responsesJSONStringKey:
				keyIsType = !stringEscaped && bytes.Equal(value, []byte("type"))
				state = responsesJSONExpectColon
			case responsesJSONStringValue:
				if keyIsType && !stringEscaped {
					eventType := string(value)
					if isResponsesTerminalType(eventType) {
						return eventType
					}
				}
				keyIsType = false
				state = responsesJSONAfterValue
			}
			inString = false
			stringRole = responsesJSONStringIgnored
			continue
		}

		if depth == 0 {
			if b == '{' {
				depth = 1
				state = responsesJSONExpectKey
				keyIsType = false
			}
			continue
		}

		if b == '"' {
			inString = true
			escaped = false
			stringEscaped = false
			stringStart = i + 1
			if depth == 1 {
				switch state {
				case responsesJSONExpectKey:
					stringRole = responsesJSONStringKey
				case responsesJSONExpectValue:
					stringRole = responsesJSONStringValue
				default:
					stringRole = responsesJSONStringIgnored
				}
			}
			continue
		}

		if depth == 1 {
			switch b {
			case ':':
				if state == responsesJSONExpectColon {
					state = responsesJSONExpectValue
				}
				continue
			case ',':
				state = responsesJSONExpectKey
				keyIsType = false
				continue
			case '}':
				depth = 0
				state = responsesJSONExpectKey
				keyIsType = false
				continue
			case ' ', '\t', '\r', '\n':
				continue
			}
			if state == responsesJSONExpectValue {
				state = responsesJSONAfterValue
				keyIsType = false
			}
		}

		switch b {
		case '{', '[':
			depth++
		case '}', ']':
			if depth > 0 {
				depth--
			}
		}
	}
	return ""
}

func extractResponsesResponseID(buf []byte) (string, bool) {
	start := bytes.IndexByte(buf, '{')
	if start < 0 {
		return "", false
	}
	dec := json.NewDecoder(bytes.NewReader(buf[start:]))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return "", false
	}
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return "", false
		}
		key, ok := keyToken.(string)
		if !ok {
			return "", false
		}
		if key != "response" {
			if err := skipJSONValue(dec); err != nil {
				return "", false
			}
			continue
		}
		tok, err := dec.Token()
		if err != nil || tok != json.Delim('{') {
			return "", false
		}
		for dec.More() {
			responseKeyToken, err := dec.Token()
			if err != nil {
				return "", false
			}
			responseKey, ok := responseKeyToken.(string)
			if !ok {
				return "", false
			}
			if responseKey == "id" {
				var responseID string
				if err := dec.Decode(&responseID); err != nil {
					return "", false
				}
				responseID = strings.TrimSpace(responseID)
				return responseID, responseID != ""
			}
			if err := skipJSONValue(dec); err != nil {
				return "", false
			}
		}
		return "", false
	}
	return "", false
}

func updateResponsesTerminalEvent(event *responsesWebSocketStreamEvent, hintedType string, buf []byte) {
	if event == nil {
		return
	}
	if event.Type == "" {
		if isResponsesTerminalType(hintedType) {
			event.Type = strings.TrimSpace(hintedType)
		} else {
			event.Type = extractResponsesTerminalType(buf)
		}
	}
	if event.Response.ID == "" {
		if responseID, ok := extractResponsesResponseID(buf); ok {
			event.Response.ID = responseID
		}
	}
	if usage, ok := extractResponsesUsageObject(buf); ok {
		event.Response.Usage = usage
	}
	if failure, ok := extractResponsesFailureObject(buf); ok {
		if strings.TrimSpace(event.Type) == "error" {
			event.Error = failure
		} else {
			event.Response.Error = failure
		}
	}
	if details, ok := extractResponsesIncompleteDetails(buf); ok {
		event.Response.IncompleteDetails = details
	}
}

func responsesStreamEventError(event responsesWebSocketStreamEvent) responsesWebSocketStreamError {
	if strings.TrimSpace(event.Type) != "error" {
		return event.Response.Error
	}
	// Canonical Responses error events carry code/message at the event root,
	// while Azure Foundry can wrap richer details and headers in an `error`
	// object. Start with the canonical shape and overlay any nested extension so
	// both forms retain their diagnostics.
	result := responsesWebSocketStreamError{
		Code:    event.Code,
		Message: event.Message,
		Param:   event.Param,
		Headers: mergeResponsesStreamErrorHeaders(event.Headers, event.Error.Headers),
	}
	if value := strings.TrimSpace(event.Error.Type); value != "" {
		result.Type = value
	}
	if value := strings.TrimSpace(event.Error.Code); value != "" {
		result.Code = value
	}
	if value := strings.TrimSpace(event.Error.Message); value != "" {
		result.Message = value
	}
	if value := strings.TrimSpace(event.Error.Param); value != "" {
		result.Param = value
	}
	return result
}

func mergeResponsesStreamErrorHeaders(root, nested map[string]json.RawMessage) map[string]json.RawMessage {
	if len(root) == 0 && len(nested) == 0 {
		return nil
	}
	merged := make(map[string]json.RawMessage, len(root)+len(nested))
	add := func(headers map[string]json.RawMessage) {
		for name, value := range headers {
			name = strings.TrimSpace(name)
			if name == "" {
				continue
			}
			merged[http.CanonicalHeaderKey(name)] = value
		}
	}
	add(root)
	// Nested Foundry metadata is the more specific representation and only
	// replaces root values with the same case-insensitive header name.
	add(nested)
	return merged
}

func responsesStreamErrorHeaders(streamErr responsesWebSocketStreamError) http.Header {
	if len(streamErr.Headers) == 0 {
		return nil
	}
	headers := make(http.Header)
	for name, raw := range streamErr.Headers {
		name = strings.TrimSpace(name)
		if name == "" || len(raw) == 0 {
			continue
		}
		var value string
		if json.Unmarshal(raw, &value) == nil {
			if value = strings.TrimSpace(value); value != "" {
				headers.Add(name, value)
			}
			continue
		}
		var values []string
		if json.Unmarshal(raw, &values) == nil {
			for _, value := range values {
				if value = strings.TrimSpace(value); value != "" {
					headers.Add(name, value)
				}
			}
			continue
		}
		var number json.Number
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&number) == nil {
			if value := strings.TrimSpace(number.String()); value != "" {
				headers.Add(name, value)
			}
		}
	}
	if len(headers) == 0 {
		return nil
	}
	return headers
}

func responsesFailureHeaders(event responsesWebSocketStreamEvent, upstream http.Header) http.Header {
	eventHeaders := responsesStreamErrorHeaders(responsesStreamEventError(event))
	if len(eventHeaders) == 0 {
		return upstream
	}
	merged := upstream.Clone()
	if merged == nil {
		merged = make(http.Header)
	}
	for name, values := range eventHeaders {
		deleteHeaderCI(merged, name)
		for _, value := range values {
			merged.Add(name, value)
		}
	}
	return merged
}

func deleteHeaderCI(headers http.Header, name string) {
	for key := range headers {
		if strings.EqualFold(key, name) {
			delete(headers, key)
		}
	}
}

type responsesPreparedStream struct {
	resp          *http.Response
	pr            *io.PipeReader
	peekDone      chan peekResult
	peekState     *responsesPeekState
	commitCh      chan struct{}
	abortCh       chan struct{}
	doneCh        chan struct{}
	commitFn      func()
	abortFn       func()
	observeOnlyFn func()
}

type responsesPeekState struct {
	observeTerminal         bool
	holdPreamble            bool
	mu                      sync.Mutex
	terminal                peekResult
	hasTerminal             bool
	terminalDone            chan struct{}
	doneOnce                sync.Once
	outcome                 responsesPeekReadOutcome
	hasOutcome              bool
	outcomeDone             chan struct{}
	outcomeOnce             sync.Once
	recoveredUsage          responsesUsage
	hasRecoveredUsage       bool
	heldPreambleMaxBytes    int
	stopTerminalObservation chan struct{}
	stopTerminalOnce        sync.Once
}

func newResponsesPeekState() *responsesPeekState {
	return &responsesPeekState{
		observeTerminal:         true,
		terminalDone:            make(chan struct{}),
		outcomeDone:             make(chan struct{}),
		stopTerminalObservation: make(chan struct{}),
	}
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
	if !s.hasTerminal {
		s.terminal = result
		s.hasTerminal = true
	}
	s.mu.Unlock()
	s.doneOnce.Do(func() { close(s.terminalDone) })
}

func (s *responsesPeekState) publishRecoveredUsage(usage responsesUsage) {
	if s == nil || usage.isZero() {
		return
	}
	s.mu.Lock()
	if !s.hasRecoveredUsage && !s.hasTerminal {
		s.recoveredUsage = usage
		s.hasRecoveredUsage = true
	}
	s.mu.Unlock()
}

func (s *responsesPeekState) recoveredUsageResult() (responsesUsage, bool) {
	if s == nil {
		return responsesUsage{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.recoveredUsage, s.hasRecoveredUsage
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
	reader        *io.PipeReader
	peekState     *responsesPeekState
	closeFn       func()
	observeOnlyFn func()
	closeOnce     sync.Once
	closeErr      error
}

func (b *responsesPreparedBody) terminalResultWithin(grace time.Duration) (peekResult, bool) {
	if b == nil || b.peekState == nil {
		return peekResult{}, false
	}
	if terminal, ok := b.peekState.terminalResult(); ok {
		return terminal, true
	}
	if b.observeOnlyFn != nil {
		b.observeOnlyFn()
	}
	if grace <= 0 {
		return peekResult{}, false
	}
	timer := time.NewTimer(grace)
	defer timer.Stop()
	select {
	case <-b.peekState.terminalDone:
		return b.peekState.terminalResult()
	case <-b.peekState.outcomeDone:
		if terminal, ok := b.peekState.terminalResult(); ok {
			return terminal, true
		}
		return peekResult{}, false
	case <-timer.C:
		return peekResult{}, false
	}
}

func (b *responsesPreparedBody) recoveredUsage() (responsesUsage, bool) {
	if b == nil || b.peekState == nil {
		return responsesUsage{}, false
	}
	return b.peekState.recoveredUsageResult()
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

func newResponsesPreparedStream(resp *http.Response, maxPeekBytes int, observeTerminal bool) *responsesPreparedStream {
	return newResponsesPreparedStreamConfigured(resp, maxPeekBytes, observeTerminal, false, responsesPrecommitHeldPreambleMaxBytes)
}

func newResponsesPreparedStreamConfigured(resp *http.Response, maxPeekBytes int, observeTerminal, holdPreamble bool, heldPreambleMaxBytes int) *responsesPreparedStream {
	pr, pw := io.Pipe()
	peekDone := make(chan peekResult, 1)
	peekState := newResponsesPeekState()
	peekState.observeTerminal = observeTerminal
	peekState.holdPreamble = holdPreamble
	if heldPreambleMaxBytes <= 0 {
		heldPreambleMaxBytes = responsesPrecommitHeldPreambleMaxBytes
	}
	peekState.heldPreambleMaxBytes = heldPreambleMaxBytes
	commitCh := make(chan struct{})
	abortCh := make(chan struct{})
	observeOnlyCh := make(chan struct{})
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
			go func() { _ = upstreamBody.Close() }()
		})
	}
	var observeOnlyOnce sync.Once
	observeOnly := func() {
		observeOnlyOnce.Do(func() { close(observeOnlyCh) })
	}

	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		runResponsesPeekPump(upstreamBody, pw, resp.Header, peekDone, peekState, commitCh, abortCh, observeOnlyCh, maxPeekBytes)
	}()

	return &responsesPreparedStream{
		resp:          resp,
		pr:            pr,
		peekDone:      peekDone,
		peekState:     peekState,
		commitCh:      commitCh,
		abortCh:       abortCh,
		doneCh:        doneCh,
		commitFn:      commit,
		abortFn:       abort,
		observeOnlyFn: observeOnly,
	}
}

func (s *responsesPreparedStream) stopTerminalObservation() {
	if s == nil || s.peekState == nil || s.peekState.stopTerminalObservation == nil {
		return
	}
	s.peekState.stopTerminalOnce.Do(func() { close(s.peekState.stopTerminalObservation) })
}

func (s *responsesPreparedStream) terminalResult() (peekResult, bool) {
	if s == nil || s.peekState == nil {
		return peekResult{}, false
	}
	return s.peekState.terminalResult()
}

func isIndependentResponsesPeekOutcome(outcome responsesPeekReadOutcome) bool {
	if outcome.err == nil || outcome.lifecycleCanceledAtFailure {
		return false
	}
	return !errors.Is(outcome.err, context.Canceled) && !errors.Is(outcome.err, context.DeadlineExceeded)
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
	s.resp.Body = &responsesPreparedBody{reader: s.pr, peekState: s.peekState, closeFn: s.abortFn, observeOnlyFn: s.observeOnlyFn}
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

	prepared := newResponsesPreparedStream(resp, maxPeekBytes, true)
	result, hasResult, awaitSource, err := prepared.await(r.Context(), upstreamCtx, peekTimeout)
	// Inbound cancellation owns the downstream response even if the peek pump
	// publishes a simultaneous passthrough decision. Do not race a 200 header
	// against a client that has already gone away.
	if r.Context().Err() != nil {
		prepared.abort()
		return
	}
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
	if lifecycleCanceled && (!hasResult || result.terminal == nil) {
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
		failureHeaders := resp.Header
		if result.failure != nil {
			failureHeaders = responsesFailureHeaders(*result.failure, resp.Header)
		}
		logResponsesPrecommitTranslated(h, result, model, failureHeaders)
		if result.failure != nil {
			observeResponsesUsage(r.Context(), result.failure.Response.Usage)
		}
		prepared.abort()
		errorCode, errorParam := "", ""
		if result.failure != nil {
			streamErr := responsesStreamEventError(*result.failure)
			errorCode = strings.TrimSpace(streamErr.Code)
			errorParam = strings.TrimSpace(streamErr.Param)
		}
		writeOpenAIErrorWithRetryAfterDetails(w, result.status, result.message, result.errType, result.retryAfter, failureHeaders, errorParam, errorCode)
		return
	}
	if !hasResult || result.terminal == nil {
		if terminal, ok := prepared.terminalResult(); ok {
			terminal.decision = responsesPeekDecisionPassthrough
			result, hasResult = terminal, true
		}
	}
	if (!hasResult || result.terminal == nil) && lifecycle.transportCanceled != nil && lifecycle.transportCanceled() {
		terminal, hasTerminal, _, _ := prepared.awaitCancellationResolution(responsesPeekCancellationGrace)
		if hasTerminal {
			terminal.decision = responsesPeekDecisionPassthrough
			result, hasResult = terminal, true
		} else {
			prepared.abort()
			lifecycle.suppressKnownTransportCancellation(false)
			return
		}
	}
	if hasResult && result.failure != nil && result.decision == responsesPeekDecisionPassthrough {
		logResponsesPrecommitFailOpen(h, result.failure, model, responsesFailureHeaders(*result.failure, resp.Header))
	}
	// The committed HTTP path has its own bounded failure tap. Stop the
	// speculative observer here so large response.completed payloads are not
	// buffered twice for the remainder of the stream.
	prepared.stopTerminalObservation()
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
	return prepareResponsesStreamAttemptWithGrace(waitCtx, streamCtx, responsesPeekCancellationGrace, request)
}

func prepareResponsesStreamAttemptWithGrace(waitCtx, streamCtx context.Context, cancellationGrace time.Duration, request func() (*http.Response, error)) (*http.Response, *peekResult, http.Header, error) {
	resp, err := request()
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
		return resp, nil, nil, err
	}

	prepared := newResponsesPreparedStream(resp, responsesPrecommitMaxPeekBytes, true)
	result, hasResult, awaitSource, err := prepared.await(waitCtx, streamCtx, responsesPrecommitPeekTimeout)
	if err != nil {
		terminal, hasTerminal, outcome, hasOutcome := prepared.awaitCancellationResolution(cancellationGrace)
		if hasTerminal {
			terminal.decision = responsesPeekDecisionPassthrough
			if awaitSource == responsesPreparedAwaitInbound {
				prepared.abort()
				return nil, &terminal, resp.Header.Clone(), nil
			}
			// Upstream timeout/shutdown lost the await race to an authoritative
			// terminal event. Commit the prepared body so the websocket bridge can
			// forward the original bytes and run normal session finalization.
			return prepared.commitResponse(), &terminal, nil, nil
		}
		if hasOutcome && isIndependentResponsesPeekOutcome(outcome) {
			// The upstream body independently reached EOF or a transport reset while
			// cancellation was winning the await race. Preserve that provider outcome
			// on the prepared body instead of replacing it with local cancellation.
			return prepared.commitResponse(), nil, nil, nil
		}
		prepared.abort()
		return nil, nil, nil, err
	}
	if streamCtx != nil && streamCtx.Err() != nil && errors.Is(context.Cause(streamCtx), errProxyLifecycleShutdown) && (!hasResult || result.terminal == nil) {
		terminal, hasTerminal, outcome, hasOutcome := prepared.awaitCancellationResolution(cancellationGrace)
		switch {
		case hasTerminal:
			terminal.decision = responsesPeekDecisionPassthrough
			result, hasResult = terminal, true
		case hasOutcome && isIndependentResponsesPeekOutcome(outcome):
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
		translatedHeaders := resp.Header
		if result.failure != nil {
			translatedHeaders = responsesFailureHeaders(*result.failure, resp.Header)
		}
		prepared.abort()
		return nil, &result, translatedHeaders.Clone(), nil
	}
	if hasResult && result.terminal != nil {
		return prepared.commitResponse(), &result, nil, nil
	}
	return prepared.commitResponse(), nil, nil, nil
}

func (h *ProxyHandler) prepareResponsesStream(waitCtx, streamCtx context.Context, model string, request func() (*http.Response, error)) (*http.Response, *peekResult, http.Header, error) {
	return h.prepareResponsesStreamWithGrace(waitCtx, streamCtx, model, responsesPeekCancellationGrace, request)
}

func (h *ProxyHandler) prepareResponsesStreamWithGrace(waitCtx, streamCtx context.Context, model string, cancellationGrace time.Duration, request func() (*http.Response, error)) (*http.Response, *peekResult, http.Header, error) {
	resp, result, translatedHeaders, err := prepareResponsesStreamAttemptWithGrace(waitCtx, streamCtx, cancellationGrace, request)
	if err != nil || result == nil {
		return resp, nil, nil, err
	}
	if result.decision == responsesPeekDecisionTranslate {
		logResponsesPrecommitTranslated(h, *result, model, translatedHeaders)
		return nil, result, translatedHeaders, nil
	}
	headers := translatedHeaders
	if resp != nil {
		headers = resp.Header
	}
	if result.failure != nil {
		logResponsesPrecommitFailOpen(h, result.failure, model, headers)
	}
	if result.terminal != nil {
		// A cancellation race can salvage an authoritative terminal event after the
		// prepared response has already been aborted. Propagate it even without resp
		// so the production websocket wrapper can retain terminal accounting.
		return resp, result, translatedHeaders, nil
	}
	return resp, nil, nil, nil
}

func runResponsesPeekPump(body io.ReadCloser, pw *io.PipeWriter, headers http.Header, peekDone chan<- peekResult, peekState *responsesPeekState, commitCh, abortCh, observeOnlyCh <-chan struct{}, maxPeekBytes int) {
	chunkCh := make(chan responsesPeekChunk, 1)
	go readResponsesPeekChunks(body, chunkCh, abortCh, observeOnlyCh, headers, peekState)

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
			// The abort function closes the upstream body. Wait for the read pump
			// to observe that close before publishing cleanup completion so a new
			// route target cannot overlap the abandoned attempt.
			for range chunkCh {
			}
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
				holdPreamble := peekState != nil && peekState.holdPreamble
				if !holdPreamble {
					holdPreamble = shouldHoldResponsesPrecommitPreamble(headers)
				}
				result, sawSemantic, sawBeyondPreamble := inspectResponsesPeekMessages(&parser, headers, peekState, holdPreamble)
				result.bufferedBytes = prefix.Len()
				result.peekDuration = time.Since(start)
				if !decisionSent {
					heldPreambleMaxBytes := responsesPrecommitHeldPreambleMaxBytes
					if peekState != nil && peekState.heldPreambleMaxBytes > 0 {
						heldPreambleMaxBytes = peekState.heldPreambleMaxBytes
					}
					if heldPreambleMaxBytes < maxPeekBytes {
						heldPreambleMaxBytes = maxPeekBytes
					}
					if result.decision == responsesPeekDecisionTranslate {
						sendResult(result)
					} else if prefix.Len() >= maxPeekBytes && (!holdPreamble || sawBeyondPreamble || prefix.Len() >= heldPreambleMaxBytes) {
						if sawSemantic {
							sendResult(result)
						} else {
							sendResult(peekResult{decision: responsesPeekDecisionPassthrough})
						}
					} else if sawSemantic && (!holdPreamble || sawBeyondPreamble) {
						sendResult(result)
					}
				}
			}

			if chunk.err != nil {
				if errors.Is(chunk.err, io.EOF) && !decisionSent {
					parser.finalizeEOF()
					holdPreamble := shouldHoldResponsesPrecommitPreamble(headers)
					result, sawSemantic, _ := inspectResponsesPeekMessages(&parser, headers, peekState, holdPreamble)
					if result.decision == responsesPeekDecisionTranslate || sawSemantic {
						sendResult(result)
					}
				}
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

func responsesFailureOutputHasProgress(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("[]")) || bytes.Equal(trimmed, []byte("{}")) || bytes.Equal(trimmed, []byte(`""`)) {
		return false
	}
	var items []json.RawMessage
	if json.Unmarshal(trimmed, &items) == nil {
		return len(items) > 0
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		return strings.TrimSpace(text) != ""
	}
	return true
}

func inspectResponsesPeekMessages(parser *responsesSSEParser, headers http.Header, peekState *responsesPeekState, preferTranslatedFailure bool) (peekResult, bool, bool) {
	result := peekResult{decision: responsesPeekDecisionPassthrough}
	sawSemantic := false
	sawBeyondPreamble := false
	sawUnsafeProgress := false
	for {
		msg, ok := parser.nextSemantic()
		if !ok {
			break
		}
		if strings.TrimSpace(msg.data) == "[DONE]" {
			parser.done = true
			break
		}
		classified := classifyResponsesPeekMessage(msg, headers)
		unsafeBeforeMessage := sawUnsafeProgress
		failureHasOutput := classified.failure != nil && responsesFailureOutputHasProgress(classified.failure.Response.Output)
		failureHasUsage := classified.failure != nil && !classified.failure.Response.Usage.isZero()
		if classified.failure != nil && !unsafeBeforeMessage && !failureHasOutput && !failureHasUsage {
			classified.precommitReplaySafe = true
		}
		if failureHasOutput || failureHasUsage {
			// Partial output and usage both prove that the target made semantic
			// progress, so another target must not be attempted. Usage alone does
			// not prevent translating a still-uncommitted terminal failure into
			// its HTTP error; embedded output does, because translation would drop
			// client-visible model output carried by the failure event.
			sawUnsafeProgress = true
		}
		if failureHasOutput {
			classified.decision = responsesPeekDecisionPassthrough
		}
		peekState.publishTerminal(classified)
		if !sawSemantic || (preferTranslatedFailure && !unsafeBeforeMessage && !failureHasOutput && classified.decision == responsesPeekDecisionTranslate) {
			result = classified
		}
		if !classified.preamble {
			sawBeyondPreamble = true
			if classified.terminal == nil || classified.failure == nil {
				sawUnsafeProgress = true
			}
		}
		sawSemantic = true
	}
	return result, sawSemantic, sawBeyondPreamble
}

func readResponsesPeekChunks(body io.ReadCloser, chunkCh chan<- responsesPeekChunk, abortCh, observeOnlyCh <-chan struct{}, headers http.Header, peekState *responsesPeekState) {
	defer close(chunkCh)

	buf := make([]byte, responsesPeekReadChunkSize)
	var terminalObserver *responsesTerminalObserver
	if peekState == nil || peekState.observeTerminal {
		terminalObserver = newResponsesTerminalObserver(headers, peekState)
	}
	observeOnly := false
	for {
		n, err := body.Read(buf)
		if n > 0 {
			chunk := append([]byte(nil), buf[:n]...)
			if terminalObserver != nil && peekState != nil && peekState.stopTerminalObservation != nil {
				select {
				case <-peekState.stopTerminalObservation:
					terminalObserver = nil
				default:
				}
			}
			if terminalObserver != nil {
				terminalObserver.Write(chunk)
			}
			if !observeOnly {
				select {
				case chunkCh <- responsesPeekChunk{data: chunk}:
				case <-observeOnlyCh:
					observeOnly = true
				case <-abortCh:
					return
				}
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) && terminalObserver != nil {
				terminalObserver.FinalizeEOF()
			}
			lifecycleCanceledAtFailure := false
			if observer, ok := body.(interface{ canceledAtFailure() bool }); ok {
				lifecycleCanceledAtFailure = observer.canceledAtFailure()
			}
			peekState.publishOutcome(responsesPeekReadOutcome{err: err, lifecycleCanceledAtFailure: lifecycleCanceledAtFailure})
			if !observeOnly {
				select {
				case chunkCh <- responsesPeekChunk{err: err, lifecycleCanceledAtFailure: lifecycleCanceledAtFailure}:
				case <-observeOnlyCh:
				case <-abortCh:
				}
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
	event, err := parseResponsesStreamEvent(msg.data)
	if err != nil {
		return peekResult{decision: responsesPeekDecisionPassthrough}
	}
	return classifyResponsesPeekEvent(event, msg.event, headers)
}

func classifyResponsesPeekEvent(event responsesWebSocketStreamEvent, eventName string, headers http.Header) peekResult {
	result := peekResult{decision: responsesPeekDecisionPassthrough}
	eventName = strings.TrimSpace(eventName)
	terminalType := strings.TrimSpace(event.Type)
	if terminalType == "" {
		terminalType = eventName
	}
	event.Type = terminalType
	result.preamble = terminalType == "response.created" || terminalType == "response.in_progress"
	switch terminalType {
	case "response.completed", "response.failed", "response.incomplete", "error":
		terminal := event
		result.terminal = &terminal
		if terminalType == "response.failed" || terminalType == "response.incomplete" || terminalType == "error" {
			result.failure = &terminal
		}
	}
	if terminalType != "response.failed" && terminalType != "error" {
		return result
	}

	failureHeaders := responsesFailureHeaders(event, headers)
	status, errType, ok := classifyResponsesFailure(event, failureHeaders)
	if !ok {
		return result
	}

	retryAfter, source := "", ""
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable || status == http.StatusGatewayTimeout {
		retryAfter, source = selectResponsesRetryAfter(failureHeaders)
	}

	result.decision = responsesPeekDecisionTranslate
	result.status = status
	result.errType = errType
	result.message = responsesPrecommitErrorMessage(event, status)
	result.retryAfter = retryAfter
	result.retryAfterSource = source
	return result
}

// shouldHoldResponsesPrecommitPreamble keeps Azure/compatible streaming
// responses uncommitted when their headers already report exhausted quota.
// These upstreams can emit response.created and then fail the stream a few
// hundred milliseconds later with response.failed. Waiting within the existing
// precommit timeout lets the proxy translate that terminal event into HTTP 429
// so clients can apply their normal Retry-After behavior. Healthy streams and
// providers without exhausted-quota headers retain the low-latency first-event
// passthrough path.
func shouldHoldResponsesPrecommitPreamble(headers http.Header) bool {
	return responsesQuotaEvidence(headers)
}

func classifyResponsesFailure(event responsesWebSocketStreamEvent, headers http.Header) (int, string, bool) {
	headers = responsesFailureHeaders(event, headers)
	streamErr := responsesStreamEventError(event)
	code := strings.ToLower(strings.TrimSpace(streamErr.Code))
	errType := strings.ToLower(strings.TrimSpace(streamErr.Type))

	if strings.TrimSpace(event.Type) == "error" {
		if status, resultType, ok := classifyResponsesErrorEventCode(code); ok {
			return status, resultType, true
		}
		switch errType {
		case "too_many_requests", "rate_limit_error", "rate_limit_exceeded":
			return http.StatusTooManyRequests, "rate_limit_error", true
		case "overloaded_error", "model_overloaded", "engine_overloaded", "service_unavailable":
			return http.StatusServiceUnavailable, "server_error", true
		case "forbidden", "permission_error":
			return http.StatusForbidden, "permission_error", true
		case "user_error", "invalid_request_error":
			return http.StatusBadRequest, "invalid_request_error", true
		case "authentication_error":
			return http.StatusUnauthorized, "authentication_error", true
		case "not_found_error":
			return http.StatusNotFound, "not_found_error", true
		case "conflict_error":
			return http.StatusConflict, "conflict_error", true
		}
		if code == "" && (errType == "" || errType == "server_error") && responsesQuotaEvidence(headers) && responsesRateLimitMessage(streamErr.Message) {
			return http.StatusTooManyRequests, "rate_limit_error", true
		}
		if errType == "server_error" {
			return http.StatusInternalServerError, "server_error", true
		}
		// A top-level Responses error event is terminal even when a provider uses an
		// unknown type/code. Translate it before commit rather than returning HTTP
		// 200 for a stream that can never produce response.completed.
		return http.StatusBadGateway, "server_error", true
	}

	if status, errType, ok := classifyPrecommitResponsesFailure(event); ok {
		return status, errType, true
	}
	if code == "" && (errType == "" || errType == "server_error") &&
		responsesQuotaEvidence(headers) && responsesRateLimitMessage(streamErr.Message) {
		return http.StatusTooManyRequests, "rate_limit_error", true
	}
	return 0, "", false
}

func classifyResponsesErrorEventCode(code string) (int, string, bool) {
	if status, errType, ok := classifyResponsesErrorCode(code); ok {
		return status, errType, true
	}
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "400", "invalid_prompt", "bio_policy",
		"invalid_image", "invalid_image_format", "invalid_base64_image", "invalid_image_url",
		"image_too_large", "image_too_small", "image_parse_error", "image_content_policy_violation",
		"invalid_image_mode", "image_file_too_large", "unsupported_image_media_type", "empty_image_file",
		"failed_to_download_image":
		return http.StatusBadRequest, "invalid_request_error", true
	case "401", "authentication_error":
		return http.StatusUnauthorized, "authentication_error", true
	case "403", "forbidden", "permission_error":
		return http.StatusForbidden, "permission_error", true
	case "404", "not_found", "not_found_error", "image_file_not_found":
		return http.StatusNotFound, "not_found_error", true
	case "409", "conflict", "conflict_error":
		return http.StatusConflict, "conflict_error", true
	case "500", "server_error":
		return http.StatusInternalServerError, "server_error", true
	case "vector_store_timeout":
		return http.StatusGatewayTimeout, "server_error", true
	default:
		return 0, "", false
	}
}

func responsesQuotaEvidence(headers http.Header) bool {
	if _, source := selectResponsesRetryAfter(headers); source == "retry-after-ms" || source == "Retry-After" {
		return true
	}
	for _, dimension := range []string{"tokens", "requests"} {
		remaining, exhausted := responsesQuotaRemaining(headers, dimension)
		if exhausted && remaining != -1 {
			return true
		}
	}
	return false
}

func responsesRateLimitMessage(message string) bool {
	message = strings.ToLower(strings.TrimSpace(message))
	return strings.Contains(message, "rate limit") || strings.Contains(message, "too many requests") || strings.Contains(message, "quota exceeded")
}

func responsesQuotaRemaining(headers http.Header, dimension string) (int64, bool) {
	value := strings.TrimSpace(headerGetCI(headers, "x-ratelimit-remaining-"+dimension))
	remaining, err := strconv.ParseInt(value, 10, 64)
	if err != nil || remaining > 0 {
		return 0, false
	}
	return remaining, true
}

func normalizePositiveDecimal(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", false
	}
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return "", false
		}
	}
	value = strings.TrimLeft(value, "0")
	if value == "" {
		return "", false
	}
	return value, true
}

func decimalMillisecondsToRetryAfterSeconds(value string) (string, bool) {
	normalized, ok := normalizePositiveDecimal(value)
	if !ok {
		return "", false
	}
	milliseconds, ok := new(big.Int).SetString(normalized, 10)
	if !ok {
		return "", false
	}
	milliseconds.Add(milliseconds, big.NewInt(999))
	milliseconds.Quo(milliseconds, big.NewInt(1000))
	if milliseconds.Sign() <= 0 {
		return "", false
	}
	return milliseconds.String(), true
}

func retryAfterHeaderSeconds(value string) (string, bool) {
	value = strings.TrimSpace(value)
	if normalized, ok := normalizePositiveDecimal(value); ok {
		return normalized, true
	}
	retryAt, err := http.ParseTime(value)
	if err != nil {
		return "", false
	}
	now := time.Now()
	if !retryAt.After(now) {
		return "", false
	}
	seconds := retryAt.Unix() - now.Unix()
	if seconds <= 0 {
		seconds = 1
	}
	return strconv.FormatInt(seconds, 10), true
}

func responsesQuotaResetRetryAfter(headers http.Header, dimension string) (string, *big.Int, bool) {
	value := strings.TrimSpace(headerGetCI(headers, "x-ratelimit-reset-"+dimension))
	if normalized, ok := normalizePositiveDecimal(value); ok {
		seconds, parsed := new(big.Int).SetString(normalized, 10)
		return normalized, seconds, parsed && seconds.Sign() > 0
	}
	delay, err := time.ParseDuration(value)
	if err != nil || delay <= 0 {
		return "", nil, false
	}
	seconds := durationSecondsCeil(delay)
	if seconds <= 0 {
		return "", nil, false
	}
	value = strconv.FormatInt(seconds, 10)
	parsed, _ := new(big.Int).SetString(value, 10)
	return value, parsed, true
}

func classifyPrecommitResponsesFailure(event responsesWebSocketStreamEvent) (int, string, bool) {
	streamErr := responsesStreamEventError(event)
	code := strings.ToLower(strings.TrimSpace(streamErr.Code))
	if status, errType, ok := classifyResponsesErrorCode(code); ok {
		return status, errType, true
	}

	errType := strings.ToLower(strings.TrimSpace(streamErr.Type))
	switch {
	case errType == "too_many_requests" || (code == "" && (errType == "rate_limit_error" || errType == "rate_limit_exceeded")):
		return http.StatusTooManyRequests, "rate_limit_error", true
	case errType == "overloaded_error" || errType == "model_overloaded" || errType == "engine_overloaded" || errType == "service_unavailable":
		return http.StatusServiceUnavailable, "server_error", true
	}

	return 0, "", false
}

func classifyResponsesErrorCode(code string) (int, string, bool) {
	switch strings.ToLower(strings.TrimSpace(code)) {
	case "429", "too_many_requests", "rate_limit_exceeded", "rate_limit_error", "quota_exceeded":
		return http.StatusTooManyRequests, "rate_limit_error", true
	case "503", "model_overloaded", "engine_overloaded", "overloaded_error", "service_unavailable":
		return http.StatusServiceUnavailable, "server_error", true
	case "502", "bad_gateway":
		return http.StatusBadGateway, "server_error", true
	case "504", "timeout", "gateway_timeout":
		return http.StatusGatewayTimeout, "server_error", true
	default:
		return 0, "", false
	}
}

func selectResponsesRetryAfter(headers http.Header) (string, string) {
	if headers == nil {
		return "", ""
	}

	// This value is forwarded to the client. Keep the proxy's bounded internal
	// sleep policy separate so an authoritative provider delay is never shortened.
	if seconds, ok := decimalMillisecondsToRetryAfterSeconds(headerGetCI(headers, "retry-after-ms")); ok {
		return seconds, "retry-after-ms"
	}

	if seconds, ok := retryAfterHeaderSeconds(headerGetCI(headers, "Retry-After")); ok {
		return seconds, "Retry-After"
	}

	var resetSeconds *big.Int
	resetValue := ""
	resetSource := ""
	for _, dimension := range []string{"tokens", "requests"} {
		remaining, exhausted := responsesQuotaRemaining(headers, dimension)
		if !exhausted || remaining == -1 {
			continue
		}
		value, seconds, ok := responsesQuotaResetRetryAfter(headers, dimension)
		if !ok || (resetSeconds != nil && seconds.Cmp(resetSeconds) <= 0) {
			continue
		}
		resetSeconds = seconds
		resetValue = value
		resetSource = "x-ratelimit-reset-" + dimension
	}
	if resetValue != "" {
		return resetValue, resetSource
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
	tap.finalizeEOF()

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
	streamErr := responsesStreamEventError(event)
	message := strings.TrimSpace(streamErr.Message)
	if message != "" {
		return message
	}
	if code := strings.TrimSpace(streamErr.Code); code != "" {
		return code
	}
	if errType := strings.TrimSpace(streamErr.Type); errType != "" {
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
	streamErr := responsesStreamEventError(*result.failure)
	h.log.Info("translated responses stream failure before commit",
		logger.F("endpoint", "responses_precommit_translated"),
		logger.F("status", result.status),
		logger.F("error_code", strings.TrimSpace(streamErr.Code)),
		logger.F("error_type", strings.TrimSpace(streamErr.Type)),
		logger.F("error_message", truncateResponsesFailureLogMessage(streamErr.Message)),
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
	streamErr := responsesStreamEventError(*event)
	h.log.Info("left responses stream failure as passthrough",
		logger.F("endpoint", "responses_precommit_failopen"),
		logger.F("error_code", strings.TrimSpace(streamErr.Code)),
		logger.F("error_type", strings.TrimSpace(streamErr.Type)),
		logger.F("error_message", truncateResponsesFailureLogMessage(streamErr.Message)),
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

func (p *responsesSSEParser) finalizeEOF() {
	if p == nil || p.done || len(p.pending) == 0 {
		return
	}
	// The streaming consumer dispatches a final SSE event at EOF even if the
	// provider omits the conventional blank-line delimiter. Add only parser-local
	// framing so the pre-commit classifier follows the same rule without changing
	// the raw bytes later forwarded on a fail-open path.
	if p.pending[len(p.pending)-1] == '\n' {
		p.pending = append(p.pending, '\n')
		return
	}
	p.pending = append(p.pending, '\n', '\n')
}

func (p *responsesSSEParser) nextSemantic() (responsesSSEMessage, bool) {
	if p.done {
		return responsesSSEMessage{}, false
	}
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
	h                   *ProxyHandler
	upstreamHeaders     http.Header
	ctx                 context.Context
	store               *ToolExecutionContextStore
	scope               string
	responseScope       string
	parser              responsesSSEParser
	pendingBoundaryTail []byte
	// overflowed is set while a single SSE event has exceeded the buffer cap, so
	// later writes keep best-effort extracting terminal details from the rolling
	// tail after the normal parser buffer is dropped.
	overflowed bool
	// overflowActive remains true while forwarding the rest of an overflowed event.
	// It is cleared only after that event's explicit blank-line boundary is seen.
	overflowActive bool
	// overflowEvent retains the terminal type plus bounded usage/failure details
	// recovered from the prefix before overflow and the rolling tail afterward.
	overflowEvent        responsesWebSocketStreamEvent
	overflowBoundaryTail []byte
	// overflowUsageRecorded avoids re-observing the same partial usage on every
	// subsequent write while the oversized event is still in flight.
	overflowUsageRecorded bool
	// usageTail is a bounded rolling window of the most recent raw stream bytes.
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
	total := len(p)
	const maxChunk = responsesFailureTapOverflowTail
	for len(p) > 0 {
		chunkSize := min(len(p), maxChunk)
		t.writeChunk(p[:chunkSize])
		p = p[chunkSize:]
	}
	return total, nil
}

func (t *responsesFailureTap) finalizeEOF() {
	if t == nil || t.terminalSeen || t.overflowActive {
		return
	}
	// A valid final SSE event is dispatched at EOF even when the provider omits
	// the conventional blank-line delimiter. Add parser-only framing and route the
	// event through the same terminal/accounting path without altering wire bytes.
	t.parser.finalizeEOF()
	for {
		msg, ok := t.parser.nextSemantic()
		if !ok {
			return
		}
		if strings.TrimSpace(msg.data) == "[DONE]" {
			t.parser.done = true
			return
		}
		t.maybeProcess(msg)
	}
}

func (t *responsesFailureTap) writeChunk(p []byte) {
	if len(p) == 0 {
		return
	}
	if t.overflowActive {
		if remaining := t.consumeOverflowChunk(p); len(remaining) > 0 {
			t.writeChunk(remaining)
		}
		return
	}

	// Maintain a small rolling tail independent of the SSE parser so terminal
	// type, failure details, and usage can be recovered after an oversized event
	// forces the normal parser buffer to be dropped.
	t.appendUsageTail(p)
	t.parser.push(p)

	combinedBoundary := append(append([]byte(nil), t.pendingBoundaryTail...), p...)
	if responsesSSEBoundaryEnd(combinedBoundary) == 0 {
		if len(t.parser.pending) > responsesFailureTapMaxBuffer {
			t.startOverflowFromPending()
			return
		}
		t.pendingBoundaryTail = trailingBytes(combinedBoundary, 3)
		return
	}

	for {
		boundaryEnd := responsesSSEBoundaryEnd(t.parser.pending)
		if boundaryEnd == 0 {
			break
		}
		if boundaryEnd > responsesFailureTapMaxBuffer {
			// The first complete event itself exceeds the supported parser limit.
			// Recover usage from that event only, discard its unvalidated status,
			// then continue parsing any following event bytes from the same write.
			overflowEvent := append([]byte(nil), t.parser.pending[:boundaryEnd]...)
			remainder := append([]byte(nil), t.parser.pending[boundaryEnd:]...)
			t.overflowed = true
			t.overflowActive = true
			t.usageTail = trailingBytes(overflowEvent, responsesFailureTapOverflowTail)
			t.rememberOverflowEvent(overflowEvent)
			t.sniffUsageFromOverflow(t.usageTail)
			t.finishOverflowEvent()
			t.parser.pending = nil
			t.parser.allowBOM = false
			t.pendingBoundaryTail = nil
			if len(remainder) > 0 {
				t.writeChunk(remainder)
			}
			return
		}

		msg, consumed, incomplete := nextResponsesSSEMessage(t.parser.pending, t.parser.allowBOM)
		if incomplete {
			break
		}
		t.parser.allowBOM = false
		t.parser.pending = t.parser.pending[consumed:]
		if msg.semantic {
			t.maybeProcess(msg)
		}
		t.usageTail = trailingBytes(t.parser.pending, responsesFailureTapOverflowTail)
	}

	if len(t.parser.pending) > responsesFailureTapMaxBuffer {
		t.startOverflowFromPending()
		return
	}
	t.pendingBoundaryTail = trailingBytes(t.parser.pending, 3)

}

func (t *responsesFailureTap) startOverflowFromPending() {
	if t == nil || len(t.parser.pending) == 0 {
		return
	}
	// The current event exceeded the limit before its delimiter arrived. Retain
	// only bounded usage hints and let later writes locate the boundary.
	t.overflowed = true
	t.overflowActive = true
	t.rememberOverflowEvent(t.parser.pending)
	t.overflowBoundaryTail = trailingBytes(t.parser.pending, 3)
	t.sniffUsageFromOverflow(t.usageTail)
	t.parser.pending = nil
	t.parser.allowBOM = false
	t.pendingBoundaryTail = nil
}

// appendUsageTail keeps the last responsesFailureTapOverflowTail bytes of the raw
// stream in t.usageTail for best-effort usage recovery from oversized events.
func (t *responsesFailureTap) appendUsageTail(p []byte) {
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
func (t *responsesFailureTap) rememberOverflowEvent(buf []byte) {
	if t == nil {
		return
	}
	updateResponsesTerminalEvent(&t.overflowEvent, t.overflowEvent.Type, buf)
}

func (t *responsesFailureTap) sniffUsageFromOverflow(buf []byte) {
	if t == nil {
		return
	}
	t.rememberOverflowEvent(buf)
	if t.overflowUsageRecorded || !isResponsesTerminalType(t.overflowEvent.Type) {
		return
	}
	usage := t.overflowEvent.Response.Usage
	if usage.InputTokens == 0 && usage.OutputTokens == 0 && usage.TotalTokens == 0 {
		return
	}
	observeResponsesUsage(t.ctx, usage)
	t.overflowUsageRecorded = true
}

func (t *responsesFailureTap) finishOverflowEvent() {
	if t == nil {
		return
	}
	t.rememberOverflowEvent(t.usageTail)
	// HTTP is a passthrough surface: an explicit, bounded SSE event name plus its
	// blank-line boundary is authoritative framing even when the JSON body is too
	// large for accounting inspection. Do not synthesize detailed provider status
	// from fragments; completed remains successful, while failure terminals retain
	// a conservative 502 plus best-effort usage.
	switch t.overflowEvent.Type {
	case "response.completed":
		t.terminalSeen = true
	case "response.failed", "response.incomplete", "error":
		t.terminalSeen = true
		observeResponseFailureStatus(t.ctx, http.StatusBadGateway)
	}
	t.overflowed = false
	t.overflowActive = false
	t.overflowEvent = responsesWebSocketStreamEvent{}
	t.overflowBoundaryTail = nil
	t.overflowUsageRecorded = false
	t.usageTail = nil
}

func (t *responsesFailureTap) consumeOverflowChunk(p []byte) []byte {
	if t == nil || !t.overflowActive || len(p) == 0 {
		return p
	}
	combined := append(append([]byte(nil), t.overflowBoundaryTail...), p...)
	boundaryEnd := responsesSSEBoundaryEnd(combined)
	if boundaryEnd == 0 {
		t.appendUsageTail(p)
		t.rememberOverflowEvent(t.usageTail)
		t.sniffUsageFromOverflow(t.usageTail)
		t.overflowBoundaryTail = trailingBytes(combined, 3)
		return nil
	}

	eventBytes := boundaryEnd - len(t.overflowBoundaryTail)
	if eventBytes < 0 {
		eventBytes = 0
	}
	if eventBytes > len(p) {
		eventBytes = len(p)
	}
	if eventBytes > 0 {
		t.appendUsageTail(p[:eventBytes])
	}
	t.rememberOverflowEvent(t.usageTail)
	t.sniffUsageFromOverflow(t.usageTail)
	t.finishOverflowEvent()
	return p[eventBytes:]
}

func responsesSSEBoundaryEnd(buf []byte) int {
	for i := 0; i < len(buf); i++ {
		if buf[i] != '\n' || i+1 >= len(buf) {
			continue
		}
		switch buf[i+1] {
		case '\n':
			return i + 2
		case '\r':
			if i+2 < len(buf) && buf[i+2] == '\n' {
				return i + 3
			}
		}
	}
	return 0
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

func (t *responsesFailureTap) maybeProcess(msg responsesSSEMessage) {
	eventName := strings.TrimSpace(msg.event)
	if eventName != "" &&
		eventName != "response.created" &&
		eventName != "response.completed" &&
		eventName != "response.output_item.done" &&
		eventName != "response.failed" &&
		eventName != "response.incomplete" &&
		eventName != "error" {
		return
	}

	event, err := parseResponsesStreamEvent(msg.data)
	if err != nil {
		return
	}

	eventType := strings.TrimSpace(event.Type)
	if eventName == "" {
		eventName = eventType
	} else if eventType == "" {
		event.Type = eventName
	}
	switch eventName {
	case "response.completed", "response.failed", "response.incomplete", "error":
		t.terminalSeen = true
	}
	if eventName == "response.completed" || eventName == "response.failed" || eventName == "response.incomplete" || eventName == "error" {
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
	// Oversized terminals become authoritative only after finishOverflowEvent sees
	// their explicit blank-line boundary and routes the retained event through the
	// normal terminal classifier.
	return t.terminalSeen
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
	if eventName != "response.failed" && eventName != "response.incomplete" && eventName != "error" {
		return
	}
	// The HTTP 200 was already committed before this post-commit failure event,
	// so the stats middleware would otherwise record the turn as a success.
	// Record an out-of-band failure status on the request summary so the
	// dashboard reflects the failure, mirroring how the websocket bridge maps the
	// same failure terminal into an errored turn. Classify the event so
	// rate limits (429) and overloads (503) keep their exact status rather than
	// all collapsing to bad-gateway.
	failureHeaders := responsesFailureHeaders(event, t.upstreamHeaders)
	failureStatus, _, _, _ := responsesWebSocketStreamFailureDetails(event, failureHeaders)
	if failureStatus == 0 {
		failureStatus = http.StatusBadGateway
	}
	observeResponseFailureStatus(t.ctx, failureStatus)

	fields := []logger.Field{
		logger.F("endpoint", "responses_stream_failure"),
		logger.F("event_type", eventName),
		logger.F("upstream_request_id", responsesUpstreamRequestID(failureHeaders)),
	}
	switch eventName {
	case "response.failed", "error":
		streamErr := responsesStreamEventError(event)
		fields = append(fields,
			logger.F("error_code", strings.TrimSpace(streamErr.Code)),
			logger.F("error_type", strings.TrimSpace(streamErr.Type)),
			logger.F("error_message", truncateResponsesFailureLogMessage(streamErr.Message)),
		)
	case "response.incomplete":
		fields = append(fields,
			logger.F("reason", strings.TrimSpace(event.Response.IncompleteDetails.Reason)),
		)
	}
	t.h.log.Info("responses stream reported failure after commit", fields...)
}
