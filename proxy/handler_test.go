package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/copilot-proxy/auth"
	"github.com/sozercan/copilot-proxy/models"
)

func newTestProxyHandler(t *testing.T, backend http.HandlerFunc) *ProxyHandler {
	t.Helper()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	return &ProxyHandler{
		auth:       auth.NewTestAuthenticator("test-token"),
		client:     server.Client(),
		copilotURL: server.URL,
	}
}

func TestHandleHealthz(t *testing.T) {
	h := &ProxyHandler{}
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	w := httptest.NewRecorder()

	h.HandleHealthz(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	body, _ := io.ReadAll(resp.Body)
	var result map[string]string
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if result["status"] != "ok" {
		t.Fatalf("expected status ok, got %q", result["status"])
	}
}

func TestHandleAnthropicMessages(t *testing.T) {
	finishReason := "stop"
	oaiResp := models.OpenAIResponse{
		ID:      "chatcmpl-123",
		Object:  "chat.completion",
		Created: 1234567890,
		Model:   "gpt-4",
		Choices: []models.OpenAIChoice{
			{
				Index: 0,
				Message: models.OpenAIMessage{
					Role:    "assistant",
					Content: json.RawMessage(`"Hello from the backend!"`),
				},
				FinishReason: &finishReason,
			},
		},
		Usage: &models.OpenAIUsage{
			PromptTokens:     10,
			CompletionTokens: 5,
			TotalTokens:      15,
		},
	}

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify headers are set
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected application/json content-type, got %q", r.Header.Get("Content-Type"))
		}
		// Verify the request was translated to OpenAI format
		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Errorf("failed to parse upstream request: %v", err)
			return
		}
		if oaiReq.Model != "claude-sonnet-4" {
			t.Errorf("expected model claude-sonnet-4, got %q", oaiReq.Model)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(oaiResp)
	})

	anthropicReq := `{
		"model": "claude-sonnet-4",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "Hello"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(anthropicReq))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessages(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var anthropicResp models.AnthropicResponse
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &anthropicResp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if anthropicResp.Type != "message" {
		t.Errorf("expected type message, got %q", anthropicResp.Type)
	}
	if anthropicResp.Role != "assistant" {
		t.Errorf("expected role assistant, got %q", anthropicResp.Role)
	}
	if anthropicResp.Model != "claude-sonnet-4" {
		t.Errorf("expected model claude-sonnet-4, got %q", anthropicResp.Model)
	}
	if anthropicResp.StopReason != "end_turn" {
		t.Errorf("expected stop_reason end_turn, got %q", anthropicResp.StopReason)
	}
	if len(anthropicResp.Content) == 0 {
		t.Fatal("expected content blocks, got none")
	}
	if anthropicResp.Content[0].Text != "Hello from the backend!" {
		t.Errorf("expected text 'Hello from the backend!', got %q", anthropicResp.Content[0].Text)
	}
	if anthropicResp.Usage.InputTokens != 10 {
		t.Errorf("expected input_tokens 10, got %d", anthropicResp.Usage.InputTokens)
	}
	if anthropicResp.Usage.OutputTokens != 5 {
		t.Errorf("expected output_tokens 5, got %d", anthropicResp.Usage.OutputTokens)
	}
}

func TestHandleAnthropicMessagesInvalidJSON(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("backend should not be called for invalid JSON")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{invalid json`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessages(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	var errResp models.AnthropicError
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	if errResp.Type != "error" {
		t.Errorf("expected type error, got %q", errResp.Type)
	}
	if errResp.Error.Type != "invalid_request_error" {
		t.Errorf("expected error type invalid_request_error, got %q", errResp.Error.Type)
	}
}

func TestHandleAnthropicUpstreamError(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte(`{"error": "internal server error"}`))
	})

	anthropicReq := `{
		"model": "claude-sonnet-4",
		"max_tokens": 1024,
		"messages": [{"role": "user", "content": "Hello"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(anthropicReq))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessages(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d", resp.StatusCode)
	}

	var errResp models.AnthropicError
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	if errResp.Error.Type != "api_error" {
		t.Errorf("expected error type api_error, got %q", errResp.Error.Type)
	}
}

func TestHandleOpenAIChatCompletions(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Errorf("expected path /chat/completions, got %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %q", r.Header.Get("Authorization"))
		}

		// Echo back the request body as a mock response
		body, _ := io.ReadAll(r.Body)
		var oaiReq models.OpenAIRequest
		json.Unmarshal(body, &oaiReq)

		finishReason := "stop"
		resp := models.OpenAIResponse{
			ID:      "chatcmpl-456",
			Object:  "chat.completion",
			Created: 1234567890,
			Model:   oaiReq.Model,
			Choices: []models.OpenAIChoice{
				{
					Index: 0,
					Message: models.OpenAIMessage{
						Role:    "assistant",
						Content: json.RawMessage(`"Hello!"`),
					},
					FinishReason: &finishReason,
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	})

	oaiReq := `{
		"model": "gpt-4",
		"messages": [{"role": "user", "content": "Hello"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(oaiReq))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var oaiResp models.OpenAIResponse
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &oaiResp); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if oaiResp.ID != "chatcmpl-456" {
		t.Errorf("expected id chatcmpl-456, got %q", oaiResp.ID)
	}
	if oaiResp.Model != "gpt-4" {
		t.Errorf("expected model gpt-4, got %q", oaiResp.Model)
	}
}

func TestHandleResponses(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("expected path /responses, got %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %q", r.Header.Get("Authorization"))
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp-123","object":"response","status":"completed"}`))
	})

	responsesReq := `{
		"model": "gpt-4",
		"input": "Hello"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(responsesReq))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	var result map[string]interface{}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if result["id"] != "resp-123" {
		t.Errorf("expected id resp-123, got %v", result["id"])
	}
}

func TestSetCopilotHeaders(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	setCopilotHeaders(req, "my-test-token")

	tests := []struct {
		header   string
		expected string
	}{
		{"Authorization", "Bearer my-test-token"},
		{"editor-version", "vscode/1.95.0"},
		{"editor-plugin-version", "copilot-chat/0.26.7"},
		{"user-agent", "GitHubCopilotChat/0.26.7"},
		{"copilot-integration-id", "vscode-chat"},
		{"x-github-api-version", "2025-04-01"},
		{"openai-intent", "conversation-panel"},
		{"Content-Type", "application/json"},
	}

	for _, tt := range tests {
		got := req.Header.Get(tt.header)
		if got != tt.expected {
			t.Errorf("header %q: expected %q, got %q", tt.header, tt.expected, got)
		}
	}

	// x-request-id should be set but is a UUID, just check it's non-empty
	if req.Header.Get("x-request-id") == "" {
		t.Error("expected x-request-id to be set")
	}
}

func TestHandleOpenAIChatCompletionsUpstreamError(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte(`{"error":{"message":"service unavailable","type":"server_error"}}`))
	})

	oaiReq := `{
		"model": "gpt-4",
		"messages": [{"role": "user", "content": "Hello"}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(oaiReq))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", resp.StatusCode)
	}
}
