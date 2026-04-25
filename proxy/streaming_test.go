package proxy

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/vekil/models"
)

func buildSSEStream(chunks ...string) io.ReadCloser {
	var buf strings.Builder
	for _, c := range chunks {
		buf.WriteString("data: " + c + "\n\n")
	}
	return io.NopCloser(strings.NewReader(buf.String()))
}

func oversizedSSEPayload() string {
	return strings.Repeat("x", openAIStreamScannerMaxBuffer+32)
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
	if err := json.Unmarshal([]byte(events[2].Data), &delta1); err != nil {
		t.Fatalf("unmarshal first content_block_delta: %v", err)
	}
	if err := json.Unmarshal([]byte(events[3].Data), &delta2); err != nil {
		t.Fatalf("unmarshal second content_block_delta: %v", err)
	}
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
	if msgDelta.Usage.InputTokens != 10 {
		t.Errorf("message_delta usage input_tokens = %v, want 10", msgDelta.Usage.InputTokens)
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
	if string(cbStart.ContentBlock.Input) != "{}" {
		t.Errorf("content_block input = %q, want %q", string(cbStart.ContentBlock.Input), "{}")
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

func TestStreamOpenAIToAnthropic_MultipleToolCalls(t *testing.T) {
	toolCallStop := "tool_calls"
	idx0, idx1 := 0, 1

	// First tool call start
	chunk1 := models.OpenAIStreamChunk{
		ID: "chatcmpl-3", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{
			ToolCalls: []models.OpenAIToolCall{
				{ID: "call_1", Index: &idx0, Function: models.OpenAIFunctionCall{Name: "delegate_task", Arguments: ""}},
			},
		}}},
	}
	// First tool call args
	chunk2 := models.OpenAIStreamChunk{
		ID: "chatcmpl-3", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{
			ToolCalls: []models.OpenAIToolCall{
				{Index: &idx0, Function: models.OpenAIFunctionCall{Arguments: `{"agent":"researcher"}`}},
			},
		}}},
	}
	// Second tool call start
	chunk3 := models.OpenAIStreamChunk{
		ID: "chatcmpl-3", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{
			ToolCalls: []models.OpenAIToolCall{
				{ID: "call_2", Index: &idx1, Function: models.OpenAIFunctionCall{Name: "wait_for_tasks", Arguments: ""}},
			},
		}}},
	}
	// Second tool call args
	chunk4 := models.OpenAIStreamChunk{
		ID: "chatcmpl-3", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{
			ToolCalls: []models.OpenAIToolCall{
				{Index: &idx1, Function: models.OpenAIFunctionCall{Arguments: `{}`}},
			},
		}}},
	}
	// Finish
	chunk5 := models.OpenAIStreamChunk{
		ID: "chatcmpl-3", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{}, FinishReason: &toolCallStop}},
	}

	body := buildSSEStream(
		mustMarshal(t, chunk1), mustMarshal(t, chunk2),
		mustMarshal(t, chunk3), mustMarshal(t, chunk4),
		mustMarshal(t, chunk5), "[DONE]",
	)

	w := httptest.NewRecorder()
	StreamOpenAIToAnthropic(w, body, "claude-opus-4.6-fast", "req-789")
	events := parseSSEEvents(w.Body.String())

	expectedTypes := []string{
		"message_start",
		"content_block_start", // tool_use call_1
		"content_block_delta", // args for call_1
		"content_block_start", // tool_use call_2
		"content_block_delta", // args for call_2
		"content_block_stop",  // close call_1
		"content_block_stop",  // close call_2
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

	// Verify first tool call block
	var cb1 models.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(events[1].Data), &cb1); err != nil {
		t.Fatalf("unmarshal first tool block: %v", err)
	}
	if cb1.ContentBlock == nil || cb1.ContentBlock.Type != "tool_use" || cb1.ContentBlock.ID != "call_1" || cb1.ContentBlock.Name != "delegate_task" {
		t.Errorf("first tool block = %+v, want tool_use call_1 delegate_task", cb1.ContentBlock)
	}
	if cb1.Index == nil || *cb1.Index != 0 {
		t.Errorf("first tool block index = %v, want 0", cb1.Index)
	}

	// Verify second tool call block
	var cb2 models.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(events[3].Data), &cb2); err != nil {
		t.Fatalf("unmarshal second tool block: %v", err)
	}
	if cb2.ContentBlock == nil || cb2.ContentBlock.Type != "tool_use" || cb2.ContentBlock.ID != "call_2" || cb2.ContentBlock.Name != "wait_for_tasks" {
		t.Errorf("second tool block = %+v, want tool_use call_2 wait_for_tasks", cb2.ContentBlock)
	}
	if cb2.Index == nil || *cb2.Index != 1 {
		t.Errorf("second tool block index = %v, want 1", cb2.Index)
	}

	// Verify argument deltas go to correct blocks
	var d1, d2 models.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(events[2].Data), &d1); err != nil {
		t.Fatalf("unmarshal first arg delta: %v", err)
	}
	if err := json.Unmarshal([]byte(events[4].Data), &d2); err != nil {
		t.Fatalf("unmarshal second arg delta: %v", err)
	}
	if d1.Index == nil || *d1.Index != 0 {
		t.Errorf("first arg delta index = %v, want 0", d1.Index)
	}
	if d2.Index == nil || *d2.Index != 1 {
		t.Errorf("second arg delta index = %v, want 1", d2.Index)
	}

	var stop1, stop2 models.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(events[5].Data), &stop1); err != nil {
		t.Fatalf("unmarshal first tool stop: %v", err)
	}
	if err := json.Unmarshal([]byte(events[6].Data), &stop2); err != nil {
		t.Fatalf("unmarshal second tool stop: %v", err)
	}
	if stop1.Index == nil || *stop1.Index != 0 {
		t.Errorf("first tool stop index = %v, want 0", stop1.Index)
	}
	if stop2.Index == nil || *stop2.Index != 1 {
		t.Errorf("second tool stop index = %v, want 1", stop2.Index)
	}

	// Verify stop reason is tool_use
	var msgDelta models.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(events[7].Data), &msgDelta); err != nil {
		t.Fatalf("unmarshal message_delta: %v", err)
	}
	if msgDelta.Delta == nil || msgDelta.Delta.StopReason != "tool_use" {
		t.Errorf("message_delta stop_reason = %q, want %q", msgDelta.Delta.StopReason, "tool_use")
	}
}

