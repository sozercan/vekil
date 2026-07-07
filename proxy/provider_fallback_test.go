package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
	"github.com/sozercan/vekil/proxy/selector"
)

func newFallbackTestHandler(t *testing.T, primaryURL, backupURL string) *ProxyHandler {
	t.Helper()
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{
					ID:       "primary",
					Type:     "openai-compatible",
					Default:  true,
					BaseURL:  primaryURL,
					AuthType: "none",
					Models: []ProviderModelConfig{{
						PublicID:   "gpt-primary",
						Deployment: "primary-upstream",
						Endpoints:  []string{providerEndpointChatCompletions},
					}},
				},
				{
					ID:       "backup",
					Type:     "openai-compatible",
					BaseURL:  backupURL,
					AuthType: "none",
					Models: []ProviderModelConfig{{
						PublicID:   "gpt-backup",
						Deployment: "backup-upstream",
						Endpoints:  []string{providerEndpointChatCompletions},
					}},
				},
			},
			Fallbacks: []ProviderFallbackConfig{{
				Public: "gpt-primary",
				Chain: []ProviderFallbackChainEntry{
					{Provider: "primary", Model: "gpt-primary"},
					{Provider: "backup", Model: "gpt-backup"},
				},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.maxRetries = 1
	handler.retryBaseDelay = time.Millisecond
	return handler
}

func TestProviderFallbackSucceedsOnFirstHop(t *testing.T) {
	var primaryHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		assertFallbackUpstreamModel(t, r, "primary-upstream")
		writeFallbackChatOK(w, "primary-ok")
	}))
	defer primary.Close()
	var backupHits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		writeFallbackChatOK(w, "backup-ok")
	}))
	defer backup.Close()

	handler := newFallbackTestHandler(t, primary.URL, backup.URL)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-primary","messages":[{"role":"user","content":"hi"}]}`))
	handler.HandleOpenAIChatCompletions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if primaryHits.Load() != 1 || backupHits.Load() != 0 {
		t.Fatalf("hits primary/backup = %d/%d, want 1/0", primaryHits.Load(), backupHits.Load())
	}
}

func TestProviderFallbackSucceedsOnSecondHopAfter429(t *testing.T) {
	var primaryHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryHits.Add(1)
		assertFallbackUpstreamModel(t, r, "primary-upstream")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer primary.Close()
	var backupHits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		assertFallbackUpstreamModel(t, r, "backup-upstream")
		writeFallbackChatOK(w, "backup-ok")
	}))
	defer backup.Close()

	handler := newFallbackTestHandler(t, primary.URL, backup.URL)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-primary","messages":[{"role":"user","content":"hi"}]}`))
	handler.HandleOpenAIChatCompletions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if primaryHits.Load() != 1 || backupHits.Load() != 1 {
		t.Fatalf("hits primary/backup = %d/%d, want 1/1", primaryHits.Load(), backupHits.Load())
	}
}

func TestProviderFallbackAllHopsFailReturnsLastError(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"primary limited"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"backup down"}}`))
	}))
	defer backup.Close()

	handler := newFallbackTestHandler(t, primary.URL, backup.URL)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-primary","messages":[{"role":"user","content":"hi"}]}`))
	handler.HandleOpenAIChatCompletions(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "backup down") {
		t.Fatalf("body = %s, want last upstream error", w.Body.String())
	}
}

func TestProviderFallbackClientErrorShortCircuits(t *testing.T) {
	var backupHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"bad request"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		writeFallbackChatOK(w, "backup-ok")
	}))
	defer backup.Close()

	handler := newFallbackTestHandler(t, primary.URL, backup.URL)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-primary","messages":[{"role":"user","content":"hi"}]}`))
	handler.HandleOpenAIChatCompletions(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", w.Code, w.Body.String())
	}
	if backupHits.Load() != 0 {
		t.Fatalf("backup hits = %d, want 0", backupHits.Load())
	}
}

func assertFallbackUpstreamModel(t *testing.T, r *http.Request, want string) {
	t.Helper()
	body, _ := io.ReadAll(r.Body)
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode request: %v", err)
	}
	if got := strings.Trim(string(payload["model"]), `"`); got != want {
		t.Fatalf("upstream model = %q, want %q", got, want)
	}
}

