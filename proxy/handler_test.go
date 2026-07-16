package proxy

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
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

type observedDoneContext struct {
	context.Context
	observed chan struct{}
	once     sync.Once
}

func (c *observedDoneContext) Done() <-chan struct{} {
	c.once.Do(func() { close(c.observed) })
	return c.Context.Done()
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

func paddedProviderModelCatalog(t testing.TB, size int, modelID string) []byte {
	t.Helper()
	prefix := []byte(fmt.Sprintf(`{"object":"list","data":[{"id":%q,"object":"model","owned_by":"test","name":"Oversized metadata","description":"Oversized metadata"}]}`, modelID))
	if len(prefix) > size {
		t.Fatalf("catalog prefix is %d bytes, exceeds requested size %d", len(prefix), size)
	}
	body := make([]byte, size)
	copy(body, prefix)
	for i := len(prefix); i < len(body); i++ {
		body[i] = ' '
	}
	return body
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

	t.Run("static generic provider does not require models probe", func(t *testing.T) {
		var probeHits atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			probeHits.Add(1)
			w.WriteHeader(http.StatusNotFound)
		}))
		defer upstream.Close()

		h, err := NewProxyHandler(
			auth.NewTestAuthenticator("test-token"),
			logger.New(logger.LevelInfo),
			WithProvidersConfig(ProvidersConfig{
				Providers: []ProviderConfig{{
					ID:       "local",
					Type:     "openai-compatible",
					Default:  true,
					BaseURL:  upstream.URL,
					AuthType: "none",
					Models: []ProviderModelConfig{{
						PublicID:  "local-public",
						Endpoints: []string{"/chat/completions"},
					}},
				}},
			}),
		)
		if err != nil {
			t.Fatalf("NewProxyHandler returned error: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		w := httptest.NewRecorder()
		h.HandleReadyz(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}
		if got := probeHits.Load(); got != 0 {
			t.Fatalf("expected no upstream probe, got %d hits", got)
		}
	})

	t.Run("dynamic generic provider still requires models probe", func(t *testing.T) {
		var probeHits atomic.Int32
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			probeHits.Add(1)
			if r.URL.Path != "/models" {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
		}))
		defer upstream.Close()

		h, err := NewProxyHandler(
			auth.NewTestAuthenticator("test-token"),
			logger.New(logger.LevelInfo),
			WithProvidersConfig(ProvidersConfig{
				Providers: []ProviderConfig{{
					ID:             "dynamic",
					Type:           "openai-compatible",
					Default:        true,
					BaseURL:        upstream.URL,
					AuthType:       "none",
					ModelDiscovery: "openai",
				}},
			}),
		)
		if err != nil {
			t.Fatalf("NewProxyHandler returned error: %v", err)
		}

		req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
		w := httptest.NewRecorder()
		h.HandleReadyz(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
		}
		if got := probeHits.Load(); got != 1 {
			t.Fatalf("expected one dynamic upstream probe, got %d hits", got)
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
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-123\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello from the backend!\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-123\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
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

func TestHandleAnthropicMessagesCountTokens(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("expected path /chat/completions, got %q", r.URL.Path)
		}

		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("failed to parse upstream request: %v", err)
		}
		if oaiReq.Model != "claude-sonnet-4" {
			t.Fatalf("model = %q, want claude-sonnet-4", oaiReq.Model)
		}
		if oaiReq.Stream == nil || *oaiReq.Stream {
			t.Fatalf("stream = %v, want false", oaiReq.Stream)
		}
		if oaiReq.StreamOptions != nil {
			t.Fatalf("stream_options = %#v, want nil", oaiReq.StreamOptions)
		}
		if oaiReq.MaxCompletionTokens == nil || *oaiReq.MaxCompletionTokens != 1 {
			t.Fatalf("max_completion_tokens = %v, want 1", oaiReq.MaxCompletionTokens)
		}
		if oaiReq.MaxTokens != nil {
			t.Fatalf("max_tokens = %v, want nil", oaiReq.MaxTokens)
		}
		if len(oaiReq.Tools) != 1 || oaiReq.Tools[0].Function.Name != "lookup" {
			t.Fatalf("tools = %#v, want translated lookup tool", oaiReq.Tools)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-count","object":"chat.completion","created":1,"model":"claude-sonnet-4","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"length"}],"usage":{"prompt_tokens":123,"completion_tokens":1,"total_tokens":124}}`))
	})

	countReq := `{
		"model": "claude-sonnet-4",
		"messages": [{"role": "user", "content": "Count this"}],
		"tools": [{"name": "lookup", "description": "Lookup", "input_schema": {"type": "object"}}]
	}`
	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(countReq))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessagesCountTokens(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var countResp models.AnthropicCountTokensResponse
	if err := json.NewDecoder(resp.Body).Decode(&countResp); err != nil {
		t.Fatalf("decode count_tokens response: %v", err)
	}
	if countResp.InputTokens != 123 {
		t.Fatalf("input_tokens = %d, want 123", countResp.InputTokens)
	}
}

func TestHandleAnthropicMessagesCountTokensFallbacksToMaxTokens(t *testing.T) {
	var attempts int
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		var oaiReq models.OpenAIRequest
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("failed to parse upstream request: %v", err)
		}

		switch attempts {
		case 1:
			if oaiReq.MaxCompletionTokens == nil || *oaiReq.MaxCompletionTokens != 1 || oaiReq.MaxTokens != nil {
				t.Fatalf("first probe max fields = max_completion_tokens:%v max_tokens:%v", oaiReq.MaxCompletionTokens, oaiReq.MaxTokens)
			}
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unsupported field max_completion_tokens"}}`))
		case 2:
			if oaiReq.MaxCompletionTokens != nil || oaiReq.MaxTokens == nil || *oaiReq.MaxTokens != 1 {
				t.Fatalf("fallback probe max fields = max_completion_tokens:%v max_tokens:%v", oaiReq.MaxCompletionTokens, oaiReq.MaxTokens)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-count","object":"chat.completion","created":1,"model":"claude-sonnet-4","choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"length"}],"usage":{"prompt_tokens":77,"completion_tokens":1,"total_tokens":78}}`))
		default:
			t.Fatalf("unexpected attempt %d", attempts)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{
		"model": "claude-sonnet-4",
		"messages": [{"role": "user", "content": "Count this"}]
	}`))
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessagesCountTokens(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var countResp models.AnthropicCountTokensResponse
	if err := json.NewDecoder(resp.Body).Decode(&countResp); err != nil {
		t.Fatalf("decode count_tokens response: %v", err)
	}
	if countResp.InputTokens != 77 {
		t.Fatalf("input_tokens = %d, want 77", countResp.InputTokens)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestHandleAnthropicMessagesCountTokens_DirectGenericAnthropicCompatibleProvider(t *testing.T) {
	const upstreamBody = "{\n  \"input_tokens\": 42\n}\n"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/native/messages/count_tokens" {
			t.Fatalf("expected native count_tokens path /native/messages/count_tokens, got %s", got)
		}
		if got := r.Header.Get("X-API-Key"); got != "anthropic-key" {
			t.Fatalf("expected X-API-Key auth, got %q", got)
		}
		if got := r.Header.Get("Anthropic-Version"); got != "2023-06-01" {
			t.Fatalf("expected Anthropic-Version forwarded, got %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("expected client Authorization stripped, got %q", got)
		}

		var upstreamReq models.AnthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if upstreamReq.Model != "claude-upstream" {
			t.Fatalf("upstream model = %q, want claude-upstream", upstreamReq.Model)
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Count", "preserved")
		_, _ = io.WriteString(w, upstreamBody)
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:           "native",
				Type:         "anthropic-compatible",
				Default:      true,
				BaseURL:      upstream.URL,
				APIKey:       "anthropic-key",
				AuthType:     "api-key-header",
				AuthHeader:   "X-API-Key",
				MessagesPath: "/native/messages",
				Models: []ProviderModelConfig{{
					PublicID:   "claude-public",
					Deployment: "claude-upstream",
					Endpoints:  []string{"/v1/messages"},
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{
		"model": "claude-public",
		"messages": [{"role": "user", "content": "Count this"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessagesCountTokens(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("X-Upstream-Count"); got != "preserved" {
		t.Fatalf("X-Upstream-Count = %q, want preserved", got)
	}
	rawBody, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read count_tokens response: %v", err)
	}
	if string(rawBody) != upstreamBody {
		t.Fatalf("count_tokens body = %q, want byte-identical %q", rawBody, upstreamBody)
	}
	var countResp models.AnthropicCountTokensResponse
	if err := json.Unmarshal(rawBody, &countResp); err != nil {
		t.Fatalf("decode count_tokens response: %v", err)
	}
	if countResp.InputTokens != 42 {
		t.Fatalf("input_tokens = %d, want 42", countResp.InputTokens)
	}
}

func TestHandleAnthropicMessages_FilteredDynamicProviderRetainsRawDiscoveredModelIDs(t *testing.T) {
	rawModels := []string{"claude-sonnet-4-5", "claude-sonnet-4-20250514"}
	var modelsHits atomic.Int32
	var messagesMu sync.Mutex
	forwardedModels := make([]string, 0, len(rawModels))

	anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			modelsHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"claude-sonnet-4-5","object":"model","owned_by":"anthropic"},{"id":"claude-sonnet-4-20250514","object":"model","owned_by":"anthropic"}]}`))
		case "/v1/messages":
			var payload struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode upstream messages request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			messagesMu.Lock()
			forwardedModels = append(forwardedModels, payload.Model)
			messagesMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"msg-1","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, payload.Model)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer anthropicUpstream.Close()

	var fallbackHits atomic.Int32
	fallbackUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer fallbackUpstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
			{
				ID:             "fallback",
				Type:           "openai-compatible",
				Default:        true,
				BaseURL:        fallbackUpstream.URL,
				AuthType:       "none",
				ModelDiscovery: "static",
				Models:         []ProviderModelConfig{{PublicID: "fallback-only"}},
			},
			{
				ID:             "anthropic",
				Type:           "anthropic-compatible",
				BaseURL:        anthropicUpstream.URL,
				AuthType:       "none",
				ModelDiscovery: "openai",
				IncludeModels:  rawModels,
			},
		}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}
	if got := modelsHits.Load(); got != 1 {
		t.Fatalf("startup /models hits = %d, want 1", got)
	}

	for _, model := range rawModels {
		t.Run(model, func(t *testing.T) {
			body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, model)
			req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			handler.HandleAnthropicMessages(w, req)

			resp := w.Result()
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != http.StatusOK {
				responseBody, _ := io.ReadAll(resp.Body)
				t.Fatalf("messages status = %d, want 200: %s", resp.StatusCode, responseBody)
			}
		})
	}

	messagesMu.Lock()
	gotForwardedModels := append([]string(nil), forwardedModels...)
	messagesMu.Unlock()
	if !reflect.DeepEqual(gotForwardedModels, rawModels) {
		t.Fatalf("forwarded models = %v, want raw discovered IDs %v", gotForwardedModels, rawModels)
	}
	if got := fallbackHits.Load(); got != 0 {
		t.Fatalf("fallback upstream hits = %d, want 0", got)
	}
}

func TestHandleAnthropicMessages_ExactRawOwnerPrecedesNormalizedAlias(t *testing.T) {
	type observedRequest struct {
		path  string
		model string
	}
	var rawMu sync.Mutex
	rawRequests := make([]observedRequest, 0, 2)
	rawUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode raw-owner request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		rawMu.Lock()
		rawRequests = append(rawRequests, observedRequest{path: r.URL.Path, model: payload.Model})
		rawMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/count_tokens") {
			_, _ = w.Write([]byte(`{"input_tokens":7}`))
			return
		}
		_, _ = fmt.Fprintf(w, `{"id":"msg-raw","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, payload.Model)
	}))
	defer rawUpstream.Close()

	var normalizedMu sync.Mutex
	normalizedRequests := make([]observedRequest, 0, 1)
	normalizedUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode normalized-owner request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		normalizedMu.Lock()
		normalizedRequests = append(normalizedRequests, observedRequest{path: r.URL.Path, model: payload.Model})
		normalizedMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"msg-normalized","type":"message","role":"assistant","model":%q,"content":[],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`, payload.Model)
	}))
	defer normalizedUpstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
			{
				ID:           "normalized",
				Type:         "anthropic-compatible",
				Default:      true,
				BaseURL:      normalizedUpstream.URL,
				AuthType:     "none",
				MessagesPath: "/normalized/messages",
				Models: []ProviderModelConfig{
					{PublicID: "claude-sonnet-4.5", Deployment: "normalized-sonnet", Endpoints: []string{"/v1/messages"}},
					{PublicID: "claude-haiku-4.5", Deployment: "normalized-haiku", Endpoints: []string{"/v1/messages"}},
				},
			},
			{
				ID:           "raw",
				Type:         "anthropic-compatible",
				BaseURL:      rawUpstream.URL,
				AuthType:     "none",
				MessagesPath: "/raw/messages",
				Models: []ProviderModelConfig{{
					PublicID:   "claude-sonnet-4-5",
					Deployment: "raw-sonnet",
					Endpoints:  []string{"/v1/messages"},
				}},
			},
		}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	requestMessages := func(model string) *http.Response {
		t.Helper()
		body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, model)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.HandleAnthropicMessages(w, req)
		return w.Result()
	}

	rawResp := requestMessages("claude-sonnet-4-5")
	_ = rawResp.Body.Close()
	if rawResp.StatusCode != http.StatusOK {
		t.Fatalf("raw-owner messages status = %d, want 200", rawResp.StatusCode)
	}

	countReq := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"count"}]}`))
	countReq.Header.Set("Content-Type", "application/json")
	countW := httptest.NewRecorder()
	handler.HandleAnthropicMessagesCountTokens(countW, countReq)
	countResp := countW.Result()
	_ = countResp.Body.Close()
	if countResp.StatusCode != http.StatusOK {
		t.Fatalf("raw-owner count_tokens status = %d, want 200", countResp.StatusCode)
	}

	ctx, summary := WithRequestSummary(context.Background())
	handler.observeRequestSummary(ctx, "anthropic", "claude-sonnet-4-5", false, providerEndpointMessages)
	if summary.provider != "raw" {
		t.Fatalf("request summary provider = %q, want raw", summary.provider)
	}
	publicModel, upstreamModel := handler.directAnthropicResponseModels(&models.AnthropicRequest{Model: "claude-sonnet-4-5"})
	if publicModel != "claude-sonnet-4-5" || upstreamModel != "raw-sonnet" {
		t.Fatalf("direct response models = (%q, %q), want (claude-sonnet-4-5, raw-sonnet)", publicModel, upstreamModel)
	}

	aliasResp := requestMessages("claude-haiku-4-5")
	_ = aliasResp.Body.Close()
	if aliasResp.StatusCode != http.StatusOK {
		t.Fatalf("normalized alias messages status = %d, want 200", aliasResp.StatusCode)
	}

	rawMu.Lock()
	gotRawRequests := append([]observedRequest(nil), rawRequests...)
	rawMu.Unlock()
	wantRawRequests := []observedRequest{
		{path: "/raw/messages", model: "raw-sonnet"},
		{path: "/raw/messages/count_tokens", model: "raw-sonnet"},
	}
	if !reflect.DeepEqual(gotRawRequests, wantRawRequests) {
		t.Fatalf("raw-owner requests = %+v, want %+v", gotRawRequests, wantRawRequests)
	}
	normalizedMu.Lock()
	gotNormalizedRequests := append([]observedRequest(nil), normalizedRequests...)
	normalizedMu.Unlock()
	wantNormalizedRequests := []observedRequest{{path: "/normalized/messages", model: "normalized-haiku"}}
	if !reflect.DeepEqual(gotNormalizedRequests, wantNormalizedRequests) {
		t.Fatalf("normalized-owner requests = %+v, want %+v", gotNormalizedRequests, wantNormalizedRequests)
	}
}

func TestHandleAnthropicTranslationPreservesExactRawOwnerAndNormalizedFallback(t *testing.T) {
	type observedRequest struct {
		model string
		count bool
	}
	newChatUpstream := func(t *testing.T, observations *[]observedRequest, mu *sync.Mutex) *httptest.Server {
		t.Helper()
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/chat/completions" {
				t.Errorf("upstream path = %q, want /chat/completions", r.URL.Path)
				w.WriteHeader(http.StatusNotFound)
				return
			}
			var payload models.OpenAIRequest
			if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
				t.Errorf("decode translated request: %v", err)
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			isCount := payload.Stream != nil && !*payload.Stream
			mu.Lock()
			*observations = append(*observations, observedRequest{model: payload.Model, count: isCount})
			mu.Unlock()
			if isCount {
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprintf(w, `{"id":"chat-count","object":"chat.completion","model":%q,"choices":[{"index":0,"message":{"role":"assistant","content":"x"},"finish_reason":"length"}],"usage":{"prompt_tokens":9,"completion_tokens":1,"total_tokens":10}}`, payload.Model)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"chat-1\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n", payload.Model)
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"chat-1\",\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n", payload.Model)
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}))
	}

	var rawMu sync.Mutex
	rawRequests := make([]observedRequest, 0, 2)
	rawUpstream := newChatUpstream(t, &rawRequests, &rawMu)
	defer rawUpstream.Close()

	var normalizedMu sync.Mutex
	normalizedRequests := make([]observedRequest, 0, 2)
	normalizedUpstream := newChatUpstream(t, &normalizedRequests, &normalizedMu)
	defer normalizedUpstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
			{
				ID:             "raw",
				Type:           "openai-compatible",
				Default:        true,
				BaseURL:        rawUpstream.URL,
				AuthType:       "none",
				ModelDiscovery: "static",
				Models: []ProviderModelConfig{{
					PublicID:   "claude-sonnet-4-5",
					Deployment: "raw-sonnet-upstream",
					Endpoints:  []string{"/chat/completions"},
				}},
			},
			{
				ID:             "normalized",
				Type:           "openai-compatible",
				BaseURL:        normalizedUpstream.URL,
				AuthType:       "none",
				ModelDiscovery: "static",
				Models: []ProviderModelConfig{
					{PublicID: "claude-sonnet-4.5", Deployment: "normalized-sonnet-upstream", Endpoints: []string{"/chat/completions"}},
					{PublicID: "claude-haiku-4.5", Deployment: "normalized-haiku-upstream", Endpoints: []string{"/chat/completions"}},
				},
			},
		}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	requestMessages := func(model string) *RequestSummary {
		t.Helper()
		ctx, summary := WithRequestSummary(context.Background())
		body := fmt.Sprintf(`{"model":%q,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, model)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.HandleAnthropicMessages(w, req)
		resp := w.Result()
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			responseBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("messages(%q) status = %d, want 200: %s", model, resp.StatusCode, responseBody)
		}
		return summary
	}
	requestCountTokens := func(model string) *RequestSummary {
		t.Helper()
		ctx, summary := WithRequestSummary(context.Background())
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"count"}]}`, model)
		req := httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body)).WithContext(ctx)
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.HandleAnthropicMessagesCountTokens(w, req)
		resp := w.Result()
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			responseBody, _ := io.ReadAll(resp.Body)
			t.Fatalf("count_tokens(%q) status = %d, want 200: %s", model, resp.StatusCode, responseBody)
		}
		return summary
	}

	if summary := requestMessages("claude-sonnet-4-5"); summary.provider != "raw" {
		t.Fatalf("raw Messages summary provider = %q, want raw", summary.provider)
	}
	if summary := requestCountTokens("claude-sonnet-4-5"); summary.provider != "raw" {
		t.Fatalf("raw count_tokens summary provider = %q, want raw", summary.provider)
	}
	if summary := requestMessages("claude-haiku-4-5"); summary.provider != "normalized" {
		t.Fatalf("alias Messages summary provider = %q, want normalized", summary.provider)
	}
	if summary := requestCountTokens("claude-haiku-4-5"); summary.provider != "normalized" {
		t.Fatalf("alias count_tokens summary provider = %q, want normalized", summary.provider)
	}

	rawMu.Lock()
	gotRawRequests := append([]observedRequest(nil), rawRequests...)
	rawMu.Unlock()
	wantRawRequests := []observedRequest{
		{model: "raw-sonnet-upstream", count: false},
		{model: "raw-sonnet-upstream", count: true},
	}
	if !reflect.DeepEqual(gotRawRequests, wantRawRequests) {
		t.Fatalf("raw-owner translated requests = %+v, want %+v", gotRawRequests, wantRawRequests)
	}
	normalizedMu.Lock()
	gotNormalizedRequests := append([]observedRequest(nil), normalizedRequests...)
	normalizedMu.Unlock()
	wantNormalizedRequests := []observedRequest{
		{model: "normalized-haiku-upstream", count: false},
		{model: "normalized-haiku-upstream", count: true},
	}
	if !reflect.DeepEqual(gotNormalizedRequests, wantNormalizedRequests) {
		t.Fatalf("normalized-owner translated requests = %+v, want %+v", gotNormalizedRequests, wantNormalizedRequests)
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
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"tool schema is invalid","type":"invalid_request_error","param":"tools.0","code":"invalid_tool_schema"}}`))
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
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}

	var errResp models.AnthropicError
	body, _ := io.ReadAll(resp.Body)
	if err := json.Unmarshal(body, &errResp); err != nil {
		t.Fatalf("failed to parse error response: %v", err)
	}
	if errResp.Error.Type != "invalid_request_error" {
		t.Errorf("expected error type invalid_request_error, got %q", errResp.Error.Type)
	}
	for _, want := range []string{"upstream error (400)", "tool schema is invalid", "type=invalid_request_error", "param=tools.0", "code=invalid_tool_schema"} {
		if !strings.Contains(errResp.Error.Message, want) {
			t.Errorf("message = %q, want %q", errResp.Error.Message, want)
		}
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
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("json.Unmarshal(upstream request) error = %v", err)
		}

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
		_ = json.NewEncoder(w).Encode(resp)
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
		_, _ = w.Write([]byte(`{"id":"resp-123","object":"response","status":"completed"}`))
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

// TestHandleResponsesRecordsUsage verifies the non-streaming POST /v1/responses
// path observes token usage into the per-request RequestSummary (so the traffic
// dashboard records Codex /responses traffic instead of zeros).
func TestHandleResponsesRecordsUsage(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed",` +
			`"usage":{"input_tokens":300,"output_tokens":120,"total_tokens":420,` +
			`"input_tokens_details":{"cached_tokens":90},"output_tokens_details":{"reasoning_tokens":40}}}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses",
		strings.NewReader(`{"model":"gpt-4","input":"Hello"}`))
	req.Header.Set("Content-Type", "application/json")
	ctx, summary := WithRequestSummary(req.Context())
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Result().StatusCode)
	}
	d := readSummaryForStats(summary)
	if d.prompt != 300 || d.completion != 120 || d.total != 420 {
		t.Fatalf("usage not recorded: prompt=%d completion=%d total=%d", d.prompt, d.completion, d.total)
	}
	if d.cached != 90 || d.reasoning != 40 {
		t.Fatalf("detail usage not recorded: cached=%d reasoning=%d", d.cached, d.reasoning)
	}
	// The response body must still carry the upstream usage block unchanged.
	body, _ := io.ReadAll(w.Result().Body)
	if !strings.Contains(string(body), `"input_tokens":300`) {
		t.Fatalf("response body altered: %s", body)
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
	var gotOpenAIBeta, gotLegacySessionID, gotSessionID, gotThreadID, gotClientRequestID, gotInstallationID string
	var gotInferenceCallID, gotTurnState, gotTurnMetadata, gotParentThreadID, gotWindowID, gotSubagent, gotMemgen string
	var gotAttestation, gotTimingMetrics, gotTraceparent, gotTracestate string
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		gotOpenAIBeta = r.Header.Get("OpenAI-Beta")
		gotLegacySessionID = r.Header.Get("session_id")
		gotSessionID = r.Header.Get("session-id")
		gotThreadID = r.Header.Get("thread-id")
		gotClientRequestID = r.Header.Get("X-Client-Request-Id")
		gotInstallationID = r.Header.Get("X-Codex-Installation-Id")
		gotInferenceCallID = r.Header.Get("X-Codex-Inference-Call-Id")
		gotTurnState = r.Header.Get("X-Codex-Turn-State")
		gotTurnMetadata = r.Header.Get("X-Codex-Turn-Metadata")
		gotParentThreadID = r.Header.Get("X-Codex-Parent-Thread-Id")
		gotWindowID = r.Header.Get("X-Codex-Window-Id")
		gotSubagent = r.Header.Get("X-OpenAI-Subagent")
		gotMemgen = r.Header.Get("X-OpenAI-Memgen-Request")
		gotAttestation = r.Header.Get("X-OAI-Attestation")
		gotTimingMetrics = r.Header.Get("X-ResponsesAPI-Include-Timing-Metrics")
		gotTraceparent = r.Header.Get("Traceparent")
		gotTracestate = r.Header.Get("Tracestate")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-headers","object":"response","status":"completed"}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4","input":"Hello"}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses_websockets=2026-02-06")
	req.Header.Set("session_id", "sess-abc-123")
	req.Header.Set("session-id", "sess-hyphen-123")
	req.Header.Set("thread-id", "thread-abc-123")
	req.Header.Set("X-Client-Request-Id", "client-req-456")
	req.Header.Set("X-Codex-Installation-Id", "install-789")
	req.Header.Set("X-Codex-Inference-Call-Id", "inference-123")
	req.Header.Set("X-Codex-Turn-State", "turn-state-123")
	req.Header.Set("X-Codex-Turn-Metadata", `{"turn_id":"turn-1"}`)
	req.Header.Set("X-Codex-Parent-Thread-Id", "parent-123")
	req.Header.Set("X-Codex-Window-Id", "thread-abc-123:2")
	req.Header.Set("X-OpenAI-Subagent", "collab_spawn")
	req.Header.Set("X-OpenAI-Memgen-Request", "true")
	req.Header.Set("X-OAI-Attestation", "attestation-token")
	req.Header.Set("X-ResponsesAPI-Include-Timing-Metrics", "true")
	req.Header.Set("Traceparent", "00-11111111111111111111111111111111-2222222222222222-01")
	req.Header.Set("Tracestate", "vendor=value")
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
	if gotLegacySessionID != "sess-abc-123" {
		t.Fatalf("expected session_id to be forwarded, got %q", gotLegacySessionID)
	}
	if gotSessionID != "sess-hyphen-123" {
		t.Fatalf("expected session-id to be forwarded, got %q", gotSessionID)
	}
	if gotThreadID != "thread-abc-123" {
		t.Fatalf("expected thread-id to be forwarded, got %q", gotThreadID)
	}
	if gotClientRequestID != "client-req-456" {
		t.Fatalf("expected X-Client-Request-Id to be forwarded, got %q", gotClientRequestID)
	}
	if gotInstallationID != "install-789" {
		t.Fatalf("expected X-Codex-Installation-Id to be forwarded, got %q", gotInstallationID)
	}
	if gotInferenceCallID != "inference-123" {
		t.Fatalf("expected X-Codex-Inference-Call-Id to be forwarded, got %q", gotInferenceCallID)
	}
	if gotTurnState != "turn-state-123" {
		t.Fatalf("expected X-Codex-Turn-State to be forwarded, got %q", gotTurnState)
	}
	if gotTurnMetadata != `{"turn_id":"turn-1"}` {
		t.Fatalf("expected X-Codex-Turn-Metadata to be forwarded, got %q", gotTurnMetadata)
	}
	if gotParentThreadID != "parent-123" {
		t.Fatalf("expected X-Codex-Parent-Thread-Id to be forwarded, got %q", gotParentThreadID)
	}
	if gotWindowID != "thread-abc-123:2" {
		t.Fatalf("expected X-Codex-Window-Id to be forwarded, got %q", gotWindowID)
	}
	if gotSubagent != "collab_spawn" {
		t.Fatalf("expected X-OpenAI-Subagent to be forwarded, got %q", gotSubagent)
	}
	if gotMemgen != "true" {
		t.Fatalf("expected X-OpenAI-Memgen-Request to be forwarded, got %q", gotMemgen)
	}
	if gotAttestation != "attestation-token" {
		t.Fatalf("expected X-OAI-Attestation to be forwarded, got %q", gotAttestation)
	}
	if gotTimingMetrics != "true" {
		t.Fatalf("expected X-ResponsesAPI-Include-Timing-Metrics to be forwarded, got %q", gotTimingMetrics)
	}
	if gotTraceparent != "00-11111111111111111111111111111111-2222222222222222-01" {
		t.Fatalf("expected Traceparent to be forwarded, got %q", gotTraceparent)
	}
	if gotTracestate != "vendor=value" {
		t.Fatalf("expected Tracestate to be forwarded, got %q", gotTracestate)
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

func TestHandleCompact_UsesStreamingUpstreamTimeout(t *testing.T) {
	const customTimeout = 17 * time.Minute

	deadlineCh := make(chan time.Duration, 1)
	handler := newRoundTripTestProxyHandler(t, roundTripFunc(func(r *http.Request) (*http.Response, error) {
		deadline, ok := r.Context().Deadline()
		if !ok {
			t.Fatal("expected upstream request deadline")
		}
		deadlineCh <- time.Until(deadline)
		return jsonHTTPResponse(`{"id":"resp-compact-deadline","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"compact ok"}]}]}`), nil
	}))
	WithStreamingUpstreamTimeout(customTimeout)(handler)

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", strings.NewReader(`{"model":"gpt-4","input":"Hello"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	if resp := w.Result(); resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	assertDeadlineApprox(t, <-deadlineCh, customTimeout)
}

func TestHandleResponses_LargeBodyAllowed(t *testing.T) {
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
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if upstreamHits.Load() != 1 {
		t.Fatalf("expected oversized request to be forwarded upstream once, got %d upstream hits", upstreamHits.Load())
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
		t.Fatalf("expected retained message plus compaction item, got %d output items", len(result.Output))
	}
	// First item: retained original user message.
	if result.Output[0].Type != "message" {
		t.Errorf("expected first item type message, got %q", result.Output[0].Type)
	}
	if result.Output[0].Role != "user" {
		t.Errorf("expected role user, got %q", result.Output[0].Role)
	}
	if len(result.Output[0].Content) == 0 || result.Output[0].Content[0].Text != "Hello" {
		t.Errorf("expected retained user text in message content, got %+v", result.Output[0].Content)
	}
	// Second item: compaction with encrypted_content.
	if result.Output[1].Type != "compaction" {
		t.Errorf("expected second item type compaction, got %q", result.Output[1].Type)
	}
	if got := decodeCompactionSummaryForTest(t, result.Output[1].EncryptedContent); got != "compacted summary of conversation" {
		t.Errorf("expected encoded compaction summary, got %q", got)
	}
}

