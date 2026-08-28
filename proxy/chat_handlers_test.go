package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func TestDecodeInternedOpenAIChatRequestModelRawReturnsStableStrings(t *testing.T) {
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:             "local-openai",
			Type:           string(providerTypeOpenAICompatible),
			Default:        true,
			BaseURL:        "http://upstream.test/v1",
			AuthType:       string(providerAuthTypeNone),
			ModelDiscovery: string(providerModelDiscoveryStatic),
			Models: []ProviderModelConfig{{
				PublicID: "claude-sonnet-4.5",
			}},
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	for _, tt := range []struct {
		name string
		raw  string
		want string
	}{
		{name: "exact configured id", raw: `"claude-sonnet-4.5"`, want: "claude-sonnet-4.5"},
		{name: "trimmed configured id", raw: `"  claude-sonnet-4.5  "`, want: "claude-sonnet-4.5"},
		{name: "normalized alias fallback", raw: `"claude-sonnet-4-5"`, want: "claude-sonnet-4-5"},
		{name: "escaped id fallback", raw: `"claude-sonnet-4\u002e5"`, want: "claude-sonnet-4.5"},
		{name: "unknown id fallback", raw: `"unknown-model"`, want: "unknown-model"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			raw := []byte(tt.raw)
			got := handler.decodeInternedRequestModelRaw(raw)
			for i := range raw {
				raw[i] = 'x'
			}
			if got != tt.want {
				t.Fatalf("decoded model = %q after source mutation, want %q", got, tt.want)
			}
		})
	}
}

func TestHandleAnthropicMessages_CopilotNativeMessages(t *testing.T) {
	t.Run("a model advertising native Messages preserves the Anthropic request", func(t *testing.T) {
		var modelFetches, messagesPosts, chatPosts int
		var upstreamReq models.AnthropicRequest
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			switch {
			case r.Method == http.MethodGet && r.URL.Path == providerEndpointModels:
				modelFetches++
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"claude-sonnet-5","supported_endpoints":["/chat/completions","/v1/messages"]},{"id":"claude-chat-only","supported_endpoints":["/chat/completions"]},{"id":"claude-no-endpoints"}]}`)
			case r.Method == http.MethodPost && r.URL.Path == providerEndpointMessages:
				messagesPosts++
				if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
					t.Errorf("decode upstream request: %v", err)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"msg-native","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"<block>no</block>"}],"stop_reason":"stop_sequence","stop_sequence":"</block>","usage":{"input_tokens":12,"output_tokens":3}}`)
			case r.Method == http.MethodPost && r.URL.Path == providerEndpointChatCompletions:
				chatPosts++
				http.Error(w, "unexpected Chat translation", http.StatusInternalServerError)
			default:
				http.NotFound(w, r)
			}
		}))
		defer upstream.Close()

		handler, err := NewProxyHandler(
			auth.NewTestAuthenticator("test-token"),
			logger.NewWithWriter(logger.LevelError, io.Discard),
			WithCopilotBaseURL(upstream.URL),
		)
		if err != nil {
			t.Fatalf("NewProxyHandler() error = %v", err)
		}

		req := httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
			"model":"claude-sonnet-5",
			"messages":[{"role":"user","content":"Classify this action"}],
			"max_tokens":64,
			"stream":false,
			"stop_sequences":["</block>"],
			"thinking":{"type":"disabled"}
		}`))
		req.Header.Set("Anthropic-Version", "2023-06-01")
		w := httptest.NewRecorder()

		handler.HandleAnthropicMessages(w, req)

		if modelFetches != 1 || messagesPosts != 1 || chatPosts != 0 {
			t.Fatalf("upstream requests models/messages/chat = %d/%d/%d, want 1/1/0", modelFetches, messagesPosts, chatPosts)
		}
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
		}

		owner, known := handler.providerSetup().lookupModel("claude-sonnet-5")
		if !known {
			t.Fatal("claude-sonnet-5 was not loaded from the Copilot catalog")
		}
		if !supportsEndpoint(owner.supportedEndpoints, providerEndpointMessages) {
			t.Fatalf("discovered endpoints = %v, want /v1/messages", owner.supportedEndpoints)
		}
		if !handler.shouldForwardAnthropicMessagesDirect("claude-sonnet-5") {
			t.Fatal("Copilot model advertising /v1/messages did not select direct forwarding")
		}
		if handler.shouldForwardAnthropicMessagesDirect("claude-chat-only") {
			t.Fatal("Chat-only Copilot model selected direct Messages forwarding")
		}
		if handler.shouldForwardAnthropicMessagesDirect("claude-no-endpoints") {
			t.Fatal("Copilot model without advertised endpoints selected direct Messages forwarding")
		}
		if handler.shouldForwardAnthropicMessagesDirect("claude-unknown") {
			t.Fatal("unknown Copilot model selected direct Messages forwarding")
		}
		if upstreamReq.Stream {
			t.Fatal("upstream stream = true, want the client's non-streaming request preserved")
		}
		if upstreamReq.Thinking == nil || upstreamReq.Thinking.Type != "disabled" {
			t.Fatalf("upstream thinking = %+v, want disabled", upstreamReq.Thinking)
		}
		if len(upstreamReq.StopSequences) != 1 || upstreamReq.StopSequences[0] != "</block>" {
			t.Fatalf("upstream stop_sequences = %v, want [</block>]", upstreamReq.StopSequences)
		}

		var response models.AnthropicResponse
		if err := json.NewDecoder(w.Body).Decode(&response); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if response.StopReason == nil || *response.StopReason != "stop_sequence" || response.StopSequence == nil || *response.StopSequence != "</block>" {
			t.Fatalf("response stop reason/sequence = %v/%v, want stop_sequence/</block>", response.StopReason, response.StopSequence)
		}
	})
}

func TestHandleAnthropicMessagesRejectsDuplicateEffortEndingInNull(t *testing.T) {
	handler := newTestProxyHandler(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("backend should not be called for invalid output_config.effort")
	})
	req := httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"claude-sonnet-4",
		"messages":[{"role":"user","content":"hello"}],
		"max_tokens":64,
		"output_config":{"effort":"high","effort":null}
	}`))
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessages(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "output_config.effort must be a non-empty string") {
		t.Fatalf("body = %q, want effort validation error", w.Body.String())
	}
}