func TestStreamOpenAIToAnthropic_InterleavedParallelToolCalls(t *testing.T) {
	toolCallStop := "tool_calls"
	idx0, idx1 := 0, 1

	chunk1 := models.OpenAIStreamChunk{
		ID: "chatcmpl-interleaved", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{
			ToolCalls: []models.OpenAIToolCall{
				{ID: "call_1", Index: &idx0, Function: models.OpenAIFunctionCall{Name: "delegate_task"}},
			},
		}}},
	}
	chunk2 := models.OpenAIStreamChunk{
		ID: "chatcmpl-interleaved", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{
			ToolCalls: []models.OpenAIToolCall{
				{ID: "call_2", Index: &idx1, Function: models.OpenAIFunctionCall{Name: "wait_for_tasks"}},
			},
		}}},
	}
	chunk3 := models.OpenAIStreamChunk{
		ID: "chatcmpl-interleaved", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{
			ToolCalls: []models.OpenAIToolCall{
				{Index: &idx0, Function: models.OpenAIFunctionCall{Arguments: `{"agent":"researcher"}`}},
			},
		}}},
	}
	chunk4 := models.OpenAIStreamChunk{
		ID: "chatcmpl-interleaved", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{
			ToolCalls: []models.OpenAIToolCall{
				{Index: &idx1, Function: models.OpenAIFunctionCall{Arguments: `{"ids":["call_1"]}`}},
			},
		}}},
	}
	chunk5 := models.OpenAIStreamChunk{
		ID: "chatcmpl-interleaved", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{}, FinishReason: &toolCallStop}},
	}

	body := buildSSEStream(
		mustMarshal(t, chunk1),
		mustMarshal(t, chunk2),
		mustMarshal(t, chunk3),
		mustMarshal(t, chunk4),
		mustMarshal(t, chunk5),
		"[DONE]",
	)

	w := httptest.NewRecorder()
	StreamOpenAIToAnthropic(w, body, "claude-opus-4.6-fast", "req-interleaved")
	events := parseSSEEvents(w.Body.String())

	expectedTypes := []string{
		"message_start",
		"content_block_start",
		"content_block_start",
		"content_block_delta",
		"content_block_delta",
		"content_block_stop",
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

	var start1, start2, delta1, delta2, stop1, stop2 models.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(events[1].Data), &start1); err != nil {
		t.Fatalf("unmarshal first start: %v", err)
	}
	if err := json.Unmarshal([]byte(events[2].Data), &start2); err != nil {
		t.Fatalf("unmarshal second start: %v", err)
	}
	if err := json.Unmarshal([]byte(events[3].Data), &delta1); err != nil {
		t.Fatalf("unmarshal first delta: %v", err)
	}
	if err := json.Unmarshal([]byte(events[4].Data), &delta2); err != nil {
		t.Fatalf("unmarshal second delta: %v", err)
	}
	if err := json.Unmarshal([]byte(events[5].Data), &stop1); err != nil {
		t.Fatalf("unmarshal first stop: %v", err)
	}
	if err := json.Unmarshal([]byte(events[6].Data), &stop2); err != nil {
		t.Fatalf("unmarshal second stop: %v", err)
	}

	if start1.Index == nil || *start1.Index != 0 {
		t.Errorf("first start index = %v, want 0", start1.Index)
	}
	if start2.Index == nil || *start2.Index != 1 {
		t.Errorf("second start index = %v, want 1", start2.Index)
	}
	if delta1.Index == nil || *delta1.Index != 0 {
		t.Errorf("first delta index = %v, want 0", delta1.Index)
	}
	if delta2.Index == nil || *delta2.Index != 1 {
		t.Errorf("second delta index = %v, want 1", delta2.Index)
	}
	if stop1.Index == nil || *stop1.Index != 0 {
		t.Errorf("first stop index = %v, want 0", stop1.Index)
	}
	if stop2.Index == nil || *stop2.Index != 1 {
		t.Errorf("second stop index = %v, want 1", stop2.Index)
	}
}