func writeFallbackChatOK(w http.ResponseWriter, content string) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"id":"chatcmpl-ok","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"` + content + `"}}]}`))
}

func TestProviderFallbackSucceedsOnSecondHopAfter500(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"primary internal"}}`))
	}))
	defer primary.Close()
	var backupHits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		assertFallbackUpstreamModel(t, r, "backup-upstream")
		writeFallbackChatOK(w, "backup-ok")
	}))
	defer backup.Close()

	handler := newFallbackTestHandler(t, primary.URL, backup.URL)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-primary","messages":[{"role":"user","content":"hi"}]}`))
	handler.HandleOpenAIChatCompletions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if backupHits.Load() != 1 {
		t.Fatalf("backup hits = %d, want 1", backupHits.Load())
	}
}

func TestFallbackChainsConfiguredAfterDynamicModelLoading(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-primary"}]}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-backup"}]}`))
	}))
	defer backup.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{ID: "primary", Type: "openai-compatible", Default: true, BaseURL: primary.URL, AuthType: "none", ModelDiscovery: "openai"},
				{ID: "backup", Type: "openai-compatible", BaseURL: backup.URL, AuthType: "none", ModelDiscovery: "openai"},
			},
			Fallbacks: []ProviderFallbackConfig{{Public: "gpt-primary", Chain: []ProviderFallbackChainEntry{{Provider: "primary", Model: "gpt-primary"}, {Provider: "backup", Model: "gpt-backup"}}}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	chain := handler.providerSetup().fallbackChain("gpt-primary")
	if len(chain) != 2 || chain[0].providerID != "primary" || chain[1].providerID != "backup" {
		t.Fatalf("fallback chain = %#v, want primary->backup", chain)
	}
}

func TestDirectAnthropicFallbackPreservesRequestedResponseModel(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"primary internal"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg-1","type":"message","role":"assistant","model":"backup-upstream","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer backup.Close()
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{ID: "primary", Type: "anthropic-compatible", Default: true, BaseURL: primary.URL, AuthType: "none", MessagesPath: "/v1/messages", Models: []ProviderModelConfig{{PublicID: "claude-primary", Deployment: "primary-upstream", Endpoints: []string{providerEndpointMessages}}}},
				{ID: "backup", Type: "anthropic-compatible", BaseURL: backup.URL, AuthType: "none", MessagesPath: "/v1/messages", Models: []ProviderModelConfig{{PublicID: "claude-backup", Deployment: "backup-upstream", Endpoints: []string{providerEndpointMessages}}}},
			},
			Fallbacks: []ProviderFallbackConfig{{Public: "claude-primary", Chain: []ProviderFallbackChainEntry{{Provider: "primary", Model: "claude-primary"}, {Provider: "backup", Model: "claude-backup"}}}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.maxRetries = 1
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-primary","max_tokens":128,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleAnthropicMessages(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp models.AnthropicResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Model != "claude-primary" {
		t.Fatalf("response model = %q, want requested claude-primary", resp.Model)
	}
}

func TestProviderFallbackDoesNotMaskPermanentTransportError(t *testing.T) {
	backupHits := atomic.Int32{}
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		writeFallbackChatOK(w, "backup-ok")
	}))
	defer backup.Close()
	handler := newFallbackTestHandler(t, "https://missing.invalid", backup.URL)
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-primary","messages":[{"role":"user","content":"hi"}]}`))
	handler.HandleOpenAIChatCompletions(w, req)
	if backupHits.Load() != 0 {
		t.Fatalf("backup hits = %d, want 0 for permanent DNS failure", backupHits.Load())
	}
}

func TestProviderFallbackAfterProviderEndpointExhaustion(t *testing.T) {
	backupHits := atomic.Int32{}
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		writeFallbackChatOK(w, "backup-ok")
	}))
	defer backup.Close()
	handler := newFallbackTestHandler(t, "http://127.0.0.1:1", backup.URL)
	primary := handler.providerSetup().providerByID("primary")
	primary.endpoints = []*providerEndpointRuntime{{endpoint: selector.Endpoint{Name: "dead", BaseURL: "http://127.0.0.1:1", Healthy: true}, health: newEndpointHealthTracker(endpointHealthConfig{errorBudget: endpointErrorBudget{Limit: 1, Window: time.Minute}, cooldown: time.Hour})}}
	primary.endpointByName = map[string]*providerEndpointRuntime{"dead": primary.endpoints[0]}
	primary.selector = selector.NewRoundRobin()
	primary.endpointByName["dead"].health.recordFailure(time.Now())

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-primary","messages":[{"role":"user","content":"hi"}]}`))
	handler.HandleOpenAIChatCompletions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if backupHits.Load() != 1 {
		t.Fatalf("backup hits = %d, want 1", backupHits.Load())
	}
}