func TestHandleCompact_DeduplicatesConcurrentIdenticalRequests(t *testing.T) {
	const callers = 8

	var upstreamCalls atomic.Int32
	upstreamStarted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if call := upstreamCalls.Add(1); call == 1 {
			close(upstreamStarted)
		}
		<-releaseUpstream
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-compact-dedup","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"shared compact summary"}]}]}`))
	})

	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "compact this shared history"},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	start := make(chan struct{})
	var wg sync.WaitGroup
	statuses := make([]int, callers)
	bodies := make([][]byte, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			handler.HandleCompact(w, req)

			resp := w.Result()
			statuses[i] = resp.StatusCode
			bodies[i], _ = io.ReadAll(resp.Body)
		}(i)
	}

	close(start)
	<-upstreamStarted
	waitForCompactInflightWaiters(t, handler, callers-1)
	close(releaseUpstream)
	wg.Wait()

	if upstreamCalls.Load() != 1 {
		t.Fatalf("expected one upstream compact call for identical in-flight requests, got %d", upstreamCalls.Load())
	}
	for i := range statuses {
		if statuses[i] != http.StatusOK {
			t.Fatalf("caller %d expected 200, got %d: %s", i, statuses[i], bodies[i])
		}
		summary, encryptedSummary := requireCompactResponseSummaryForTest(t, bodies[i])
		if summary != "shared compact summary" || encryptedSummary != "shared compact summary" {
			t.Fatalf("caller %d expected shared compact summary, got summary=%q encrypted=%q", i, summary, encryptedSummary)
		}
	}
}

func TestHandleCompact_CapsInflightErrorBodyReplay(t *testing.T) {
	const callers = 2

	largeBody := strings.Repeat("x", compactUpstreamErrorBodySize+1024)
	var upstreamCalls atomic.Int32
	upstreamStarted := make(chan struct{})
	releaseUpstream := make(chan struct{})
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if call := upstreamCalls.Add(1); call == 1 {
			close(upstreamStarted)
		}
		<-releaseUpstream
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", fmt.Sprintf("%d", len(largeBody)))
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, largeBody)
	})

	reqBody := []byte(`{"model":"gpt-5.4","input":"compact error replay"}`)
	start := make(chan struct{})
	var wg sync.WaitGroup
	statuses := make([]int, callers)
	contentLengths := make([]string, callers)
	bodies := make([][]byte, callers)
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			handler.HandleCompact(w, req)

			resp := w.Result()
			statuses[i] = resp.StatusCode
			contentLengths[i] = resp.Header.Get("Content-Length")
			bodies[i], _ = io.ReadAll(resp.Body)
		}(i)
	}

	close(start)
	<-upstreamStarted
	waitForCompactInflightWaiters(t, handler, callers-1)
	close(releaseUpstream)
	wg.Wait()

	if upstreamCalls.Load() != 1 {
		t.Fatalf("expected one upstream compact call for identical in-flight error requests, got %d", upstreamCalls.Load())
	}
	for i := range statuses {
		if statuses[i] != http.StatusInternalServerError {
			t.Fatalf("caller %d expected 500, got %d", i, statuses[i])
		}
		if len(bodies[i]) != compactUpstreamErrorBodySize {
			t.Fatalf("caller %d expected capped body length %d, got %d", i, compactUpstreamErrorBodySize, len(bodies[i]))
		}
		if contentLengths[i] != "" {
			t.Fatalf("caller %d expected stale Content-Length to be cleared, got %q", i, contentLengths[i])
		}
	}
}

func waitForCompactInflightWaiters(t *testing.T, handler *ProxyHandler, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		got := 0
		handler.compactInflightMu.Lock()
		for _, call := range handler.compactInflight {
			got += int(call.waiters.Load())
		}
		handler.compactInflightMu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}

	handler.compactInflightMu.Lock()
	got := 0
	for _, call := range handler.compactInflight {
		got += int(call.waiters.Load())
	}
	handler.compactInflightMu.Unlock()
	t.Fatalf("timed out waiting for %d in-flight compact waiters, got %d", want, got)
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

func TestHandleCompact_FallsBackToChunkedCompactionOnUpstream413(t *testing.T) {
	largeText := strings.Repeat("a", compactUpstreamChunkBodySize*3/8)
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "first " + largeText},
				},
			},
			map[string]interface{}{
				"type": "message",
				"role": "assistant",
				"content": []map[string]string{
					{"type": "input_text", "text": "second " + largeText},
				},
			},
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "third " + largeText},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	var mu sync.Mutex
	var bodySizes []int
	var inputCounts []int
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("expected upstream path /responses, got %q", r.URL.Path)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}
		instructions, _ := req["instructions"].(string)
		if !strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
			t.Fatalf("expected compaction prompt as instructions, got %q", instructions)
		}
		input, ok := req["input"].([]interface{})
		if !ok {
			t.Fatalf("expected input array, got %#v", req["input"])
		}

		mu.Lock()
		bodySizes = append(bodySizes, len(body))
		inputCounts = append(inputCounts, len(input))
		call := len(bodySizes)
		mu.Unlock()

		switch call {
		case 1:
			if len(input) != 3 {
				t.Fatalf("expected initial one-shot compact input to have 3 items, got %d", len(input))
			}
			if len(body) <= compactUpstreamChunkBodySize {
				t.Fatalf("expected initial compact body to exceed chunk target, got %d bytes", len(body))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"payload too large"}}`))
		case 2:
			if len(input) != 1 {
				t.Fatalf("expected first fallback chunk to be one historical text message, got %d", len(input))
			}
			if len(body) > compactUpstreamChunkBodySize {
				t.Fatalf("expected first fallback chunk body to fit target, got %d bytes", len(body))
			}
			text := requireMessageTextWithRole(t, input[0], "user")
			if !strings.Contains(text, "Historical compact input chunk") || !strings.Contains(text, "first ") || !strings.Contains(text, "second ") || strings.Contains(text, "third ") {
				t.Fatalf("expected first historical chunk to contain first two original items only, got %q", text)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-chunk-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary of first two items"}]}]}`))
		case 3:
			if len(input) != 1 {
				t.Fatalf("expected second fallback chunk to be one historical text message, got %d", len(input))
			}
			if len(body) > compactUpstreamChunkBodySize {
				t.Fatalf("expected second fallback chunk body to fit target, got %d bytes", len(body))
			}
			text := requireMessageTextWithRole(t, input[0], "user")
			if !strings.Contains(text, "Historical compact input chunk") || !strings.Contains(text, "third ") || strings.Contains(text, "first ") || strings.Contains(text, "second ") {
				t.Fatalf("expected second historical chunk to contain final original item only, got %q", text)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-chunk-2","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary of final item"}]}]}`))
		case 4:
			if len(input) != 2 {
				t.Fatalf("expected merge request to contain 2 chunk summaries, got %d", len(input))
			}
			if got := requireMessageTextWithRole(t, input[0], "user"); got != "Partial checkpoint summary 1 of 2:\nsummary of first two items" {
				t.Fatalf("unexpected first merge summary: %q", got)
			}
			if got := requireMessageTextWithRole(t, input[1], "user"); got != "Partial checkpoint summary 2 of 2:\nsummary of final item" {
				t.Fatalf("unexpected second merge summary: %q", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-merged","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final merged summary"}]}]}`))
		default:
			t.Fatalf("unexpected /responses request count %d", call)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	mu.Lock()
	gotBodySizes := append([]int(nil), bodySizes...)
	gotInputCounts := append([]int(nil), inputCounts...)
	mu.Unlock()
	if len(gotBodySizes) != 4 {
		t.Fatalf("expected 4 upstream requests (initial + 2 chunks + merge), got %d", len(gotBodySizes))
	}
	if gotInputCounts[0] != 3 || gotInputCounts[1] != 1 || gotInputCounts[2] != 1 || gotInputCounts[3] != 2 {
		t.Fatalf("unexpected upstream input counts: %v", gotInputCounts)
	}
	if gotBodySizes[0] <= gotBodySizes[1] || gotBodySizes[0] <= gotBodySizes[2] {
		t.Fatalf("expected fallback chunk bodies to be smaller than initial body, sizes=%v", gotBodySizes)
	}

	body, _ := io.ReadAll(resp.Body)
	_, gotCompaction := requireCompactResponseSummaryForTest(t, body)
	if gotCompaction != "final merged summary" {
		t.Errorf("expected encoded final merged summary, got %q", gotCompaction)
	}
}

func TestHandleCompact_UsesLearnedChunkTargetAfterPrior413(t *testing.T) {
	const initialTarget = 256 << 10
	const rejectAbove = initialTarget / 2

	var oversizedPosts atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		if len(body) > rejectAbove {
			oversizedPosts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-learned-target","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]}]}`))
	})
	handler.compactChunkBodyBytes = initialTarget

	chunkText := strings.Repeat("a", 35<<10)
	input := make([]interface{}, 0, 4)
	for i := 0; i < 4; i++ {
		input = append(input, map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": fmt.Sprintf("chunk %d: %s", i+1, chunkText)},
			},
		})
	}
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": input,
	})
	if err != nil {
		t.Fatalf("failed to marshal compact request: %v", err)
	}

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		handler.HandleCompact(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("attempt %d: expected 200, got %d: %s", i+1, resp.StatusCode, body)
		}
		if i == 0 && oversizedPosts.Load() != 1 {
			t.Fatalf("expected first request to learn after one oversized post, got %d", oversizedPosts.Load())
		}
		if i == 1 && oversizedPosts.Load() != 1 {
			t.Fatalf("expected second request to reuse learned target without another oversized post, got %d", oversizedPosts.Load())
		}
	}
}

func TestCompactResponsesRequestDepth_ProactiveSplitDoesNotConsumeBudgetBeforePosting(t *testing.T) {
	const targetBodySize = 64 << 10

	input := make([]json.RawMessage, 0, 8)
	for i := 0; i < 8; i++ {
		message, err := compactTextInputRawMessage(fmt.Sprintf("item %d: %s", i+1, strings.Repeat("x", 12<<10)))
		if err != nil {
			t.Fatalf("build input message: %v", err)
		}
		input = append(input, message)
	}
	inputRaw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	requestFields := map[string]json.RawMessage{
		"model": json.RawMessage(`"gpt-5.4"`),
		"input": inputRaw,
	}

	body, err := marshalCompactResponsesRequest(requestFields, nil)
	if err != nil {
		t.Fatalf("marshal compact request: %v", err)
	}
	if len(body) <= targetBodySize {
		t.Fatalf("test setup expected original body %d to exceed target %d", len(body), targetBodySize)
	}

	fallbackFields, _, err := compactFallbackRequestFieldsForBodySize(requestFields, targetBodySize)
	if err != nil {
		t.Fatalf("build fallback fields: %v", err)
	}
	chunks, _, _, err := splitCompactInputAsHistoricalChunksByBodySize(fallbackFields, input, targetBodySize)
	if err != nil {
		t.Fatalf("split input: %v", err)
	}
	if len(chunks) == 0 {
		t.Fatal("test setup expected at least one chunk")
	}
	expectedAttempts := len(chunks)
	if len(chunks) > 1 {
		expectedAttempts++
	}

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if len(body) > targetBodySize {
			t.Fatalf("proactive split should avoid posting original oversized body: got %d > %d", len(body), targetBodySize)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]}]}`))
	})

	budget := newCompactBudget(expectedAttempts)
	summary, resp, err := handler.compactResponsesRequestDepth(context.Background(), requestFields, nil, 0, targetBodySize, budget, true)
	if err != nil {
		t.Fatalf("expected proactive split to fit exact budget: %v", err)
	}
	if resp != nil {
		t.Fatalf("expected successful summary, got response status %d", resp.StatusCode)
	}
	if summary != "summary" {
		t.Fatalf("expected summary, got %q", summary)
	}
	if budget.attempts > expectedAttempts {
		t.Fatalf("expected at most %d budgeted attempts, got %d", expectedAttempts, budget.attempts)
	}
}

func TestCompactResponsesRequestInChunks_RespectsChunkConcurrency(t *testing.T) {
	const (
		targetBodySize = 96 << 10
		concurrency    = 2
	)

	texts := make([]string, 0, 7)
	for i := 0; i < 7; i++ {
		texts = append(texts, fmt.Sprintf("item %d: %s", i+1, strings.Repeat("x", 60<<10)))
	}
	requestFields, chunks := compactChunkTestRequestFields(t, targetBodySize, texts)
	if len(chunks) < 5 {
		t.Fatalf("test setup expected at least 5 chunks, got %d", len(chunks))
	}

	var activeChunks atomic.Int32
	var maxActiveChunks atomic.Int32
	var chunkCalls atomic.Int32
	var mergeCalls atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		input := upstreamInputItems(t, req)
		if len(input) > 0 {
			text := requireMessageTextWithRole(t, input[0], "user")
			if strings.HasPrefix(text, "Partial checkpoint summary") {
				mergeCalls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"resp-merge","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"merged summary"}]}]}`))
				return
			}
		}

		current := activeChunks.Add(1)
		recordMaxInt32(&maxActiveChunks, current)
		if current > concurrency {
			t.Errorf("active chunk requests = %d, want <= %d", current, concurrency)
		}
		time.Sleep(25 * time.Millisecond)
		activeChunks.Add(-1)
		call := chunkCalls.Add(1)

		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"resp-chunk-%d","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"chunk summary %d"}]}]}`, call, call)
	})
	handler.compactChunkConcurrency = concurrency

	budget := newCompactBudget(len(chunks) + 2)
	summary, err := handler.compactResponsesRequestInChunks(context.Background(), requestFields, nil, 0, targetBodySize, budget)
	if err != nil {
		t.Fatalf("compact chunk request failed: %v", err)
	}
	if summary != "merged summary" {
		t.Fatalf("expected merged summary, got %q", summary)
	}
	if got := chunkCalls.Load(); got != int32(len(chunks)) {
		t.Fatalf("expected %d chunk calls, got %d", len(chunks), got)
	}
	if mergeCalls.Load() != 1 {
		t.Fatalf("expected one merge call, got %d", mergeCalls.Load())
	}
	if got := maxActiveChunks.Load(); got != concurrency {
		t.Fatalf("expected max active chunk requests %d, got %d", concurrency, got)
	}
}

func TestCompactResponsesRequestInChunks_CancelsFanoutOnFirstError(t *testing.T) {
	const (
		targetBodySize = 96 << 10
		concurrency    = 2
	)

	texts := make([]string, 0, 7)
	for i := 0; i < 7; i++ {
		texts = append(texts, fmt.Sprintf("item %d: %s", i+1, strings.Repeat("y", 60<<10)))
	}
	requestFields, chunks := compactChunkTestRequestFields(t, targetBodySize, texts)
	if len(chunks) < 5 {
		t.Fatalf("test setup expected at least 5 chunks, got %d", len(chunks))
	}

	var chunkCalls atomic.Int32
	var mergeCalls atomic.Int32
	var failurePublished atomic.Bool
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		call := chunkCalls.Add(1)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			// The second request is the asserted failure source and must remain
			// active long enough to receive its complete request body. Once it
			// publishes the 500, compact fanout intentionally cancels in-flight
			// siblings; net/http may surface those aborted uploads as
			// context.Canceled, io.ErrUnexpectedEOF, or an opaque closed-transport
			// error. Only tolerate them after the asserted failure was published and
			// this specific sibling's request context has actually been canceled.
			if call != 2 && failurePublished.Load() {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(100 * time.Millisecond):
				}
			}
			t.Errorf("read active upstream body for call %d: %v", call, err)
			return
		}
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		input := upstreamInputItems(t, req)
		if len(input) > 0 {
			text := requireMessageTextWithRole(t, input[0], "user")
			if strings.HasPrefix(text, "Partial checkpoint summary") {
				mergeCalls.Add(1)
				t.Fatal("merge should not run after a chunk fanout error")
			}
		}

		if call == 2 {
			failurePublished.Store(true)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"chunk failed"}}`))
			return
		}
		time.Sleep(25 * time.Millisecond)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":"resp-chunk-%d","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"chunk summary %d"}]}]}`, call, call)
	})
	handler.compactChunkConcurrency = concurrency

	budget := newCompactBudget(len(chunks) + 2)
	summary, err := handler.compactResponsesRequestInChunks(context.Background(), requestFields, nil, 0, targetBodySize, budget)
	if err == nil {
		t.Fatalf("expected fanout error, got summary %q", summary)
	}
	if !strings.Contains(err.Error(), "returned 500") {
		t.Fatalf("expected chunk 500 error, got %v", err)
	}
	if mergeCalls.Load() != 0 {
		t.Fatalf("expected no merge calls after fanout error, got %d", mergeCalls.Load())
	}
	if got := chunkCalls.Load(); got >= int32(len(chunks)) {
		t.Fatalf("expected cancellation to stop before all %d chunks ran, got %d chunk calls", len(chunks), got)
	}
}

func TestCompactResponsesRequestInChunks_PreservesWorkerErrorAfterShutdown(t *testing.T) {
	const targetBodySize = 96 << 10
	texts := make([]string, 0, 7)
	for i := 0; i < 7; i++ {
		texts = append(texts, fmt.Sprintf("item %d: %s", i+1, strings.Repeat("z", 60<<10)))
	}
	requestFields, chunks := compactChunkTestRequestFields(t, targetBodySize, texts)
	if len(chunks) < 3 {
		t.Fatalf("test setup expected at least 3 chunks, got %d", len(chunks))
	}

	siblingStarted := make(chan struct{})
	fanoutCanceled := make(chan struct{})
	releaseSibling := make(chan struct{})
	var calls atomic.Int32
	handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
		switch call := calls.Add(1); call {
		case 1:
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body: io.NopCloser(strings.NewReader(
					`{"id":"chunk-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"first summary"}]}]}`)),
				Request: req,
			}, nil
		case 2:
			<-siblingStarted
			return &http.Response{
				StatusCode: http.StatusInternalServerError,
				Header:     http.Header{"X-Worker-Failure": []string{"independent"}},
				Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"worker failed"}}`)),
				Request:    req,
			}, nil
		case 3:
			close(siblingStarted)
			<-req.Context().Done()
			close(fanoutCanceled)
			<-releaseSibling
			return nil, req.Context().Err()
		default:
			return nil, fmt.Errorf("unexpected upstream call %d", call)
		}
	})
	handler.maxRetries = 1
	handler.compactChunkConcurrency = 2
	handler.stats = newStatsCollector()
	ctx, cancel := handler.newInferenceUpstreamContext(false)
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := handler.compactResponsesRequestInChunks(ctx, requestFields, nil, 0, targetBodySize, newCompactBudget(len(chunks)+2))
		done <- err
	}()
	waitForLifecycleSignal(t, fanoutCanceled, "fanout worker error cancellation")
	handler.BeginShutdown()
	close(releaseSibling)

	var err error
	select {
	case err = <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for compact fanout result")
	}
	if err == nil || !strings.Contains(err.Error(), "returned 500") {
		t.Fatalf("fanout error = %v, want independent worker 500", err)
	}
	if errors.Is(err, context.Canceled) {
		t.Fatalf("worker failure was replaced by shutdown cancellation: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", nil)
	requestCtx, summary := WithRequestSummary(req.Context())
	req = req.WithContext(requestCtx)
	if handler.handleShutdownError(httptest.NewRecorder(), req, ctx, err) {
		t.Fatal("independent worker failure was misclassified as local shutdown")
	}
	providerStatus := upstreamStatusCode(err, http.StatusBadGateway)
	if providerStatus != http.StatusBadGateway {
		t.Fatalf("provider status = %d, want classified 502", providerStatus)
	}
	handler.RecordRequest(summary, providerStatus, "fanout-race", time.Millisecond)
	snap := handler.stats.snapshot()
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 1 {
		t.Fatalf("stats = requests:%d errors:%d, want 1/1", snap.Totals.Requests, snap.Totals.Errors)
	}
}

func TestCompactResponsesRequestInChunks_ResplitsUnsentFanoutAfterLearnedTarget(t *testing.T) {
	const targetBodySize = 160 << 10
	rejectAbove := targetBodySize / 2

	texts := []string{
		"first: " + strings.Repeat("a", 72<<10),
		"second: " + strings.Repeat("b", 100<<10),
		"third: " + strings.Repeat("c", 100<<10),
		"fourth: " + strings.Repeat("d", 100<<10),
	}
	requestFields, chunks := compactChunkTestRequestFields(t, targetBodySize, texts)
	if len(chunks) != len(texts) {
		t.Fatalf("test setup expected one item per chunk, got %d chunks for %d items", len(chunks), len(texts))
	}

	var oversizedPosts atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		if len(body) > rejectAbove {
			oversizedPosts.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary"}]}]}`))
	})
	handler.compactChunkConcurrency = 1

	budget := newCompactBudget(32)
	summary, err := handler.compactResponsesRequestInChunks(context.Background(), requestFields, nil, 0, targetBodySize, budget)
	if err != nil {
		t.Fatalf("compact chunk request failed: %v", err)
	}
	if summary != "summary" {
		t.Fatalf("expected summary, got %q", summary)
	}
	if got := oversizedPosts.Load(); got != 1 {
		t.Fatalf("expected only the first over-cap sibling to discover the smaller target, got %d oversized posts", got)
	}
}

func TestSplitCompactInputAsHistoricalChunks_FlattenPreservesOriginalItems(t *testing.T) {
	first, err := compactTextInputRawMessage("first " + strings.Repeat("a", 4096))
	if err != nil {
		t.Fatalf("build first message: %v", err)
	}
	second, err := compactTextInputRawMessage("second " + strings.Repeat("b", 4096))
	if err != nil {
		t.Fatalf("build second message: %v", err)
	}
	requestFields := map[string]json.RawMessage{"model": json.RawMessage(`"gpt-5.4"`)}

	_, oneSize, err := compactHistoricalChunkFitsBodySize(requestFields, []json.RawMessage{first}, 1, 1<<20)
	if err != nil {
		t.Fatalf("measure one-item chunk: %v", err)
	}
	_, twoSize, err := compactHistoricalChunkFitsBodySize(requestFields, []json.RawMessage{first, second}, 1, 1<<20)
	if err != nil {
		t.Fatalf("measure two-item chunk: %v", err)
	}
	maxBodySize := oneSize + 16
	if twoSize <= maxBodySize {
		t.Fatalf("test setup expected two-item historical chunk size %d to exceed target %d", twoSize, maxBodySize)
	}

	chunks, splitAny, expandedItems, err := splitCompactInputAsHistoricalChunksByBodySize(requestFields, []json.RawMessage{first, second}, maxBodySize)
	if err != nil {
		t.Fatalf("split historical chunks: %v", err)
	}
	if splitAny {
		t.Fatalf("did not expect oversized input item splitting")
	}
	if expandedItems != 2 {
		t.Fatalf("expandedItems = %d, want 2", expandedItems)
	}
	if len(chunks) != 2 || len(chunks[0]) != 1 || len(chunks[1]) != 1 {
		t.Fatalf("expected two one-item original chunks, got %#v", chunks)
	}
	if !bytes.Equal(chunks[0][0], first) || !bytes.Equal(chunks[1][0], second) {
		t.Fatalf("expected chunks to retain original items for future re-splitting")
	}

	remaining := flattenCompactChunks(chunks[1:])
	if len(remaining) != 1 || !bytes.Equal(remaining[0], second) {
		t.Fatalf("expected flattened remainder to contain original second item, got %#v", remaining)
	}
	if bytes.Contains(remaining[0], []byte("Historical compact input chunk")) {
		t.Fatalf("flattened remainder should not contain a nested historical wrapper: %s", remaining[0])
	}

	wireInput, err := compactHistoricalChunkInput(chunks[0], 1)
	if err != nil {
		t.Fatalf("wrap chunk for upstream: %v", err)
	}
	if len(wireInput) != 1 {
		t.Fatalf("expected one wrapped wire message, got %#v", wireInput)
	}
	var wireMessage struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(wireInput[0], &wireMessage); err != nil {
		t.Fatalf("unmarshal wrapped wire message: %v", err)
	}
	if len(wireMessage.Content) == 0 || !strings.Contains(wireMessage.Content[0].Text, "Historical compact input chunk") {
		t.Fatalf("expected send-time historical wrapper, got %#v", wireMessage)
	}
}

func TestHandleCompact_FallbackWrapsChunksAsHistoricalText(t *testing.T) {
	inputHasType := func(input []interface{}, want string) bool {
		for _, raw := range input {
			item, ok := raw.(map[string]interface{})
			if ok && item["type"] == want {
				return true
			}
		}
		return false
	}

	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "summarize this tool-call history"},
				},
			},
			map[string]interface{}{
				"type":      "function_call",
				"call_id":   "call_missing_output",
				"name":      "lookup",
				"arguments": `{"query":"large history"}`,
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var calls atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		var upstreamReq map[string]interface{}
		if err := json.Unmarshal(body, &upstreamReq); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}
		input, ok := upstreamReq["input"].([]interface{})
		if !ok {
			t.Fatalf("expected upstream input array, got %#v", upstreamReq["input"])
		}

		switch calls.Add(1) {
		case 1:
			if !inputHasType(input, "function_call") {
				t.Fatalf("expected initial compact attempt to preserve function_call, got %#v", input)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
			return
		default:
			if inputHasType(input, "function_call") {
				t.Fatalf("fallback chunk replayed raw function_call input: %#v", input)
			}
			if len(input) != 1 {
				t.Fatalf("expected fallback chunk to be one historical text message, got %#v", input)
			}
			text := requireMessageTextWithRole(t, input[0], "user")
			if !strings.Contains(text, "Historical compact input chunk") || !strings.Contains(text, `"type":"function_call"`) {
				t.Fatalf("expected serialized function_call in historical text chunk, got %q", text)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-chunk","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summarized tool-call history"}]}]}`))
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleCompact(w, req)
	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 for historical-text fallback chunk, got %d: %s", resp.StatusCode, body)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected initial 413 plus one fallback chunk, got %d calls", calls.Load())
	}

	body, _ := io.ReadAll(resp.Body)
	gotSummary, gotCompaction := requireCompactResponseSummaryForTest(t, body)
	if gotSummary != "summarized tool-call history" || gotCompaction != "summarized tool-call history" {
		t.Fatalf("unexpected compact response summary=%q compaction=%q", gotSummary, gotCompaction)
	}
}

func TestHandleCompact_ShrinksHistoricalChunksOnPromptTokenLimit(t *testing.T) {
	const initialTarget = 128 * 1024
	const tokenCap = (initialTarget * 3) / 4

	largeText := strings.Repeat("token ", 25000)
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": largeText},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var calls atomic.Int32
	var sawPromptLimit atomic.Bool
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}

		if calls.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
			return
		}

		if len(body) > tokenCap {
			sawPromptLimit.Store(true)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"prompt token count of 341106 exceeds the limit of 272000","code":"model_max_prompt_tokens_exceeded"}}`))
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"token-limit chunk ok"}]}]}`))
	})
	handler.compactChunkBodyBytes = initialTarget

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleCompact(w, req)
	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 after shrinking prompt-token-limited chunk, got %d: %s", resp.StatusCode, body)
	}
	if !sawPromptLimit.Load() {
		t.Fatalf("expected upstream prompt-token-limit 400 to be exercised")
	}
	body, _ := io.ReadAll(resp.Body)
	gotSummary, gotCompaction := requireCompactResponseSummaryForTest(t, body)
	if gotSummary != "token-limit chunk ok" || gotCompaction != "token-limit chunk ok" {
		t.Fatalf("unexpected compact response summary=%q compaction=%q", gotSummary, gotCompaction)
	}
}

func TestHandleCompact_FallsBackToChunkedCompactionForStringInputOnUpstream413(t *testing.T) {
	largeText := strings.Repeat("a", compactUpstreamChunkBodySize+(4*1024))
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": largeText,
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	var calls atomic.Int32
	var chunkCalls atomic.Int32
	var mergeCalls atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}

		switch call := calls.Add(1); call {
		case 1:
			if len(body) <= compactUpstreamChunkBodySize {
				t.Fatalf("expected initial compact body to exceed chunk target, got %d bytes", len(body))
			}
			if _, ok := req["input"].(string); !ok {
				t.Fatalf("expected initial input to remain a string, got %#v", req["input"])
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
			return
		default:
			if len(body) > compactUpstreamChunkBodySize {
				t.Fatalf("fallback compact body exceeded chunk target: got %d bytes", len(body))
			}
		}

		input, ok := req["input"].([]interface{})
		if !ok || len(input) == 0 {
			t.Fatalf("expected fallback input array, got %#v", req["input"])
		}
		text := requireMessageTextWithRole(t, input[0], "user")
		switch {
		case strings.Contains(text, "Partial checkpoint summary"):
			mergeCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-merge","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"final merged string summary"}]}]}`))
		case strings.Contains(text, "Oversized compact input item chunk"):
			chunkCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-chunk","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"chunk summary"}]}]}`))
		default:
			t.Fatalf("unexpected fallback compact input text: %.200q", text)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if chunkCalls.Load() < 2 {
		t.Fatalf("expected at least two fallback chunk requests, got %d", chunkCalls.Load())
	}
	if mergeCalls.Load() != 1 {
		t.Fatalf("expected one merge request, got %d", mergeCalls.Load())
	}
	if calls.Load() != 1+chunkCalls.Load()+mergeCalls.Load() {
		t.Fatalf("unexpected upstream request accounting: calls=%d chunks=%d merges=%d", calls.Load(), chunkCalls.Load(), mergeCalls.Load())
	}

	body, _ := io.ReadAll(resp.Body)
	_, gotCompaction := requireCompactResponseSummaryForTest(t, body)
	if gotCompaction != "final merged string summary" {
		t.Fatalf("expected encoded final merged summary, got %q", gotCompaction)
	}
}

