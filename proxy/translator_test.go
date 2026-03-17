package proxy

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/sozercan/copilot-proxy/models"
)

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

func decodeContentParts(t *testing.T, raw json.RawMessage) []models.OpenAIContentPart {
	t.Helper()

	var parts []models.OpenAIContentPart
	if err := json.Unmarshal(raw, &parts); err != nil {
		t.Fatalf("content is not a JSON content-part array: %v", err)
	}
	return parts
}

func TestTranslateAnthropicToOpenAI(t *testing.T) {
	t.Run("simple text message", func(t *testing.T) {
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(100),
			Messages: []models.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`"Hello"`)},
			},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.Messages) != 1 {
			t.Fatalf("expected 1 message, got %d", len(got.Messages))
		}
		if got.Messages[0].Role != "user" {
			t.Errorf("role = %q, want %q", got.Messages[0].Role, "user")
		}
		var text string
		if err := json.Unmarshal(got.Messages[0].Content, &text); err != nil {
			t.Fatalf("content is not a JSON string: %v", err)
		}
		if text != "Hello" {
			t.Errorf("content = %q, want %q", text, "Hello")
		}
	})

	t.Run("text and image content blocks become multimodal content array", func(t *testing.T) {
		content := `[{"type":"text","text":"What is in this screenshot?"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AQID"}}]`
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(100),
			Messages: []models.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(content)},
			},
		}

		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.Messages) != 1 {
			t.Fatalf("expected 1 message, got %d", len(got.Messages))
		}

		parts := decodeContentParts(t, got.Messages[0].Content)
		if len(parts) != 2 {
			t.Fatalf("expected 2 content parts, got %d", len(parts))
		}
		if parts[0].Type != "text" || parts[0].Text == nil || *parts[0].Text != "What is in this screenshot?" {
			t.Fatalf("parts[0] = %#v, want text part", parts[0])
		}
		if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,AQID" {
			t.Fatalf("parts[1] = %#v, want image_url data URL", parts[1])
		}
	})

	t.Run("image-only content block is preserved", func(t *testing.T) {
		content := `[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AQID"}}]`
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(100),
			Messages: []models.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(content)},
			},
		}

		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.Messages) != 1 {
			t.Fatalf("expected 1 message, got %d", len(got.Messages))
		}

		parts := decodeContentParts(t, got.Messages[0].Content)
		if len(parts) != 1 {
			t.Fatalf("expected 1 content part, got %d", len(parts))
		}
		if parts[0].Type != "image_url" || parts[0].ImageURL == nil || parts[0].ImageURL.URL != "data:image/png;base64,AQID" {
			t.Fatalf("parts[0] = %#v, want image_url data URL", parts[0])
		}
	})

	t.Run("unsupported content blocks fail instead of being dropped", func(t *testing.T) {
		content := `[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"AQID"}}]`
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(100),
			Messages: []models.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(content)},
			},
		}

		_, err := TranslateAnthropicToOpenAI(req)
		if err == nil {
			t.Fatal("expected error for unsupported content block")
		}
		if !strings.Contains(err.Error(), `unsupported content block type "document"`) {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("system message as string", func(t *testing.T) {
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(100),
			System:    json.RawMessage(`"You are helpful"`),
			Messages: []models.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`"Hi"`)},
			},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.Messages) != 2 {
			t.Fatalf("expected 2 messages, got %d", len(got.Messages))
		}
		if got.Messages[0].Role != "system" {
			t.Errorf("first message role = %q, want %q", got.Messages[0].Role, "system")
		}
		var sysText string
		if err := json.Unmarshal(got.Messages[0].Content, &sysText); err != nil {
			t.Fatalf("system content not a JSON string: %v", err)
		}
		if sysText != "You are helpful" {
			t.Errorf("system content = %q, want %q", sysText, "You are helpful")
		}
	})

	t.Run("system message as content blocks", func(t *testing.T) {
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(100),
			System:    json.RawMessage(`[{"type":"text","text":"Be helpful"}]`),
			Messages: []models.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`"Hi"`)},
			},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.Messages) < 1 {
			t.Fatal("expected at least 1 message")
		}
		if got.Messages[0].Role != "system" {
			t.Errorf("first message role = %q, want %q", got.Messages[0].Role, "system")
		}
		var sysText string
		if err := json.Unmarshal(got.Messages[0].Content, &sysText); err != nil {
			t.Fatalf("system content not a JSON string: %v", err)
		}
		if sysText != "Be helpful" {
			t.Errorf("system content = %q, want %q", sysText, "Be helpful")
		}
	})

	t.Run("content blocks with tool_use", func(t *testing.T) {
		content := `[{"type":"text","text":"Let me call a tool"},{"type":"tool_use","id":"call_1","name":"get_weather","input":{"city":"NYC"}}]`
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(100),
			Messages: []models.AnthropicMessage{
				{Role: "assistant", Content: json.RawMessage(content)},
			},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.Messages) != 1 {
			t.Fatalf("expected 1 message, got %d", len(got.Messages))
		}
		msg := got.Messages[0]
		if msg.Role != "assistant" {
			t.Errorf("role = %q, want %q", msg.Role, "assistant")
		}
		var text string
		if err := json.Unmarshal(msg.Content, &text); err != nil {
			t.Fatalf("content not a JSON string: %v", err)
		}
		if text != "Let me call a tool" {
			t.Errorf("content = %q, want %q", text, "Let me call a tool")
		}
		if len(msg.ToolCalls) != 1 {
			t.Fatalf("expected 1 tool call, got %d", len(msg.ToolCalls))
		}
		tc := msg.ToolCalls[0]
		if tc.ID != "call_1" {
			t.Errorf("tool call ID = %q, want %q", tc.ID, "call_1")
		}
		if tc.Type != "function" {
			t.Errorf("tool call type = %q, want %q", tc.Type, "function")
		}
		if tc.Function.Name != "get_weather" {
			t.Errorf("function name = %q, want %q", tc.Function.Name, "get_weather")
		}
	})

	t.Run("tool result", func(t *testing.T) {
		content := `[{"type":"tool_result","tool_use_id":"call_1","content":"sunny"}]`
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(100),
			Messages: []models.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(content)},
			},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.Messages) != 1 {
			t.Fatalf("expected 1 message, got %d", len(got.Messages))
		}
		msg := got.Messages[0]
		if msg.Role != "tool" {
			t.Errorf("role = %q, want %q", msg.Role, "tool")
		}
		if msg.ToolCallID != "call_1" {
			t.Errorf("tool_call_id = %q, want %q", msg.ToolCallID, "call_1")
		}
	})

	t.Run("tools mapping", func(t *testing.T) {
		schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(100),
			Messages: []models.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`"Hi"`)},
			},
			Tools: []models.AnthropicTool{
				{Name: "get_weather", Description: "Get weather", InputSchema: schema},
			},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.Tools) != 1 {
			t.Fatalf("expected 1 tool, got %d", len(got.Tools))
		}
		tool := got.Tools[0]
		if tool.Type != "function" {
			t.Errorf("tool type = %q, want %q", tool.Type, "function")
		}
		if tool.Function.Name != "get_weather" {
			t.Errorf("function name = %q, want %q", tool.Function.Name, "get_weather")
		}
		if tool.Function.Description != "Get weather" {
			t.Errorf("function description = %q, want %q", tool.Function.Description, "Get weather")
		}
	})

	t.Run("tool choice mappings", func(t *testing.T) {
		tests := []struct {
			name   string
			tc     *models.AnthropicToolChoice
			expect string
		}{
			{"auto", &models.AnthropicToolChoice{Type: "auto"}, `"auto"`},
			{"any", &models.AnthropicToolChoice{Type: "any"}, `"required"`},
			{"none", &models.AnthropicToolChoice{Type: "none"}, `"none"`},
			{"tool with name", &models.AnthropicToolChoice{Type: "tool", Name: "get_weather"}, `{"function":{"name":"get_weather"},"type":"function"}`},
		}
		for _, tt := range tests {
			t.Run(tt.name, func(t *testing.T) {
				req := &models.AnthropicRequest{
					Model:      "claude-3-opus",
					MaxTokens:  intPtr(100),
					Messages:   []models.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
					ToolChoice: tt.tc,
				}
				got, err := TranslateAnthropicToOpenAI(req)
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				// Compare as parsed JSON to avoid key-order issues
				var gotVal, expectVal interface{}
				if err := json.Unmarshal(got.ToolChoice, &gotVal); err != nil {
					t.Fatalf("unmarshal got: %v", err)
				}
				if err := json.Unmarshal([]byte(tt.expect), &expectVal); err != nil {
					t.Fatalf("unmarshal expect: %v", err)
				}
				gotJSON, _ := json.Marshal(gotVal)
				expectJSON, _ := json.Marshal(expectVal)
				if string(gotJSON) != string(expectJSON) {
					t.Errorf("tool_choice = %s, want %s", gotJSON, expectJSON)
				}
			})
		}
	})

	t.Run("stream flag", func(t *testing.T) {
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(100),
			Stream:    true,
			Messages: []models.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`"Hi"`)},
			},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.Stream == nil || !*got.Stream {
			t.Error("expected Stream=true")
		}
		if got.StreamOptions == nil {
			t.Fatal("expected StreamOptions to be set")
		}
		if !got.StreamOptions.IncludeUsage {
			t.Error("expected StreamOptions.IncludeUsage=true")
		}
	})

	t.Run("thinking/extended thinking", func(t *testing.T) {
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(500),
			Messages: []models.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`"Think hard"`)},
			},
			Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(1000)},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.MaxCompletionTokens == nil || *got.MaxCompletionTokens != 1000 {
			t.Errorf("MaxCompletionTokens = %v, want 1000", got.MaxCompletionTokens)
		}
		if got.MaxTokens != nil {
			t.Errorf("MaxTokens should be nil when thinking is enabled, got %v", *got.MaxTokens)
		}
	})

	t.Run("thinking/disabled", func(t *testing.T) {
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(500),
			Messages: []models.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`"Hi"`)},
			},
			Thinking: &models.AnthropicThinking{Type: "disabled"},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.MaxCompletionTokens != nil {
			t.Errorf("MaxCompletionTokens should be nil when thinking is disabled, got %v", *got.MaxCompletionTokens)
		}
		if got.MaxTokens == nil || *got.MaxTokens != 500 {
			t.Errorf("MaxTokens = %v, want 500", got.MaxTokens)
		}
	})

	t.Run("thinking/adaptive", func(t *testing.T) {
		req := &models.AnthropicRequest{
			Model:     "claude-opus-4-6",
			MaxTokens: intPtr(500),
			Messages: []models.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`"Hi"`)},
			},
			Thinking: &models.AnthropicThinking{Type: "adaptive"},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.MaxCompletionTokens != nil {
			t.Errorf("MaxCompletionTokens should be nil when thinking is adaptive, got %v", *got.MaxCompletionTokens)
		}
		if got.MaxTokens == nil || *got.MaxTokens != 500 {
			t.Errorf("MaxTokens = %v, want 500", got.MaxTokens)
		}
	})

	t.Run("parallel tool calls enabled by default", func(t *testing.T) {
		schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(100),
			Messages:  []models.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
			Tools:     []models.AnthropicTool{{Name: "get_weather", Description: "Get weather", InputSchema: schema}},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ParallelToolCalls == nil || !*got.ParallelToolCalls {
			t.Error("expected ParallelToolCalls=true when tools are present")
		}
	})

	t.Run("parallel tool calls disabled via tool_choice", func(t *testing.T) {
		schema := json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)
		disable := true
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(100),
			Messages:  []models.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
			Tools:     []models.AnthropicTool{{Name: "get_weather", Description: "Get weather", InputSchema: schema}},
			ToolChoice: &models.AnthropicToolChoice{
				Type:                   "auto",
				DisableParallelToolUse: &disable,
			},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ParallelToolCalls == nil || *got.ParallelToolCalls {
			t.Error("expected ParallelToolCalls=false when disable_parallel_tool_use is true")
		}
	})

	t.Run("no parallel tool calls without tools", func(t *testing.T) {
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(100),
			Messages:  []models.AnthropicMessage{{Role: "user", Content: json.RawMessage(`"Hi"`)}},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.ParallelToolCalls != nil {
			t.Error("expected ParallelToolCalls=nil when no tools are present")
		}
	})

	t.Run("thinking and redacted_thinking blocks skipped", func(t *testing.T) {
		content := `[{"type":"thinking","thinking":"deep thought","signature":"sig"},{"type":"redacted_thinking","data":"secret"},{"type":"text","text":"Here is my answer"}]`
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(100),
			Messages: []models.AnthropicMessage{
				{Role: "assistant", Content: json.RawMessage(content)},
			},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(got.Messages) != 1 {
			t.Fatalf("expected 1 message, got %d", len(got.Messages))
		}
		msg := got.Messages[0]
		var text string
		if err := json.Unmarshal(msg.Content, &text); err != nil {
			t.Fatalf("content not a JSON string: %v", err)
		}
		if text != "Here is my answer" {
			t.Errorf("content = %q, want %q", text, "Here is my answer")
		}
		if len(msg.ToolCalls) != 0 {
			t.Errorf("expected 0 tool calls, got %d", len(msg.ToolCalls))
		}
	})

	t.Run("stop sequences", func(t *testing.T) {
		req := &models.AnthropicRequest{
			Model:         "claude-3-opus",
			MaxTokens:     intPtr(100),
			StopSequences: []string{"STOP", "END"},
			Messages: []models.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`"Hi"`)},
			},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		var stops []string
		if err := json.Unmarshal(got.Stop, &stops); err != nil {
			t.Fatalf("stop is not a string array: %v", err)
		}
		if len(stops) != 2 || stops[0] != "STOP" || stops[1] != "END" {
			t.Errorf("stop = %v, want [STOP END]", stops)
		}
	})

	t.Run("max tokens passthrough", func(t *testing.T) {
		req := &models.AnthropicRequest{
			Model:     "claude-3-opus",
			MaxTokens: intPtr(256),
			Messages: []models.AnthropicMessage{
				{Role: "user", Content: json.RawMessage(`"Hi"`)},
			},
		}
		got, err := TranslateAnthropicToOpenAI(req)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.MaxTokens == nil || *got.MaxTokens != 256 {
			t.Errorf("MaxTokens = %v, want 256", got.MaxTokens)
		}
	})
}

