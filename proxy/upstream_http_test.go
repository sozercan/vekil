package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func newProviderRoutingTestHandler(t *testing.T, endpoints []string) *ProxyHandler {
	t.Helper()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:      "azure",
				Type:    "azure-openai",
				Default: true,
				BaseURL: "https://example.openai.azure.com/openai/v1",
				APIKey:  "azure-test-key",
				Models: []ProviderModelConfig{{
					PublicID:   "gpt-5-public",
					Deployment: "gpt-5-4-prod",
					Endpoints:  append([]string(nil), endpoints...),
					Name:       "GPT-5 Public",
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	return handler
}

func TestExtractRequestModel(t *testing.T) {
	tests := []struct {
		name string
		body string
		want string
	}{
		{
			name: "top level model",
			body: `{"model":"gpt-4.1","input":"hello"}`,
			want: "gpt-4.1",
		},
		{
			name: "nested content before model",
			body: `{"input":{"messages":[{"role":"user","content":"hi"}],"metadata":{"nested":[1,2,3]}},"model":"  gpt-4o-mini  "}`,
			want: "gpt-4o-mini",
		},
		{
			name: "nested model key is ignored",
			body: `{"input":{"model":"wrong"},"metadata":{"flags":["a","b"]}}`,
			want: "",
		},
		{
			name: "non string model is ignored",
			body: `{"model":123,"input":"hello"}`,
			want: "",
		},
		{
			name: "non object payload is ignored",
			body: `["gpt-4.1"]`,
			want: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := extractRequestModel([]byte(tt.body)); got != tt.want {
				t.Fatalf("extractRequestModel() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveProviderRequest_RewritesConfiguredResponsesModelToProviderDeployment(t *testing.T) {
	handler := newProviderRoutingTestHandler(t, []string{"/responses"})

	provider, _, rewrittenBody, err := handler.resolveProviderRequest([]byte(`{"model":"gpt-5-public","input":"hello"}`), "/responses")
	if err != nil {
		t.Fatalf("resolveProviderRequest() error = %v", err)
	}
	if provider == nil {
		t.Fatal("resolveProviderRequest() provider = nil, want azure provider")
	}
	if provider.id != "azure" {
		t.Fatalf("resolveProviderRequest() provider.id = %q, want azure", provider.id)
	}
	if got := extractResponsesRequestModel(rewrittenBody); got != "gpt-5-4-prod" {
		t.Fatalf("rewritten model = %q, want gpt-5-4-prod", got)
	}

	var payload struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal(rewrittenBody, &payload); err != nil {
		t.Fatalf("json.Unmarshal(rewrittenBody) error = %v", err)
	}
	if payload.Input != "hello" {
		t.Fatalf("rewritten input = %q, want hello", payload.Input)
	}
}

func TestResolveProviderRequest_RejectsKnownModelWithoutEndpointSupport(t *testing.T) {
	handler := newProviderRoutingTestHandler(t, []string{"/responses"})

	provider, _, rewrittenBody, err := handler.resolveProviderRequest([]byte(`{"model":"gpt-5-public","messages":[{"role":"user","content":"hello"}]}`), "/chat/completions")
	if err == nil {
		t.Fatal("resolveProviderRequest() error = nil, want unsupported endpoint error")
	}
	if provider != nil {
		t.Fatalf("resolveProviderRequest() provider = %#v, want nil on error", provider)
	}
	if rewrittenBody != nil {
		t.Fatalf("resolveProviderRequest() rewrittenBody = %q, want nil on error", rewrittenBody)
	}

	var providerErr *providerRequestError
	if !errors.As(err, &providerErr) {
		t.Fatalf("resolveProviderRequest() error = %T, want *providerRequestError", err)
	}
	if providerErr.statusCode != http.StatusBadRequest {
		t.Fatalf("providerRequestError.statusCode = %d, want %d", providerErr.statusCode, http.StatusBadRequest)
	}
	if !strings.Contains(providerErr.Error(), `does not support /chat/completions`) {
		t.Fatalf("providerRequestError.Error() = %q, want unsupported endpoint message", providerErr.Error())
	}
}

func TestHandleResponses_StripsInternalChatMessageMetadataBeforeUpstream(t *testing.T) {
	var upstreamBody []byte
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("upstream path = %q, want /responses", r.URL.Path)
		}
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed"}`))
	})

	body := `{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HandleResponses status = %d, body = %s", w.Code, w.Body.String())
	}
	if strings.Contains(string(upstreamBody), "internal_chat_message_metadata_passthrough") {
		t.Fatalf("upstream body retained internal chat metadata passthrough: %s", upstreamBody)
	}
	if !strings.Contains(string(upstreamBody), `"text":"hello"`) {
		t.Fatalf("upstream body lost user content: %s", upstreamBody)
	}
}

func TestResponsesRequestRewriting_StripsInternalChatMessageMetadataForDefaultCopilot(t *testing.T) {
	handler := newTestProxyHandler(t, func(http.ResponseWriter, *http.Request) {})
	originalBody := []byte(`{
		"model":"gpt-5.4",
		"input":[
			{
				"type":"message",
				"role":"user",
				"content":[{"type":"input_text","text":"hello"}],
				"internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}
			},
			{
				"type":"function_call_output",
				"call_id":"call-1",
				"output":"{}",
				"nested":{"internal_chat_message_metadata_passthrough":{"turn_id":"nested-turn"}}
			}
		]
	}`)

	rewrittenForResponses := handler.rewriteResponsesRequestBody(originalBody, "responses", true)
	provider, _, rewrittenBody, err := handler.resolveProviderRequest(rewrittenForResponses, "/responses")
	if err != nil {
		t.Fatalf("resolveProviderRequest() error = %v", err)
	}
	if provider == nil {
		t.Fatal("resolveProviderRequest() provider = nil, want default copilot provider")
	}
	if provider.kind != providerTypeCopilot {
		t.Fatalf("resolveProviderRequest() provider.kind = %q, want %q", provider.kind, providerTypeCopilot)
	}

	if strings.Contains(string(rewrittenBody), "internal_chat_message_metadata_passthrough") {
		t.Fatalf("rewritten body retained internal chat metadata passthrough: %s", rewrittenBody)
	}

	payload := decodeResponsesRequestPayload(t, rewrittenBody)
	var input []map[string]interface{}
	if err := json.Unmarshal(payload["input"], &input); err != nil {
		t.Fatalf("json.Unmarshal(input) error = %v", err)
	}
	if len(input) != 2 {
		t.Fatalf("len(input) = %d, want 2", len(input))
	}
	if got := input[0]["type"]; got != "message" {
		t.Fatalf("input[0].type = %v, want message", got)
	}
}

func TestResponsesRequestRewriting_PreservesInternalChatMessageMetadataForOpenAICodex(t *testing.T) {
	body := []byte(`{"model":"gpt-5.4","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-1"}}],"top_p":1}`)

	rewrittenBody, strippedFields := stripUnsupportedResponsesRequestFields(body, &providerRuntime{kind: providerTypeOpenAICodex})
	if !strings.Contains(string(rewrittenBody), "internal_chat_message_metadata_passthrough") {
		t.Fatalf("rewritten body stripped OpenAI Codex internal chat metadata passthrough: %s", rewrittenBody)
	}
	if strings.Contains(strings.Join(strippedFields, ","), "internal_chat_message_metadata_passthrough") {
		t.Fatalf("stripped fields unexpectedly include internal chat metadata passthrough: %v", strippedFields)
	}

	payload := decodeResponsesRequestPayload(t, rewrittenBody)
	if _, ok := payload["top_p"]; ok {
		t.Fatalf("rewritten payload retained top_p for OpenAI Codex: %s", rewrittenBody)
	}
}

func TestResponsesRequestRewriting_StripsUnsupportedImageGenerationToolForAzure(t *testing.T) {
	handler := newProviderRoutingTestHandler(t, []string{"/responses"})
	originalBody := []byte(`{"model":"gpt-5-public","input":"hello","tools":[{"type":"function","name":"lookup_weather","description":"Lookup the weather","parameters":{"type":"object","properties":{}}},{"type":"image_generation"}],"tool_choice":"auto"}`)

	rewrittenForResponses := handler.rewriteResponsesRequestBody(originalBody, "responses", true)
	provider, _, rewrittenBody, err := handler.resolveProviderRequest(rewrittenForResponses, "/responses")
	if err != nil {
		t.Fatalf("resolveProviderRequest() error = %v", err)
	}
	if provider == nil {
		t.Fatal("resolveProviderRequest() provider = nil, want azure provider")
	}
	if provider.kind != providerTypeAzureOpenAI {
		t.Fatalf("resolveProviderRequest() provider.kind = %q, want %q", provider.kind, providerTypeAzureOpenAI)
	}
	if got := extractResponsesRequestModel(rewrittenBody); got != "gpt-5-4-prod" {
		t.Fatalf("rewritten model = %q, want gpt-5-4-prod", got)
	}

	payload := decodeResponsesRequestPayload(t, rewrittenBody)
	tools := decodeResponsesRequestTools(t, payload)
	if len(tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1 after stripping unsupported tools", len(tools))
	}
	if tools[0].Type != "function" {
		t.Fatalf("tools[0].Type = %q, want function", tools[0].Type)
	}
	if tools[0].Name != "lookup_weather" {
		t.Fatalf("tools[0].Name = %q, want lookup_weather", tools[0].Name)
	}

	rawToolChoice, ok := payload["tool_choice"]
	if !ok {
		t.Fatal("rewritten payload missing tool_choice")
	}
	var toolChoice string
	if err := json.Unmarshal(rawToolChoice, &toolChoice); err != nil {
		t.Fatalf("json.Unmarshal(tool_choice) error = %v", err)
	}
	if toolChoice != "auto" {
		t.Fatalf("tool_choice = %q, want auto", toolChoice)
	}
}

func TestResponsesRequestRewriting_StripsUnsupportedImageGenerationToolForDefaultCopilot(t *testing.T) {
	handler := newTestProxyHandler(t, func(http.ResponseWriter, *http.Request) {})
	originalBody := []byte(`{"model":"gpt-5.4","input":"hello","tools":[{"type":"image_generation"}],"tool_choice":"required"}`)

	rewrittenForResponses := handler.rewriteResponsesRequestBody(originalBody, "responses", true)
	provider, _, rewrittenBody, err := handler.resolveProviderRequest(rewrittenForResponses, "/responses")
	if err != nil {
		t.Fatalf("resolveProviderRequest() error = %v", err)
	}
	if provider == nil {
		t.Fatal("resolveProviderRequest() provider = nil, want default copilot provider")
	}
	if provider.kind != providerTypeCopilot {
		t.Fatalf("resolveProviderRequest() provider.kind = %q, want %q", provider.kind, providerTypeCopilot)
	}
	if provider.id != "copilot" {
		t.Fatalf("resolveProviderRequest() provider.id = %q, want copilot", provider.id)
	}
	if got := extractResponsesRequestModel(rewrittenBody); got != "gpt-5.4" {
		t.Fatalf("rewritten model = %q, want gpt-5.4", got)
	}

	payload := decodeResponsesRequestPayload(t, rewrittenBody)
	tools := decodeResponsesRequestTools(t, payload)
	if len(tools) != 0 {
		t.Fatalf("len(tools) = %d, want 0 after stripping unsupported tools", len(tools))
	}
	if _, ok := payload["tool_choice"]; ok {
		t.Fatalf("rewritten payload unexpectedly retained tool_choice: %s", payload["tool_choice"])
	}
}

func TestResponsesRequestRewriting_StripsUnsupportedImageGenerationToolChoiceForAzure(t *testing.T) {
	handler := newProviderRoutingTestHandler(t, []string{"/responses"})
	originalBody := []byte(`{"model":"gpt-5-public","input":"hello","tools":[{"type":"function","name":"lookup_weather","description":"Lookup the weather","parameters":{"type":"object","properties":{}}},{"type":"image_generation"}],"tool_choice":{"type":"image_generation"}}`)

	rewrittenForResponses := handler.rewriteResponsesRequestBody(originalBody, "responses", true)
	provider, _, rewrittenBody, err := handler.resolveProviderRequest(rewrittenForResponses, "/responses")
	if err != nil {
		t.Fatalf("resolveProviderRequest() error = %v", err)
	}
	if provider == nil {
		t.Fatal("resolveProviderRequest() provider = nil, want azure provider")
	}
	if provider.kind != providerTypeAzureOpenAI {
		t.Fatalf("resolveProviderRequest() provider.kind = %q, want %q", provider.kind, providerTypeAzureOpenAI)
	}

	payload := decodeResponsesRequestPayload(t, rewrittenBody)
	tools := decodeResponsesRequestTools(t, payload)
	if len(tools) != 1 {
		t.Fatalf("len(tools) = %d, want 1 after stripping unsupported tools", len(tools))
	}
	if tools[0].Type != "function" {
		t.Fatalf("tools[0].Type = %q, want function", tools[0].Type)
	}
	if _, ok := payload["tool_choice"]; ok {
		t.Fatalf("rewritten payload unexpectedly retained tool_choice: %s", payload["tool_choice"])
	}
}

type responsesRequestTool struct {
	Type string `json:"type"`
	Name string `json:"name"`
}

func decodeResponsesRequestPayload(t *testing.T, body []byte) map[string]json.RawMessage {
	t.Helper()

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("json.Unmarshal(body) error = %v", err)
	}
	return payload
}

func decodeResponsesRequestTools(t *testing.T, payload map[string]json.RawMessage) []responsesRequestTool {
	t.Helper()

	rawTools, ok := payload["tools"]
	if !ok {
		return nil
	}

	var tools []responsesRequestTool
	if err := json.Unmarshal(rawTools, &tools); err != nil {
		t.Fatalf("json.Unmarshal(tools) error = %v", err)
	}
	return tools
}

func TestRewriteRequestModelForProvider_RewritesGenericJSONModelAndNoopsWhenUnchanged(t *testing.T) {
	originalBody := []byte(`{"model":"gpt-5-public","messages":[{"role":"user","content":"hello"}]}`)

	rewrittenBody, rewritten, err := rewriteRequestModelForProvider(originalBody, "gpt-5-4-prod")
	if err != nil {
		t.Fatalf("rewriteRequestModelForProvider() error = %v", err)
	}
	if !rewritten {
		t.Fatal("rewriteRequestModelForProvider() rewritten = false, want true")
	}
	if got := extractRequestModel(rewrittenBody); got != "gpt-5-4-prod" {
		t.Fatalf("rewritten model = %q, want gpt-5-4-prod", got)
	}

	var payload struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(rewrittenBody, &payload); err != nil {
		t.Fatalf("json.Unmarshal(rewrittenBody) error = %v", err)
	}
	if len(payload.Messages) != 1 || payload.Messages[0].Content != "hello" {
		t.Fatalf("rewritten messages = %+v, want original message content preserved", payload.Messages)
	}

	unchangedBody, rewritten, err := rewriteRequestModelForProvider(rewrittenBody, "gpt-5-4-prod")
	if err != nil {
		t.Fatalf("rewriteRequestModelForProvider(already mapped) error = %v", err)
	}
	if rewritten {
		t.Fatal("rewriteRequestModelForProvider(already mapped) rewritten = true, want false")
	}
	if string(unchangedBody) != string(rewrittenBody) {
		t.Fatalf("rewriteRequestModelForProvider(already mapped) body changed: got %s want %s", unchangedBody, rewrittenBody)
	}
}

func TestPostChatCompletions_UsesAzureClassicDeploymentURLAndKeepsPublicBodyModel(t *testing.T) {
	var sawRequest bool
	azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawRequest = true
		if got, want := r.URL.Path, "/openai/deployments/gpt-5-4-prod/chat/completions"; got != want {
			t.Fatalf("upstream path = %q, want %q", got, want)
		}
		if got, want := r.URL.Query().Get("api-version"), "2025-04-01-preview"; got != want {
			t.Fatalf("api-version = %q, want %q", got, want)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("unmarshal upstream body: %v", err)
		}
		if got := rawJSONString(payload["model"]); got != "gpt-5-public" {
			t.Fatalf("upstream body model = %q, want public model to remain unchanged", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-test","choices":[]}`))
	}))
	defer azureServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:         "azure",
				Type:       "azure-openai",
				Default:    true,
				BaseURL:    azureServer.URL + "/openai",
				APIVersion: "2025-04-01-preview",
				APIKey:     "azure-test-key",
				Models: []ProviderModelConfig{{
					PublicID:   "gpt-5-public",
					Deployment: "gpt-5-4-prod",
					Endpoints:  []string{providerEndpointChatCompletions},
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	resp, err := handler.postChatCompletions(context.Background(), []byte(`{"model":"gpt-5-public","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatalf("postChatCompletions() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if !sawRequest {
		t.Fatal("upstream server was not called")
	}
}

// fakeBodyResponse builds a minimal 200 *http.Response whose body is the given
// bytes, for exercising the passthrough/usage-sniff helpers directly.
func fakeBodyResponse(body string) *http.Response {
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func TestWriteOpenAIPassthroughObservingUsage_SniffsWithinCap(t *testing.T) {
	h := &ProxyHandler{}
	ctx, summary := WithRequestSummary(context.Background())
	body := `{"choices":[{"message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`

	w := httptest.NewRecorder()
	h.writeOpenAIPassthroughObservingUsage(ctx, w, fakeBodyResponse(body))

	if got := w.Body.String(); got != body {
		t.Fatalf("body changed:\n got: %s\nwant: %s", got, body)
	}
	if summary.totalTokens == nil || *summary.totalTokens != 15 {
		t.Fatalf("totalTokens = %v, want 15", summary.totalTokens)
	}
	if summary.promptTokens == nil || *summary.promptTokens != 11 {
		t.Fatalf("promptTokens = %v, want 11", summary.promptTokens)
	}
}

func TestWriteOpenAIPassthroughObservingUsage_OversizedStreamsThroughIntact(t *testing.T) {
	h := &ProxyHandler{}
	ctx, summary := WithRequestSummary(context.Background())
	// A body larger than the sniff cap: usage is skipped (fail-open) but every
	// byte must still reach the client unchanged.
	filler := strings.Repeat("x", usageSniffMaxBuffer+1024)
	body := `{"padding":"` + filler + `","usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`

	w := httptest.NewRecorder()
	h.writeOpenAIPassthroughObservingUsage(ctx, w, fakeBodyResponse(body))

	if got := w.Body.String(); got != body {
		t.Fatalf("oversized body not streamed through intact: got %d bytes, want %d", len(got), len(body))
	}
	// Usage parse is skipped for oversized bodies.
	if summary.totalTokens != nil {
		t.Fatalf("totalTokens = %v, want nil (usage skipped for oversized body)", *summary.totalTokens)
	}
}

func TestWriteOpenAIPassthroughObservingUsage_Non200PlainCopy(t *testing.T) {
	h := &ProxyHandler{}
	ctx, summary := WithRequestSummary(context.Background())
	resp := fakeBodyResponse(`{"error":"boom"}`)
	resp.StatusCode = http.StatusTooManyRequests

	w := httptest.NewRecorder()
	h.writeOpenAIPassthroughObservingUsage(ctx, w, resp)

	if w.Result().StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", w.Result().StatusCode)
	}
	if summary.totalTokens != nil {
		t.Fatal("usage should not be observed on a non-200 response")
	}
}