func TestResponsesFallbackPreservesRequestedModel(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"primary down"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assertFallbackUpstreamModel(t, r, "backup-upstream")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-backup","object":"response","status":"completed","model":"backup-upstream","output":[]}`))
	}))
	defer backup.Close()
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{ID: "primary", Type: "openai-compatible", Default: true, BaseURL: primary.URL, AuthType: "none", Models: []ProviderModelConfig{{PublicID: "gpt-primary", Deployment: "primary-upstream", Endpoints: []string{providerEndpointResponses}}}},
				{ID: "backup", Type: "openai-compatible", BaseURL: backup.URL, AuthType: "none", Models: []ProviderModelConfig{{PublicID: "gpt-backup", Deployment: "backup-upstream", Endpoints: []string{providerEndpointResponses}}}},
			},
			Fallbacks: []ProviderFallbackConfig{{Public: "gpt-primary", Chain: []ProviderFallbackChainEntry{{Provider: "primary", Model: "gpt-primary"}, {Provider: "backup", Model: "gpt-backup"}}}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.maxRetries = 1
	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-primary","input":"hi"}`))
	req.Header.Set("Content-Type", "application/json")
	handler.HandleResponses(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if got := rawJSONString(payload["model"]); got != "gpt-primary" {
		t.Fatalf("response model = %q, want requested gpt-primary", got)
	}
}

func TestFallbackPublicMustMatchFirstChainEntry(t *testing.T) {
	handler := &ProxyHandler{copilotURL: "https://copilot.example.test"}
	_, err := handler.buildConfiguredProviderSetup(context.Background(), ProvidersConfig{
		Providers: []ProviderConfig{{
			ID:       "primary",
			Type:     "openai-compatible",
			Default:  true,
			BaseURL:  "https://primary.example.test/v1",
			AuthType: "none",
			Models:   []ProviderModelConfig{{PublicID: "gpt-primary", Endpoints: []string{providerEndpointChatCompletions}}},
		}},
		Fallbacks: []ProviderFallbackConfig{{Public: "typo-primary", Chain: []ProviderFallbackChainEntry{{Provider: "primary", Model: "gpt-primary"}}}},
	})
	if err == nil || !strings.Contains(err.Error(), "must match first chain model") {
		t.Fatalf("buildConfiguredProviderSetup() error = %v, want public mismatch", err)
	}
}

func TestProviderFallbackDoesNotMaskProviderAuthError(t *testing.T) {
	err := &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("provider auth failed")}
	if shouldFallbackToNextProvider(err) {
		t.Fatal("provider auth/config errors should not trigger fallback")
	}
}

func TestResponsesFallbackAttributesUsageToBackupProvider(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"primary down"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-backup","object":"response","status":"completed","model":"backup-upstream","output":[],"usage":{"input_tokens":3,"output_tokens":4,"total_tokens":7}}`))
	}))
	defer backup.Close()
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{ID: "primary", Type: "openai-compatible", Default: true, BaseURL: primary.URL, AuthType: "none", Models: []ProviderModelConfig{{PublicID: "gpt-primary", Deployment: "primary-upstream", Endpoints: []string{providerEndpointResponses}}}},
				{ID: "backup", Type: "openai-compatible", BaseURL: backup.URL, AuthType: "none", Models: []ProviderModelConfig{{PublicID: "gpt-backup", Deployment: "backup-upstream", Endpoints: []string{providerEndpointResponses}}}},
			},
			Fallbacks: []ProviderFallbackConfig{{Public: "gpt-primary", Chain: []ProviderFallbackChainEntry{{Provider: "primary", Model: "gpt-primary"}, {Provider: "backup", Model: "gpt-backup"}}}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.maxRetries = 1
	ctx, summary := WithRequestSummary(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-primary","input":"hi"}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleResponses(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if summary.provider != "backup" {
		t.Fatalf("summary provider = %q, want backup", summary.provider)
	}
}

