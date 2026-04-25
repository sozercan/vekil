package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func newTestProxyHandler(t testing.TB, backend http.HandlerFunc) *ProxyHandler {
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newRoundTripTestProxyHandler(t testing.TB, transport roundTripFunc) *ProxyHandler {
	t.Helper()
	return &ProxyHandler{
		auth:           auth.NewTestAuthenticator("test-token"),
		client:         &http.Client{Transport: transport},
		copilotURL:     "http://upstream.test",
		log:            logger.New(logger.LevelInfo),
		retryBaseDelay: 1 * time.Millisecond,
	}
}

type trackingReadCloser struct {
	reader    io.Reader
	bytesRead int
	closed    bool
}

func newTrackingReadCloser(body string) *trackingReadCloser {
	return &trackingReadCloser{reader: strings.NewReader(body)}
}

func (r *trackingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytesRead += n
	return n, err
}

func (r *trackingReadCloser) Close() error {
	r.closed = true
	return nil
}

func assertDeadlineApprox(t *testing.T, got, want time.Duration) {
	t.Helper()
	const tolerance = 15 * time.Second
	if got < want-tolerance || got > want+tolerance {
		t.Fatalf("deadline remaining = %v, want about %v", got, want)
	}
}

func jsonHTTPResponse(body string) *http.Response {
	h := make(http.Header)
	h.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func sseHTTPResponse(body string) *http.Response {
	h := make(http.Header)
	h.Set("Content-Type", "text/event-stream")
	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

func assertOnlySubagentHeaderForwarded(t testing.TB, r *http.Request, want string) {
	t.Helper()
	if got := r.Header.Get("X-OpenAI-Subagent"); got != want {
		t.Fatalf("expected X-OpenAI-Subagent %q, got %q", want, got)
	}
	if got := r.Header.Get("X-Test-Client-Header"); got != "" {
		t.Fatalf("expected X-Test-Client-Header to be stripped, got %q", got)
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

func TestHandleReadyz(t *testing.T) {
	assertProbeAborted := func(t *testing.T, w *httptest.ResponseRecorder) {
		t.Helper()
		if got := w.Header().Get("Content-Type"); got != "" {
			t.Fatalf("expected no content type for aborted probe, got %q", got)
		}
		if got := w.Body.String(); got != "" {
			t.Fatalf("expected no response body for aborted probe, got %q", got)
		}
	}

	t.Run("ready when auth and upstream probe succeed", func(t *testing.T) {
		h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/models" {
				t.Fatalf("expected readiness probe to hit /models, got %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
		})

		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		w := httptest.NewRecorder()

		h.HandleReadyz(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var result map[string]string
		body, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if result["status"] != "ready" {
			t.Fatalf("expected status ready, got %q", result["status"])
		}
		if _, hasError := result["error"]; hasError {
			t.Fatalf("unexpected error field in ready response: %v", result)
		}
	})

	t.Run("not ready when auth fails", func(t *testing.T) {
		authenticator, err := auth.NewAuthenticator(t.TempDir())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		authenticator.DisableAutoDeviceFlow = true

		h := &ProxyHandler{
			auth: authenticator,
			log:  logger.New(logger.LevelInfo),
		}

		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		w := httptest.NewRecorder()

		h.HandleReadyz(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d", resp.StatusCode)
		}

		var result map[string]string
		body, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if result["status"] != "not_ready" {
			t.Fatalf("expected status not_ready, got %q", result["status"])
		}
		if !strings.Contains(result["error"], "failed to get token") {
			t.Fatalf("unexpected error message: %q", result["error"])
		}
	})

	t.Run("not ready when upstream probe fails", func(t *testing.T) {
		h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":"service unavailable"}`))
		})

		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		w := httptest.NewRecorder()

		h.HandleReadyz(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d", resp.StatusCode)
		}

		var result map[string]string
		body, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if result["status"] != "not_ready" {
			t.Fatalf("expected status not_ready, got %q", result["status"])
		}
		if !strings.Contains(result["error"], "upstream probe returned 503") {
			t.Fatalf("unexpected error message: %q", result["error"])
		}
	})

	t.Run("canceled request does not rewrite readiness status", func(t *testing.T) {
		var probeHits atomic.Int32
		h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			probeHits.Add(1)
			t.Fatalf("upstream probe should not be sent for a canceled request")
		})

		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		ctx, cancel := context.WithCancel(req.Context())
		cancel()
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		h.HandleReadyz(w, req)

		if got := probeHits.Load(); got != 0 {
			t.Fatalf("expected no upstream probe, got %d hits", got)
		}
		assertProbeAborted(t, w)
	})

	t.Run("timed out upstream probe does not rewrite readiness status", func(t *testing.T) {
		probeStarted := make(chan struct{})
		probeCanceled := make(chan struct{})

		h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			close(probeStarted)
			<-r.Context().Done()
			close(probeCanceled)
		})

		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		ctx, cancel := context.WithTimeout(req.Context(), 100*time.Millisecond)
		defer cancel()
		req = req.WithContext(ctx)

		w := httptest.NewRecorder()
		done := make(chan struct{})
		go func() {
			defer close(done)
			h.HandleReadyz(w, req)
		}()

		select {
		case <-probeStarted:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for readiness probe to start")
		}

		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for handler to return")
		}

		select {
		case <-probeCanceled:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for upstream probe cancellation")
		}

		assertProbeAborted(t, w)
	})

	t.Run("proxy readiness timeout still returns not ready", func(t *testing.T) {
		h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatalf("unexpected upstream request handler invocation")
		})
		h.client = &http.Client{
			Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
				if err := r.Context().Err(); err != nil {
					t.Fatalf("expected active request context, got %v", err)
				}
				return nil, context.DeadlineExceeded
			}),
		}

		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		w := httptest.NewRecorder()

		h.HandleReadyz(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusServiceUnavailable {
			t.Fatalf("expected 503, got %d", resp.StatusCode)
		}

		body, _ := io.ReadAll(resp.Body)
		var result map[string]string
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if result["status"] != "not_ready" {
			t.Fatalf("expected status not_ready, got %q", result["status"])
		}
		if !strings.Contains(result["error"], "upstream probe failed") {
			t.Fatalf("expected upstream timeout error, got %q", result["error"])
		}
	})
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
	if derefString(anthropicResp.Content[0].Text) != "Hello from the backend!" {
		t.Errorf("expected text 'Hello from the backend!', got %q", derefString(anthropicResp.Content[0].Text))
	}
	if anthropicResp.Usage.InputTokens != 10 {
		t.Errorf("expected input_tokens 10, got %d", anthropicResp.Usage.InputTokens)
	}
	if anthropicResp.Usage.OutputTokens != 5 {
		t.Errorf("expected output_tokens 5, got %d", anthropicResp.Usage.OutputTokens)
	}
}

func TestHandleAnthropicMessages_ImageBlocksForwarded(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("failed to parse upstream request: %v", err)
		}
		if len(oaiReq.Messages) != 1 {
			t.Fatalf("expected 1 upstream message, got %d", len(oaiReq.Messages))
		}

		var parts []models.OpenAIContentPart
		if err := json.Unmarshal(oaiReq.Messages[0].Content, &parts); err != nil {
			t.Fatalf("expected multimodal content array, got error: %v", err)
		}
		if len(parts) != 2 {
			t.Fatalf("expected 2 content parts, got %d", len(parts))
		}
		if parts[0].Type != "text" || parts[0].Text == nil || *parts[0].Text != "What is in this screenshot?" {
			t.Fatalf("parts[0] = %#v, want text part", parts[0])
		}
		if parts[1].Type != "image_url" || parts[1].ImageURL == nil || parts[1].ImageURL.URL != "data:image/png;base64,AQID" {
			t.Fatalf("parts[1] = %#v, want image_url data URL", parts[1])
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-image\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"I can see the screenshot.\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-image\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})

	anthropicReq := `{
		"model": "claude-sonnet-4",
		"max_tokens": 1024,
		"messages": [{
			"role": "user",
			"content": [
				{"type":"text","text":"What is in this screenshot?"},
				{"type":"image","source":{"type":"base64","media_type":"image/png","data":"AQID"}}
			]
		}]
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
	if len(anthropicResp.Content) != 1 || anthropicResp.Content[0].Type != "text" || derefString(anthropicResp.Content[0].Text) != "I can see the screenshot." {
		t.Fatalf("unexpected content: %+v", anthropicResp.Content)
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
		assertOnlySubagentHeaderForwarded(t, r, "review")
		body, _ := io.ReadAll(r.Body)
		var upstreamReq map[string]json.RawMessage
		if err := json.Unmarshal(body, &upstreamReq); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}
		var serviceTier string
		if err := json.Unmarshal(upstreamReq["service_tier"], &serviceTier); err != nil {
			t.Fatalf("upstream request should preserve service_tier: %v", err)
		}
		if serviceTier != "auto" {
			t.Fatalf("expected upstream service_tier auto, got %q", serviceTier)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"resp-123","object":"response","status":"completed"}`))
	})

	responsesReq := `{
		"model": "gpt-4",
		"input": "Hello",
		"service_tier": "auto"
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(responsesReq))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OpenAI-Subagent", "review")
	req.Header.Set("X-Test-Client-Header", "blocked")
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

func TestHandleResponses_RoutesConfiguredAzureModelAndPreservesPriorityServiceTier(t *testing.T) {
	t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

	azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/openai/v1/responses" {
			t.Fatalf("expected Azure path /openai/v1/responses, got %s", got)
		}
		if got := r.URL.RawQuery; got != "" {
			t.Fatalf("expected no Azure query params for /openai/v1 base URL, got %q", got)
		}
		if got := r.Header.Get("api-key"); got != "azure-test-key" {
			t.Fatalf("expected api-key header, got %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("expected no Copilot Authorization header, got %q", got)
		}

		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request body: %v", err)
		}
		var upstreamReq map[string]json.RawMessage
		if err := json.Unmarshal(bodyBytes, &upstreamReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}

		var model string
		if err := json.Unmarshal(upstreamReq["model"], &model); err != nil {
			t.Fatalf("decode upstream model: %v", err)
		}
		if model != "gpt-5-4-prod" {
			t.Fatalf("expected Azure deployment model gpt-5-4-prod, got %q", model)
		}

		var serviceTier string
		if err := json.Unmarshal(upstreamReq["service_tier"], &serviceTier); err != nil {
			t.Fatalf("decode upstream service_tier: %v", err)
		}
		if serviceTier != "priority" {
			t.Fatalf("expected upstream service_tier priority, got %q", serviceTier)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-azure","object":"response","status":"completed","model":"gpt-5-4-prod","output":[]}`))
	}))
	defer azureServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:         "azure",
				Type:       "azure-openai",
				Default:    true,
				BaseURL:    azureServer.URL + "/openai/v1",
				APIKeyEnv:  "TEST_AZURE_API_KEY",
				APIVersion: "preview",
				Models: []ProviderModelConfig{{
					PublicID:   "gpt-5-public",
					Deployment: "gpt-5-4-prod",
					Endpoints:  []string{"/responses"},
					Name:       "GPT-5 Public",
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model": "gpt-5-public",
		"input": "Hello",
		"service_tier": "priority"
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var responseBody struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&responseBody); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if responseBody.ID != "resp-azure" {
		t.Fatalf("unexpected response body: %+v", responseBody)
	}
}

func TestHandleResponses_ForwardsCodexClientHeaders(t *testing.T) {
	var gotOpenAIBeta, gotSessionID, gotClientRequestID string
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotOpenAIBeta = r.Header.Get("OpenAI-Beta")
		gotSessionID = r.Header.Get("session_id")
		gotClientRequestID = r.Header.Get("X-Client-Request-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-headers","object":"response","status":"completed"}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4","input":"Hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	req.Header.Set("session_id", "sess-abc-123")
	req.Header.Set("X-Client-Request-Id", "client-req-456")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	if gotOpenAIBeta != "responses_websockets=2026-02-06" {
		t.Fatalf("expected OpenAI-Beta to be forwarded, got %q", gotOpenAIBeta)
	}
	if gotSessionID != "sess-abc-123" {
		t.Fatalf("expected session_id to be forwarded, got %q", gotSessionID)
	}
	if gotClientRequestID != "client-req-456" {
		t.Fatalf("expected X-Client-Request-Id to be forwarded, got %q", gotClientRequestID)
	}
}

func TestHandleResponses_UpstreamDeadlineDependsOnStreamFlag(t *testing.T) {
	t.Run("non-streaming", func(t *testing.T) {
		deadlineCh := make(chan time.Duration, 1)
		handler := newRoundTripTestProxyHandler(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			if !ok {
				t.Fatal("expected upstream request deadline")
			}
			deadlineCh <- time.Until(deadline)
			return jsonHTTPResponse(`{"id":"resp-non-stream","object":"response","status":"completed"}`), nil
		}))

		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4","input":"Hello","stream":false}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleResponses(w, req)

		if resp := w.Result(); resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		assertDeadlineApprox(t, <-deadlineCh, upstreamTimeout)
	})

	t.Run("streaming", func(t *testing.T) {
		deadlineCh := make(chan time.Duration, 1)
		handler := newRoundTripTestProxyHandler(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			if !ok {
				t.Fatal("expected upstream request deadline")
			}
			deadlineCh <- time.Until(deadline)
			return sseHTTPResponse("data: {\"id\":\"resp-stream\",\"object\":\"response\",\"status\":\"in_progress\"}\n\ndata: [DONE]\n\n"), nil
		}))

		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4","input":"Hello","stream":true}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleResponses(w, req)

		if resp := w.Result(); resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		assertDeadlineApprox(t, <-deadlineCh, streamingUpstreamTimeout)
	})

	t.Run("streaming uses configured timeout", func(t *testing.T) {
		const customTimeout = 17 * time.Minute

		deadlineCh := make(chan time.Duration, 1)
		handler := newRoundTripTestProxyHandler(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
			deadline, ok := r.Context().Deadline()
			if !ok {
				t.Fatal("expected upstream request deadline")
			}
			deadlineCh <- time.Until(deadline)
			return sseHTTPResponse("data: {\"id\":\"resp-stream-custom\",\"object\":\"response\",\"status\":\"in_progress\"}\n\ndata: [DONE]\n\n"), nil
		}))
		WithStreamingUpstreamTimeout(customTimeout)(handler)

		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4","input":"Hello","stream":true}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleResponses(w, req)

		if resp := w.Result(); resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		assertDeadlineApprox(t, <-deadlineCh, customTimeout)
	})
}

