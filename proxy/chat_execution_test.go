package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestExecuteChatCompletionsUsesNativeChatWhenAvailable(t *testing.T) {
	var gotPath string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-native","object":"chat.completion","created":1,"model":"gpt-upstream","choices":[{"index":0,"message":{"role":"assistant","content":"native"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()

	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointChatCompletions, providerEndpointResponses})
	result, err := h.executeChatCompletions(context.Background(), []byte(`{"model":"gpt-public","messages":[{"role":"user","content":"hello"}]}`), chatExecutionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Response.Body.Close()
	if result.Backend != chatBackendNativeChat || result.Response == nil || result.Completion != nil || result.Stream != nil {
		t.Fatalf("result = %#v", result)
	}
	if gotPath != providerEndpointChatCompletions {
		t.Fatalf("upstream path = %q", gotPath)
	}
}

func TestExecuteChatCompletionsConvertsResponsesJSON(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/nonstream_text.json")
	if err != nil {
		t.Fatal(err)
	}
	var gotPath string
	var gotBody map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Errorf("decode upstream request: %v", err)
		}
		w.Header().Set("X-Request-Id", "req-synthetic")
		w.Header().Set("Set-Cookie", "fixture=discarded")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()

	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	result, err := h.executeChatCompletions(context.Background(), []byte(`{"model":"gpt-public","messages":[{"role":"user","content":"hello"}],"max_tokens":64}`), chatExecutionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if result.Backend != chatBackendResponses || result.Completion == nil || result.Response != nil || result.Stream != nil {
		t.Fatalf("result = %#v", result)
	}
	if gotPath != providerEndpointResponses || gotBody["model"] != "gpt-upstream" || gotBody["max_output_tokens"] != float64(64) {
		t.Fatalf("path/body = %q %#v", gotPath, gotBody)
	}
	if result.Completion.Model != "gpt-public" || result.Completion.Choices[0].Message.Role != "assistant" {
		t.Fatalf("completion = %#v", result.Completion)
	}
	if result.Headers.Get("X-Request-Id") != "req-synthetic" || result.Headers.Get("Set-Cookie") != "" || result.Headers.Get("Content-Type") != "" {
		t.Fatalf("safe headers = %#v", result.Headers)
	}
}

func newChatExecutionTestHandler(t *testing.T, baseURL string, endpoints []string) *ProxyHandler {
	t.Helper()
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:             "test-provider",
			Type:           string(providerTypeOpenAICompatible),
			Default:        true,
			BaseURL:        baseURL,
			AuthType:       string(providerAuthTypeNone),
			ModelDiscovery: string(providerModelDiscoveryStatic),
			Models: []ProviderModelConfig{{
				PublicID:   "gpt-public",
				Deployment: "gpt-upstream",
				Endpoints:  endpoints,
			}},
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)
	return h
}

func TestExecuteChatCompletionsPinsReplayIDsToResponsesBackend(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer upstream.Close()
	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointChatCompletions, providerEndpointResponses})
	body := []byte(`{"model":"gpt-public","messages":[{"role":"assistant","tool_calls":[{"id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","content":"ok"}]}`)
	_, err := h.executeChatCompletions(context.Background(), body, chatExecutionOptions{})
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.Code != responsesChatReplayMissingCode {
		t.Fatalf("error = %#v", err)
	}
	if upstreamCalls != 0 {
		t.Fatalf("upstream calls = %d; replay ID must not be sent to native Chat", upstreamCalls)
	}
}

func TestExecuteChatCompletionsCanonicalErrorRetainsSafeHeaders(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "4")
		w.Header().Set("X-Request-Id", "req-error")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, "plain failure")
	}))
	defer upstream.Close()
	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	result, err := h.executeChatCompletions(context.Background(), []byte(`{"model":"gpt-public","messages":[{"role":"user","content":"hello"}]}`), chatExecutionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Response.Body.Close()
	if result.Headers.Get("Retry-After") != "4" || result.Headers.Get("X-Request-Id") != "req-error" {
		t.Fatalf("headers = %#v response headers=%#v", result.Headers, result.Response.Header)
	}
}

func TestExecuteChatCompletionsDoesNotRerouteReplayLikeNativeIDs(t *testing.T) {
	upstreamCalls := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls++
		if r.URL.Path != providerEndpointChatCompletions {
			t.Fatalf("path = %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-native","object":"chat.completion","created":1,"model":"gpt-upstream","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer upstream.Close()
	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointChatCompletions, providerEndpointResponses})
	body := []byte(`{"model":"gpt-public","messages":[{"role":"assistant","tool_calls":[{"id":"call_vekil_customer_job","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_vekil_customer_job","content":"ok"}]}`)
	result, err := h.executeChatCompletions(context.Background(), body, chatExecutionOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer result.Response.Body.Close()
	if result.Backend != chatBackendNativeChat || upstreamCalls != 1 {
		t.Fatalf("backend/calls = %v/%d", result.Backend, upstreamCalls)
	}
}

func TestChatExecutionErrorFromStreamTermination(t *testing.T) {
	if got := chatExecutionErrorFromStreamTermination(context.DeadlineExceeded); got == nil || got.StatusCode != http.StatusGatewayTimeout || got.Code != "gateway_timeout" {
		t.Fatalf("deadline mapping = %#v", got)
	}
	if got := chatExecutionErrorFromStreamTermination(errChatStreamClientWriteFailed); got != nil {
		t.Fatalf("client write mapping = %#v", got)
	}
}
