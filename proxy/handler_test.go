package proxy

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/sozercan/copilot-proxy/auth"
	"github.com/sozercan/copilot-proxy/logger"
	"github.com/sozercan/copilot-proxy/models"
)

func newTestProxyHandler(t *testing.T, backend http.HandlerFunc) *ProxyHandler {
	t.Helper()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)
	return &ProxyHandler{
		auth:           auth.NewTestAuthenticator("test-token"),
		client:         server.Client(),
		copilotURL:     server.URL,
		log:            logger.New(logger.LevelInfo),
		retryBaseDelay: 1 * time.Millisecond,
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
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify headers are set
		if r.Header.Get("Authorization") != "Bearer test-token" {
			t.Errorf("expected Bearer test-token, got %q", r.Header.Get("Authorization"))
		}
		if r.Header.Get("Content-Type") != "application/json" {
			t.Errorf("expected application/json content-type, got %q", r.Header.Get("Content-Type"))
		}
		// Verify the request was translated to OpenAI format with forced streaming
		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Errorf("failed to parse upstream request: %v", err)
			return
		}
		if oaiReq.Model != "claude-sonnet-4" {
			t.Errorf("expected model claude-sonnet-4, got %q", oaiReq.Model)
		}
		if oaiReq.Stream == nil || !*oaiReq.Stream {
			t.Error("expected stream=true in upstream request (forced streaming)")
		}

		// Return SSE streaming response (since handler forces streaming)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"id\":\"chatcmpl-123\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello from the backend!\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"id\":\"chatcmpl-123\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
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
	if anthropicResp.StopReason == nil || *anthropicResp.StopReason != "end_turn" {
		t.Errorf("expected stop_reason end_turn, got %v", anthropicResp.StopReason)
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

func TestHandleModels(t *testing.T) {
	t.Run("proxies upstream response", func(t *testing.T) {
		h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/models" {
				t.Errorf("expected path /models, got %s", r.URL.Path)
			}
			if r.Method != http.MethodGet {
				t.Errorf("expected GET, got %s", r.Method)
			}
			if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Errorf("expected Authorization header 'Bearer test-token', got %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4o","object":"model","created":0,"owned_by":"github-copilot"},{"id":"claude-sonnet-4","object":"model","created":0,"owned_by":"github-copilot"}]}`))
		})
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()

		h.HandleModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		body, _ := io.ReadAll(resp.Body)
		var result struct {
			Object string `json:"object"`
			Data   []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if result.Object != "list" {
			t.Errorf("expected object list, got %q", result.Object)
		}
		if len(result.Data) != 2 {
			t.Fatalf("expected 2 models, got %d", len(result.Data))
		}
		if result.Data[0].ID != "gpt-4o" {
			t.Errorf("expected first model gpt-4o, got %q", result.Data[0].ID)
		}
		if result.Data[1].ID != "claude-sonnet-4" {
			t.Errorf("expected second model claude-sonnet-4, got %q", result.Data[1].ID)
		}
	})

	t.Run("upstream error is forwarded", func(t *testing.T) {
		h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			w.Write([]byte(`{"error":"internal server error"}`))
		})
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()

		h.HandleModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusInternalServerError {
			t.Fatalf("expected 500, got %d", resp.StatusCode)
		}
	})
}

// TestOpenAIErrorResponseShape validates error responses match the OpenAI spec:
// {"error": {"message": "...", "type": "...", "param": null, "code": null}}
func TestOpenAIErrorResponseShape(t *testing.T) {
	w := httptest.NewRecorder()
	writeOpenAIError(w, http.StatusBadRequest, "test error message", "invalid_request_error")

	if w.Code != http.StatusBadRequest {
		t.Fatalf("expected status 400, got %d", w.Code)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Must have top-level "error" key
	if _, ok := raw["error"]; !ok {
		t.Fatal("missing top-level 'error' key")
	}

	var errObj map[string]json.RawMessage
	if err := json.Unmarshal(raw["error"], &errObj); err != nil {
		t.Fatalf("unmarshal error object: %v", err)
	}

	// Check all required fields exist
	requiredFields := []string{"message", "type", "param", "code"}
	for _, f := range requiredFields {
		if _, ok := errObj[f]; !ok {
			t.Errorf("error object missing required field %q", f)
		}
	}

	// Check values
	var msg string
	json.Unmarshal(errObj["message"], &msg)
	if msg != "test error message" {
		t.Errorf("message = %q, want %q", msg, "test error message")
	}

	var errType string
	json.Unmarshal(errObj["type"], &errType)
	if errType != "invalid_request_error" {
		t.Errorf("type = %q, want %q", errType, "invalid_request_error")
	}

	// param and code should be null
	if string(errObj["param"]) != "null" {
		t.Errorf("param = %s, want null", errObj["param"])
	}
	if string(errObj["code"]) != "null" {
		t.Errorf("code = %s, want null", errObj["code"])
	}
}

// TestOpenAIChatCompletionsStreaming validates that streaming responses are
// passed through correctly with proper SSE headers.
func TestOpenAIChatCompletionsStreaming(t *testing.T) {
	sseBody := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"index\":0}]}\n\ndata: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\ndata: [DONE]\n\n"

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify streaming detection
		var partial struct {
			Stream *bool `json:"stream"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &partial)
		if partial.Stream == nil || !*partial.Stream {
			t.Error("expected stream=true in upstream request")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sseBody))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	// Verify SSE headers
	ct := resp.Header.Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	// Verify SSE body is passed through
	body, _ := io.ReadAll(resp.Body)
	bodyStr := string(body)
	if !strings.Contains(bodyStr, "data: {") {
		t.Error("streaming response should contain 'data:' lines")
	}
	if !strings.Contains(bodyStr, "[DONE]") {
		t.Error("streaming response should contain [DONE]")
	}
}