func TestResponsesStreamModelLineRewrite(t *testing.T) {
	line := "data: {\"type\":\"response.created\",\"model\":\"backup-upstream\"}\n"
	rewritten := rewriteResponsesSSEModelLine(line, "gpt-primary")
	if !strings.Contains(rewritten, `"model":"gpt-primary"`) {
		t.Fatalf("rewritten line = %q, want public model", rewritten)
	}
}

func TestNormalizeResponsesModelResponseLeavesOversizedBodyStreaming(t *testing.T) {
	body := strings.Repeat("x", usageSniffMaxBuffer+2)
	resp := &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body))}
	got := normalizeResponsesModelResponse(resp, "public", "upstream")
	read, err := io.ReadAll(got.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(read) != body {
		t.Fatalf("oversized body changed, got len %d want %d", len(read), len(body))
	}
}

func TestChatFallbackAttributesUsageToBackupProvider(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-ok","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`))
	}))
	defer backup.Close()
	handler := newFallbackTestHandler(t, primary.URL, backup.URL)
	ctx, summary := WithRequestSummary(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-primary","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	handler.HandleOpenAIChatCompletions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if summary.provider != "backup" {
		t.Fatalf("summary provider = %q, want backup", summary.provider)
	}
}

func TestFallbackChainsRejectDynamicAliasMissingFromLoadedCatalog(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"unrelated-upstream"}]}`))
	}))
	defer primary.Close()

	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-backup"}]}`))
	}))
	defer backup.Close()

	_, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{
					ID:             "primary",
					Type:           "openai-compatible",
					Default:        true,
					BaseURL:        primary.URL,
					AuthType:       "none",
					ModelDiscovery: "openai",
					Models: []ProviderModelConfig{{
						PublicID:   "gpt-primary",
						Deployment: "missing-upstream",
						Endpoints:  []string{providerEndpointChatCompletions},
					}},
				},
				{ID: "backup", Type: "openai-compatible", BaseURL: backup.URL, AuthType: "none", ModelDiscovery: "openai"},
			},
			Fallbacks: []ProviderFallbackConfig{{Public: "gpt-primary", Chain: []ProviderFallbackChainEntry{{Provider: "primary", Model: "gpt-primary"}, {Provider: "backup", Model: "gpt-backup"}}}},
		}),
	)
	if err == nil || !strings.Contains(err.Error(), "references unknown model") {
		t.Fatalf("NewProxyHandler() error = %v, want fallback unknown dynamic model", err)
	}
}

func TestProviderFallbackExhaustsPrimaryEndpointsBeforeProviderFallbackOn500(t *testing.T) {
	var eastHits atomic.Int32
	east := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eastHits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"east down"}}`))
	}))
	defer east.Close()

	var westHits atomic.Int32
	west := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		westHits.Add(1)
		assertFallbackUpstreamModel(t, r, "primary-upstream")
		writeFallbackChatOK(w, "west-ok")
	}))
	defer west.Close()

	var backupHits atomic.Int32
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		backupHits.Add(1)
		writeFallbackChatOK(w, "backup-ok")
	}))
	defer backup.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{
					ID:       "primary",
					Type:     "openai-compatible",
					Default:  true,
					AuthType: "none",
					Endpoints: []ProviderEndpointConfig{
						{Name: "east", BaseURL: east.URL, Health: ProviderEndpointHealthConfig{ErrorBudget: "1/m", Cooldown: "1h"}},
						{Name: "west", BaseURL: west.URL, Health: ProviderEndpointHealthConfig{ErrorBudget: "1/m", Cooldown: "1h"}},
					},
					Models: []ProviderModelConfig{{PublicID: "gpt-primary", Deployment: "primary-upstream", Endpoints: []string{providerEndpointChatCompletions}}},
				},
				{ID: "backup", Type: "openai-compatible", BaseURL: backup.URL, AuthType: "none", Models: []ProviderModelConfig{{PublicID: "gpt-backup", Deployment: "backup-upstream", Endpoints: []string{providerEndpointChatCompletions}}}},
			},
			Fallbacks: []ProviderFallbackConfig{{Public: "gpt-primary", Chain: []ProviderFallbackChainEntry{{Provider: "primary", Model: "gpt-primary"}, {Provider: "backup", Model: "gpt-backup"}}}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.maxRetries = 2
	handler.retryBaseDelay = time.Millisecond

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-primary","messages":[{"role":"user","content":"hi"}]}`))
	handler.HandleOpenAIChatCompletions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if eastHits.Load() != 1 || westHits.Load() != 1 || backupHits.Load() != 0 {
		t.Fatalf("hits east/west/backup = %d/%d/%d, want 1/1/0", eastHits.Load(), westHits.Load(), backupHits.Load())
	}
}