func TestStreamOpenAIToAnthropic_TextAfterToolCall(t *testing.T) {
	stop := "stop"
	idx0 := 0

	chunk1 := models.OpenAIStreamChunk{
		ID: "chatcmpl-tool-text", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{
			ToolCalls: []models.OpenAIToolCall{
				{ID: "call_1", Index: &idx0, Function: models.OpenAIFunctionCall{Name: "get_weather"}},
			},
		}}},
	}
	chunk2 := models.OpenAIStreamChunk{
		ID: "chatcmpl-tool-text", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{
			ToolCalls: []models.OpenAIToolCall{
				{Index: &idx0, Function: models.OpenAIFunctionCall{Arguments: `{"location":"NYC"}`}},
			},
		}}},
	}
	chunk3 := models.OpenAIStreamChunk{
		ID: "chatcmpl-tool-text", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{
			Content: json.RawMessage(`"Done."`),
		}}},
	}
	chunk4 := models.OpenAIStreamChunk{
		ID: "chatcmpl-tool-text", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{}, FinishReason: &stop}},
	}

	body := buildSSEStream(
		mustMarshal(t, chunk1),
		mustMarshal(t, chunk2),
		mustMarshal(t, chunk3),
		mustMarshal(t, chunk4),
		"[DONE]",
	)

	w := httptest.NewRecorder()
	StreamOpenAIToAnthropic(w, body, "claude-opus-4.6-fast", "req-tool-text")
	events := parseSSEEvents(w.Body.String())

	expectedTypes := []string{
		"message_start",
		"content_block_start",
		"content_block_delta",
		"content_block_stop",
		"content_block_start",
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

	var toolStart, toolStop, textStart, textDelta, textStop models.AnthropicStreamEvent
	if err := json.Unmarshal([]byte(events[1].Data), &toolStart); err != nil {
		t.Fatalf("unmarshal tool start: %v", err)
	}
	if err := json.Unmarshal([]byte(events[3].Data), &toolStop); err != nil {
		t.Fatalf("unmarshal tool stop: %v", err)
	}
	if err := json.Unmarshal([]byte(events[4].Data), &textStart); err != nil {
		t.Fatalf("unmarshal text start: %v", err)
	}
	if err := json.Unmarshal([]byte(events[5].Data), &textDelta); err != nil {
		t.Fatalf("unmarshal text delta: %v", err)
	}
	if err := json.Unmarshal([]byte(events[6].Data), &textStop); err != nil {
		t.Fatalf("unmarshal text stop: %v", err)
	}

	if toolStart.Index == nil || *toolStart.Index != 0 {
		t.Errorf("tool start index = %v, want 0", toolStart.Index)
	}
	if toolStop.Index == nil || *toolStop.Index != 0 {
		t.Errorf("tool stop index = %v, want 0", toolStop.Index)
	}
	if textStart.Index == nil || *textStart.Index != 1 {
		t.Errorf("text start index = %v, want 1", textStart.Index)
	}
	if textStart.ContentBlock == nil || textStart.ContentBlock.Type != "text" {
		t.Errorf("text start content block = %+v, want text", textStart.ContentBlock)
	}
	if textDelta.Index == nil || *textDelta.Index != 1 {
		t.Errorf("text delta index = %v, want 1", textDelta.Index)
	}
	if textDelta.Delta == nil || textDelta.Delta.Text != "Done." {
		t.Errorf("text delta = %+v, want Done.", textDelta.Delta)
	}
	if textStop.Index == nil || *textStop.Index != 1 {
		t.Errorf("text stop index = %v, want 1", textStop.Index)
	}
}