// TestOpenAIResponsesStreaming validates streaming passthrough for the Responses API.
func TestOpenAIResponsesStreaming(t *testing.T) {
	sseBody := "event: response.created\ndata: {\"id\":\"resp-1\",\"type\":\"response\"}\n\nevent: response.completed\ndata: {\"id\":\"resp-1\",\"status\":\"completed\"}\n\n"

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("expected path /responses, got %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sseBody))
	})

	reqBody := `{"model":"gpt-4","input":"Hello","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	ct := resp.Header.Get("Content-Type")
	if ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want text/event-stream", ct)
	}

	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "response.created") {
		t.Error("streaming response should contain event data")
	}
}

// TestOpenAIChatCompletionsUpstreamErrorPassthrough validates that upstream error
// responses are forwarded with correct status and content-type.
func TestOpenAIChatCompletionsUpstreamErrorPassthrough(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"Invalid model","type":"invalid_request_error","param":"model","code":null}}`))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	// Verify the error body is passed through unchanged
	body, _ := io.ReadAll(resp.Body)
	var errResp map[string]map[string]interface{}
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("failed to parse error: %v", err)
	}
	if errResp["error"]["type"] != "invalid_request_error" {
		t.Errorf("error.type = %v, want invalid_request_error", errResp["error"]["type"])
	}
}

