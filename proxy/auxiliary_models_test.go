package proxy

import (
	"context"
	"encoding/json"
	"fmt"
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

func newAuxiliaryCopilotCatalog(t *testing.T) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path != providerEndpointModels {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, `{"data":[{"id":"claude-opus-5.5","supported_endpoints":["/v1/messages","/chat/completions"]},{"id":"claude-sonnet-5","supported_endpoints":["/v1/messages","/chat/completions"]}]}`)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestAuxiliaryModelInstalledForNonDefaultCopilotProvider(t *testing.T) {
	copilot := newAuxiliaryCopilotCatalog(t)
	for _, tc := range []struct {
		name         string
		staticModels []string
		wantAdmitted bool
	}{
		{name: "copilot serves the classifier", staticModels: []string{"anthropic-model"}, wantAdmitted: true},
		{name: "another provider already claims the classifier", staticModels: []string{"anthropic-model", "claude-sonnet-5"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			static := make([]ProviderModelConfig, 0, len(tc.staticModels))
			for _, model := range tc.staticModels {
				static = append(static, ProviderModelConfig{PublicID: model, Deployment: model, Endpoints: []string{"/v1/messages"}})
			}
			h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard),
				WithCopilotBaseURL(copilot.URL), WithDeferredDynamicProviderModelValidation(true),
				WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
					{ID: "anthropic", Type: "anthropic-compatible", Default: true, BaseURL: "https://anthropic.example.test", AuthType: "none", Models: static},
					{ID: "copilot", Type: "copilot"},
				}}),
				WithAllowedModels("claude-opus-5.5"), WithAuxiliaryModels("claude-sonnet-5"))
			if err != nil {
				t.Fatalf("NewProxyHandler() error = %v", err)
			}
			t.Cleanup(h.BeginShutdown)
			if err := h.ValidateDynamicProviderModels(context.Background()); err != nil {
				t.Fatalf("ValidateDynamicProviderModels() error = %v", err)
			}
			if got := h.modelAllowedForRequest("claude-sonnet-5", providerEndpointMessages); got != tc.wantAdmitted {
				t.Fatalf("classifier admitted = %v, want %v", got, tc.wantAdmitted)
			}
			recorder := httptest.NewRecorder()
			h.HandleModels(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
			var listing struct {
				Data []struct {
					ID string `json:"id"`
				} `json:"data"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &listing); err != nil {
				t.Fatalf("decode /v1/models %s: %v", recorder.Body.String(), err)
			}
			for _, model := range listing.Data {
				if model.ID == "claude-sonnet-5" {
					t.Fatalf("/v1/models listed the auxiliary model: %s", recorder.Body.String())
				}
			}
		})
	}
}

func TestAuxiliaryModelThatIsAlsoPinnedIsInstalledOnce(t *testing.T) {
	copilot := newAuxiliaryCopilotCatalog(t)
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard),
		WithCopilotBaseURL(copilot.URL), WithDeferredDynamicProviderModelValidation(true),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{
			{ID: "anthropic", Type: "anthropic-compatible", Default: true, BaseURL: "https://anthropic.example.test", AuthType: "none",
				Models: []ProviderModelConfig{{PublicID: "anthropic-model", Deployment: "anthropic-model", Endpoints: []string{"/v1/messages"}}}},
			{ID: "copilot", Type: "copilot"},
		}}),
		WithAllowedModels("claude-sonnet-5"), WithAuxiliaryModels("claude-sonnet-5"))
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)
	if err := h.ValidateDynamicProviderModels(context.Background()); err != nil {
		t.Fatalf("ValidateDynamicProviderModels() error = %v; pinning the auxiliary model must not duplicate it", err)
	}
	recorder := httptest.NewRecorder()
	h.HandleModels(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusOK || strings.Count(recorder.Body.String(), `"id":"claude-sonnet-5"`) != 1 {
		t.Fatalf("/v1/models status/body = %d/%s, want claude-sonnet-5 listed once", recorder.Code, recorder.Body.String())
	}
}

func TestAuxiliaryModelInstalledForPinnedExplicitRouteProvider(t *testing.T) {
	copilot := newAuxiliaryCopilotCatalog(t)
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard),
		WithCopilotBaseURL(copilot.URL), WithDeferredDynamicProviderModelValidation(true),
		WithStateBindingsConfig(StateBindingsConfig{Mode: "memory"}),
		WithProvidersConfig(ProvidersConfig{
			SchemaVersion: ProvidersConfigSchemaVersion2,
			Providers: []ProviderConfig{
				{ID: "anthropic", Type: "anthropic-compatible", Default: true, BaseURL: "https://anthropic.example.test", AuthType: "none",
					Models: []ProviderModelConfig{{PublicID: "anthropic-model", Deployment: "anthropic-model", Endpoints: []string{"/v1/messages"}}}},
				{ID: "copilot", Type: "copilot"},
			},
			ModelRoutes: []ModelRouteConfig{{
				ID:        "my-opus",
				PublicID:  "my-opus",
				Endpoints: []string{providerEndpointChatCompletions},
				Targets:   []ModelRouteTargetConfig{{ID: "copilot", Provider: "copilot", UpstreamModel: "claude-opus-5.5"}},
			}},
		}),
		WithAllowedModels("my-opus"), WithAuxiliaryModels("claude-sonnet-5"))
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)
	if err := h.ValidateDynamicProviderModels(context.Background()); err != nil {
		t.Fatalf("ValidateDynamicProviderModels() error = %v", err)
	}
	if !h.modelAllowedForRequest("claude-sonnet-5", providerEndpointMessages) {
		t.Fatal("classifier was not admitted through the provider serving the pinned route")
	}
}

func TestAuxiliaryModelAliasReservedByDatedModelDoesNotFailStartup(t *testing.T) {
	copilot := newAuxiliaryCopilotCatalog(t)
	for _, deferred := range []bool{false, true} {
		t.Run(fmt.Sprintf("deferred=%v", deferred), func(t *testing.T) {
			h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard),
				WithCopilotBaseURL(copilot.URL), WithDeferredDynamicProviderModelValidation(deferred),
				WithStateBindingsConfig(StateBindingsConfig{Mode: "memory"}),
				WithProvidersConfig(ProvidersConfig{
					SchemaVersion: ProvidersConfigSchemaVersion2,
					Providers: []ProviderConfig{
						{ID: "copilot", Type: "copilot", Default: true},
						{ID: "anthropic", Type: "anthropic-compatible", BaseURL: "https://anthropic.example.test", AuthType: "none",
							Models: []ProviderModelConfig{{PublicID: "claude-sonnet-5-20260929", Deployment: "sonnet", Endpoints: []string{"/v1/messages"}}}},
					},
				}),
				WithAllowedModels("claude-opus-5.5"), WithAuxiliaryModels("claude-sonnet-5"))
			if err != nil {
				t.Fatalf("NewProxyHandler() error = %v; a reserved auxiliary alias must not fail startup", err)
			}
			t.Cleanup(h.BeginShutdown)
			if err := h.ValidateDynamicProviderModels(context.Background()); err != nil {
				t.Fatalf("ValidateDynamicProviderModels() error = %v", err)
			}
			if h.modelAllowedForRequest("claude-sonnet-5", providerEndpointMessages) {
				t.Fatal("classifier was admitted through a dated model owned by an out-of-scope provider")
			}
		})
	}
}

func TestAuxiliaryModelRequiresEveryExplicitRouteTargetInScope(t *testing.T) {
	copilot := newAuxiliaryCopilotCatalog(t)
	for _, tc := range []struct {
		name         string
		targets      []ModelRouteTargetConfig
		wantAdmitted bool
	}{
		{name: "same provider", targets: []ModelRouteTargetConfig{{ID: "copilot", Provider: "copilot", UpstreamModel: "claude-sonnet-5"}}, wantAdmitted: true},
		{name: "other provider", targets: []ModelRouteTargetConfig{{ID: "other", Provider: "other", UpstreamModel: "sonnet"}}},
		{name: "out-of-scope failover", targets: []ModelRouteTargetConfig{
			{ID: "copilot", Provider: "copilot", UpstreamModel: "claude-sonnet-5"},
			{ID: "other", Provider: "other", UpstreamModel: "sonnet"},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard),
				WithCopilotBaseURL(copilot.URL), WithDeferredDynamicProviderModelValidation(true),
				WithStateBindingsConfig(StateBindingsConfig{Mode: "memory"}),
				WithProvidersConfig(ProvidersConfig{
					SchemaVersion: ProvidersConfigSchemaVersion2,
					Providers: []ProviderConfig{
						{ID: "copilot", Type: "copilot", Default: true},
						{ID: "other", Type: "openai-compatible", BaseURL: "https://other.example.test/v1", AuthType: "none",
							Models: []ProviderModelConfig{{PublicID: "other-model", Deployment: "other-model", Endpoints: []string{providerEndpointChatCompletions}}}},
					},
					ModelRoutes: []ModelRouteConfig{{
						ID:        "classifier",
						PublicID:  "claude-sonnet-5",
						Endpoints: []string{providerEndpointChatCompletions},
						Targets:   tc.targets,
					}},
				}),
				WithAllowedModels("claude-opus-5.5"), WithAuxiliaryModels("claude-sonnet-5"))
			if err != nil {
				t.Fatalf("NewProxyHandler() error = %v", err)
			}
			t.Cleanup(h.BeginShutdown)
			if err := h.ValidateDynamicProviderModels(context.Background()); err != nil {
				t.Fatalf("ValidateDynamicProviderModels() error = %v", err)
			}
			if got := h.modelAllowedForRequest("claude-sonnet-5", providerEndpointMessages); got != tc.wantAdmitted {
				t.Fatalf("classifier admitted = %v, want %v", got, tc.wantAdmitted)
			}
		})
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