func TestFallbackResponseBodySurvivesPrimaryContextTimeout(t *testing.T) {
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(25 * time.Millisecond)
		_, _ = w.Write([]byte(`{"id":"chatcmpl-ok","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"backup-ok"}}]}`))
	}))
	defer backup.Close()

	handler := newFallbackTestHandler(t, "http://127.0.0.1:1", backup.URL)
	handler.maxRetries = 1
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	resp, owner, _, err := handler.postJSONEndpointWithHeadersTracked(ctx, providerEndpointChatCompletions, []byte(`{"model":"gpt-primary","messages":[{"role":"user","content":"hi"}]}`), nil)
	if err != nil {
		t.Fatalf("postJSONEndpointWithHeadersTracked() error = %v", err)
	}
	if owner.providerID != "backup" {
		t.Fatalf("owner.providerID = %q, want backup", owner.providerID)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll(fallback body) error = %v", err)
	}
	if !strings.Contains(string(body), "backup-ok") {
		t.Fatalf("fallback body = %s, want backup-ok", string(body))
	}
}

func TestResponsesStreamNestedModelLineRewrite(t *testing.T) {
	line := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-backup\",\"model\":\"backup-upstream\"}}\n"
	rewritten := rewriteResponsesSSEModelLine(line, "gpt-primary")
	if !strings.Contains(rewritten, `"model":"gpt-primary"`) {
		t.Fatalf("rewritten line = %q, want nested public model", rewritten)
	}
	if strings.Contains(rewritten, "backup-upstream") {
		t.Fatalf("rewritten line = %q, leaked backup model", rewritten)
	}
}

func TestResponsesStreamModelRewriteLeavesOversizedDataLineUnchanged(t *testing.T) {
	body := "data: {\"model\":\"backup-upstream\",\"payload\":\"" + strings.Repeat("x", responsesFailureTapMaxBuffer) + "\"}\n"
	readCloser := newResponsesModelRewriteReadCloser(io.NopCloser(strings.NewReader(body)), "gpt-primary")
	defer readCloser.Close()
	read, err := io.ReadAll(readCloser)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	if string(read) != body {
		t.Fatalf("oversized SSE line changed; got len %d want len %d", len(read), len(body))
	}
}

func TestResponsesWebSocketStatsUseSelectedFallbackOwner(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-backup","object":"response","status":"completed","model":"backup-upstream","output":[]}`))
	}))
	defer backup.Close()
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{
				{ID: "primary", Type: "openai-compatible", Default: true, BaseURL: primary.URL, AuthType: "none", Models: []ProviderModelConfig{{PublicID: "gpt-primary", Deployment: "primary-upstream", Endpoints: []string{providerEndpointResponses}}}},
				{ID: "backup", Type: "openai-compatible", BaseURL: backup.URL, AuthType: "none", Models: []ProviderModelConfig{{PublicID: "gpt-backup", Deployment: "backup-upstream", Endpoints: []string{providerEndpointResponses}}}},
			},
			Fallbacks: []ProviderFallbackConfig{{Public: "gpt-primary", Chain: []ProviderFallbackChainEntry{{Provider: "primary", Model: "gpt-primary"}, {Provider: "backup", Model: "gpt-backup"}}}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	session := &responsesWebSocketSession{selectedOwner: providerModel{providerID: "backup"}, userAgent: "codex/1.0"}
	session.recordTurnStats(handler, "gpt-primary", http.StatusOK, responsesUsage{InputTokens: 2, OutputTokens: 3})
	snap := handler.stats.snapshot()
	if len(snap.ByProvider) == 0 || snap.ByProvider[0].Provider != "backup" {
		t.Fatalf("ByProvider = %#v, want backup attribution", snap.ByProvider)
	}
}