func TestHandleResponses_LargeBodyStillRejected(t *testing.T) {
	var upstreamHits atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-too-large","object":"response","status":"completed"}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(makeOversizedResponsesRequestBody(t)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 413, got %d: %s", resp.StatusCode, body)
	}
	if upstreamHits.Load() != 0 {
		t.Fatalf("expected oversized request to be rejected before upstream call, got %d upstream hits", upstreamHits.Load())
	}

	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("request body too large")) {
		t.Fatalf("expected 413 body to mention oversized request, got %s", body)
	}
}

func TestHandleCompact(t *testing.T) {
	priorSummary := "previous compacted context"
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("expected upstream path /responses, got %q", r.URL.Path)
		}
		assertOnlySubagentHeaderForwarded(t, r, "compact")

		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}
		instructions, ok := req["instructions"].(string)
		if !ok || instructions == "" {
			t.Error("expected instructions to be injected for compact")
		}
		input, ok := req["input"].([]interface{})
		if !ok || len(input) != 2 {
			t.Fatalf("expected rewritten input with 2 items, got %#v", req["input"])
		}
		contextText := requireCompactionContextMessage(t, input[0])
		if !strings.Contains(contextText, priorSummary) {
			t.Errorf("expected compacted context in rewritten input, got %q", contextText)
		}

		// Return a standard /responses response — the handler should transform it
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"compacted summary of conversation"}]}]}`))
	})

	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": encodeSyntheticCompaction(priorSummary),
			},
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "Hello"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OpenAI-Subagent", "compact")
	req.Header.Set("X-Test-Client-Header", "blocked")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Output []struct {
			Type             string `json:"type"`
			Role             string `json:"role"`
			EncryptedContent string `json:"encrypted_content"`
			Content          []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(result.Output) != 2 {
		t.Fatalf("expected 2 output items (message + compaction), got %d", len(result.Output))
	}
	// First item: assistant message with summary
	if result.Output[0].Type != "message" {
		t.Errorf("expected first item type message, got %q", result.Output[0].Type)
	}
	if result.Output[0].Role != "assistant" {
		t.Errorf("expected role assistant, got %q", result.Output[0].Role)
	}
	if len(result.Output[0].Content) == 0 || result.Output[0].Content[0].Text != "compacted summary of conversation" {
		t.Errorf("expected summary text in message content, got %+v", result.Output[0].Content)
	}
	// Second item: compaction with encrypted_content
	if result.Output[1].Type != "compaction" {
		t.Errorf("expected second item type compaction, got %q", result.Output[1].Type)
	}
	if got := decodeCompactionSummaryForTest(t, result.Output[1].EncryptedContent); got != "compacted summary of conversation" {
		t.Errorf("expected encoded compaction summary, got %q", got)
	}
}

func TestHandleCompact_LargeBodyAllowed(t *testing.T) {
	var upstreamHits atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if r.URL.Path != "/responses" {
			t.Errorf("expected upstream path /responses, got %q", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		if len(body) <= maxRequestBodySize {
			t.Fatalf("expected forwarded compact body to exceed default limit, got %d bytes", len(body))
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"compacted summary of conversation"}]}]}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(makeOversizedResponsesRequestBody(t)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("expected upstream to receive oversized compact request, got %d hits", upstreamHits.Load())
	}
}

func TestHandleCompact_StripsInlineRenderMarkers(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Keep the passthrough tests. citeturn5view1turn9view0"}]}]}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"gpt-5.4","input":"Hello"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Output []struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
			Content          []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(result.Output) != 2 {
		t.Fatalf("expected 2 output items, got %d", len(result.Output))
	}

	gotText := result.Output[0].Content[0].Text
	if strings.Contains(gotText, "") || strings.Contains(gotText, "") {
		t.Fatalf("expected summary text to be sanitized, got %q", gotText)
	}
	if gotText != "Keep the passthrough tests." {
		t.Errorf("summary text = %q, want %q", gotText, "Keep the passthrough tests.")
	}

	if got := decodeCompactionSummaryForTest(t, result.Output[1].EncryptedContent); got != "Keep the passthrough tests." {
		t.Errorf("encoded compaction summary = %q, want %q", got, "Keep the passthrough tests.")
	}
}

func TestHandleCompact_ReplacesInstructions(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}
		instructions, ok := req["instructions"].(string)
		if !ok {
			t.Fatal("expected instructions to be a string")
		}
		// Instructions should be replaced with compaction prompt, not appended
		if strings.Contains(instructions, "custom prompt") {
			t.Errorf("expected original instructions to be replaced, but they were preserved: %q", instructions)
		}
		if !strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
			t.Errorf("expected compaction prompt as instructions, got %q", instructions)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"custom summary"}]}]}`))
	})

	reqBody := `{"model":"gpt-5.4","input":"Hello","instructions":"custom prompt"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	if w.Result().StatusCode != http.StatusOK {
		body, _ := io.ReadAll(w.Result().Body)
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, body)
	}

	body, _ := io.ReadAll(w.Result().Body)
	var result struct {
		Output []struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse compact response: %v", err)
	}
	if len(result.Output) != 2 {
		t.Fatalf("expected 2 output items, got %d", len(result.Output))
	}
	if result.Output[0].Type != "message" {
		t.Errorf("expected first item type message, got %q", result.Output[0].Type)
	}
	if result.Output[1].Type != "compaction" {
		t.Errorf("expected second item type compaction, got %q", result.Output[1].Type)
	}
	if got := decodeCompactionSummaryForTest(t, result.Output[1].EncryptedContent); got != "custom summary" {
		t.Errorf("expected encoded custom summary, got %q", got)
	}
}

func TestHandleCompact_FallsBackWhenModelUnsupported(t *testing.T) {
	responsesRequests := 0
	modelsRequests := 0

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/responses":
			responsesRequests++

			body, _ := io.ReadAll(r.Body)
			var req map[string]interface{}
			if err := json.Unmarshal(body, &req); err != nil {
				t.Fatalf("upstream received invalid JSON: %v", err)
			}
			model, _ := req["model"].(string)

			switch responsesRequests {
			case 1:
				if model != "gpt-4o" {
					t.Fatalf("expected first compaction attempt to use gpt-4o, got %q", model)
				}
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = w.Write([]byte(`{"error":{"message":"model gpt-4o is not supported via Responses API.","code":"unsupported_api_for_model","param":"model","type":"invalid_request_error"}}`))
			case 2:
				if model != "gpt-5.4" {
					t.Fatalf("expected fallback compaction attempt to use gpt-5.4, got %q", model)
				}
				instructions, _ := req["instructions"].(string)
				if !strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
					t.Fatalf("expected compaction prompt to be preserved on fallback, got %q", instructions)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"fallback summary"}]}]}`))
			default:
				t.Fatalf("unexpected /responses request count %d", responsesRequests)
			}
		case "/models":
			modelsRequests++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5-mini","supported_endpoints":["/responses"]},{"id":"gpt-5.4","supported_endpoints":["/responses"]},{"id":"gpt-4o","supported_endpoints":["/chat/completions"]}]}`))
		default:
			t.Fatalf("unexpected upstream path %q", r.URL.Path)
		}
	})

	reqBody := `{"model":"gpt-4o","input":"Hello"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if responsesRequests != 2 {
		t.Fatalf("expected 2 /responses attempts, got %d", responsesRequests)
	}
	if modelsRequests != 1 {
		t.Fatalf("expected 1 /models lookup, got %d", modelsRequests)
	}

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Output []struct {
			Type             string `json:"type"`
			EncryptedContent string `json:"encrypted_content"`
			Content          []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse compact response: %v", err)
	}
	if len(result.Output) != 2 {
		t.Fatalf("expected 2 output items, got %d", len(result.Output))
	}
	if len(result.Output[0].Content) == 0 || result.Output[0].Content[0].Text != "fallback summary" {
		t.Fatalf("expected fallback summary in first output item, got %+v", result.Output[0].Content)
	}
	if got := decodeCompactionSummaryForTest(t, result.Output[1].EncryptedContent); got != "fallback summary" {
		t.Fatalf("expected encoded fallback summary, got %q", got)
	}
}

func TestHandleResponses_RewritesSyntheticCompaction(t *testing.T) {
	summary := "Synthetic compacted summary"
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}

		input, ok := req["input"].([]interface{})
		if !ok || len(input) != 2 {
			t.Fatalf("expected 2 input items, got %#v", req["input"])
		}

		contextText := requireCompactionContextMessage(t, input[0])
		if !strings.Contains(contextText, summary) {
			t.Errorf("expected rewritten compaction summary, got %q", contextText)
		}
		if got := requireMessageTextWithRole(t, input[1], "user"); got != "continue" {
			t.Errorf("expected original user follow-up to be preserved, got %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-synth","object":"response","status":"completed"}`))
	})

	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": encodeSyntheticCompaction(summary),
			},
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "continue"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	if w.Result().StatusCode != http.StatusOK {
		body, _ := io.ReadAll(w.Result().Body)
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, body)
	}
}

func TestHandleResponses_RewritesSyntheticCompaction_StripsInlineRenderMarkers(t *testing.T) {
	summary := "Synthetic compacted summary. citeturn5view1turn9view0"
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}

		input, ok := req["input"].([]interface{})
		if !ok || len(input) != 2 {
			t.Fatalf("expected 2 input items, got %#v", req["input"])
		}

		contextText := requireCompactionContextMessage(t, input[0])
		if strings.Contains(contextText, "") || strings.Contains(contextText, "") {
			t.Fatalf("expected rewritten compaction summary to be sanitized, got %q", contextText)
		}
		if !strings.Contains(contextText, "Synthetic compacted summary.") {
			t.Errorf("expected sanitized summary in rewritten input, got %q", contextText)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-synth","object":"response","status":"completed"}`))
	})

	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": encodeSyntheticCompaction(summary),
			},
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "continue"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	if w.Result().StatusCode != http.StatusOK {
		body, _ := io.ReadAll(w.Result().Body)
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, body)
	}
}

func TestHandleResponses_RewritesLegacyPlaintextCompaction(t *testing.T) {
	legacySummary := "The previous work fixed auth refresh but left retry handling open."
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}

		input, ok := req["input"].([]interface{})
		if !ok || len(input) != 2 {
			t.Fatalf("expected 2 input items, got %#v", req["input"])
		}

		contextText := requireCompactionContextMessage(t, input[0])
		if !strings.Contains(contextText, legacySummary) {
			t.Errorf("expected legacy summary to be rewritten, got %q", contextText)
		}
		resumePrompt := requireMessageTextWithRole(t, input[1], "user")
		if !strings.Contains(resumePrompt, "Continue from the checkpoint above and resume the interrupted task") {
			t.Errorf("expected resume prompt to be appended, got %q", resumePrompt)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-legacy","object":"response","status":"completed"}`))
	})

	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": legacySummary,
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	if w.Result().StatusCode != http.StatusOK {
		body, _ := io.ReadAll(w.Result().Body)
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, body)
	}
}

func TestHandleResponses_PreservesOpaqueCompaction(t *testing.T) {
	opaqueToken := strings.Repeat("Abc123_-", 8)
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}

		input, ok := req["input"].([]interface{})
		if !ok || len(input) != 1 {
			t.Fatalf("expected 1 input item, got %#v", req["input"])
		}

		item, ok := input[0].(map[string]interface{})
		if !ok {
			t.Fatalf("expected input item object, got %#v", input[0])
		}
		if item["type"] != "compaction" {
			t.Fatalf("expected opaque token to pass through as compaction, got %#v", item)
		}
		if item["encrypted_content"] != opaqueToken {
			t.Errorf("expected opaque token to be preserved, got %v", item["encrypted_content"])
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-opaque","object":"response","status":"completed"}`))
	})

	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": opaqueToken,
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	if w.Result().StatusCode != http.StatusOK {
		body, _ := io.ReadAll(w.Result().Body)
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, body)
	}
}

