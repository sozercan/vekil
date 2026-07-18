package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/sozercan/vekil/models"
)

func intVal(i int) *int { return &i }

type lifecycleAwareReadCloser struct {
	io.ReadCloser
	ctx                   context.Context
	cancellationAtFailure atomic.Bool
}

func newLifecycleAwareReadCloser(body io.ReadCloser, ctx context.Context) *lifecycleAwareReadCloser {
	return &lifecycleAwareReadCloser{ReadCloser: body, ctx: ctx}
}

func (r *lifecycleAwareReadCloser) Read(p []byte) (int, error) {
	n, err := r.ReadCloser.Read(p)
	// HTTP/2 response bodies may collapse a request cancel cause to plain
	// context.Canceled. Consult the request context only for that exact class of
	// read failure; EOF, resets, and deadlines remain provider failures even if
	// shutdown races immediately afterward.
	if err != nil && errors.Is(err, context.Canceled) && r.ctx != nil && errors.Is(context.Cause(r.ctx), errProxyLifecycleShutdown) {
		r.cancellationAtFailure.Store(true)
	}
	return n, err
}

func (r *lifecycleAwareReadCloser) canceledAtFailure() bool {
	return r != nil && r.cancellationAtFailure.Load()
}

type streamLifecycleHooks struct {
	transportCanceled      func() bool
	suppressStats          func()
	writePrecommitShutdown func()
}

func (h streamLifecycleHooks) suppressTransportCancellation(committed bool) bool {
	if h.transportCanceled == nil || !h.transportCanceled() {
		return false
	}
	h.suppressKnownTransportCancellation(committed)
	return true
}

func (h streamLifecycleHooks) suppressKnownTransportCancellation(committed bool) {
	if committed {
		if h.suppressStats != nil {
			h.suppressStats()
		}
	} else if h.writePrecommitShutdown != nil {
		h.writePrecommitShutdown()
	} else if h.suppressStats != nil {
		h.suppressStats()
	}
}

type commitTrackingResponseWriter struct {
	http.ResponseWriter
	committed bool
}

func (w *commitTrackingResponseWriter) WriteHeader(status int) {
	w.committed = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *commitTrackingResponseWriter) Write(p []byte) (int, error) {
	w.committed = true
	return w.ResponseWriter.Write(p)
}

func (w *commitTrackingResponseWriter) Flush() {
	w.committed = true
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

// bufPool reduces GC pressure by reusing bytes.Buffer instances for JSON encoding.
var bufPool = sync.Pool{
	New: func() interface{} { return new(bytes.Buffer) },
}

func writeSSEEvent(w http.ResponseWriter, eventType string, data interface{}) error {
	buf := bufPool.Get().(*bytes.Buffer)
	buf.Reset()
	defer bufPool.Put(buf)

	enc := json.NewEncoder(buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(data); err != nil {
		return err
	}
	// Encode adds a trailing newline; trim it for SSE format
	b := bytes.TrimRight(buf.Bytes(), "\n")
	_, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, b)
	if err != nil {
		return err
	}
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
	return nil
}

func parseSSELine(line string) (string, bool) {
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	data := strings.TrimPrefix(line, "data:")
	data = strings.TrimPrefix(data, " ")
	return data, true
}

func setSSEHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
}

type explicitRouteStreamProtocol uint8

const (
	explicitRouteStreamNone explicitRouteStreamProtocol = iota
	explicitRouteStreamOpenAIChat
	explicitRouteStreamAnthropic
)

type explicitRouteStreamFailure struct {
	statusCode int
	errType    string
	code       string
	message    string
}

func (f *explicitRouteStreamFailure) Error() string {
	if f == nil {
		return ""
	}
	if strings.TrimSpace(f.message) != "" {
		return f.message
	}
	if strings.TrimSpace(f.code) != "" {
		return f.code
	}
	if strings.TrimSpace(f.errType) != "" {
		return f.errType
	}
	return "upstream stream error"
}

func (f *explicitRouteStreamFailure) asUpstreamError(headers http.Header) error {
	if f == nil {
		return nil
	}
	status := f.statusCode
	if status == 0 {
		status = http.StatusBadGateway
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"message": f.Error(),
			"type":    strings.TrimSpace(f.errType),
			"code":    strings.TrimSpace(f.code),
		},
	})
	return &upstreamError{statusCode: status, body: body, headers: headers.Clone()}
}

type explicitRouteStreamInspection struct {
	chunk           *models.OpenAIStreamChunk
	progress        upstreamSemanticProgress
	failure         *explicitRouteStreamFailure
	terminalSuccess bool
}

func mergeUpstreamSemanticProgress(current, next upstreamSemanticProgress) upstreamSemanticProgress {
	if next == "" || next == upstreamProgressNone {
		return current
	}
	if current == upstreamProgressUnknown || next == upstreamProgressUnknown {
		return upstreamProgressUnknown
	}
	if current == upstreamProgressToolActivity || next == upstreamProgressToolActivity {
		return upstreamProgressToolActivity
	}
	if current == upstreamProgressSemanticOutput || next == upstreamProgressSemanticOutput {
		return upstreamProgressSemanticOutput
	}
	if current == upstreamProgressTerminalSuccess || next == upstreamProgressTerminalSuccess {
		return upstreamProgressTerminalSuccess
	}
	if current == upstreamProgressTerminalFailure || next == upstreamProgressTerminalFailure {
		return upstreamProgressTerminalFailure
	}
	if current == upstreamProgressAllowedPreamble || next == upstreamProgressAllowedPreamble {
		return upstreamProgressAllowedPreamble
	}
	return next
}

func upstreamProgressAllowsTargetSwitch(progress upstreamSemanticProgress) bool {
	return progress == "" || progress == upstreamProgressNone || progress == upstreamProgressAllowedPreamble
}

func inspectOpenAIChatStreamEvent(eventType, data string) explicitRouteStreamInspection {
	data = strings.TrimSpace(data)
	if data == "[DONE]" {
		return explicitRouteStreamInspection{progress: upstreamProgressTerminalSuccess, terminalSuccess: true}
	}
	if streamErr, ok := parseOpenAIStreamError(eventType, data); ok {
		failureProgress := upstreamProgressNone
		var envelope map[string]json.RawMessage
		if json.Unmarshal([]byte(data), &envelope) != nil || envelope == nil {
			failureProgress = upstreamProgressUnknown
		} else {
			for key, value := range envelope {
				switch key {
				case "error", "request_id", "request-id":
				default:
					if !rawJSONIsNullOrEmpty(value) {
						failureProgress = upstreamProgressUnknown
					}
				}
			}
		}
		return explicitRouteStreamInspection{failure: &explicitRouteStreamFailure{
			statusCode: streamErr.httpStatus(),
			errType:    streamErr.Type,
			code:       streamErr.Code,
			message:    streamErr.Error(),
		}, progress: failureProgress}
	}
	if data == "" {
		return explicitRouteStreamInspection{progress: upstreamProgressAllowedPreamble}
	}

	trimmedEvent := strings.ToLower(strings.TrimSpace(eventType))
	if trimmedEvent != "" && trimmedEvent != "message" && trimmedEvent != "completion" && trimmedEvent != "chat.completion.chunk" {
		return explicitRouteStreamInspection{progress: upstreamProgressUnknown}
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &raw); err != nil || raw == nil {
		return explicitRouteStreamInspection{progress: upstreamProgressUnknown}
	}
	var chunk models.OpenAIStreamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return explicitRouteStreamInspection{progress: upstreamProgressUnknown}
	}

	progress := classifyOpenAIChatChunkProgress(raw)
	return explicitRouteStreamInspection{chunk: &chunk, progress: progress}
}

