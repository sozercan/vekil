package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/models"
)

func TestPolicyChatSafeHeadersQuotaAllowlist(t *testing.T) {
	pastHTTPDate := time.Now().Add(-time.Hour).UTC().Format(http.TimeFormat)
	tests := []struct {
		name   string
		header string
		value  string
		want   bool
	}{
		{name: "retry after zero", header: "Retry-After", value: "0", want: true},
		{name: "retry after past date", header: "Retry-After", value: pastHTTPDate, want: true},
		{name: "retry after malformed", header: "Retry-After", value: "power-provider", want: false},
		{name: "upstream request id omitted", header: "X-Request-ID", value: "opaque-upstream-id", want: false},
		{name: "topology-bearing request id omitted", header: "Request-ID", value: "westus3-power-provider", want: false},
		{name: "standard limit window", header: "RateLimit-Limit", value: "100;w=60, 20;w=1", want: true},
		{name: "standard limit arbitrary parameter", header: "RateLimit-Limit", value: "100;policy=power-provider", want: false},
		{name: "standard remaining", header: "RateLimit-Remaining", value: "42", want: true},
		{name: "standard reset", header: "RateLimit-Reset", value: "0", want: true},
		{name: "openai request limit", header: "X-RateLimit-Limit-Requests", value: "100", want: true},
		{name: "openai token remaining", header: "X-RateLimit-Remaining-Tokens", value: "42", want: true},
		{name: "openai negative token remaining", header: "X-RateLimit-Remaining-Tokens", value: "-36161", want: true},
		{name: "standard negative remaining rejected", header: "RateLimit-Remaining", value: "-1", want: false},
		{name: "openai request reset duration", header: "X-RateLimit-Reset-Requests", value: "250ms", want: true},
		{name: "openai token reset seconds", header: "X-RateLimit-Reset-Tokens", value: "7", want: true},
		{name: "openai invalid numeric", header: "X-RateLimit-Limit-Requests", value: "power-model", want: false},
		{name: "provider model header", header: "X-RateLimit-Model", value: "power-model", want: false},
		{name: "provider policy header", header: "RateLimit-Policy", value: "power-provider", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			src := http.Header{tc.header: []string{tc.value}}
			got := policyChatSafeHeaders(src, "public-model")
			values := got.Values(tc.header)
			if tc.want {
				if len(values) != 1 || values[0] != tc.value {
					t.Fatalf("safe header values = %q, want [%q]", values, tc.value)
				}
			} else if len(values) != 0 {
				t.Fatalf("unsafe header values = %q, want omitted", values)
			}
		})
	}
}

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
		{name: "zero max_tokens rejects thinking", req: &models.AnthropicRequest{MaxTokens: intPtr(0), Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(1024)}}, wantErr: "max_tokens must be greater than 0 when thinking is enabled"},
		{name: "zero max_tokens rejects required tool choice", req: &models.AnthropicRequest{MaxTokens: intPtr(0), ToolChoice: &models.AnthropicToolChoice{Type: "any"}}, wantErr: "max_tokens must be greater than 0 when tool_choice forces tool use"},
		{name: "zero max_tokens rejects named tool choice", req: &models.AnthropicRequest{MaxTokens: intPtr(0), ToolChoice: &models.AnthropicToolChoice{Type: "tool", Name: "lookup"}}, wantErr: "max_tokens must be greater than 0 when tool_choice forces tool use"},
		{name: "enabled thinking requires budget", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled"}}, wantErr: "thinking.budget_tokens is required"},
		{name: "enabled thinking requires minimum budget", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(1023)}}, wantErr: "thinking.budget_tokens must be greater than or equal to 1024"},
		{name: "enabled thinking rejects required tool choice", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(1024)}, ToolChoice: &models.AnthropicToolChoice{Type: "any"}}, wantErr: "thinking is not compatible with forced tool_choice"},
		{name: "enabled thinking rejects named tool choice", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(1024)}, ToolChoice: &models.AnthropicToolChoice{Type: "tool", Name: "lookup"}}, wantErr: "thinking is not compatible with forced tool_choice"},
		{name: "enabled thinking budget must fit total", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(4096)}}, wantErr: "thinking.budget_tokens must be less than max_tokens"},
		{name: "interleaved thinking with tools permits larger budget", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(8192)}, Tools: []models.AnthropicTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}}, headers: http.Header{"Anthropic-Beta": []string{anthropicInterleavedThinkingBeta}}},
		{name: "interleaved thinking with explicit auto permits larger budget", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(8192)}, Tools: []models.AnthropicTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}, ToolChoice: &models.AnthropicToolChoice{Type: "auto"}}, headers: http.Header{"Anthropic-Beta": []string{anthropicInterleavedThinkingBeta}}},
		{name: "streaming interleaved thinking uses budget above positive max tokens", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Stream: true, Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(8192)}, Tools: []models.AnthropicTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}}, headers: http.Header{"Anthropic-Beta": []string{anthropicInterleavedThinkingBeta}}},
		{name: "interleaved thinking without tools does not bypass validation", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(8192)}}, headers: http.Header{"Anthropic-Beta": []string{anthropicInterleavedThinkingBeta}}, wantErr: "thinking.budget_tokens must be less than max_tokens"},
		{name: "interleaved thinking with disabled tools does not bypass validation", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(8192)}, Tools: []models.AnthropicTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}, ToolChoice: &models.AnthropicToolChoice{Type: "none"}}, headers: http.Header{"Anthropic-Beta": []string{anthropicInterleavedThinkingBeta}}, wantErr: "thinking.budget_tokens must be less than max_tokens"},
		{name: "unknown interleaved beta does not bypass validation", req: &models.AnthropicRequest{MaxTokens: intPtr(4096), Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: intPtr(8192)}, Tools: []models.AnthropicTool{{Name: "lookup", InputSchema: json.RawMessage(`{"type":"object"}`)}}}, headers: http.Header{"Anthropic-Beta": []string{"interleaved-thinking-disabled"}}, wantErr: "thinking.budget_tokens must be less than max_tokens"},
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

