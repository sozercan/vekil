package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

	// Send message_start
	writeSSEEvent(w, "message_start", models.AnthropicStreamEvent{
		Type: "message_start",
		Message: &models.AnthropicResponse{
			ID:      requestID,
			Type:    "message",
			Role:    "assistant",
			Model:   model,
			Content: []models.ContentBlock{},
			Usage:   models.AnthropicUsage{},
		},
	})

	blockIndex := 0
	textBlockOpen := false
	var storedFinishReason string
	var storedUsage *models.OpenAIUsage
	// Map OpenAI tool call index → Anthropic block index
	toolCallBlockIndex := make(map[int]int)

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
			// Close any open content block
			if textBlockOpen || len(toolCallBlockIndex) > 0 {
				writeSSEEvent(w, "content_block_stop", models.AnthropicStreamEvent{
					Type:  "content_block_stop",
					Index: intVal(blockIndex),
				})
			}

			// Send message_delta with stop reason and usage
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
					OutputTokens: storedUsage.CompletionTokens,
				}
			}
			writeSSEEvent(w, "message_delta", evt)
			writeSSEEvent(w, "message_stop", models.AnthropicStreamEvent{Type: "message_stop"})
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
					if !textBlockOpen {
						writeSSEEvent(w, "content_block_start", models.AnthropicStreamEvent{
							Type:  "content_block_start",
							Index: intVal(blockIndex),
							ContentBlock: &models.ContentBlock{
								Type: "text",
								Text: "",
							},
						})
						textBlockOpen = true
					}
					writeSSEEvent(w, "content_block_delta", models.AnthropicStreamEvent{
						Type:  "content_block_delta",
						Index: intVal(blockIndex),
						Delta: &models.AnthropicDelta{
							Type: "text_delta",
							Text: text,
						},
					})
				}
			}

			// Handle tool calls
			for _, tc := range choice.Delta.ToolCalls {
				tcIdx := 0
				if tc.Index != nil {
					tcIdx = *tc.Index
				}

				if tc.ID != "" {
					// New tool call — close previous block if open
					if textBlockOpen {
						writeSSEEvent(w, "content_block_stop", models.AnthropicStreamEvent{
							Type:  "content_block_stop",
							Index: intVal(blockIndex),
						})
						textBlockOpen = false
						blockIndex++
					} else if len(toolCallBlockIndex) > 0 {
						writeSSEEvent(w, "content_block_stop", models.AnthropicStreamEvent{
							Type:  "content_block_stop",
							Index: intVal(blockIndex),
						})
						blockIndex++
					}

					writeSSEEvent(w, "content_block_start", models.AnthropicStreamEvent{
						Type:  "content_block_start",
						Index: intVal(blockIndex),
						ContentBlock: &models.ContentBlock{
							Type: "tool_use",
							ID:   tc.ID,
							Name: tc.Function.Name,
						},
					})
					toolCallBlockIndex[tcIdx] = blockIndex
				}

				if tc.Function.Arguments != "" {
					targetBlock := blockIndex
					if bi, ok := toolCallBlockIndex[tcIdx]; ok {
						targetBlock = bi
					}
					writeSSEEvent(w, "content_block_delta", models.AnthropicStreamEvent{
						Type:  "content_block_delta",
						Index: intVal(targetBlock),
						Delta: &models.AnthropicDelta{
							Type:        "input_json_delta",
							PartialJSON: tc.Function.Arguments,
						},
					})
				}
			}

			if choice.FinishReason != nil {
				storedFinishReason = *choice.FinishReason
			}
		}
	}

	// After loop: handle case where stream ended without [DONE]
	if textBlockOpen || len(toolCallBlockIndex) > 0 {
		writeSSEEvent(w, "content_block_stop", models.AnthropicStreamEvent{
			Type:  "content_block_stop",
			Index: intVal(blockIndex),
		})
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
			OutputTokens: storedUsage.CompletionTokens,
		}
	}
	writeSSEEvent(w, "message_delta", evt)
	writeSSEEvent(w, "message_stop", models.AnthropicStreamEvent{Type: "message_stop"})
}

// aggregateStreamToResponse collects an OpenAI SSE stream into a complete
// OpenAIResponse. This is used when we force streaming to the upstream for
// reliable parallel tool call support, but the client requested non-streaming.
func aggregateStreamToResponse(body io.ReadCloser) (*models.OpenAIResponse, error) {
	defer body.Close()

	var result models.OpenAIResponse
	var contentBuilder strings.Builder
	toolCalls := make(map[int]*models.OpenAIToolCall)
	var finishReason *string

	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		data, ok := parseSSELine(line)
		if !ok {
			continue
		}
		if data == "[DONE]" {
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

		if chunk.Usage != nil {
			result.Usage = chunk.Usage
		}

		for _, choice := range chunk.Choices {
			if choice.Delta.Content != nil {
				var text string
				if err := json.Unmarshal(choice.Delta.Content, &text); err == nil {
					contentBuilder.WriteString(text)
				}
			}

			for _, tc := range choice.Delta.ToolCalls {
				idx := 0
				if tc.Index != nil {
					idx = *tc.Index
				}

				if _, exists := toolCalls[idx]; !exists {
					toolCalls[idx] = &models.OpenAIToolCall{}
				}

				if tc.ID != "" {
					toolCalls[idx].ID = tc.ID
					toolCalls[idx].Type = tc.Type
					toolCalls[idx].Function.Name = tc.Function.Name
				}

				toolCalls[idx].Function.Arguments += tc.Function.Arguments
			}

			if choice.FinishReason != nil {
				finishReason = choice.FinishReason
			}
		}
	}

	msg := models.OpenAIMessage{Role: "assistant"}
	if contentBuilder.Len() > 0 {
		c, _ := json.Marshal(contentBuilder.String())
		msg.Content = c
	}

	if len(toolCalls) > 0 {
		maxIdx := 0
		for idx := range toolCalls {
			if idx > maxIdx {
				maxIdx = idx
			}
		}
		for i := 0; i <= maxIdx; i++ {
			if tc, ok := toolCalls[i]; ok {
				msg.ToolCalls = append(msg.ToolCalls, *tc)
			}
		}
	}

	result.Choices = []models.OpenAIChoice{{
		Index:        0,
		Message:      msg,
		FinishReason: finishReason,
	}}

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
