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
	responsesPrecommitPeekTimeout          = 750 * time.Millisecond
	responsesPrecommitMaxPeekBytes         = 64 * 1024
	responsesPrecommitHeldPreambleMaxBytes = 512 * 1024
	responsesPrecommitQuotaHoldTimeout     = 10 * time.Second
	responsesPeekReadChunkSize             = 4 * 1024
	responsesPeekCancellationGrace         = 10 * time.Millisecond
	responsesFailureTapMaxBuffer           = openAIStreamScannerMaxBuffer
	responsesFailureTapOverflowTail        = 64 * 1024
	responsesFailureLogMessageLimit        = 256
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
	eventOutputProgress bool
	eventUsageProgress  bool
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
	pending           []byte
	allowBOM          bool
	done              bool
	sawUnsafeProgress bool
}

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
		o.rememberOverflowEvent(o.usageTail)
		if isResponsesTerminalType(o.overflowEvent.Type) {
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
	case "response.completed", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
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
	merged := make(map[string]json.RawMessage, len(root))
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

	headers := resp.Header.Clone()
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		runResponsesPeekPump(upstreamBody, pw, headers, peekDone, peekState, commitCh, abortCh, observeOnlyCh, maxPeekBytes)
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
	if hasResult && result.failure != nil {
		h.observeCopilotResponseFailure(resp.Request, *result.failure, responsesFailureHeaders(*result.failure, resp.Header))
	}
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
	prepared.stopTerminalObservation()
	resp = prepared.commitResponse()
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(http.StatusOK)
	var store *ToolExecutionContextStore
	if h != nil {
		store = h.toolContexts
	}
	streamCtx := r.Context()
	if resp.Request != nil {
		streamCtx = context.WithValue(streamCtx, responsesUpstreamRequestContextKey{}, resp.Request)
	}
	streamResponsesPipeWithFailureLog(streamCtx, h, w, resp.Body, resp.Header, store, toolScope, lifecycleHooks...)
}

func prepareResponsesStreamAttempt(waitCtx, streamCtx context.Context, request func() (*http.Response, error)) (*http.Response, *peekResult, http.Header, error) {
	return prepareResponsesStreamAttemptWithGrace(nil, waitCtx, streamCtx, responsesPeekCancellationGrace, request)
}

