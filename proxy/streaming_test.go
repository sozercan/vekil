package proxy

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/copilot-proxy/models"
)

func buildSSEStream(chunks ...string) io.ReadCloser {
	var buf strings.Builder
	for _, c := range chunks {
		buf.WriteString("data: " + c + "\n\n")
	}
	return io.NopCloser(strings.NewReader(buf.String()))
}

type sseEvent struct {
	Event string
	Data  string
}

func parseSSEEvents(body string) []sseEvent {
	var events []sseEvent
	lines := strings.Split(body, "\n")
	var curEvent, curData string
	for _, line := range lines {
		if strings.HasPrefix(line, "event: ") {
			curEvent = strings.TrimPrefix(line, "event: ")
		} else if strings.HasPrefix(line, "data: ") {
			curData = strings.TrimPrefix(line, "data: ")
		} else if line == "" && (curEvent != "" || curData != "") {
			events = append(events, sseEvent{Event: curEvent, Data: curData})
			curEvent = ""
			curData = ""
		}
	}
	return events
}

func mustMarshal(t *testing.T, v interface{}) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json.Marshal failed: %v", err)
	}
	return string(b)
}

func TestStreamOpenAIPassthrough(t *testing.T) {
	input := "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}\n\ndata: [DONE]\n\n"
	body := io.NopCloser(strings.NewReader(input))

	w := httptest.NewRecorder()
	StreamOpenAIPassthrough(w, body)

	result := w.Body.String()

	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}

	if !strings.Contains(result, "data: {\"choices\":[{\"delta\":{\"content\":\"Hello\"}}]}") {
		t.Errorf("output missing first chunk, got:\n%s", result)
	}
	if !strings.Contains(result, "data: {\"choices\":[{\"delta\":{\"content\":\" world\"}}]}") {
		t.Errorf("output missing second chunk, got:\n%s", result)
	}
	if !strings.Contains(result, "data: [DONE]") {
		t.Errorf("output missing [DONE], got:\n%s", result)
	}
}

func TestStreamOpenAIToAnthropic_TextOnly(t *testing.T) {
	stop := "stop"
	idx := 0

	chunk1 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-1",
		Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{
			{Index: idx, Delta: models.OpenAIMessage{Content: json.RawMessage(`"Hello"`)}},
		},
	}
	chunk2 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-1",
		Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{
			{Index: idx, Delta: models.OpenAIMessage{Content: json.RawMessage(`" world"`)}},
		},
	}
	chunk3 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-1",
		Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{
			{Index: idx, Delta: models.OpenAIMessage{}, FinishReason: &stop},
		},
	}
	chunk4 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-1",
		Model: "gpt-4",
		Usage: &models.OpenAIUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}

	body := buildSSEStream(
		mustMarshal(t, chunk1),
		mustMarshal(t, chunk2),
		mustMarshal(t, chunk3),
		mustMarshal(t, chunk4),
		"[DONE]",
	)

	w := httptest.NewRecorder()
	StreamOpenAIToAnthropic(w, body, "claude-3-sonnet", "req-123")

	if ct := w.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want %q", ct, "text/event-stream")
	}

	events := parseSSEEvents(w.Body.String())
	if len(events) == 0 {
		t.Fatalf("no SSE events parsed from output:\n%s", w.Body.String())
	}

	// Verify event sequence
	expectedTypes := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}

	if len(events) != len(expectedTypes) {
		t.Fatalf("got %d events, want %d\nevents: %+v\nraw:\n%s", len(events), len(expectedTypes), events, w.Body.String())
	}

	for i, exp := range expectedTypes {
		if events[i].Event != exp {
			t.Errorf("event[%d].Event = %q, want %q", i, events[i].Event, exp)
		}
	}

	// Verify message_start
	var msgStart models.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(events[0].Data), &msgStart); err != nil {
		t.Fatalf("unmarshal message_start: %v", err)
	}
	if msgStart.Message == nil {
		t.Fatal("message_start has nil message")
	}
	if msgStart.Message.ID != "req-123" {
		t.Errorf("message_start ID = %q, want %q", msgStart.Message.ID, "req-123")
	}
	if msgStart.Message.Model != "claude-3-sonnet" {
		t.Errorf("message_start model = %q, want %q", msgStart.Message.Model, "claude-3-sonnet")
	}

	// Verify content_block_start
	var cbStart models.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(events[1].Data), &cbStart); err != nil {
		t.Fatalf("unmarshal content_block_start: %v", err)
	}
	if cbStart.ContentBlock == nil || cbStart.ContentBlock.Type != "text" {
		t.Errorf("content_block_start type = %v, want text", cbStart.ContentBlock)
	}

	// Verify content_block_delta texts
	var delta1, delta2 models.AnthropicStreamEvent
	json.Unmarshal([]byte(events[2].Data), &delta1)
	json.Unmarshal([]byte(events[3].Data), &delta2)
	if delta1.Delta == nil || delta1.Delta.Text != "Hello" {
		t.Errorf("delta[0] text = %q, want %q", delta1.Delta.Text, "Hello")
	}
	if delta2.Delta == nil || delta2.Delta.Text != " world" {
		t.Errorf("delta[1] text = %q, want %q", delta2.Delta.Text, " world")
	}

	// Verify message_delta has stop_reason
	var msgDelta models.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(events[5].Data), &msgDelta); err != nil {
		t.Fatalf("unmarshal message_delta: %v", err)
	}
	if msgDelta.Delta == nil || msgDelta.Delta.StopReason != "end_turn" {
		t.Errorf("message_delta stop_reason = %q, want %q", msgDelta.Delta.StopReason, "end_turn")
	}
	if msgDelta.Usage == nil || msgDelta.Usage.OutputTokens != 5 {
		t.Errorf("message_delta usage output_tokens = %v, want 5", msgDelta.Usage)
	}
}