func TestHandleResponses_RetriesCompacted413Replay(t *testing.T) {
	var upstreamRequestsMu sync.Mutex
	upstreamRequests := make([]map[string]interface{}, 0, 3)
	var normalRequests atomic.Int32

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}

		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("failed to decode upstream request body: %v", err)
		}

		upstreamRequestsMu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		upstreamRequestsMu.Unlock()

		if instructions, _ := body["instructions"].(string); strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"comp-413","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint summary after 413"}]}]}`))
			return
		}

		switch normalRequests.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
		case 2:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-413-retried","object":"response","status":"completed"}`))
		default:
			t.Fatalf("unexpected normal upstream request count %d", normalRequests.Load())
		}
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		DisableAutoCompact:  true,
		AutoCompactKeepTail: 2,
	}

	reqBody, err := json.Marshal(map[string]interface{}{
		"model":                "gpt-5.4",
		"previous_response_id": "resp-prev",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "first turn"},
				},
			},
			map[string]interface{}{
				"type": "message",
				"role": "assistant",
				"content": []map[string]string{
					{"type": "input_text", "text": "first answer"},
				},
			},
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "second turn"},
				},
			},
			map[string]interface{}{
				"type": "message",
				"role": "assistant",
				"content": []map[string]string{
					{"type": "input_text", "text": "second answer"},
				},
			},
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "latest turn"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
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
		t.Fatalf("failed to parse retried response: %v", err)
	}
	if result["id"] != "resp-413-retried" {
		t.Fatalf("expected retried response id resp-413-retried, got %v", result["id"])
	}

	upstreamRequestsMu.Lock()
	requests := append([]map[string]interface{}(nil), upstreamRequests...)
	upstreamRequestsMu.Unlock()

	if len(requests) != 3 {
		t.Fatalf("expected 3 upstream requests (413 + compaction + retry), got %d", len(requests))
	}

	initialReplayInput := upstreamInputItems(t, requests[0])
	if len(initialReplayInput) != 5 {
		t.Fatalf("expected first upstream request to keep full replay, got %d items", len(initialReplayInput))
	}
	if got := requireMessageTextWithRole(t, initialReplayInput[0], "user"); got != "first turn" {
		t.Fatalf("expected first upstream request to start with oldest input, got %q", got)
	}
	if got := requireMessageTextWithRole(t, initialReplayInput[4], "user"); got != "latest turn" {
		t.Fatalf("expected first upstream request to keep latest input, got %q", got)
	}

	compactionInput := upstreamInputItems(t, requests[1])
	if len(compactionInput) != 3 {
		t.Fatalf("expected compaction request to summarize only the replay prefix, got %d items", len(compactionInput))
	}
	if got := requireMessageTextWithRole(t, compactionInput[0], "user"); got != "first turn" {
		t.Fatalf("expected compaction request to preserve oldest user turn, got %q", got)
	}
	if got := requireMessageTextWithRole(t, compactionInput[1], "assistant"); got != "first answer" {
		t.Fatalf("expected compaction request to include first assistant reply, got %q", got)
	}
	if got := requireMessageTextWithRole(t, compactionInput[2], "user"); got != "second turn" {
		t.Fatalf("expected compaction request to stop before kept tail, got %q", got)
	}

	retriedInput := upstreamInputItems(t, requests[2])
	if len(retriedInput) != 3 {
		t.Fatalf("expected retried request to include compacted checkpoint plus tail, got %d items", len(retriedInput))
	}
	if got := requireCompactionContextMessage(t, retriedInput[0]); !strings.Contains(got, "checkpoint summary after 413") {
		t.Fatalf("expected retried request to start with compacted checkpoint, got %q", got)
	}
	if got := requireMessageTextWithRole(t, retriedInput[1], "assistant"); got != "second answer" {
		t.Fatalf("expected retried request to keep assistant tail item, got %q", got)
	}
	if got := requireMessageTextWithRole(t, retriedInput[2], "user"); got != "latest turn" {
		t.Fatalf("expected retried request to keep latest user tail item, got %q", got)
	}
}

func TestHandleResponses_DoesNotRetry413WithoutPreviousResponseID(t *testing.T) {
	var upstreamRequests atomic.Int32

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		DisableAutoCompact:  true,
		AutoCompactKeepTail: 2,
	}

	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "first turn"},
				},
			},
			map[string]interface{}{
				"type": "message",
				"role": "assistant",
				"content": []map[string]string{
					{"type": "input_text", "text": "first answer"},
				},
			},
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "second turn"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 413, got %d: %s", resp.StatusCode, body)
	}
	if upstreamRequests.Load() != 1 {
		t.Fatalf("expected no retry without previous_response_id, got %d upstream requests", upstreamRequests.Load())
	}

	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("payload_too_large")) {
		t.Fatalf("expected original 413 body to be preserved, got %s", body)
	}
}

func TestHandleResponses_DoesNotRetry413WithinKeepTail(t *testing.T) {
	var upstreamRequests atomic.Int32

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
	})
	handler.responsesWS = ResponsesWebSocketConfig{
		DisableAutoCompact:  true,
		AutoCompactKeepTail: 2,
	}

	reqBody, err := json.Marshal(map[string]interface{}{
		"model":                "gpt-5.4",
		"previous_response_id": "resp-prev",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "assistant",
				"content": []map[string]string{
					{"type": "input_text", "text": "second answer"},
				},
			},
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "latest turn"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 413, got %d: %s", resp.StatusCode, body)
	}
	if upstreamRequests.Load() != 1 {
		t.Fatalf("expected no retry when replay already fits keep-tail window, got %d upstream requests", upstreamRequests.Load())
	}

	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("payload_too_large")) {
		t.Fatalf("expected original 413 body to be preserved, got %s", body)
	}
}

func TestHandleMemorySummarize(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("expected upstream path /responses, got %q", r.URL.Path)
		}
		assertOnlySubagentHeaderForwarded(t, r, "memory_consolidation")

		// Return a response with the model's JSON summary
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"[{\"trace_summary\":\"User asked to fix a bug in auth module\",\"memory_summary\":\"Fixed auth bug\"}]"}]}]}`))
	})

	reqBody := `{"model":"gpt-5.4","traces":[{"id":"t1","metadata":{"source_path":"/tmp/trace.json"},"items":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/memories/trace_summarize", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-OpenAI-Subagent", "memory_consolidation")
	req.Header.Set("X-Test-Client-Header", "blocked")
	w := httptest.NewRecorder()

	handler.HandleMemorySummarize(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Output []struct {
			TraceSummary  string `json:"trace_summary"`
			MemorySummary string `json:"memory_summary"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(result.Output) != 1 {
		t.Fatalf("expected 1 output, got %d", len(result.Output))
	}
	if result.Output[0].TraceSummary == "" {
		t.Error("expected non-empty trace_summary")
	}
	if result.Output[0].MemorySummary == "" {
		t.Error("expected non-empty memory_summary")
	}
}

func TestHandleMemorySummarize_LargeBodyAllowed(t *testing.T) {
	var upstreamHits atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		if r.URL.Path != "/responses" {
			t.Errorf("expected upstream path /responses, got %q", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		if len(body) <= maxRequestBodySize {
			t.Fatalf("expected forwarded memory summarize body to exceed default limit, got %d bytes", len(body))
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"[{\"trace_summary\":\"trace\",\"memory_summary\":\"memory\"}]"}]}]}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/memories/trace_summarize", bytes.NewReader(makeOversizedMemorySummarizeRequestBody(t)))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleMemorySummarize(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("expected upstream to receive oversized memory summarize request, got %d hits", upstreamHits.Load())
	}
}

func TestHandleMemorySummarize_StripsInlineRenderMarkers(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("expected upstream path /responses, got %q", r.URL.Path)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"[{\"trace_summary\":\"trace\",\"memory_summary\":\"memory\"}] citeturn5view1turn9view0"}]}]}`))
	})

	reqBody := `{"model":"gpt-5.4","traces":[{"id":"t1","metadata":{"source_path":"/tmp/trace.json"},"items":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/memories/trace_summarize", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleMemorySummarize(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Output []struct {
			TraceSummary  string `json:"trace_summary"`
			MemorySummary string `json:"memory_summary"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(result.Output) != 1 {
		t.Fatalf("expected 1 output, got %d", len(result.Output))
	}
	if got := result.Output[0].TraceSummary; got != "trace" {
		t.Errorf("trace_summary = %q, want %q", got, "trace")
	}
	if got := result.Output[0].MemorySummary; got != "memory" {
		t.Errorf("memory_summary = %q, want %q", got, "memory")
	}
}

func TestHandleMemorySummarize_PassesReasoning(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req map[string]json.RawMessage
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}

		var reasoning map[string]string
		if err := json.Unmarshal(req["reasoning"], &reasoning); err != nil {
			t.Fatalf("expected reasoning object, got %s: %v", req["reasoning"], err)
		}
		if reasoning["effort"] != "high" {
			t.Errorf("reasoning.effort = %q, want %q", reasoning["effort"], "high")
		}
		if reasoning["summary"] != "detailed" {
			t.Errorf("reasoning.summary = %q, want %q", reasoning["summary"], "detailed")
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"[{\"trace_summary\":\"trace\",\"memory_summary\":\"memory\"}]"}]}]}`))
	})

	reqBody := `{"model":"gpt-5.4","traces":[{"id":"t1","metadata":{"source_path":"/tmp/trace.json"},"items":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the bug"}]}]}],"reasoning":{"effort":"high","summary":"detailed"}}`
	req := httptest.NewRequest(http.MethodPost, "/v1/memories/trace_summarize", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleMemorySummarize(w, req)

	if w.Result().StatusCode != http.StatusOK {
		body, _ := io.ReadAll(w.Result().Body)
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, body)
	}
}

func decodeCompactionSummaryForTest(t *testing.T, encryptedContent string) string {
	t.Helper()
	summary, ok := extractSyntheticOrLegacyCompactionSummary(encryptedContent)
	if !ok {
		t.Fatalf("expected synthetic compaction payload, got %q", encryptedContent)
	}
	return summary
}

func requireCompactionContextMessage(t *testing.T, raw interface{}) string {
	t.Helper()
	return requireMessageTextWithRole(t, raw, "developer")
}

func requireMessageTextWithRole(t *testing.T, raw interface{}, wantRole string) string {
	t.Helper()
	item, ok := raw.(map[string]interface{})
	if !ok {
		t.Fatalf("expected message object, got %#v", raw)
	}
	if item["type"] != "message" {
		t.Fatalf("expected rewritten item type message, got %#v", item)
	}
	if item["role"] != wantRole {
		t.Fatalf("expected rewritten item role %s, got %#v", wantRole, item)
	}

	content, ok := item["content"].([]interface{})
	if !ok || len(content) == 0 {
		t.Fatalf("expected message content, got %#v", item["content"])
	}

	part, ok := content[0].(map[string]interface{})
	if !ok {
		t.Fatalf("expected content object, got %#v", content[0])
	}
	if part["type"] != "input_text" {
		t.Fatalf("expected input_text content, got %#v", part)
	}

	text, ok := part["text"].(string)
	if !ok {
		t.Fatalf("expected text content, got %#v", part["text"])
	}
	return text
}

func makeOversizedResponsesRequestBody(t testing.TB) []byte {
	t.Helper()

	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": strings.Repeat("a", maxRequestBodySize),
	})
	if err != nil {
		t.Fatalf("failed to marshal oversized responses request: %v", err)
	}
	if len(reqBody) <= maxRequestBodySize {
		t.Fatalf("expected oversized responses body, got %d bytes", len(reqBody))
	}
	if len(reqBody) > maxLargeRequestBodySize {
		t.Fatalf("expected oversized responses body to stay below large limit, got %d bytes", len(reqBody))
	}
	return reqBody
}

func makeOversizedMemorySummarizeRequestBody(t testing.TB) []byte {
	t.Helper()

	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"traces": []interface{}{
			map[string]interface{}{
				"id": "t1",
				"metadata": map[string]string{
					"source_path": "/tmp/trace.json",
				},
				"items": []interface{}{
					map[string]interface{}{
						"type": "message",
						"role": "user",
						"content": []map[string]string{
							{"type": "input_text", "text": strings.Repeat("a", maxRequestBodySize)},
						},
					},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal oversized memory summarize request: %v", err)
	}
	if len(reqBody) <= maxRequestBodySize {
		t.Fatalf("expected oversized memory summarize body, got %d bytes", len(reqBody))
	}
	if len(reqBody) > maxLargeRequestBodySize {
		t.Fatalf("expected oversized memory summarize body to stay below large limit, got %d bytes", len(reqBody))
	}
	return reqBody
}

func TestRewriteSyntheticCompactionRequest(t *testing.T) {
	syntheticSummary := "Synthetic checkpoint summary"
	legacySummary := "Legacy plaintext summary from an older proxy run."
	opaqueToken := strings.Repeat("Abc123_-", 8)

	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": encodeSyntheticCompaction(syntheticSummary),
			},
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": legacySummary,
			},
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": opaqueToken,
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	rewritten, rewriteCount := rewriteSyntheticCompactionRequest(reqBody)
	if rewriteCount != 2 {
		t.Fatalf("expected 2 rewritten compaction items, got %d", rewriteCount)
	}

	var req map[string]interface{}
	if err := json.Unmarshal(rewritten, &req); err != nil {
		t.Fatalf("failed to parse rewritten request: %v", err)
	}

	input, ok := req["input"].([]interface{})
	if !ok || len(input) != 3 {
		t.Fatalf("expected 3 input items, got %#v", req["input"])
	}

	if got := requireCompactionContextMessage(t, input[0]); !strings.Contains(got, syntheticSummary) {
		t.Errorf("expected synthetic summary to be rewritten, got %q", got)
	}
	if got := requireCompactionContextMessage(t, input[1]); !strings.Contains(got, legacySummary) {
		t.Errorf("expected legacy summary to be rewritten, got %q", got)
	}

	item, ok := input[2].(map[string]interface{})
	if !ok {
		t.Fatalf("expected opaque item object, got %#v", input[2])
	}
	if item["type"] != "compaction" {
		t.Fatalf("expected opaque token to remain a compaction item, got %#v", item)
	}
}

func TestInjectSyntheticCompactionResumePrompt(t *testing.T) {
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			proxyCompactionContextMessage("Checkpoint summary"),
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	rewritten, injected := injectSyntheticCompactionResumePrompt(reqBody)
	if !injected {
		t.Fatal("expected resume prompt to be injected")
	}

	var req map[string]interface{}
	if err := json.Unmarshal(rewritten, &req); err != nil {
		t.Fatalf("failed to parse rewritten request: %v", err)
	}

	input, ok := req["input"].([]interface{})
	if !ok || len(input) != 2 {
		t.Fatalf("expected 2 input items, got %#v", req["input"])
	}
	if got := requireMessageTextWithRole(t, input[1], "user"); !strings.Contains(got, "Continue from the checkpoint above and resume the interrupted task") {
		t.Fatalf("expected injected resume prompt, got %q", got)
	}
}

func TestInjectSyntheticCompactionResumePrompt_IgnoresHistoricalUserMessagesBeforeCheckpoint(t *testing.T) {
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "Run /review on my current changes"},
				},
			},
			proxyCompactionContextMessage("Checkpoint summary"),
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	rewritten, injected := injectSyntheticCompactionResumePrompt(reqBody)
	if !injected {
		t.Fatal("expected resume prompt to be injected when only historical user messages remain")
	}

	var req map[string]interface{}
	if err := json.Unmarshal(rewritten, &req); err != nil {
		t.Fatalf("failed to parse rewritten request: %v", err)
	}

	input, ok := req["input"].([]interface{})
	if !ok || len(input) != 3 {
		t.Fatalf("expected 3 input items, got %#v", req["input"])
	}
	if got := requireMessageTextWithRole(t, input[0], "user"); got != "Run /review on my current changes" {
		t.Fatalf("expected historical user message to be preserved, got %q", got)
	}
	if got := requireMessageTextWithRole(t, input[2], "user"); !strings.Contains(got, "Continue from the checkpoint above and resume the interrupted task") {
		t.Fatalf("expected injected resume prompt, got %q", got)
	}
}

