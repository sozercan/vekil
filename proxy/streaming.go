package proxy

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sozercan/copilot-proxy/models"
)

func writeSSEEvent(w http.ResponseWriter, eventType string, data interface{}) error {
	b, err := json.Marshal(data)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType, b)
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

// StreamOpenAIPassthrough streams OpenAI SSE bytes directly to the client with no parsing.
func StreamOpenAIPassthrough(w http.ResponseWriter, body io.ReadCloser) {
	defer body.Close()
	setSSEHeaders(w)

	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := scanner.Text()
		fmt.Fprintf(w, "%s\n", line)
		// Flush after blank lines (SSE event delimiter)
		if line == "" {
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}
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
	// Track open tool call blocks by tool call index
	openToolBlocks := make(map[int]bool)

	scanner := bufio.NewScanner(body)
	for scanner.Scan() {
		line := scanner.Text()

		data, ok := parseSSELine(line)
		if !ok {
			continue
		}

		if data == "[DONE]" {
			// Close any open content block
			if textBlockOpen || len(openToolBlocks) > 0 {
				writeSSEEvent(w, "content_block_stop", models.AnthropicStreamEvent{
					Type:  "content_block_stop",
					Index: blockIndex,
				})
			}

			// Send message_delta with stop reason and usage
			delta := &models.AnthropicDelta{Type: "message_delta"}
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
							Index: blockIndex,
							ContentBlock: &models.ContentBlock{
								Type: "text",
								Text: "",
							},
						})
						textBlockOpen = true
					}
					writeSSEEvent(w, "content_block_delta", models.AnthropicStreamEvent{
						Type:  "content_block_delta",
						Index: blockIndex,
						Delta: &models.AnthropicDelta{
							Type: "text_delta",
							Text: text,
						},
					})
				}
			}

			// Handle tool calls
			for _, tc := range choice.Delta.ToolCalls {
				if tc.ID != "" {
					// New tool call — close previous block if open
					if textBlockOpen {
						writeSSEEvent(w, "content_block_stop", models.AnthropicStreamEvent{
							Type:  "content_block_stop",
							Index: blockIndex,
						})
						textBlockOpen = false
						blockIndex++
					} else if len(openToolBlocks) > 0 {
						writeSSEEvent(w, "content_block_stop", models.AnthropicStreamEvent{
							Type:  "content_block_stop",
							Index: blockIndex,
						})
						blockIndex++
					}

					writeSSEEvent(w, "content_block_start", models.AnthropicStreamEvent{
						Type:  "content_block_start",
						Index: blockIndex,
						ContentBlock: &models.ContentBlock{
							Type: "tool_use",
							ID:   tc.ID,
							Name: tc.Function.Name,
						},
					})
					idx := 0
					if tc.Index != nil {
						idx = *tc.Index
					}
					openToolBlocks[idx] = true
				}

				if tc.Function.Arguments != "" {
					writeSSEEvent(w, "content_block_delta", models.AnthropicStreamEvent{
						Type:  "content_block_delta",
						Index: blockIndex,
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
	if textBlockOpen || len(openToolBlocks) > 0 {
		writeSSEEvent(w, "content_block_stop", models.AnthropicStreamEvent{
			Type:  "content_block_stop",
			Index: blockIndex,
		})
	}

	delta := &models.AnthropicDelta{Type: "message_delta"}
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

func convertFinishReason(reason string) string {
	switch reason {
	case "stop":
		return "end_turn"
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	default:
		return reason
	}
}
