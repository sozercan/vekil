package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"

	"github.com/sozercan/copilot-proxy/models"
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

// StreamOpenAIPassthrough streams OpenAI SSE bytes directly to the client with no parsing.
func StreamOpenAIPassthrough(w http.ResponseWriter, body io.ReadCloser) {
	defer body.Close()
	setSSEHeaders(w)

	fw := &flushWriter{w: w}
	if f, ok := w.(http.Flusher); ok {
		fw.flusher = f
	}
	io.Copy(fw, body)
}

// StreamOpenAIToAnthropic translates an OpenAI SSE stream into Anthropic SSE format.
func StreamOpenAIToAnthropic(w http.ResponseWriter, body io.ReadCloser, model string, requestID string) {
	defer body.Close()
	setSSEHeaders(w)

	emitSSE := func(eventType string, data interface{}) bool {
		return writeSSEEvent(w, eventType, data) == nil
	}

	// Send message_start
	if !emitSSE("message_start", models.AnthropicStreamEvent{
		Type: "message_start",
		Message: &models.AnthropicResponse{
			ID:      requestID,
			Type:    "message",
			Role:    "assistant",
			Model:   model,
			Content: []models.ContentBlock{},
			Usage:   models.AnthropicUsage{},
		},
	}) {
		return
	}

	nextBlockIndex := 0
	textBlockIndex := -1
	var storedFinishReason string
	var storedUsage *models.OpenAIUsage
	// Map OpenAI tool call index → Anthropic block index
	toolCallBlockIndex := make(map[int]int)
	openToolCallIndexes := make(map[int]struct{})

	closeTextBlock := func() bool {
		if textBlockIndex < 0 {
			return true
		}
		if !emitSSE("content_block_stop", models.AnthropicStreamEvent{
			Type:  "content_block_stop",
			Index: intVal(textBlockIndex),
		}) {
			return false
		}
		textBlockIndex = -1
		return true
	}

	closeOpenToolBlocks := func() bool {
		if len(openToolCallIndexes) == 0 {
			return true
		}

		blockIndexes := make([]int, 0, len(openToolCallIndexes))
		for tcIdx := range openToolCallIndexes {
			if bi, ok := toolCallBlockIndex[tcIdx]; ok {
				blockIndexes = append(blockIndexes, bi)
			}
		}
		sort.Ints(blockIndexes)

		for _, bi := range blockIndexes {
			if !emitSSE("content_block_stop", models.AnthropicStreamEvent{
				Type:  "content_block_stop",
				Index: intVal(bi),
			}) {
				return false
			}
		}
		clear(openToolCallIndexes)
		return true
	}

	finishMessage := func() bool {
		if !closeTextBlock() {
			return false
		}
		if !closeOpenToolBlocks() {
			return false
		}

		delta := &models.AnthropicDelta{}
		if storedFinishReason != "" {
			delta.StopReason = convertFinishReason(storedFinishReason)
		}
		evt := models.AnthropicStreamEvent{
			Type:  "message_delta",
			Delta: delta,
		}
		if storedUsage != nil {
			evt.Usage = &models.AnthropicUsage{
				InputTokens:  storedUsage.PromptTokens,
				OutputTokens: storedUsage.CompletionTokens,
			}
		}
		if !emitSSE("message_delta", evt) {
			return false
		}
		return emitSSE("message_stop", models.AnthropicStreamEvent{Type: "message_stop"})
	}

	// 1MB buffer to handle large tool call arguments
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for scanner.Scan() {
		line := scanner.Text()

		data, ok := parseSSELine(line)
		if !ok {
			continue
		}

		if data == "[DONE]" {
			_ = finishMessage()
			return
		}

		var chunk models.OpenAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if chunk.Usage != nil {
			storedUsage = chunk.Usage
		}

		for _, choice := range chunk.Choices {
			// Handle text content
			if choice.Delta.Content != nil {
				var text string
				if err := json.Unmarshal(choice.Delta.Content, &text); err == nil && text != "" {
					if !closeOpenToolBlocks() {
						return
					}
					if textBlockIndex < 0 {
						textBlockIndex = nextBlockIndex
						nextBlockIndex++
						if !emitSSE("content_block_start", models.AnthropicStreamEvent{
							Type:  "content_block_start",
							Index: intVal(textBlockIndex),
							ContentBlock: &models.ContentBlock{
								Type: "text",
								Text: "",
							},
						}) {
							return
						}
					}
					if !emitSSE("content_block_delta", models.AnthropicStreamEvent{
						Type:  "content_block_delta",
						Index: intVal(textBlockIndex),
						Delta: &models.AnthropicDelta{
							Type: "text_delta",
							Text: text,
						},
					}) {
						return
					}
				}
			}

			// Handle tool calls
			for _, tc := range choice.Delta.ToolCalls {
				tcIdx := 0
				if tc.Index != nil {
					tcIdx = *tc.Index
				}

				if tc.ID != "" {
					if !closeTextBlock() {
						return
					}

					if _, ok := toolCallBlockIndex[tcIdx]; !ok {
						toolCallBlockIndex[tcIdx] = nextBlockIndex
						nextBlockIndex++
					}

					if _, open := openToolCallIndexes[tcIdx]; !open {
						bi := toolCallBlockIndex[tcIdx]
						if !emitSSE("content_block_start", models.AnthropicStreamEvent{
							Type:  "content_block_start",
							Index: intVal(bi),
							ContentBlock: &models.ContentBlock{
								Type:  "tool_use",
								ID:    tc.ID,
								Name:  tc.Function.Name,
								Input: json.RawMessage(`{}`),
							},
						}) {
							return
						}
						openToolCallIndexes[tcIdx] = struct{}{}
					}
				}

				if tc.Function.Arguments != "" {
					// Only emit deltas for tool calls that still have an open block.
					if bi, ok := toolCallBlockIndex[tcIdx]; ok {
						if _, open := openToolCallIndexes[tcIdx]; !open {
							continue
						}
						if !emitSSE("content_block_delta", models.AnthropicStreamEvent{
							Type:  "content_block_delta",
							Index: intVal(bi),
							Delta: &models.AnthropicDelta{
								Type:        "input_json_delta",
								PartialJSON: tc.Function.Arguments,
							},
						}) {
							return
						}
					}
				}
			}

			if choice.FinishReason != nil {
				storedFinishReason = *choice.FinishReason
			}
		}
	}

	// Unexpected EOF or scanner error: do not emit synthetic success events.
	if scanner.Err() != nil {
		return
	}
}