func TestInjectSyntheticCompactionResumePrompt_SkipsWhenUserMessageExists(t *testing.T) {
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			proxyCompactionContextMessage("Checkpoint summary"),
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "continue"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	rewritten, injected := injectSyntheticCompactionResumePrompt(reqBody)
	if injected {
		t.Fatal("expected resume prompt injection to be skipped")
	}
	if !bytes.Equal(rewritten, reqBody) {
		t.Fatal("expected request body to remain unchanged when user message exists")
	}
}

func TestExtractSyntheticOrLegacyCompactionSummary(t *testing.T) {
	summary := "Compacted conversation summary"
	if got, ok := extractSyntheticOrLegacyCompactionSummary(encodeSyntheticCompaction(summary)); !ok || got != summary {
		t.Fatalf("expected synthetic summary round-trip, got %q ok=%v", got, ok)
	}

	legacySyntheticSummary := "Legacy synthetic compaction summary"
	legacyPayload, err := json.Marshal(syntheticCompactionPayload{Summary: legacySyntheticSummary})
	if err != nil {
		t.Fatalf("marshal legacy synthetic payload: %v", err)
	}
	legacyEncoded := legacySyntheticCompactionPrefix + base64.RawURLEncoding.EncodeToString(legacyPayload)
	if got, ok := extractSyntheticOrLegacyCompactionSummary(legacyEncoded); !ok || got != legacySyntheticSummary {
		t.Fatalf("expected legacy synthetic summary round-trip, got %q ok=%v", got, ok)
	}

	legacySummary := "The issue is partially fixed."
	if got, ok := extractSyntheticOrLegacyCompactionSummary(legacySummary); !ok || got != legacySummary {
		t.Fatalf("expected legacy summary salvage, got %q ok=%v", got, ok)
	}

	opaqueToken := strings.Repeat("Abc123_-", 8)
	if got, ok := extractSyntheticOrLegacyCompactionSummary(opaqueToken); ok {
		t.Fatalf("expected opaque token to pass through unchanged, got %q", got)
	}
}

func TestHandleMemorySummarize_FallbackOnInvalidJSON(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		// Model returns plain text instead of JSON
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"This is a plain text summary, not JSON"}]}]}`))
	})

	reqBody := `{"model":"gpt-5.4","traces":[{"id":"t1","metadata":{"source_path":"/tmp/trace.json"},"items":[]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/memories/trace_summarize", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleMemorySummarize(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Output []struct {
			TraceSummary  string `json:"trace_summary"`
			MemorySummary string `json:"memory_summary"`
		} `json:"output"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("failed to parse response: %v", err)
	}
	if len(result.Output) != 1 {
		t.Fatalf("expected 1 output, got %d", len(result.Output))
	}
	// Fallback: raw text used for both fields
	if result.Output[0].TraceSummary == "" {
		t.Error("expected fallback trace_summary")
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

func TestSetCopilotHeadersWithConfig(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	setCopilotHeadersWithConfig(req, "my-test-token", CopilotHeaderConfig{
		EditorVersion:       "vscode/1.96.0",
		EditorPluginVersion: "copilot-chat/0.27.0",
		UserAgent:           "GitHubCopilotChat/0.27.0",
		GitHubAPIVersion:    "2025-05-01",
	})

	tests := []struct {
		header   string
		expected string
	}{
		{"Authorization", "Bearer my-test-token"},
		{"editor-version", "vscode/1.96.0"},
		{"editor-plugin-version", "copilot-chat/0.27.0"},
		{"user-agent", "GitHubCopilotChat/0.27.0"},
		{"copilot-integration-id", defaultCopilotIntegrationID},
		{"x-github-api-version", "2025-05-01"},
		{"openai-intent", defaultCopilotOpenAIIntent},
		{"Content-Type", "application/json"},
	}

	for _, tt := range tests {
		got := req.Header.Get(tt.header)
		if got != tt.expected {
			t.Errorf("header %q: expected %q, got %q", tt.header, tt.expected, got)
		}
	}
}

func TestPostJSONEndpointWithHeaders_ProxyHeadersTakePrecedence(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Fatalf("expected Authorization header from proxy, got %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("expected Content-Type header from proxy, got %q", got)
		}
		if got := r.Header.Get("Traceparent"); got != "00-11111111111111111111111111111111-2222222222222222-01" {
			t.Fatalf("expected passthrough header to survive merge, got %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	resp, err := handler.postJSONEndpointWithHeaders(
		context.Background(),
		"/responses",
		[]byte(`{"input":"hello"}`),
		http.Header{
			"Authorization": []string{"Bearer wrong-token"},
			"Content-Type":  []string{"text/plain"},
			"Traceparent":   []string{"00-11111111111111111111111111111111-2222222222222222-01"},
		},
	)
	if err != nil {
		t.Fatalf("postJSONEndpointWithHeaders returned error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
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

func TestHandleOpenAIChatCompletions_RoutesConfiguredAzureModel(t *testing.T) {
	t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

	azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/openai/v1/chat/completions" {
			t.Fatalf("expected Azure path /openai/v1/chat/completions, got %s", got)
		}
		if got := r.URL.RawQuery; got != "" {
			t.Fatalf("expected no Azure query params for /openai/v1 base URL, got %q", got)
		}
		if got := r.Header.Get("api-key"); got != "azure-test-key" {
			t.Fatalf("expected api-key header, got %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("expected no Copilot Authorization header, got %q", got)
		}

		var upstreamReq models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if upstreamReq.Model != "gpt-5-4-prod" {
			t.Fatalf("expected Azure deployment model gpt-5-4-prod, got %q", upstreamReq.Model)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID:     "chatcmpl-1",
			Object: "chat.completion",
			Choices: []models.OpenAIChoice{{
				Index: 0,
				Message: models.OpenAIMessage{
					Role:    "assistant",
					Content: json.RawMessage(`"Hi"`),
				},
			}},
		})
	}))
	defer azureServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:         "azure",
				Type:       "azure-openai",
				Default:    true,
				BaseURL:    azureServer.URL + "/openai/v1",
				APIKeyEnv:  "TEST_AZURE_API_KEY",
				APIVersion: "preview",
				Models: []ProviderModelConfig{{
					PublicID:   "gpt-5.4",
					Deployment: "gpt-5-4-prod",
					Endpoints:  []string{"/chat/completions", "/responses"},
					Name:       "GPT-5.4",
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model": "gpt-5.4",
		"messages": [{"role": "user", "content": "Hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var oaiResp models.OpenAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&oaiResp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(oaiResp.Choices) != 1 || string(oaiResp.Choices[0].Message.Content) != `"Hi"` {
		t.Fatalf("unexpected response body: %+v", oaiResp)
	}
}

func TestHandleOpenAIChatCompletions_RejectsConfiguredAzureModelWithoutChatSupport(t *testing.T) {
	t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:        "azure",
				Type:      "azure-openai",
				Default:   true,
				BaseURL:   "https://example.openai.azure.com/openai",
				APIKeyEnv: "TEST_AZURE_API_KEY",
				Models: []ProviderModelConfig{{
					PublicID:   "gpt-5.4-pro",
					Deployment: "gpt-5.4-pro",
					Endpoints:  []string{"/responses"},
					Name:       "GPT-5.4 Pro",
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model": "gpt-5.4-pro",
		"messages": [{"role": "user", "content": "Hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}

	var errResp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if !strings.Contains(errResp.Error.Message, `does not support /chat/completions`) {
		t.Fatalf("expected unsupported endpoint message, got %q", errResp.Error.Message)
	}
}

func TestHandleResponses_RejectsConfiguredAzureModelWithoutResponsesSupport(t *testing.T) {
	t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:        "azure",
				Type:      "azure-openai",
				Default:   true,
				BaseURL:   "https://example.openai.azure.com/openai",
				APIKeyEnv: "TEST_AZURE_API_KEY",
				Models: []ProviderModelConfig{{
					PublicID:   "gpt-5.4-pro",
					Deployment: "gpt-5.4-pro",
					Endpoints:  []string{"/chat/completions"},
					Name:       "GPT-5.4 Pro",
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model": "gpt-5.4-pro",
		"input": "Hello"
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}

	var errResp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if !strings.Contains(errResp.Error.Message, `does not support /responses`) {
		t.Fatalf("expected unsupported endpoint message, got %q", errResp.Error.Message)
	}
}

func TestHandleResponses_RoutesConfiguredOpenAICodexProvider(t *testing.T) {
	tokens := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-123", true, "refresh-token")
	codexHome := t.TempDir()
	writeTestOpenAICodexAuth(t, codexHome, tokens)
	t.Setenv("CODEX_HOME", codexHome)

	codexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/responses" {
			t.Fatalf("expected OpenAI Codex path /responses, got %s", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer "+tokens.AccessToken {
			t.Fatalf("expected Codex Authorization header, got %q", got)
		}
		if got := r.Header.Get("ChatGPT-Account-ID"); got != "acct-123" {
			t.Fatalf("expected ChatGPT-Account-ID acct-123, got %q", got)
		}
		if got := r.Header.Get("X-OpenAI-Fedramp"); got != "true" {
			t.Fatalf("expected X-OpenAI-Fedramp true, got %q", got)
		}
		if got := r.Header.Get("X-Codex-Turn-State"); got != "sticky-turn-state" {
			t.Fatalf("expected X-Codex-Turn-State passthrough, got %q", got)
		}
		if got := r.Header.Get("api-key"); got != "" {
			t.Fatalf("expected no Azure api-key header, got %q", got)
		}

		var upstreamReq struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if upstreamReq.Model != "gpt-5.5" {
			t.Fatalf("upstream model = %q, want gpt-5.5", upstreamReq.Model)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-codex","object":"response","status":"completed"}`))
	}))
	defer codexServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:      "codex",
				Type:    "openai-codex",
				Default: true,
				BaseURL: codexServer.URL,
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"Hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Turn-State", "sticky-turn-state")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var responseBody struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&responseBody); err != nil {
		t.Fatalf("decode responses body: %v", err)
	}
	if responseBody.ID != "resp-codex" {
		t.Fatalf("expected OpenAI Codex response, got %+v", responseBody)
	}
}

func TestHandleOpenAIChatCompletions_RejectsOpenAICodexProvider(t *testing.T) {
	codexHome := t.TempDir()
	writeTestOpenAICodexAuth(t, codexHome, testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-123", false, "refresh-token"))
	t.Setenv("CODEX_HOME", codexHome)

	codexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("OpenAI Codex upstream should not be called for chat completions")
	}))
	defer codexServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:      "codex",
				Type:    "openai-codex",
				Default: true,
				BaseURL: codexServer.URL,
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model": "gpt-5.5",
		"messages": [{"role": "user", "content": "Hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}

	var errResp struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if !strings.Contains(errResp.Error.Message, `provider "codex" does not support /chat/completions`) {
		t.Fatalf("expected provider unsupported endpoint message, got %q", errResp.Error.Message)
	}
}

func TestNewProxyHandler_FailsWhenProvidersSharePlainModelID(t *testing.T) {
	t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

	copilotServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/models" {
			t.Fatalf("expected /models lookup, got %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.4","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/responses"],"name":"GPT-5.4"}]}`))
	}))
	defer copilotServer.Close()

	_, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		withCopilotBaseURLForTest(copilotServer.URL),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{
					ID:      "copilot",
					Type:    "copilot",
					Default: true,
				},
				{
					ID:        "azure",
					Type:      "azure-openai",
					BaseURL:   "https://example.openai.azure.com/openai/v1",
					APIKeyEnv: "TEST_AZURE_API_KEY",
					Models: []ProviderModelConfig{{
						PublicID:   "gpt-5.4",
						Deployment: "gpt-5-4-prod",
					}},
				},
			},
		}),
	)
	if err == nil {
		t.Fatal("expected provider model collision error")
	}
	if !strings.Contains(err.Error(), "gpt-5.4") {
		t.Fatalf("expected model id in error, got %v", err)
	}
	if !strings.Contains(err.Error(), "copilot") || !strings.Contains(err.Error(), "azure") {
		t.Fatalf("expected both provider ids in error, got %v", err)
	}
}

func TestNewProxyHandler_FailsWhenOpenAICodexModelCollidesWithAzure(t *testing.T) {
	t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")
	codexHome := t.TempDir()
	writeTestOpenAICodexAuth(t, codexHome, testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-123", false, "refresh-token"))
	t.Setenv("CODEX_HOME", codexHome)

	codexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/models" {
			t.Fatalf("expected /models lookup, got %s", got)
		}
		if got := r.URL.Query().Get("client_version"); got != defaultOpenAICodexClientVersion {
			t.Fatalf("client_version = %q, want %q", got, defaultOpenAICodexClientVersion)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.4","display_name":"GPT-5.4","visibility":"list","supported_in_api":true,"supported_reasoning_levels":[{"effort":"medium"}],"context_window":128000}]}`))
	}))
	defer codexServer.Close()

	_, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{
					ID:      "codex",
					Type:    "openai-codex",
					BaseURL: codexServer.URL,
				},
				{
					ID:        "azure",
					Type:      "azure-openai",
					Default:   true,
					BaseURL:   "https://example.openai.azure.com/openai/v1",
					APIKeyEnv: "TEST_AZURE_API_KEY",
					Models: []ProviderModelConfig{{
						PublicID:   "gpt-5.4",
						Deployment: "gpt-5-4-prod",
					}},
				},
			},
		}),
	)
	if err == nil {
		t.Fatal("expected provider model collision error")
	}
	if !strings.Contains(err.Error(), "gpt-5.4") {
		t.Fatalf("expected model id in error, got %v", err)
	}
	if !strings.Contains(err.Error(), "codex") || !strings.Contains(err.Error(), "azure") {
		t.Fatalf("expected both provider ids in error, got %v", err)
	}
	if !strings.Contains(err.Error(), "include_models") || !strings.Contains(err.Error(), "exclude_models") {
		t.Fatalf("expected include/exclude guidance in error, got %v", err)
	}
}

