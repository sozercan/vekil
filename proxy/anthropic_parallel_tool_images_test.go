package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/models"
)

func validateCompleteImageToolHistory(messages []models.OpenAIMessage) error {
	pending := make(map[string]bool)
	seen := make(map[string]bool)
	for index, message := range messages {
		if message.Role != "tool" && len(pending) != 0 {
			return fmt.Errorf("message %d (%s) interrupts %d pending tool results", index, message.Role, len(pending))
		}
		if message.Role == "tool" {
			if !pending[message.ToolCallID] || seen[message.ToolCallID] {
				return fmt.Errorf("message %d has duplicate or unrequested tool result", index)
			}
			seen[message.ToolCallID] = true
			delete(pending, message.ToolCallID)
			var text string
			if err := json.Unmarshal(message.Content, &text); err != nil {
				return fmt.Errorf("message %d has non-text tool content: %w", index, err)
			}
		}
		for _, call := range message.ToolCalls {
			if call.ID == "" || pending[call.ID] {
				return fmt.Errorf("message %d has missing or duplicate call ID", index)
			}
			pending[call.ID] = true
		}
	}
	if len(pending) != 0 {
		return fmt.Errorf("request ends with %d pending tool results", len(pending))
	}
	return nil
}

func parallelImageToolBody(t *testing.T, split, reversed, stream bool) string {
	t.Helper()
	ids := []string{"call_capture_a", "call_capture_b", "call_capture_c"}
	calls := make([]map[string]any, 0, len(ids))
	for index, id := range ids {
		calls = append(calls, map[string]any{"type": "tool_use", "id": id, "name": "capture", "input": map[string]int{"page": index}})
	}
	messages := []map[string]any{{"role": "assistant", "content": calls}}
	order := slices.Clone(ids)
	if reversed {
		slices.Reverse(order)
	}
	results := make([]map[string]any, 0, len(ids))
	for _, id := range order {
		result := map[string]any{
			"type": "tool_result", "tool_use_id": id,
			"content": []map[string]any{{"type": "image", "source": map[string]string{"type": "url", "url": "https://images.invalid/" + id + ".png"}}},
		}
		if split {
			messages = append(messages, map[string]any{"role": "user", "content": []map[string]any{result}})
		} else {
			results = append(results, result)
		}
	}
	if !split {
		messages = append(messages, map[string]any{"role": "user", "content": results})
	}
	body, err := json.Marshal(map[string]any{"model": "chat-model", "max_tokens": 64, "stream": stream, "messages": messages})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

func TestAnthropicParallelToolResultImagesPreserveChatHistory(t *testing.T) {
	for _, grouping := range []struct {
		name     string
		split    bool
		reversed bool
		captions bool
	}{{name: "grouped"}, {name: "split", split: true}, {name: "split reversed", split: true, reversed: true}, {name: "CLI companion captions", captions: true}} {
		for _, mode := range []string{"messages", "stream", "count"} {
			t.Run(grouping.name+" "+mode, func(t *testing.T) {
				var calls atomic.Int32
				var mu sync.Mutex
				var sequences [][]string
				h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
					calls.Add(1)
					var request models.OpenAIRequest
					if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
						t.Error(err)
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					roles := make([]string, len(request.Messages))
					for i, message := range request.Messages {
						roles[i] = message.Role
					}
					mu.Lock()
					sequences = append(sequences, roles)
					mu.Unlock()
					if err := validateCompleteImageToolHistory(request.Messages); err != nil {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(http.StatusBadRequest)
						_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": err.Error(), "type": "invalid_request_error", "code": "invalid_request_body"}})
						return
					}
					imageCount := 0
					for _, message := range request.Messages {
						var parts []models.OpenAIContentPart
						if json.Unmarshal(message.Content, &parts) == nil {
							for _, part := range parts {
								if part.Type == "image_url" {
									imageCount++
								}
							}
						}
					}
					if imageCount != 3 {
						t.Errorf("forwarded image count = %d, want 3", imageCount)
					}
					if grouping.captions {
						last := request.Messages[len(request.Messages)-1]
						text := jsonRawString(last.Content)
						for page := 1; page <= 3; page++ {
							if last.Role != "user" || !strings.Contains(text, fmt.Sprintf("[Image: synthetic page %d, displayed at 2000x1000]", page)) {
								t.Errorf("CLI caption %d was lost or moved across attachments: %s", page, last.Content)
							}
						}
					}
					writeToolImageContractChatReply(w, request)
				})
				body := parallelImageToolBody(t, grouping.split, grouping.reversed, mode == "stream")
				if grouping.captions {
					var payload models.AnthropicRequest
					if err := json.Unmarshal([]byte(body), &payload); err != nil {
						t.Fatal(err)
					}
					last := &payload.Messages[len(payload.Messages)-1]
					var blocks []models.ContentBlock
					if err := json.Unmarshal(last.Content, &blocks); err != nil {
						t.Fatal(err)
					}
					// Claude Code puts all result blocks before same-turn image
					// companions; transcript row boundaries are not API turns.
					for page := 1; page <= 3; page++ {
						caption := fmt.Sprintf("[Image: synthetic page %d, displayed at 2000x1000]\n", page)
						blocks = append(blocks, models.ContentBlock{Type: "text", Text: &caption})
					}
					encoded, err := json.Marshal(blocks)
					if err != nil {
						t.Fatal(err)
					}
					last.Content = encoded
					encoded, err = json.Marshal(payload)
					if err != nil {
						t.Fatal(err)
					}
					body = string(encoded)
				}
				request := httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(body))
				recorder := httptest.NewRecorder()
				if mode == "count" {
					h.HandleAnthropicMessagesCountTokens(recorder, request)
				} else {
					h.HandleAnthropicMessages(recorder, request)
				}
				mu.Lock()
				gotSequences := slices.Clone(sequences)
				mu.Unlock()
				if recorder.Code != http.StatusOK {
					t.Fatalf("status=%d, submitted role sequences=%v, response=%s", recorder.Code, gotSequences, recorder.Body.String())
				}
				if calls.Load() != 1 {
					t.Fatalf("upstream sends = %d, want exactly 1", calls.Load())
				}
				if mode == "count" && !strings.Contains(recorder.Body.String(), `"input_tokens":23`) {
					t.Fatalf("count response = %s", recorder.Body.String())
				}
			})
		}
	}
}