func TestHandleCompact_NormalizesOversizedToolsBeforeInternalCompaction(t *testing.T) {
	const inputText = "please compact this small history"
	hugeToolDescription := strings.Repeat("a", compactUpstreamChunkBodySize)
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": inputText},
				},
			},
		},
		"tools": []interface{}{
			map[string]interface{}{
				"type":        "function",
				"name":        "oversized_tool",
				"description": hugeToolDescription,
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		"tool_choice": map[string]interface{}{"type": "function", "name": "oversized_tool"},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}
	if len(reqBody) <= compactUpstreamChunkBodySize {
		t.Fatalf("test setup expected caller tools to push body over chunk target, got %d bytes", len(reqBody))
	}

	var calls atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if call := calls.Add(1); call != 1 {
			t.Fatalf("unexpected /responses request count %d", call)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		if len(body) > compactUpstreamChunkBodySize {
			t.Fatalf("expected normalized compact body to fit target, got %d bytes", len(body))
		}

		var upstreamRequest map[string]interface{}
		if err := json.Unmarshal(body, &upstreamRequest); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}
		if _, ok := upstreamRequest["tools"]; ok {
			t.Fatalf("internal compaction request must omit caller tools: %s", body)
		}
		if _, ok := upstreamRequest["tool_choice"]; ok {
			t.Fatalf("internal compaction request must omit caller tool_choice: %s", body)
		}
		if got := upstreamRequest["model"]; got != "gpt-5.4" {
			t.Fatalf("expected model to survive normalization, got %#v", got)
		}
		input, ok := upstreamRequest["input"].([]interface{})
		if !ok || len(input) != 1 {
			t.Fatalf("expected one compact input item, got %#v", upstreamRequest["input"])
		}
		if got := requireMessageTextWithRole(t, input[0], "user"); got != inputText {
			t.Fatalf("expected ordinary input to survive normalization, got %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-normalized-tools","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary without caller tools"}]}]}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly one normalized upstream request, got %d", calls.Load())
	}

	body, _ := io.ReadAll(resp.Body)
	summary, encryptedSummary := requireCompactResponseSummaryForTest(t, body)
	if summary == "" || summary != "summary without caller tools" {
		t.Fatalf("expected nonempty normalized tools summary, got %q", summary)
	}
	if encryptedSummary != summary {
		t.Fatalf("expected encoded normalized tools summary %q, got %q", summary, encryptedSummary)
	}
}

func TestHandleCompact_NormalizesStructuredTextFormatBeforeInternalCompaction(t *testing.T) {
	const inputText = "please compact this history with a text schema"
	hugeJSONSchemaDescription := strings.Repeat("b", compactUpstreamChunkBodySize)
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": inputText},
				},
			},
		},
		"tools": []interface{}{
			map[string]interface{}{
				"type":        "function",
				"name":        "small_tool",
				"description": "caller tool must not influence internal compaction",
				"parameters": map[string]interface{}{
					"type":       "object",
					"properties": map[string]interface{}{},
				},
			},
		},
		"tool_choice": map[string]interface{}{"type": "function", "name": "small_tool"},
		"text": map[string]interface{}{
			"format": map[string]interface{}{
				"type": "json_schema",
				"name": "oversized_schema",
				"schema": map[string]interface{}{
					"type":        "object",
					"description": hugeJSONSchemaDescription,
				},
			},
			"verbosity": "low",
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}
	if len(reqBody) <= compactUpstreamChunkBodySize {
		t.Fatalf("test setup expected structured format to push body over chunk target, got %d bytes", len(reqBody))
	}

	var calls atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if call := calls.Add(1); call != 1 {
			t.Fatalf("unexpected /responses request count %d", call)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		if len(body) > compactUpstreamChunkBodySize {
			t.Fatalf("expected normalized compact body to fit target, got %d bytes", len(body))
		}

		var upstreamRequest map[string]interface{}
		if err := json.Unmarshal(body, &upstreamRequest); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}
		if _, ok := upstreamRequest["tools"]; ok {
			t.Fatalf("internal compaction request must omit caller tools: %s", body)
		}
		if _, ok := upstreamRequest["tool_choice"]; ok {
			t.Fatalf("internal compaction request must omit caller tool_choice: %s", body)
		}
		text, ok := upstreamRequest["text"].(map[string]interface{})
		if !ok {
			t.Fatalf("expected non-format text controls to survive normalization, got %#v", upstreamRequest["text"])
		}
		if _, ok := text["format"]; ok {
			t.Fatalf("internal compaction request must omit caller text.format: %#v", text)
		}
		if got := text["verbosity"]; got != "low" {
			t.Fatalf("expected text.verbosity to survive normalization, got %#v", got)
		}
		if got := upstreamRequest["model"]; got != "gpt-5.4" {
			t.Fatalf("expected model to survive normalization, got %#v", got)
		}
		input, ok := upstreamRequest["input"].([]interface{})
		if !ok || len(input) != 1 {
			t.Fatalf("expected one compact input item, got %#v", upstreamRequest["input"])
		}
		if got := requireMessageTextWithRole(t, input[0], "user"); got != inputText {
			t.Fatalf("expected ordinary input to survive normalization, got %q", got)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-normalized-text","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary without caller schema"}]}]}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if calls.Load() != 1 {
		t.Fatalf("expected exactly one normalized upstream request, got %d", calls.Load())
	}

	body, _ := io.ReadAll(resp.Body)
	summary, encryptedSummary := requireCompactResponseSummaryForTest(t, body)
	if summary == "" || summary != "summary without caller schema" {
		t.Fatalf("expected nonempty normalized text summary, got %q", summary)
	}
	if encryptedSummary != summary {
		t.Fatalf("expected encoded normalized text summary %q, got %q", summary, encryptedSummary)
	}
}

func TestHandleCompact_ReturnsOriginal413WhenChunkedMergeFails(t *testing.T) {
	largeText := strings.Repeat("a", compactUpstreamChunkBodySize*5/8)
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "first " + largeText},
				},
			},
			map[string]interface{}{
				"type": "message",
				"role": "assistant",
				"content": []map[string]string{
					{"type": "input_text", "text": "second " + largeText},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	const original413Body = `{"error":{"message":"original payload too large"}}`

	var mu sync.Mutex
	var inputCounts []int
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}
		input, ok := req["input"].([]interface{})
		if !ok {
			t.Fatalf("expected input array, got %#v", req["input"])
		}

		mu.Lock()
		inputCounts = append(inputCounts, len(input))
		call := len(inputCounts)
		mu.Unlock()

		switch call {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(original413Body))
		case 2:
			if len(input) != 1 {
				t.Fatalf("expected first fallback chunk to have 1 item, got %d", len(input))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-chunk-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary of first item"}]}]}`))
		case 3:
			if len(input) != 1 {
				t.Fatalf("expected second fallback chunk to have 1 item, got %d", len(input))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-chunk-2","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary of second item"}]}]}`))
		case 4:
			if len(input) != 2 {
				t.Fatalf("expected merge request to contain 2 chunk summaries, got %d", len(input))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"merge failed"}}`))
		default:
			t.Fatalf("unexpected /responses request count %d", call)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected original 413, got %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if string(body) != original413Body {
		t.Fatalf("expected original 413 body %s, got %s", original413Body, body)
	}

	mu.Lock()
	gotInputCounts := append([]int(nil), inputCounts...)
	mu.Unlock()
	if len(gotInputCounts) != 4 {
		t.Fatalf("expected 4 upstream requests (initial + 2 chunks + failed merge), got %d", len(gotInputCounts))
	}
	if gotInputCounts[0] != 2 || gotInputCounts[1] != 1 || gotInputCounts[2] != 1 || gotInputCounts[3] != 2 {
		t.Fatalf("unexpected upstream input counts: %v", gotInputCounts)
	}
}

func TestHandleCompact_UsesPartialSummariesWhenChunkedMerge413s(t *testing.T) {
	largeText := strings.Repeat("a", compactUpstreamChunkBodySize*5/8)
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "first " + largeText},
				},
			},
			map[string]interface{}{
				"type": "message",
				"role": "assistant",
				"content": []map[string]string{
					{"type": "input_text", "text": "second " + largeText},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	var calls atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}
		input, ok := req["input"].([]interface{})
		if !ok {
			t.Fatalf("expected input array, got %#v", req["input"])
		}

		switch call := calls.Add(1); call {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request"}}`))
		case 2:
			if len(input) != 1 {
				t.Fatalf("expected first fallback chunk to have 1 item, got %d", len(input))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-chunk-1","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary of first item"}]}]}`))
		case 3:
			if len(input) != 1 {
				t.Fatalf("expected second fallback chunk to have 1 item, got %d", len(input))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-chunk-2","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary of second item"}]}]}`))
		case 4:
			if len(input) != 2 {
				t.Fatalf("expected merge request to contain 2 chunk summaries, got %d", len(input))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request"}}`))
		default:
			t.Fatalf("unexpected /responses request count %d", call)
		}
	})
	// Allow the initial request, two successful chunk requests, and the first
	// merge attempt. If that merge 413s, the proxy should synthesize a local
	// merged summary from the already-successful chunks instead of retrying until
	// it ultimately replays the initial 413 to the client.
	handler.compactMaxAttempts = 4

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 with partial-summary fallback, got %d: %s", resp.StatusCode, body)
	}

	body, _ := io.ReadAll(resp.Body)
	summary, encryptedSummary := requireCompactResponseSummaryForTest(t, body)
	for _, want := range []string{"summary of first item", "summary of second item"} {
		if !strings.Contains(summary, want) {
			t.Fatalf("expected summary to contain %q, got %q", want, summary)
		}
		if !strings.Contains(encryptedSummary, want) {
			t.Fatalf("expected encrypted summary to contain %q, got %q", want, encryptedSummary)
		}
	}
	if calls.Load() != 4 {
		t.Fatalf("expected initial request, chunks, and one merge attempt; got %d calls", calls.Load())
	}
}

func TestHandleCompact_CapsOriginal413BodyWhenChunkedMergeFails(t *testing.T) {
	largeText := strings.Repeat("a", compactUpstreamChunkBodySize*5/8)
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "first " + largeText},
				},
			},
			map[string]interface{}{
				"type": "message",
				"role": "assistant",
				"content": []map[string]string{
					{"type": "input_text", "text": "second " + largeText},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	oversized413Body := strings.Repeat("x", compactUpstreamErrorBodySize+1024)
	var calls atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream body: %v", err)
		}
		var req map[string]interface{}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}
		input, ok := req["input"].([]interface{})
		if !ok {
			t.Fatalf("expected input array, got %#v", req["input"])
		}

		switch call := calls.Add(1); call {
		case 1:
			w.Header().Set("Content-Type", "text/plain")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(oversized413Body))
		case 2, 3:
			if len(input) != 1 {
				t.Fatalf("expected fallback chunk to have 1 item, got %d", len(input))
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-chunk","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"chunk summary"}]}]}`))
		case 4:
			if len(input) != 2 {
				t.Fatalf("expected merge request to contain 2 chunk summaries, got %d", len(input))
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":{"message":"merge failed"}}`))
		default:
			t.Fatalf("unexpected /responses request count %d", call)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleCompact(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected capped original 413, got %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if len(body) != compactUpstreamErrorBodySize {
		t.Fatalf("expected capped 413 body length %d, got %d", compactUpstreamErrorBodySize, len(body))
	}
	if strings.Trim(string(body), "x") != "" {
		t.Fatalf("expected capped body to preserve the upstream prefix, got %q", body[:min(len(body), 32)])
	}
	if calls.Load() != 4 {
		t.Fatalf("expected initial request, 2 chunk requests, and failed merge; got %d calls", calls.Load())
	}
}

func TestSplitOversizedCompactInputItemsByBodySize(t *testing.T) {
	modelRaw, _ := json.Marshal("gpt-5.4")
	inputRaw := json.RawMessage(`[]`)
	requestFields := map[string]json.RawMessage{
		"model": modelRaw,
		"input": inputRaw,
	}
	oversizedItem, err := json.Marshal(map[string]interface{}{
		"type": "message",
		"role": "user",
		"content": []map[string]string{
			{"type": "input_text", "text": strings.Repeat("oversized ", 2048)},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal oversized item: %v", err)
	}

	maxBodySize := 4096
	singleBody, err := marshalCompactResponsesRequest(requestFields, []json.RawMessage{oversizedItem})
	if err != nil {
		t.Fatalf("failed to marshal single item body: %v", err)
	}
	if len(singleBody) <= maxBodySize {
		t.Fatalf("expected single item body to exceed test max, got %d", len(singleBody))
	}

	splitItems, split, err := splitOversizedCompactInputItemsByBodySize(requestFields, []json.RawMessage{oversizedItem}, maxBodySize)
	if err != nil {
		t.Fatalf("split oversized item failed: %v", err)
	}
	if !split {
		t.Fatal("expected oversized item to be split")
	}
	if len(splitItems) < 2 {
		t.Fatalf("expected at least two split items, got %d", len(splitItems))
	}
	for i, item := range splitItems {
		body, err := marshalCompactResponsesRequest(requestFields, []json.RawMessage{item})
		if err != nil {
			t.Fatalf("failed to marshal split item %d: %v", i, err)
		}
		if len(body) > maxBodySize {
			t.Fatalf("split item %d exceeded max body size: got %d, max %d", i, len(body), maxBodySize)
		}
		var decoded map[string]interface{}
		if err := json.Unmarshal(item, &decoded); err != nil {
			t.Fatalf("split item %d is invalid JSON: %v", i, err)
		}
		text := requireMessageTextWithRole(t, decoded, "user")
		if !strings.Contains(text, "Oversized compact input item chunk") {
			t.Fatalf("split item %d missing oversized context marker: %.200q", i, text)
		}
	}
}

func TestSplitCompactInputByBodySizeUsesEncodedItemSizes(t *testing.T) {
	requestFields := map[string]json.RawMessage{
		"model": json.RawMessage(`"gpt-5.4"`),
		"input": json.RawMessage(`[]`),
	}
	input := []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"<>& first"}]}`),
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<>& second"}]}`),
		json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"<>& third"}]}`),
	}

	twoItemBody, err := marshalCompactResponsesRequest(requestFields, input[:2])
	if err != nil {
		t.Fatalf("failed to marshal two-item body: %v", err)
	}
	threeItemBody, err := marshalCompactResponsesRequest(requestFields, input)
	if err != nil {
		t.Fatalf("failed to marshal three-item body: %v", err)
	}
	if len(threeItemBody) <= len(twoItemBody) {
		t.Fatalf("expected three-item body to exceed two-item body: %d <= %d", len(threeItemBody), len(twoItemBody))
	}

	chunks, err := splitCompactInputByBodySize(requestFields, input, len(twoItemBody))
	if err != nil {
		t.Fatalf("split failed: %v", err)
	}
	gotSizes := make([]int, 0, len(chunks))
	for _, chunk := range chunks {
		gotSizes = append(gotSizes, len(chunk))
	}
	if len(gotSizes) != 2 || gotSizes[0] != 2 || gotSizes[1] != 1 {
		t.Fatalf("expected chunks sized [2,1], got %#v", gotSizes)
	}
	for i, chunk := range chunks {
		body, err := marshalCompactResponsesRequest(requestFields, chunk)
		if err != nil {
			t.Fatalf("failed to marshal chunk %d: %v", i, err)
		}
		if len(body) > len(twoItemBody) {
			t.Fatalf("chunk %d body exceeded max: got %d, max %d", i, len(body), len(twoItemBody))
		}
	}
}

func TestHandleCompact_HalvesChunkTargetOnRecursive413(t *testing.T) {
	// Three roughly half-target items: with the initial default target each
	// chunk holds 1 item (3 chunks total). Upstream rejects them at the default
	// target but accepts them at <=half target, forcing the recursive halving
	// path to engage.
	itemText := strings.Repeat("a", compactUpstreamChunkBodySize/2-2048)
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message", "role": "user",
				"content": []map[string]string{{"type": "input_text", "text": "first " + itemText}},
			},
			map[string]interface{}{
				"type": "message", "role": "assistant",
				"content": []map[string]string{{"type": "input_text", "text": "second " + itemText}},
			},
			map[string]interface{}{
				"type": "message", "role": "user",
				"content": []map[string]string{{"type": "input_text", "text": "third " + itemText}},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	const halvedCap = compactUpstreamChunkBodySize / 2
	var mu sync.Mutex
	var bodySizes []int
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		mu.Lock()
		bodySizes = append(bodySizes, len(body))
		mu.Unlock()

		// Reject anything above the halved cap with 413; succeed otherwise.
		if len(body) > halvedCap {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"chunk summary"}]}]}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleCompact(w, req)
	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 from adaptive halving, got %d: %s", resp.StatusCode, body)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodySizes) < 4 {
		t.Fatalf("expected at least one initial 413, several halved retries, and a merge; got %d calls (%v)", len(bodySizes), bodySizes)
	}
	if bodySizes[0] <= halvedCap {
		t.Fatalf("expected initial body to exceed halved cap; sizes=%v", bodySizes)
	}
	rejectsAfterFirst := 0
	for _, size := range bodySizes[1:] {
		if size > halvedCap {
			rejectsAfterFirst++
		}
	}
	if rejectsAfterFirst == 0 {
		t.Fatalf("expected halving recursion to attempt sizes above the halved cap before shrinking; sizes=%v", bodySizes)
	}
}

func TestCompactResponsesRequestDepth_AllowsConfiguredTargetToReachFloor(t *testing.T) {
	// A large operator-configured initial target can require more than the old
	// fixed depth guard before the fallback reaches the documented 64 KiB floor.
	// Depth by itself must not stop a floor-sized logical compaction call; the
	// floor and shared attempt budget are the terminating guards.
	message, err := compactTextInputRawMessage("small floor-sized retry")
	if err != nil {
		t.Fatalf("build input message: %v", err)
	}
	input, err := json.Marshal([]json.RawMessage{message})
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	requestFields := map[string]json.RawMessage{
		"model": json.RawMessage(`"gpt-5.4"`),
		"input": input,
	}

	var calls int
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		calls++
		if len(body) > compactUpstreamChunkBodyFloor {
			t.Fatalf("test setup expected a floor-sized body, got %d > %d", len(body), compactUpstreamChunkBodyFloor)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"floor ok"}]}]}`))
	})

	summary, resp, err := handler.compactResponsesRequestDepth(context.Background(), requestFields, nil, 9, compactUpstreamChunkBodyFloor, newCompactBudget(2), false)
	if err != nil {
		t.Fatalf("expected high-depth floor retry to proceed: %v", err)
	}
	if resp != nil {
		t.Fatalf("expected successful summary, got response status %d", resp.StatusCode)
	}
	if summary != "floor ok" {
		t.Fatalf("unexpected summary %q", summary)
	}
	if calls != 1 {
		t.Fatalf("expected one upstream call, got %d", calls)
	}
}

func TestHandleCompact_HalvesEagerlyWhenSubTargetRequest413s(t *testing.T) {
	// Single small item: the initial compact body fits inside the configured
	// target, but upstream still 413s because its real cap is lower (300 KiB
	// here). The fix is for the depth=0 path to halve the target instead of
	// bailing on a single chunk; we expect at least one upstream retry below the
	// original body size, ending in 200.
	itemText := strings.Repeat("a", 400*1024) // ~400 KiB content; total body ~410 KiB < default target
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message", "role": "user",
				"content": []map[string]string{{"type": "input_text", "text": itemText}},
			},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	const upstreamCap = 300 * 1024
	var mu sync.Mutex
	var bodySizes []int
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		mu.Lock()
		bodySizes = append(bodySizes, len(body))
		mu.Unlock()

		if len(body) > upstreamCap {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleCompact(w, req)
	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 from sub-target halving, got %d: %s", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodySizes) < 2 {
		t.Fatalf("expected at least one initial 413 plus a halved retry; got %d calls (%v)", len(bodySizes), bodySizes)
	}
	if bodySizes[0] > compactUpstreamChunkBodySize {
		t.Fatalf("expected initial body to be sub-target; sizes=%v", bodySizes)
	}
}

func TestHandleCompact_BoundsUpstreamFanoutByMaxAttempts(t *testing.T) {
	// Multiple half-target items + tiny upstream cap = unbounded fanout
	// without the budget. Set a deliberately low max-attempts and verify the
	// proxy bails instead of fanning out further. The original 413 should be
	// surfaced because the chunked merge ultimately failed.
	itemText := strings.Repeat("a", compactUpstreamChunkBodySize/2-2048)
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "a " + itemText}}},
			map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "b " + itemText}}},
			map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "c " + itemText}}},
			map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "d " + itemText}}},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	const maxAttempts = 3
	var mu sync.Mutex
	var calls int
	backend := func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
	}
	server := httptest.NewServer(http.HandlerFunc(backend))
	t.Cleanup(server.Close)
	handler := &ProxyHandler{
		auth:               auth.NewTestAuthenticator("test-token"),
		client:             server.Client(),
		copilotURL:         server.URL,
		log:                logger.New(logger.LevelInfo),
		retryBaseDelay:     1 * time.Millisecond,
		compactMaxAttempts: maxAttempts,
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleCompact(w, req)
	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected the original 413 after budget exhaustion, got %d: %s", resp.StatusCode, body)
	}
	mu.Lock()
	defer mu.Unlock()
	if calls > maxAttempts {
		t.Fatalf("expected upstream calls to be bounded by max-attempts (%d); saw %d", maxAttempts, calls)
	}
}

func TestHandleCompact_PropagatesLearnedTargetAcrossSiblings(t *testing.T) {
	// Build many small siblings against an upstream cap that's ~half the
	// configured test target. Without learned-target propagation, each sibling
	// would burn one discovery 413 at the larger target before recursing,
	// quickly exhausting the attempt budget. With propagation, only the FIRST
	// sibling pays the discovery cost; the rest plan at the learned smaller
	// size from the start.
	const (
		siblings           = 10
		configuredTarget   = 1 << 20              // 1 MiB
		upstreamCap        = configuredTarget / 2 // 512 KiB
		itemBodyTargetSize = (configuredTarget * 6) / 10
	)
	itemText := strings.Repeat("a", itemBodyTargetSize-2048)
	inputs := make([]interface{}, 0, siblings)
	for i := 0; i < siblings; i++ {
		inputs = append(inputs, map[string]interface{}{
			"type": "message", "role": "user",
			"content": []map[string]string{{"type": "input_text", "text": fmt.Sprintf("item-%d %s", i, itemText)}},
		})
	}
	reqBody, err := json.Marshal(map[string]interface{}{"model": "gpt-5.4", "input": inputs})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	var mu sync.Mutex
	var bodySizes []int
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		mu.Lock()
		bodySizes = append(bodySizes, len(body))
		mu.Unlock()

		if len(body) > upstreamCap {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	})
	handler.compactChunkBodyBytes = configuredTarget

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleCompact(w, req)
	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 with learned-target propagation, got %d: %s", resp.StatusCode, body)
	}

	mu.Lock()
	defer mu.Unlock()
	rejects := 0
	for _, size := range bodySizes {
		if size > upstreamCap {
			rejects++
		}
	}
	// One initial 413 at full size + at most one chunk-level discovery 413
	// before the learned target propagates. Without the fix, every sibling
	// burns its own discovery 413 (>= siblings rejects).
	if rejects > 3 {
		t.Fatalf("expected at most 3 over-cap upstream POSTs once the learned target propagates; saw %d (sizes=%v)", rejects, bodySizes)
	}
}

func TestHandleCompact_LargeMultiMiBSucceedsUnderDefaults(t *testing.T) {
	// Codex-style large session: ~24 MiB of input that should chunk happily
	// under the default attempt budget and inbound request limit.
	const items = 8
	itemText := strings.Repeat("x", (compactUpstreamChunkBodySize*3)/4) // ~3 MiB content
	inputs := make([]interface{}, 0, items)
	for i := 0; i < items; i++ {
		inputs = append(inputs, map[string]interface{}{
			"type": "message", "role": "user",
			"content": []map[string]string{{"type": "input_text", "text": fmt.Sprintf("item-%d %s", i, itemText)}},
		})
	}
	reqBody, err := json.Marshal(map[string]interface{}{"model": "gpt-5.4", "input": inputs})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	const upstreamCap = compactUpstreamChunkBodySize // upstream accepts up to the default 4 MiB target
	var mu sync.Mutex
	var calls int
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		mu.Lock()
		calls++
		mu.Unlock()
		if len(body) > upstreamCap {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleCompact(w, req)
	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200 for large multi-MiB compact under defaults; got %d: %s", resp.StatusCode, body)
	}
	if calls > compactUpstreamMaxAttempts {
		t.Fatalf("expected total upstream calls <= %d (default budget); got %d", compactUpstreamMaxAttempts, calls)
	}
}

func TestHandleCompact_EmptyInputAfter413SurfacesOriginal413(t *testing.T) {
	// Pathological client request: input:[] arrives, upstream 413s anyway
	// (e.g. a tools/text/instructions field is huge so the request is over
	// cap even with no input items). Without explicit handling, the chunker
	// would skip the empty fanout and return 200 with an empty compaction
	// summary; the client would then think it has a valid checkpoint when
	// no upstream success ever occurred.
	hugeText := strings.Repeat("a", compactUpstreamChunkBodySize+(8*1024))
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{},
		"text":  map[string]interface{}{"format": map[string]interface{}{"type": "json_schema", "name": "huge", "schema": map[string]interface{}{"description": hugeText}}},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	const original413Body = `{"error":{"message":"original payload too large"}}`
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(original413Body))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleCompact(w, req)
	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected original 413 for empty input + huge stripped fields; got %d: %s", resp.StatusCode, body)
	}
}

func TestHandleCompact_EmptyInputNoStripFieldsAfter413SurfacesOriginal413(t *testing.T) {
	// Even more degenerate: input:[] with no oversized fixed fields. The
	// fallback splitter returns zero chunks and there's nothing to strip, so
	// historically the loop would no-op and the merge would emit an empty
	// summary as a 200. The new len(chunks)==0 guard refuses to fabricate
	// success and surfaces the upstream 413 verbatim.
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	const original413Body = `{"error":{"message":"upstream rejected empty compact"}}`
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(original413Body))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleCompact(w, req)
	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected original 413 for empty input with no stripped fields; got %d: %s", resp.StatusCode, body)
	}
}

func TestHandleCompact_MemoizesModelFallbackAcrossSiblings(t *testing.T) {
	// Configure a request whose model is rejected as unsupported by upstream.
	// Without memoization, the model-fallback probe would fire on every chunk
	// (each logical compaction call doubles to 2 real POSTs). With B applied,
	// the probe fires once on the first chunk and the resolved fallback model
	// is memoized on the budget, so siblings 2..N skip the probe entirely.
	const siblings = 5
	itemText := strings.Repeat("a", 32*1024) // ~32 KiB per item; well under upstream cap
	inputs := make([]interface{}, 0, siblings)
	for i := 0; i < siblings; i++ {
		inputs = append(inputs, map[string]interface{}{
			"type": "message", "role": "user",
			"content": []map[string]string{{"type": "input_text", "text": fmt.Sprintf("item-%d %s", i, itemText)}},
		})
	}
	reqBody, err := json.Marshal(map[string]interface{}{"model": "gpt-4o", "input": inputs})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	const upstreamCap = 128 * 1024 // forces chunk fanout while allowing historical-text chunks
	var mu sync.Mutex
	var requestedModels []string
	var fallbackProbes int
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		// /models probes are part of the fallback lookup; track them but don't
		// route through the compact body sniffing below.
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"gpt-5.4","object":"model","capabilities":{"supports":{"streaming":true,"tool_calls":true}}}]}`))
			return
		}

		// Trip the small upstream cap so we engage the chunk fanout first.
		if len(body) > upstreamCap {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
			return
		}

		model := extractRequestModel(body)
		mu.Lock()
		requestedModels = append(requestedModels, model)
		mu.Unlock()

		if model == "gpt-4o" {
			mu.Lock()
			fallbackProbes++
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"model not supported","param":"model","code":"model_not_supported"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	})
	handler.compactChunkBodyBytes = upstreamCap

	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleCompact(w, req)
	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200; got %d: %s", resp.StatusCode, body)
	}

	mu.Lock()
	defer mu.Unlock()
	if fallbackProbes != 1 {
		t.Fatalf("expected the unsupported-model probe to fire exactly once across %d-sibling fanout (memoization); got %d (models=%v)", siblings, fallbackProbes, requestedModels)
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
	gotText, gotCompaction := requireCompactResponseSummaryForTest(t, body)
	if strings.Contains(gotText, "") || strings.Contains(gotText, "") {
		t.Fatalf("expected compaction text to be sanitized, got %q", gotText)
	}
	if gotText != "Keep the passthrough tests." {
		t.Errorf("summary text = %q, want %q", gotText, "Keep the passthrough tests.")
	}
	if gotCompaction != "Keep the passthrough tests." {
		t.Errorf("encoded compaction summary = %q, want %q", gotCompaction, "Keep the passthrough tests.")
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
	_, gotCompaction := requireCompactResponseSummaryForTest(t, body)
	if gotCompaction != "custom summary" {
		t.Errorf("expected encoded custom summary, got %q", gotCompaction)
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
	_, gotCompaction := requireCompactResponseSummaryForTest(t, body)
	if gotCompaction != "fallback summary" {
		t.Fatalf("expected encoded fallback summary, got %q", gotCompaction)
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
			t.Errorf("expected legacy plaintext summary to be rewritten, got %q", contextText)
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

func TestHandleResponses_RetriesWithoutUnverifiableEncryptedContent(t *testing.T) {
	const encryptedToken = "gAAAAABencryptedReasoningPayloadpQ=="
	var upstreamRequestsMu sync.Mutex
	upstreamRequests := make([]map[string]interface{}, 0, 2)
	var attempts atomic.Int32

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("failed to read upstream request body: %v", err)
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}

		upstreamRequestsMu.Lock()
		upstreamRequests = append(upstreamRequests, body)
		upstreamRequestsMu.Unlock()

		switch attempts.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"The encrypted content %s could not be verified. Reason: Encrypted content could not be decrypted or parsed.","code":"invalid_request_body"}}`, encryptedToken)
		case 2:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-sanitized-encrypted-content","object":"response","status":"completed","output":[]}`))
		default:
			t.Fatalf("unexpected upstream attempt %d", attempts.Load())
		}
	})

	reqBody, err := json.Marshal(map[string]interface{}{
		"model":                "gpt-5.4",
		"previous_response_id": "resp-prev",
		"input": []interface{}{
			map[string]interface{}{
				"type":              "reasoning",
				"id":                "rs-prev",
				"encrypted_content": encryptedToken,
			},
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": encryptedToken,
			},
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "continue without opaque state"},
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
		t.Fatalf("expected retry to recover with 200, got %d: %s", resp.StatusCode, body)
	}

	upstreamRequestsMu.Lock()
	requests := append([]map[string]interface{}(nil), upstreamRequests...)
	upstreamRequestsMu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("expected initial request and sanitized retry, got %d", len(requests))
	}

	initialInput := upstreamInputItems(t, requests[0])
	if len(initialInput) != 3 {
		t.Fatalf("expected initial replay to include opaque items, got %d items", len(initialInput))
	}
	if initialInput[0]["type"] != "reasoning" || initialInput[0]["encrypted_content"] != encryptedToken {
		t.Fatalf("expected initial request to include encrypted reasoning item, got %#v", initialInput[0])
	}
	if initialInput[1]["type"] != "compaction" || initialInput[1]["encrypted_content"] != encryptedToken {
		t.Fatalf("expected initial request to include encrypted compaction item, got %#v", initialInput[1])
	}

	retryInput := upstreamInputItems(t, requests[1])
	if len(retryInput) != 1 {
		t.Fatalf("expected retry to drop unverifiable opaque items and keep user input, got %d: %#v", len(retryInput), retryInput)
	}
	if got := requireMessageTextWithRole(t, retryInput[0], "user"); got != "continue without opaque state" {
		t.Fatalf("expected retry to preserve user message, got %q", got)
	}
	if got := requests[1]["previous_response_id"]; got != "resp-prev" {
		t.Fatalf("expected retry to preserve previous_response_id, got %#v", got)
	}
}

