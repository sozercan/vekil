package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestAuxiliaryModelIsAdmittedThroughTheInScopeProvider(t *testing.T) {
	var mu sync.Mutex
	var served []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == providerEndpointModels {
			_, _ = io.WriteString(w, `{"data":[{"id":"claude-opus-5.5","supported_endpoints":["/v1/messages"]},{"id":"claude-sonnet-5","supported_endpoints":["/v1/messages"]},{"id":"claude-haiku-5.5","supported_endpoints":["/v1/messages"]}]}`)
			return
		}
		var payload struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&payload)
		mu.Lock()
		served = append(served, payload.Model)
		mu.Unlock()
		_, _ = io.WriteString(w, `{"id":"msg","type":"message","role":"assistant","model":"`+payload.Model+`","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer upstream.Close()
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard),
		WithCopilotBaseURL(upstream.URL), WithAllowedModels("claude-opus-5.5"), WithAuxiliaryModels("claude-sonnet-5"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	if err := h.ValidateDynamicProviderModels(context.Background()); err != nil {
		t.Fatalf("ValidateDynamicProviderModels() error = %v", err)
	}

	request := func(model string) int {
		body := `{"model":"` + model + `","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`
		recorder := httptest.NewRecorder()
		h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body)))
		return recorder.Code
	}
	if code := request("claude-sonnet-5"); code != http.StatusOK {
		t.Fatalf("auxiliary model status = %d, want 200", code)
	}
	if code := request("claude-haiku-5.5"); code == http.StatusOK {
		t.Fatal("non-auxiliary catalog model was admitted outside the launch scope")
	}
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(served, ",") != "claude-sonnet-5" {
		t.Fatalf("upstream served %v, want only the auxiliary model", served)
	}
}

func TestAuxiliaryModelDoesNotWidenProviderScope(t *testing.T) {
	copilot := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("unexpected Copilot request %s %s", r.Method, r.URL.Path)
		http.NotFound(w, r)
	}))
	defer copilot.Close()
	newHandler := func(t *testing.T, option Option) *ProxyHandler {
		t.Helper()
		h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard),
			WithCopilotBaseURL(copilot.URL), WithDeferredDynamicProviderModelValidation(true),
			WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
				{ID: "copilot", Type: "copilot"},
				{
					ID: "anthropic", Type: "anthropic-compatible", Default: true, BaseURL: "https://anthropic.example.test", AuthType: "none",
					Models: []ProviderModelConfig{{PublicID: "anthropic-pinned", Deployment: "pinned", Endpoints: []string{"/v1/messages"}}},
				},
			}}),
			WithAllowedModels("anthropic-pinned"), option)
		if err != nil {
			t.Fatalf("NewProxyHandler() error = %v", err)
		}
		t.Cleanup(h.BeginShutdown)
		return h
	}

	baseline := newHandler(t, WithAuxiliaryModels())
	h := newHandler(t, WithAuxiliaryModels("claude-sonnet-5"))
	if h.UsesCopilot() != baseline.UsesCopilot() || h.DynamicProviderValidationPending() != baseline.DynamicProviderValidationPending() {
		t.Fatal("an auxiliary model changed provider discovery or credential scope")
	}
	if h.modelAllowedForRequest("claude-sonnet-5", providerEndpointMessages) {
		t.Fatal("auxiliary model was admitted through a provider that serves no allowed model")
	}
}