func TestNewProxyHandler_CopilotExcludeAndOpenAICodexIncludeAvoidsCollision(t *testing.T) {
	codexHome := t.TempDir()
	writeTestOpenAICodexAuth(t, codexHome, testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-123", false, "refresh-token"))
	t.Setenv("CODEX_HOME", codexHome)

	copilotServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/models" {
			t.Fatalf("expected Copilot /models lookup, got %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"gpt-5.4","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/responses"],"name":"GPT-5.4"},
			{"id":"gpt-5.5","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/responses"],"name":"GPT-5.5 Copilot"}
		]}`))
	}))
	defer copilotServer.Close()

	codexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/models" {
			t.Fatalf("expected OpenAI Codex /models lookup, got %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[
			{"slug":"gpt-5.4","display_name":"GPT-5.4","visibility":"list","supported_in_api":true},
			{"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list","supported_in_api":true,"supported_reasoning_levels":[{"effort":"medium"}],"context_window":272000}
		]}`))
	}))
	defer codexServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		withCopilotBaseURLForTest(copilotServer.URL),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{
					ID:            "copilot",
					Type:          "copilot",
					Default:       true,
					ExcludeModels: []string{"gpt-5.5"},
				},
				{
					ID:            "codex",
					Type:          "openai-codex",
					BaseURL:       codexServer.URL,
					IncludeModels: []string{"gpt-5.5"},
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	handler.HandleModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode models response: %v", err)
	}

	ownersByID := make(map[string][]string)
	for _, model := range result.Data {
		ownersByID[model.ID] = append(ownersByID[model.ID], model.OwnedBy)
	}
	if _, ok := ownersByID["gpt-5.4"]; !ok {
		t.Fatalf("expected Copilot model gpt-5.4 in merged catalog, got %+v", ownersByID)
	}
	owners := ownersByID["gpt-5.5"]
	if len(owners) != 1 {
		t.Fatalf("expected exactly one gpt-5.5 entry, got owners %v in %+v", owners, ownersByID)
	}
	if owners[0] != "codex" {
		t.Fatalf("expected gpt-5.5 to come from codex after Copilot exclusion, got owner %q", owners[0])
	}
}

func TestNewProxyHandler_OpenAICodexIncludeModelsAvoidsCollision(t *testing.T) {
	t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")
	codexHome := t.TempDir()
	writeTestOpenAICodexAuth(t, codexHome, testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-123", false, "refresh-token"))
	t.Setenv("CODEX_HOME", codexHome)

	codexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"models":[
				{"slug":"gpt-5.4","display_name":"GPT-5.4","visibility":"list","supported_in_api":true},
				{"slug":"gpt-5.5","display_name":"GPT-5.5","visibility":"list","supported_in_api":true,"supported_reasoning_levels":[{"effort":"low"},{"effort":"medium"},{"effort":"high"}],"supports_parallel_tool_calls":true,"context_window":272000,"input_modalities":["text","image"],"priority":0}
			]}`))
		default:
			t.Fatalf("unexpected OpenAI Codex path %q", r.URL.Path)
		}
	}))
	defer codexServer.Close()

	azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/models" {
			t.Fatalf("unexpected Azure path %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer azureServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{
					ID:            "codex",
					Type:          "openai-codex",
					BaseURL:       codexServer.URL,
					IncludeModels: []string{"gpt-5.5"},
				},
				{
					ID:        "azure",
					Type:      "azure-openai",
					Default:   true,
					BaseURL:   azureServer.URL + "/openai/v1",
					APIKeyEnv: "TEST_AZURE_API_KEY",
					Models: []ProviderModelConfig{{
						PublicID:   "gpt-5.4",
						Deployment: "gpt-5-4-prod",
						Endpoints:  []string{"/responses"},
					}},
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	handler.HandleModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Data []struct {
			ID                 string   `json:"id"`
			SupportedEndpoints []string `json:"supported_endpoints"`
		} `json:"data"`
		Models []struct {
			Slug                      string  `json:"slug"`
			DefaultReasoningLevel     *string `json:"default_reasoning_level,omitempty"`
			SupportsParallelToolCalls bool    `json:"supports_parallel_tool_calls"`
			ContextWindow             *int64  `json:"context_window,omitempty"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode models response: %v", err)
	}

	ids := make(map[string][]string)
	for _, model := range result.Data {
		ids[model.ID] = model.SupportedEndpoints
	}
	if _, ok := ids["gpt-5.4"]; !ok {
		t.Fatalf("expected Azure model gpt-5.4 in merged catalog, got %+v", ids)
	}
	if endpoints, ok := ids["gpt-5.5"]; !ok {
		t.Fatalf("expected OpenAI Codex model gpt-5.5 in merged catalog, got %+v", ids)
	} else if got := strings.Join(endpoints, ","); got != "/responses" {
		t.Fatalf("expected gpt-5.5 responses-only endpoint, got %q", got)
	}

	var codexModel *struct {
		Slug                      string  `json:"slug"`
		DefaultReasoningLevel     *string `json:"default_reasoning_level,omitempty"`
		SupportsParallelToolCalls bool    `json:"supports_parallel_tool_calls"`
		ContextWindow             *int64  `json:"context_window,omitempty"`
	}
	for i := range result.Models {
		if result.Models[i].Slug == "gpt-5.5" {
			codexModel = &result.Models[i]
			break
		}
	}
	if codexModel == nil {
		t.Fatalf("expected Codex model metadata for gpt-5.5, got %+v", result.Models)
	}
	if codexModel.DefaultReasoningLevel == nil || *codexModel.DefaultReasoningLevel != "medium" {
		t.Fatalf("expected default reasoning medium, got %v", codexModel.DefaultReasoningLevel)
	}
	if !codexModel.SupportsParallelToolCalls {
		t.Fatal("expected supports_parallel_tool_calls true")
	}
	if codexModel.ContextWindow == nil || *codexModel.ContextWindow != 272000 {
		t.Fatalf("expected context_window 272000, got %v", codexModel.ContextWindow)
	}
}

func TestNewProxyHandler_RejectsMultipleCopilotProviders(t *testing.T) {
	_, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{
					ID:      "copilot",
					Type:    "copilot",
					Default: true,
				},
				{
					ID:   "copilot-secondary",
					Type: "copilot",
				},
			},
		}),
	)
	if err == nil {
		t.Fatal("expected multiple copilot providers to be rejected")
	}
	if !strings.Contains(err.Error(), "multiple copilot providers configured") {
		t.Fatalf("expected multiple copilot providers error, got %v", err)
	}
}

func TestNewProxyHandler_AllowsDuplicateModelIDsWithinSameProvider(t *testing.T) {
	t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

	copilotServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/models" {
			t.Fatalf("expected /models lookup, got %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[
			{"id":"gpt-4","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/chat/completions"],"name":"GPT-4"},
			{"id":"gpt-4","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/responses"],"name":"GPT-4"},
			{"id":"gpt-4o","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/responses"],"name":"GPT-4o"}
		]}`))
	}))
	defer copilotServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		withCopilotBaseURLForTest(copilotServer.URL),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{
					ID:      "copilot",
					Type:    "copilot",
					Default: true,
				},
				{
					ID:        "azure",
					Type:      "azure-openai",
					BaseURL:   "https://example.openai.azure.com/openai/v1",
					APIKeyEnv: "TEST_AZURE_API_KEY",
					Models: []ProviderModelConfig{{
						PublicID:   "gpt-5.4-pro",
						Deployment: "gpt-5-4-pro",
					}},
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("expected duplicate IDs within one provider to be deduped, got %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	handler.HandleModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Data []struct {
			ID                 string   `json:"id"`
			SupportedEndpoints []string `json:"supported_endpoints"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode models response: %v", err)
	}

	seen := make(map[string]int)
	var gpt4Endpoints []string
	for _, model := range result.Data {
		seen[model.ID]++
		if model.ID == "gpt-4" {
			gpt4Endpoints = model.SupportedEndpoints
		}
	}
	if seen["gpt-4"] != 1 {
		t.Fatalf("expected deduped gpt-4 entry once, got %+v", seen)
	}
	if !supportsEndpoint(gpt4Endpoints, "/chat/completions") || !supportsEndpoint(gpt4Endpoints, "/responses") {
		t.Fatalf("expected merged gpt-4 endpoints, got %+v", gpt4Endpoints)
	}
}

func TestHandleResponses_AllowsMergedDuplicateDynamicProviderEndpoints(t *testing.T) {
	t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

	var responsesCalls int32
	copilotServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[
				{"id":"gpt-4","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/chat/completions"],"name":"GPT-4"},
				{"id":"gpt-4","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/responses"],"name":"GPT-4"}
			]}`))
		case "/responses":
			atomic.AddInt32(&responsesCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-copilot","object":"response","status":"completed"}`))
		default:
			t.Fatalf("unexpected Copilot path %q", r.URL.Path)
		}
	}))
	defer copilotServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		withCopilotBaseURLForTest(copilotServer.URL),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{
					ID:      "copilot",
					Type:    "copilot",
					Default: true,
				},
				{
					ID:        "azure",
					Type:      "azure-openai",
					BaseURL:   "https://example.openai.azure.com/openai/v1",
					APIKeyEnv: "TEST_AZURE_API_KEY",
					Models: []ProviderModelConfig{{
						PublicID:   "gpt-5.4-pro",
						Deployment: "gpt-5-4-pro",
						Endpoints:  []string{"/responses"},
					}},
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4","input":"Hello"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var responseBody struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&responseBody); err != nil {
		t.Fatalf("decode responses body: %v", err)
	}
	if responseBody.ID != "resp-copilot" {
		t.Fatalf("expected Copilot response, got %+v", responseBody)
	}
	if got := atomic.LoadInt32(&responsesCalls); got != 1 {
		t.Fatalf("expected one Copilot responses request, got %d", got)
	}
}

func TestHandleModels_RefreshesDynamicProviderOwnershipForRouting(t *testing.T) {
	t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

	var modelsCalls int32
	var copilotResponses int32
	var azureResponses int32

	copilotServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			call := atomic.AddInt32(&modelsCalls, 1)
			w.Header().Set("Content-Type", "application/json")
			switch call {
			case 1:
				_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/responses"],"name":"GPT-4"}]}`))
			default:
				_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-new","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/responses"],"name":"GPT New"}]}`))
			}
		case "/responses":
			atomic.AddInt32(&copilotResponses, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-copilot","object":"response","status":"completed"}`))
		default:
			t.Fatalf("unexpected Copilot path %q", r.URL.Path)
		}
	}))
	defer copilotServer.Close()

	azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/openai/v1/models":
			w.WriteHeader(http.StatusInternalServerError)
		case "/openai/v1/responses":
			atomic.AddInt32(&azureResponses, 1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-azure","object":"response","status":"completed"}`))
		default:
			t.Fatalf("unexpected Azure path %q", r.URL.Path)
		}
	}))
	defer azureServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		withCopilotBaseURLForTest(copilotServer.URL),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{
					ID:   "copilot",
					Type: "copilot",
				},
				{
					ID:        "azure",
					Type:      "azure-openai",
					Default:   true,
					BaseURL:   azureServer.URL + "/openai/v1",
					APIKeyEnv: "TEST_AZURE_API_KEY",
					Models: []ProviderModelConfig{{
						PublicID:   "gpt-5.4-pro",
						Deployment: "gpt-5-4-pro",
						Endpoints:  []string{"/responses"},
					}},
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	modelsReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsW := httptest.NewRecorder()
	handler.HandleModels(modelsW, modelsReq)

	modelsResp := modelsW.Result()
	if modelsResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(modelsResp.Body)
		t.Fatalf("expected models refresh 200, got %d: %s", modelsResp.StatusCode, body)
	}

	var modelsBody struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(modelsResp.Body).Decode(&modelsBody); err != nil {
		t.Fatalf("decode refreshed models response: %v", err)
	}
	if len(modelsBody.Data) == 0 || modelsBody.Data[0].ID != "gpt-new" {
		t.Fatalf("expected refreshed models to include gpt-new, got %+v", modelsBody.Data)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-new","input":"Hello"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var responseBody struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&responseBody); err != nil {
		t.Fatalf("decode responses body: %v", err)
	}
	if responseBody.ID != "resp-copilot" {
		t.Fatalf("expected Copilot routing after refresh, got %+v", responseBody)
	}
	if got := atomic.LoadInt32(&copilotResponses); got != 1 {
		t.Fatalf("expected one Copilot responses request, got %d", got)
	}
	if got := atomic.LoadInt32(&azureResponses); got != 0 {
		t.Fatalf("expected no Azure responses requests, got %d", got)
	}
}

func TestNewProviderJSONRequest_OmitsBodyForGetNilPayload(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {})

	req, err := handler.newProviderJSONRequest(
		context.Background(),
		&providerRuntime{
			id:      "copilot",
			kind:    providerTypeCopilot,
			baseURL: "http://example.test",
		},
		http.MethodGet,
		"/models",
		nil,
		nil,
		"",
	)
	if err != nil {
		t.Fatalf("newProviderJSONRequest returned error: %v", err)
	}
	if req.Body != nil && req.Body != http.NoBody {
		t.Fatalf("expected no GET body, got %T", req.Body)
	}
	if req.GetBody != nil {
		t.Fatal("expected GetBody to be nil for GET without payload")
	}
	if req.ContentLength != 0 {
		t.Fatalf("expected ContentLength 0, got %d", req.ContentLength)
	}
}