func TestConvertFinishReason(t *testing.T) {
	tests := []struct {
		name   string
		input  string
		expect string
	}{
		{"stop", "stop", "end_turn"},
		{"length", "length", "max_tokens"},
		{"tool_calls", "tool_calls", "tool_use"},
		{"content_filter", "content_filter", "end_turn"},
		{"unknown", "unknown_reason", "unknown_reason"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := convertFinishReason(tt.input)
			if got != tt.expect {
				t.Errorf("convertFinishReason(%q) = %q, want %q", tt.input, got, tt.expect)
			}
		})
	}
}

func TestStreamMessageDeltaNoTypeField(t *testing.T) {
	finishReason := "stop"
	body := buildSSEStream(
		`{"id":"1","choices":[{"index":0,"delta":{"content":"Hi"},"finish_reason":null}]}`,
		`{"id":"1","choices":[{"index":0,"delta":{},"finish_reason":"`+finishReason+`"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		"[DONE]",
	)
	w := httptest.NewRecorder()
	StreamOpenAIToAnthropic(w, body, "claude-test", "msg_test")
	events := parseSSEEvents(w.Body.String())

	// Find message_delta event
	var found bool
	for _, evt := range events {
		if evt.Event == "message_delta" {
			found = true
			// Parse the raw JSON to check the delta doesn't have a "type" field
			var raw map[string]json.RawMessage
			if err := json.Unmarshal([]byte(evt.Data), &raw); err != nil {
				t.Fatalf("unmarshal event data: %v", err)
			}
			var delta map[string]json.RawMessage
			if err := json.Unmarshal(raw["delta"], &delta); err != nil {
				t.Fatalf("unmarshal delta: %v", err)
			}
			if _, hasType := delta["type"]; hasType {
				t.Error("message_delta delta should not have a 'type' field")
			}
			if _, hasStopReason := delta["stop_reason"]; !hasStopReason {
				t.Error("message_delta delta should have 'stop_reason' field")
			}
			break
		}
	}
	if !found {
		t.Fatal("did not find message_delta event")
	}
}

// TestAnthropicResponseJSONShape validates the non-streaming response matches
// the exact shape the Anthropic Messages API returns.
func TestAnthropicResponseJSONShape(t *testing.T) {
	resp := &models.OpenAIResponse{
		ID:      "chatcmpl-abc",
		Created: 1000,
		Choices: []models.OpenAIChoice{
			{
				Index:        0,
				Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"Hello"`)},
				FinishReason: strPtr("stop"),
			},
		},
		Usage: &models.OpenAIUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
	}
	got := TranslateOpenAIToAnthropic(resp, "claude-sonnet-4")

	b, err := json.Marshal(got)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	// Parse as generic map to validate exact field presence
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Required fields per Anthropic spec
	requiredFields := []string{"id", "type", "role", "content", "model", "stop_reason", "usage"}
	for _, f := range requiredFields {
		if _, ok := raw[f]; !ok {
			t.Errorf("missing required field %q in response", f)
		}
	}

	// Validate type is "message"
	var typ string
	if err := json.Unmarshal(raw["type"], &typ); err != nil {
		t.Fatalf("unmarshal type: %v", err)
	}
	if typ != "message" {
		t.Errorf("type = %q, want %q", typ, "message")
	}

	// Validate role is "assistant"
	var role string
	if err := json.Unmarshal(raw["role"], &role); err != nil {
		t.Fatalf("unmarshal role: %v", err)
	}
	if role != "assistant" {
		t.Errorf("role = %q, want %q", role, "assistant")
	}

	// Validate content is an array
	var content []json.RawMessage
	if err := json.Unmarshal(raw["content"], &content); err != nil {
		t.Fatalf("content is not an array: %v", err)
	}
	if len(content) != 1 {
		t.Fatalf("expected 1 content block, got %d", len(content))
	}

	// Validate content block shape
	var block map[string]json.RawMessage
	if err := json.Unmarshal(content[0], &block); err != nil {
		t.Fatalf("unmarshal content block: %v", err)
	}
	if _, ok := block["type"]; !ok {
		t.Error("content block missing 'type' field")
	}
	if _, ok := block["text"]; !ok {
		t.Error("text content block missing 'text' field")
	}

	// Validate usage shape
	var usage map[string]json.RawMessage
	if err := json.Unmarshal(raw["usage"], &usage); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	if _, ok := usage["input_tokens"]; !ok {
		t.Error("usage missing 'input_tokens'")
	}
	if _, ok := usage["output_tokens"]; !ok {
		t.Error("usage missing 'output_tokens'")
	}
}