func classifyOpenAIChatChunkProgress(raw map[string]json.RawMessage) upstreamSemanticProgress {
	if raw == nil {
		return upstreamProgressUnknown
	}
	knownTopLevel := map[string]struct{}{
		"id": {}, "object": {}, "created": {}, "model": {}, "choices": {},
		"system_fingerprint": {}, "service_tier": {}, "usage": {},
	}
	for key, value := range raw {
		if _, known := knownTopLevel[key]; !known && !rawJSONIsNullOrEmpty(value) {
			return upstreamProgressUnknown
		}
	}
	if usage, ok := raw["usage"]; ok && !rawJSONIsNullOrEmpty(usage) {
		// A usage frame proves the attempt reached provider-side accounting. Even an
		// otherwise empty usage-only chunk is therefore beyond a replay-safe preamble.
		return upstreamProgressTerminalSuccess
	}

	choicesRaw, hasChoices := raw["choices"]
	if !hasChoices || rawJSONIsNullOrEmpty(choicesRaw) {
		return upstreamProgressAllowedPreamble
	}
	var choices []json.RawMessage
	if err := json.Unmarshal(choicesRaw, &choices); err != nil {
		return upstreamProgressUnknown
	}
	if len(choices) == 0 {
		return upstreamProgressAllowedPreamble
	}

	progress := upstreamProgressAllowedPreamble
	for _, choiceRaw := range choices {
		var choice map[string]json.RawMessage
		if err := json.Unmarshal(choiceRaw, &choice); err != nil || choice == nil {
			return upstreamProgressUnknown
		}
		if finish, ok := choice["finish_reason"]; ok && !rawJSONIsNullOrEmpty(finish) {
			progress = mergeUpstreamSemanticProgress(progress, upstreamProgressTerminalSuccess)
		}
		deltaRaw, ok := choice["delta"]
		if !ok || rawJSONIsNullOrEmpty(deltaRaw) {
			continue
		}
		var delta map[string]json.RawMessage
		if err := json.Unmarshal(deltaRaw, &delta); err != nil || delta == nil {
			return upstreamProgressUnknown
		}
		deltaProgress := classifyOpenAIChatDeltaProgress(delta)
		progress = mergeUpstreamSemanticProgress(progress, deltaProgress)
	}
	return progress
}

func classifyOpenAIChatDeltaProgress(delta map[string]json.RawMessage) upstreamSemanticProgress {
	if delta == nil {
		return upstreamProgressUnknown
	}
	progress := upstreamProgressAllowedPreamble
	known := map[string]struct{}{
		"role": {}, "content": {}, "tool_calls": {}, "function_call": {},
		"reasoning": {}, "reasoning_content": {}, "reasoning_text": {},
		"refusal": {}, "audio": {},
	}
	for key, value := range delta {
		switch key {
		case "role":
			// Role-only chunks are the OpenAI chat preamble equivalent.
		case "tool_calls", "function_call":
			if !rawJSONIsNullOrEmpty(value) {
				progress = mergeUpstreamSemanticProgress(progress, upstreamProgressToolActivity)
			}
		case "content", "reasoning", "reasoning_content", "reasoning_text", "refusal", "audio":
			if rawJSONHasSemanticValue(value) {
				progress = mergeUpstreamSemanticProgress(progress, upstreamProgressSemanticOutput)
			}
		default:
			if _, ok := known[key]; !ok && !rawJSONIsNullOrEmpty(value) {
				return upstreamProgressUnknown
			}
		}
	}
	return progress
}

func rawJSONIsNullOrEmpty(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func rawJSONHasSemanticValue(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte(`""`)) {
		return false
	}
	var text string
	if json.Unmarshal(trimmed, &text) == nil {
		return text != ""
	}
	return true
}