func TestFetchProviderModels_AzureNonOKDrainsResponseBody(t *testing.T) {
	body := newTrackingReadCloser("azure metadata unavailable")
	handler := newRoundTripTestProxyHandler(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if got := r.URL.Path; got != "/openai/v1/models" {
			t.Fatalf("expected /openai/v1/models, got %s", got)
		}
		return &http.Response{
			StatusCode: http.StatusBadRequest,
			Header:     make(http.Header),
			Body:       body,
		}, nil
	}))

	result, err := handler.fetchProviderModels(context.Background(), &providerRuntime{
		id:      "azure",
		kind:    providerTypeAzureOpenAI,
		baseURL: "http://example.test/openai/v1",
	}, "", "")
	if err != nil {
		t.Fatalf("fetchProviderModels returned error: %v", err)
	}
	if len(result.models) != 0 {
		t.Fatalf("expected no models, got %+v", result.models)
	}
	if !body.closed {
		t.Fatal("expected Azure /models body to be closed")
	}
	if body.bytesRead == 0 {
		t.Fatal("expected Azure /models body to be drained before close")
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
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.4","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/responses"],"capabilities":{"supports":{"parallel_tool_calls":true,"vision":true,"reasoning_effort":["low","medium","high"]},"limits":{"max_context_window_tokens":128000}},"model_picker_enabled":true,"model_picker_category":"powerful","name":"GPT-5.4"},{"id":"claude-sonnet-4","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/chat/completions","/v1/messages"],"name":"Claude Sonnet 4","model_picker_enabled":true,"model_picker_category":"versatile"}]}`))
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
			Models []struct {
				Slug                      string `json:"slug"`
				DisplayName               string `json:"display_name"`
				Visibility                string `json:"visibility"`
				SupportedInAPI            bool   `json:"supported_in_api"`
				ContextWindow             *int64 `json:"context_window"`
				SupportsParallelToolCalls bool   `json:"supports_parallel_tool_calls"`
				ShellType                 string `json:"shell_type"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if result.Object != "list" {
			t.Errorf("expected object list, got %q", result.Object)
		}
		if len(result.Data) != 2 {
			t.Fatalf("expected 2 data entries, got %d", len(result.Data))
		}
		if result.Data[0].ID != "gpt-5.4" {
			t.Errorf("expected first model gpt-5.4, got %q", result.Data[0].ID)
		}
		// Verify Codex-compatible models field
		if len(result.Models) != 2 {
			t.Fatalf("expected 2 models entries, got %d", len(result.Models))
		}
		if result.Models[0].Slug != "gpt-5.4" {
			t.Errorf("expected first model slug gpt-5.4, got %q", result.Models[0].Slug)
		}
		if result.Models[0].DisplayName != "GPT-5.4" {
			t.Errorf("expected display_name GPT-5.4, got %q", result.Models[0].DisplayName)
		}
		if result.Models[0].Visibility != "list" {
			t.Errorf("expected visibility list, got %q", result.Models[0].Visibility)
		}
		if !result.Models[0].SupportedInAPI {
			t.Error("expected first model supported_in_api true")
		}
		if result.Models[0].ContextWindow == nil || *result.Models[0].ContextWindow != 128000 {
			t.Errorf("expected context_window 128000, got %v", result.Models[0].ContextWindow)
		}
		if !result.Models[0].SupportsParallelToolCalls {
			t.Error("expected supports_parallel_tool_calls true")
		}
		if result.Models[0].ShellType != "shell_command" {
			t.Errorf("expected shell_type shell_command, got %q", result.Models[0].ShellType)
		}
		// Second model should have visibility "hide" (model_picker_enabled not set)
		if result.Models[1].Visibility != "hide" {
			t.Errorf("expected second model visibility hide, got %q", result.Models[1].Visibility)
		}
		if result.Models[1].SupportedInAPI {
			t.Error("expected second model supported_in_api false")
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

	t.Run("empty data still returns models array", func(t *testing.T) {
		h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
		})
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()

		h.HandleModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		body, _ := io.ReadAll(resp.Body)
		if !bytes.Contains(body, []byte(`"models":[]`)) {
			t.Fatalf("expected transformed response to include empty models array, got %s", body)
		}

		var result struct {
			Data   []json.RawMessage `json:"data"`
			Models []json.RawMessage `json:"models"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if len(result.Data) != 0 {
			t.Fatalf("expected 0 data entries, got %d", len(result.Data))
		}
		if result.Models == nil {
			t.Fatal("expected models to be an empty array, got nil")
		}
		if len(result.Models) != 0 {
			t.Fatalf("expected 0 models entries, got %d", len(result.Models))
		}
	})

	t.Run("default reasoning falls back to first supported level", func(t *testing.T) {
		h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5-thinking","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/responses"],"capabilities":{"supports":{"reasoning_effort":["low","high"]}},"model_picker_enabled":true,"name":"GPT-5 Thinking"}]}`))
		})
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()

		h.HandleModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}

		var result struct {
			Models []struct {
				DefaultReasoningLevel    *string `json:"default_reasoning_level,omitempty"`
				SupportedReasoningLevels []struct {
					Effort string `json:"effort"`
				} `json:"supported_reasoning_levels"`
			} `json:"models"`
		}
		body, _ := io.ReadAll(resp.Body)
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if len(result.Models) != 1 {
			t.Fatalf("expected 1 model entry, got %d", len(result.Models))
		}
		if result.Models[0].DefaultReasoningLevel == nil || *result.Models[0].DefaultReasoningLevel != "low" {
			t.Fatalf("expected default_reasoning_level low, got %v", result.Models[0].DefaultReasoningLevel)
		}
		if len(result.Models[0].SupportedReasoningLevels) != 2 {
			t.Fatalf("expected 2 supported reasoning levels, got %d", len(result.Models[0].SupportedReasoningLevels))
		}
	})

	t.Run("static Azure model serializes empty reasoning levels as array", func(t *testing.T) {
		t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

		azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/openai/v1/models" {
				t.Fatalf("unexpected Azure path %q", r.URL.Path)
			}
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer azureServer.Close()

		handler, err := NewProxyHandler(
			auth.NewTestAuthenticator("test-token"),
			logger.New(logger.LevelInfo),
			WithProvidersConfig(ProvidersConfig{
				Providers: []ProviderConfig{{
					ID:        "azure",
					Type:      "azure-openai",
					Default:   true,
					BaseURL:   azureServer.URL + "/openai/v1",
					APIKeyEnv: "TEST_AZURE_API_KEY",
					Models: []ProviderModelConfig{{
						PublicID:   "gpt-5.4",
						Deployment: "gpt-5-4-prod",
						Endpoints:  []string{"/responses"},
						Name:       "GPT-5.4",
					}},
				}},
			}),
		)
		if err != nil {
			t.Fatalf("NewProxyHandler returned error: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()

		handler.HandleModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		body, _ := io.ReadAll(resp.Body)
		if !bytes.Contains(body, []byte(`"supported_reasoning_levels":[]`)) {
			t.Fatalf("expected supported_reasoning_levels to serialize as [], got %s", body)
		}

		var result struct {
			Models []struct {
				Slug                     string `json:"slug"`
				SupportedReasoningLevels []struct {
					Effort string `json:"effort"`
				} `json:"supported_reasoning_levels"`
			} `json:"models"`
		}
		if err := json.Unmarshal(body, &result); err != nil {
			t.Fatalf("invalid JSON: %v", err)
		}
		if len(result.Models) != 1 {
			t.Fatalf("expected 1 model entry, got %d", len(result.Models))
		}
		if result.Models[0].Slug != "gpt-5.4" {
			t.Fatalf("expected model slug gpt-5.4, got %q", result.Models[0].Slug)
		}
		if result.Models[0].SupportedReasoningLevels == nil {
			t.Fatal("expected supported_reasoning_levels to be a non-nil empty slice")
		}
		if len(result.Models[0].SupportedReasoningLevels) != 0 {
			t.Fatalf("expected 0 supported reasoning levels, got %d", len(result.Models[0].SupportedReasoningLevels))
		}
	})

	t.Run("static Azure model exposes configured Codex metadata", func(t *testing.T) {
		t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

		azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/openai/v1/models" {
				t.Fatalf("unexpected Azure path %q", r.URL.Path)
			}
			w.WriteHeader(http.StatusInternalServerError)
		}))
		defer azureServer.Close()

		parallelToolCalls := true
		vision := true
		contextWindow := int64(400000)
		handler, err := NewProxyHandler(
			auth.NewTestAuthenticator("test-token"),
			logger.New(logger.LevelInfo),
			WithProvidersConfig(ProvidersConfig{
				Providers: []ProviderConfig{{
					ID:        "azure",
					Type:      "azure-openai",
					Default:   true,
					BaseURL:   azureServer.URL + "/openai/v1",
					APIKeyEnv: "TEST_AZURE_API_KEY",
					Models: []ProviderModelConfig{{
						PublicID:            "gpt-5.4",
						Deployment:          "gpt-5-4-prod",
						Endpoints:           []string{"/responses"},
						Name:                "GPT-5.4",
						ModelPickerCategory: "powerful",
						ReasoningEffort:     []string{"low", "medium", "high"},
						Vision:              &vision,
						ParallelToolCalls:   &parallelToolCalls,
						ContextWindow:       &contextWindow,
					}},
				}},
			}),
		)
		if err != nil {
			t.Fatalf("NewProxyHandler returned error: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()

		handler.HandleModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		var result struct {
			Models []struct {
				Slug                     string  `json:"slug"`
				DefaultReasoningLevel    *string `json:"default_reasoning_level,omitempty"`
				SupportedReasoningLevels []struct {
					Effort string `json:"effort"`
				} `json:"supported_reasoning_levels"`
				Priority                   int      `json:"priority"`
				SupportsReasoningSummaries bool     `json:"supports_reasoning_summaries"`
				SupportsParallelToolCalls  bool     `json:"supports_parallel_tool_calls"`
				ContextWindow              *int64   `json:"context_window,omitempty"`
				InputModalities            []string `json:"input_modalities"`
			} `json:"models"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(result.Models) != 1 {
			t.Fatalf("expected 1 model entry, got %d", len(result.Models))
		}
		model := result.Models[0]
		if model.Slug != "gpt-5.4" {
			t.Fatalf("expected model slug gpt-5.4, got %q", model.Slug)
		}
		if model.DefaultReasoningLevel == nil || *model.DefaultReasoningLevel != "medium" {
			t.Fatalf("expected default_reasoning_level medium, got %v", model.DefaultReasoningLevel)
		}
		if len(model.SupportedReasoningLevels) != 3 {
			t.Fatalf("expected 3 supported reasoning levels, got %d", len(model.SupportedReasoningLevels))
		}
		if model.Priority != 0 {
			t.Fatalf("expected powerful model priority 0, got %d", model.Priority)
		}
		if !model.SupportsReasoningSummaries {
			t.Fatal("expected supports_reasoning_summaries true")
		}
		if !model.SupportsParallelToolCalls {
			t.Fatal("expected supports_parallel_tool_calls true")
		}
		if model.ContextWindow == nil || *model.ContextWindow != 400000 {
			t.Fatalf("expected context_window 400000, got %v", model.ContextWindow)
		}
		if got := strings.Join(model.InputModalities, ","); got != "text,image" {
			t.Fatalf("expected input_modalities text,image, got %q", got)
		}
	})

	t.Run("Azure upstream metadata best-effort enriches configured public model", func(t *testing.T) {
		t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

		azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/openai/v1/models" {
				t.Fatalf("unexpected Azure path %q", r.URL.Path)
			}
			if got := r.URL.RawQuery; got != "" {
				t.Fatalf("expected no Azure query params for /openai/v1 base URL, got %q", got)
			}
			if got := r.Header.Get("api-key"); got != "azure-test-key" {
				t.Fatalf("expected api-key header, got %q", got)
			}

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.4","object":"model","created":0,"owned_by":"azure-openai","supported_endpoints":["/chat/completions","/responses"],"capabilities":{"supports":{"parallel_tool_calls":true,"vision":true,"reasoning_effort":["low","medium","high"]},"limits":{"max_context_window_tokens":128000}},"model_picker_enabled":true,"model_picker_category":"powerful","name":"GPT-5.4 Overlay"}]}`))
		}))
		defer azureServer.Close()

		handler, err := NewProxyHandler(
			auth.NewTestAuthenticator("test-token"),
			logger.New(logger.LevelInfo),
			WithProvidersConfig(ProvidersConfig{
				Providers: []ProviderConfig{{
					ID:         "azure",
					Type:       "azure-openai",
					Default:    true,
					BaseURL:    azureServer.URL + "/openai/v1",
					APIKeyEnv:  "TEST_AZURE_API_KEY",
					APIVersion: "preview",
					Models: []ProviderModelConfig{{
						PublicID:   "gpt-5.4",
						Deployment: "gpt-5-4-prod",
						Endpoints:  []string{"/responses"},
					}},
				}},
			}),
		)
		if err != nil {
			t.Fatalf("NewProxyHandler returned error: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()

		handler.HandleModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		var result struct {
			Data []struct {
				ID                 string   `json:"id"`
				Name               string   `json:"name"`
				SupportedEndpoints []string `json:"supported_endpoints"`
			} `json:"data"`
			Models []struct {
				Slug                      string   `json:"slug"`
				DisplayName               string   `json:"display_name"`
				DefaultReasoningLevel     *string  `json:"default_reasoning_level,omitempty"`
				SupportsParallelToolCalls bool     `json:"supports_parallel_tool_calls"`
				ContextWindow             *int64   `json:"context_window,omitempty"`
				InputModalities           []string `json:"input_modalities"`
				Priority                  int      `json:"priority"`
			} `json:"models"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(result.Data) != 1 || len(result.Models) != 1 {
			t.Fatalf("expected one Azure model entry, got data=%d models=%d", len(result.Data), len(result.Models))
		}
		if result.Data[0].ID != "gpt-5.4" {
			t.Fatalf("expected public id gpt-5.4, got %q", result.Data[0].ID)
		}
		if result.Data[0].Name != "GPT-5.4 Overlay" {
			t.Fatalf("expected Azure overlay name, got %q", result.Data[0].Name)
		}
		if got := strings.Join(result.Data[0].SupportedEndpoints, ","); got != "/responses" {
			t.Fatalf("expected configured supported_endpoints /responses, got %q", got)
		}

		model := result.Models[0]
		if model.Slug != "gpt-5.4" {
			t.Fatalf("expected slug gpt-5.4, got %q", model.Slug)
		}
		if model.DisplayName != "GPT-5.4 Overlay" {
			t.Fatalf("expected Azure overlay display name, got %q", model.DisplayName)
		}
		if model.DefaultReasoningLevel == nil || *model.DefaultReasoningLevel != "medium" {
			t.Fatalf("expected default_reasoning_level medium, got %v", model.DefaultReasoningLevel)
		}
		if !model.SupportsParallelToolCalls {
			t.Fatal("expected Azure overlay supports_parallel_tool_calls true")
		}
		if model.ContextWindow == nil || *model.ContextWindow != 128000 {
			t.Fatalf("expected Azure overlay context_window 128000, got %v", model.ContextWindow)
		}
		if got := strings.Join(model.InputModalities, ","); got != "text,image" {
			t.Fatalf("expected Azure overlay input_modalities text,image, got %q", got)
		}
		if model.Priority != 0 {
			t.Fatalf("expected powerful priority 0, got %d", model.Priority)
		}
	})

	t.Run("Azure sparse upstream metadata leaves static model minimal but valid", func(t *testing.T) {
		t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

		azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/openai/v1/models" {
				t.Fatalf("unexpected Azure path %q", r.URL.Path)
			}

			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.4","object":"model","created":0,"owned_by":"azure-openai"}]}`))
		}))
		defer azureServer.Close()

		handler, err := NewProxyHandler(
			auth.NewTestAuthenticator("test-token"),
			logger.New(logger.LevelInfo),
			WithProvidersConfig(ProvidersConfig{
				Providers: []ProviderConfig{{
					ID:        "azure",
					Type:      "azure-openai",
					Default:   true,
					BaseURL:   azureServer.URL + "/openai/v1",
					APIKeyEnv: "TEST_AZURE_API_KEY",
					Models: []ProviderModelConfig{{
						PublicID:   "gpt-5.4",
						Deployment: "gpt-5-4-prod",
						Endpoints:  []string{"/responses"},
					}},
				}},
			}),
		)
		if err != nil {
			t.Fatalf("NewProxyHandler returned error: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()

		handler.HandleModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		var result struct {
			Data []struct {
				ID                 string   `json:"id"`
				Name               string   `json:"name"`
				SupportedEndpoints []string `json:"supported_endpoints"`
			} `json:"data"`
			Models []struct {
				Slug                       string   `json:"slug"`
				DisplayName                string   `json:"display_name"`
				Visibility                 string   `json:"visibility"`
				SupportedInAPI             bool     `json:"supported_in_api"`
				DefaultReasoningLevel      *string  `json:"default_reasoning_level,omitempty"`
				SupportedReasoningLevels   []string `json:"supported_reasoning_levels"`
				SupportsReasoningSummaries bool     `json:"supports_reasoning_summaries"`
				Priority                   int      `json:"priority"`
				SupportsParallelToolCalls  bool     `json:"supports_parallel_tool_calls"`
				ContextWindow              *int64   `json:"context_window,omitempty"`
				InputModalities            []string `json:"input_modalities"`
			} `json:"models"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(result.Data) != 1 || len(result.Models) != 1 {
			t.Fatalf("expected one Azure model entry, got data=%d models=%d", len(result.Data), len(result.Models))
		}
		if result.Data[0].ID != "gpt-5.4" {
			t.Fatalf("expected public id gpt-5.4, got %q", result.Data[0].ID)
		}
		if result.Data[0].Name != "gpt-5.4" {
			t.Fatalf("expected static fallback name gpt-5.4, got %q", result.Data[0].Name)
		}
		if got := strings.Join(result.Data[0].SupportedEndpoints, ","); got != "/responses" {
			t.Fatalf("expected configured supported_endpoints /responses, got %q", got)
		}

		model := result.Models[0]
		if model.Slug != "gpt-5.4" {
			t.Fatalf("expected slug gpt-5.4, got %q", model.Slug)
		}
		if model.DisplayName != "gpt-5.4" {
			t.Fatalf("expected static fallback display name gpt-5.4, got %q", model.DisplayName)
		}
		if model.Visibility != "list" {
			t.Fatalf("expected default visibility list for /responses model, got %q", model.Visibility)
		}
		if !model.SupportedInAPI {
			t.Fatal("expected supported_in_api true from configured /responses endpoint")
		}
		if model.DefaultReasoningLevel != nil {
			t.Fatalf("expected no default_reasoning_level for sparse metadata, got %v", model.DefaultReasoningLevel)
		}
		if len(model.SupportedReasoningLevels) != 0 {
			t.Fatalf("expected no supported_reasoning_levels for sparse metadata, got %v", model.SupportedReasoningLevels)
		}
		if model.SupportsReasoningSummaries {
			t.Fatal("expected supports_reasoning_summaries false for sparse metadata")
		}
		if model.Priority != 5 {
			t.Fatalf("expected default versatile priority 5, got %d", model.Priority)
		}
		if model.SupportsParallelToolCalls {
			t.Fatal("expected supports_parallel_tool_calls false for sparse metadata")
		}
		if model.ContextWindow != nil {
			t.Fatalf("expected no context_window for sparse metadata, got %v", model.ContextWindow)
		}
		if got := strings.Join(model.InputModalities, ","); got != "text" {
			t.Fatalf("expected text-only input_modalities for sparse metadata, got %q", got)
		}
	})

	t.Run("Azure best-effort upstream metadata matches deployment and respects explicit overrides", func(t *testing.T) {
		t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

		azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/openai/v1/models" {
				t.Fatalf("unexpected Azure path %q", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5-4-prod","object":"model","created":0,"owned_by":"azure-openai","supported_endpoints":["/chat/completions","/responses"],"capabilities":{"supports":{"parallel_tool_calls":true,"vision":true,"reasoning_effort":["low","medium","high"]},"limits":{"max_context_window_tokens":128000}},"model_picker_enabled":true,"model_picker_category":"versatile","name":"Overlay GPT-5.4 Prod"}]}`))
		}))
		defer azureServer.Close()

		modelPickerEnabled := false
		parallelToolCalls := false
		vision := false
		contextWindow := int64(64000)

		handler, err := NewProxyHandler(
			auth.NewTestAuthenticator("test-token"),
			logger.New(logger.LevelInfo),
			WithProvidersConfig(ProvidersConfig{
				Providers: []ProviderConfig{{
					ID:        "azure",
					Type:      "azure-openai",
					Default:   true,
					BaseURL:   azureServer.URL + "/openai/v1",
					APIKeyEnv: "TEST_AZURE_API_KEY",
					Models: []ProviderModelConfig{{
						PublicID:            "gpt-5.4-proxy",
						Deployment:          "gpt-5-4-prod",
						Endpoints:           []string{"/responses"},
						Name:                "Alias GPT-5.4",
						ModelPickerEnabled:  &modelPickerEnabled,
						ModelPickerCategory: "powerful",
						ReasoningEffort:     []string{"low"},
						Vision:              &vision,
						ParallelToolCalls:   &parallelToolCalls,
						ContextWindow:       &contextWindow,
					}},
				}},
			}),
		)
		if err != nil {
			t.Fatalf("NewProxyHandler returned error: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()

		handler.HandleModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}

		var result struct {
			Data []struct {
				ID                 string   `json:"id"`
				Name               string   `json:"name"`
				SupportedEndpoints []string `json:"supported_endpoints"`
			} `json:"data"`
			Models []struct {
				Slug                     string  `json:"slug"`
				DisplayName              string  `json:"display_name"`
				Visibility               string  `json:"visibility"`
				DefaultReasoningLevel    *string `json:"default_reasoning_level,omitempty"`
				SupportedReasoningLevels []struct {
					Effort string `json:"effort"`
				} `json:"supported_reasoning_levels"`
				Priority                  int      `json:"priority"`
				SupportsParallelToolCalls bool     `json:"supports_parallel_tool_calls"`
				ContextWindow             *int64   `json:"context_window,omitempty"`
				InputModalities           []string `json:"input_modalities"`
			} `json:"models"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(result.Data) != 1 || len(result.Models) != 1 {
			t.Fatalf("expected one Azure model entry, got data=%d models=%d", len(result.Data), len(result.Models))
		}
		if result.Data[0].ID != "gpt-5.4-proxy" {
			t.Fatalf("expected aliased public id, got %q", result.Data[0].ID)
		}
		if result.Data[0].Name != "Alias GPT-5.4" {
			t.Fatalf("expected configured name override, got %q", result.Data[0].Name)
		}
		if got := strings.Join(result.Data[0].SupportedEndpoints, ","); got != "/responses" {
			t.Fatalf("expected configured supported_endpoints /responses, got %q", got)
		}

		model := result.Models[0]
		if model.Slug != "gpt-5.4-proxy" {
			t.Fatalf("expected aliased slug, got %q", model.Slug)
		}
		if model.DisplayName != "Alias GPT-5.4" {
			t.Fatalf("expected configured display name override, got %q", model.DisplayName)
		}
		if model.Visibility != "hide" {
			t.Fatalf("expected hidden visibility from configured model_picker_enabled=false, got %q", model.Visibility)
		}
		if model.DefaultReasoningLevel == nil || *model.DefaultReasoningLevel != "low" {
			t.Fatalf("expected configured default_reasoning_level low, got %v", model.DefaultReasoningLevel)
		}
		if len(model.SupportedReasoningLevels) != 1 || model.SupportedReasoningLevels[0].Effort != "low" {
			t.Fatalf("expected configured reasoning_effort override, got %+v", model.SupportedReasoningLevels)
		}
		if model.Priority != 0 {
			t.Fatalf("expected configured powerful priority 0, got %d", model.Priority)
		}
		if model.SupportsParallelToolCalls {
			t.Fatal("expected configured parallel_tool_calls=false to win over Azure overlay metadata")
		}
		if model.ContextWindow == nil || *model.ContextWindow != 64000 {
			t.Fatalf("expected configured context_window 64000, got %v", model.ContextWindow)
		}
		if got := strings.Join(model.InputModalities, ","); got != "text" {
			t.Fatalf("expected configured vision=false to keep text-only input_modalities, got %q", got)
		}
	})
}

func TestHandleModels_CodexContractFixture(t *testing.T) {
	upstreamBody, err := os.ReadFile("testdata/codex_models_upstream.json")
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}

	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(upstreamBody)
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()

	h.HandleModels(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}

	type reasoningPreset struct {
		Effort      string `json:"effort"`
		Description string `json:"description"`
	}
	type truncationPolicy struct {
		Mode  string `json:"mode"`
		Limit int64  `json:"limit"`
	}
	type codexModelContract struct {
		Slug                       string            `json:"slug"`
		DisplayName                string            `json:"display_name"`
		DefaultReasoningLevel      *string           `json:"default_reasoning_level,omitempty"`
		SupportedReasoningLevels   []reasoningPreset `json:"supported_reasoning_levels"`
		ShellType                  string            `json:"shell_type"`
		Visibility                 string            `json:"visibility"`
		SupportedInAPI             bool              `json:"supported_in_api"`
		Priority                   int               `json:"priority"`
		BaseInstructions           string            `json:"base_instructions"`
		SupportsReasoningSummaries bool              `json:"supports_reasoning_summaries"`
		SupportVerbosity           bool              `json:"support_verbosity"`
		TruncationPolicy           truncationPolicy  `json:"truncation_policy"`
		SupportsParallelToolCalls  bool              `json:"supports_parallel_tool_calls"`
		ContextWindow              *int64            `json:"context_window,omitempty"`
		ExperimentalSupportedTools []string          `json:"experimental_supported_tools"`
		InputModalities            []string          `json:"input_modalities"`
	}
	var result struct {
		Data   []json.RawMessage    `json:"data"`
		Models []codexModelContract `json:"models"`
	}

	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result.Data) != 3 {
		t.Fatalf("expected 3 data entries, got %d", len(result.Data))
	}
	if len(result.Models) != 3 {
		t.Fatalf("expected 3 transformed models, got %d", len(result.Models))
	}

	bySlug := make(map[string]codexModelContract, len(result.Models))
	for _, model := range result.Models {
		bySlug[model.Slug] = model
	}

	gpt54, ok := bySlug["gpt-5.4"]
	if !ok {
		t.Fatal("expected gpt-5.4 in transformed models")
	}
	if gpt54.DisplayName != "GPT-5.4" {
		t.Errorf("gpt-5.4 display_name = %q, want GPT-5.4", gpt54.DisplayName)
	}
	if gpt54.DefaultReasoningLevel == nil || *gpt54.DefaultReasoningLevel != "medium" {
		t.Errorf("gpt-5.4 default_reasoning_level = %v, want medium", gpt54.DefaultReasoningLevel)
	}
	if len(gpt54.SupportedReasoningLevels) != 3 {
		t.Fatalf("gpt-5.4 supported_reasoning_levels = %d, want 3", len(gpt54.SupportedReasoningLevels))
	}
	if gpt54.ShellType != "shell_command" {
		t.Errorf("gpt-5.4 shell_type = %q, want shell_command", gpt54.ShellType)
	}
	if gpt54.Visibility != "list" {
		t.Errorf("gpt-5.4 visibility = %q, want list", gpt54.Visibility)
	}
	if !gpt54.SupportedInAPI {
		t.Error("expected gpt-5.4 supported_in_api = true")
	}
	if gpt54.TruncationPolicy.Mode != "bytes" || gpt54.TruncationPolicy.Limit != 10000 {
		t.Errorf("gpt-5.4 truncation_policy = %+v, want bytes/10000", gpt54.TruncationPolicy)
	}
	if !gpt54.SupportsParallelToolCalls {
		t.Error("expected gpt-5.4 supports_parallel_tool_calls = true")
	}
	if got := strings.Join(gpt54.InputModalities, ","); got != "text,image" {
		t.Errorf("gpt-5.4 input_modalities = %q, want text,image", got)
	}

	claude, ok := bySlug["claude-sonnet-4.5"]
	if !ok {
		t.Fatal("expected claude-sonnet-4.5 in transformed models")
	}
	if claude.Visibility != "hide" {
		t.Errorf("claude-sonnet-4.5 visibility = %q, want hide", claude.Visibility)
	}
	if claude.SupportedInAPI {
		t.Error("expected claude-sonnet-4.5 supported_in_api = false")
	}
	if claude.SupportsReasoningSummaries {
		t.Error("expected claude-sonnet-4.5 supports_reasoning_summaries = false")
	}
	if len(claude.SupportedReasoningLevels) != 0 {
		t.Errorf("claude-sonnet-4.5 supported_reasoning_levels = %d, want 0", len(claude.SupportedReasoningLevels))
	}
}

func TestHandleModels_ForwardsQueryAndETag(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.RawQuery; got != "client_version=0.99.0" {
			t.Errorf("expected client_version query, got %q", got)
		}
		if got := r.Header.Get("If-None-Match"); got != "" {
			t.Errorf("expected no If-None-Match on first request, got %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("ETag", `"models-etag-1"`)
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4o","object":"model","created":0,"owned_by":"github-copilot","name":"GPT-4o"}]}`))
	})

	req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.99.0", nil)
	w := httptest.NewRecorder()

	handler.HandleModels(w, req)

	resp := w.Result()
	if got := resp.Header.Get("ETag"); got != `"models-etag-1"` {
		t.Errorf("ETag = %q, want %q", got, `"models-etag-1"`)
	}
}