// TestAnthropicStreamEventShapes validates all streaming event types match
// the exact JSON shapes the Anthropic Messages API returns.
func TestAnthropicStreamEventShapes(t *testing.T) {
	stop := "stop"
	chunk1 := models.OpenAIStreamChunk{
		ID: "1", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{
			Index: 0,
			Delta: models.OpenAIMessage{Content: json.RawMessage(`"Hello"`)},
		}},
	}
	chunk2 := models.OpenAIStreamChunk{
		ID: "1", Model: "gpt-4",
		Choices: []models.OpenAIStreamChoice{{
			Index:        0,
			Delta:        models.OpenAIMessage{},
			FinishReason: &stop,
		}},
		Usage: &models.OpenAIUsage{PromptTokens: 10, CompletionTokens: 3, TotalTokens: 13},
	}

	body := buildSSEStream(mustMarshal(t, chunk1), mustMarshal(t, chunk2), "[DONE]")
	w := httptest.NewRecorder()
	StreamOpenAIToAnthropic(w, body, "claude-sonnet-4", "msg_123")
	events := parseSSEEvents(w.Body.String())

	// Validate each event type's JSON shape
	for _, evt := range events {
		var raw map[string]json.RawMessage
		if err := json.Unmarshal([]byte(evt.Data), &raw); err != nil {
			t.Fatalf("event %q: unmarshal failed: %v", evt.Event, err)
		}

		// Every event must have a "type" field
		if _, ok := raw["type"]; !ok {
			t.Errorf("event %q missing 'type' field", evt.Event)
		}

		switch evt.Event {
		case "message_start":
			if _, ok := raw["message"]; !ok {
				t.Error("message_start missing 'message' field")
			}
			var msg map[string]json.RawMessage
			if err := json.Unmarshal(raw["message"], &msg); err != nil {
				t.Fatalf("message_start.message unmarshal failed: %v", err)
			}
			for _, f := range []string{"id", "type", "role", "model", "content", "usage"} {
				if _, ok := msg[f]; !ok {
					t.Errorf("message_start.message missing '%s'", f)
				}
			}

		case "content_block_start":
			if _, ok := raw["index"]; !ok {
				t.Error("content_block_start missing 'index'")
			}
			if _, ok := raw["content_block"]; !ok {
				t.Error("content_block_start missing 'content_block'")
			}

		case "content_block_delta":
			if _, ok := raw["index"]; !ok {
				t.Error("content_block_delta missing 'index'")
			}
			if _, ok := raw["delta"]; !ok {
				t.Error("content_block_delta missing 'delta'")
			}
			// content_block_delta's delta MUST have a "type" field
			var delta map[string]json.RawMessage
			if err := json.Unmarshal(raw["delta"], &delta); err != nil {
				t.Fatalf("content_block_delta.delta unmarshal failed: %v", err)
			}
			if _, ok := delta["type"]; !ok {
				t.Error("content_block_delta.delta missing 'type'")
			}

		case "content_block_stop":
			if _, ok := raw["index"]; !ok {
				t.Error("content_block_stop missing 'index'")
			}

		case "message_delta":
			if _, ok := raw["delta"]; !ok {
				t.Error("message_delta missing 'delta'")
			}
			// message_delta's delta must NOT have a "type" field
			var delta map[string]json.RawMessage
			if err := json.Unmarshal(raw["delta"], &delta); err != nil {
				t.Fatalf("message_delta.delta unmarshal failed: %v", err)
			}
			if _, hasType := delta["type"]; hasType {
				t.Error("message_delta.delta should NOT have 'type' field")
			}
			if _, ok := delta["stop_reason"]; !ok {
				t.Error("message_delta.delta missing 'stop_reason'")
			}

		case "message_stop":
			// Just needs "type"
		}
	}
}

// TestAnthropicRequestDeserialization validates that all official Anthropic API
// request parameters are accepted without error.
func TestAnthropicRequestDeserialization(t *testing.T) {
	// Full request with all known Anthropic parameters
	reqJSON := `{
		"model": "claude-sonnet-4-20250514",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "Hello"}],
		"system": "You are helpful",
		"stream": false,
		"stop_sequences": ["STOP"],
		"temperature": 0.7,
		"top_p": 0.9,
		"top_k": 40,
		"metadata": {"user_id": "user-123"},
		"thinking": {"type": "enabled", "budget_tokens": 2000},
		"service_tier": "auto",
		"inference_geo": "us",
		"output_config": {"effort": "high"},
		"tools": [{"name": "get_weather", "description": "Get weather", "input_schema": {"type": "object"}}],
		"tool_choice": {"type": "auto"}
	}`

	var req models.AnthropicRequest
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		t.Fatalf("failed to deserialize full Anthropic request: %v", err)
	}

	if req.Model != "claude-sonnet-4-20250514" {
		t.Errorf("model = %q", req.Model)
	}
	if req.MaxTokens == nil || *req.MaxTokens != 1024 {
		t.Errorf("max_tokens = %v", req.MaxTokens)
	}
	if req.ServiceTier != "auto" {
		t.Errorf("service_tier = %q, want %q", req.ServiceTier, "auto")
	}
	if req.InferenceGeo != "us" {
		t.Errorf("inference_geo = %q, want %q", req.InferenceGeo, "us")
	}
	if req.OutputConfig == nil || req.OutputConfig.Effort != "high" {
		t.Errorf("output_config.effort = %v", req.OutputConfig)
	}
	if req.Thinking == nil || req.Thinking.Type != "enabled" || req.Thinking.BudgetTokens == nil || *req.Thinking.BudgetTokens != 2000 {
		t.Errorf("thinking = %v", req.Thinking)
	}

	// Verify translation works
	oai, err := TranslateAnthropicToOpenAI(&req)
	if err != nil {
		t.Fatalf("translation error: %v", err)
	}
	if oai.Model != "claude-sonnet-4" {
		t.Errorf("translated model = %q, want %q", oai.Model, "claude-sonnet-4")
	}
}