func TestHandleAnthropicMessagesDetachesBorrowedDirectBody(t *testing.T) {
	const requestBody = `{"model":"claude-public","messages":[{"role":"user","content":"hello"}],"max_tokens":64}`
	const responseBody = `{"id":"msg","type":"message","role":"assistant","model":"claude-public","content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

	retainedBody := make(chan io.ReadCloser, 1)
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:       "native",
			Type:     string(providerTypeAnthropicCompatible),
			Default:  true,
			BaseURL:  "http://upstream.test",
			AuthType: string(providerAuthTypeNone),
			Models: []ProviderModelConfig{{
				PublicID:  "claude-public",
				Endpoints: []string{providerEndpointMessages},
			}},
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.client = &http.Client{Transport: handlerTestRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		retainedBody <- req.Body
		return &http.Response{
			StatusCode:    http.StatusOK,
			Header:        http.Header{"Content-Type": []string{"application/json"}},
			Body:          io.NopCloser(strings.NewReader(responseBody)),
			ContentLength: int64(len(responseBody)),
			Request:       req,
		}, nil
	})}

	req := httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(requestBody))
	w := httptest.NewRecorder()
	handler.HandleAnthropicMessages(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", w.Code, w.Body.String())
	}

	upstreamBody := <-retainedBody
	got, err := io.ReadAll(upstreamBody)
	_ = upstreamBody.Close()
	if err != nil {
		t.Fatalf("read retained upstream body: %v", err)
	}
	if string(got) != requestBody {
		t.Fatalf("retained upstream body = %q, want %q", got, requestBody)
	}
}

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

func TestRecognizedPolicyOpenAIStreamChunk(t *testing.T) {
	tests := []struct {
		name      string
		eventType string
		data      string
		want      bool
	}{
		{name: "content", data: `{"object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":null}]}`, want: true},
		{name: "finish", eventType: "chat.completion.chunk", data: `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`, want: true},
		{name: "usage only", data: `{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`, want: true},
		{name: "foundry prompt filter", data: `{"id":"","object":"","created":0,"model":"","prompt_filter_results":[{"prompt_index":0,"content_filter_results":{}}],"choices":[],"usage":null}`, want: true},
		{name: "foundry prompt annotations", data: `{"id":"","object":"","created":0,"model":"","prompt_annotations":[{"prompt_index":0,"content_filter_results":{}}],"choices":[],"usage":null}`, want: true},
		{name: "OpenAI moderation", data: `{"id":"chat","object":"chat.completion.chunk","created":1,"model":"gpt","choices":[],"usage":null,"moderation":{"input":{"type":"moderation_results","model":"omni-moderation-latest","results":[]},"output":{"type":"moderation_results","model":"omni-moderation-latest","results":[]}}}`, want: true},
		{name: "malformed OpenAI moderation", data: `{"id":"chat","object":"chat.completion.chunk","created":1,"model":"gpt","choices":[],"usage":null,"moderation":{}}`},
		{name: "malformed foundry envelope", data: `{"object":[],"prompt_filter_results":[{}],"choices":[],"usage":null}`},
		{name: "empty object", data: `{}`},
		{name: "null choices", data: `{"choices":null}`},
		{name: "empty choices without usage", data: `{"choices":[]}`},
		{name: "empty choice", data: `{"choices":[{}]}`},
		{name: "missing choice index", data: `{"choices":[{"delta":{"content":"ok"}}]}`},
		{name: "null choice index", data: `{"choices":[{"index":null,"delta":{"content":"ok"}}]}`},
		{name: "case-folded choice index alias", data: `{"choices":[{"index":0,"Index":1,"delta":{"content":"ok"}}]}`},
		{name: "case-folded finish alias", data: `{"choices":[{"index":0,"delta":{"content":"ok"},"finish_reason":"stop","Finish_Reason":null}]}`},
		{name: "case-folded delta alias", data: `{"choices":[{"index":0,"delta":{"content":null,"Content":"late"},"finish_reason":"stop"}]}`},
		{name: "duplicate choice delta", data: `{"choices":[{"index":0,"delta":{"content":"late"},"delta":{},"finish_reason":"stop"}]}`},
		{name: "duplicate delta content", data: `{"choices":[{"index":0,"delta":{"content":"late","content":null},"finish_reason":"stop"}]}`},
		{name: "nonzero choice index", data: `{"choices":[{"index":1,"delta":{"content":"ok"}}]}`},
		{name: "unknown event", eventType: "vendor.chunk", data: `{"choices":[{"index":0,"delta":{"content":"ok"}}]}`},
		{name: "wrong object", data: `{"object":"vendor.chunk","choices":[{"index":0,"delta":{"content":"ok"}}]}`},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := recognizedPolicyOpenAIStreamChunk(tc.eventType, tc.data); got != tc.want {
				t.Fatalf("recognizedPolicyOpenAIStreamChunk() = %v, want %v", got, tc.want)
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

func TestParseOpenAIChatCompletionsModePreservesCaseInsensitiveJSONFields(t *testing.T) {
	input := []byte(`{"STREAM":true,"STREAM_OPTIONS":{"INCLUDE_USAGE":true},"TOOLS":[{"type":"function"}]}`)
	mode := parseOpenAIChatCompletionsMode(input)
	if !mode.clientRequestedStream {
		t.Fatal("clientRequestedStream = false, want true")
	}
	if !mode.clientRequestedStreamUsage {
		t.Fatal("clientRequestedStreamUsage = false, want true")
	}
	if !mode.requestHasTools {
		t.Fatal("requestHasTools = false, want true")
	}
	if mode.forceUpstreamStream {
		t.Fatal("forceUpstreamStream = true, want false for client streaming")
	}
}

func TestParseOpenAIChatCompletionsModePreservesUnicodeCaseFoldedJSONFields(t *testing.T) {
	input := []byte(`{"ſtream":true,"ſtream_options":{"include_uſage":true},"toolſ":[{"type":"function"}]}`)
	if _, ok := parseOpenAIChatCompletionsModeFast(input); ok {
		t.Fatal("fast parser accepted Unicode case-folded fields")
	}
	mode := parseOpenAIChatCompletionsMode(input)
	if !mode.clientRequestedStream {
		t.Fatal("clientRequestedStream = false, want true")
	}
	if !mode.clientRequestedStreamUsage {
		t.Fatal("clientRequestedStreamUsage = false, want true")
	}
	if !mode.requestHasTools {
		t.Fatal("requestHasTools = false, want true")
	}
	if mode.forceUpstreamStream {
		t.Fatal("forceUpstreamStream = true, want false for client streaming")
	}
}

func TestParseOpenAIChatCompletionsModePreservesDuplicateTypeErrorSemantics(t *testing.T) {
	tests := []string{
		`{"stream":1,"stream":true,"messages":[{"role":"user","content":"hi"}]}`,
		`{"stream":true,"stream_options":1,"stream_options":{"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`,
		`{"stream":true,"stream_options":{"include_usage":1,"include_usage":true},"messages":[{"role":"user","content":"hi"}]}`,
		`{"tools":1,"tools":[{"type":"function"}],"messages":[{"role":"user","content":"hi"}]}`,
	}
	for _, input := range tests {
		input := []byte(input)
		if _, ok := parseOpenAIChatCompletionsModeFast(input); ok {
			t.Fatalf("fast parser accepted duplicate tracked fields: %s", input)
		}
		got := parseOpenAIChatCompletionsMode(input)
		want := parseOpenAIChatCompletionsModeWithJSON(input)
		if got != want {
			t.Fatalf("mode = %#v, want encoding/json result %#v for %s", got, want, input)
		}
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

// A carrier is vekil's own state and Anthropic is a different provider. Carrier extraction sits
// AFTER the direct-forward early return, so a client that keeps our thinking block and switches
// to a natively-Anthropic model used to forward Copilot's reasoning ciphertext -- and a
// signature Anthropic never issued -- straight to Anthropic.
func TestHandleAnthropicMessagesDirectStripsVekilCarriers(t *testing.T) {
	var raw json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == providerEndpointModels:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"claude-sonnet-5","supported_endpoints":["/chat/completions","/v1/messages"]}]}`)
		case r.Method == http.MethodPost && r.URL.Path == providerEndpointMessages:
			body, _ := io.ReadAll(r.Body)
			raw = json.RawMessage(body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"msg-native","type":"message","role":"assistant","model":"claude-sonnet-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithCopilotBaseURL(upstream.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	// One assistant turn holding a carrier beside real content, and one holding only a carrier
	// -- the orphan shape a client produces when it splits a parallel group.
	body := `{"model":"claude-sonnet-5","max_tokens":64,"Messages":[
		{"role":"user","content":"go"},
		{"Role":"assistant","Content":[
			{"type":"thinking","Type":"text","thinking":"","signature":"vekil1.OPAQUECARRIERPAYLOAD","Signature":"anthropic-native-sig"},
			{"type":"text","text":"working"}]},
		{"role":"user","content":"again"},
		{"Role":"assistant","Content":[{"type":"thinking","thinking":"","signature":"vekil1.SECONDCARRIER"}]},
		{"role":"user","content":"and again"}]}`
	req := httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(body))
	req.Header.Set("Anthropic-Version", "2023-06-01")
	rec := httptest.NewRecorder()
	handler.HandleAnthropicMessages(rec, req)

	if !handler.shouldForwardAnthropicMessagesDirect("claude-sonnet-5") {
		t.Fatal("fixture did not select direct forwarding; this test would prove nothing")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if len(raw) == 0 {
		t.Fatal("upstream never received the forwarded request")
	}
	if strings.Contains(string(raw), reasoningCarrierPrefix) {
		t.Fatalf("a vekil carrier reached Anthropic: %s", string(raw))
	}
	// The turn that had real content keeps it; the one that was only a carrier is gone, which
	// is the transcript the client would have sent if vekil had never injected anything.
	var forwarded models.AnthropicRequest
	if err := json.Unmarshal(raw, &forwarded); err != nil {
		t.Fatal(err)
	}
	if len(forwarded.Messages) != 4 {
		t.Fatalf("forwarded %d messages, want 4 (the carrier-only assistant turn dropped): %s", len(forwarded.Messages), string(raw))
	}
	if !strings.Contains(string(forwarded.Messages[1].Content), "working") {
		t.Fatalf("stripping removed the assistant's real content: %s", string(forwarded.Messages[1].Content))
	}
	// Dropping a carrier-only turn can leave two user messages adjacent. That is the shape the
	// client would have had without vekil's injection, and it is strictly better than the two
	// alternatives -- forwarding an empty content array, or forwarding our carrier. Like
	// everything else here it is not asserted against live Anthropic.
	if forwarded.Messages[2].Role != "user" || forwarded.Messages[3].Role != "user" {
		t.Fatalf("expected the two trailing user turns to survive: %s", string(raw))
	}
	if forwarded.MaxTokens == nil || *forwarded.MaxTokens != 64 {
		t.Fatalf("max_tokens = %v, want the client's 64 preserved through the rewrite", forwarded.MaxTokens)
	}
}