func TestHandleResponses_PreservesLargeNonEncryptedContentBadRequestBody(t *testing.T) {
	const suffix = "--large-non-matching-error-end"
	largeError := strings.Repeat("x", compactUpstreamErrorBodySize+1024) + suffix
	var attempts atomic.Int32

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(largeError))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected upstream 400 passthrough, got %d: %s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got != largeError {
		t.Fatalf("expected non-matching 400 body to be preserved in full, got len=%d want len=%d suffix=%v", len(got), len(largeError), strings.HasSuffix(got, suffix))
	}
	if got := attempts.Load(); got != 1 {
		t.Fatalf("expected no retry for non-matching 400, got %d upstream attempts", got)
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

func TestHandleResponses_RetriesCompacted413ReplayWithoutPreviousResponseID(t *testing.T) {
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
			_, _ = w.Write([]byte(`{"id":"comp-413","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint summary after 413 without previous id"}]}]}`))
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
	if _, ok := requests[2]["previous_response_id"]; ok {
		t.Fatalf("retried request should not invent previous_response_id, got %v", requests[2]["previous_response_id"])
	}

	compactionInput := upstreamInputItems(t, requests[1])
	if len(compactionInput) != 3 {
		t.Fatalf("expected compaction request to summarize only the replay prefix, got %d items", len(compactionInput))
	}
	if got := requireMessageTextWithRole(t, compactionInput[0], "user"); got != "first turn" {
		t.Fatalf("expected compaction request to preserve oldest user turn, got %q", got)
	}
	if got := requireMessageTextWithRole(t, compactionInput[2], "user"); got != "second turn" {
		t.Fatalf("expected compaction request to stop before kept tail, got %q", got)
	}

	retriedInput := upstreamInputItems(t, requests[2])
	if len(retriedInput) != 3 {
		t.Fatalf("expected retried request to include compacted checkpoint plus tail, got %d", len(retriedInput))
	}
	if got := requireCompactionContextMessage(t, retriedInput[0]); !strings.Contains(got, "checkpoint summary after 413 without previous id") {
		t.Fatalf("expected retried request to start with compacted checkpoint, got %q", got)
	}
	if got := requireMessageTextWithRole(t, retriedInput[1], "assistant"); got != "second answer" {
		t.Fatalf("expected retried request to keep assistant tail item, got %q", got)
	}
	if got := requireMessageTextWithRole(t, retriedInput[2], "user"); got != "latest turn" {
		t.Fatalf("expected retried request to keep latest user tail item, got %q", got)
	}
}

func TestHandleResponses_Skips413CompactionForPureUserOnlyInput(t *testing.T) {
	var upstreamRequests atomic.Int32

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
	})

	reqBody, err := json.Marshal(map[string]interface{}{
		"model":                "gpt-5.4",
		"previous_response_id": "resp-prev-current",
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "Here is a huge current spec"}}},
			map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "Implement it exactly"}}},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Codex-Turn-Metadata", `{"turn_id":"turn-first","turn_started_at_unix_ms":1760000000000}`)
	req.Header.Set("X-Codex-Window-Id", "thread-first:0")
	req.Header.Set("X-Codex-Parent-Thread-Id", "parent-first")
	req.Header.Set("X-Codex-Turn-State", "state-from-prior-request")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 413, got %d: %s", resp.StatusCode, body)
	}
	if upstreamRequests.Load() != 1 {
		t.Fatalf("expected pure user-only input to be sent upstream once without compaction retry, got %d requests", upstreamRequests.Load())
	}

	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("payload_too_large")) {
		t.Fatalf("expected original 413 body to be preserved, got %s", body)
	}
}

func TestHandleResponses_ReducesKeepTailWhenCompactedReplayStill413s(t *testing.T) {
	var upstreamRequestsMu sync.Mutex
	upstreamRequests := make([]map[string]interface{}, 0, 5)
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
			input := upstreamInputItems(t, body)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprintf(w, `{"id":"comp-dynamic-tail","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary for %d items"}]}]}`, len(input))
			return
		}

		switch normalRequests.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"failed to parse request","code":"payload_too_large"}}`))
		case 2:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = w.Write([]byte(`{"error":{"message":"compacted replay still too large","code":"payload_too_large"}}`))
		case 3:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-dynamic-tail-retried","object":"response","status":"completed"}`))
		default:
			t.Fatalf("unexpected normal upstream request count %d", normalRequests.Load())
		}
	})

	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "first turn"}}},
			map[string]interface{}{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "input_text", "text": "first answer"}}},
			map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "second turn"}}},
			map[string]interface{}{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "input_text", "text": "second answer"}}},
			map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "latest turn"}}},
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
	if result["id"] != "resp-dynamic-tail-retried" {
		t.Fatalf("expected retried response id resp-dynamic-tail-retried, got %v", result["id"])
	}

	upstreamRequestsMu.Lock()
	requests := append([]map[string]interface{}(nil), upstreamRequests...)
	upstreamRequestsMu.Unlock()

	if len(requests) != 5 {
		t.Fatalf("expected 5 upstream requests (413 + compact + 413 + compact + retry), got %d", len(requests))
	}

	firstCompactionInput := upstreamInputItems(t, requests[1])
	if len(firstCompactionInput) != 1 {
		t.Fatalf("expected first compaction to summarize one item after shrinking default keep-tail 12 to 4, got %d", len(firstCompactionInput))
	}
	firstRetriedInput := upstreamInputItems(t, requests[2])
	if len(firstRetriedInput) != 5 {
		t.Fatalf("expected first retry to keep checkpoint plus 4 tail items, got %d", len(firstRetriedInput))
	}
	if got := requireCompactionContextMessage(t, firstRetriedInput[0]); !strings.Contains(got, "summary for 1 items") {
		t.Fatalf("expected first retry checkpoint for 1 item, got %q", got)
	}

	secondCompactionInput := upstreamInputItems(t, requests[3])
	if len(secondCompactionInput) != 3 {
		t.Fatalf("expected second compaction to reduce keep-tail to 2 and summarize three items, got %d", len(secondCompactionInput))
	}
	secondRetriedInput := upstreamInputItems(t, requests[4])
	if len(secondRetriedInput) != 3 {
		t.Fatalf("expected second retry to keep checkpoint plus 2 tail items, got %d", len(secondRetriedInput))
	}
	if got := requireCompactionContextMessage(t, secondRetriedInput[0]); !strings.Contains(got, "summary for 3 items") {
		t.Fatalf("expected second retry checkpoint for 3 items, got %q", got)
	}
	if got := requireMessageTextWithRole(t, secondRetriedInput[1], "assistant"); got != "second answer" {
		t.Fatalf("expected reduced retry to keep assistant tail item, got %q", got)
	}
	if got := requireMessageTextWithRole(t, secondRetriedInput[2], "user"); got != "latest turn" {
		t.Fatalf("expected reduced retry to keep latest user tail item, got %q", got)
	}
}

func TestIsLikelyResponsesReplay(t *testing.T) {
	raw := func(v interface{}) json.RawMessage {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("failed to marshal raw message: %v", err)
		}
		return b
	}

	checkpoint, err := proxyCompactionContextRawMessage("prior checkpoint")
	if err != nil {
		t.Fatalf("failed to build compaction context: %v", err)
	}

	tests := []struct {
		name  string
		input []json.RawMessage
		want  bool
	}{
		{
			name: "assistant message marks replay",
			input: []json.RawMessage{
				raw(map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "first"}}}),
				raw(map[string]interface{}{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "output_text", "text": "answer"}}}),
			},
			want: true,
		},
		{
			name: "tool output marks replay",
			input: []json.RawMessage{
				raw(map[string]interface{}{"type": "function_call_output", "call_id": "call-1", "output": "done"}),
			},
			want: true,
		},
		{
			name: "proxy compaction checkpoint marks replay",
			input: []json.RawMessage{
				checkpoint,
			},
			want: true,
		},
		{
			name: "pure user-only input is not replay-like",
			input: []json.RawMessage{
				raw(map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "current spec"}}}),
				raw(map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "current ask"}}}),
			},
			want: false,
		},
		{
			name: "developer instruction alone is not replay-like",
			input: []json.RawMessage{
				raw(map[string]interface{}{"type": "message", "role": "developer", "content": []map[string]string{{"type": "input_text", "text": "current instruction"}}}),
				raw(map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "current ask"}}}),
			},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isLikelyResponsesReplay(tt.input); got != tt.want {
				t.Fatalf("isLikelyResponsesReplay() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestCompactedResponsesAlignedPrefixLen(t *testing.T) {
	raw := func(v interface{}) json.RawMessage {
		t.Helper()
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("failed to marshal raw message: %v", err)
		}
		return b
	}
	msg := func(role, text string) json.RawMessage {
		return raw(map[string]interface{}{"type": "message", "role": role, "content": []map[string]string{{"type": "input_text", "text": text}}})
	}
	call := raw(map[string]interface{}{"type": "function_call", "call_id": "call-1", "name": "shell", "arguments": "{}"})
	out := raw(map[string]interface{}{"type": "function_call_output", "call_id": "call-1", "output": "done"})

	tests := []struct {
		name     string
		input    []json.RawMessage
		keepTail int
		want     int
	}{
		{
			name:     "normal message boundary",
			input:    []json.RawMessage{msg("user", "one"), msg("assistant", "two"), msg("user", "three")},
			keepTail: 1,
			want:     2,
		},
		{
			name: "normal message id is not treated as tool call",
			input: []json.RawMessage{
				raw(map[string]interface{}{"type": "message", "role": "assistant", "id": "msg-1", "content": []map[string]string{{"type": "output_text", "text": "two"}}}),
				msg("user", "three"),
			},
			keepTail: 1,
			want:     1,
		},
		{
			name:     "tail starting on output includes call",
			input:    []json.RawMessage{msg("user", "one"), call, out, msg("user", "latest")},
			keepTail: 2,
			want:     1,
		},
		{
			name:     "tail after call includes call",
			input:    []json.RawMessage{msg("user", "one"), msg("assistant", "two"), call, msg("user", "latest")},
			keepTail: 1,
			want:     2,
		},
		{
			name:     "tool output chain walks backward",
			input:    []json.RawMessage{msg("user", "one"), call, out, raw(map[string]interface{}{"type": "mcp_approval_response", "call_id": "call-1"}), msg("user", "latest")},
			keepTail: 3,
			want:     1,
		},
		{
			name: "non adjacent output inside retained tail includes matching call",
			input: []json.RawMessage{
				msg("user", "one"),
				call,
				msg("assistant", "thinking one"),
				msg("assistant", "thinking two"),
				out,
				msg("user", "latest"),
			},
			keepTail: 3,
			want:     1,
		},
		{
			name: "non adjacent MCP approval response preserves approval request",
			input: []json.RawMessage{
				msg("user", "one"),
				raw(map[string]interface{}{
					"type":         "mcp_approval_request",
					"id":           "approval-1",
					"server_label": "github",
					"name":         "create_pull_request",
				}),
				msg("assistant", "thinking"),
				raw(map[string]interface{}{
					"type":                "mcp_approval_response",
					"approval_request_id": "approval-1",
					"approve":             true,
				}),
				msg("user", "latest"),
			},
			keepTail: 3,
			want:     1,
		},
		{
			name: "parallel retained outputs preserve all matching calls",
			input: []json.RawMessage{
				msg("user", "one"),
				raw(map[string]interface{}{"type": "function_call", "call_id": "call-1", "name": "first", "arguments": "{}"}),
				raw(map[string]interface{}{"type": "function_call", "call_id": "call-2", "name": "second", "arguments": "{}"}),
				msg("assistant", "thinking"),
				raw(map[string]interface{}{"type": "function_call_output", "call_id": "call-1", "output": "first done"}),
				msg("assistant", "between"),
				raw(map[string]interface{}{"type": "function_call_output", "call_id": "call-2", "output": "second done"}),
				msg("user", "latest"),
			},
			keepTail: 4,
			want:     1,
		},
		{
			name: "pending function call remains available for future output",
			input: []json.RawMessage{
				msg("user", "one"),
				call,
				msg("assistant", "thinking one"),
				msg("assistant", "thinking two"),
			},
			keepTail: 1,
			want:     1,
		},
		{
			name: "chat-style tool output preserves assistant tool call",
			input: []json.RawMessage{
				msg("user", "one"),
				raw(map[string]interface{}{
					"type":       "message",
					"role":       "assistant",
					"tool_calls": []map[string]interface{}{{"id": "tool-1", "type": "function", "function": map[string]string{"name": "shell", "arguments": "{}"}}},
				}),
				msg("assistant", "thinking one"),
				msg("assistant", "thinking two"),
				raw(map[string]interface{}{"type": "message", "role": "tool", "tool_call_id": "tool-1", "content": "done"}),
				msg("user", "latest"),
			},
			keepTail: 3,
			want:     1,
		},
		{
			name:     "single item preserves latest",
			input:    []json.RawMessage{msg("user", "latest")},
			keepTail: 12,
			want:     0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := compactedResponsesAlignedPrefixLen(tt.input, tt.keepTail); got != tt.want {
				t.Fatalf("expected prefix len %d, got %d", tt.want, got)
			}
		})
	}
}

func TestCompactedResponsesRetryKeepTailSchedule(t *testing.T) {
	tests := []struct {
		name               string
		inputItems         int
		configuredKeepTail int
		want               []int
	}{
		{name: "default tail larger than short replay", inputItems: 5, configuredKeepTail: 12, want: []int{4, 2, 1}},
		{name: "configured tail halves", inputItems: 50, configuredKeepTail: 12, want: []int{12, 6, 3, 1}},
		{name: "one item cannot preserve latest and compact prefix", inputItems: 1, configuredKeepTail: 12, want: nil},
		{name: "disabled", inputItems: 5, configuredKeepTail: 0, want: nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compactedResponsesRetryKeepTailSchedule(tt.inputItems, tt.configuredKeepTail)
			if len(got) != len(tt.want) {
				t.Fatalf("expected schedule %v, got %v", tt.want, got)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("expected schedule %v, got %v", tt.want, got)
				}
			}
		})
	}
}

func TestHandleResponses_ReturnsOriginal413WhenReducedTailCompactionFails(t *testing.T) {
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
	if upstreamRequests.Load() <= 1 {
		t.Fatalf("expected reduced-tail compaction attempts after initial 413, got %d upstream requests", upstreamRequests.Load())
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

func requireCompactResponseSummaryForTest(t *testing.T, body []byte) (string, string) {
	t.Helper()
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
	for _, item := range result.Output {
		if item.Type != "compaction" {
			continue
		}
		decoded := decodeCompactionSummaryForTest(t, item.EncryptedContent)
		return decoded, decoded
	}
	t.Fatalf("expected compact response compaction item, got %+v", result.Output)
	return "", ""
}

func compactChunkTestRequestFields(t *testing.T, targetBodySize int, texts []string) (map[string]json.RawMessage, [][]json.RawMessage) {
	t.Helper()

	input := make([]json.RawMessage, 0, len(texts))
	for _, text := range texts {
		message, err := compactTextInputRawMessage(text)
		if err != nil {
			t.Fatalf("build input message: %v", err)
		}
		input = append(input, message)
	}
	inputRaw, err := json.Marshal(input)
	if err != nil {
		t.Fatalf("marshal input: %v", err)
	}
	requestFields := map[string]json.RawMessage{
		"model": json.RawMessage(`"gpt-5.4"`),
		"input": inputRaw,
	}
	fallbackFields, _, err := compactFallbackRequestFieldsForBodySize(requestFields, targetBodySize)
	if err != nil {
		t.Fatalf("build compact fallback fields: %v", err)
	}
	chunks, _, _, err := splitCompactInputAsHistoricalChunksByBodySize(fallbackFields, input, targetBodySize)
	if err != nil {
		t.Fatalf("split compact input: %v", err)
	}
	return requestFields, chunks
}

func recordMaxInt32(max *atomic.Int32, value int32) {
	for {
		current := max.Load()
		if value <= current || max.CompareAndSwap(current, value) {
			return
		}
	}
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
	legacySyntheticSummary := "Legacy synthetic checkpoint summary"
	legacyPlaintextSummary := "Legacy plaintext summary from an older proxy run."
	opaqueToken := strings.Repeat("Abc123_-", 8)
	opaqueTokenWithProviderChars := "opaque+server/token/with/slashes=="
	legacyPayload, err := json.Marshal(syntheticCompactionPayload{Summary: legacySyntheticSummary})
	if err != nil {
		t.Fatalf("marshal legacy payload: %v", err)
	}

	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": encodeSyntheticCompaction(syntheticSummary),
			},
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": legacySyntheticCompactionPrefix + base64.RawURLEncoding.EncodeToString(legacyPayload),
			},
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": legacyPlaintextSummary,
			},
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": opaqueToken,
			},
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": opaqueTokenWithProviderChars,
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	rewritten, rewriteCount := rewriteSyntheticCompactionRequest(reqBody)
	if rewriteCount != 3 {
		t.Fatalf("expected 3 rewritten compaction items, got %d", rewriteCount)
	}

	var req map[string]interface{}
	if err := json.Unmarshal(rewritten, &req); err != nil {
		t.Fatalf("failed to parse rewritten request: %v", err)
	}

	input, ok := req["input"].([]interface{})
	if !ok || len(input) != 5 {
		t.Fatalf("expected 5 input items, got %#v", req["input"])
	}

	if got := requireCompactionContextMessage(t, input[0]); !strings.Contains(got, syntheticSummary) {
		t.Errorf("expected synthetic summary to be rewritten, got %q", got)
	}
	if got := requireCompactionContextMessage(t, input[1]); !strings.Contains(got, legacySyntheticSummary) {
		t.Errorf("expected legacy synthetic summary to be rewritten, got %q", got)
	}
	if got := requireCompactionContextMessage(t, input[2]); !strings.Contains(got, legacyPlaintextSummary) {
		t.Errorf("expected legacy plaintext summary to be rewritten, got %q", got)
	}

	for i, want := range map[int]string{3: opaqueToken, 4: opaqueTokenWithProviderChars} {
		item, ok := input[i].(map[string]interface{})
		if !ok {
			t.Fatalf("expected opaque item object, got %#v", input[i])
		}
		if item["type"] != "compaction" {
			t.Fatalf("expected unknown token to remain a compaction item, got %#v", item)
		}
		if item["encrypted_content"] != want {
			t.Fatalf("expected unknown token %q to be preserved, got %#v", want, item["encrypted_content"])
		}
	}
}

func TestRewriteSyntheticCompactionRequest_LaterUserInstructionTakesPrecedence(t *testing.T) {
	summary := "Previous task: update proxy/tool_optimizer_responses_test.go and report done."
	latestUserInstruction := "go through all the github code review comments in #116"
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
					{"type": "input_text", "text": latestUserInstruction},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	rewritten, rewriteCount := rewriteSyntheticCompactionRequest(reqBody)
	if rewriteCount != 1 {
		t.Fatalf("expected 1 rewritten compaction item, got %d", rewriteCount)
	}
	rewritten, injected := injectSyntheticCompactionResumePrompt(rewritten)
	if injected {
		t.Fatal("expected resume prompt injection to be skipped when a later user instruction exists")
	}

	var req map[string]interface{}
	if err := json.Unmarshal(rewritten, &req); err != nil {
		t.Fatalf("failed to parse rewritten request: %v", err)
	}

	input, ok := req["input"].([]interface{})
	if !ok || len(input) != 2 {
		t.Fatalf("expected 2 input items, got %#v", req["input"])
	}

	contextText := requireCompactionContextMessage(t, input[0])
	if !strings.Contains(contextText, "Messages after this checkpoint are the active request and take precedence") {
		t.Fatalf("expected checkpoint to defer to later user instructions, got %q", contextText)
	}
	if strings.Contains(contextText, "Continue the same task immediately") {
		t.Fatalf("checkpoint must not force stale work ahead of a later user instruction, got %q", contextText)
	}
	if got := requireMessageTextWithRole(t, input[1], "user"); got != latestUserInstruction {
		t.Fatalf("expected latest user instruction to be preserved, got %q", got)
	}
}

func TestRewriteSyntheticCompactionRequest_CodexPostCompactionOrdering(t *testing.T) {
	summary := "Prior compacted work said to finish the old smoke-test task."
	latestUserInstruction := "USER_THREE"
	reqBody, err := json.Marshal(map[string]interface{}{
		"model": "gpt-5.4",
		"input": []interface{}{
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "USER_ONE"},
				},
			},
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "USER_TWO"},
				},
			},
			map[string]interface{}{
				"type":              "compaction",
				"encrypted_content": encodeSyntheticCompaction(summary),
			},
			map[string]interface{}{
				"type": "message",
				"role": "developer",
				"content": []map[string]string{
					{"type": "input_text", "text": "<PERMISSIONS_INSTRUCTIONS>"},
				},
			},
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": "<ENVIRONMENT_CONTEXT:cwd=PRETURN_CONTEXT_DIFF_CWD>"},
				},
			},
			map[string]interface{}{
				"type": "message",
				"role": "user",
				"content": []map[string]string{
					{"type": "input_text", "text": latestUserInstruction},
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("failed to marshal request body: %v", err)
	}

	rewritten, rewriteCount := rewriteSyntheticCompactionRequest(reqBody)
	if rewriteCount != 1 {
		t.Fatalf("expected 1 rewritten compaction item, got %d", rewriteCount)
	}
	rewritten, injected := injectSyntheticCompactionResumePrompt(rewritten)
	if injected {
		t.Fatal("expected resume prompt injection to be skipped when Codex includes a later user instruction")
	}

	var req map[string]interface{}
	if err := json.Unmarshal(rewritten, &req); err != nil {
		t.Fatalf("failed to parse rewritten request: %v", err)
	}

	input, ok := req["input"].([]interface{})
	if !ok || len(input) != 6 {
		t.Fatalf("expected 6 input items, got %#v", req["input"])
	}
	if got := requireMessageTextWithRole(t, input[0], "user"); got != "USER_ONE" {
		t.Fatalf("expected historical user one to be preserved, got %q", got)
	}
	contextText := requireMessageTextWithRole(t, input[2], "developer")
	if !strings.Contains(contextText, "Messages after this checkpoint are the active request and take precedence") {
		t.Fatalf("expected checkpoint to defer to Codex's later request items, got %q", contextText)
	}
	if strings.Contains(contextText, "Continue the same task immediately") {
		t.Fatalf("checkpoint must not force stale work ahead of later Codex request items, got %q", contextText)
	}
	if got := requireMessageTextWithRole(t, input[3], "developer"); got != "<PERMISSIONS_INSTRUCTIONS>" {
		t.Fatalf("expected later developer instructions to be preserved, got %q", got)
	}
	if got := requireMessageTextWithRole(t, input[5], "user"); got != latestUserInstruction {
		t.Fatalf("expected latest user instruction to be preserved, got %q", got)
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

	legacyPlaintextSummary := "The issue is partially fixed."
	if got, ok := extractSyntheticOrLegacyCompactionSummary(legacyPlaintextSummary); !ok || got != legacyPlaintextSummary {
		t.Fatalf("expected legacy plaintext summary to be recovered, got %q ok=%v", got, ok)
	}

	opaqueToken := strings.Repeat("Abc123_-", 8)
	if got, ok := extractSyntheticOrLegacyCompactionSummary(opaqueToken); ok {
		t.Fatalf("expected opaque token to pass through unchanged, got %q", got)
	}

	opaqueTokenWithProviderChars := "opaque+server/token/with/slashes=="
	if got, ok := extractSyntheticOrLegacyCompactionSummary(opaqueTokenWithProviderChars); ok {
		t.Fatalf("expected provider opaque token to pass through unchanged, got %q", got)
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
		{"x-github-api-version", "2025-05-01"},
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

func TestCopilotHeaderProfilesConfigProfileForEndpointRawDoesNotApplyDefaults(t *testing.T) {
	profiles := CopilotHeaderProfilesConfig{
		Default: CopilotHeaderConfig{
			UserAgent: "provider-agent",
		},
	}
	base := CopilotHeaderConfig{
		EditorVersion: "base-editor",
	}

	raw := profiles.profileForEndpointRaw("/models", base)
	if raw.EditorVersion != "base-editor" {
		t.Fatalf("raw EditorVersion = %q, want base-editor", raw.EditorVersion)
	}
	if raw.UserAgent != "provider-agent" {
		t.Fatalf("raw UserAgent = %q, want provider-agent", raw.UserAgent)
	}
	if raw.OpenAIIntent != "" {
		t.Fatalf("raw OpenAIIntent = %q, want empty", raw.OpenAIIntent)
	}

	withDefaults := profiles.profileForEndpoint("/models", base)
	if withDefaults.OpenAIIntent != defaultCopilotOpenAIIntent {
		t.Fatalf("defaulted OpenAIIntent = %q, want %q", withDefaults.OpenAIIntent, defaultCopilotOpenAIIntent)
	}
}

func TestCopilotHeaderProfilesConfigProfileForEndpoint(t *testing.T) {
	base := CopilotHeaderConfig{
		EditorVersion:       "base-editor",
		EditorPluginVersion: "base-plugin",
		UserAgent:           "base-agent",
		IntegrationID:       "base-integration",
		GitHubAPIVersion:    "base-api",
		OpenAIIntent:        "base-intent",
	}
	profiles := CopilotHeaderProfilesConfig{
		Default: CopilotHeaderConfig{
			UserAgent:     "provider-agent",
			IntegrationID: "provider-integration",
		},
		ChatCompletions: CopilotHeaderConfig{
			EditorVersion: "chat-editor",
			OpenAIIntent:  "chat-intent",
		},
		Responses: CopilotHeaderConfig{
			EditorVersion: "responses-editor",
			OpenAIIntent:  "responses-intent",
		},
	}

	tests := []struct {
		name       string
		endpoint   string
		wantEditor string
		wantIntent string
	}{
		{name: "chat completions profile", endpoint: "/chat/completions", wantEditor: "chat-editor", wantIntent: "chat-intent"},
		{name: "responses profile", endpoint: "/responses", wantEditor: "responses-editor", wantIntent: "responses-intent"},
		{name: "models use provider default profile", endpoint: "/models", wantEditor: "base-editor", wantIntent: "base-intent"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := profiles.profileForEndpoint(tt.endpoint, base)
			if got.EditorVersion != tt.wantEditor {
				t.Fatalf("EditorVersion = %q, want %q", got.EditorVersion, tt.wantEditor)
			}
			if got.OpenAIIntent != tt.wantIntent {
				t.Fatalf("OpenAIIntent = %q, want %q", got.OpenAIIntent, tt.wantIntent)
			}
			if got.UserAgent != "provider-agent" {
				t.Fatalf("UserAgent = %q, want provider-agent", got.UserAgent)
			}
			if got.IntegrationID != "provider-integration" {
				t.Fatalf("IntegrationID = %q, want provider-integration", got.IntegrationID)
			}
			if got.EditorPluginVersion != "base-plugin" {
				t.Fatalf("EditorPluginVersion = %q, want base-plugin", got.EditorPluginVersion)
			}
			if got.GitHubAPIVersion != "base-api" {
				t.Fatalf("GitHubAPIVersion = %q, want base-api", got.GitHubAPIVersion)
			}
		})
	}
}

func TestNewProviderJSONRequest_UsesConfiguredCopilotHeaderProfiles(t *testing.T) {
	handler := &ProxyHandler{
		auth: auth.NewTestAuthenticator("test-token"),
		copilotHeaders: CopilotHeaderConfig{
			EditorVersion:       "base-editor",
			EditorPluginVersion: "base-plugin",
			UserAgent:           "base-agent",
			IntegrationID:       "base-integration",
			GitHubAPIVersion:    "base-api",
			OpenAIIntent:        "base-intent",
		},
	}
	provider := &providerRuntime{
		id:      "copilot",
		kind:    providerTypeCopilot,
		baseURL: "https://copilot.example.test",
		headerProfiles: CopilotHeaderProfilesConfig{
			Default: CopilotHeaderConfig{
				UserAgent:     "provider-agent",
				IntegrationID: "provider-integration",
			},
			ChatCompletions: CopilotHeaderConfig{
				EditorVersion: "chat-editor",
				OpenAIIntent:  "chat-intent",
			},
			Responses: CopilotHeaderConfig{
				EditorVersion: "responses-editor",
				OpenAIIntent:  "responses-intent",
			},
		},
	}

	tests := []struct {
		path       string
		wantEditor string
		wantIntent string
	}{
		{path: "/chat/completions", wantEditor: "chat-editor", wantIntent: "chat-intent"},
		{path: "/responses", wantEditor: "responses-editor", wantIntent: "responses-intent"},
	}

	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			req, err := handler.newProviderJSONRequest(context.Background(), provider, http.MethodPost, tt.path, []byte(`{"model":"gpt-test"}`), nil, "")
			if err != nil {
				t.Fatalf("newProviderJSONRequest() error = %v", err)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer test-token" {
				t.Fatalf("Authorization = %q, want bearer token", got)
			}
			if got := req.Header.Get("editor-version"); got != tt.wantEditor {
				t.Fatalf("editor-version = %q, want %q", got, tt.wantEditor)
			}
			if got := req.Header.Get("openai-intent"); got != tt.wantIntent {
				t.Fatalf("openai-intent = %q, want %q", got, tt.wantIntent)
			}
			if got := req.Header.Get("user-agent"); got != "provider-agent" {
				t.Fatalf("user-agent = %q, want provider-agent", got)
			}
			if got := req.Header.Get("copilot-integration-id"); got != "provider-integration" {
				t.Fatalf("copilot-integration-id = %q, want provider-integration", got)
			}
			if got := req.Header.Get("editor-plugin-version"); got != "base-plugin" {
				t.Fatalf("editor-plugin-version = %q, want base-plugin", got)
			}
			if got := req.Header.Get("x-github-api-version"); got != "base-api" {
				t.Fatalf("x-github-api-version = %q, want base-api", got)
			}
		})
	}
}