// TestOpenAIChatCompletionsResponseShape validates a non-streaming response
// has the correct OpenAI Chat Completions response structure.
func TestOpenAIChatCompletionsResponseShape(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id": "chatcmpl-test",
			"object": "chat.completion",
			"created": 1700000000,
			"model": "gpt-4",
			"choices": [{
				"index": 0,
				"message": {"role": "assistant", "content": "Hello!"},
				"finish_reason": "stop",
				"logprobs": null
			}],
			"usage": {"prompt_tokens": 5, "completion_tokens": 3, "total_tokens": 8},
			"system_fingerprint": "fp_test"
		}`))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)

	// Verify passthrough preserved all OpenAI response fields
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	requiredFields := []string{"id", "object", "created", "model", "choices", "usage"}
	for _, f := range requiredFields {
		if _, ok := raw[f]; !ok {
			t.Errorf("response missing required field %q", f)
		}
	}

	// Verify object is "chat.completion"
	var obj string
	json.Unmarshal(raw["object"], &obj)
	if obj != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", obj)
	}

	// Verify choices structure
	var choices []map[string]json.RawMessage
	json.Unmarshal(raw["choices"], &choices)
	if len(choices) != 1 {
		t.Fatalf("expected 1 choice, got %d", len(choices))
	}
	for _, f := range []string{"index", "message", "finish_reason"} {
		if _, ok := choices[0][f]; !ok {
			t.Errorf("choice missing field %q", f)
		}
	}

	// Verify system_fingerprint is preserved
	if _, ok := raw["system_fingerprint"]; !ok {
		t.Error("response missing system_fingerprint (should be preserved in passthrough)")
	}
}

// TestOpenAIResponsesResponseShape validates the Responses API non-streaming
// passthrough preserves the response structure.
func TestOpenAIResponsesResponseShape(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{
			"id": "resp-test",
			"object": "response",
			"created_at": 1700000000,
			"status": "completed",
			"model": "gpt-4",
			"output": [
				{"type": "message", "role": "assistant", "content": [{"type": "output_text", "text": "Hello!"}]}
			],
			"usage": {"input_tokens": 5, "output_tokens": 3, "total_tokens": 8}
		}`))
	})

	reqBody := `{"model":"gpt-4","input":"Hi"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	body, _ := io.ReadAll(resp.Body)

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	// Verify key Responses API fields are preserved in passthrough
	for _, f := range []string{"id", "object", "status", "model", "output", "usage"} {
		if _, ok := raw[f]; !ok {
			t.Errorf("response missing field %q", f)
		}
	}

	var obj string
	json.Unmarshal(raw["object"], &obj)
	if obj != "response" {
		t.Errorf("object = %q, want response", obj)
	}
}

// TestHandleAnthropicMessages_ParallelToolCalls verifies that parallel tool
// calls are preserved through the forced-streaming aggregation path.
func TestHandleAnthropicMessages_ParallelToolCalls(t *testing.T) {
	idx0, idx1 := 0, 1
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &oaiReq)
		if oaiReq.Stream == nil || !*oaiReq.Stream {
			t.Error("expected stream=true (forced streaming)")
		}
		if oaiReq.ParallelToolCalls == nil || !*oaiReq.ParallelToolCalls {
			t.Error("expected parallel_tool_calls=true")
		}

		// Return SSE with text + 2 parallel tool calls (interleaved by index)
		chunks := []models.OpenAIStreamChunk{
			{ID: "c1", Model: "gpt-4", Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{Content: json.RawMessage(`"I'll delegate both tasks"`)}}}},
			{ID: "c1", Model: "gpt-4", Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{ID: "call_1", Index: &idx0, Type: "function", Function: models.OpenAIFunctionCall{Name: "delegate_task", Arguments: ""}}}}}}},
			{ID: "c1", Model: "gpt-4", Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{Index: &idx0, Function: models.OpenAIFunctionCall{Arguments: `{"agent":"researcher","prompt":"pros"}`}}}}}}},
			{ID: "c1", Model: "gpt-4", Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{ID: "call_2", Index: &idx1, Type: "function", Function: models.OpenAIFunctionCall{Name: "delegate_task", Arguments: ""}}}}}}},
			{ID: "c1", Model: "gpt-4", Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{ToolCalls: []models.OpenAIToolCall{{Index: &idx1, Function: models.OpenAIFunctionCall{Arguments: `{"agent":"researcher","prompt":"cons"}`}}}}}}},
			{ID: "c1", Model: "gpt-4", Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{}, FinishReason: strPtr("tool_calls")}}, Usage: &models.OpenAIUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150}},
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			b, _ := json.Marshal(chunk)
			w.Write([]byte("data: " + string(b) + "\n\n"))
		}
		w.Write([]byte("data: [DONE]\n\n"))
	})

	anthropicReq := `{
		"model": "claude-opus-4.6-fast",
		"max_tokens": 4096,
		"messages": [{"role": "user", "content": "Call delegate_task twice"}],
		"tools": [{"name": "delegate_task", "description": "Delegate", "input_schema": {"type": "object"}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(anthropicReq))
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

	if anthropicResp.StopReason == nil || *anthropicResp.StopReason != "tool_use" {
		t.Errorf("stop_reason = %v, want tool_use", anthropicResp.StopReason)
	}
	if len(anthropicResp.Content) != 3 {
		t.Fatalf("expected 3 content blocks (1 text + 2 tool_use), got %d", len(anthropicResp.Content))
	}
	if anthropicResp.Content[0].Type != "text" || anthropicResp.Content[0].Text != "I'll delegate both tasks" {
		t.Errorf("content[0] = %+v, want text", anthropicResp.Content[0])
	}
	if anthropicResp.Content[1].Type != "tool_use" || anthropicResp.Content[1].ID != "call_1" || anthropicResp.Content[1].Name != "delegate_task" {
		t.Errorf("content[1] = %+v, want tool_use call_1", anthropicResp.Content[1])
	}
	if anthropicResp.Content[2].Type != "tool_use" || anthropicResp.Content[2].ID != "call_2" || anthropicResp.Content[2].Name != "delegate_task" {
		t.Errorf("content[2] = %+v, want tool_use call_2", anthropicResp.Content[2])
	}
	if anthropicResp.Usage.InputTokens != 100 || anthropicResp.Usage.OutputTokens != 50 {
		t.Errorf("usage = %+v, want input=100 output=50", anthropicResp.Usage)
	}
}

// TestInjectParallelToolCalls validates the parallel_tool_calls injection for OpenAI passthrough.
func TestInjectParallelToolCalls(t *testing.T) {
	t.Run("injects when tools present", func(t *testing.T) {
		input := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f"}}]}`
		result := injectParallelToolCalls([]byte(input))
		var m map[string]json.RawMessage
		json.Unmarshal(result, &m)
		if string(m["parallel_tool_calls"]) != "true" {
			t.Errorf("parallel_tool_calls = %s, want true", m["parallel_tool_calls"])
		}
	})

	t.Run("preserves existing value", func(t *testing.T) {
		input := `{"model":"gpt-4","tools":[{"type":"function"}],"parallel_tool_calls":false}`
		result := injectParallelToolCalls([]byte(input))
		var m map[string]json.RawMessage
		json.Unmarshal(result, &m)
		if string(m["parallel_tool_calls"]) != "false" {
			t.Errorf("parallel_tool_calls = %s, want false (preserved)", m["parallel_tool_calls"])
		}
	})

	t.Run("no-op without tools", func(t *testing.T) {
		input := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`
		result := injectParallelToolCalls([]byte(input))
		if string(result) != input {
			t.Errorf("body was modified: %s", result)
		}
	})

	t.Run("no-op for invalid JSON", func(t *testing.T) {
		input := `{invalid}`
		result := injectParallelToolCalls([]byte(input))
		if string(result) != input {
			t.Errorf("body was modified for invalid JSON: %s", result)
		}
	})
}

// TestOpenAIChatCompletions_InjectsParallelToolCalls verifies parallel_tool_calls
// is injected and forced streaming is used when tools are present.
func TestOpenAIChatCompletions_InjectsParallelToolCalls(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &oaiReq)
		if oaiReq.ParallelToolCalls == nil || !*oaiReq.ParallelToolCalls {
			t.Error("expected parallel_tool_calls=true injected by proxy")
		}
		if oaiReq.Stream == nil || !*oaiReq.Stream {
			t.Error("expected stream=true forced by proxy when tools present")
		}

		// Return SSE since proxy forced streaming
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n"))
		w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		w.Write([]byte("data: [DONE]\n\n"))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	if w.Result().StatusCode != http.StatusOK {
		body, _ := io.ReadAll(w.Result().Body)
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, body)
	}
}

func TestHandleResponses_GzipBody(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		// The upstream should receive the decompressed body
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v (body: %q)", err, body)
		}
		if req["model"] != "gpt-4" {
			t.Errorf("expected model gpt-4, got %v", req["model"])
		}
		if req["input"] != "Hello" {
			t.Errorf("expected input Hello, got %v", req["input"])
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp-gz","object":"response","status":"completed"}`))
	})

	// Gzip-compress the request body
	responsesReq := `{"model":"gpt-4","input":"Hello"}`
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Write([]byte(responsesReq))
	gw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
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
	if result["id"] != "resp-gz" {
		t.Errorf("expected id resp-gz, got %v", result["id"])
	}
}