// TestAnthropicRequestDisabledThinking validates disabled thinking is deserialized correctly.
func TestAnthropicRequestDisabledThinking(t *testing.T) {
	reqJSON := `{
		"model": "claude-sonnet-4",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "Hi"}],
		"thinking": {"type": "disabled"}
	}`
	var req models.AnthropicRequest
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if req.Thinking == nil || req.Thinking.Type != "disabled" {
		t.Errorf("thinking.type = %v, want disabled", req.Thinking)
	}
	if req.Thinking.BudgetTokens != nil {
		t.Errorf("thinking.budget_tokens should be nil for disabled, got %v", *req.Thinking.BudgetTokens)
	}
}

// TestAnthropicRequestAdaptiveThinking validates adaptive thinking is deserialized correctly.
func TestAnthropicRequestAdaptiveThinking(t *testing.T) {
	reqJSON := `{
		"model": "claude-opus-4.6",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "Hi"}],
		"thinking": {"type": "adaptive"}
	}`
	var req models.AnthropicRequest
	if err := json.Unmarshal([]byte(reqJSON), &req); err != nil {
		t.Fatalf("deserialize: %v", err)
	}
	if req.Thinking == nil || req.Thinking.Type != "adaptive" {
		t.Errorf("thinking.type = %v, want adaptive", req.Thinking)
	}
	if req.Thinking.BudgetTokens != nil {
		t.Errorf("thinking.budget_tokens should be nil for adaptive, got %v", *req.Thinking.BudgetTokens)
	}
}

// TestAnthropicErrorShape validates the error response matches Anthropic spec.
func TestAnthropicErrorShape(t *testing.T) {
	w := httptest.NewRecorder()
	writeAnthropicError(w, 400, "invalid_request_error", "test error")

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Must have "type" = "error"
	var typ string
	if err := json.Unmarshal(raw["type"], &typ); err != nil {
		t.Fatalf("type field unmarshal: %v", err)
	}
	if typ != "error" {
		t.Errorf("type = %q, want %q", typ, "error")
	}

	// Must have "error" object with "type" and "message"
	var errObj map[string]string
	if err := json.Unmarshal(raw["error"], &errObj); err != nil {
		t.Fatalf("error field unmarshal: %v", err)
	}
	if errObj["type"] != "invalid_request_error" {
		t.Errorf("error.type = %q, want %q", errObj["type"], "invalid_request_error")
	}
	if errObj["message"] != "test error" {
		t.Errorf("error.message = %q, want %q", errObj["message"], "test error")
	}
}

// TestToolChoiceNone validates "none" tool_choice produces correct OpenAI format.
func TestToolChoiceNone(t *testing.T) {
	req := &models.AnthropicRequest{
		Model:      "claude-sonnet-4",
		MaxTokens:  intPtr(100),
		Messages:   []models.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
		ToolChoice: &models.AnthropicToolChoice{Type: "none"},
	}
	got, err := TranslateAnthropicToOpenAI(req)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var val string
	if err := json.Unmarshal(got.ToolChoice, &val); err != nil {
		t.Fatalf("tool_choice not a string: %v", err)
	}
	if val != "none" {
		t.Errorf("tool_choice = %q, want %q", val, "none")
	}
}

