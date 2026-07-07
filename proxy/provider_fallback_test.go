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