func TestDirectAnthropicFallbackAttributesUsageToBackupProvider(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":{"message":"primary down"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg-1","type":"message","role":"assistant","model":"backup-upstream","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":2,"output_tokens":3}}`))
	}))
	defer backup.Close()
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
			{ID: "primary", Type: "anthropic-compatible", Default: true, BaseURL: primary.URL, AuthType: "none", MessagesPath: "/v1/messages", Models: []ProviderModelConfig{{PublicID: "claude-primary", Deployment: "primary-upstream", Endpoints: []string{providerEndpointMessages}}}},
			{ID: "backup", Type: "anthropic-compatible", BaseURL: backup.URL, AuthType: "none", MessagesPath: "/v1/messages", Models: []ProviderModelConfig{{PublicID: "claude-backup", Deployment: "backup-upstream", Endpoints: []string{providerEndpointMessages}}}},
		}, Fallbacks: []ProviderFallbackConfig{{Public: "claude-primary", Chain: []ProviderFallbackChainEntry{{Provider: "primary", Model: "claude-primary"}, {Provider: "backup", Model: "claude-backup"}}}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.maxRetries = 1
	ctx, summary := WithRequestSummary(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-primary","max_tokens":128,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleAnthropicMessages(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if summary.provider != "backup" {
		t.Fatalf("summary provider = %q, want backup", summary.provider)
	}
}

func TestFailedFallbackErrorAttributesFinalProvider(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"primary limited"}}`))
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"message":"backup down"}}`))
	}))
	defer backup.Close()
	handler := newFallbackTestHandler(t, primary.URL, backup.URL)
	handler.maxRetries = 1
	ctx, summary := WithRequestSummary(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-primary","messages":[{"role":"user","content":"hi"}]}`)).WithContext(ctx)
	w := httptest.NewRecorder()
	handler.HandleOpenAIChatCompletions(w, req)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503, body = %s", w.Code, w.Body.String())
	}
	if summary.provider != "backup" {
		t.Fatalf("summary provider = %q, want backup", summary.provider)
	}
}

func TestFallbackChainsRejectDuplicatePublicDeclarations(t *testing.T) {
	_, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID: "primary", Type: "openai-compatible", Default: true, BaseURL: "https://primary.example.test", AuthType: "none", Models: []ProviderModelConfig{{PublicID: "gpt-primary", Endpoints: []string{providerEndpointChatCompletions}}},
		}}, Fallbacks: []ProviderFallbackConfig{
			{Public: "gpt-primary", Chain: []ProviderFallbackChainEntry{{Provider: "primary", Model: "gpt-primary"}}},
			{Public: "gpt-primary", Chain: []ProviderFallbackChainEntry{{Provider: "primary", Model: "gpt-primary"}}},
		}}),
	)
	if err == nil || !strings.Contains(err.Error(), "configured more than once") {
		t.Fatalf("NewProxyHandler() error = %v, want duplicate fallback error", err)
	}
}

func TestResponsesProviderFallbackStripsPreviousResponseIDAcrossProviders(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer primary.Close()
	var backupPayload map[string]json.RawMessage
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&backupPayload); err != nil {
			t.Fatalf("decode backup body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-backup","object":"response","status":"completed","model":"backup-upstream","output":[]}`))
	}))
	defer backup.Close()
	handler := newResponsesFallbackTestHandler(t, primary.URL, backup.URL)
	resp, owner, err := handler.postResponsesWithHeadersTracked(context.Background(), []byte(`{"model":"gpt-primary","previous_response_id":"resp-primary","input":"hi"}`), nil)
	if err != nil {
		t.Fatalf("postResponsesWithHeadersTracked() error = %v", err)
	}
	_ = resp.Body.Close()
	if owner.providerID != "backup" {
		t.Fatalf("owner.providerID = %q, want backup", owner.providerID)
	}
	if _, ok := backupPayload["previous_response_id"]; ok {
		t.Fatalf("backup payload kept previous_response_id: %s", backupPayload["previous_response_id"])
	}
}

func TestResponsesEncryptedContentRetryReturnsFallbackOwner(t *testing.T) {
	const encryptedToken = "gAAAAABencryptedReasoningPayloadpQ=="
	var primaryHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch primaryHits.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = fmt.Fprintf(w, `{"error":{"message":"The encrypted content %s could not be verified. Reason: Encrypted content could not be decrypted or parsed.","code":"invalid_request_body"}}`, encryptedToken)
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-backup","object":"response","status":"completed","model":"backup-upstream","output":[]}`))
	}))
	defer backup.Close()
	handler := newResponsesFallbackTestHandler(t, primary.URL, backup.URL)
	body := []byte(`{"model":"gpt-primary","input":[{"type":"reasoning","encrypted_content":"` + encryptedToken + `"},{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}]}`)
	resp, owner, err := handler.postResponsesWithHeadersTracked(context.Background(), body, nil)
	if err != nil {
		t.Fatalf("postResponsesWithHeadersTracked() error = %v", err)
	}
	_ = resp.Body.Close()
	if owner.providerID != "backup" {
		t.Fatalf("owner.providerID = %q, want backup", owner.providerID)
	}
}