func TestTranslateOpenAIToAnthropic(t *testing.T) {
	t.Run("simple text response", func(t *testing.T) {
		resp := &models.OpenAIResponse{
			ID:      "chatcmpl-123",
			Created: 1000,
			Choices: []models.OpenAIChoice{
				{
					Index:        0,
					Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"Hello there"`)},
					FinishReason: strPtr("stop"),
				},
			},
			Usage: &models.OpenAIUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15},
		}
		got := TranslateOpenAIToAnthropic(resp, "claude-3-opus")
		if got.ID != "chatcmpl-123" {
			t.Errorf("ID = %q, want %q", got.ID, "chatcmpl-123")
		}
		if got.Type != "message" {
			t.Errorf("Type = %q, want %q", got.Type, "message")
		}
		if got.Role != "assistant" {
			t.Errorf("Role = %q, want %q", got.Role, "assistant")
		}
		if len(got.Content) != 1 {
			t.Fatalf("expected 1 content block, got %d", len(got.Content))
		}
		if got.Content[0].Type != "text" {
			t.Errorf("content type = %q, want %q", got.Content[0].Type, "text")
		}
		if got.Content[0].Text != "Hello there" {
			t.Errorf("content text = %q, want %q", got.Content[0].Text, "Hello there")
		}
		if got.StopReason == nil || *got.StopReason != "end_turn" {
			t.Errorf("stop_reason = %v, want %q", got.StopReason, "end_turn")
		}
	})

	t.Run("tool calls response", func(t *testing.T) {
		resp := &models.OpenAIResponse{
			ID:      "chatcmpl-456",
			Created: 2000,
			Choices: []models.OpenAIChoice{
				{
					Index: 0,
					Message: models.OpenAIMessage{
						Role: "assistant",
						ToolCalls: []models.OpenAIToolCall{
							{
								ID:   "call_1",
								Type: "function",
								Function: models.OpenAIFunctionCall{
									Name:      "get_weather",
									Arguments: `{"city":"NYC"}`,
								},
							},
						},
					},
					FinishReason: strPtr("tool_calls"),
				},
			},
		}
		got := TranslateOpenAIToAnthropic(resp, "claude-3-opus")
		if len(got.Content) != 1 {
			t.Fatalf("expected 1 content block, got %d", len(got.Content))
		}
		block := got.Content[0]
		if block.Type != "tool_use" {
			t.Errorf("type = %q, want %q", block.Type, "tool_use")
		}
		if block.ID != "call_1" {
			t.Errorf("id = %q, want %q", block.ID, "call_1")
		}
		if block.Name != "get_weather" {
			t.Errorf("name = %q, want %q", block.Name, "get_weather")
		}
		if got.StopReason == nil || *got.StopReason != "tool_use" {
			t.Errorf("stop_reason = %v, want %q", got.StopReason, "tool_use")
		}
	})

	t.Run("multiple tool calls response", func(t *testing.T) {
		resp := &models.OpenAIResponse{
			ID:      "chatcmpl-multi",
			Created: 6000,
			Choices: []models.OpenAIChoice{
				{
					Index: 0,
					Message: models.OpenAIMessage{
						Role:    "assistant",
						Content: json.RawMessage(`"I'll delegate both tasks"`),
						ToolCalls: []models.OpenAIToolCall{
							{
								ID:   "call_1",
								Type: "function",
								Function: models.OpenAIFunctionCall{
									Name:      "delegate_task",
									Arguments: `{"agent":"researcher","prompt":"List 3 pros of Go"}`,
								},
							},
							{
								ID:   "call_2",
								Type: "function",
								Function: models.OpenAIFunctionCall{
									Name:      "delegate_task",
									Arguments: `{"agent":"researcher","prompt":"List 3 cons of Go"}`,
								},
							},
						},
					},
					FinishReason: strPtr("tool_calls"),
				},
			},
		}
		got := TranslateOpenAIToAnthropic(resp, "claude-opus-4.6-fast")
		if len(got.Content) != 3 {
			t.Fatalf("expected 3 content blocks, got %d", len(got.Content))
		}
		if got.Content[0].Type != "text" || got.Content[0].Text != "I'll delegate both tasks" {
			t.Errorf("first block = %+v, want text", got.Content[0])
		}
		if got.Content[1].Type != "tool_use" || got.Content[1].ID != "call_1" || got.Content[1].Name != "delegate_task" {
			t.Errorf("second block = %+v, want tool_use call_1", got.Content[1])
		}
		if got.Content[2].Type != "tool_use" || got.Content[2].ID != "call_2" || got.Content[2].Name != "delegate_task" {
			t.Errorf("third block = %+v, want tool_use call_2", got.Content[2])
		}
		if got.StopReason == nil || *got.StopReason != "tool_use" {
			t.Errorf("stop_reason = %v, want %q", got.StopReason, "tool_use")
		}
	})

	t.Run("mixed text and tool calls", func(t *testing.T) {
		resp := &models.OpenAIResponse{
			ID:      "chatcmpl-789",
			Created: 3000,
			Choices: []models.OpenAIChoice{
				{
					Index: 0,
					Message: models.OpenAIMessage{
						Role:    "assistant",
						Content: json.RawMessage(`"Let me check"`),
						ToolCalls: []models.OpenAIToolCall{
							{
								ID:   "call_2",
								Type: "function",
								Function: models.OpenAIFunctionCall{
									Name:      "search",
									Arguments: `{"q":"test"}`,
								},
							},
						},
					},
					FinishReason: strPtr("tool_calls"),
				},
			},
		}
		got := TranslateOpenAIToAnthropic(resp, "claude-3-opus")
		if len(got.Content) != 2 {
			t.Fatalf("expected 2 content blocks, got %d", len(got.Content))
		}
		if got.Content[0].Type != "text" || got.Content[0].Text != "Let me check" {
			t.Errorf("first block = %+v, want text 'Let me check'", got.Content[0])
		}
		if got.Content[1].Type != "tool_use" || got.Content[1].Name != "search" {
			t.Errorf("second block = %+v, want tool_use 'search'", got.Content[1])
		}
	})

	t.Run("empty choices", func(t *testing.T) {
		resp := &models.OpenAIResponse{
			ID:      "chatcmpl-empty",
			Created: 4000,
			Choices: []models.OpenAIChoice{},
		}
		got := TranslateOpenAIToAnthropic(resp, "claude-3-opus")
		if got.StopReason == nil || *got.StopReason != "end_turn" {
			t.Errorf("stop_reason = %v, want %q", got.StopReason, "end_turn")
		}
		if got.Content == nil {
			t.Fatal("content should not be nil")
		}
		if len(got.Content) != 0 {
			t.Errorf("expected 0 content blocks, got %d", len(got.Content))
		}
	})

	t.Run("empty text content skipped", func(t *testing.T) {
		resp := &models.OpenAIResponse{
			ID:      "chatcmpl-empty-text",
			Created: 7000,
			Choices: []models.OpenAIChoice{
				{
					Index:        0,
					Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`""`)},
					FinishReason: strPtr("stop"),
				},
			},
		}
		got := TranslateOpenAIToAnthropic(resp, "claude-3-opus")
		if len(got.Content) != 0 {
			t.Errorf("expected 0 content blocks for empty text, got %d", len(got.Content))
		}
	})

	t.Run("whitespace-only text content skipped", func(t *testing.T) {
		resp := &models.OpenAIResponse{
			ID:      "chatcmpl-ws-text",
			Created: 7001,
			Choices: []models.OpenAIChoice{
				{
					Index:        0,
					Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"   \n\t  "`)},
					FinishReason: strPtr("stop"),
				},
			},
		}
		got := TranslateOpenAIToAnthropic(resp, "claude-3-opus")
		if len(got.Content) != 0 {
			t.Errorf("expected 0 content blocks for whitespace text, got %d", len(got.Content))
		}
	})

	t.Run("usage mapping", func(t *testing.T) {
		resp := &models.OpenAIResponse{
			ID:      "chatcmpl-usage",
			Created: 5000,
			Choices: []models.OpenAIChoice{
				{
					Index:        0,
					Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)},
					FinishReason: strPtr("stop"),
				},
			},
			Usage: &models.OpenAIUsage{PromptTokens: 42, CompletionTokens: 17, TotalTokens: 59},
		}
		got := TranslateOpenAIToAnthropic(resp, "claude-3-opus")
		if got.Usage.InputTokens != 42 {
			t.Errorf("InputTokens = %d, want 42", got.Usage.InputTokens)
		}
		if got.Usage.OutputTokens != 17 {
			t.Errorf("OutputTokens = %d, want 17", got.Usage.OutputTokens)
		}
	})
}

func TestMapStopReason(t *testing.T) {
	tests := []struct {
		name   string
		input  *string
		expect string
	}{
		{"stop", strPtr("stop"), "end_turn"},
		{"length", strPtr("length"), "max_tokens"},
		{"tool_calls", strPtr("tool_calls"), "tool_use"},
		{"content_filter", strPtr("content_filter"), "end_turn"},
		{"nil", nil, "end_turn"},
		{"empty string", strPtr(""), "end_turn"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MapStopReason(tt.input)
			if got != tt.expect {
				t.Errorf("MapStopReason(%v) = %q, want %q", tt.input, got, tt.expect)
			}
		})
	}
}