func TestPrepareAnthropicChatCompletionsRequest_PrewarmStaysNonStreaming(t *testing.T) {
	zero := 0
	req := &models.AnthropicRequest{
		Model:     "claude-sonnet-4",
		MaxTokens: &zero,
		Messages: []models.AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"warm cache"`)},
		},
	}

	prepared, mode, err := prepareAnthropicChatCompletionsRequest(req)
	if err != nil {
		t.Fatalf("prepareAnthropicChatCompletionsRequest: %v", err)
	}
	if mode.clientRequestedStream || mode.forceUpstreamStream || mode.injectedStreamUsage {
		t.Fatalf("prewarm mode = %+v, want non-streaming passthrough", mode)
	}

	var oaiReq models.OpenAIRequest
	if err := json.Unmarshal(prepared, &oaiReq); err != nil {
		t.Fatalf("unmarshal prepared request: %v", err)
	}
	if oaiReq.Stream != nil {
		t.Fatalf("stream = %v, want omitted", *oaiReq.Stream)
	}
	if oaiReq.StreamOptions != nil {
		t.Fatalf("stream_options = %+v, want nil", oaiReq.StreamOptions)
	}
	if oaiReq.MaxTokens == nil || *oaiReq.MaxTokens != 0 {
		t.Fatalf("max_tokens = %v, want 0", oaiReq.MaxTokens)
	}
}

func TestPrepareAnthropicChatCompletionsRequest_InterleavedThinkingPreservesPerResponseLimit(t *testing.T) {
	maxTokens := 4096
	budget := 8192
	req := &models.AnthropicRequest{
		Model:     "claude-opus-4-5",
		MaxTokens: &maxTokens,
		Messages: []models.AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`"use tools"`)},
		},
		Thinking: &models.AnthropicThinking{Type: "enabled", BudgetTokens: &budget},
		Tools: []models.AnthropicTool{{
			Name:        "lookup",
			InputSchema: json.RawMessage(`{"type":"object"}`),
		}},
	}

	prepared, mode, err := prepareAnthropicChatCompletionsRequest(req)
	if err != nil {
		t.Fatalf("prepareAnthropicChatCompletionsRequest: %v", err)
	}
	if !mode.forceUpstreamStream || !mode.injectedStreamUsage {
		t.Fatalf("interleaved thinking mode = %+v, want forced upstream streaming", mode)
	}

	var oaiReq models.OpenAIRequest
	if err := json.Unmarshal(prepared, &oaiReq); err != nil {
		t.Fatalf("unmarshal prepared request: %v", err)
	}
	if oaiReq.Stream == nil || !*oaiReq.Stream {
		t.Fatalf("stream = %v, want true", oaiReq.Stream)
	}
	if oaiReq.MaxCompletionTokens == nil || *oaiReq.MaxCompletionTokens != maxTokens {
		t.Fatalf("max_completion_tokens = %v, want per-response max_tokens %d", oaiReq.MaxCompletionTokens, maxTokens)
	}
	if oaiReq.MaxTokens != nil {
		t.Fatalf("max_tokens = %v, want nil", oaiReq.MaxTokens)
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

func TestAggregateStreamToResponseWithProgress(t *testing.T) {
	tests := []struct {
		name         string
		stream       string
		wantProgress upstreamSemanticProgress
	}{
		{
			name: "role preamble before rate limit remains replay safe",
			stream: "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
				"event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n",
			wantProgress: upstreamProgressAllowedPreamble,
		},
		{
			name: "text before rate limit is semantic progress",
			stream: "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n" +
				"event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n",
			wantProgress: upstreamProgressSemanticOutput,
		},
		{
			name: "tool call before reset is tool progress",
			stream: "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]}}]}\n\n" +
				"event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n",
			wantProgress: upstreamProgressToolActivity,
		},
		{
			name: "malformed event makes progress unknown",
			stream: "data: not-json\n\n" +
				"event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"slow down\"}}\n\n",
			wantProgress: upstreamProgressUnknown,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, progress, err := aggregateStreamToResponseWithProgress(io.NopCloser(strings.NewReader(tt.stream)))
			if err == nil {
				t.Fatal("aggregate error = nil")
			}
			if progress != tt.wantProgress {
				t.Fatalf("progress = %q want %q", progress, tt.wantProgress)
			}
		})
	}
}

func TestInspectOpenAIChatStreamEventUnknownTopLevelIsNotReplaySafe(t *testing.T) {
	result := inspectOpenAIChatStreamEvent("", `{"id":"chat","choices":[],"vendor_tool_progress":{"started":true}}`)
	if result.progress != upstreamProgressUnknown {
		t.Fatalf("progress = %q, want unknown", result.progress)
	}
}

func TestConsumeOpenAIStreamChunksWithProgressEventOnlyIsUnknown(t *testing.T) {
	_, progress, err := consumeOpenAIStreamChunksWithProgress(strings.NewReader("event: vendor.tool.started\n\n"), nil)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	if progress != upstreamProgressUnknown {
		t.Fatalf("progress = %q, want unknown", progress)
	}
}

func TestExplicitRoutePreparedStreamTimeoutFlushesBufferedPreamble(t *testing.T) {
	body := newBlockingSSEReadCloser("data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
	resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}
	prepared := newExplicitRoutePreparedStream(resp, explicitRouteStreamOpenAIChat, responsesPrecommitMaxPeekBytes)
	if _, hasResult, err := prepared.await(context.Background(), context.Background(), 10*time.Millisecond); err != nil || hasResult {
		t.Fatalf("await result has=%v err=%v, want timeout without decision", hasResult, err)
	}
	committed := prepared.commitResponse()
	readDone := make(chan string, 1)
	go func() {
		buf := make([]byte, 256)
		n, _ := committed.Body.Read(buf)
		readDone <- string(buf[:n])
	}()
	select {
	case got := <-readDone:
		if !strings.Contains(got, `"role":"assistant"`) {
			t.Fatalf("flushed prefix = %q", got)
		}
	case <-time.After(250 * time.Millisecond):
		t.Fatal("committed preamble did not flush after timeout")
	}
	_ = committed.Body.Close()
}

func TestInspectOpenAIChatErrorWithEmbeddedProgressIsNotReplaySafe(t *testing.T) {
	result := inspectOpenAIChatStreamEvent("error", `{"error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"late"},"choices":[{"delta":{"content":"partial"}}]}`)
	if result.failure == nil {
		t.Fatal("failure = nil")
	}
	if upstreamProgressAllowsTargetSwitch(result.progress) {
		t.Fatalf("progress = %q unexpectedly replay safe", result.progress)
	}
}