func TestResponsesCompactionRetryReturnsFallbackOwner(t *testing.T) {
	var primaryHits atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch primaryHits.Add(1) {
		case 1:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"resp-summary","object":"response","status":"completed","model":"primary-upstream","output":[{"type":"message","content":[{"type":"output_text","text":"summary"}]}]}`))
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer primary.Close()
	backup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-backup","object":"response","status":"completed","model":"backup-upstream","output":[]}`))
	}))
	defer backup.Close()
	handler := newResponsesFallbackTestHandler(t, primary.URL, backup.URL)
	handler.responsesWS = ResponsesWebSocketConfig{AutoCompactKeepTail: 1}
	input := []json.RawMessage{json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"old"}]}`), json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"new"}]}`)}
	inputRaw, _ := json.Marshal(input)
	body := []byte(`{"model":"gpt-primary","input":` + string(inputRaw) + `}`)
	initial := &http.Response{StatusCode: http.StatusRequestEntityTooLarge, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"too large"}}`))}
	resp, owner, err := handler.maybeRetryCompactedResponsesRequest(context.Background(), context.Background(), body, nil, nil, initial)
	if err != nil {
		t.Fatalf("maybeRetryCompactedResponsesRequest() error = %v", err)
	}
	_ = resp.Body.Close()
	if owner.providerID != "backup" {
		t.Fatalf("owner.providerID = %q, want backup", owner.providerID)
	}
}

func TestFallbackAfterCanceledStreamingContextUsesStreamingTimeout(t *testing.T) {
	customTimeout := upstreamTimeout + time.Hour
	ctx := contextWithUpstreamAttemptTimeout(context.Background(), customTimeout)
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	attemptCtx, cleanup := context.WithTimeout(context.WithoutCancel(ctx), upstreamAttemptTimeoutFromContext(ctx, upstreamTimeout))
	defer cleanup()
	deadline, ok := attemptCtx.Deadline()
	if !ok {
		t.Fatal("fallback attempt context has no deadline")
	}
	remaining := time.Until(deadline)
	if remaining < upstreamTimeout+30*time.Minute {
		t.Fatalf("fallback attempt timeout = %s, want preserved streaming/custom timeout greater than upstreamTimeout %s", remaining, upstreamTimeout)
	}
}

func newResponsesFallbackTestHandler(t *testing.T, primaryURL, backupURL string) *ProxyHandler {
	t.Helper()
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
			{ID: "primary", Type: "openai-compatible", Default: true, BaseURL: primaryURL, AuthType: "none", Models: []ProviderModelConfig{{PublicID: "gpt-primary", Deployment: "primary-upstream", Endpoints: []string{providerEndpointResponses}}}},
			{ID: "backup", Type: "openai-compatible", BaseURL: backupURL, AuthType: "none", Models: []ProviderModelConfig{{PublicID: "gpt-backup", Deployment: "backup-upstream", Endpoints: []string{providerEndpointResponses}}}},
		}, Fallbacks: []ProviderFallbackConfig{{Public: "gpt-primary", Chain: []ProviderFallbackChainEntry{{Provider: "primary", Model: "gpt-primary"}, {Provider: "backup", Model: "gpt-backup"}}}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.maxRetries = 1
	return handler
}