func TestNewProviderJSONRequest_CopilotModelsOmitsIntentAndContentTypeByDefault(t *testing.T) {
	handler := &ProxyHandler{
		auth: auth.NewTestAuthenticator("test-token"),
	}
	provider := &providerRuntime{
		id:      "copilot",
		kind:    providerTypeCopilot,
		baseURL: "https://copilot.example.test",
	}

	req, err := handler.newProviderJSONRequest(context.Background(), provider, http.MethodGet, "/models", nil, nil, "")
	if err != nil {
		t.Fatalf("newProviderJSONRequest() error = %v", err)
	}

	tests := []struct {
		header   string
		expected string
	}{
		{"Authorization", "Bearer test-token"},
		{"editor-version", defaultCopilotEditorVersion},
		{"editor-plugin-version", defaultCopilotEditorPluginVersion},
		{"user-agent", defaultCopilotUserAgent},
		{"copilot-integration-id", defaultCopilotIntegrationID},
		{"x-github-api-version", defaultCopilotGitHubAPIVersion},
	}
	for _, tt := range tests {
		if got := req.Header.Get(tt.header); got != tt.expected {
			t.Fatalf("%s = %q, want %q", tt.header, got, tt.expected)
		}
	}
	if got := req.Header.Get("x-request-id"); got == "" {
		t.Fatal("x-request-id = empty, want generated UUID")
	}
	if got := req.Header.Get("openai-intent"); got != "" {
		t.Fatalf("openai-intent = %q, want omitted for default /models", got)
	}
	if got := req.Header.Get("Content-Type"); got != "" {
		t.Fatalf("Content-Type = %q, want omitted for GET /models", got)
	}
}

func TestNewProviderJSONRequest_CopilotModelsKeepsExplicitIntent(t *testing.T) {
	tests := []struct {
		name           string
		copilotHeaders CopilotHeaderConfig
		headerProfiles CopilotHeaderProfilesConfig
		wantIntent     string
	}{
		{
			name: "global config",
			copilotHeaders: CopilotHeaderConfig{
				OpenAIIntent: "global-intent",
			},
			wantIntent: "global-intent",
		},
		{
			name: "provider default profile",
			headerProfiles: CopilotHeaderProfilesConfig{
				Default: CopilotHeaderConfig{
					OpenAIIntent: "provider-intent",
				},
			},
			wantIntent: "provider-intent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &ProxyHandler{
				auth:           auth.NewTestAuthenticator("test-token"),
				copilotHeaders: tt.copilotHeaders,
			}
			provider := &providerRuntime{
				id:             "copilot",
				kind:           providerTypeCopilot,
				baseURL:        "https://copilot.example.test",
				headerProfiles: tt.headerProfiles,
			}

			req, err := handler.newProviderJSONRequest(context.Background(), provider, http.MethodGet, "/models", nil, nil, "")
			if err != nil {
				t.Fatalf("newProviderJSONRequest() error = %v", err)
			}
			if got := req.Header.Get("openai-intent"); got != tt.wantIntent {
				t.Fatalf("openai-intent = %q, want %q", got, tt.wantIntent)
			}
			if got := req.Header.Get("Content-Type"); got != "" {
				t.Fatalf("Content-Type = %q, want omitted for GET /models", got)
			}
		})
	}
}

func TestNewProviderJSONRequest_CopilotChatAndResponsesUseDefaultIntent(t *testing.T) {
	handler := &ProxyHandler{
		auth: auth.NewTestAuthenticator("test-token"),
	}
	provider := &providerRuntime{
		id:      "copilot",
		kind:    providerTypeCopilot,
		baseURL: "https://copilot.example.test",
	}

	for _, path := range []string{"/chat/completions", "/responses"} {
		t.Run(path, func(t *testing.T) {
			req, err := handler.newProviderJSONRequest(context.Background(), provider, http.MethodPost, path, []byte(`{"model":"gpt-test"}`), nil, "")
			if err != nil {
				t.Fatalf("newProviderJSONRequest() error = %v", err)
			}
			if got := req.Header.Get("openai-intent"); got != defaultCopilotOpenAIIntent {
				t.Fatalf("openai-intent = %q, want %q", got, defaultCopilotOpenAIIntent)
			}
			if got := req.Header.Get("Content-Type"); got != "application/json" {
				t.Fatalf("Content-Type = %q, want application/json", got)
			}
		})
	}
}

func TestNewProviderJSONRequest_StripsClientCopilotHeadersForAzure(t *testing.T) {
	handler := &ProxyHandler{}
	provider := &providerRuntime{
		id:      "azure",
		kind:    providerTypeAzureOpenAI,
		baseURL: "https://azure.example.test/openai/v1",
		apiKey:  "azure-key",
	}

	req, err := handler.newProviderJSONRequest(
		context.Background(),
		provider,
		http.MethodPost,
		"/responses",
		[]byte(`{"model":"gpt-test"}`),
		http.Header{
			"Authorization":          []string{"Bearer client-copilot-token"},
			"editor-version":         []string{"client-editor"},
			"editor-plugin-version":  []string{"client-plugin"},
			"user-agent":             []string{"client-agent"},
			"copilot-integration-id": []string{"client-integration"},
			"x-github-api-version":   []string{"client-api"},
			"x-request-id":           []string{"client-request-id"},
			"openai-intent":          []string{"client-intent"},
			"Traceparent":            []string{"00-11111111111111111111111111111111-2222222222222222-01"},
		},
		"",
	)
	if err != nil {
		t.Fatalf("newProviderJSONRequest() error = %v", err)
	}

	for _, header := range []string{"Authorization", "editor-version", "editor-plugin-version", "user-agent", "copilot-integration-id", "x-github-api-version", "x-request-id", "openai-intent"} {
		if got := req.Header.Get(header); got != "" {
			t.Fatalf("%s = %q, want stripped for Azure", header, got)
		}
	}
	if got := req.Header.Get("api-key"); got != "azure-key" {
		t.Fatalf("api-key = %q, want azure-key", got)
	}
	if got := req.Header.Get("Traceparent"); got != "00-11111111111111111111111111111111-2222222222222222-01" {
		t.Fatalf("Traceparent = %q, want passthrough trace header", got)
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

func TestHandleOpenAIChatCompletions_RetriesWithoutInjectedStreamOptions(t *testing.T) {
	var calls atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		body, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("upstream request JSON: %v", err)
		}
		if n == 1 {
			if _, ok := payload["stream_options"]; !ok {
				t.Fatalf("first request should include injected stream_options: %s", body)
			}
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"unknown field stream_options","type":"invalid_request_error"}}`))
			return
		}
		if n != 2 {
			t.Fatalf("unexpected upstream call %d", n)
		}
		if _, ok := payload["stream_options"]; ok {
			t.Fatalf("retry should remove injected stream_options: %s", body)
		}
		if string(payload["stream"]) != "true" {
			t.Fatalf("retry should remain streaming, stream=%s", payload["stream"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n"))
	})

	reqBody := `{"model":"gpt-4","stream":true,"messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d want 200 after retry, body=%s", resp.StatusCode, body)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "data: [DONE]") || !strings.Contains(string(body), "ok") {
		t.Fatalf("stream response not forwarded after retry: %s", body)
	}
	if calls.Load() != 2 {
		t.Fatalf("upstream calls = %d want 2", calls.Load())
	}
}

func TestHandleOpenAIChatCompletionsUpstreamError(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"service unavailable","type":"server_error"}}`))
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

func TestHandleOpenAIChatCompletions_RoutesGenericOpenAICompatibleModel(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/chat/completions" {
			t.Fatalf("expected generic chat path /v1/chat/completions, got %s", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer generic-key" {
			t.Fatalf("expected generic bearer auth, got %q", got)
		}
		if got := r.Header.Get("X-Provider"); got != "local" {
			t.Fatalf("expected configured X-Provider header, got %q", got)
		}
		if got := r.Header.Get("editor-version"); got != "" {
			t.Fatalf("expected Copilot headers stripped, got editor-version %q", got)
		}

		var upstreamReq models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if upstreamReq.Model != "local-upstream" {
			t.Fatalf("upstream model = %q, want local-upstream", upstreamReq.Model)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID:     "chatcmpl-generic",
			Object: "chat.completion",
			Choices: []models.OpenAIChoice{{
				Index: 0,
				Message: models.OpenAIMessage{
					Role:    "assistant",
					Content: json.RawMessage(`"Hi from generic"`),
				},
			}},
		})
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:                  "local",
				Type:                "openai-compatible",
				Default:             true,
				BaseURL:             upstream.URL,
				APIKey:              "generic-key",
				ExtraHeaders:        map[string]string{"X-Provider": "local"},
				ChatCompletionsPath: "/v1/chat/completions",
				Models: []ProviderModelConfig{{
					PublicID:   "local-public",
					Deployment: "local-upstream",
					Endpoints:  []string{"/chat/completions"},
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model": "local-public",
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
	if len(oaiResp.Choices) != 1 || string(oaiResp.Choices[0].Message.Content) != `"Hi from generic"` {
		t.Fatalf("unexpected response body: %+v", oaiResp)
	}
}

func TestHandleAnthropicMessages_UsesOpenAITranslationForGenericOpenAICompatibleProvider(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/chat/completions" {
			t.Fatalf("expected translated Anthropic request to hit /chat/completions, got %s", got)
		}
		var upstreamReq models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if upstreamReq.Model != "local-upstream" {
			t.Fatalf("upstream model = %q, want local-upstream", upstreamReq.Model)
		}
		if len(upstreamReq.Messages) == 0 || upstreamReq.Messages[len(upstreamReq.Messages)-1].Role != "user" {
			t.Fatalf("expected translated user message, got %+v", upstreamReq.Messages)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chunk-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello via translation\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:      "local",
				Type:    "openai-compatible",
				Default: true,
				BaseURL: upstream.URL,
				Models: []ProviderModelConfig{{
					PublicID:   "local-public",
					Deployment: "local-upstream",
					Endpoints:  []string{"/chat/completions"},
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model": "local-public",
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "Hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessages(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var anthropicResp models.AnthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&anthropicResp); err != nil {
		t.Fatalf("decode Anthropic response: %v", err)
	}
	if len(anthropicResp.Content) != 1 || anthropicResp.Content[0].Text == nil || *anthropicResp.Content[0].Text != "Hello via translation" {
		t.Fatalf("unexpected Anthropic response: %+v", anthropicResp)
	}
}

func TestHandleAnthropicMessages_RejectsMissingMaxTokens(t *testing.T) {
	var upstreamHits atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model": "claude-sonnet-4",
		"messages": [{"role": "user", "content": "Hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessages(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 400: %s", resp.StatusCode, body)
	}
	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("upstream hits = %d, want 0", got)
	}
	var anthropicErr models.AnthropicError
	if err := json.NewDecoder(resp.Body).Decode(&anthropicErr); err != nil {
		t.Fatalf("decode error response: %v", err)
	}
	if anthropicErr.Error.Type != "invalid_request_error" || !strings.Contains(anthropicErr.Error.Message, "max_tokens") {
		t.Fatalf("unexpected Anthropic error: %+v", anthropicErr)
	}
}

func TestHandleAnthropicMessages_PrewarmUsesNonStreamingUpstream(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		var upstreamReq models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if upstreamReq.Stream != nil {
			t.Fatalf("stream = %v, want omitted for prewarm", *upstreamReq.Stream)
		}
		if upstreamReq.StreamOptions != nil {
			t.Fatalf("stream_options = %+v, want nil for prewarm", upstreamReq.StreamOptions)
		}
		if upstreamReq.MaxTokens == nil || *upstreamReq.MaxTokens != 0 {
			t.Fatalf("max_tokens = %v, want 0", upstreamReq.MaxTokens)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-prewarm","object":"chat.completion","model":"claude-sonnet-4","choices":[],"usage":{"prompt_tokens":5,"completion_tokens":0,"total_tokens":5}}`)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model": "claude-sonnet-4",
		"max_tokens": 0,
		"messages": [{"role": "user", "content": "warm cache"}]
	}`))
	w := httptest.NewRecorder()
	handler.HandleAnthropicMessages(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	var anthropicResp models.AnthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&anthropicResp); err != nil {
		t.Fatalf("decode Anthropic response: %v", err)
	}
	if anthropicResp.StopReason == nil || *anthropicResp.StopReason != "end_turn" || anthropicResp.Usage.InputTokens != 5 || anthropicResp.Usage.OutputTokens != 0 {
		t.Fatalf("unexpected prewarm response: %+v", anthropicResp)
	}
}

func TestHandleAnthropicMessages_StreamedInterleavedThinkingUsesEffectiveLimit(t *testing.T) {
	var upstreamHits atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		var upstreamReq models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if upstreamReq.Stream == nil || !*upstreamReq.Stream {
			t.Fatalf("stream = %v, want true", upstreamReq.Stream)
		}
		if upstreamReq.MaxCompletionTokens == nil || *upstreamReq.MaxCompletionTokens != 8192 {
			t.Fatalf("max_completion_tokens = %v, want 8192", upstreamReq.MaxCompletionTokens)
		}
		if upstreamReq.MaxTokens != nil {
			t.Fatalf("max_tokens = %v, want nil", upstreamReq.MaxTokens)
		}
		if upstreamReq.StreamOptions == nil || !upstreamReq.StreamOptions.IncludeUsage {
			t.Fatalf("stream_options = %+v, want include_usage", upstreamReq.StreamOptions)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chunk-interleaved\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"streamed\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model": "claude-opus-4-5",
		"max_tokens": 0,
		"stream": true,
		"thinking": {"type": "enabled", "budget_tokens": 8192},
		"tools": [{"name": "lookup", "input_schema": {"type": "object"}}],
		"messages": [{"role": "user", "content": "use tools"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Anthropic-Beta", anthropicInterleavedThinkingBeta)
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessages(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d, want 200: %s", resp.StatusCode, body)
	}
	if got := upstreamHits.Load(); got != 1 {
		t.Fatalf("upstream hits = %d, want 1", got)
	}
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("streamed")) {
		t.Fatalf("streamed response body = %q, want translated content", body)
	}
}

func TestHandleAnthropicMessages_AzureUsesConfiguredMaxCompletionTokens(t *testing.T) {
	t.Setenv("TEST_AZURE_API_KEY", "test-value")
	useMaxCompletionTokens := true
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/openai/v1/chat/completions" {
			t.Fatalf("expected Azure path /openai/v1/chat/completions, got %s", got)
		}

		var upstreamReq models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if upstreamReq.Model != "gpt-5.6-sol" {
			t.Fatalf("upstream model = %q, want gpt-5.6-sol", upstreamReq.Model)
		}
		if upstreamReq.MaxCompletionTokens == nil || *upstreamReq.MaxCompletionTokens != 64 {
			t.Fatalf("max_completion_tokens = %v, want 64", upstreamReq.MaxCompletionTokens)
		}
		if upstreamReq.MaxTokens != nil {
			t.Fatalf("max_tokens = %v, want nil", upstreamReq.MaxTokens)
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chunk-1\",\"object\":\"chat.completion.chunk\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello from Sol\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":4,\"total_tokens\":7}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:        "azure",
				Type:      "azure-openai",
				Default:   true,
				BaseURL:   upstream.URL + "/openai/v1",
				APIKeyEnv: "TEST_AZURE_API_KEY",
				Models: []ProviderModelConfig{{
					PublicID:               "gpt-5.6-sol",
					Deployment:             "gpt-5.6-sol",
					Endpoints:              []string{"/chat/completions"},
					UseMaxCompletionTokens: &useMaxCompletionTokens,
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model": "gpt-5.6-sol",
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "Hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessages(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var anthropicResp models.AnthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&anthropicResp); err != nil {
		t.Fatalf("decode Anthropic response: %v", err)
	}
	if len(anthropicResp.Content) != 1 || anthropicResp.Content[0].Text == nil || *anthropicResp.Content[0].Text != "Hello from Sol" {
		t.Fatalf("unexpected Anthropic response: %+v", anthropicResp)
	}
}

func TestHandleAnthropicMessages_DirectGenericAnthropicCompatibleProvider(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/native/messages" {
			t.Fatalf("expected native messages path /native/messages, got %s", got)
		}
		if got := r.Header.Get("X-API-Key"); got != "anthropic-key" {
			t.Fatalf("expected X-API-Key auth, got %q", got)
		}
		if got := r.Header.Get("Anthropic-Version"); got != "2023-06-01" {
			t.Fatalf("expected Anthropic-Version forwarded, got %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("expected client Authorization stripped, got %q", got)
		}

		var upstreamReq models.AnthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if upstreamReq.Model != "claude-upstream" {
			t.Fatalf("upstream model = %q, want claude-upstream", upstreamReq.Model)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg-direct","type":"message","role":"assistant","model":"claude-upstream","content":[{"type":"text","text":"direct"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:           "native",
				Type:         "anthropic-compatible",
				Default:      true,
				BaseURL:      upstream.URL,
				APIKey:       "anthropic-key",
				AuthType:     "api-key-header",
				AuthHeader:   "X-API-Key",
				MessagesPath: "/native/messages",
				Models: []ProviderModelConfig{{
					PublicID:   "claude-public",
					Deployment: "claude-upstream",
					Endpoints:  []string{"/v1/messages"},
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model": "claude-public",
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "Hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer client-token")
	req.Header.Set("Anthropic-Version", "2023-06-01")
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessages(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var anthropicResp models.AnthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&anthropicResp); err != nil {
		t.Fatalf("decode Anthropic response: %v", err)
	}
	if anthropicResp.ID != "msg-direct" || len(anthropicResp.Content) != 1 || anthropicResp.Content[0].Text == nil || *anthropicResp.Content[0].Text != "direct" {
		t.Fatalf("unexpected direct Anthropic response: %+v", anthropicResp)
	}
	if anthropicResp.Model != "claude-public" {
		t.Fatalf("direct Anthropic response model = %q, want public alias", anthropicResp.Model)
	}
}

func TestHandleAnthropicMessages_DirectGenericAnthropicCompatibleProviderStreamsSSE(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/messages" {
			t.Fatalf("expected default native messages path /v1/messages, got %s", got)
		}
		var upstreamReq models.AnthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if !upstreamReq.Stream {
			t.Fatal("expected stream=true to be forwarded")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg-stream\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-upstream\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"))
		_, _ = w.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"stream-direct\"}}\n\n"))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:       "native",
				Type:     "anthropic-compatible",
				Default:  true,
				BaseURL:  upstream.URL,
				AuthType: "none",
				Models: []ProviderModelConfig{{
					PublicID:   "claude-public",
					Deployment: "claude-upstream",
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model": "claude-public",
		"max_tokens": 64,
		"stream": true,
		"messages": [{"role": "user", "content": "Hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessages(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", got)
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "stream-direct") {
		t.Fatalf("expected direct SSE body to pass through, got %q", body)
	}
	if !strings.Contains(string(body), `"model":"claude-public"`) {
		t.Fatalf("expected direct SSE model to be rewritten to public alias, got %q", body)
	}
	if strings.Contains(string(body), `"model":"claude-upstream"`) {
		t.Fatalf("direct SSE leaked upstream model: %q", body)
	}
	if !w.Flushed {
		t.Fatal("expected direct SSE response to flush")
	}
}

func TestHandleAnthropicMessages_DirectGenericAnthropicCompatibleProviderNormalizesModelAlias(t *testing.T) {
	var openAIHits atomic.Int32
	openAIUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		openAIHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer openAIUpstream.Close()

	anthropicUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var upstreamReq models.AnthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if upstreamReq.Model != "claude-upstream" {
			t.Fatalf("upstream model = %q, want claude-upstream", upstreamReq.Model)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg-normalized","type":"message","role":"assistant","model":"claude-upstream","content":[{"type":"text","text":"normalized"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer anthropicUpstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{
					ID:       "openai-default",
					Type:     "openai-compatible",
					Default:  true,
					BaseURL:  openAIUpstream.URL,
					AuthType: "none",
					Models: []ProviderModelConfig{{
						PublicID:  "gpt-default",
						Endpoints: []string{"/chat/completions"},
					}},
				},
				{
					ID:       "native",
					Type:     "anthropic-compatible",
					BaseURL:  anthropicUpstream.URL,
					AuthType: "none",
					Models: []ProviderModelConfig{{
						PublicID:   "claude-sonnet-4.5",
						Deployment: "claude-upstream",
					}},
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model": "claude-sonnet-4-5",
		"max_tokens": 64,
		"messages": [{"role": "user", "content": "Hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessages(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	var anthropicResp models.AnthropicResponse
	if err := json.NewDecoder(resp.Body).Decode(&anthropicResp); err != nil {
		t.Fatalf("decode Anthropic response: %v", err)
	}
	if anthropicResp.Model != "claude-sonnet-4.5" {
		t.Fatalf("response model = %q, want normalized public alias", anthropicResp.Model)
	}
	if got := openAIHits.Load(); got != 0 {
		t.Fatalf("expected normalized Anthropic model to route direct, got %d OpenAI hits", got)
	}
}

func TestHandleOpenAIChatCompletions_RejectsUnknownStaticGenericModel(t *testing.T) {
	var upstreamHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:       "local",
				Type:     "openai-compatible",
				Default:  true,
				BaseURL:  upstream.URL,
				AuthType: "none",
				Models: []ProviderModelConfig{{
					PublicID:  "local-public",
					Endpoints: []string{"/chat/completions"},
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model": "other",
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
	if got := upstreamHits.Load(); got != 0 {
		t.Fatalf("expected unknown static model to be rejected before upstream, got %d hits", got)
	}
}

func TestHandleModels_GenericOpenAICompatibleEndpointVisibility(t *testing.T) {
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:       "local",
				Type:     "openai-compatible",
				Default:  true,
				BaseURL:  "http://localhost:1234",
				AuthType: "none",
				Models: []ProviderModelConfig{
					{
						PublicID: "chat-only",
						Name:     "Chat Only",
					},
					{
						PublicID:  "responses-capable",
						Name:      "Responses Capable",
						Endpoints: []string{"/chat/completions", "/responses"},
					},
				},
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
			SupportedEndpoints []string `json:"supported_endpoints"`
		} `json:"data"`
		Models []struct {
			Slug           string `json:"slug"`
			Visibility     string `json:"visibility"`
			SupportedInAPI bool   `json:"supported_in_api"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode models response: %v", err)
	}

	endpointsByID := make(map[string][]string)
	for _, model := range result.Data {
		endpointsByID[model.ID] = model.SupportedEndpoints
	}
	if !reflect.DeepEqual(endpointsByID["chat-only"], []string{"/chat/completions"}) {
		t.Fatalf("chat-only endpoints = %v, want [/chat/completions]", endpointsByID["chat-only"])
	}
	if endpoints := endpointsByID["responses-capable"]; !supportsEndpoint(endpoints, "/responses") {
		t.Fatalf("responses-capable endpoints = %v, want /responses support", endpoints)
	}

	codexBySlug := make(map[string]struct {
		Visibility     string
		SupportedInAPI bool
	})
	for _, model := range result.Models {
		codexBySlug[model.Slug] = struct {
			Visibility     string
			SupportedInAPI bool
		}{Visibility: model.Visibility, SupportedInAPI: model.SupportedInAPI}
	}
	if got := codexBySlug["chat-only"]; got.Visibility != "hide" || got.SupportedInAPI {
		t.Fatalf("chat-only Codex metadata = %+v, want hidden and unsupported", got)
	}
	if got := codexBySlug["responses-capable"]; got.Visibility != "list" || !got.SupportedInAPI {
		t.Fatalf("responses-capable Codex metadata = %+v, want listed and supported", got)
	}
}

func TestHandleResponses_AllowsDiscoveredDynamicGenericResponsesModel(t *testing.T) {
	var responsesHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"dynamic-responses","object":"model","supported_endpoints":["/responses"],"name":"Dynamic Responses"}]}`))
		case "/responses":
			responsesHits.Add(1)
			var upstreamReq map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
				t.Fatalf("decode upstream request: %v", err)
			}
			if got := rawJSONString(upstreamReq["model"]); got != "dynamic-responses" {
				t.Fatalf("upstream model = %q, want dynamic-responses", got)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-dynamic","object":"response","status":"completed","model":"dynamic-responses","output":[]}`))
		default:
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:             "dynamic",
				Type:           "openai-compatible",
				Default:        true,
				BaseURL:        upstream.URL,
				AuthType:       "none",
				ModelDiscovery: "openai",
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	modelsReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsW := httptest.NewRecorder()
	handler.HandleModels(modelsW, modelsReq)
	if resp := modelsW.Result(); resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected models 200, got %d: %s", resp.StatusCode, body)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model": "dynamic-responses",
		"input": "Hello"
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if got := responsesHits.Load(); got != 1 {
		t.Fatalf("expected one responses hit, got %d", got)
	}
}

func TestHandleModels_GenericDynamicDiscoveryMatchesStaticAliasByDeployment(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"upstream-model","object":"model","supported_endpoints":["/chat/completions","/responses"],"name":"Upstream Model"}]}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:             "dynamic",
				Type:           "openai-compatible",
				Default:        true,
				BaseURL:        upstream.URL,
				AuthType:       "none",
				ModelDiscovery: "openai",
				Models: []ProviderModelConfig{{
					PublicID:   "public-alias",
					Deployment: "upstream-model",
					Endpoints:  []string{"/responses"},
					Name:       "Public Alias",
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
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	if len(result.Data) != 1 {
		t.Fatalf("models count = %d, want 1: %+v", len(result.Data), result.Data)
	}
	if result.Data[0].ID != "public-alias" {
		t.Fatalf("model id = %q, want public-alias", result.Data[0].ID)
	}
	if result.Data[0].Name != "Public Alias" {
		t.Fatalf("model name = %q, want Public Alias", result.Data[0].Name)
	}
	if !reflect.DeepEqual(result.Data[0].SupportedEndpoints, []string{"/responses"}) {
		t.Fatalf("supported endpoints = %v, want [/responses]", result.Data[0].SupportedEndpoints)
	}
}

func TestHandleModels_GenericDynamicDiscoveryExpandsDeploymentAliases(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected upstream path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"upstream-model","object":"model","supported_endpoints":["/chat/completions","/responses"],"name":"Upstream Model"}]}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:             "dynamic",
				Type:           "openai-compatible",
				Default:        true,
				BaseURL:        upstream.URL,
				AuthType:       "none",
				ModelDiscovery: "openai",
				Models: []ProviderModelConfig{
					{
						PublicID:   "public-chat",
						Deployment: "upstream-model",
						Endpoints:  []string{"/chat/completions"},
						Name:       "Public Chat",
					},
					{
						PublicID:   "public-responses",
						Deployment: "upstream-model",
						Endpoints:  []string{"/responses"},
						Name:       "Public Responses",
					},
				},
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
			SupportedEndpoints []string `json:"supported_endpoints"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode models response: %v", err)
	}

	endpointsByID := make(map[string][]string, len(result.Data))
	for _, model := range result.Data {
		endpointsByID[model.ID] = model.SupportedEndpoints
	}
	if len(endpointsByID) != 2 {
		t.Fatalf("models = %+v, want two public aliases", result.Data)
	}
	if !reflect.DeepEqual(endpointsByID["public-chat"], []string{"/chat/completions"}) {
		t.Fatalf("public-chat endpoints = %v, want [/chat/completions]", endpointsByID["public-chat"])
	}
	if !reflect.DeepEqual(endpointsByID["public-responses"], []string{"/responses"}) {
		t.Fatalf("public-responses endpoints = %v, want [/responses]", endpointsByID["public-responses"])
	}
	if _, exists := endpointsByID["upstream-model"]; exists {
		t.Fatalf("upstream model should be replaced by configured public aliases")
	}
}

func TestHandleModels_GenericOllamaDiscoveryUsesConfiguredPath(t *testing.T) {
	var modelsCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelsCalls.Add(1)
		if got := r.URL.Path; got != "/api/tags" {
			t.Fatalf("expected Ollama models path /api/tags, got %s", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Fatalf("expected no auth header for auth_type none, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"name":"llama3.2:latest"},{"model":"qwen2.5-coder:latest"}]}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:                  "ollama",
				Type:                "openai-compatible",
				Default:             true,
				BaseURL:             upstream.URL,
				AuthType:            "none",
				ModelDiscovery:      "ollama",
				ModelsPath:          "/api/tags",
				ChatCompletionsPath: "/v1/chat/completions",
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
	if got := modelsCalls.Load(); got != 1 {
		t.Fatalf("expected one Ollama models call, got %d", got)
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
	ids := map[string][]string{}
	for _, model := range result.Data {
		ids[model.ID] = model.SupportedEndpoints
	}
	if !reflect.DeepEqual(ids["llama3.2:latest"], []string{"/chat/completions"}) {
		t.Fatalf("llama endpoints = %v, want [/chat/completions]", ids["llama3.2:latest"])
	}
	if !reflect.DeepEqual(ids["qwen2.5-coder:latest"], []string{"/chat/completions"}) {
		t.Fatalf("qwen endpoints = %v, want [/chat/completions]", ids["qwen2.5-coder:latest"])
	}
}

func TestHandleModels_GenericOpenRouterToolsDiscoveryFiltersToolModels(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/api/v1/models" {
			t.Fatalf("expected OpenRouter models path /api/v1/models, got %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[
			{"id":"tool-capable","name":"Tool Capable","supported_parameters":["tools","tool_choice"],"supported_endpoints":["/chat/completions"]},
			{"id":"plain-chat","name":"Plain Chat","supported_parameters":["temperature"]}
		]}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:             "openrouter",
				Type:           "anthropic-compatible",
				Default:        true,
				BaseURL:        upstream.URL,
				AuthType:       "none",
				ModelDiscovery: "openrouter-tools",
				ModelsPath:     "/api/v1/models",
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
			SupportedEndpoints []string `json:"supported_endpoints"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode models response: %v", err)
	}
	if len(result.Data) != 1 {
		t.Fatalf("models count = %d, want only tool-capable model: %+v", len(result.Data), result.Data)
	}
	if result.Data[0].ID != "tool-capable" {
		t.Fatalf("model id = %q, want tool-capable", result.Data[0].ID)
	}
	if !reflect.DeepEqual(result.Data[0].SupportedEndpoints, []string{"/v1/messages"}) {
		t.Fatalf("supported endpoints = %v, want [/v1/messages]", result.Data[0].SupportedEndpoints)
	}
}

