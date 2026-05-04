package proxy

import (
	"bytes"
	"encoding/json"
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
	if strings.HasPrefix(line, "data: ") {
		return line[6:], true
	}
	return "", false
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

type openAIStreamUsageTap struct {
	pending []byte
	usage   *models.OpenAIUsage
}

func (t *openAIStreamUsageTap) Write(p []byte) (int, error) {
	t.pending = append(t.pending, p...)
	for {
		idx := bytes.IndexByte(t.pending, '\n')
		if idx < 0 {
			break
		}
		line := strings.TrimRight(string(t.pending[:idx]), "\r")
		t.processLine(line)
		t.pending = t.pending[idx+1:]
	}
	return len(p), nil
}

func (t *openAIStreamUsageTap) finish() {
	if len(t.pending) == 0 {
		return
	}
	t.processLine(strings.TrimRight(string(t.pending), "\r"))
	t.pending = nil
}

func (t *openAIStreamUsageTap) processLine(line string) {
	data, ok := parseSSELine(line)
	if !ok || data == "[DONE]" {
		return
	}

	var chunk models.OpenAIStreamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil || chunk.Usage == nil {
		return
	}

	usage := *chunk.Usage
	t.usage = &usage
}

func StreamOpenAIPassthroughWithUsage(w http.ResponseWriter, body io.ReadCloser) (*models.OpenAIUsage, error) {
	defer func() { _ = body.Close() }()
	setSSEHeaders(w)

	fw := &flushWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		fw.flusher = f
	}
	tap := &openAIStreamUsageTap{}
	// Errors here (client disconnect, upstream drop) are unrecoverable for SSE
	// since headers have already been sent. The client must handle truncated streams.
	_, err := io.Copy(fw, io.TeeReader(body, tap))
	tap.finish()
	return tap.usage, err
}

// StreamOpenAIPassthrough streams OpenAI SSE bytes directly to the client with no parsing.
func StreamOpenAIPassthrough(w http.ResponseWriter, body io.ReadCloser) {
	_, _ = StreamOpenAIPassthroughWithUsage(w, body)
}

// StreamOpenAIToAnthropicWithUsage translates an OpenAI SSE stream into
// Anthropic SSE format and returns the final upstream usage when present.
func StreamOpenAIToAnthropicWithUsage(w http.ResponseWriter, body io.ReadCloser, model string, requestID string) *models.OpenAIUsage {
	defer func() { _ = body.Close() }()
	setSSEHeaders(w)

	state := newAnthropicStreamState(w, model, requestID)
	if !state.start() {
		return nil
	}

	sawDone, err := consumeOpenAIStreamChunks(body, state.consumeChunk)
	if err != nil || !sawDone {
		return state.storedUsage
	}

	_ = state.finish()
	return state.storedUsage
}

// StreamOpenAIToAnthropic translates an OpenAI SSE stream into Anthropic SSE format.
func StreamOpenAIToAnthropic(w http.ResponseWriter, body io.ReadCloser, model string, requestID string) {
	_ = StreamOpenAIToAnthropicWithUsage(w, body, model, requestID)
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
		event.Usage = &models.AnthropicUsage{
			InputTokens:  s.storedUsage.PromptTokens,
			OutputTokens: s.storedUsage.CompletionTokens,
		}
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
	switch reason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "content_filter":
		return "end_turn"
	default:
		return reason
	}
}