// aggregateStreamToResponse collects an OpenAI SSE stream into a complete
// OpenAIResponse. This is used when we force streaming to the upstream for
// reliable parallel tool call support, but the client requested non-streaming.
func aggregateStreamToResponse(body io.ReadCloser) (*models.OpenAIResponse, error) {
	defer body.Close()

	type aggregatedChoice struct {
		index        int
		role         string
		content      strings.Builder
		toolCalls    map[int]*models.OpenAIToolCall
		finishReason *string
	}

	var result models.OpenAIResponse
	choicesByIndex := make(map[int]*aggregatedChoice)
	sawDone := false

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		data, ok := parseSSELine(line)
		if !ok {
			continue
		}
		if data == "[DONE]" {
			sawDone = true
			break
		}

		var chunk models.OpenAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}

		if result.ID == "" {
			result.ID = chunk.ID
			result.Object = "chat.completion"
			result.Created = chunk.Created
			result.Model = chunk.Model
		}
		if result.SystemFingerprint == "" && chunk.SystemFingerprint != "" {
			result.SystemFingerprint = chunk.SystemFingerprint
		}

		if chunk.Usage != nil {
			result.Usage = chunk.Usage
		}

		for _, choice := range chunk.Choices {
			aggChoice, ok := choicesByIndex[choice.Index]
			if !ok {
				aggChoice = &aggregatedChoice{
					index:     choice.Index,
					role:      "assistant",
					toolCalls: make(map[int]*models.OpenAIToolCall),
				}
				choicesByIndex[choice.Index] = aggChoice
			}

			if choice.Delta.Role != "" {
				aggChoice.role = choice.Delta.Role
			}

			if choice.Delta.Content != nil {
				var text string
				if err := json.Unmarshal(choice.Delta.Content, &text); err == nil {
					aggChoice.content.WriteString(text)
				}
			}

			for _, tc := range choice.Delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}

				if _, exists := aggChoice.toolCalls[idx]; !exists {
					aggChoice.toolCalls[idx] = &models.OpenAIToolCall{}
				}

				call := aggChoice.toolCalls[idx]
				if tc.ID != "" {
					call.ID = tc.ID
				}
				if tc.Type != "" {
					call.Type = tc.Type
				}
				if tc.Function.Name != "" {
					call.Function.Name = tc.Function.Name
				}

				call.Function.Arguments += tc.Function.Arguments
			}

			if choice.FinishReason != nil {
				finishReason := *choice.FinishReason
				aggChoice.finishReason = &finishReason
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("reading SSE stream: %w", err)
	}
	if !sawDone {
		return nil, fmt.Errorf("stream ended before [DONE]")
	}

	choiceIndexes := make([]int, 0, len(choicesByIndex))
	for idx := range choicesByIndex {
		choiceIndexes = append(choiceIndexes, idx)
	}
	sort.Ints(choiceIndexes)

	for _, choiceIndex := range choiceIndexes {
		aggChoice := choicesByIndex[choiceIndex]
		msg := models.OpenAIMessage{Role: aggChoice.role}
		if aggChoice.content.Len() > 0 {
			c, _ := json.Marshal(aggChoice.content.String())
			msg.Content = c
		}

		if len(aggChoice.toolCalls) > 0 {
			toolIndexes := make([]int, 0, len(aggChoice.toolCalls))
			for toolIndex := range aggChoice.toolCalls {
				toolIndexes = append(toolIndexes, toolIndex)
			}
			sort.Ints(toolIndexes)
			for _, toolIndex := range toolIndexes {
				tc := aggChoice.toolCalls[toolIndex]
				// Validate concatenated arguments are valid JSON.
				if !json.Valid([]byte(tc.Function.Arguments)) {
					tc.Function.Arguments = "{}"
				}
				msg.ToolCalls = append(msg.ToolCalls, *tc)
			}
		}

		result.Choices = append(result.Choices, models.OpenAIChoice{
			Index:        choiceIndex,
			Message:      msg,
			FinishReason: aggChoice.finishReason,
		})
	}

	return &result, nil
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
