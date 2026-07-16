package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/sozercan/vekil/models"
)

func TestPrepareOpenAIChatCompletionsRequest_ForceStreamWithTools(t *testing.T) {
	input := []byte(`{
		"model":"gpt-4.1",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[{"type":"function","function":{"name":"lookup_weather","parameters":{"type":"object"}}}]
	}`)

	prepared, mode := prepareOpenAIChatCompletionsRequest(input)
	if mode.clientRequestedStream {
		t.Fatal("clientRequestedStream = true, want false")
	}
	if !mode.forceUpstreamStream {
		t.Fatal("forceUpstreamStream = false, want true")
	}

	var req models.OpenAIRequest
	if err := json.Unmarshal(prepared, &req); err != nil {
		t.Fatalf("unmarshal prepared request: %v", err)
	}
	if req.Stream == nil || !*req.Stream {
		t.Fatalf("stream = %v, want true", req.Stream)
	}
	if req.StreamOptions == nil || !req.StreamOptions.IncludeUsage {
		t.Fatalf("stream_options = %+v, want include_usage=true", req.StreamOptions)
	}
	if req.ParallelToolCalls == nil || !*req.ParallelToolCalls {
		t.Fatalf("parallel_tool_calls = %v, want true", req.ParallelToolCalls)
	}
}

func TestPrepareOpenAIChatCompletionsRequest_EmptyToolsRemainNonStreaming(t *testing.T) {
	input := []byte(`{
		"model":"gpt-4.1",
		"messages":[{"role":"user","content":"hi"}],
		"tools":[]
	}`)

	prepared, mode := prepareOpenAIChatCompletionsRequest(input)
	if mode.clientRequestedStream {
		t.Fatal("clientRequestedStream = true, want false")
	}
	if mode.forceUpstreamStream {
		t.Fatal("forceUpstreamStream = true, want false")
	}

	var req map[string]json.RawMessage
	if err := json.Unmarshal(prepared, &req); err != nil {
		t.Fatalf("unmarshal prepared request: %v", err)
	}
	if _, ok := req["stream"]; ok {
		t.Fatal("stream present, want omitted")
	}
	if _, ok := req["stream_options"]; ok {
		t.Fatal("stream_options present, want omitted")
	}
	if _, ok := req["parallel_tool_calls"]; ok {
		t.Fatal("parallel_tool_calls present, want omitted")
	}
}

func TestPrepareOpenAIChatCompletionsRequest_ClientStreamGetsIncludeUsage(t *testing.T) {
	input := []byte(`{"model":"gpt-4.1","stream":true,"messages":[{"role":"user","content":"hi"}]}`)

	prepared, mode := prepareOpenAIChatCompletionsRequest(input)
	if !mode.clientRequestedStream {
		t.Fatal("clientRequestedStream = false, want true")
	}
	var req map[string]json.RawMessage
	if err := json.Unmarshal(prepared, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	so, ok := req["stream_options"]
	if !ok {
		t.Fatal("stream_options not injected for client stream")
	}
	if !strings.Contains(string(so), `"include_usage":true`) {
		t.Fatalf("include_usage not set: %s", so)
	}
	if !mode.injectedClientStreamUsage {
		t.Fatal("injectedClientStreamUsage = false, want true when proxy injects include_usage")
	}
}

func TestPrepareOpenAIChatCompletionsRequest_ClientStreamOptionsPreserved(t *testing.T) {
	// Client already set stream_options — we must not clobber it.
	input := []byte(`{"model":"gpt-4.1","stream":true,"stream_options":{"include_usage":false},"messages":[{"role":"user","content":"hi"}]}`)

	prepared, mode := prepareOpenAIChatCompletionsRequest(input)
	var req map[string]json.RawMessage
	if err := json.Unmarshal(prepared, &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !strings.Contains(string(req["stream_options"]), `"include_usage":false`) {
		t.Fatalf("client stream_options was overwritten: %s", req["stream_options"])
	}
	if mode.injectedClientStreamUsage {
		t.Fatal("injectedClientStreamUsage = true, want false when client supplied stream_options")
	}
}

func TestValidateAnthropicMessageTokenLimits(t *testing.T) {
	intPtr := func(value int) *int { return &value }
	tests := []struct {
		name    string
		req     *models.AnthropicRequest
		headers http.Header
		wantErr string
	}{
		{name: "missing max_tokens", req: &models.AnthropicRequest{}, wantErr: "max_tokens is required"},
		{name: "negative max_tokens", req: &models.AnthropicRequest{MaxTokens: intPtr(-1)}, wantErr: "max_tokens must be greater than or equal to 0"},
		{name: "zero max_tokens is valid for non-streaming prewarm", req: &models.AnthropicRequest{MaxTokens: intPtr(0)}},
		{name: "zero max_tokens rejects streaming", req: &models.AnthropicRequest{MaxTokens: intPtr(0), Stream: true}, wantErr: "max_tokens must be greater than 0 when stream is true"},
		{name: "enabled thinking requires budget", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled"}}, wantErr: "thinking.budget_tokens is required"},
		{name: "enabled thinking requires minimum budget", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(1023)}}, wantErr: "thinking.budget_tokens must be greater than or equal to 1024"},
		{name: "enabled thinking budget must fit total", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(4096)}}, wantErr: "thinking.budget_tokens must be less than max_tokens"},
		{name: "interleaved thinking permits larger budget", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(8192)}}, headers: http.Header{"Anthropic-Beta": []string{anthropicInterleavedThinkingBeta}}},
		{name: "unknown interleaved beta does not bypass validation", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(8192)}}, headers: http.Header{"Anthropic-Beta": []string{"interleaved-thinking-disabled"}}, wantErr: "thinking.budget_tokens must be less than max_tokens"},
		{name: "enabled thinking valid", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(1024)}}, wantErr: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAnthropicMessageTokenLimits(tt.req, tt.headers)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validateAnthropicMessageTokenLimits() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validateAnthropicMessageTokenLimits() error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestPrepareAnthropicChatCompletionsRequest_ForcesStreaming(t *testing.T) {
	req := &models.AnthropicRequest{
		Model: "claude-sonnet-4",
		Messages: []models.AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"hello"`)},
		},
		Stream: false,
	}

	prepared, mode, err := prepareAnthropicChatCompletionsRequest(req)
	if err != nil {
		t.Fatalf("prepareAnthropicChatCompletionsRequest: %v", err)
	}
	if mode.clientRequestedStream {
		t.Fatal("clientRequestedStream = true, want false")
	}
	if !mode.forceUpstreamStream {
		t.Fatal("forceUpstreamStream = false, want true")
	}

	var oaiReq models.OpenAIRequest
	if err := json.Unmarshal(prepared, &oaiReq); err != nil {
		t.Fatalf("unmarshal prepared request: %v", err)
	}
	if oaiReq.Stream == nil || !*oaiReq.Stream {
		t.Fatalf("stream = %v, want true", oaiReq.Stream)
	}
	if oaiReq.StreamOptions == nil || !oaiReq.StreamOptions.IncludeUsage {
		t.Fatalf("stream_options = %+v, want include_usage=true", oaiReq.StreamOptions)
	}
}