func TestHandleOpenAIChatCompletions_RejectsConfiguredAzureModelWithoutChatSupport(t *testing.T) {
	t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:         "azure",
				Type:       "azure-openai",
				Default:    true,
				BaseURL:    "https://example.openai.azure.com/openai",
				APIVersion: "2025-04-01-preview",
				APIKeyEnv:  "TEST_AZURE_API_KEY",
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
				ID:         "azure",
				Type:       "azure-openai",
				Default:    true,
				BaseURL:    "https://example.openai.azure.com/openai",
				APIVersion: "2025-04-01-preview",
				APIKeyEnv:  "TEST_AZURE_API_KEY",
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

func TestNewProxyHandler_DefersConfiguredCopilotDynamicModelValidation(t *testing.T) {
	t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

	var modelHits atomic.Int32
	copilotServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.4","object":"model","created":0,"owned_by":"github-copilot"}]}`))
	}))
	defer copilotServer.Close()

	authenticator, err := auth.NewAuthenticator(t.TempDir())
	if err != nil {
		t.Fatalf("NewAuthenticator returned error: %v", err)
	}

	_, err = NewProxyHandler(
		authenticator,
		logger.New(logger.LevelInfo),
		withCopilotBaseURLForTest(copilotServer.URL),
		WithDeferredDynamicProviderModelValidation(true),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{ID: "copilot", Type: "copilot", Default: true},
				{
					ID:         "azure",
					Type:       "azure-openai",
					BaseURL:    "https://example.openai.azure.com/openai/v1",
					APIVersion: "2025-04-01-preview",
					APIKeyEnv:  "TEST_AZURE_API_KEY",
					Models: []ProviderModelConfig{{
						PublicID:   "azure-only",
						Deployment: "azure-only",
					}},
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}
	if got := modelHits.Load(); got != 0 {
		t.Fatalf("expected deferred startup to skip Copilot /models fetch, got %d hits", got)
	}
}

func TestValidateDynamicProviderModelsLoadsDeferredConfiguredCopilotModels(t *testing.T) {
	t.Setenv("TEST_AZURE_API_KEY", "azure-test-key")

	copilotServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/models" {
			t.Fatalf("expected /models lookup, got %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.4","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/responses"],"name":"GPT-5.4"}]}`))
	}))
	defer copilotServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		withCopilotBaseURLForTest(copilotServer.URL),
		WithDeferredDynamicProviderModelValidation(true),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{ID: "copilot", Type: "copilot", Default: true},
				{
					ID:         "azure",
					Type:       "azure-openai",
					BaseURL:    "https://example.openai.azure.com/openai/v1",
					APIVersion: "2025-04-01-preview",
					APIKeyEnv:  "TEST_AZURE_API_KEY",
					Models: []ProviderModelConfig{{
						PublicID:   "azure-only",
						Deployment: "azure-only",
					}},
				},
			},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}
	if _, ok := handler.providerSetup().lookupModel("gpt-5.4"); ok {
		t.Fatal("expected deferred Copilot model to be absent before validation")
	}

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	handler.HandleReadyz(w, req)
	resp := w.Result()
	if resp.StatusCode != http.StatusServiceUnavailable {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("readyz before validation status = %d, want 503: %s", resp.StatusCode, body)
	}
	var ready map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&ready); err != nil {
		t.Fatalf("decode readyz response: %v", err)
	}
	if ready["status"] != "not_ready" || !strings.Contains(ready["error"], "provider model validation pending") {
		t.Fatalf("unexpected readyz response before validation: %+v", ready)
	}

	if err := handler.ValidateDynamicProviderModels(context.Background()); err != nil {
		t.Fatalf("ValidateDynamicProviderModels returned error: %v", err)
	}

	model, ok := handler.providerSetup().lookupModel("gpt-5.4")
	if !ok {
		t.Fatal("expected deferred Copilot model to load after validation")
	}
	if model.providerID != "copilot" {
		t.Fatalf("model provider = %q, want copilot", model.providerID)
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
					ID:         "azure",
					Type:       "azure-openai",
					BaseURL:    "https://example.openai.azure.com/openai/v1",
					APIVersion: "2025-04-01-preview",
					APIKeyEnv:  "TEST_AZURE_API_KEY",
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

func TestValidateDynamicProviderModelsLoadsDeferredSingleFilteredProvider(t *testing.T) {
	var modelHits atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/models" {
			t.Fatalf("expected /models lookup, got %s", got)
		}
		modelHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"allowed","object":"model","owned_by":"dynamic"},{"id":"blocked","object":"model","owned_by":"dynamic"}]}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithDeferredDynamicProviderModelValidation(true),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:             "dynamic",
			Type:           "openai-compatible",
			Default:        true,
			BaseURL:        upstream.URL,
			AuthType:       "none",
			ModelDiscovery: "openai",
			IncludeModels:  []string{"allowed"},
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}
	if got := modelHits.Load(); got != 0 {
		t.Fatalf("deferred startup /models hits = %d, want 0", got)
	}
	if !handler.DynamicProviderValidationPending() {
		t.Fatal("expected single filtered dynamic provider validation to be pending")
	}
	if _, ok := handler.providerSetup().lookupModel("allowed"); ok {
		t.Fatal("expected allowed model to be absent before deferred validation")
	}

	if err := handler.ValidateDynamicProviderModels(context.Background()); err != nil {
		t.Fatalf("ValidateDynamicProviderModels returned error: %v", err)
	}
	if got := modelHits.Load(); got != 1 {
		t.Fatalf("deferred validation /models hits = %d, want 1", got)
	}
	if handler.DynamicProviderValidationPending() {
		t.Fatal("expected dynamic provider validation pending state to clear")
	}
	if model, ok := handler.providerSetup().lookupModel("allowed"); !ok || model.providerID != "dynamic" {
		t.Fatalf("allowed canonical model = %+v, %v; want dynamic ownership", model, ok)
	}
	if _, ok := handler.providerSetup().lookupModel("blocked"); ok {
		t.Fatal("expected blocked model to be filtered from canonical ownership")
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
					ID:         "azure",
					Type:       "azure-openai",
					Default:    true,
					BaseURL:    "https://example.openai.azure.com/openai/v1",
					APIVersion: "2025-04-01-preview",
					APIKeyEnv:  "TEST_AZURE_API_KEY",
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
	defer func() { _ = resp.Body.Close() }()
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
					ID:         "azure",
					Type:       "azure-openai",
					BaseURL:    "https://example.openai.azure.com/openai/v1",
					APIVersion: "2025-04-01-preview",
					APIKeyEnv:  "TEST_AZURE_API_KEY",
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
					ID:         "azure",
					Type:       "azure-openai",
					BaseURL:    "https://example.openai.azure.com/openai/v1",
					APIVersion: "2025-04-01-preview",
					APIKeyEnv:  "TEST_AZURE_API_KEY",
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

func TestHandleModels_CanonicalCatalogOwnsRoutingAcrossQueryVariants(t *testing.T) {
	var modelsHits atomic.Int32
	var dynamicChatHits atomic.Int32
	var fallbackChatHits atomic.Int32

	dynamicServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			modelsHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			if r.URL.RawQuery == "view=partial" {
				_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-a","object":"model","owned_by":"dynamic"}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-a","object":"model","owned_by":"dynamic"},{"id":"model-b","object":"model","owned_by":"dynamic"}]}`))
		case "/chat/completions":
			dynamicChatHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"dynamic-chat","object":"chat.completion","choices":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer dynamicServer.Close()

	fallbackServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fallbackChatHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"fallback-chat","object":"chat.completion","choices":[]}`))
	}))
	defer fallbackServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
			{
				ID:             "dynamic",
				Type:           "openai-compatible",
				BaseURL:        dynamicServer.URL,
				AuthType:       "none",
				ModelDiscovery: "openai",
			},
			{
				ID:             "fallback",
				Type:           "openai-compatible",
				Default:        true,
				BaseURL:        fallbackServer.URL,
				AuthType:       "none",
				ModelDiscovery: "static",
				Models: []ProviderModelConfig{{
					PublicID: "fallback-only",
				}},
			},
		}}),
		WithDeferredDynamicProviderModelValidation(true),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	readModelIDs := func(target string) map[string]bool {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		w := httptest.NewRecorder()
		handler.HandleModels(w, req)
		resp := w.Result()
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("HandleModels(%q) status = %d, want 200: %s", target, resp.StatusCode, body)
		}
		var result struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode HandleModels(%q): %v", target, err)
		}
		ids := make(map[string]bool, len(result.Data))
		for _, model := range result.Data {
			ids[model.ID] = true
		}
		return ids
	}

	canonical := readModelIDs("/v1/models")
	if !canonical["model-a"] || !canonical["model-b"] {
		t.Fatalf("canonical catalog = %v, want model-a and model-b", canonical)
	}

	partial := readModelIDs("/v1/models?view=partial")
	if !partial["model-a"] || partial["model-b"] {
		t.Fatalf("partial query catalog = %v, want only model-a from dynamic provider", partial)
	}

	canonicalCached := readModelIDs("/v1/models")
	if !canonicalCached["model-b"] {
		t.Fatalf("cached canonical catalog = %v, want model-b", canonicalCached)
	}
	owner, ok := handler.providerSetup().lookupModel("model-b")
	if !ok || owner.providerID != "dynamic" {
		t.Fatalf("routing owner for model-b = %+v, %v; want dynamic owner matching cached canonical catalog", owner, ok)
	}

	resp, err := handler.postChatCompletions(context.Background(), []byte(`{"model":"model-b","messages":[{"role":"user","content":"hello"}]}`))
	if err != nil {
		t.Fatalf("postChatCompletions(model-b) error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("postChatCompletions(model-b) status = %d, want 200", resp.StatusCode)
	}
	if got := modelsHits.Load(); got != 2 {
		t.Fatalf("dynamic /models hits = %d, want 2 (canonical plus variant; canonical reread cached)", got)
	}
	if got := dynamicChatHits.Load(); got != 1 {
		t.Fatalf("dynamic chat hits = %d, want 1", got)
	}
	if got := fallbackChatHits.Load(); got != 0 {
		t.Fatalf("fallback chat hits = %d, want 0", got)
	}
}

func TestHandleModels_QueryVariantFirstSeedsCanonicalResponsesRouting(t *testing.T) {
	var modelsMu sync.Mutex
	modelsQueries := make([]string, 0, 2)
	var responsesHits atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			modelsMu.Lock()
			modelsQueries = append(modelsQueries, r.URL.RawQuery)
			modelsMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"responses-model","object":"model","owned_by":"dynamic","supported_endpoints":["/responses"]}]}`))
		case "/responses":
			responsesHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","output":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:             "dynamic",
			Type:           "openai-compatible",
			Default:        true,
			BaseURL:        upstream.URL,
			AuthType:       "none",
			ModelDiscovery: "openai",
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}
	if _, ok := handler.providerSetup().lookupModel("responses-model"); ok {
		t.Fatal("expected unfiltered single-provider routing to start without discovery")
	}

	modelsReq := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=x", nil)
	modelsW := httptest.NewRecorder()
	handler.HandleModels(modelsW, modelsReq)
	modelsResp := modelsW.Result()
	defer func() { _ = modelsResp.Body.Close() }()
	if modelsResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(modelsResp.Body)
		t.Fatalf("query variant models status = %d, want 200: %s", modelsResp.StatusCode, body)
	}
	if owner, ok := handler.providerSetup().lookupModel("responses-model"); !ok || owner.providerID != "dynamic" {
		t.Fatalf("canonical owner after query-first request = %+v, %v; want dynamic", owner, ok)
	}

	responsesReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"responses-model","input":"hello"}`))
	responsesReq.Header.Set("Content-Type", "application/json")
	responsesW := httptest.NewRecorder()
	handler.HandleResponses(responsesW, responsesReq)
	responsesResp := responsesW.Result()
	defer func() { _ = responsesResp.Body.Close() }()
	if responsesResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(responsesResp.Body)
		t.Fatalf("responses status = %d, want 200: %s", responsesResp.StatusCode, body)
	}
	if got := responsesHits.Load(); got != 1 {
		t.Fatalf("responses upstream hits = %d, want 1", got)
	}

	modelsMu.Lock()
	gotQueries := append([]string(nil), modelsQueries...)
	modelsMu.Unlock()
	if want := []string{"", "client_version=x"}; !reflect.DeepEqual(gotQueries, want) {
		t.Fatalf("upstream models queries = %v, want %v", gotQueries, want)
	}
}

func TestHandleModels_QueryVariantRefreshesExpiredCanonicalOwnership(t *testing.T) {
	var changed atomic.Bool
	var canonicalConditional atomic.Bool
	var modelsMu sync.Mutex
	modelsQueries := make([]string, 0, 3)
	var responsesHits atomic.Int32

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			modelsMu.Lock()
			modelsQueries = append(modelsQueries, r.URL.RawQuery)
			modelsMu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			if r.URL.RawQuery == "" {
				if !changed.Load() {
					w.Header().Set("ETag", `"catalog-a"`)
					_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-a","object":"model","owned_by":"dynamic","supported_endpoints":["/responses"]}]}`))
					return
				}
				if r.Header.Get("If-None-Match") == `"catalog-a"` {
					canonicalConditional.Store(true)
				}
				w.Header().Set("ETag", `"catalog-b"`)
				_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-b","object":"model","owned_by":"dynamic","supported_endpoints":["/responses"]}]}`))
				return
			}
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-b","object":"model","owned_by":"dynamic","supported_endpoints":["/responses"]}]}`))
		case "/responses":
			responsesHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","output":[]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:             "dynamic",
			Type:           "openai-compatible",
			Default:        true,
			BaseURL:        upstream.URL,
			AuthType:       "none",
			ModelDiscovery: "openai",
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	initialReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	initialW := httptest.NewRecorder()
	handler.HandleModels(initialW, initialReq)
	initialResp := initialW.Result()
	_ = initialResp.Body.Close()
	if initialResp.StatusCode != http.StatusOK {
		t.Fatalf("initial canonical status = %d, want 200", initialResp.StatusCode)
	}
	if _, ok := handler.providerSetup().lookupModel("model-a"); !ok {
		t.Fatal("expected model-a ownership after initial canonical build")
	}

	handler.models.mu.Lock()
	canonical := handler.models.entries[""]
	canonical.expiry = time.Now().Add(-time.Minute)
	handler.models.entries[""] = canonical
	handler.models.mu.Unlock()
	changed.Store(true)

	variantReq := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=x", nil)
	variantW := httptest.NewRecorder()
	handler.HandleModels(variantW, variantReq)
	variantResp := variantW.Result()
	defer func() { _ = variantResp.Body.Close() }()
	if variantResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(variantResp.Body)
		t.Fatalf("query variant status = %d, want 200: %s", variantResp.StatusCode, body)
	}
	if !canonicalConditional.Load() {
		t.Fatal("expired canonical refresh did not send its cached ETag")
	}
	if _, ok := handler.providerSetup().lookupModel("model-a"); ok {
		t.Fatal("model-a retained ownership after canonical catalog changed")
	}
	if owner, ok := handler.providerSetup().lookupModel("model-b"); !ok || owner.providerID != "dynamic" {
		t.Fatalf("model-b owner = %+v, %v; want dynamic", owner, ok)
	}

	modelBResp, err := handler.postResponsesWithHeaders(context.Background(), []byte(`{"model":"model-b","input":"hello"}`), nil)
	if err != nil {
		t.Fatalf("model-b responses error = %v", err)
	}
	_ = modelBResp.Body.Close()
	if _, err := handler.postResponsesWithHeaders(context.Background(), []byte(`{"model":"model-a","input":"hello"}`), nil); err == nil {
		t.Fatal("model-a responses error = nil, want local rejection after ownership replacement")
	}
	if got := responsesHits.Load(); got != 1 {
		t.Fatalf("responses upstream hits = %d, want only model-b", got)
	}

	modelsMu.Lock()
	gotQueries := append([]string(nil), modelsQueries...)
	modelsMu.Unlock()
	if want := []string{"", "", "client_version=x"}; !reflect.DeepEqual(gotQueries, want) {
		t.Fatalf("upstream models queries = %v, want %v", gotQueries, want)
	}
}

func TestHandleModels_QueryVariantCanonicalRefreshFailureUsesOnlyCachedFallback(t *testing.T) {
	for _, seedVariant := range []bool{false, true} {
		name := "without cached variant"
		if seedVariant {
			name = "with cached variant"
		}
		t.Run(name, func(t *testing.T) {
			var outage atomic.Bool
			var canonicalHits atomic.Int32
			var variantHits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/models" {
					w.WriteHeader(http.StatusNotFound)
					return
				}
				if r.URL.RawQuery == "" {
					canonicalHits.Add(1)
					if outage.Load() {
						w.WriteHeader(http.StatusServiceUnavailable)
						_, _ = w.Write([]byte(`{"error":"catalog unavailable"}`))
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-a","object":"model","owned_by":"dynamic","supported_endpoints":["/responses"]}]}`))
					return
				}
				variantHits.Add(1)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"variant-model","object":"model","owned_by":"dynamic","supported_endpoints":["/responses"]}]}`))
			}))
			defer upstream.Close()

			handler, err := NewProxyHandler(
				auth.NewTestAuthenticator("test-token"),
				logger.New(logger.LevelInfo),
				WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
					ID:             "dynamic",
					Type:           "openai-compatible",
					Default:        true,
					BaseURL:        upstream.URL,
					AuthType:       "none",
					ModelDiscovery: "openai",
				}}}),
			)
			if err != nil {
				t.Fatalf("NewProxyHandler returned error: %v", err)
			}
			handler.maxRetries = 1

			requestModels := func(target string) *http.Response {
				t.Helper()
				req := httptest.NewRequest(http.MethodGet, target, nil)
				w := httptest.NewRecorder()
				handler.HandleModels(w, req)
				return w.Result()
			}

			initial := requestModels("/v1/models")
			_ = initial.Body.Close()
			if initial.StatusCode != http.StatusOK {
				t.Fatalf("initial canonical status = %d, want 200", initial.StatusCode)
			}
			if seedVariant {
				variant := requestModels("/v1/models?client_version=x")
				_ = variant.Body.Close()
				if variant.StatusCode != http.StatusOK {
					t.Fatalf("seed variant status = %d, want 200", variant.StatusCode)
				}
			}

			handler.models.mu.Lock()
			canonical := handler.models.entries[""]
			canonical.expiry = time.Now().Add(-time.Minute)
			handler.models.entries[""] = canonical
			handler.models.mu.Unlock()
			outage.Store(true)

			resp := requestModels("/v1/models?client_version=x")
			defer func() { _ = resp.Body.Close() }()
			if seedVariant {
				if resp.StatusCode != http.StatusOK {
					body, _ := io.ReadAll(resp.Body)
					t.Fatalf("cached variant fallback status = %d, want 200: %s", resp.StatusCode, body)
				}
				var result struct {
					Data []struct {
						ID string `json:"id"`
					} `json:"data"`
				}
				if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
					t.Fatalf("decode cached variant fallback: %v", err)
				}
				if len(result.Data) != 1 || result.Data[0].ID != "variant-model" {
					t.Fatalf("cached variant fallback = %+v, want variant-model", result.Data)
				}
			} else if resp.StatusCode != http.StatusServiceUnavailable {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("uncached variant status = %d, want 503: %s", resp.StatusCode, body)
			}

			if got := canonicalHits.Load(); got != 2 {
				t.Fatalf("canonical hits = %d, want initial build plus failed refresh", got)
			}
			wantVariantHits := int32(0)
			if seedVariant {
				wantVariantHits = 1
			}
			if got := variantHits.Load(); got != wantVariantHits {
				t.Fatalf("variant hits = %d, want %d; failed canonical refresh must not fetch a new variant", got, wantVariantHits)
			}
			if _, ok := handler.providerSetup().lookupModel("model-a"); !ok {
				t.Fatal("stale canonical routing for model-a was lost after refresh failure")
			}
		})
	}
}

func TestHandleModels_QueryVariantRefreshDoesNotMixCanonicalStateFrom304(t *testing.T) {
	var firstProviderCalls atomic.Int32
	var secondProviderCalls atomic.Int32
	var firstProviderConditional atomic.Bool
	var secondProviderConditional atomic.Bool

	firstProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := firstProviderCalls.Add(1)
		if r.URL.RawQuery == "" {
			w.Header().Set("ETag", `"canonical-first"`)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"first-a","object":"model","owned_by":"first"},{"id":"first-canonical-only","object":"model","owned_by":"first"}]}`))
			return
		}

		if call >= 3 && r.Header.Get("If-None-Match") != "" {
			firstProviderConditional.Store(true)
			w.WriteHeader(http.StatusNotModified)
			return
		}
		w.Header().Set("ETag", fmt.Sprintf(`"variant-first-%d"`, call))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"first-a","object":"model","owned_by":"first"}]}`))
	}))
	defer firstProvider.Close()

	secondProvider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := secondProviderCalls.Add(1)
		if r.URL.RawQuery == "" {
			w.Header().Set("ETag", `"canonical-second"`)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"second-a","object":"model","owned_by":"second"},{"id":"second-canonical-only","object":"model","owned_by":"second"}]}`))
			return
		}

		if call >= 3 && r.Header.Get("If-None-Match") != "" {
			secondProviderConditional.Store(true)
		}
		modelID := "second-a"
		if call >= 3 {
			modelID = "second-b"
		}
		w.Header().Set("ETag", fmt.Sprintf(`"variant-second-%d"`, call))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"object":"list","data":[{"id":%q,"object":"model","owned_by":"second"}]}`, modelID)
	}))
	defer secondProvider.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
			{
				ID:             "first",
				Type:           "openai-compatible",
				Default:        true,
				BaseURL:        firstProvider.URL,
				AuthType:       "none",
				ModelDiscovery: "openai",
			},
			{
				ID:             "second",
				Type:           "openai-compatible",
				BaseURL:        secondProvider.URL,
				AuthType:       "none",
				ModelDiscovery: "openai",
			},
		}}),
		WithDeferredDynamicProviderModelValidation(true),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	readIDs := func(target string) map[string]bool {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		w := httptest.NewRecorder()
		handler.HandleModels(w, req)
		resp := w.Result()
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(resp.Body)
			t.Fatalf("HandleModels(%q) status = %d, want 200: %s", target, resp.StatusCode, body)
		}
		var result struct {
			Data []struct {
				ID string `json:"id"`
			} `json:"data"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode HandleModels(%q): %v", target, err)
		}
		ids := make(map[string]bool, len(result.Data))
		for _, model := range result.Data {
			ids[model.ID] = true
		}
		return ids
	}

	canonical := readIDs("/v1/models")
	if !canonical["first-canonical-only"] || !canonical["second-canonical-only"] {
		t.Fatalf("canonical catalog = %v, want both canonical-only models", canonical)
	}
	firstVariant := readIDs("/v1/models?view=partial")
	if !firstVariant["first-a"] || !firstVariant["second-a"] || firstVariant["first-canonical-only"] {
		t.Fatalf("first variant catalog = %v, want first-a and second-a only", firstVariant)
	}
	secondVariant := readIDs("/v1/models?view=partial")
	if !secondVariant["first-a"] || !secondVariant["second-b"] || secondVariant["first-canonical-only"] {
		t.Fatalf("refreshed variant catalog = %v, want fresh first-a and second-b without canonical-only entries", secondVariant)
	}
	if firstProviderConditional.Load() || secondProviderConditional.Load() {
		t.Fatalf("query variant sent conditional ETag headers: first=%v second=%v", firstProviderConditional.Load(), secondProviderConditional.Load())
	}
}

func TestHandleModels_BoundsAndPrunesQueryCache(t *testing.T) {
	const queryCount = modelsCacheMaxEntries * 4

	var upstreamHits atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-a","object":"model","owned_by":"copilot"}]}`))
	})
	handler.models.entries = map[string]cachedModelsResponse{
		"expired=true": {
			body:       []byte(`{"object":"list","data":[]}`),
			statusCode: http.StatusOK,
			expiry:     time.Now().Add(-time.Minute),
		},
	}

	for i := 0; i < queryCount; i++ {
		req := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/v1/models?caller=%d", i), nil)
		w := httptest.NewRecorder()
		handler.HandleModels(w, req)
		resp := w.Result()
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("query %d status = %d, want 200", i, resp.StatusCode)
		}
	}

	handler.models.mu.RLock()
	cacheEntries := len(handler.models.entries)
	_, keptExpired := handler.models.entries["expired=true"]
	handler.models.mu.RUnlock()

	if cacheEntries != modelsCacheMaxEntries {
		t.Fatalf("models cache entries = %d after %d unique queries, want bounded at %d", cacheEntries, queryCount, modelsCacheMaxEntries)
	}
	if keptExpired {
		t.Fatal("models cache retained expired query entry, want it pruned")
	}
	if got, want := upstreamHits.Load(), int32(queryCount+1); got != want {
		t.Fatalf("upstream /models hits = %d, want %d (one canonical seed plus unique query responses)", got, want)
	}
}

func TestHandleModels_VariantCacheWritePreservesExpiredCanonicalForStaleFallback(t *testing.T) {
	var canonicalHits atomic.Int32
	var variantHits atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			variantHits.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"variant-model","object":"model","owned_by":"copilot"}]}`))
			return
		}

		if canonicalHits.Add(1) == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"stale-canonical","object":"model","owned_by":"copilot"}]}`))
			return
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"catalog unavailable"}`))
	})
	handler.maxRetries = 1

	requestModels := func(target string) *http.Response {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, target, nil)
		w := httptest.NewRecorder()
		handler.HandleModels(w, req)
		return w.Result()
	}

	initial := requestModels("/v1/models")
	if initial.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(initial.Body)
		_ = initial.Body.Close()
		t.Fatalf("initial canonical status = %d, want 200: %s", initial.StatusCode, body)
	}
	_ = initial.Body.Close()

	handler.models.mu.Lock()
	canonicalEntry, ok := handler.models.entries[""]
	if !ok {
		handler.models.mu.Unlock()
		t.Fatal("canonical cache entry missing after initial request")
	}
	canonicalEntry.expiry = time.Now().Add(-time.Minute)
	handler.models.entries[""] = canonicalEntry
	handler.models.mu.Unlock()

	handler.storeModelsCacheEntry("view=partial", cachedModelsResponse{
		body:       []byte(`{"object":"list","data":[{"id":"variant-model","object":"model","owned_by":"copilot"}]}`),
		statusCode: http.StatusOK,
		expiry:     time.Now().Add(modelsCacheTTL),
	})

	stale := requestModels("/v1/models")
	defer func() { _ = stale.Body.Close() }()
	if stale.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(stale.Body)
		t.Fatalf("canonical outage status = %d, want stale 200 response: %s", stale.StatusCode, body)
	}
	var result struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(stale.Body).Decode(&result); err != nil {
		t.Fatalf("decode stale canonical response: %v", err)
	}
	if len(result.Data) != 1 || result.Data[0].ID != "stale-canonical" {
		t.Fatalf("stale canonical response = %+v, want stale-canonical", result.Data)
	}
	if got := canonicalHits.Load(); got != 2 {
		t.Fatalf("canonical upstream hits = %d, want initial success plus outage", got)
	}
	if got := variantHits.Load(); got != 0 {
		t.Fatalf("variant upstream hits = %d, want 0 for direct cache write", got)
	}
}

func TestHandleModels_CoalescesIdenticalQueryVariantMissForCanceledWaiter(t *testing.T) {
	var variantHits atomic.Int32
	variantStarted := make(chan struct{})
	releaseVariant := make(chan struct{})

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.RawQuery == "" {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"canonical-model","object":"model","owned_by":"copilot"}]}`))
			return
		}

		call := variantHits.Add(1)
		if call == 1 {
			close(variantStarted)
			<-releaseVariant
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"variant-model","object":"model","owned_by":"copilot"}]}`))
	})

	seedReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	seedW := httptest.NewRecorder()
	handler.HandleModels(seedW, seedReq)
	seedResp := seedW.Result()
	_ = seedResp.Body.Close()
	if seedResp.StatusCode != http.StatusOK {
		t.Fatalf("canonical seed status = %d, want 200", seedResp.StatusCode)
	}

	leaderDone := make(chan *http.Response, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=x", nil)
		w := httptest.NewRecorder()
		handler.HandleModels(w, req)
		leaderDone <- w.Result()
	}()

	select {
	case <-variantStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for query-variant leader")
	}

	waiterCtx, cancelWaiter := context.WithCancel(context.Background())
	cancelWaiter()
	waiterReq := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=x", nil).WithContext(waiterCtx)
	waiterW := httptest.NewRecorder()
	handler.HandleModels(waiterW, waiterReq)

	if got := variantHits.Load(); got != 1 {
		close(releaseVariant)
		t.Fatalf("query-variant upstream hits before leader release = %d, want 1 shared fetch", got)
	}
	if got := waiterW.Body.String(); got != "" {
		close(releaseVariant)
		t.Fatalf("canceled waiter wrote response body %q, want no response", got)
	}

	close(releaseVariant)
	select {
	case resp := <-leaderDone:
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("query-variant leader status = %d, want 200", resp.StatusCode)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for query-variant leader")
	}
}

func TestHandleModels_CoalescesConcurrentIdenticalQueryVariantMisses(t *testing.T) {
	const waiters = 12

	var variantHits atomic.Int32
	variantStarted := make(chan struct{})
	releaseVariant := make(chan struct{})
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.RawQuery == "" {
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"canonical-model","object":"model","owned_by":"copilot"}]}`))
			return
		}

		if call := variantHits.Add(1); call == 1 {
			close(variantStarted)
			<-releaseVariant
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"variant-model","object":"model","owned_by":"copilot"}]}`))
	})

	seedReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	seedW := httptest.NewRecorder()
	handler.HandleModels(seedW, seedReq)
	seedResp := seedW.Result()
	_ = seedResp.Body.Close()
	if seedResp.StatusCode != http.StatusOK {
		t.Fatalf("canonical seed status = %d, want 200", seedResp.StatusCode)
	}

	responses := make(chan *http.Response, waiters+1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=x", nil)
		w := httptest.NewRecorder()
		handler.HandleModels(w, req)
		responses <- w.Result()
	}()
	select {
	case <-variantStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for query-variant leader")
	}

	observed := make([]chan struct{}, 0, waiters)
	for range waiters {
		waitObserved := make(chan struct{})
		observed = append(observed, waitObserved)
		go func() {
			ctx := &observedDoneContext{Context: context.Background(), observed: waitObserved}
			req := httptest.NewRequest(http.MethodGet, "/v1/models?client_version=x", nil).WithContext(ctx)
			w := httptest.NewRecorder()
			handler.HandleModels(w, req)
			responses <- w.Result()
		}()
	}
	for i, joined := range observed {
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			close(releaseVariant)
			t.Fatalf("waiter %d did not join the query-variant flight", i)
		}
	}
	if got := variantHits.Load(); got != 1 {
		close(releaseVariant)
		t.Fatalf("query-variant upstream hits while waiters joined = %d, want 1", got)
	}

	close(releaseVariant)
	for i := 0; i < waiters+1; i++ {
		select {
		case resp := <-responses:
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"variant-model"`)) {
				t.Fatalf("query-variant caller %d response = status %d body %s", i, resp.StatusCode, body)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for query-variant caller %d", i)
		}
	}
	if got := variantHits.Load(); got != 1 {
		t.Fatalf("query-variant upstream hits after completion = %d, want 1", got)
	}
}