func TestAggregateStreamToResponse_ParallelToolCalls(t *testing.T) {
	idx0, idx1 := 0, 1
	chunks := []models.OpenAIStreamChunk{
		{ID: "c1", Created: 1700000000, Model: "gpt-4", Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{Content: json.RawMessage(`"I'll delegate"`)}}}},
		{ID: "c1", Model: "gpt-4", Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{ID: "call_1", Index: &idx0, Type: "function", Function: models.OpenAIFunctionCall{Name: "delegate_task", Arguments: ""}}}}}}},
		{ID: "c1", Model: "gpt-4", Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{Index: &idx0, Function: models.OpenAIFunctionCall{Arguments: `{"agent":"r"}`}}}}}}},
		{ID: "c1", Model: "gpt-4", Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{ID: "call_2", Index: &idx1, Type: "function", Function: models.OpenAIFunctionCall{Name: "wait", Arguments: ""}}}}}}},
		{ID: "c1", Model: "gpt-4", Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{Index: &idx1, Function: models.OpenAIFunctionCall{Arguments: `{}`}}}}}}},
		{ID: "c1", Model: "gpt-4", Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{}, FinishReason: strPtr("tool_calls")}}, Usage: &models.OpenAIUsage{PromptTokens: 42, CompletionTokens: 17, TotalTokens: 59}},
	}

	body := buildSSEStream(append(
		func() []string {
			var s []string
			for _, c := range chunks {
				s = append(s, mustMarshal(t, c))
			}
			return s
		}(), "[DONE]")...)

	resp, err := aggregateStreamToResponse(body)
	if err != nil {
		t.Fatalf("aggregateStreamToResponse: %v", err)
	}

	if resp.ID != "c1" {
		t.Errorf("ID = %q, want c1", resp.ID)
	}
	if len(resp.Choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(resp.Choices))
	}

	msg := resp.Choices[0].Message
	var text string
	if err := json.Unmarshal(msg.Content, &text); err != nil {
		t.Fatalf("content unmarshal: %v", err)
	}
	if text != "I'll delegate" {
		t.Errorf("content = %q, want 'I'll delegate'", text)
	}

	if len(msg.ToolCalls) != 2 {
		t.Fatalf("expected 2 tool_calls, got %d", len(msg.ToolCalls))
	}
	if msg.ToolCalls[0].ID != "call_1" || msg.ToolCalls[0].Function.Name != "delegate_task" || msg.ToolCalls[0].Function.Arguments != `{"agent":"r"}` {
		t.Errorf("tool_calls[0] = %+v", msg.ToolCalls[0])
	}
	if msg.ToolCalls[1].ID != "call_2" || msg.ToolCalls[1].Function.Name != "wait" || msg.ToolCalls[1].Function.Arguments != `{}` {
		t.Errorf("tool_calls[1] = %+v", msg.ToolCalls[1])
	}

	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "tool_calls" {
		t.Errorf("finish_reason = %v, want tool_calls", resp.Choices[0].FinishReason)
	}
	if resp.Usage == nil || resp.Usage.PromptTokens != 42 || resp.Usage.CompletionTokens != 17 {
		t.Errorf("usage = %+v, want prompt=42 completion=17", resp.Usage)
	}
}

