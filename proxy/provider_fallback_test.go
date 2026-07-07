package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
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