func TestRefreshModelsCacheVariantStartsFreshLifecycleDeadline(t *testing.T) {
	remaining := make(chan time.Duration, 1)
	handler := newTestProxyHandler(t, func(http.ResponseWriter, *http.Request) {})
	handler.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		deadline, ok := req.Context().Deadline()
		if !ok {
			return nil, errors.New("variant upstream request has no deadline")
		}
		remaining <- time.Until(deadline)
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"object":"list","data":[{"id":"fresh-deadline-model","object":"model","owned_by":"copilot"}]}`)),
			Request:    req,
		}, nil
	})}

	result := handler.refreshModelsCacheVariant(context.Background(), "client_version=fresh-deadline")
	if result.err != nil {
		t.Fatalf("refreshModelsCacheVariant() error = %v", result.err)
	}
	if !result.hasEntry || result.entry.statusCode != http.StatusOK {
		t.Fatalf("refresh result = %+v, want cached 200 entry", result)
	}
	select {
	case got := <-remaining:
		if got < modelsUpstreamTimeout-time.Second {
			t.Fatalf("variant upstream deadline remaining = %s, want fresh %s timeout", got, modelsUpstreamTimeout)
		}
	case <-time.After(time.Second):
		t.Fatal("variant upstream request was not observed")
	}
}

func TestRefreshModelsCacheVariantPreservesLifecycleShutdownCause(t *testing.T) {
	started := make(chan struct{})
	handler := newTestProxyHandler(t, func(http.ResponseWriter, *http.Request) {})
	handler.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	handler.maxRetries = 1

	done := make(chan modelsCacheFlightResult, 1)
	go func() {
		done <- handler.refreshModelsCacheVariant(context.Background(), "client_version=shutdown-cause")
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("variant refresh did not start")
	}
	handler.BeginShutdown()

	select {
	case result := <-done:
		if !errors.Is(result.err, errProxyLifecycleShutdown) {
			t.Fatalf("variant refresh error = %v, want lifecycle shutdown cause", result.err)
		}
	case <-time.After(time.Second):
		t.Fatal("variant refresh did not return after shutdown")
	}
}

func TestHandleModels_CoalescesExpiredCanonicalFailureAndBacksOff(t *testing.T) {
	const callers = 16

	now := time.Date(2026, time.July, 11, 12, 0, 0, 0, time.UTC)
	var canonicalHits atomic.Int32
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.RawQuery != "" {
			t.Fatalf("unexpected query variant during canonical refresh: %q", r.URL.RawQuery)
		}

		call := canonicalHits.Add(1)
		if call == 1 {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"stale-model","object":"model","owned_by":"copilot"}]}`))
			return
		}
		if call == 2 {
			close(refreshStarted)
			<-releaseRefresh
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":"catalog unavailable"}`))
	})
	handler.maxRetries = 1
	handler.models.now = func() time.Time { return now }

	seedReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	seedW := httptest.NewRecorder()
	handler.HandleModels(seedW, seedReq)
	seedResp := seedW.Result()
	_ = seedResp.Body.Close()
	if seedResp.StatusCode != http.StatusOK {
		t.Fatalf("canonical seed status = %d, want 200", seedResp.StatusCode)
	}

	now = now.Add(modelsCacheTTL + time.Nanosecond)

	results := make(chan *http.Response, callers)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()
		handler.HandleModels(w, req)
		results <- w.Result()
	}()

	select {
	case <-refreshStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for failed canonical refresh")
	}

	observed := make([]chan struct{}, 0, callers-1)
	for range callers - 1 {
		waitObserved := make(chan struct{})
		observed = append(observed, waitObserved)
		go func() {
			ctx := &observedDoneContext{Context: context.Background(), observed: waitObserved}
			req := httptest.NewRequest(http.MethodGet, "/v1/models", nil).WithContext(ctx)
			w := httptest.NewRecorder()
			handler.HandleModels(w, req)
			results <- w.Result()
		}()
	}
	for i, joined := range observed {
		select {
		case <-joined:
		case <-time.After(5 * time.Second):
			close(releaseRefresh)
			t.Fatalf("canonical waiter %d did not join the refresh", i)
		}
	}
	close(releaseRefresh)

	for i := 0; i < callers; i++ {
		select {
		case resp := <-results:
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK || !bytes.Contains(body, []byte(`"stale-model"`)) {
				t.Fatalf("caller %d stale response = status %d body %s; want stale 200", i, resp.StatusCode, body)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for canonical caller %d", i)
		}
	}

	if got := canonicalHits.Load(); got != 2 {
		t.Fatalf("canonical upstream hits after concurrent failure = %d, want seed plus one shared refresh", got)
	}

	backoffReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	backoffW := httptest.NewRecorder()
	handler.HandleModels(backoffW, backoffReq)
	backoffResp := backoffW.Result()
	defer func() { _ = backoffResp.Body.Close() }()
	if backoffResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(backoffResp.Body)
		t.Fatalf("backoff stale status = %d, want 200: %s", backoffResp.StatusCode, body)
	}
	if got := canonicalHits.Load(); got != 2 {
		t.Fatalf("canonical upstream hits during failure backoff = %d, want no additional refresh", got)
	}

	now = now.Add(modelsCacheFailureBackoff + time.Nanosecond)
	retryReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	retryW := httptest.NewRecorder()
	handler.HandleModels(retryW, retryReq)
	retryResp := retryW.Result()
	defer func() { _ = retryResp.Body.Close() }()
	if retryResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(retryResp.Body)
		t.Fatalf("post-backoff stale status = %d, want 200: %s", retryResp.StatusCode, body)
	}
	if got := canonicalHits.Load(); got != 3 {
		t.Fatalf("canonical upstream hits after bounded backoff = %d, want one retry", got)
	}
}

func TestHandleModels_ConcurrentCanonicalAndVariantRefreshKeepsCanonicalRouting(t *testing.T) {
	variantStarted := make(chan struct{})
	releaseVariant := make(chan struct{})
	var startOnce sync.Once
	var modelsHits atomic.Int32

	dynamicServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelsHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if r.URL.RawQuery == "view=partial" {
			startOnce.Do(func() { close(variantStarted) })
			<-releaseVariant
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-a","object":"model","owned_by":"dynamic"}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"model-a","object":"model","owned_by":"dynamic"},{"id":"model-b","object":"model","owned_by":"dynamic"}]}`))
	}))
	defer dynamicServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
			{
				ID:             "dynamic",
				Type:           "openai-compatible",
				BaseURL:        dynamicServer.URL,
				AuthType:       "none",
				ModelDiscovery: "openai",
			},
			{
				ID:             "fallback",
				Type:           "openai-compatible",
				Default:        true,
				BaseURL:        "http://fallback.invalid",
				AuthType:       "none",
				ModelDiscovery: "static",
				Models:         []ProviderModelConfig{{PublicID: "fallback-only"}},
			},
		}}),
		WithDeferredDynamicProviderModelValidation(true),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	variantDone := make(chan int, 1)
	go func() {
		req := httptest.NewRequest(http.MethodGet, "/v1/models?view=partial", nil)
		w := httptest.NewRecorder()
		handler.HandleModels(w, req)
		resp := w.Result()
		variantDone <- resp.StatusCode
		_ = resp.Body.Close()
	}()

	select {
	case <-variantStarted:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for variant refresh to start")
	}

	canonicalReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	canonicalW := httptest.NewRecorder()
	handler.HandleModels(canonicalW, canonicalReq)
	canonicalResp := canonicalW.Result()
	defer func() { _ = canonicalResp.Body.Close() }()
	if canonicalResp.StatusCode != http.StatusOK {
		close(releaseVariant)
		t.Fatalf("canonical refresh status = %d, want 200", canonicalResp.StatusCode)
	}
	close(releaseVariant)

	select {
	case status := <-variantDone:
		if status != http.StatusOK {
			t.Fatalf("variant refresh status = %d, want 200", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for variant refresh to finish")
	}

	owner, ok := handler.providerSetup().lookupModel("model-b")
	if !ok || owner.providerID != "dynamic" {
		t.Fatalf("routing owner for model-b after concurrent refreshes = %+v, %v; want canonical dynamic owner", owner, ok)
	}
	if got := modelsHits.Load(); got != 2 {
		t.Fatalf("dynamic /models hits = %d, want 2", got)
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

func TestReadProviderModelCatalogBodyBoundsDecodedBytes(t *testing.T) {
	exact := paddedProviderModelCatalog(t, maxProviderModelCatalogBodySize, "model-exact")
	body, err := readProviderModelCatalogBody(bytes.NewReader(exact))
	if err != nil {
		t.Fatalf("exact-limit catalog error = %v", err)
	}
	if len(body) != maxProviderModelCatalogBodySize {
		t.Fatalf("exact-limit catalog bytes = %d, want %d", len(body), maxProviderModelCatalogBodySize)
	}

	oversized := paddedProviderModelCatalog(t, maxProviderModelCatalogBodySize+1, "model-too-large")
	if _, err := readProviderModelCatalogBody(bytes.NewReader(oversized)); !errors.Is(err, errProviderModelCatalogTooLarge) {
		t.Fatalf("oversized catalog error = %v, want %v", err, errProviderModelCatalogTooLarge)
	}
}

func TestFetchProviderModelsRejectsOversizedSuccessfulCatalogs(t *testing.T) {
	oversized := paddedProviderModelCatalog(t, maxProviderModelCatalogBodySize+1, "oversized-model")
	codexAuthPath := writeTestOpenAICodexAuth(
		t,
		t.TempDir(),
		testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-size-test", false, "refresh-token"),
	)

	tests := []struct {
		name     string
		provider *providerRuntime
	}{
		{
			name: "Copilot",
			provider: &providerRuntime{
				id:      "copilot",
				kind:    providerTypeCopilot,
				baseURL: "http://upstream.test",
			},
		},
		{
			name: "OpenAI Codex",
			provider: &providerRuntime{
				id:        "codex",
				kind:      providerTypeOpenAICodex,
				baseURL:   "http://upstream.test",
				codexAuth: &openAICodexAuth{path: codexAuthPath},
			},
		},
		{
			name: "generic OpenAI discovery",
			provider: &providerRuntime{
				id:             "generic",
				kind:           providerTypeOpenAICompatible,
				baseURL:        "http://upstream.test",
				authType:       providerAuthTypeNone,
				modelDiscovery: providerModelDiscoveryOpenAI,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := newRoundTripTestProxyHandler(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
				return &http.Response{
					StatusCode: http.StatusOK,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader(oversized)),
				}, nil
			}))

			result, err := handler.fetchProviderModels(context.Background(), tt.provider, "", "")
			if !errors.Is(err, errProviderModelCatalogTooLarge) {
				t.Fatalf("fetchProviderModels() error = %v, want %v", err, errProviderModelCatalogTooLarge)
			}
			if len(result.models) != 0 || result.etag != "" || result.notModified {
				t.Fatalf("oversized catalog result = %+v, want no accepted provider state", result)
			}
		})
	}
}

func TestFetchProviderModelsAzureOversizedOverlayKeepsStaticCatalog(t *testing.T) {
	provider, err := buildProviderRuntime(ProviderConfig{
		ID:       "azure",
		Type:     "azure-openai",
		BaseURL:  "https://example.openai.azure.com/openai/v1",
		AuthMode: "api_key",
		APIKey:   "test-key",
		Models: []ProviderModelConfig{{
			PublicID:   "gpt-static",
			Deployment: "gpt-static-deployment",
			Name:       "Configured Static Name",
		}},
	}, "", nil)
	if err != nil {
		t.Fatalf("buildProviderRuntime() error = %v", err)
	}

	oversized := paddedProviderModelCatalog(t, maxProviderModelCatalogBodySize+1, "gpt-static")
	handler := newRoundTripTestProxyHandler(t, roundTripFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader(oversized)),
		}, nil
	}))

	result, err := handler.fetchProviderModels(context.Background(), provider, "", "")
	if err != nil {
		t.Fatalf("fetchProviderModels() error = %v, want best-effort static fallback", err)
	}
	if len(result.models) != 1 || result.models[0].publicID != "gpt-static" {
		t.Fatalf("Azure oversized overlay models = %+v, want configured static catalog", result.models)
	}
	if bytes.Contains(result.models[0].raw, []byte("Oversized metadata")) {
		t.Fatalf("Azure oversized overlay mutated static metadata: %s", result.models[0].raw)
	}
}

func TestHandleModelsRejectsCompressedOversizedCatalogWithoutInstallingState(t *testing.T) {
	oversized := paddedProviderModelCatalog(t, maxProviderModelCatalogBodySize+1, "oversized-model")
	var acceptedGzip atomic.Bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		acceptedGzip.Store(strings.Contains(r.Header.Get("Accept-Encoding"), "gzip"))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		gz := gzip.NewWriter(w)
		_, _ = gz.Write(oversized)
		_ = gz.Close()
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:             "dynamic",
			Type:           "openai-compatible",
			Default:        true,
			BaseURL:        upstream.URL,
			AuthType:       "none",
			ModelDiscovery: "openai",
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	provider := handler.providerSetup().defaultProvider()
	if _, err := handler.fetchProviderModels(context.Background(), provider, "", ""); !errors.Is(err, errProviderModelCatalogTooLarge) {
		t.Fatalf("compressed fetchProviderModels() error = %v, want decoded %v", err, errProviderModelCatalogTooLarge)
	}
	if !acceptedGzip.Load() {
		t.Fatal("test transport did not negotiate gzip, so decompressed-size limit was not exercised")
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	handler.HandleModels(w, req)
	resp := w.Result()
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("compressed oversized catalog status = %d, want 502: %s", resp.StatusCode, body)
	}
	if _, ok := handler.providerSetup().lookupModel("oversized-model"); ok {
		t.Fatal("oversized compressed catalog installed dynamic routing ownership")
	}
	handler.models.mu.RLock()
	_, cached := handler.models.entries[""]
	handler.models.mu.RUnlock()
	if cached {
		t.Fatal("oversized compressed catalog installed a canonical cache entry")
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

	t.Run("maps Copilot prompt cap to Codex context window", func(t *testing.T) {
		h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/models" {
				t.Errorf("expected path /models, got %s", r.URL.Path)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.1-codex-max","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/responses"],"capabilities":{"supports":{"parallel_tool_calls":true,"vision":true,"reasoning_effort":["medium"]},"limits":{"max_context_window_tokens":400000,"max_prompt_tokens":128000,"max_output_tokens":128000}},"model_picker_enabled":true,"model_picker_category":"powerful","name":"GPT-5.1-Codex-Max"}]}`))
		})
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()

		h.HandleModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		defer func() { _ = resp.Body.Close() }()

		var result struct {
			Models []struct {
				Slug             string `json:"slug"`
				ContextWindow    *int64 `json:"context_window,omitempty"`
				MaxContextWindow *int64 `json:"max_context_window,omitempty"`
			} `json:"models"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(result.Models) != 1 {
			t.Fatalf("expected 1 model, got %d", len(result.Models))
		}
		model := result.Models[0]
		if model.Slug != "gpt-5.1-codex-max" {
			t.Fatalf("slug = %q, want gpt-5.1-codex-max", model.Slug)
		}
		if model.ContextWindow == nil || *model.ContextWindow != 128000 {
			t.Fatalf("context_window = %v, want prompt cap 128000", model.ContextWindow)
		}
		if model.MaxContextWindow == nil || *model.MaxContextWindow != 400000 {
			t.Fatalf("max_context_window = %v, want total context 400000", model.MaxContextWindow)
		}
	})

	t.Run("falls back to Copilot total window when prompt cap is absent", func(t *testing.T) {
		h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.4","object":"model","created":0,"owned_by":"github-copilot","supported_endpoints":["/responses"],"capabilities":{"supports":{"parallel_tool_calls":true,"vision":true,"reasoning_effort":["medium"]},"limits":{"max_context_window_tokens":400000}},"model_picker_enabled":true,"model_picker_category":"powerful","name":"GPT-5.4"}]}`))
		})
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		w := httptest.NewRecorder()

		h.HandleModels(w, req)

		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("expected 200, got %d", resp.StatusCode)
		}
		defer func() { _ = resp.Body.Close() }()

		var result struct {
			Models []struct {
				ContextWindow    *int64 `json:"context_window,omitempty"`
				MaxContextWindow *int64 `json:"max_context_window,omitempty"`
			} `json:"models"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		if len(result.Models) != 1 {
			t.Fatalf("expected 1 model, got %d", len(result.Models))
		}
		if result.Models[0].ContextWindow == nil || *result.Models[0].ContextWindow != 400000 {
			t.Fatalf("context_window = %v, want fallback total context 400000", result.Models[0].ContextWindow)
		}
		if result.Models[0].MaxContextWindow == nil || *result.Models[0].MaxContextWindow != 400000 {
			t.Fatalf("max_context_window = %v, want total context 400000", result.Models[0].MaxContextWindow)
		}
	})

	t.Run("upstream error is forwarded", func(t *testing.T) {
		h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte(`{"error":"internal server error"}`))
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

func TestTransformModelsResponsePreservesCodexContextMetadata(t *testing.T) {
	contextWindow := int64(272000)
	maxContextWindow := int64(1000000)
	autoCompactTokenLimit := int64(244800)
	effectiveContextWindowPercent := int64(90)

	body := []byte(`{"object":"list","data":[{"id":"gpt-5.4","object":"model","created":0,"owned_by":"codex","supported_endpoints":["/responses"],"capabilities":{"supports":{"parallel_tool_calls":true,"vision":true,"reasoning_effort":["medium"]},"limits":{"max_context_window_tokens":400000}},"model_picker_enabled":true,"model_picker_category":"powerful","name":"GPT-5.4","context_window":272000,"max_context_window":1000000,"auto_compact_token_limit":244800,"effective_context_window_percent":90}]}`)

	var result struct {
		Models []struct {
			Slug                          string `json:"slug"`
			ContextWindow                 *int64 `json:"context_window,omitempty"`
			MaxContextWindow              *int64 `json:"max_context_window,omitempty"`
			AutoCompactTokenLimit         *int64 `json:"auto_compact_token_limit,omitempty"`
			EffectiveContextWindowPercent int64  `json:"effective_context_window_percent"`
		} `json:"models"`
	}
	if err := json.Unmarshal(transformModelsResponse(body), &result); err != nil {
		t.Fatalf("decode transformed models response: %v", err)
	}
	if len(result.Models) != 1 {
		t.Fatalf("expected 1 model entry, got %d", len(result.Models))
	}
	model := result.Models[0]
	if model.Slug != "gpt-5.4" {
		t.Fatalf("expected slug gpt-5.4, got %q", model.Slug)
	}
	if model.ContextWindow == nil || *model.ContextWindow != contextWindow {
		t.Fatalf("context_window = %v, want %d", model.ContextWindow, contextWindow)
	}
	if model.MaxContextWindow == nil || *model.MaxContextWindow != maxContextWindow {
		t.Fatalf("max_context_window = %v, want %d", model.MaxContextWindow, maxContextWindow)
	}
	if model.AutoCompactTokenLimit == nil || *model.AutoCompactTokenLimit != autoCompactTokenLimit {
		t.Fatalf("auto_compact_token_limit = %v, want %d", model.AutoCompactTokenLimit, autoCompactTokenLimit)
	}
	if model.EffectiveContextWindowPercent != effectiveContextWindowPercent {
		t.Fatalf("effective_context_window_percent = %d, want %d", model.EffectiveContextWindowPercent, effectiveContextWindowPercent)
	}
}

func TestHandleModels_OpenAICodexPreservesContextMetadata(t *testing.T) {
	codexHome := t.TempDir()
	writeTestOpenAICodexAuth(t, codexHome, testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-123", false, "refresh-token"))
	t.Setenv("CODEX_HOME", codexHome)

	codexServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/models" {
			t.Fatalf("expected /models lookup, got %s", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"models":[{"slug":"gpt-5.4","display_name":"GPT-5.4","visibility":"list","supported_in_api":true,"supported_reasoning_levels":[{"effort":"medium"}],"supports_parallel_tool_calls":true,"context_window":272000,"max_context_window":1000000,"auto_compact_token_limit":244800,"effective_context_window_percent":90,"input_modalities":["text","image"],"priority":0}]}`))
	}))
	defer codexServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:            "codex",
				Type:          "openai-codex",
				BaseURL:       codexServer.URL,
				Default:       true,
				IncludeModels: []string{"gpt-5.4"},
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
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var result struct {
		Data []struct {
			ID           string `json:"id"`
			Capabilities struct {
				Limits struct {
					MaxContextWindowTokens int64 `json:"max_context_window_tokens"`
				} `json:"limits"`
			} `json:"capabilities"`
		} `json:"data"`
		Models []struct {
			Slug                          string `json:"slug"`
			ContextWindow                 *int64 `json:"context_window,omitempty"`
			MaxContextWindow              *int64 `json:"max_context_window,omitempty"`
			AutoCompactTokenLimit         *int64 `json:"auto_compact_token_limit,omitempty"`
			EffectiveContextWindowPercent int64  `json:"effective_context_window_percent"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if len(result.Data) != 1 || len(result.Models) != 1 {
		t.Fatalf("expected one model, got data=%d models=%d", len(result.Data), len(result.Models))
	}
	if result.Data[0].Capabilities.Limits.MaxContextWindowTokens != 1000000 {
		t.Fatalf("max_context_window_tokens = %d, want 1000000", result.Data[0].Capabilities.Limits.MaxContextWindowTokens)
	}
	model := result.Models[0]
	if model.ContextWindow == nil || *model.ContextWindow != 272000 {
		t.Fatalf("context_window = %v, want 272000", model.ContextWindow)
	}
	if model.MaxContextWindow == nil || *model.MaxContextWindow != 1000000 {
		t.Fatalf("max_context_window = %v, want 1000000", model.MaxContextWindow)
	}
	if model.AutoCompactTokenLimit == nil || *model.AutoCompactTokenLimit != 244800 {
		t.Fatalf("auto_compact_token_limit = %v, want 244800", model.AutoCompactTokenLimit)
	}
	if model.EffectiveContextWindowPercent != 90 {
		t.Fatalf("effective_context_window_percent = %d, want 90", model.EffectiveContextWindowPercent)
	}
}

func TestHandleModels_ForwardsQueryAndETag(t *testing.T) {
	var queries []string
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		queries = append(queries, r.URL.RawQuery)
		if got := r.Header.Get("If-None-Match"); got != "" {
			t.Errorf("expected no If-None-Match while seeding canonical/query responses, got %q", got)
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
	if want := []string{"", "client_version=0.99.0"}; !reflect.DeepEqual(queries, want) {
		t.Fatalf("upstream queries = %v, want %v", queries, want)
	}
}

func TestHandleModels_QueryVariantRefreshesWithoutConditionalETag(t *testing.T) {
	requestCount := 0
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		requestCount++

		w.Header().Set("Content-Type", "application/json")
		switch requestCount {
		case 1:
			if got := r.URL.RawQuery; got != "" {
				t.Errorf("canonical seed query = %q, want empty", got)
			}
			if got := r.Header.Get("If-None-Match"); got != "" {
				t.Errorf("expected no If-None-Match on canonical seed, got %q", got)
			}
			w.Header().Set("ETag", `"canonical-etag"`)
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"canonical-model","object":"model","created":0,"owned_by":"github-copilot","name":"Canonical"}]}`))
		case 2:
			if got := r.URL.RawQuery; got != "client_version=0.99.0" {
				t.Errorf("first variant query = %q, want client_version=0.99.0", got)
			}
			if got := r.Header.Get("If-None-Match"); got != "" {
				t.Errorf("expected no If-None-Match for first query variant, got %q", got)
			}
			w.Header().Set("ETag", `"models-etag-1"`)
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4o","object":"model","created":0,"owned_by":"github-copilot","name":"GPT-4o"}]}`))
		case 3:
			if got := r.URL.RawQuery; got != "client_version=0.99.0" {
				t.Errorf("refreshed variant query = %q, want client_version=0.99.0", got)
			}
			if got := r.Header.Get("If-None-Match"); got != "" {
				t.Errorf("expected no If-None-Match for query variant refresh, got %q", got)
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

	if requestCount != 3 {
		t.Fatalf("expected canonical seed plus 2 variant requests, got %d", requestCount)
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

	req1 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w1 := httptest.NewRecorder()
	handler.HandleModels(w1, req1)
	handler.models.mu.Lock()
	entry := handler.models.entries[""]
	entry.expiry = time.Now().Add(-time.Minute)
	handler.models.entries[""] = entry
	handler.models.mu.Unlock()

	req2 := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w2 := httptest.NewRecorder()
	handler.HandleModels(w2, req2)

	resp := w2.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected cached 200 response, got %d", resp.StatusCode)
	}
	if got := resp.Header.Get("ETag"); got != `"models-etag-1"` {
		t.Errorf("ETag = %q, want %q", got, `"models-etag-1"`)
	}
	if requestCount != 2 {
		t.Fatalf("upstream request count = %d, want initial response plus expired-entry revalidation", requestCount)
	}
	handler.models.mu.RLock()
	refreshedExpiry := handler.models.entries[""].expiry
	handler.models.mu.RUnlock()
	if !time.Now().Before(refreshedExpiry) {
		t.Fatalf("canonical expiry = %v, want extended TTL after 304", refreshedExpiry)
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
	if err := json.Unmarshal(errObj["message"], &msg); err != nil {
		t.Fatalf("json.Unmarshal(message) error = %v", err)
	}
	if msg != "test error message" {
		t.Errorf("message = %q, want %q", msg, "test error message")
	}

	var errType string
	if err := json.Unmarshal(errObj["type"], &errType); err != nil {
		t.Fatalf("json.Unmarshal(type) error = %v", err)
	}
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
// forwarded with proper SSE headers.
func TestOpenAIChatCompletionsStreaming(t *testing.T) {
	sseBody := "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"index\":0}]}\n\ndata: {\"id\":\"chatcmpl-1\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\ndata: [DONE]\n\n"

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		// Verify streaming detection
		var partial struct {
			Stream *bool `json:"stream"`
		}
		body, _ := io.ReadAll(r.Body)
		if err := json.Unmarshal(body, &partial); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}
		if partial.Stream == nil || !*partial.Stream {
			t.Error("expected stream=true in upstream request")
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody))
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

	// Verify SSE body is forwarded
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
	sseBody := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\",\"object\":\"response\",\"created_at\":1700000000,\"status\":\"in_progress\"}}\n\nevent: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"object\":\"response\",\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":2,\"total_tokens\":3}}}\n\n"

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Errorf("expected path /responses, got %q", r.URL.Path)
		}
		if got := r.Header.Get("Accept"); got != "text/event-stream" {
			t.Fatalf("expected streaming responses request to send Accept text/event-stream, got %q", got)
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
		_, _ = w.Write([]byte(sseBody))
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
	if got := string(body); got != sseBody {
		t.Fatalf("Responses stream should be preserved exactly:\n got: %q\nwant: %q", got, sseBody)
	}
	events := parseSSEEvents(string(body))
	if len(events) != 2 {
		t.Fatalf("Responses stream events = %d, want 2", len(events))
	}
	if events[0].Event != "response.created" || events[1].Event != "response.completed" {
		t.Fatalf("Responses event types = %q/%q, want response.created/response.completed", events[0].Event, events[1].Event)
	}
	var createdEvent struct {
		Type     string `json:"type"`
		Response struct {
			ID     string `json:"id"`
			Object string `json:"object"`
		} `json:"response"`
	}
	if err := json.Unmarshal([]byte(events[0].Data), &createdEvent); err != nil {
		t.Fatalf("unmarshal response.created event: %v", err)
	}
	if createdEvent.Type != "response.created" || createdEvent.Response.ID != "resp-1" || createdEvent.Response.Object != "response" {
		t.Fatalf("unexpected response.created payload: %+v", createdEvent)
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

func TestOpenAIChatCompletionsStreamingNormalizesChunks(t *testing.T) {
	sseBody := "data: {\"id\":\"chatcmpl-stream\",\"object\":\"\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	events := parseSSEEvents(string(body))
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2; body=%s", len(events), body)
	}
	var chunk map[string]json.RawMessage
	if err := json.Unmarshal([]byte(events[0].Data), &chunk); err != nil {
		t.Fatalf("unmarshal chunk: %v", err)
	}
	var object string
	if err := json.Unmarshal(chunk["object"], &object); err != nil {
		t.Fatalf("unmarshal object: %v", err)
	}
	if object != "chat.completion.chunk" {
		t.Fatalf("object = %q, want chat.completion.chunk", object)
	}
	var created int64
	if err := json.Unmarshal(chunk["created"], &created); err != nil {
		t.Fatalf("unmarshal created: %v", err)
	}
	if created == 0 {
		t.Fatal("created = 0, want non-zero Unix seconds")
	}
	var model string
	if err := json.Unmarshal(chunk["model"], &model); err != nil {
		t.Fatalf("unmarshal model: %v", err)
	}
	if model != "gpt-4" {
		t.Fatalf("model = %q, want requested model", model)
	}
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(chunk["choices"], &choices); err != nil {
		t.Fatalf("unmarshal choices: %v", err)
	}
	if len(choices) != 1 {
		t.Fatalf("choices = %d, want 1", len(choices))
	}
	var index int
	if err := json.Unmarshal(choices[0]["index"], &index); err != nil {
		t.Fatalf("unmarshal choice index: %v", err)
	}
	if index != 0 {
		t.Fatalf("choice index = %d, want 0", index)
	}
	if _, ok := choices[0]["delta"]; !ok {
		t.Fatal("choice delta missing")
	}
	if events[1].Data != "[DONE]" {
		t.Fatalf("terminal event = %q, want [DONE]", events[1].Data)
	}
}

func TestOpenAIChatCompletionsStreamingKeepsSyntheticIDStable(t *testing.T) {
	sseBody := "data: {\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"index\":0}]}\n\ndata: {\"id\":\"chatcmpl-upstream\",\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\",\"index\":0}]}\n\ndata: [DONE]\n\n"
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	events := parseSSEEvents(w.Body.String())
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3; body=%s", len(events), w.Body.String())
	}
	var first, second map[string]json.RawMessage
	if err := json.Unmarshal([]byte(events[0].Data), &first); err != nil {
		t.Fatalf("decode first chunk: %v", err)
	}
	if err := json.Unmarshal([]byte(events[1].Data), &second); err != nil {
		t.Fatalf("decode second chunk: %v", err)
	}
	var firstID, secondID string
	if err := json.Unmarshal(first["id"], &firstID); err != nil {
		t.Fatalf("decode first id: %v", err)
	}
	if err := json.Unmarshal(second["id"], &secondID); err != nil {
		t.Fatalf("decode second id: %v", err)
	}
	if firstID == "" || !strings.HasPrefix(firstID, "chatcmpl-") {
		t.Fatalf("first id = %q, want synthetic chatcmpl- id", firstID)
	}
	if secondID != firstID {
		t.Fatalf("second id = %q, want stable synthetic id %q", secondID, firstID)
	}
}

