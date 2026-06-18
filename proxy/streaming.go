package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/sozercan/vekil/models"
)

func intVal(i int) *int { return &i }

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

// flushWriter wraps an http.ResponseWriter and flushes after every Write.
type flushWriter struct {
	w       http.ResponseWriter
	flusher http.Flusher
}

func (fw *flushWriter) Write(p []byte) (int, error) {
	n, err := fw.w.Write(p)
	if err == nil && fw.flusher != nil {
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
	defer func() { _ = body.Close() }()
	setSSEHeaders(w)

	var flusher http.Flusher
	if f, ok := w.(http.Flusher); ok {
		flusher = f
	}

	onUsage := firstOpenAIUsageCallback(onUsageCallbacks)
	if onFinalResponse == nil && onUsage == nil {
		_, _ = io.Copy(&flushWriter{w: w, flusher: flusher}, body)
		return
	}

	var aggregator *openAIResponseAggregator
	if onFinalResponse != nil {
		aggregator = newOpenAIResponseAggregator()
	}
	reader := bufio.NewReaderSize(body, openAIStreamScannerInitialBuffer)

	sawDone := false
	var accumulator sseDataAccumulator
	processData := func(_ string, data string) bool {
		if data == "[DONE]" {
			sawDone = true
			return true
		}

		var chunk models.OpenAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return true
		}
		aggregator.addChunk(chunk)
		return true
	}

	for {
		line, err := readOpenAISSELine(reader)
		if len(line) > 0 {
			if _, writeErr := io.WriteString(w, line); writeErr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			accumulator.consumeLine(line, processData)
		}
		if err != nil {
			if err != io.EOF {
				return
			}
			break
		}
	}

	accumulator.dispatch(processData)
	if !sawDone || onFinalResponse == nil || aggregator == nil {
		return
	}
	onFinalResponse(aggregator.buildResponse())
}

func firstOpenAIUsageCallback(callbacks []func(*models.OpenAIUsage)) func(*models.OpenAIUsage) {
	for _, callback := range callbacks {
		if callback != nil {
			return callback
		}
	}
	return nil
}

func streamAnthropicPassthroughBody(w http.ResponseWriter, body io.Reader, publicModel, upstreamModel string) {
	publicModel = strings.TrimSpace(publicModel)
	upstreamModel = strings.TrimSpace(upstreamModel)

	var flusher http.Flusher
	if f, ok := w.(http.Flusher); ok {
		flusher = f
	}

	if publicModel == "" || publicModel == upstreamModel {
		_, _ = io.Copy(&flushWriter{w: w, flusher: flusher}, body)
		return
	}

	reader := bufio.NewReaderSize(body, openAIStreamScannerInitialBuffer)
	frame := make([]string, 0, 4)
	for {
		line, err := readOpenAISSELine(reader)
		if len(line) > 0 {
			frame = append(frame, line)
			if strings.TrimRight(line, "\r\n") == "" {
				writeAnthropicSSEFrame(w, frame, publicModel)
				if flusher != nil {
					flusher.Flush()
				}
				frame = frame[:0]
			}
		}
		if err != nil {
			if len(frame) > 0 {
				writeAnthropicSSEFrame(w, frame, publicModel)
				if flusher != nil {
					flusher.Flush()
				}
			}
			return
		}
	}
}

func writeAnthropicSSEFrame(w io.Writer, frame []string, publicModel string) {
	for _, line := range frame {
		content, ending := splitSSELineEnding(line)
		data, ok := parseSSELine(content)
		if !ok {
			_, _ = io.WriteString(w, line)
			continue
		}

		rewritten, changed := rewriteAnthropicResponseModelJSON([]byte(data), publicModel, "")
		if !changed {
			_, _ = io.WriteString(w, line)
			continue
		}
		_, _ = fmt.Fprintf(w, "data: %s%s", rewritten, ending)
	}
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
	StreamOpenAIToAnthropicWithFinalResponse(w, body, model, requestID, nil)
}

// StreamOpenAIToAnthropicWithFinalResponse translates an OpenAI SSE stream into
// Anthropic SSE format and optionally invokes onFinalResponse with the complete
// aggregated OpenAI response after the translated stream finishes successfully.
func StreamOpenAIToAnthropicWithFinalResponse(
	w http.ResponseWriter,
	body io.ReadCloser,
	model string,
	requestID string,
	onFinalResponse func(*models.OpenAIResponse),
	onUsageCallbacks ...func(*models.OpenAIUsage),
) {
	defer func() { _ = body.Close() }()
	setSSEHeaders(w)

	state := newAnthropicStreamState(w, model, requestID)
	if !state.start() {
		return
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
			if aggregator != nil {
				aggregator.addChunk(chunk)
			}
			if onUsage != nil && chunk.Usage != nil {
				onUsage(chunk.Usage)
			}
		}
		return state.consumeChunk(chunk)
	})
	if err != nil {
		var streamErr *openAIStreamError
		if errors.As(err, &streamErr) {
			state.emitError(streamErr.Error())
			return
		}
		state.emitError(fmt.Sprintf("upstream stream read failed: %v", err))
		return
	}
	if !sawDone {
		state.emitError("upstream stream ended before [DONE]")
		return
	}

	if !state.finish() {
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
	defer func() { _ = body.Close() }()

	aggregator := newOpenAIResponseAggregator()
	sawDone, err := consumeOpenAIStreamChunks(body, func(chunk models.OpenAIStreamChunk) bool {
		aggregator.addChunk(chunk)
		return true
	})
	if err != nil {
		return nil, err
	}
	if !sawDone {
		return nil, fmt.Errorf("stream ended before [DONE]")
	}

	return aggregator.buildResponse(), nil
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
	return writeSSEEvent(s.w, eventType, data) == nil
}

func (s *anthropicStreamState) emitError(message string) bool {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "upstream stream ended unexpectedly"
	}
	return s.emit("error", map[string]interface{}{
		"type": "error",
		"error": map[string]string{
			"type":    "api_error",
			"message": message,
		},
	})
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
	role         string
	content      strings.Builder
	toolCalls    map[int]*models.OpenAIToolCall
	finishReason *string
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

		call.Function.Arguments += toolCall.Function.Arguments
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

	if len(choice.toolCalls) == 0 {
		return message
	}

	toolIndexes := make([]int, 0, len(choice.toolCalls))
	for toolIndex := range choice.toolCalls {
		toolIndexes = append(toolIndexes, toolIndex)
	}
	sort.Ints(toolIndexes)

	for _, toolIndex := range toolIndexes {
		toolCall := choice.toolCalls[toolIndex]
		if !json.Valid([]byte(toolCall.Function.Arguments)) {
			toolCall.Function.Arguments = "{}"
		}
		message.ToolCalls = append(message.ToolCalls, *toolCall)
	}

	return message
}

func convertFinishReason(reason string) string {
	return MapStopReason(&reason)
}