func prepareResponsesStreamAttemptWithGrace(h *ProxyHandler, waitCtx, streamCtx context.Context, cancellationGrace time.Duration, request func() (*http.Response, error)) (*http.Response, *peekResult, http.Header, error) {
	resp, err := request()
	if err != nil || resp == nil || resp.StatusCode != http.StatusOK {
		return resp, nil, nil, err
	}

	prepared := newResponsesPreparedStream(resp, responsesPrecommitMaxPeekBytes, true)
	result, hasResult, awaitSource, err := prepared.await(waitCtx, streamCtx, responsesPrecommitPeekTimeout)
	if hasResult && result.failure != nil {
		h.observeCopilotResponseFailure(resp.Request, *result.failure, responsesFailureHeaders(*result.failure, resp.Header))
	}
	if err != nil {
		terminal, hasTerminal, outcome, hasOutcome := prepared.awaitCancellationResolution(cancellationGrace)
		if hasTerminal {
			if terminal.failure != nil {
				h.observeCopilotResponseFailure(resp.Request, *terminal.failure, responsesFailureHeaders(*terminal.failure, resp.Header))
			}
			terminal.decision = responsesPeekDecisionPassthrough
			if awaitSource == responsesPreparedAwaitInbound {
				prepared.abort()
				return nil, &terminal, resp.Header.Clone(), nil
			}
			return prepared.commitResponse(), &terminal, nil, nil
		}
		if hasOutcome && isIndependentResponsesPeekOutcome(outcome) {
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
	resp, result, translatedHeaders, err := prepareResponsesStreamAttemptWithGrace(h, waitCtx, streamCtx, cancellationGrace, request)
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

func responsesOutputHasProgress(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var items []json.RawMessage
	if err := json.Unmarshal(bytes.TrimSpace(raw), &items); err != nil {
		return true
	}
	return len(items) > 0
}

func inspectResponsesPeekMessages(parser *responsesSSEParser, headers http.Header, peekState *responsesPeekState, preferTranslatedFailure bool) (peekResult, bool, bool) {
	result := peekResult{decision: responsesPeekDecisionPassthrough}
	sawSemantic := false
	sawBeyondPreamble := false
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
		unsafeBeforeMessage := parser.sawUnsafeProgress
		if classified.failure != nil && !unsafeBeforeMessage && !classified.eventOutputProgress && !classified.eventUsageProgress {
			classified.precommitReplaySafe = routeResponseBodyAllowsReplay([]byte(msg.data))
		}
		if classified.eventOutputProgress || classified.eventUsageProgress {
			parser.sawUnsafeProgress = true
		}
		if classified.eventOutputProgress {
			classified.decision = responsesPeekDecisionPassthrough
		}
		peekState.publishTerminal(classified)
		preserveUnsafeStream := unsafeBeforeMessage && classified.decision == responsesPeekDecisionTranslate
		if (!sawSemantic && !preserveUnsafeStream) || (preferTranslatedFailure && !unsafeBeforeMessage && !classified.eventOutputProgress && classified.decision == responsesPeekDecisionTranslate) {
			result = classified
		}
		if !classified.preamble {
			sawBeyondPreamble = true
			if classified.terminal == nil || classified.failure == nil {
				parser.sawUnsafeProgress = true
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
	if rejectDuplicateJSONMappingKeys([]byte(msg.data)) != nil {
		return peekResult{decision: responsesPeekDecisionPassthrough}
	}
	event, err := parseResponsesStreamEvent(msg.data)
	if err != nil {
		return peekResult{decision: responsesPeekDecisionPassthrough}
	}
	return classifyResponsesPeekEvent(event, msg.event, headers)
}

func classifyResponsesPeekEvent(event responsesWebSocketStreamEvent, eventName string, headers http.Header) peekResult {
	result := peekResult{
		decision:            responsesPeekDecisionPassthrough,
		eventOutputProgress: responsesOutputHasProgress(event.Response.Output),
		eventUsageProgress:  !event.Response.Usage.isZero(),
	}
	eventName = strings.TrimSpace(eventName)
	terminalType := strings.TrimSpace(event.Type)
	if terminalType == "" {
		terminalType = eventName
	}
	event.Type = terminalType
	result.preamble = terminalType == "response.queued" || terminalType == "response.created" || terminalType == "response.in_progress"
	switch terminalType {
	case "response.completed", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
		terminal := event
		result.terminal = &terminal
		if terminalType == "response.failed" || terminalType == "error" {
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
	firstNonZero := -1
	for i := 0; i < len(value); i++ {
		if value[i] < '0' || value[i] > '9' {
			return "", false
		}
		if firstNonZero == -1 && value[i] != '0' {
			firstNonZero = i
		}
	}
	if firstNonZero == -1 {
		return "", false
	}
	return value[firstNonZero:], true
}

func incrementPositiveDecimal(value string) string {
	carryIndex := len(value) - 1
	for carryIndex >= 0 && value[carryIndex] == '9' {
		carryIndex--
	}

	var incremented strings.Builder
	if carryIndex < 0 {
		incremented.Grow(len(value) + 1)
		incremented.WriteByte('1')
		for range len(value) {
			incremented.WriteByte('0')
		}
		return incremented.String()
	}

	incremented.Grow(len(value))
	incremented.WriteString(value[:carryIndex])
	incremented.WriteByte(value[carryIndex] + 1)
	for i := carryIndex + 1; i < len(value); i++ {
		incremented.WriteByte('0')
	}
	return incremented.String()
}

func decimalMillisecondsToRetryAfterSeconds(value string) (string, bool) {
	normalized, ok := normalizePositiveDecimal(value)
	if !ok {
		return "", false
	}
	if len(normalized) <= 3 {
		return "1", true
	}

	secondsEnd := len(normalized) - 3
	seconds := normalized[:secondsEnd]
	if normalized[secondsEnd:] == "000" {
		return seconds, true
	}
	return incrementPositiveDecimal(seconds), true
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

func responsesQuotaResetRetryAfter(headers http.Header, dimension string) (string, bool) {
	value := strings.TrimSpace(headerGetCI(headers, "x-ratelimit-reset-"+dimension))
	if normalized, ok := normalizePositiveDecimal(value); ok {
		return normalized, true
	}
	delay, err := time.ParseDuration(value)
	if err != nil || delay <= 0 {
		return "", false
	}
	seconds := durationSecondsCeil(delay)
	if seconds <= 0 {
		return "", false
	}
	return strconv.FormatInt(seconds, 10), true
}

func positiveDecimalGreater(left, right string) bool {
	if len(left) != len(right) {
		return len(left) > len(right)
	}
	return left > right
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
	case "429", "too_many_requests", "rate_limit_exceeded", "rate_limit_error", "quota_exceeded",
		"user_model_rate_limited", "user_global_rate_limited", "user_weekly_rate_limited", "integration_rate_limited":
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

	if seconds, ok := decimalMillisecondsToRetryAfterSeconds(headerGetCI(headers, "retry-after-ms")); ok {
		return seconds, "retry-after-ms"
	}

	if seconds, ok := retryAfterHeaderSeconds(headerGetCI(headers, "Retry-After")); ok {
		return seconds, "Retry-After"
	}

	resetValue := ""
	resetSource := ""
	for _, dimension := range []string{"tokens", "requests"} {
		remaining, exhausted := responsesQuotaRemaining(headers, dimension)
		if !exhausted || remaining == -1 {
			continue
		}
		value, ok := responsesQuotaResetRetryAfter(headers, dimension)
		if !ok || (resetValue != "" && !positiveDecimalGreater(value, resetValue)) {
			continue
		}
		resetValue = value
		resetSource = "x-ratelimit-reset-" + dimension
	}
	if resetValue != "" {
		return resetValue, resetSource
	}

	return "", ""
}

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
		if code := providerRequestErrorCode(err); strings.HasPrefix(code, "conversation_") {
			observeResponseFailureStatus(ctx, upstreamStatusCode(err, http.StatusBadGateway))
			if ctx.Err() == nil && !isClientWriteError(fw, err) {
				writeResponsesFailedEvent(fw, code, err.Error())
			}
			return
		}
		if _, _, ok := durableStateFailureDetails(err); ok {
			observeResponseFailureStatus(ctx, http.StatusServiceUnavailable)
			if ctx.Err() == nil && !isClientWriteError(fw, err) {
				writeDurableStateStreamFailure(fw, err)
			}
			return
		}
		if tap.completedCleanly() {
			if ctx.Err() == nil && !isClientWriteError(fw, err) {
				tap.writeFailedAfterError(fw)
			}
			return
		}
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
	if ctx.Err() == nil {
		tap.writeFailedAfterError(fw)
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

func writeResponsesFailedEvent(w io.Writer, code, message string) {
	data, _ := json.Marshal(map[string]any{"type": "response.failed", "response": map[string]any{
		"object": "response", "status": "failed", "output": []any{},
		"error": map[string]string{"code": code, "message": message},
	}})
	_, _ = io.WriteString(w, "event: response.failed\ndata: "+string(data)+"\n\n")
}

func (t *responsesFailureTap) writeFailedAfterError(w io.Writer) {
	if t == nil || t.terminalError == nil {
		return
	}
	code, message := responsesFailedEventError(*t.terminalError)
	t.terminalError = nil
	writeResponsesFailedEvent(w, code, message)
}

func responsesFailedEventError(streamErr responsesWebSocketStreamError) (string, string) {
	code, message := strings.TrimSpace(streamErr.Code), strings.TrimSpace(streamErr.Message)
	if !strings.EqualFold(code, "content_filter") || !strings.Contains(strings.ToLower(message), "cybersecurity risk") {
		return code, message
	}
	if i := strings.Index(message, `{"error":`); i >= 0 {
		var wrapped struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.NewDecoder(strings.NewReader(message[i:])).Decode(&wrapped) == nil {
			if inner := strings.TrimSpace(wrapped.Error.Message); inner != "" {
				message = inner
			}
		}
	}
	return "cyber_policy", message
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
	for _, name := range []string{"X-Copilot-Service-Request-Id", "X-Request-Id", "X-Azure-Request-Id", "Openai-Request-Id"} {
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

type responsesUpstreamRequestContextKey struct{}

type responsesFailureTap struct {
	h                     *ProxyHandler
	upstreamHeaders       http.Header
	ctx                   context.Context
	store                 *ToolExecutionContextStore
	scope                 string
	responseScope         string
	parser                responsesSSEParser
	pendingBoundaryTail   []byte
	overflowed            bool
	overflowActive        bool
	overflowEvent         responsesWebSocketStreamEvent
	overflowBoundaryTail  []byte
	overflowUsageRecorded bool
	usageTail             []byte
	terminalSeen          bool
	terminalError         *responsesWebSocketStreamError
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
	if t == nil || (t.terminalSeen && t.terminalError == nil) || t.overflowActive {
		return
	}
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
	t.overflowed = true
	t.overflowActive = true
	t.rememberOverflowEvent(t.parser.pending)
	t.overflowBoundaryTail = trailingBytes(t.parser.pending, 3)
	t.sniffUsageFromOverflow(t.usageTail)
	t.parser.pending = nil
	t.parser.allowBOM = false
	t.pendingBoundaryTail = nil
}

func (t *responsesFailureTap) appendUsageTail(p []byte) {
	t.usageTail = append(t.usageTail, p...)
	if len(t.usageTail) > responsesFailureTapOverflowTail {
		t.usageTail = t.usageTail[len(t.usageTail)-responsesFailureTapOverflowTail:]
	}
}

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
	switch t.overflowEvent.Type {
	case "response.completed", "response.cancelled", "response.canceled":
		t.terminalSeen = true
		t.terminalError = nil
	case "response.failed", "response.incomplete", "error":
		t.terminalSeen = true
		t.terminalError = nil
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
		eventName != "response.cancelled" &&
		eventName != "response.canceled" &&
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
	if isResponsesTerminalType(eventName) {
		t.terminalSeen = true
		t.terminalError = nil
		if eventName == "error" {
			streamErr := responsesStreamEventError(event)
			t.terminalError = &streamErr
		}
		observeResponsesUsage(t.ctx, event.Response.Usage)
	}
	t.maybeCaptureToolCommand(eventName, event)
	t.maybeLog(eventName, event)
}

func (t *responsesFailureTap) completedCleanly() bool {
	if t == nil {
		return true
	}
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
	failureHeaders := responsesFailureHeaders(event, t.upstreamHeaders)
	if upstreamRequest, _ := t.ctx.Value(responsesUpstreamRequestContextKey{}).(*http.Request); upstreamRequest != nil {
		t.h.observeCopilotResponseFailure(upstreamRequest, event, failureHeaders)
	}
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