func inspectAnthropicStreamEvent(eventType, data string) explicitRouteStreamInspection {
	data = strings.TrimSpace(data)
	if data == "" {
		return explicitRouteStreamInspection{progress: upstreamProgressAllowedPreamble}
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &payload); err != nil || payload == nil {
		return explicitRouteStreamInspection{progress: upstreamProgressUnknown}
	}
	typeName := rawJSONString(payload["type"])
	if typeName == "" {
		typeName = strings.TrimSpace(eventType)
	}
	switch strings.ToLower(typeName) {
	case "ping", "message_start":
		return explicitRouteStreamInspection{progress: upstreamProgressAllowedPreamble}
	case "content_block_start":
		var block map[string]json.RawMessage
		if err := json.Unmarshal(payload["content_block"], &block); err != nil || block == nil {
			return explicitRouteStreamInspection{progress: upstreamProgressUnknown}
		}
		switch strings.ToLower(rawJSONString(block["type"])) {
		case "tool_use", "server_tool_use", "web_search_tool_result":
			return explicitRouteStreamInspection{progress: upstreamProgressToolActivity}
		case "text", "thinking", "redacted_thinking":
			return explicitRouteStreamInspection{progress: upstreamProgressSemanticOutput}
		default:
			return explicitRouteStreamInspection{progress: upstreamProgressUnknown}
		}
	case "content_block_delta":
		var delta map[string]json.RawMessage
		if err := json.Unmarshal(payload["delta"], &delta); err != nil || delta == nil {
			return explicitRouteStreamInspection{progress: upstreamProgressUnknown}
		}
		switch strings.ToLower(rawJSONString(delta["type"])) {
		case "input_json_delta":
			return explicitRouteStreamInspection{progress: upstreamProgressToolActivity}
		case "text_delta", "thinking_delta", "signature_delta", "citations_delta":
			return explicitRouteStreamInspection{progress: upstreamProgressSemanticOutput}
		default:
			return explicitRouteStreamInspection{progress: upstreamProgressUnknown}
		}
	case "content_block_stop", "message_delta", "message_stop":
		return explicitRouteStreamInspection{progress: upstreamProgressTerminalSuccess, terminalSuccess: true}
	case "error":
		status, ok := anthropicStreamErrorStatus([]byte(data))
		if !ok {
			status = http.StatusBadGateway
		}
		var envelope struct {
			Error struct {
				Type    string `json:"type"`
				Code    string `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal([]byte(data), &envelope)
		code := strings.TrimSpace(envelope.Error.Code)
		if code == "" {
			code = strings.TrimSpace(envelope.Error.Type)
		}
		return explicitRouteStreamInspection{failure: &explicitRouteStreamFailure{
			statusCode: status,
			errType:    strings.TrimSpace(envelope.Error.Type),
			code:       code,
			message:    strings.TrimSpace(envelope.Error.Message),
		}}
	default:
		return explicitRouteStreamInspection{progress: upstreamProgressUnknown}
	}
}

type explicitRoutePreparedStream struct {
	resp         *http.Response
	upstreamBody io.ReadCloser
	reader       *io.PipeReader
	resultCh     chan explicitRouteStreamInspection
	commitCh     chan struct{}
	abortCh      chan struct{}
	doneCh       chan struct{}
	commitOnce   sync.Once
	abortOnce    sync.Once
}

type explicitRoutePreparedBody struct {
	reader    *io.PipeReader
	abort     func()
	closeOnce sync.Once
	closeErr  error
}

func (b *explicitRoutePreparedBody) Read(p []byte) (int, error) {
	return b.reader.Read(p)
}

func (b *explicitRoutePreparedBody) Close() error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		if b.reader != nil {
			b.closeErr = b.reader.CloseWithError(context.Canceled)
		}
		if b.abort != nil {
			b.abort()
		}
	})
	return b.closeErr
}

func newExplicitRoutePreparedStream(resp *http.Response, protocol explicitRouteStreamProtocol, maxPeekBytes int) *explicitRoutePreparedStream {
	if resp == nil || resp.Body == nil {
		return nil
	}
	if maxPeekBytes <= 0 {
		maxPeekBytes = responsesPrecommitMaxPeekBytes
	}
	upstreamBody := resp.Body
	pr, pw := io.Pipe()
	prepared := &explicitRoutePreparedStream{
		resp:         resp,
		upstreamBody: upstreamBody,
		reader:       pr,
		resultCh:     make(chan explicitRouteStreamInspection, 1),
		commitCh:     make(chan struct{}),
		abortCh:      make(chan struct{}),
		doneCh:       make(chan struct{}),
	}
	go func() {
		defer close(prepared.doneCh)
		runExplicitRouteStreamPeekPump(upstreamBody, pw, protocol, prepared.resultCh, prepared.commitCh, prepared.abortCh, maxPeekBytes)
	}()
	return prepared
}

func (s *explicitRoutePreparedStream) await(waitCtx, upstreamCtx context.Context, timeout time.Duration) (explicitRouteStreamInspection, bool, error) {
	if s == nil {
		return explicitRouteStreamInspection{}, false, fmt.Errorf("prepared stream is unavailable")
	}
	if timeout <= 0 {
		timeout = responsesPrecommitPeekTimeout
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	var waitDone <-chan struct{}
	if waitCtx != nil {
		waitDone = waitCtx.Done()
	}
	var upstreamDone <-chan struct{}
	if upstreamCtx != nil && upstreamCtx != waitCtx {
		upstreamDone = upstreamCtx.Done()
	}
	select {
	case result := <-s.resultCh:
		return result, true, nil
	case <-timer.C:
		return explicitRouteStreamInspection{}, false, nil
	case <-waitDone:
		return explicitRouteStreamInspection{}, false, waitCtx.Err()
	case <-upstreamDone:
		return explicitRouteStreamInspection{}, false, upstreamCtx.Err()
	}
}

func (s *explicitRoutePreparedStream) commitResponse() *http.Response {
	if s == nil || s.resp == nil {
		return nil
	}
	s.commitOnce.Do(func() { close(s.commitCh) })
	s.resp.Body = &explicitRoutePreparedBody{reader: s.reader, abort: s.abort}
	return s.resp
}

func (s *explicitRoutePreparedStream) abort() {
	if s == nil {
		return
	}
	s.abortOnce.Do(func() {
		close(s.abortCh)
		if s.upstreamBody != nil {
			go func() { _ = s.upstreamBody.Close() }()
		}
	})
}

func (s *explicitRoutePreparedStream) abortAndWait(timeout time.Duration) bool {
	if s == nil {
		return true
	}
	s.abort()
	if s.doneCh == nil {
		return true
	}
	if timeout <= 0 {
		<-s.doneCh
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.doneCh:
		return true
	case <-timer.C:
		return false
	}
}

func isExplicitRoutePreparedChatResponse(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	_, ok := resp.Body.(*explicitRoutePreparedBody)
	return ok
}

type explicitRouteStreamLine struct {
	line string
	err  error
}

func readExplicitRouteStreamLines(body io.ReadCloser, out chan<- explicitRouteStreamLine, abortCh <-chan struct{}) {
	defer close(out)
	defer func() { _ = body.Close() }()
	reader := bufio.NewReaderSize(body, openAIStreamScannerInitialBuffer)
	for {
		line, err := readOpenAISSELine(reader)
		result := explicitRouteStreamLine{line: line, err: err}
		select {
		case out <- result:
		case <-abortCh:
			return
		}
		if err != nil {
			return
		}
	}
}

func runExplicitRouteStreamPeekPump(body io.ReadCloser, pw *io.PipeWriter, protocol explicitRouteStreamProtocol, resultCh chan<- explicitRouteStreamInspection, commitCh, abortCh <-chan struct{}, maxPeekBytes int) {
	lineCh := make(chan explicitRouteStreamLine, 1)
	go readExplicitRouteStreamLines(body, lineCh, abortCh)

	var prefix bytes.Buffer
	var accumulator sseDataAccumulator
	progress := upstreamProgressNone
	decisionSent := false

	sendResult := func(result explicitRouteStreamInspection) {
		if decisionSent {
			return
		}
		if result.progress == "" {
			result.progress = progress
		}
		decisionSent = true
		select {
		case resultCh <- result:
		default:
		}
	}
	writeCommitted := func() {
		if prefix.Len() > 0 {
			if _, err := pw.Write(prefix.Bytes()); err != nil {
				_ = pw.CloseWithError(err)
				return
			}
		}
		for result := range lineCh {
			if len(result.line) > 0 {
				if _, err := io.WriteString(pw, result.line); err != nil {
					_ = pw.CloseWithError(err)
					return
				}
			}
			if result.err != nil {
				if result.err == io.EOF {
					_ = pw.Close()
				} else {
					_ = pw.CloseWithError(result.err)
				}
				return
			}
		}
		_ = pw.Close()
	}
	abortAndDrain := func() {
		for range lineCh {
		}
		_ = pw.CloseWithError(context.Canceled)
	}
	waitForAction := func() {
		select {
		case <-abortCh:
			abortAndDrain()
		case <-commitCh:
			writeCommitted()
		}
	}
	inspect := func(eventType, data string) bool {
		var result explicitRouteStreamInspection
		switch protocol {
		case explicitRouteStreamAnthropic:
			result = inspectAnthropicStreamEvent(eventType, data)
		default:
			result = inspectOpenAIChatStreamEvent(eventType, data)
		}
		if result.failure != nil {
			result.progress = mergeUpstreamSemanticProgress(progress, result.progress)
			sendResult(result)
			return false
		}
		progress = mergeUpstreamSemanticProgress(progress, result.progress)
		if result.terminalSuccess || !upstreamProgressAllowsTargetSwitch(progress) {
			result.progress = progress
			sendResult(result)
			return false
		}
		return true
	}

	for {
		select {
		case <-abortCh:
			abortAndDrain()
			return
		case <-commitCh:
			writeCommitted()
			return
		case result, ok := <-lineCh:
			if !ok {
				sendResult(explicitRouteStreamInspection{progress: progress})
				waitForAction()
				return
			}
			line, readErr := result.line, result.err
			if len(line) > 0 {
				_, _ = prefix.WriteString(line)
				if strings.TrimRight(line, "\r\n") == "" && accumulator.eventType != "" && len(accumulator.dataLines) == 0 {
					progress = upstreamProgressUnknown
					sendResult(explicitRouteStreamInspection{progress: progress})
					waitForAction()
					return
				}
				if errors.Is(readErr, errOpenAISSELineTooLong) {
					progress = upstreamProgressUnknown
					sendResult(explicitRouteStreamInspection{progress: progress})
					waitForAction()
					return
				}
				if !accumulator.consumeLine(line, inspect) {
					waitForAction()
					return
				}
			}
			if prefix.Len() >= maxPeekBytes {
				sendResult(explicitRouteStreamInspection{progress: progress})
				waitForAction()
				return
			}
			if readErr == nil {
				continue
			}
			if readErr == io.EOF {
				_ = accumulator.dispatch(inspect)
			}
			sendResult(explicitRouteStreamInspection{progress: progress})
			waitForAction()
			return
		}
	}
}

func consumeOpenAIStreamChunksWithProgress(r io.Reader, onChunk func(models.OpenAIStreamChunk) bool) (bool, upstreamSemanticProgress, error) {
	reader := bufio.NewReaderSize(r, openAIStreamScannerInitialBuffer)
	progress := upstreamProgressNone
	sawDone := false
	var streamReadErr error
	var accumulator sseDataAccumulator
	processData := func(eventType, data string) bool {
		result := inspectOpenAIChatStreamEvent(eventType, data)
		if result.failure != nil {
			progress = mergeUpstreamSemanticProgress(progress, result.progress)
			streamReadErr = &openAIStreamError{Type: result.failure.errType, Code: result.failure.code, Message: result.failure.Error()}
			return false
		}
		progress = mergeUpstreamSemanticProgress(progress, result.progress)
		if result.terminalSuccess && strings.TrimSpace(data) == "[DONE]" {
			sawDone = true
			return false
		}
		if result.chunk == nil {
			return true
		}
		return onChunk == nil || onChunk(*result.chunk)
	}

	for {
		line, err := readOpenAISSELine(reader)
		if len(line) > 0 {
			if strings.TrimRight(line, "\r\n") == "" && accumulator.eventType != "" && len(accumulator.dataLines) == 0 {
				return sawDone, upstreamProgressUnknown, streamReadErr
			}
			if !accumulator.consumeLine(line, processData) {
				return sawDone, progress, streamReadErr
			}
		}
		if err == nil {
			continue
		}
		if err == io.EOF {
			break
		}
		progress = mergeUpstreamSemanticProgress(progress, upstreamProgressUnknown)
		return false, progress, fmt.Errorf("reading SSE stream: %w", err)
	}
	if !accumulator.dispatch(processData) {
		return sawDone, progress, streamReadErr
	}
	return sawDone, progress, nil
}

// flushWriter wraps an http.ResponseWriter and flushes after every Write.
type flushWriter struct {
	w        http.ResponseWriter
	flusher  http.Flusher
	writeErr error
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err != nil {
		// Record that the failure came from writing to the client (e.g. the client
		// disconnected) so callers can distinguish a client-side error from an
		// upstream/source read error on an io.Copy.
		fw.writeErr = err
		return n, err
	}
	if fw.flusher != nil {
		fw.flusher.Flush()
	}
	return n, err
}

// StreamOpenAIPassthrough streams OpenAI SSE bytes directly to the client.
func StreamOpenAIPassthrough(w http.ResponseWriter, body io.ReadCloser) {
	StreamOpenAIPassthroughWithFinalResponse(w, body, nil)
}

// StreamOpenAIPassthroughWithFinalResponse streams OpenAI SSE lines to the
// client and optionally invokes onFinalResponse with the complete aggregated
// OpenAI response after the upstream stream terminates successfully with [DONE].
func StreamOpenAIPassthroughWithFinalResponse(
	w http.ResponseWriter,
	body io.ReadCloser,
	onFinalResponse func(*models.OpenAIResponse),
	onUsageCallbacks ...func(*models.OpenAIUsage),
) {
	streamOpenAIPassthrough(w, body, false, nil, onFinalResponse, nil, false, streamLifecycleHooks{}, onUsageCallbacks...)
}

// StreamOpenAIPassthroughDroppingInjectedUsage behaves like
// StreamOpenAIPassthroughWithFinalResponse but omits the upstream's final
// usage-only chunk (empty choices + non-nil usage) from the client stream while
// still feeding that usage to the callbacks. It is used when the proxy injected
// stream_options.include_usage on a client stream that did not opt in, so token
// stats are captured without forwarding a terminal chunk the client never
// requested (clients indexing choices[0] on every chunk would otherwise break).
func StreamOpenAIPassthroughDroppingInjectedUsage(
	w http.ResponseWriter,
	body io.ReadCloser,
	onFinalResponse func(*models.OpenAIResponse),
	onUsageCallbacks ...func(*models.OpenAIUsage),
) {
	streamOpenAIPassthrough(w, body, true, nil, onFinalResponse, nil, false, streamLifecycleHooks{}, onUsageCallbacks...)
}

// StreamOpenAIChatPassthrough forwards an OpenAI chat SSE stream to the client
// like StreamOpenAIPassthroughWithFinalResponse, additionally invoking onError
// (with the failure's HTTP status) when a post-commit upstream error event
// (event: error / data: {"error":...}), an upstream read error, or a premature
// end is seen, and dropping the proxy-injected usage-only chunk when
// dropInjectedUsage is set. onError receives the classified status (e.g. 429 for
// a rate limit) or 502 for a generic transport break. It is the entry point for
// the client-facing /v1/chat/completions streaming path, where the request must
// be recorded as a failure if the stream errors after its 200 header was
// committed. onError is never invoked for a client-side write failure.
func StreamOpenAIChatPassthrough(
	w http.ResponseWriter,
	body io.ReadCloser,
	requestedModel string,
	dropInjectedUsage bool,
	onError func(status int),
	onFinalResponse func(*models.OpenAIResponse),
	onUsageCallbacks ...func(*models.OpenAIUsage),
) {
	streamOpenAIPassthrough(w, body, dropInjectedUsage, onError, onFinalResponse, newOpenAIChatCompletionChunkNormalizer(requestedModel).normalize, false, streamLifecycleHooks{}, onUsageCallbacks...)
}

func streamOpenAIChatPassthroughWithLifecycle(
	w http.ResponseWriter,
	body io.ReadCloser,
	requestedModel string,
	dropInjectedUsage bool,
	onError func(status int),
	onFinalResponse func(*models.OpenAIResponse),
	lifecycle streamLifecycleHooks,
	onUsageCallbacks ...func(*models.OpenAIUsage),
) {
	streamOpenAIPassthrough(w, body, dropInjectedUsage, onError, onFinalResponse, newOpenAIChatCompletionChunkNormalizer(requestedModel).normalize, false, lifecycle, onUsageCallbacks...)
}

func streamExplicitRouteOpenAIChatPassthroughWithLifecycle(
	w http.ResponseWriter,
	body io.ReadCloser,
	publicModel string,
	dropInjectedUsage bool,
	onError func(status int),
	onFinalResponse func(*models.OpenAIResponse),
	lifecycle streamLifecycleHooks,
	onUsageCallbacks ...func(*models.OpenAIUsage),
) {
	normalizer := newOpenAIChatCompletionChunkNormalizer(publicModel)
	transform := func(eventType, data string) (string, bool) {
		normalized, changed := normalizer.normalize(eventType, data)
		if !openAIChatStreamEventMayCarryChunk(eventType) || strings.TrimSpace(normalized) == "" || strings.TrimSpace(normalized) == "[DONE]" {
			return normalized, changed
		}
		var payload map[string]json.RawMessage
		if json.Unmarshal([]byte(normalized), &payload) != nil || payload == nil || hasNonNullJSONField(payload, "error") {
			return normalized, changed
		}
		model := strings.TrimSpace(publicModel)
		if model == "" || jsonRawString(payload["model"]) == model {
			return normalized, changed
		}
		payload["model"] = mustMarshalRaw(model)
		rewritten, err := json.Marshal(payload)
		if err != nil {
			return normalized, changed
		}
		return string(rewritten), true
	}
	streamOpenAIPassthrough(w, body, dropInjectedUsage, onError, onFinalResponse, transform, true, lifecycle, onUsageCallbacks...)
}

// streamOpenAIPassthrough forwards an upstream OpenAI SSE stream to the client.
// It buffers each SSE event (its raw lines up to and including the terminating
// blank line) and flushes the event as a unit once complete; SSE clients do not
// dispatch an event before its blank-line terminator, so this is byte-identical
// to a line-immediate copy for the client. Buffering by event is what lets the
// dropInjectedUsage path decide whether to emit an event after parsing it: when
// set, a usage-only chunk (empty choices + usage) is fed to the usage/final
// callbacks but not written to the client.
type openAIStreamDataTransformer func(eventType, data string) (string, bool)

func streamOpenAIPassthrough(
	w http.ResponseWriter,
	body io.ReadCloser,
	dropInjectedUsage bool,
	onError func(status int),
	onFinalResponse func(*models.OpenAIResponse),
	transformData openAIStreamDataTransformer,
	failOnOversized bool,
	lifecycle streamLifecycleHooks,
	onUsageCallbacks ...func(*models.OpenAIUsage),
) {
	trackedWriter := &commitTrackingResponseWriter{ResponseWriter: w}
	w = trackedWriter
	bodyHandled := false
	defer func() {
		if !bodyHandled {
			_ = body.Close()
		}
	}()
	setSSEHeaders(w)
	// This is a streamed SSE response, so any upstream Content-Length copied by
	// the caller (via copyPassthroughHeaders) is wrong — and definitely wrong on
	// the drop-injected-usage path, which writes fewer bytes than the upstream
	// advertised. Drop it so clients read until EOF instead of hanging on or
	// truncating at a stale length.
	w.Header().Del("Content-Length")

	var flusher http.Flusher
	if f, ok := w.(http.Flusher); ok {
		flusher = f
	}

	onUsage := firstOpenAIUsageCallback(onUsageCallbacks)
	var aggregator *openAIResponseAggregator
	if onFinalResponse != nil {
		aggregator = newOpenAIResponseAggregator()
	}
	reader := bufio.NewReaderSize(body, openAIStreamScannerInitialBuffer)

	sawDone := false
	sawTerminalError := false
	dropCurrent := false
	errorReported := false
	var accumulator sseDataAccumulator
	pending := make([]string, 0, 4)
	var transformedCurrentData *string
	processData := func(eventType string, data string) bool {
		if data == "[DONE]" {
			sawDone = true
			return false
		}

		// A post-commit upstream error (event: error or data: {"error":...})
		// arrives after the HTTP 200; surface it so the request is recorded as a
		// failure even though the client already received a 200 header, with the
		// error's classified status (e.g. 429 for a rate limit) rather than 502.
		if streamErr, isErr := parseOpenAIStreamError(eventType, data); isErr {
			sawTerminalError = true
			if onError != nil && !errorReported {
				onError(streamErr.httpStatus())
			}
			errorReported = true
			return false
		}

		if transformData != nil {
			if transformed, changed := transformData(eventType, data); changed {
				data = transformed
				transformedCurrentData = &data
			}
		}
		if !dropInjectedUsage && onUsage == nil && aggregator == nil {
			return true
		}

		var chunk models.OpenAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return true
		}
		if chunk.Usage != nil {
			if onUsage != nil {
				onUsage(chunk.Usage)
			}
			if dropInjectedUsage && len(chunk.Choices) == 0 {
				dropCurrent = true
			}
		}
		if aggregator != nil {
			aggregator.addChunk(chunk)
		}
		return true
	}

	pendingBytes := 0
	flushEvent := func() bool {
		if len(pending) == 0 {
			return true
		}
		defer func() {
			pending = pending[:0]
			pendingBytes = 0
			dropCurrent = false
			transformedCurrentData = nil
		}()
		if dropCurrent {
			return true
		}
		if transformedCurrentData != nil {
			if !writeTransformedSSEEvent(w, pending, *transformedCurrentData) {
				return false
			}
		} else {
			for _, l := range pending {
				if _, err := io.WriteString(w, l); err != nil {
					return false
				}
			}
		}
		if flusher != nil {
			flusher.Flush()
		}
		return true
	}

	reportOversized := func() {
		if onError != nil && !errorReported {
			onError(http.StatusBadGateway)
		}
		errorReported = true
	}
	rawForwardOversizedEvent := false
streamLoop:
	for {
		line, err := readOpenAISSELine(reader)
		if errors.Is(err, errOpenAISSELineTooLong) {
			if failOnOversized {
				reportOversized()
				return
			}
			// A single SSE line exceeded the parse buffer (e.g. a very large
			// tool-call argument). The returned bytes are valid but the line is
			// truncated for parsing, so flush everything buffered for this event
			// plus the partial line and forward the remainder of this event raw.
			// Once the event boundary arrives, resume parsed event buffering so
			// later usage-only chunks injected by this proxy can still be dropped.
			for _, l := range pending {
				if _, werr := io.WriteString(w, l); werr != nil {
					return
				}
			}
			pending = pending[:0]
			pendingBytes = 0
			accumulator = sseDataAccumulator{}
			dropCurrent = false
			transformedCurrentData = nil
			if _, werr := io.WriteString(w, line); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			rawForwardOversizedEvent = true
			continue
		}
		if len(line) > 0 {
			if rawForwardOversizedEvent {
				if _, werr := io.WriteString(w, line); werr != nil {
					return
				}
				if flusher != nil {
					flusher.Flush()
				}
				if strings.TrimRight(line, "\r\n") == "" {
					rawForwardOversizedEvent = false
					accumulator = sseDataAccumulator{}
					dropCurrent = false
					transformedCurrentData = nil
				}
			} else {
				if failOnOversized && pendingBytes+len(line) > openAIStreamScannerMaxBuffer {
					reportOversized()
					return
				}
				pending = append(pending, line)
				pendingBytes += len(line)
				isBoundary := strings.TrimRight(line, "\r\n") == ""
				continueStream := accumulator.consumeLine(line, processData)
				if isBoundary && !flushEvent() {
					return
				}
				if isBoundary && !continueStream {
					break streamLoop
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			// Any other non-EOF read error after the 200 was committed is a broken
			// stream (upstream reset / transport error); surface it as a failure
			// unless an error event already reported a (more specific) status.
			if lifecycle.suppressTransportCancellation(trackedWriter.committed) {
				return
			}
			if onError != nil && !errorReported {
				onError(http.StatusBadGateway)
			}
			return
		}
	}

	_ = accumulator.dispatch(processData)
	if !flushEvent() {
		return
	}
	if sawDone || sawTerminalError {
		// Consume the normal HTTP response EOF (including any bytes already held by
		// reader) before closing so HTTP/1.x keep-alive remains reusable. A held-open
		// upstream is still bounded by upstreamErrorDetailDrainTimeout.
		drainReaderAndClose(reader, body)
		bodyHandled = true
	}
	if sawTerminalError {
		return
	}
	if !sawDone {
		// The stream ended (EOF) without a [DONE] sentinel: the upstream closed
		// the connection prematurely, so the client received a truncated stream.
		if lifecycle.suppressTransportCancellation(trackedWriter.committed) {
			return
		}
		if onError != nil && !errorReported {
			onError(http.StatusBadGateway)
		}
		return
	}
	if onFinalResponse == nil || aggregator == nil {
		return
	}
	onFinalResponse(aggregator.buildResponse())
}

func writeTransformedSSEEvent(w http.ResponseWriter, pending []string, data string) bool {
	insertedData := false
	writeData := func(ending string) bool {
		if ending == "" {
			ending = "\n"
		}
		for _, line := range strings.Split(data, "\n") {
			if _, err := fmt.Fprintf(w, "data: %s%s", line, ending); err != nil {
				return false
			}
		}
		insertedData = true
		return true
	}

	for _, line := range pending {
		content, ending := splitSSELineEnding(line)
		if _, ok := parseSSELine(content); ok {
			if !insertedData && !writeData(ending) {
				return false
			}
			continue
		}
		if strings.TrimSpace(content) == "" && !insertedData {
			if !writeData(ending) {
				return false
			}
		}
		if _, err := io.WriteString(w, line); err != nil {
			return false
		}
	}
	return true
}

func firstOpenAIUsageCallback(callbacks []func(*models.OpenAIUsage)) func(*models.OpenAIUsage) {
	for _, callback := range callbacks {
		if callback != nil {
			return callback
		}
	}
	return nil
}

func streamAnthropicPassthroughBody(ctx context.Context, w http.ResponseWriter, body io.Reader, publicModel, upstreamModel string, lifecycleHooks ...streamLifecycleHooks) {
	publicModel = strings.TrimSpace(publicModel)
	upstreamModel = strings.TrimSpace(upstreamModel)

	var flusher http.Flusher
	if f, ok := w.(http.Flusher); ok {
		flusher = f
	}

	// Tap each SSE frame for Anthropic usage so direct-routed streaming traffic
	// records tokens. The tap never alters the bytes written to the client. Also
	// record a failure status when the stream carries an Anthropic error frame or
	// the upstream read breaks, so a post-commit failure is not counted as a 2xx
	// success (client-side write failures are excluded — they are client aborts).
	usage := &anthropicStreamUsageAccumulator{}
	defer usage.flush(ctx)
	lifecycle := streamLifecycleHooks{}
	if len(lifecycleHooks) > 0 {
		lifecycle = lifecycleHooks[0]
	}
	markFailure := func(data []byte) {
		if status, ok := anthropicStreamErrorStatus(data); ok {
			observeResponseFailureStatus(ctx, status)
		}
	}
	emitShutdownError := func() {
		_ = writeAnthropicShutdownSSEEvent(w)
	}

	if publicModel == "" || publicModel == upstreamModel {
		// No model rewrite: preserve the original byte-exact, unbounded passthrough
		// (io.Copy handles SSE lines of any size) while teeing the bytes through a
		// best-effort usage/error sniffer. The sniffer skips lines it cannot buffer.
		sniffer := newAnthropicUsageSniffWriter(usage, markFailure)
		fw := &flushWriter{w: w, flusher: flusher}
		handleLifecycleCancellation := func() bool {
			if lifecycle.transportCanceled == nil || !lifecycle.transportCanceled() {
				return false
			}
			if sniffer.pendingTerminalNeedsDelimiter() {
				_ = sniffer.completePendingTerminalFrame(w, flusher)
				return true
			}
			if lifecycle.suppressStats != nil {
				lifecycle.suppressStats()
			}
			// The byte-exact path cannot retract arbitrary partial bytes already sent
			// to the client. Only append a new SSE event when the stream is currently
			// at a safe frame boundary; otherwise end the connection without making
			// the partial frame publishable or adding a contradictory terminal event.
			if !sniffer.bytesSeen || anthropicSSETailEndsFrame(sniffer.tail) {
				emitShutdownError()
			}
			return true
		}
		if _, err := io.Copy(fw, io.TeeReader(body, sniffer)); err != nil {
			if sniffer.sawTerminalEvent {
				return
			}
			// io.Copy failed: distinguish a client-side write error (client gone,
			// recorded on the flushWriter) from an upstream read break. Only an
			// upstream read break is an upstream failure.
			if fw.writeErr == nil && handleLifecycleCancellation() {
				return
			}
			if ctx.Err() == nil && fw.writeErr == nil {
				observeResponseFailureStatus(ctx, http.StatusBadGateway)
			}
		} else if ctx.Err() == nil && !sniffer.sawTerminalEvent {
			if handleLifecycleCancellation() {
				return
			}
			// Clean EOF before Anthropic's terminal message_stop means the upstream
			// stream was truncated even though the transport closed without error.
			observeResponseFailureStatus(ctx, http.StatusBadGateway)
		}
		return
	}

	reader := bufio.NewReaderSize(body, openAIStreamScannerInitialBuffer)
	frame := make([]string, 0, 4)
	clientWriteFailed := false
	sawTerminalEvent := false
	emit := func() {
		if len(frame) == 0 {
			return
		}
		for _, line := range frame {
			content, _ := splitSSELineEnding(line)
			if data, ok := parseSSELine(content); ok {
				dataBytes := []byte(data)
				usage.observe(dataBytes)
				markFailure(dataBytes)
				if anthropicStreamDataIsMessageStop(dataBytes) {
					sawTerminalEvent = true
				} else if _, ok := anthropicStreamErrorStatus(dataBytes); ok {
					sawTerminalEvent = true
				}
			}
		}
		if !writeAnthropicSSEFrame(w, frame, publicModel) {
			clientWriteFailed = true
		}
		if flusher != nil {
			flusher.Flush()
		}
		frame = frame[:0]
	}
	for {
		line, err := readOpenAISSELine(reader)
		if errors.Is(err, errOpenAISSELineTooLong) {
			frame = frame[:0]
			if ctx.Err() == nil && !clientWriteFailed {
				observeResponseFailureStatus(ctx, http.StatusBadGateway)
			}
			return
		}
		if len(line) > 0 {
			frame = append(frame, line)
			if strings.TrimRight(line, "\r\n") == "" {
				emit()
			}
		}
		if err != nil {
			if sawTerminalEvent {
				frame = frame[:0]
				return
			}
			if !clientWriteFailed && lifecycle.suppressTransportCancellation(true) {
				frame = frame[:0]
				emitShutdownError()
				return
			}
			emit()
			if sawTerminalEvent {
				return
			}
			if ctx.Err() == nil && !clientWriteFailed {
				if err != io.EOF {
					// Upstream read error after the 200 was committed: a broken stream.
					observeResponseFailureStatus(ctx, http.StatusBadGateway)
				} else if !sawTerminalEvent {
					// Clean EOF before Anthropic's terminal message_stop is still a
					// truncated stream.
					observeResponseFailureStatus(ctx, http.StatusBadGateway)
				}
			}
			return
		}
	}
}

func anthropicStreamDataIsMessageStop(data []byte) bool {
	var event struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(data, &event); err != nil {
		return false
	}
	return strings.TrimSpace(event.Type) == "message_stop"
}

// anthropicUsageSniffWriter scans an Anthropic SSE byte stream for usage (and
// error frames) as it is copied to the client. It buffers a single SSE line at a
// time and, on each complete data line, feeds the payload to the accumulator and
// the optional onData callback (used to detect error frames). A line longer than
// the buffer cap is skipped so the sniffer never affects the client copy or grows
// unbounded.
type anthropicUsageSniffWriter struct {
	acc               *anthropicStreamUsageAccumulator
	onData            func([]byte)
	line              []byte
	tail              []byte
	overflow          bool
	bytesSeen         bool
	byteCount         int64
	pendingTerminal   bool
	pendingTerminalAt int64
	sawTerminalEvent  bool
}

func newAnthropicUsageSniffWriter(acc *anthropicStreamUsageAccumulator, onData func([]byte)) *anthropicUsageSniffWriter {
	return &anthropicUsageSniffWriter{
		acc:    acc,
		onData: onData,
		line:   make([]byte, 0, 512),
		tail:   make([]byte, 0, 4),
	}
}

func (s *anthropicUsageSniffWriter) Write(p []byte) (int, error) {
	for _, b := range p {
		s.bytesSeen = true
		s.byteCount++
		s.tail = append(s.tail, b)
		if len(s.tail) > 4 {
			copy(s.tail, s.tail[len(s.tail)-4:])
			s.tail = s.tail[:4]
		}
		if b == '\n' {
			lineContent := strings.TrimRight(string(s.line), "\r")
			if !s.overflow {
				if data, ok := parseSSELine(lineContent); ok {
					dataBytes := []byte(data)
					s.acc.observe(dataBytes)
					if s.onData != nil {
						s.onData(dataBytes)
					}
					if anthropicStreamDataIsMessageStop(dataBytes) {
						s.pendingTerminal = true
						s.pendingTerminalAt = s.byteCount
					} else if _, ok := anthropicStreamErrorStatus(dataBytes); ok {
						s.pendingTerminal = true
						s.pendingTerminalAt = s.byteCount
					}
				}
			}
			if lineContent == "" {
				s.sawTerminalEvent = s.sawTerminalEvent || s.pendingTerminal
				s.pendingTerminal = false
			}
			s.line = s.line[:0]
			s.overflow = false
			continue
		}
		if len(s.line) >= openAIStreamScannerMaxBuffer {
			// Pathological oversized line: stop buffering it, skip its usage.
			s.overflow = true
			s.line = s.line[:0]
			continue
		}
		s.line = append(s.line, b)
	}
	return len(p), nil
}

func (s *anthropicUsageSniffWriter) pendingTerminalNeedsDelimiter() bool {
	return s != nil && s.pendingTerminal && !s.sawTerminalEvent && s.byteCount == s.pendingTerminalAt &&
		(bytes.HasSuffix(s.tail, []byte("\n")) || bytes.HasSuffix(s.tail, []byte("\r\n")))
}

func (s *anthropicUsageSniffWriter) completePendingTerminalFrame(w io.Writer, flusher http.Flusher) bool {
	if !s.pendingTerminalNeedsDelimiter() {
		return false
	}
	separator := "\n"
	if bytes.HasSuffix(s.tail, []byte("\r\n")) {
		separator = "\r\n"
	}
	if _, err := io.WriteString(w, separator); err != nil {
		return false
	}
	s.pendingTerminal = false
	s.sawTerminalEvent = true
	if flusher != nil {
		flusher.Flush()
	}
	return true
}

func anthropicSSETailEndsFrame(tail []byte) bool {
	for _, suffix := range [][]byte{
		[]byte("\n\n"),
		[]byte("\n\r\n"),
		[]byte("\r\n\n"),
		[]byte("\r\n\r\n"),
	} {
		if bytes.HasSuffix(tail, suffix) {
			return true
		}
	}
	return false
}

func writeAnthropicSSEFrame(w io.Writer, frame []string, publicModel string) bool {
	for _, line := range frame {
		content, ending := splitSSELineEnding(line)
		data, ok := parseSSELine(content)
		if !ok {
			if _, err := io.WriteString(w, line); err != nil {
				return false
			}
			continue
		}

		rewritten, changed := rewriteAnthropicResponseModelJSON([]byte(data), publicModel, "")
		if !changed {
			if _, err := io.WriteString(w, line); err != nil {
				return false
			}
			continue
		}
		if _, err := fmt.Fprintf(w, "data: %s%s", rewritten, ending); err != nil {
			return false
		}
	}
	return true
}

func splitSSELineEnding(line string) (string, string) {
	switch {
	case strings.HasSuffix(line, "\r\n"):
		return strings.TrimSuffix(line, "\r\n"), "\r\n"
	case strings.HasSuffix(line, "\n"):
		return strings.TrimSuffix(line, "\n"), "\n"
	default:
		return line, ""
	}
}

// StreamOpenAIToAnthropic translates an OpenAI SSE stream into Anthropic SSE format.
func StreamOpenAIToAnthropic(w http.ResponseWriter, body io.ReadCloser, model string, requestID string) {
	StreamOpenAIToAnthropicWithFinalResponse(w, body, model, requestID, nil, nil)
}

// StreamOpenAIToAnthropicWithFinalResponse translates an OpenAI SSE stream into
// Anthropic SSE format and optionally invokes onFinalResponse with the complete
// aggregated OpenAI response after the translated stream finishes successfully.
// onError, when non-nil, is invoked if the upstream stream errors or ends before
// [DONE] after the SSE headers were already committed, so the request can be
// recorded as a failure even though its HTTP status was sent as 200.
func StreamOpenAIToAnthropicWithFinalResponse(
	w http.ResponseWriter,
	body io.ReadCloser,
	model string,
	requestID string,
	onError func(status int),
	onFinalResponse func(*models.OpenAIResponse),
	onUsageCallbacks ...func(*models.OpenAIUsage),
) {
	streamOpenAIToAnthropicWithLifecycle(w, body, model, requestID, onError, onFinalResponse, streamLifecycleHooks{}, onUsageCallbacks...)
}

func streamOpenAIToAnthropicWithLifecycle(
	w http.ResponseWriter,
	body io.ReadCloser,
	model string,
	requestID string,
	onError func(status int),
	onFinalResponse func(*models.OpenAIResponse),
	lifecycle streamLifecycleHooks,
	onUsageCallbacks ...func(*models.OpenAIUsage),
) {
	defer func() { _ = body.Close() }()
	trackedWriter := &commitTrackingResponseWriter{ResponseWriter: w}
	w = trackedWriter
	setSSEHeaders(w)

	state := newAnthropicStreamState(w, model, requestID)
	if !state.start() {
		return
	}
	startStream := func() bool { return true }
	handleLifecycleCancellation := func() bool {
		if !lifecycle.suppressTransportCancellation(trackedWriter.committed) {
			return false
		}
		// message_start is intentionally eager for Anthropic TTFB. Once it has
		// committed the stream, shutdown must terminate it with a valid Anthropic
		// error event rather than attempting an HTTP 503 or leaving it truncated.
		if trackedWriter.committed && !state.clientWriteFailed {
			state.emitShutdownError()
		}
		return true
	}

	var aggregator *openAIResponseAggregator
	if onFinalResponse != nil {
		aggregator = newOpenAIResponseAggregator()
	}
	onUsage := firstOpenAIUsageCallback(onUsageCallbacks)

	sawDone, err := consumeOpenAIStreamChunks(body, func(chunk models.OpenAIStreamChunk) bool {
		if onUsage != nil && chunk.Usage != nil {
			onUsage(chunk.Usage)
		}
		if aggregator != nil {
			aggregator.addChunk(chunk)
		}
		return startStream() && state.consumeChunk(chunk)
	})
	if err != nil {
		var streamErr *openAIStreamError
		if errors.As(err, &streamErr) {
			if !startStream() {
				return
			}
			// A genuine upstream error event/read error: record the classified
			// failure status (not on a client-side write abort).
			if onError != nil && !state.clientWriteFailed {
				onError(streamErr.httpStatus())
			}
			state.emitError(streamErr.Error())
			return
		}
		if handleLifecycleCancellation() {
			return
		}
		if !startStream() {
			return
		}
		if onError != nil && !state.clientWriteFailed {
			onError(http.StatusBadGateway)
		}
		state.emitError(fmt.Sprintf("upstream stream read failed: %v", err))
		return
	}
	if !sawDone {
		// The stream stopped before [DONE]. If our writes to the client failed,
		// this is a client abort, not an upstream failure — do not record it.
		if handleLifecycleCancellation() {
			return
		}
		if !startStream() {
			return
		}
		if onError != nil && !state.clientWriteFailed {
			onError(http.StatusBadGateway)
		}
		state.emitError("upstream stream ended before [DONE]")
		return
	}

	if !startStream() || !state.finish() {
		return
	}

	if onFinalResponse != nil {
		onFinalResponse(aggregator.buildResponse())
	}
}

// aggregateStreamToResponse collects an OpenAI SSE stream into a complete
// OpenAIResponse. This is used when we force streaming to the upstream for
// reliable parallel tool call support, but the client requested non-streaming.
func aggregateStreamToResponse(body io.ReadCloser) (*models.OpenAIResponse, error) {
	response, _, err := aggregateStreamToResponseWithProgress(body)
	return response, err
}

// aggregateStreamToResponseWithProgress is the explicit-route aggregation seam.
// It retains the legacy aggregate result while also reporting whether text,
// reasoning, tool activity, an unknown event, or a terminal frame was observed
// before an error. Callers may only switch targets when the returned progress is
// none or an allowed role/preamble chunk.
func aggregateStreamToResponseWithProgress(body io.ReadCloser) (*models.OpenAIResponse, upstreamSemanticProgress, error) {
	defer func() { _ = body.Close() }()

	aggregator := newOpenAIResponseAggregator()
	sawDone, progress, err := consumeOpenAIStreamChunksWithProgress(body, func(chunk models.OpenAIStreamChunk) bool {
		aggregator.addChunk(chunk)
		return true
	})
	if err != nil {
		return nil, progress, err
	}
	if !sawDone {
		return nil, progress, fmt.Errorf("stream ended before [DONE]")
	}

	return aggregator.buildResponse(), progress, nil
}

type anthropicStreamState struct {
	w         http.ResponseWriter
	model     string
	requestID string

	nextBlockIndex      int
	textBlockIndex      int
	storedFinishReason  string
	storedUsage         *models.OpenAIUsage
	toolCallBlockIndex  map[int]int
	openToolCallIndexes map[int]struct{}
	// clientWriteFailed is set when an emit to the client fails (the client
	// disconnected). It lets the caller distinguish a client-side abort from an
	// upstream stream failure so the request is not mislabeled as a 502.
	clientWriteFailed bool
}

func newAnthropicStreamState(w http.ResponseWriter, model string, requestID string) *anthropicStreamState {
	return &anthropicStreamState{
		w:                   w,
		model:               model,
		requestID:           requestID,
		textBlockIndex:      -1,
		toolCallBlockIndex:  make(map[int]int),
		openToolCallIndexes: make(map[int]struct{}),
	}
}

func (s *anthropicStreamState) start() bool {
	return s.emit("message_start", models.AnthropicStreamEvent{
		Type: "message_start",
		Message: &models.AnthropicResponse{
			ID:      s.requestID,
			Type:    "message",
			Role:    "assistant",
			Model:   s.model,
			Content: []models.ContentBlock{},
			Usage:   models.AnthropicUsage{},
		},
	})
}

func (s *anthropicStreamState) emit(eventType string, data interface{}) bool {
	if writeSSEEvent(s.w, eventType, data) != nil {
		s.clientWriteFailed = true
		return false
	}
	return true
}

func (s *anthropicStreamState) emitError(message string) bool {
	return s.emitTypedError("api_error", message)
}

func (s *anthropicStreamState) emitTypedError(errorType, message string) bool {
	errorType = strings.TrimSpace(errorType)
	if errorType == "" {
		errorType = "api_error"
	}
	message = strings.TrimSpace(message)
	if message == "" {
		message = "upstream stream ended unexpectedly"
	}
	return s.emit("error", map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    errorType,
			"message": message,
		},
	})
}

func (s *anthropicStreamState) emitShutdownError() bool {
	if !writeAnthropicShutdownSSEEvent(s.w) {
		s.clientWriteFailed = true
		return false
	}
	return true
}

func writeAnthropicShutdownSSEEvent(w http.ResponseWriter) bool {
	return writeSSEEvent(w, "error", map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    "overloaded_error",
			"message": "server shutting down",
		},
	}) == nil
}

func (s *anthropicStreamState) consumeChunk(chunk models.OpenAIStreamChunk) bool {
	if chunk.Usage != nil {
		s.storedUsage = chunk.Usage
	}

	for _, choice := range chunk.Choices {
		if !s.consumeChoice(choice) {
			return false
		}
	}

	return true
}

func (s *anthropicStreamState) consumeChoice(choice models.OpenAIStreamChoice) bool {
	if choice.Delta.Content != nil {
		var text string
		if err := json.Unmarshal(choice.Delta.Content, &text); err == nil && text != "" {
			if !s.emitText(text) {
				return false
			}
		}
	}
	if choice.Delta.Refusal != nil {
		var refusal string
		if err := json.Unmarshal(choice.Delta.Refusal, &refusal); err == nil && refusal != "" {
			if !s.emitText(refusal) {
				return false
			}
		}
	}

	for _, toolCall := range choice.Delta.ToolCalls {
		if !s.consumeToolCall(toolCall) {
			return false
		}
	}

	if choice.FinishReason != nil {
		s.storedFinishReason = *choice.FinishReason
	}

	return true
}

func (s *anthropicStreamState) emitText(text string) bool {
	if !s.closeOpenToolBlocks() {
		return false
	}

	if s.textBlockIndex < 0 {
		s.textBlockIndex = s.nextBlockIndex
		s.nextBlockIndex++
		if !s.emit("content_block_start", models.AnthropicStreamEvent{
			Type:  "content_block_start",
			Index: intVal(s.textBlockIndex),
			ContentBlock: &models.ContentBlock{
				Type: "text",
				Text: stringPtr(""),
			},
		}) {
			return false
		}
	}

	return s.emit("content_block_delta", models.AnthropicStreamEvent{
		Type:  "content_block_delta",
		Index: intVal(s.textBlockIndex),
		Delta: &models.AnthropicDelta{
			Type: "text_delta",
			Text: text,
		},
	})
}

func (s *anthropicStreamState) consumeToolCall(toolCall models.OpenAIToolCall) bool {
	toolIndex := 0
	if toolCall.Index != nil {
		toolIndex = *toolCall.Index
	}

	if toolCall.ID != "" && !s.startToolCall(toolIndex, toolCall) {
		return false
	}

	if toolCall.Function.Arguments == "" {
		return true
	}

	blockIndex, ok := s.toolCallBlockIndex[toolIndex]
	if !ok {
		return true
	}
	if _, open := s.openToolCallIndexes[toolIndex]; !open {
		return true
	}

	return s.emit("content_block_delta", models.AnthropicStreamEvent{
		Type:  "content_block_delta",
		Index: intVal(blockIndex),
		Delta: &models.AnthropicDelta{
			Type:        "input_json_delta",
			PartialJSON: toolCall.Function.Arguments,
		},
	})
}

func (s *anthropicStreamState) startToolCall(toolIndex int, toolCall models.OpenAIToolCall) bool {
	if !s.closeTextBlock() {
		return false
	}

	if _, ok := s.toolCallBlockIndex[toolIndex]; !ok {
		s.toolCallBlockIndex[toolIndex] = s.nextBlockIndex
		s.nextBlockIndex++
	}

	if _, open := s.openToolCallIndexes[toolIndex]; open {
		return true
	}

	blockIndex := s.toolCallBlockIndex[toolIndex]
	if !s.emit("content_block_start", models.AnthropicStreamEvent{
		Type:  "content_block_start",
		Index: intVal(blockIndex),
		ContentBlock: &models.ContentBlock{
			Type:  "tool_use",
			ID:    toolCall.ID,
			Name:  toolCall.Function.Name,
			Input: json.RawMessage(`{}`),
		},
	}) {
		return false
	}

	s.openToolCallIndexes[toolIndex] = struct{}{}
	return true
}

func (s *anthropicStreamState) closeTextBlock() bool {
	if s.textBlockIndex < 0 {
		return true
	}

	if !s.emit("content_block_stop", models.AnthropicStreamEvent{
		Type:  "content_block_stop",
		Index: intVal(s.textBlockIndex),
	}) {
		return false
	}

	s.textBlockIndex = -1
	return true
}

func (s *anthropicStreamState) closeOpenToolBlocks() bool {
	if len(s.openToolCallIndexes) == 0 {
		return true
	}

	blockIndexes := make([]int, 0, len(s.openToolCallIndexes))
	for toolIndex := range s.openToolCallIndexes {
		if blockIndex, ok := s.toolCallBlockIndex[toolIndex]; ok {
			blockIndexes = append(blockIndexes, blockIndex)
		}
	}
	// Sort by Anthropic block index so stop events are emitted in the same
	// client-visible order as the corresponding block_start events.
	sort.Ints(blockIndexes)

	for _, blockIndex := range blockIndexes {
		if !s.emit("content_block_stop", models.AnthropicStreamEvent{
			Type:  "content_block_stop",
			Index: intVal(blockIndex),
		}) {
			return false
		}
	}

	clear(s.openToolCallIndexes)
	return true
}

func (s *anthropicStreamState) finish() bool {
	if !s.closeTextBlock() {
		return false
	}
	if !s.closeOpenToolBlocks() {
		return false
	}

	delta := &models.AnthropicDelta{}
	if s.storedFinishReason != "" {
		delta.StopReason = convertFinishReason(s.storedFinishReason)
	}

	event := models.AnthropicStreamEvent{
		Type:  "message_delta",
		Delta: delta,
	}
	if s.storedUsage != nil {
		usage := openAIUsageToAnthropicUsage(s.storedUsage)
		event.Usage = &usage
	}

	if !s.emit("message_delta", event) {
		return false
	}

	return s.emit("message_stop", models.AnthropicStreamEvent{Type: "message_stop"})
}

type aggregatedOpenAIChoice struct {
	role              string
	content           strings.Builder
	refusal           strings.Builder
	toolCalls         map[int]*models.OpenAIToolCall
	toolCallArguments map[int]*strings.Builder
	finishReason      *string
}

type openAIResponseAggregator struct {
	response       models.OpenAIResponse
	choicesByIndex map[int]*aggregatedOpenAIChoice
}

func newOpenAIResponseAggregator() *openAIResponseAggregator {
	return &openAIResponseAggregator{
		choicesByIndex: make(map[int]*aggregatedOpenAIChoice),
	}
}

func (a *openAIResponseAggregator) addChunk(chunk models.OpenAIStreamChunk) {
	if a.response.ID == "" {
		a.response.ID = chunk.ID
		a.response.Object = "chat.completion"
		a.response.Created = chunk.Created
		a.response.Model = chunk.Model
	}
	if a.response.SystemFingerprint == "" && chunk.SystemFingerprint != "" {
		a.response.SystemFingerprint = chunk.SystemFingerprint
	}
	if chunk.Usage != nil {
		a.response.Usage = chunk.Usage
	}

	for _, choice := range chunk.Choices {
		a.addChoice(choice)
	}
}

func (a *openAIResponseAggregator) addChoice(choice models.OpenAIStreamChoice) {
	aggChoice := a.choice(choice.Index)

	if choice.Delta.Role != "" {
		aggChoice.role = choice.Delta.Role
	}

	if choice.Delta.Content != nil {
		var text string
		if err := json.Unmarshal(choice.Delta.Content, &text); err == nil {
			aggChoice.content.WriteString(text)
		}
	}
	if choice.Delta.Refusal != nil {
		var refusal string
		if err := json.Unmarshal(choice.Delta.Refusal, &refusal); err == nil {
			aggChoice.refusal.WriteString(refusal)
		}
	}

	for _, toolCall := range choice.Delta.ToolCalls {
		toolIndex := 0
		if toolCall.Index != nil {
			toolIndex = *toolCall.Index
		}

		call, ok := aggChoice.toolCalls[toolIndex]
		if !ok {
			call = &models.OpenAIToolCall{}
			aggChoice.toolCalls[toolIndex] = call
		}

		if toolCall.ID != "" {
			call.ID = toolCall.ID
		}
		if toolCall.Type != "" {
			call.Type = toolCall.Type
		}
		if toolCall.Function.Name != "" {
			call.Function.Name = toolCall.Function.Name
		}

		aggChoice.appendToolCallArguments(toolIndex, toolCall.Function.Arguments)
	}

	if choice.FinishReason != nil {
		finishReason := *choice.FinishReason
		aggChoice.finishReason = &finishReason
	}
}

func (a *openAIResponseAggregator) choice(index int) *aggregatedOpenAIChoice {
	aggChoice, ok := a.choicesByIndex[index]
	if ok {
		return aggChoice
	}

	aggChoice = &aggregatedOpenAIChoice{
		role:      "assistant",
		toolCalls: make(map[int]*models.OpenAIToolCall),
	}
	a.choicesByIndex[index] = aggChoice
	return aggChoice
}

// appendToolCallArguments keeps streamed argument fragments out of the
// OpenAIToolCall until build time so long tool-call argument streams do not pay
// repeated string-concatenation copies on every delta.
func (c *aggregatedOpenAIChoice) appendToolCallArguments(index int, arguments string) {
	if arguments == "" {
		return
	}
	if c.toolCallArguments == nil {
		c.toolCallArguments = make(map[int]*strings.Builder)
	}
	builder, ok := c.toolCallArguments[index]
	if !ok {
		builder = &strings.Builder{}
		c.toolCallArguments[index] = builder
	}
	builder.WriteString(arguments)
}

func (a *openAIResponseAggregator) buildResponse() *models.OpenAIResponse {
	choiceIndexes := make([]int, 0, len(a.choicesByIndex))
	for choiceIndex := range a.choicesByIndex {
		choiceIndexes = append(choiceIndexes, choiceIndex)
	}
	sort.Ints(choiceIndexes)

	a.response.Choices = a.response.Choices[:0]
	for _, choiceIndex := range choiceIndexes {
		aggChoice := a.choicesByIndex[choiceIndex]
		a.response.Choices = append(a.response.Choices, models.OpenAIChoice{
			Index:        choiceIndex,
			Message:      a.buildMessage(aggChoice),
			FinishReason: aggChoice.finishReason,
		})
	}

	return &a.response
}

func (a *openAIResponseAggregator) buildMessage(choice *aggregatedOpenAIChoice) models.OpenAIMessage {
	message := models.OpenAIMessage{Role: choice.role}
	if choice.content.Len() > 0 {
		content, _ := json.Marshal(choice.content.String())
		message.Content = content
	}
	if choice.refusal.Len() > 0 {
		refusal, _ := json.Marshal(choice.refusal.String())
		message.Refusal = refusal
	}

	if len(choice.toolCalls) == 0 {
		return message
	}

	toolIndexes := make([]int, 0, len(choice.toolCalls))
	for toolIndex := range choice.toolCalls {
		toolIndexes = append(toolIndexes, toolIndex)
	}
	sort.Ints(toolIndexes)

	for _, toolIndex := range toolIndexes {
		toolCall := *choice.toolCalls[toolIndex]
		if argumentBuilder := choice.toolCallArguments[toolIndex]; argumentBuilder != nil {
			toolCall.Function.Arguments = argumentBuilder.String()
		}
		if !json.Valid([]byte(toolCall.Function.Arguments)) {
			toolCall.Function.Arguments = "{}"
		}
		message.ToolCalls = append(message.ToolCalls, toolCall)
	}

	return message
}

func convertFinishReason(reason string) string {
	return MapStopReason(&reason)
}
