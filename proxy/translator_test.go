package proxy

import (
	"encoding/json"
	"testing"

	"github.com/sozercan/copilot-proxy/models"
)

func strPtr(s string) *string { return &s }
func intPtr(i int) *int       { return &i }

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
			Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: 1000},
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
		if got.StopReason != "end_turn" {
			t.Errorf("stop_reason = %q, want %q", got.StopReason, "end_turn")
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
		if got.StopReason != "tool_use" {
			t.Errorf("stop_reason = %q, want %q", got.StopReason, "tool_use")
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
		if got.StopReason != "end_turn" {
			t.Errorf("stop_reason = %q, want %q", got.StopReason, "end_turn")
		}
		if got.Content == nil {
			t.Fatal("content should not be nil")
		}
		if len(got.Content) != 0 {
			t.Errorf("expected 0 content blocks, got %d", len(got.Content))
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