func TestAggregateStreamToResponse_TextOnly(t *testing.T) {
	body := buildSSEStream(
		`{"id":"c1","created":1000,"model":"gpt-4","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		`{"id":"c1","model":"gpt-4","choices":[{"index":0,"delta":{"content":" world"}}]}`,
		`{"id":"c1","model":"gpt-4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":2,"total_tokens":7}}`,
		"[DONE]",
	)

	resp, err := aggregateStreamToResponse(body)
	if err != nil {
		t.Fatalf("aggregateStreamToResponse: %v", err)
	}

	var text string
	if err := json.Unmarshal(resp.Choices[0].Message.Content, &text); err != nil {
		t.Fatalf("content unmarshal: %v", err)
	}
	if text != "Hello world" {
		t.Errorf("content = %q, want 'Hello world'", text)
	}
	if len(resp.Choices[0].Message.ToolCalls) != 0 {
		t.Errorf("expected 0 tool_calls, got %d", len(resp.Choices[0].Message.ToolCalls))
	}
	if *resp.Choices[0].FinishReason != "stop" {
		t.Errorf("finish_reason = %q, want stop", *resp.Choices[0].FinishReason)
	}
}

func TestAggregateStreamToResponse_MultipleChoices(t *testing.T) {
	body := buildSSEStream(
		`{"id":"c1","created":1000,"model":"gpt-4","choices":[{"index":1,"delta":{"content":"Beta"}}]}`,
		`{"id":"c1","model":"gpt-4","choices":[{"index":0,"delta":{"content":"Alpha"}}]}`,
		`{"id":"c1","model":"gpt-4","choices":[{"index":1,"delta":{"content":" one"}}]}`,
		`{"id":"c1","model":"gpt-4","choices":[{"index":0,"delta":{"content":" zero"}}]}`,
		`{"id":"c1","model":"gpt-4","choices":[{"index":1,"delta":{},"finish_reason":"stop"},{"index":0,"delta":{},"finish_reason":"length"}],"usage":{"prompt_tokens":7,"completion_tokens":4,"total_tokens":11}}`,
		"[DONE]",
	)

	resp, err := aggregateStreamToResponse(body)
	if err != nil {
		t.Fatalf("aggregateStreamToResponse: %v", err)
	}

	if len(resp.Choices) != 2 {
		t.Fatalf("expected 2 choices, got %d", len(resp.Choices))
	}

	if resp.Choices[0].Index != 0 {
		t.Fatalf("choice[0].Index = %d, want 0", resp.Choices[0].Index)
	}
	if resp.Choices[1].Index != 1 {
		t.Fatalf("choice[1].Index = %d, want 1", resp.Choices[1].Index)
	}

	var text0, text1 string
	if err := json.Unmarshal(resp.Choices[0].Message.Content, &text0); err != nil {
		t.Fatalf("unmarshal choice[0] content: %v", err)
	}
	if err := json.Unmarshal(resp.Choices[1].Message.Content, &text1); err != nil {
		t.Fatalf("unmarshal choice[1] content: %v", err)
	}

	if text0 != "Alpha zero" {
		t.Errorf("choice[0] content = %q, want %q", text0, "Alpha zero")
	}
	if text1 != "Beta one" {
		t.Errorf("choice[1] content = %q, want %q", text1, "Beta one")
	}

	if resp.Choices[0].FinishReason == nil || *resp.Choices[0].FinishReason != "length" {
		t.Errorf("choice[0] finish_reason = %v, want length", resp.Choices[0].FinishReason)
	}
	if resp.Choices[1].FinishReason == nil || *resp.Choices[1].FinishReason != "stop" {
		t.Errorf("choice[1] finish_reason = %v, want stop", resp.Choices[1].FinishReason)
	}

	if resp.Usage == nil || resp.Usage.PromptTokens != 7 || resp.Usage.CompletionTokens != 4 || resp.Usage.TotalTokens != 11 {
		t.Errorf("usage = %+v, want prompt=7 completion=4 total=11", resp.Usage)
	}
}

func TestAggregateStreamToResponse_InvalidToolArgs(t *testing.T) {
	// Simulate concatenated JSON objects in tool call arguments (LiteLLM bug #20543)
	body := buildSSEStream(
		`{"id":"c1","created":1000,"model":"gpt-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"shell","arguments":""}}]}}]}`,
		`{"id":"c1","model":"gpt-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
		`{"id":"c1","model":"gpt-4","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":\"pwd\"}"}}]}}]}`,
		`{"id":"c1","model":"gpt-4","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		"[DONE]",
	)

	resp, err := aggregateStreamToResponse(body)
	if err != nil {
		t.Fatalf("aggregateStreamToResponse: %v", err)
	}

	if len(resp.Choices[0].Message.ToolCalls) != 1 {
		t.Fatalf("expected 1 tool call, got %d", len(resp.Choices[0].Message.ToolCalls))
	}
	tc := resp.Choices[0].Message.ToolCalls[0]
	// The concatenated arguments are invalid JSON, should be replaced with {}
	if !json.Valid([]byte(tc.Function.Arguments)) {
		t.Errorf("tool call arguments should be valid JSON, got %q", tc.Function.Arguments)
	}
}

func TestAggregateStreamToResponse_ErrorsWithoutDone(t *testing.T) {
	body := buildSSEStream(
		`{"id":"c1","created":1000,"model":"gpt-4","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		`{"id":"c1","model":"gpt-4","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
	)

	if _, err := aggregateStreamToResponse(body); err == nil {
		t.Fatal("expected error when stream ends before [DONE]")
	}
}

func TestAggregateStreamToResponse_ErrorsOnScannerFailure(t *testing.T) {
	body := buildSSEStream(
		`{"id":"c1","created":1000,"model":"gpt-4","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		oversizedSSEPayload(),
		"[DONE]",
	)

	if _, err := aggregateStreamToResponse(body); err == nil {
		t.Fatal("expected error on scanner failure")
	}
}

func TestStreamOpenAIToAnthropic_NoSuccessTailOnScannerFailure(t *testing.T) {
	body := buildSSEStream(
		`{"id":"c1","created":1000,"model":"gpt-4","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
		oversizedSSEPayload(),
		"[DONE]",
	)

	w := httptest.NewRecorder()
	StreamOpenAIToAnthropic(w, body, "claude-sonnet-4", "req-scan-fail")

	events := parseSSEEvents(w.Body.String())
	for _, evt := range events {
		if evt.Event == "message_delta" || evt.Event == "message_stop" {
			t.Fatalf("unexpected terminal success event %q after scanner failure\nraw:\n%s", evt.Event, w.Body.String())
		}
	}
}