func TestHandleModels_RevalidatesCachedEntryWhenETagChanges(t *testing.T) {
	requestCount := 0
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if got := r.URL.RawQuery; got != "client_version=0.99.0" {
			t.Errorf("expected client_version query, got %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		switch requestCount {
		case 1:
			if got := r.Header.Get("If-None-Match"); got != "" {
				t.Errorf("expected no If-None-Match on first request, got %q", got)
			}
			w.Header().Set("ETag", `"models-etag-1"`)
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4o","object":"model","created":0,"owned_by":"github-copilot","name":"GPT-4o"}]}`))
		case 2:
			if got := r.Header.Get("If-None-Match"); got != `"models-etag-1"` {
				t.Errorf("If-None-Match = %q, want %q", got, `"models-etag-1"`)
			}
			w.Header().Set("ETag", `"models-etag-2"`)
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5","object":"model","created":0,"owned_by":"github-copilot","name":"GPT-5"}]}`))
		default:
			t.Fatalf("unexpected request count %d", requestCount)
		}
	})

	req1 := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.99.0", nil)
	w1 := httptest.NewRecorder()
	handler.HandleModels(w1, req1)

	req2 := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.99.0", nil)
	w2 := httptest.NewRecorder()
	handler.HandleModels(w2, req2)

	if requestCount != 2 {
		t.Fatalf("expected 2 upstream requests, got %d", requestCount)
	}

	resp := w2.Result()
	if got := resp.Header.Get("ETag"); got != `"models-etag-2"` {
		t.Errorf("ETag = %q, want %q", got, `"models-etag-2"`)
	}

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(result.Models) != 1 || result.Models[0].Slug != "gpt-5" {
		t.Fatalf("expected refreshed gpt-5 model, got %+v", result.Models)
	}
}