func TestStreamOpenAIToAnthropic_ToolCall(t *testing.T) {
	toolCallStop := "tool_calls"
	idx := 0

	chunk1 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-2",
		Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{
			{Index: 0, Delta: models.OpenAIMessage{
				ToolCalls: []models.OpenAIToolCall{
					{ID: "call_abc", Index: &idx, Function: models.OpenAIFunctionCall{Name: "get_weather", Arguments: ""}},
				},
			}},
		},
	}
	chunk2 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-2",
		Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{
			{Index: 0, Delta: models.OpenAIMessage{
				ToolCalls: []models.OpenAIToolCall{
					{Index: &idx, Function: models.OpenAIFunctionCall{Arguments: `{"loc`}},
				},
			}},
		},
	}
	chunk3 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-2",
		Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{
			{Index: 0, Delta: models.OpenAIMessage{
				ToolCalls: []models.OpenAIToolCall{
					{Index: &idx, Function: models.OpenAIFunctionCall{Arguments: `ation":"NYC"}`}},
				},
			}},
		},
	}
	chunk4 := models.OpenAIStreamChunk{
		ID:    "chatcmpl-2",
		Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{
			{Index: 0, Delta: models.OpenAIMessage{}, FinishReason: &toolCallStop},
		},
	}

	body := buildSSEStream(
		mustMarshal(t, chunk1),
		mustMarshal(t, chunk2),
		mustMarshal(t, chunk3),
		mustMarshal(t, chunk4),
		"[DONE]",
	)

	w := httptest.NewRecorder()
	StreamOpenAIToAnthropic(w, body, "claude-3-sonnet", "req-456")

	events := parseSSEEvents(w.Body.String())

	expectedTypes := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
		"message_delta",
		"message_stop",
	}

	if len(events) != len(expectedTypes) {
		t.Fatalf("got %d events, want %d\nevents: %+v\nraw:\n%s", len(events), len(expectedTypes), events, w.Body.String())
	}

	for i, exp := range expectedTypes {
		if events[i].Event != exp {
			t.Errorf("event[%d].Event = %q, want %q", i, events[i].Event, exp)
		}
	}

	// Verify content_block_start has tool_use type
	var cbStart models.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(events[1].Data), &cbStart); err != nil {
		t.Fatalf("unmarshal content_block_start: %v", err)
	}
	if cbStart.ContentBlock == nil {
		t.Fatal("content_block_start has nil content_block")
	}
	if cbStart.ContentBlock.Type != "tool_use" {
		t.Errorf("content_block type = %q, want %q", cbStart.ContentBlock.Type, "tool_use")
	}
	if cbStart.ContentBlock.ID != "call_abc" {
		t.Errorf("content_block id = %q, want %q", cbStart.ContentBlock.ID, "call_abc")
	}
	if cbStart.ContentBlock.Name != "get_weather" {
		t.Errorf("content_block name = %q, want %q", cbStart.ContentBlock.Name, "get_weather")
	}

	// Verify content_block_delta has input_json_delta type
	var delta1 models.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(events[2].Data), &delta1); err != nil {
		t.Fatalf("unmarshal content_block_delta: %v", err)
	}
	if delta1.Delta == nil || delta1.Delta.Type != "input_json_delta" {
		t.Errorf("delta type = %v, want input_json_delta", delta1.Delta)
	}
	if delta1.Delta.PartialJSON != `{"loc` {
		t.Errorf("delta partial_json = %q, want %q", delta1.Delta.PartialJSON, `{"loc`)
	}

	// Verify message_delta has tool_use stop reason
	var msgDelta models.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(events[5].Data), &msgDelta); err != nil {
		t.Fatalf("unmarshal message_delta: %v", err)
	}
	if msgDelta.Delta == nil || msgDelta.Delta.StopReason != "tool_use" {
		t.Errorf("message_delta stop_reason = %q, want %q", msgDelta.Delta.StopReason, "tool_use")
	}
}