func TestOpenAIChatCompletionsStreamingSynthesizesDeltaToolCalls(t *testing.T) {
	sseBody := "data: {\"id\":\"chatcmpl-tools\",\"choices\":[{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]}]}\n\ndata: [DONE]\n\n"
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	events := parseSSEEvents(w.Body.String())
	var chunk map[string]json.RawMessage
	if err := json.Unmarshal([]byte(events[0].Data), &chunk); err != nil {
		t.Fatalf("decode chunk: %v", err)
	}
	choices := decodeChoicesForTest(t, chunk["choices"])
	var delta map[string]json.RawMessage
	if err := json.Unmarshal(choices[0]["delta"], &delta); err != nil {
		t.Fatalf("decode delta: %v", err)
	}
	if _, ok := delta["tool_calls"]; !ok {
		t.Fatalf("delta.tool_calls missing: %s", choices[0]["delta"])
	}
}

func TestOpenAIChatCompletionsStreamingNamedMessageEventNormalized(t *testing.T) {
	sseBody := "event: message\ndata: {\"id\":\"chatcmpl-stream\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\ndata: [DONE]\n\n"
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	body, _ := io.ReadAll(w.Result().Body)
	events := parseSSEEvents(string(body))
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2; body=%s", len(events), body)
	}
	if events[0].Event != "message" {
		t.Fatalf("event type = %q, want message", events[0].Event)
	}
	var chunk map[string]json.RawMessage
	if err := json.Unmarshal([]byte(events[0].Data), &chunk); err != nil {
		t.Fatalf("unmarshal named message chunk: %v", err)
	}
	var object string
	if err := json.Unmarshal(chunk["object"], &object); err != nil {
		t.Fatalf("unmarshal chunk object: %v", err)
	}
	if object != "chat.completion.chunk" {
		t.Fatalf("named message chunk object = %q, want chat.completion.chunk", object)
	}
}

func TestOpenAIChatCompletionsStreamingSidebandEventNotNormalized(t *testing.T) {
	sseBody := "event: ping\ndata: {\"time\":1700000000}\n\ndata: {\"id\":\"chatcmpl-stream\",\"choices\":[{\"delta\":{\"content\":\"Hi\"},\"index\":0}]}\n\ndata: [DONE]\n\n"
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	events := parseSSEEvents(string(body))
	if len(events) != 3 {
		t.Fatalf("events = %d, want 3; body=%s", len(events), body)
	}
	if events[0].Event != "ping" || events[0].Data != `{"time":1700000000}` {
		t.Fatalf("sideband event changed: %+v", events[0])
	}
	if strings.Contains(events[0].Data, "chat.completion.chunk") {
		t.Fatalf("sideband event was normalized as a chat chunk: %s", events[0].Data)
	}
	var chunk map[string]json.RawMessage
	if err := json.Unmarshal([]byte(events[1].Data), &chunk); err != nil {
		t.Fatalf("unmarshal normalized chat chunk: %v", err)
	}
	var object string
	if err := json.Unmarshal(chunk["object"], &object); err != nil {
		t.Fatalf("unmarshal chunk object: %v", err)
	}
	if object != "chat.completion.chunk" {
		t.Fatalf("chat chunk object = %q, want chat.completion.chunk", object)
	}
}

func TestOpenAIChatCompletionsStreamingErrorEventNotNormalized(t *testing.T) {
	sseBody := "event: error\ndata: {\"error\":{\"message\":\"rate limit\",\"type\":\"rate_limit_error\"}}\n\n"
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(sseBody))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected committed stream status 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if got := string(body); got != sseBody {
		t.Fatalf("stream error event changed:\n got: %q\nwant: %q", got, sseBody)
	}
}

// TestOpenAIChatCompletionsUpstreamErrorPassthrough validates that upstream error
// responses are forwarded with correct status and content-type.
func TestOpenAIChatCompletionsUpstreamErrorPassthrough(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"Invalid model","type":"invalid_request_error","param":"model","code":null}}`))
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

func TestOpenAIChatCompletionsNormalizesMissingTopLevelFields(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"choices": [{"message": {"role": "assistant", "content": "Hello"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 2, "total_tokens": 3},
			"copilot_usage": {"prompt_tokens": 10}
		}`))
	})

	reqBody := `{"model":"gpt-5-mini","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	var object string
	if err := json.Unmarshal(raw["object"], &object); err != nil {
		t.Fatalf("unmarshal object: %v", err)
	}
	if object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", object)
	}

	var created int64
	if err := json.Unmarshal(raw["created"], &created); err != nil {
		t.Fatalf("unmarshal created: %v", err)
	}
	if created == 0 {
		t.Fatal("created = 0, want non-zero Unix seconds")
	}

	var id string
	if err := json.Unmarshal(raw["id"], &id); err != nil {
		t.Fatalf("unmarshal id: %v", err)
	}
	if !strings.HasPrefix(id, "chatcmpl-") {
		t.Fatalf("id = %q, want chatcmpl- prefix", id)
	}

	var model string
	if err := json.Unmarshal(raw["model"], &model); err != nil {
		t.Fatalf("unmarshal model: %v", err)
	}
	if model != "gpt-5-mini" {
		t.Fatalf("model = %q, want requested model", model)
	}

	if _, ok := raw["copilot_usage"]; !ok {
		t.Fatal("copilot_usage was not preserved")
	}
}

func TestOpenAIChatCompletionsDoesNotNormalize2xxErrorPayload(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"provider failed","type":"server_error"}}`))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(w.Result().Body).Decode(&raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := raw["error"]; !ok {
		t.Fatal("error payload was not preserved")
	}
	for _, key := range []string{"object", "id", "created", "choices", "usage"} {
		if _, ok := raw[key]; ok {
			t.Fatalf("2xx error payload should not be normalized with %q: %s", key, mustCompactRawForTest(raw))
		}
	}
}

func TestOpenAIChatCompletionsNormalizesProviderToolCallsIntoMessage(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-provider-tools",
			"object":"chat.completion",
			"created":1700000000,
			"model":"gpt-4",
			"choices":[{
				"tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]
			}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(w.Result().Body).Decode(&raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	choices := decodeChoicesForTest(t, raw["choices"])
	var finishReason string
	if err := json.Unmarshal(choices[0]["finish_reason"], &finishReason); err != nil {
		t.Fatalf("decode finish_reason: %v", err)
	}
	if finishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", finishReason)
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(choices[0]["message"], &message); err != nil {
		t.Fatalf("decode message: %v", err)
	}
	if _, ok := message["tool_calls"]; !ok {
		t.Fatalf("message.tool_calls missing: %s", choices[0]["message"])
	}
}

func TestOpenAIChatCompletionsNormalizesFinishReasonFromMessageToolCalls(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-message-tools",
			"object":"chat.completion",
			"created":1700000000,
			"model":"gpt-4",
			"choices":[{
				"message":{"role":"assistant","content":"","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]}
			}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(w.Result().Body).Decode(&raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	choices := decodeChoicesForTest(t, raw["choices"])
	var finishReason string
	if err := json.Unmarshal(choices[0]["finish_reason"], &finishReason); err != nil {
		t.Fatalf("decode finish_reason: %v", err)
	}
	if finishReason != "tool_calls" {
		t.Fatalf("finish_reason = %q, want tool_calls", finishReason)
	}
}

func decodeChoicesForTest(t testing.TB, raw json.RawMessage) []map[string]json.RawMessage {
	t.Helper()
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &choices); err != nil {
		t.Fatalf("decode choices: %v", err)
	}
	if len(choices) == 0 {
		t.Fatal("expected at least one choice")
	}
	return choices
}

func mustCompactRawForTest(raw map[string]json.RawMessage) string {
	b, err := json.Marshal(raw)
	if err != nil {
		return "<invalid>"
	}
	return string(b)
}

func TestOpenAIChatCompletionsDoesNotTreatEmptyToolFieldsAsToolCalls(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-empty-tools",
			"object":"chat.completion",
			"created":1700000000,
			"model":"gpt-4",
			"choices":[{"message":{"role":"assistant","content":"done","tool_calls":[]}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(w.Result().Body).Decode(&raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	choices := decodeChoicesForTest(t, raw["choices"])
	var finishReason string
	if err := json.Unmarshal(choices[0]["finish_reason"], &finishReason); err != nil {
		t.Fatalf("decode finish_reason: %v", err)
	}
	if finishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop for empty tool_calls", finishReason)
	}
}

func TestOpenAIChatCompletionsNormalizesBlankFinishReason(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id":"chatcmpl-blank-finish",
			"object":"chat.completion",
			"created":1700000000,
			"model":"gpt-4",
			"choices":[{"index":0,"finish_reason":"","message":{"role":"assistant","content":"done"}}],
			"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}
		}`))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(w.Result().Body).Decode(&raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	choices := decodeChoicesForTest(t, raw["choices"])
	var finishReason string
	if err := json.Unmarshal(choices[0]["finish_reason"], &finishReason); err != nil {
		t.Fatalf("decode finish_reason: %v", err)
	}
	if finishReason != "stop" {
		t.Fatalf("finish_reason = %q, want stop", finishReason)
	}
}

func TestOpenAIChatCompletionsNormalizesEmptyObject(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-empty-object",
			"object": "",
			"created": 1700000000,
			"model": "gpt-4",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "Hello"}, "finish_reason": "stop"}],
			"usage": {"prompt_tokens": 1, "completion_tokens": 1, "total_tokens": 2}
		}`))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(w.Result().Body).Decode(&raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var object string
	if err := json.Unmarshal(raw["object"], &object); err != nil {
		t.Fatalf("unmarshal object: %v", err)
	}
	if object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", object)
	}
}

func TestOpenAIChatCompletionsNormalizesChoicesUsageAndPreservesVendorFields(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-upstream",
			"model": "gpt-5-mini",
			"choices": [
				{"message": {}},
				{"content": "provider content"},
				{"content": {"type": "structured"}}
			],
			"usage": {"prompt_tokens": 4, "completion_tokens": 6, "reasoning_tokens": 2},
			"prompt_filter_results": [{"prompt_index": 0}],
			"content_filter_results": {"hate": {"filtered": false}}
		}`))
	})

	reqBody := `{"model":"gpt-5-mini","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(raw["choices"], &choices); err != nil {
		t.Fatalf("unmarshal choices: %v", err)
	}
	if len(choices) != 3 {
		t.Fatalf("choices length = %d, want 3", len(choices))
	}
	for i, choice := range choices {
		var index int
		if err := json.Unmarshal(choice["index"], &index); err != nil {
			t.Fatalf("choice[%d] index: %v", i, err)
		}
		if index != i {
			t.Fatalf("choice[%d].index = %d, want %d", i, index, i)
		}
		var finishReason string
		if err := json.Unmarshal(choice["finish_reason"], &finishReason); err != nil {
			t.Fatalf("choice[%d] finish_reason: %v", i, err)
		}
		if finishReason != "stop" {
			t.Fatalf("choice[%d].finish_reason = %q, want stop", i, finishReason)
		}
	}

	var msg0 map[string]json.RawMessage
	if err := json.Unmarshal(choices[0]["message"], &msg0); err != nil {
		t.Fatalf("choice[0] message: %v", err)
	}
	var role0 string
	if err := json.Unmarshal(msg0["role"], &role0); err != nil {
		t.Fatalf("choice[0] role: %v", err)
	}
	if role0 != "assistant" {
		t.Fatalf("choice[0].message.role = %q, want assistant", role0)
	}
	var content0 string
	if err := json.Unmarshal(msg0["content"], &content0); err != nil {
		t.Fatalf("choice[0] content: %v", err)
	}
	if content0 != "" {
		t.Fatalf("choice[0].message.content = %q, want empty string", content0)
	}

	var msg1 map[string]json.RawMessage
	if err := json.Unmarshal(choices[1]["message"], &msg1); err != nil {
		t.Fatalf("choice[1] message: %v", err)
	}
	var role1, content1 string
	if err := json.Unmarshal(msg1["role"], &role1); err != nil {
		t.Fatalf("choice[1] role: %v", err)
	}
	if err := json.Unmarshal(msg1["content"], &content1); err != nil {
		t.Fatalf("choice[1] content: %v", err)
	}
	if role1 != "assistant" || content1 != "provider content" {
		t.Fatalf("choice[1] message = role %q content %q, want assistant/provider content", role1, content1)
	}

	var msg2 map[string]json.RawMessage
	if err := json.Unmarshal(choices[2]["message"], &msg2); err != nil {
		t.Fatalf("choice[2] message: %v", err)
	}
	var content2 string
	if err := json.Unmarshal(msg2["content"], &content2); err != nil {
		t.Fatalf("choice[2] content: %v", err)
	}
	if content2 != "" {
		t.Fatalf("choice[2].message.content = %q, want empty string for non-string provider content", content2)
	}

	var usage map[string]json.RawMessage
	if err := json.Unmarshal(raw["usage"], &usage); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	wantUsage := map[string]int{"prompt_tokens": 4, "completion_tokens": 6, "total_tokens": 10}
	for key, want := range wantUsage {
		var got int
		if err := json.Unmarshal(usage[key], &got); err != nil {
			t.Fatalf("usage[%s]: %v", key, err)
		}
		if got != want {
			t.Fatalf("usage[%s] = %d, want %d", key, got, want)
		}
	}
	if _, ok := usage["reasoning_tokens"]; !ok {
		t.Fatal("usage.reasoning_tokens was not preserved")
	}
	if _, ok := raw["prompt_filter_results"]; !ok {
		t.Fatal("prompt_filter_results was not preserved")
	}
	if _, ok := raw["content_filter_results"]; !ok {
		t.Fatal("content_filter_results was not preserved")
	}
}

func TestOpenAIChatCompletionsNormalizesSuccessful2xxResponses(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"created"}}]}`))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 201, got %d: %s", resp.StatusCode, body)
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var object string
	if err := json.Unmarshal(raw["object"], &object); err != nil {
		t.Fatalf("unmarshal object: %v", err)
	}
	if object != "chat.completion" {
		t.Fatalf("object = %q, want chat.completion", object)
	}
}

func TestOpenAIChatCompletionsNormalizesMissingUsage(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
			"id": "chatcmpl-usage",
			"object": "chat.completion",
			"created": 1700000000,
			"model": "gpt-4",
			"choices": [{"index": 0, "message": {"role": "assistant", "content": "Hello"}, "finish_reason": "stop"}]
		}`))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	var usage map[string]int
	if err := json.Unmarshal(raw["usage"], &usage); err != nil {
		t.Fatalf("unmarshal usage: %v", err)
	}
	for _, key := range []string{"prompt_tokens", "completion_tokens", "total_tokens"} {
		if got := usage[key]; got != 0 {
			t.Fatalf("usage[%s] = %d, want 0", key, got)
		}
	}
}

func TestOpenAIChatCompletionsDoesNotNormalizeErrorResponses(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request","type":"invalid_request_error"}}`))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400, got %d", resp.StatusCode)
	}
	var raw map[string]json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if _, ok := raw["object"]; ok {
		t.Fatal("error response should not be normalized with object")
	}
	if _, ok := raw["created"]; ok {
		t.Fatal("error response should not be normalized with created")
	}
}

func TestOpenAIChatCompletionsRetryableUpstreamErrorIncludesDetail(t *testing.T) {
	var calls atomic.Int32
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-Id", fmt.Sprintf("req-%d", n))
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limit exceeded","type":"rate_limit_error","param":"model","code":"too_many_requests"}}`))
	})

	reqBody := `{"model":"gpt-4","messages":[{"role":"user","content":"Hello"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("expected 429, got %d", resp.StatusCode)
	}
	if calls.Load() != 3 {
		t.Fatalf("expected 3 upstream attempts, got %d", calls.Load())
	}
	if got := resp.Header.Get("X-Request-Id"); got != "req-3" {
		t.Fatalf("X-Request-Id = %q, want req-3", got)
	}

	var errResp map[string]map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&errResp); err != nil {
		t.Fatalf("failed to parse error: %v", err)
	}
	message, _ := errResp["error"]["message"].(string)
	for _, want := range []string{"upstream error (429)", "rate limit exceeded", "type=rate_limit_error", "param=model", "code=too_many_requests"} {
		if !strings.Contains(message, want) {
			t.Errorf("message = %q, want %q", message, want)
		}
	}
}

// TestOpenAIChatCompletionsResponseShape validates a non-streaming response
// has the correct OpenAI Chat Completions response structure.
func TestOpenAIChatCompletionsResponseShape(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{
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
	if err := json.Unmarshal(raw["object"], &obj); err != nil {
		t.Fatalf("json.Unmarshal(object) error = %v", err)
	}
	if obj != "chat.completion" {
		t.Errorf("object = %q, want chat.completion", obj)
	}

	// Verify choices structure
	var choices []map[string]json.RawMessage
	if err := json.Unmarshal(raw["choices"], &choices); err != nil {
		t.Fatalf("json.Unmarshal(choices) error = %v", err)
	}
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
		_, _ = w.Write([]byte(`{
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
	for _, f := range []string{"id", "object", "created_at", "status", "model", "output", "usage"} {
		if _, ok := raw[f]; !ok {
			t.Errorf("response missing field %q", f)
		}
	}

	var obj string
	if err := json.Unmarshal(raw["object"], &obj); err != nil {
		t.Fatalf("json.Unmarshal(object) error = %v", err)
	}
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
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}
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
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
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
		if err := json.Unmarshal(result, &m); err != nil {
			t.Fatalf("json.Unmarshal(result) error = %v", err)
		}
		if string(m["parallel_tool_calls"]) != "true" {
			t.Errorf("parallel_tool_calls = %s, want true", m["parallel_tool_calls"])
		}
	})

	t.Run("preserves existing value", func(t *testing.T) {
		input := `{"model":"gpt-4","tools":[{"type":"function"}],"parallel_tool_calls":false}`
		result := injectParallelToolCalls([]byte(input))
		var m map[string]json.RawMessage
		if err := json.Unmarshal(result, &m); err != nil {
			t.Fatalf("json.Unmarshal(result) error = %v", err)
		}
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
		if err := json.Unmarshal(body, &oaiReq); err != nil {
			t.Fatalf("upstream received invalid JSON: %v", err)
		}
		if oaiReq.ParallelToolCalls == nil || !*oaiReq.ParallelToolCalls {
			t.Error("expected parallel_tool_calls=true injected by proxy")
		}
		if oaiReq.Stream == nil || !*oaiReq.Stream {
			t.Error("expected stream=true forced by proxy when tools present")
		}

		// Return SSE since proxy forced streaming
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"ok\"},\"finish_reason\":null}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"id\":\"c1\",\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
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

func TestNewProviderJSONRequest_StripsClientHeadersForAzureIdentity(t *testing.T) {
	tokenSource := &staticAzureTokenSource{token: "entra-token"}
	handler := &ProxyHandler{}
	provider := &providerRuntime{
		id:         "foundry",
		kind:       providerTypeAzureOpenAI,
		baseURL:    "https://foundry.example.test/openai/v1",
		authMode:   providerAuthModeAzureIdentity,
		azureToken: tokenSource,
	}

	req, err := handler.newProviderJSONRequest(
		context.Background(),
		provider,
		http.MethodPost,
		"/responses",
		[]byte(`{"model":"gpt-test"}`),
		http.Header{
			"Authorization":          []string{"Bearer client-copilot-token"},
			"api-key":                []string{"client-api-key"},
			"editor-version":         []string{"client-editor"},
			"editor-plugin-version":  []string{"client-plugin"},
			"user-agent":             []string{"client-agent"},
			"copilot-integration-id": []string{"client-integration"},
			"x-github-api-version":   []string{"client-api"},
			"x-request-id":           []string{"client-request-id"},
			"openai-intent":          []string{"client-intent"},
			"Traceparent":            []string{"00-11111111111111111111111111111111-2222222222222222-01"},
		},
		"",
	)
	if err != nil {
		t.Fatalf("newProviderJSONRequest() error = %v", err)
	}

	if got := req.Header.Get("Authorization"); got != "Bearer entra-token" {
		t.Fatalf("Authorization = %q, want Azure identity bearer token", got)
	}
	if got := req.Header.Get("api-key"); got != "" {
		t.Fatalf("api-key = %q, want omitted for Azure identity", got)
	}
	for _, header := range []string{"editor-version", "editor-plugin-version", "user-agent", "copilot-integration-id", "x-github-api-version", "x-request-id", "openai-intent"} {
		if got := req.Header.Get(header); got != "" {
			t.Fatalf("%s = %q, want stripped for Azure", header, got)
		}
	}
	if got := req.Header.Get("Traceparent"); got != "00-11111111111111111111111111111111-2222222222222222-01" {
		t.Fatalf("Traceparent = %q, want passthrough trace header", got)
	}
	if tokenSource.calls.Load() != 1 {
		t.Fatalf("token source calls = %d, want 1", tokenSource.calls.Load())
	}
}

func TestHandleReadyz_AzureIdentityProviderSkipsModelsProbe(t *testing.T) {
	tokenSource := &staticAzureTokenSource{token: "readyz-entra-token"}
	var modelsProbeHits atomic.Int32

	azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		modelsProbeHits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer azureServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:       "foundry",
			Type:     "azure-openai",
			Default:  true,
			BaseURL:  azureServer.URL + "/openai/v1",
			AuthMode: "azure_identity",
			Models: []ProviderModelConfig{{
				PublicID:  "gpt-5.4",
				Endpoints: []string{"/responses"},
			}},
		}}}),
		withAzureIdentityTokenSourceFactoryForTest(func(providerID, scope string) (azureTokenSource, error) {
			if providerID != "foundry" {
				t.Fatalf("providerID = %q, want foundry", providerID)
			}
			if scope != defaultAzureIdentityTokenScope {
				t.Fatalf("scope = %q, want %q", scope, defaultAzureIdentityTokenScope)
			}
			return tokenSource, nil
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	w := httptest.NewRecorder()
	handler.HandleReadyz(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
	if got := modelsProbeHits.Load(); got != 0 {
		t.Fatalf("Azure /models readiness probe hits = %d, want 0", got)
	}
	if tokenSource.calls.Load() != 0 {
		t.Fatalf("token source calls = %d, want 0 when readiness skips Azure metadata", tokenSource.calls.Load())
	}
}

func TestHandleModels_AzureIdentityProviderUsesBearerForOverlay(t *testing.T) {
	tokenSource := &staticAzureTokenSource{token: "models-entra-token"}
	var overlayHits atomic.Int32

	azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		overlayHits.Add(1)
		if got := r.URL.Path; got != "/openai/v1/models" {
			t.Fatalf("expected Azure models path /openai/v1/models, got %s", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer models-entra-token" {
			t.Fatalf("expected Azure identity Authorization header, got %q", got)
		}
		if got := r.Header.Get("api-key"); got != "" {
			t.Fatalf("expected no api-key header, got %q", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-5.4","object":"model","owned_by":"azure"}]}`))
	}))
	defer azureServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:       "foundry",
			Type:     "azure-openai",
			Default:  true,
			BaseURL:  azureServer.URL + "/openai/v1",
			AuthMode: "azure_identity",
			Models: []ProviderModelConfig{{
				PublicID:  "gpt-5.4",
				Endpoints: []string{"/responses"},
			}},
		}}}),
		withAzureIdentityTokenSourceFactoryForTest(func(string, string) (azureTokenSource, error) {
			return tokenSource, nil
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
	if overlayHits.Load() != 1 {
		t.Fatalf("overlay hits = %d, want 1", overlayHits.Load())
	}
}

func TestHandleResponses_RoutesConfiguredAzureIdentityProvider(t *testing.T) {
	tokenSource := &staticAzureTokenSource{token: "responses-entra-token"}

	azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/openai/v1/responses" {
			t.Fatalf("expected Azure path /openai/v1/responses, got %s", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer responses-entra-token" {
			t.Fatalf("expected Azure identity Authorization header, got %q", got)
		}
		if got := r.Header.Get("api-key"); got != "" {
			t.Fatalf("expected no api-key header, got %q", got)
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
			t.Fatalf("expected Azure deployment gpt-5-4-prod, got %q", model)
		}

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-azure","object":"response","status":"completed","model":"gpt-5-4-prod","output":[]}`))
	}))
	defer azureServer.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:       "foundry",
			Type:     "azure-openai",
			Default:  true,
			BaseURL:  azureServer.URL + "/openai/v1",
			AuthMode: "azure_identity",
			Models: []ProviderModelConfig{{
				PublicID:   "gpt-5-public",
				Deployment: "gpt-5-4-prod",
				Endpoints:  []string{"/responses"},
			}},
		}}}),
		withAzureIdentityTokenSourceFactoryForTest(func(string, string) (azureTokenSource, error) {
			return tokenSource, nil
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model": "gpt-5-public",
		"input": "Hello"
	}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleResponses(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, body)
	}
}

func TestHandleOpenAIChatCompletions_RoutesConfiguredAzureIdentityProvider(t *testing.T) {
	tokenSource := &staticAzureTokenSource{token: "chat-entra-token"}

	azureServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/openai/v1/chat/completions" {
			t.Fatalf("expected Azure path /openai/v1/chat/completions, got %s", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer chat-entra-token" {
			t.Fatalf("expected Azure identity Authorization header, got %q", got)
		}
		if got := r.Header.Get("api-key"); got != "" {
			t.Fatalf("expected no api-key header, got %q", got)
		}

		var upstreamReq models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&upstreamReq); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if upstreamReq.Model != "gpt-5-4-prod" {
			t.Fatalf("expected Azure deployment gpt-5-4-prod, got %q", upstreamReq.Model)
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID:     "chatcmpl-azure-identity",
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
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:       "foundry",
			Type:     "azure-openai",
			Default:  true,
			BaseURL:  azureServer.URL + "/openai/v1",
			AuthMode: "azure_identity",
			Models: []ProviderModelConfig{{
				PublicID:   "gpt-5-public",
				Deployment: "gpt-5-4-prod",
				Endpoints:  []string{"/chat/completions"},
			}},
		}}}),
		withAzureIdentityTokenSourceFactoryForTest(func(string, string) (azureTokenSource, error) {
			return tokenSource, nil
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler returned error: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model": "gpt-5-public",
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
}

func TestProviderResolutionFailuresSurfaceRealCauseAcrossPublicSurfaces(t *testing.T) {
	handler := &ProxyHandler{
		auth:       auth.NewTestAuthenticator("test-token"),
		log:        logger.New(logger.LevelError),
		copilotURL: "http://upstream.test",
		providersState: &providerSetup{
			providers:         map[string]*providerRuntime{},
			providerOrder:     nil,
			defaultProviderID: "missing",
			models:            map[string]providerModel{},
		},
	}

	tests := []struct {
		name        string
		path        string
		body        string
		handle      func(http.ResponseWriter, *http.Request)
		wantMessage string
		anthropic   bool
	}{
		{
			name:        "openai chat",
			path:        "/v1/chat/completions",
			body:        `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`,
			handle:      handler.HandleOpenAIChatCompletions,
			wantMessage: "no provider available for endpoint /chat/completions",
		},
		{
			name:        "anthropic messages",
			path:        "/v1/messages",
			body:        `{"model":"claude-sonnet-4","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			handle:      handler.HandleAnthropicMessages,
			wantMessage: "no provider available for endpoint /chat/completions",
			anthropic:   true,
		},
		{
			name:        "responses",
			path:        "/v1/responses",
			body:        `{"model":"gpt-5","input":"hi"}`,
			handle:      handler.HandleResponses,
			wantMessage: "no provider available for endpoint /responses",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			tt.handle(w, req)

			resp := w.Result()
			if resp.StatusCode != http.StatusInternalServerError {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("StatusCode = %d, want 500: %s", resp.StatusCode, string(body))
			}
			body, _ := io.ReadAll(resp.Body)
			if !strings.Contains(string(body), tt.wantMessage) {
				t.Fatalf("response body = %s, want real provider error %q", string(body), tt.wantMessage)
			}
			for _, forbidden := range []string{"authentication failed", "upstream request failed"} {
				if strings.Contains(string(body), forbidden) {
					t.Fatalf("response body = %s, should not contain generic message %q", string(body), forbidden)
				}
			}
		})
	}
}

func TestStripUnsupportedResponsesRequestFields_RemovesSamplingForCodexOnly(t *testing.T) {
	body := []byte(`{"model":"gpt-5-codex","input":"hi","top_p":0.9,"temperature":0.2}`)

	codex := &providerRuntime{kind: providerTypeOpenAICodex}
	rewritten, fields := stripUnsupportedResponsesRequestFields(body, codex)
	if !reflect.DeepEqual(fields, []string{"top_p", "temperature"}) {
		t.Fatalf("stripped fields = %#v, want top_p and temperature", fields)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(rewritten, &payload); err != nil {
		t.Fatalf("unmarshal rewritten body: %v", err)
	}
	if _, ok := payload["top_p"]; ok {
		t.Fatalf("top_p was not stripped: %s", rewritten)
	}
	if _, ok := payload["temperature"]; ok {
		t.Fatalf("temperature was not stripped: %s", rewritten)
	}

	copilot := &providerRuntime{kind: providerTypeCopilot}
	rewritten, fields = stripUnsupportedResponsesRequestFields(body, copilot)
	if len(fields) != 0 {
		t.Fatalf("copilot stripped fields = %#v, want none", fields)
	}
	if string(rewritten) != string(body) {
		t.Fatalf("copilot body changed: got %s want %s", rewritten, body)
	}
}

func TestSyntheticResponsesStoreFalseOnlyForProxyBuiltRequests(t *testing.T) {
	handler := &ProxyHandler{copilotURL: "https://api.githubcopilot.com"}
	fields := map[string]json.RawMessage{
		"model": json.RawMessage(`"gpt-5"`),
		"input": json.RawMessage(`"hi"`),
	}

	handler.setSyntheticResponsesStoreFalse(fields)
	if got := strings.TrimSpace(string(fields["store"])); got != "false" {
		t.Fatalf("synthetic store = %q, want false", got)
	}

	passthrough := handler.rewriteResponsesRequestBody([]byte(`{"model":"gpt-5","input":"hi"}`), "responses", true)
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(passthrough, &payload); err != nil {
		t.Fatalf("unmarshal passthrough body: %v", err)
	}
	if _, ok := payload["store"]; ok {
		t.Fatalf("passthrough rewrite unexpectedly added store: %s", passthrough)
	}
}