func TestHandleModels_UsesCachedEntryOnNotModified(t *testing.T) {
	requestCount := 0
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		switch requestCount {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.Header().Set("ETag", `"models-etag-1"`)
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4o","object":"model","created":0,"owned_by":"github-copilot","name":"GPT-4o"}]}`))
		case 2:
			if got := r.Header.Get("If-None-Match"); got != `"models-etag-1"` {
				t.Errorf("If-None-Match = %q, want %q", got, `"models-etag-1"`)
			}
			w.Header().Set("ETag", `"models-etag-1"`)
			w.WriteHeader(http.StatusNotModified)
		default:
			t.Fatalf("unexpected request count %d", requestCount)
		}
	})

	req1 := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.99.0", nil)
	w1 := httptest.NewRecorder()
	handler.HandleModels(w1, req1)

	req2 := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=0.99.0", nil)
	w2 := httptest.NewRecorder()
	handler.HandleModels(w2, req2)

	resp := w2.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected cached 200 response, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("ETag"); got != `"models-etag-1"` {
		t.Errorf("ETag = %q, want %q", got, `"models-etag-1"`)
	}

	body, _ := io.ReadAll(resp.Body)
	var result struct {
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("invalid JSON: %v", err)
	}
	if len(result.Models) != 1 || result.Models[0].Slug != "gpt-4o" {
		t.Fatalf("expected cached gpt-4o model, got %+v", result.Models)
	}
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
		body, _ := io.ReadAll(r.Body)
		var upstreamReq map[string]json.RawMessage
		if err := json.Unmarshal(body, &upstreamReq); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}
		var serviceTier string
		if err := json.Unmarshal(upstreamReq["service_tier"], &serviceTier); err != nil {
			t.Fatalf("upstream request should preserve service_tier: %v", err)
		}
		if serviceTier != "auto" {
			t.Fatalf("expected upstream service_tier auto, got %q", serviceTier)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(sseBody))
	})

	reqBody := `{"model":"gpt-4","input":"Hello","stream":true,"service_tier":"auto"}`
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

func TestOpenAIResponsesStreaming_PreservesUpstreamHeaders(t *testing.T) {
	sseBody := "event: response.created\ndata: {\"id\":\"resp-1\",\"type\":\"response\"}\n\n"

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Models-Etag", "\"models-etag-2\"")
		w.Header().Set("OpenAI-Model", "gpt-5.2")
		w.Header().Set("X-Reasoning-Included", "true")
		w.Header().Set("X-Codex-Turn-State", "sticky-turn-state")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody))
	})

	reqBody := `{"model":"gpt-4","input":"Hello","stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if got := resp.Header.Get("X-Models-Etag"); got != `"models-etag-2"` {
		t.Errorf("X-Models-Etag = %q, want %q", got, `"models-etag-2"`)
	}
	if got := resp.Header.Get("OpenAI-Model"); got != "gpt-5.2" {
		t.Errorf("OpenAI-Model = %q, want %q", got, "gpt-5.2")
	}
	if got := resp.Header.Get("X-Reasoning-Included"); got != "true" {
		t.Errorf("X-Reasoning-Included = %q, want true", got)
	}
	if got := resp.Header.Get("X-Codex-Turn-State"); got != "sticky-turn-state" {
		t.Errorf("X-Codex-Turn-State = %q, want %q", got, "sticky-turn-state")
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

func TestHandleResponses_NonStreamingPreservesUpstreamHeaders(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Models-Etag", "\"models-etag-3\"")
		w.Header().Set("OpenAI-Model", "gpt-5.3")
		w.Header().Set("X-Reasoning-Included", "true")
		w.Header().Set("X-Codex-Turn-State", "sticky-turn-state-2")
		_, _ = w.Write([]byte(`{"id":"resp-test","object":"response","status":"completed","model":"gpt-5.3","output":[]}`))
	})

	reqBody := `{"model":"gpt-5.3","input":"Hi"}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if got := resp.Header.Get("X-Models-Etag"); got != `"models-etag-3"` {
		t.Errorf("X-Models-Etag = %q, want %q", got, `"models-etag-3"`)
	}
	if got := resp.Header.Get("OpenAI-Model"); got != "gpt-5.3" {
		t.Errorf("OpenAI-Model = %q, want %q", got, "gpt-5.3")
	}
	if got := resp.Header.Get("X-Reasoning-Included"); got != "true" {
		t.Errorf("X-Reasoning-Included = %q, want true", got)
	}
	if got := resp.Header.Get("X-Codex-Turn-State"); got != "sticky-turn-state-2" {
		t.Errorf("X-Codex-Turn-State = %q, want %q", got, "sticky-turn-state-2")
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
	if anthropicResp.Content[0].Type != "text" || derefString(anthropicResp.Content[0].Text) != "I'll delegate both tasks" {
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

	t.Run("no-op with empty tools array", func(t *testing.T) {
		input := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":[]}`
		result := injectParallelToolCalls([]byte(input))
		if string(result) != input {
			t.Errorf("body was modified for empty tools array: %s", result)
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

func TestHandleOpenAIChatCompletions_ForcedStreamingUsesStreamingUpstreamTimeout(t *testing.T) {
	deadlineCh := make(chan time.Duration, 1)
	handler := newRoundTripTestProxyHandler(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Fatal("expected upstream request deadline")
		}
		deadlineCh <- time.Until(deadline)

		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}
		if oaiReq.Stream == nil || !*oaiReq.Stream {
			t.Fatal("expected proxy to force upstream stream=true when tools are present")
		}

		return sseHTTPResponse("data: {\"id\":\"chatcmpl-deadline\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\ndata: {\"id\":\"chatcmpl-deadline\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), nil
	}))

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	if resp := w.Result(); resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	assertDeadlineApprox(t, <-deadlineCh, streamingUpstreamTimeout)
}

func TestOpenAIChatCompletions_EmptyToolsDoesNotForceStreaming(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var raw map[string]json.RawMessage
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &raw); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}
		if _, ok := raw["parallel_tool_calls"]; ok {
			t.Error("did not expect parallel_tool_calls for empty tools array")
		}
		if _, ok := raw["stream"]; ok {
			t.Error("did not expect forced stream=true for empty tools array")
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID:      "chatcmpl-empty-tools",
			Object:  "chat.completion",
			Choices: []models.OpenAIChoice{{Index: 0, Message: models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"Hi"`)}}},
		})
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"hi"}],"tools":[]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	if w.Result().StatusCode != http.StatusOK {
		body, _ := io.ReadAll(w.Result().Body)
		t.Fatalf("expected 200, got %d: %s", w.Result().StatusCode, body)
	}
}

func TestOpenAIChatCompletions_ForcedStreamingPreservesMultipleChoices(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("unmarshal upstream request: %v", err)
		}
		if oaiReq.Stream == nil || !*oaiReq.Stream {
			t.Error("expected stream=true forced by proxy when tools present")
		}
		if oaiReq.N == nil || *oaiReq.N != 2 {
			t.Errorf("n = %v, want 2", oaiReq.N)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"c2\",\"created\":1000,\"model\":\"gpt-4\",\"choices\":[{\"index\":1,\"delta\":{\"content\":\"Beta\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"c2\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Alpha\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"c2\",\"model\":\"gpt-4\",\"choices\":[{\"index\":1,\"delta\":{\"content\":\" one\"}},{\"index\":0,\"delta\":{\"content\":\" zero\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"c2\",\"model\":\"gpt-4\",\"choices\":[{\"index\":1,\"delta\":{},\"finish_reason\":\"stop\"},{\"index\":0,\"delta\":{},\"finish_reason\":\"length\"}],\"usage\":{\"prompt_tokens\":9,\"completion_tokens\":5,\"total_tokens\":14}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})

	reqBody := `{"model":"gpt-4","n":2,"messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"f","parameters":{}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var got models.OpenAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if len(got.Choices) != 2 {
		t.Fatalf("expected 2 choices, got %d", len(got.Choices))
	}

	if got.Choices[0].Index != 0 {
		t.Fatalf("choice[0].Index = %d, want 0", got.Choices[0].Index)
	}
	if got.Choices[1].Index != 1 {
		t.Fatalf("choice[1].Index = %d, want 1", got.Choices[1].Index)
	}

	var text0, text1 string
	if err := json.Unmarshal(got.Choices[0].Message.Content, &text0); err != nil {
		t.Fatalf("unmarshal choice[0] content: %v", err)
	}
	if err := json.Unmarshal(got.Choices[1].Message.Content, &text1); err != nil {
		t.Fatalf("unmarshal choice[1] content: %v", err)
	}

	if text0 != "Alpha zero" {
		t.Errorf("choice[0] content = %q, want %q", text0, "Alpha zero")
	}
	if text1 != "Beta one" {
		t.Errorf("choice[1] content = %q, want %q", text1, "Beta one")
	}
	if got.Choices[0].FinishReason == nil || *got.Choices[0].FinishReason != "length" {
		t.Errorf("choice[0] finish_reason = %v, want length", got.Choices[0].FinishReason)
	}
	if got.Choices[1].FinishReason == nil || *got.Choices[1].FinishReason != "stop" {
		t.Errorf("choice[1] finish_reason = %v, want stop", got.Choices[1].FinishReason)
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
		_, _ = w.Write([]byte(`{"id":"resp-gz","object":"response","status":"completed"}`))
	})

	// Gzip-compress the request body
	responsesReq := `{"model":"gpt-4","input":"Hello"}`
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write([]byte(responsesReq))
	_ = gw.Close()

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
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hi\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":1,\"total_tokens\":11}}\n\ndata: [DONE]\n\n"))
	})

	// Gzip-compress an Anthropic request
	anthropicReq := `{"model":"claude-sonnet-4","messages":[{"role":"user","content":"Hello"}],"max_tokens":1024}`
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write([]byte(anthropicReq))
	_ = gw.Close()

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
		_, _ = w.Write([]byte(`{"id":"resp-zstd","object":"response","status":"completed"}`))
	})

	// Zstd-compress the request body
	responsesReq := `{"model":"gpt-5.4","input":"Hello"}`
	var buf bytes.Buffer
	zw, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatalf("failed to create zstd writer: %v", err)
	}
	_, _ = zw.Write([]byte(responsesReq))
	_ = zw.Close()

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
		_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID:      "chatcmpl-gz",
			Object:  "chat.completion",
			Choices: []models.OpenAIChoice{{Message: models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"Hi"`)}}},
		})
	})

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}]}`
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	_, _ = gw.Write([]byte(reqBody))
	_ = gw.Close()

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
		_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID:      "chatcmpl-zstd",
			Object:  "chat.completion",
			Choices: []models.OpenAIChoice{{Message: models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"Hi"`)}}},
		})
	})

	reqBody := `{"model":"gpt-4o","messages":[{"role":"user","content":"Hello"}]}`
	var buf bytes.Buffer
	zw, _ := zstd.NewWriter(&buf)
	_, _ = zw.Write([]byte(reqBody))
	_ = zw.Close()

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

func TestHandleOpenAIChatCompletions_InvalidGzipBodyReturnsBadRequest(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("upstream should not be called for invalid gzip body")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("not-gzip"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 400, got %d: %s", resp.StatusCode, body)
	}

	var errResp map[string]map[string]interface{}
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	if errResp["error"]["type"] != "invalid_request_error" {
		t.Errorf("error.type = %v, want invalid_request_error", errResp["error"]["type"])
	}
}