func TestHandleAnthropicMessages_GzipBody(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hi\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\ndata: [DONE]\n\n"))
	})

	// Gzip-compress an Anthropic request
	anthropicReq := `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"Hello"}],"max_tokens":1024}`
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Write([]byte(anthropicReq))
	gw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessages(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
}

func TestHandleResponses_ZstdBody(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v (body: %q)", err, body)
		}
		if req["model"] != "gpt-5.4" {
			t.Errorf("expected model gpt-5.4, got %v", req["model"])
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp-zstd","object":"response","status":"completed"}`))
	})

	// Zstd-compress the request body
	responsesReq := `{"model":"gpt-5.4","input":"Hello"}`
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("failed to create zstd writer: %v", err)
	}
	zw.Write([]byte(responsesReq))
	zw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd")
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
	if result["id"] != "resp-zstd" {
		t.Errorf("expected id resp-zstd, got %v", result["id"])
	}
}

func TestHandleOpenAIChatCompletions_GzipBody(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}
		if req["model"] != "gpt-4o" {
			t.Errorf("expected model gpt-4o, got %v", req["model"])
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID:      "chatcmpl-gz",
			Object:  "chat.completion",
			Choices: []models.OpenAIChoice{{Message: models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"Hi"`)}}},
		})
	})

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}]}`
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	gw.Write([]byte(reqBody))
	gw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	if w.Result().StatusCode != http.StatusOK {
		body, _ := io.ReadAll(w.Result().Body)
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, body)
	}
}

func TestHandleOpenAIChatCompletions_ZstdBody(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID:      "chatcmpl-zstd",
			Object:  "chat.completion",
			Choices: []models.OpenAIChoice{{Message: models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"Hi"`)}}},
		})
	})

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}]}`
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	zw.Write([]byte(reqBody))
	zw.Close()

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", &buf)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "zstd")
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	if w.Result().StatusCode != http.StatusOK {
		body, _ := io.ReadAll(w.Result().Body)
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, body)
	}
}
