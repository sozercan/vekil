package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestProviderEndpointsWeightedSelectionAndEndpointKeys(t *testing.T) {
	handler := &ProxyHandler{copilotURL: "https://copilot.example.test"}
	providers, _, _, err := handler.buildProviders(ProvidersConfig{Providers: []ProviderConfig{{
		ID:       "multi",
		Type:     "openai-compatible",
		Default:  true,
		Selector: "weighted",
		AuthType: "bearer",
		Endpoints: []ProviderEndpointConfig{
			{Name: "east", BaseURL: "https://east.example.test/v1", APIKey: "east-key", Weight: 2},
			{Name: "west", BaseURL: "https://west.example.test/v1", APIKey: "west-key", Weight: 1},
		},
		Models: []ProviderModelConfig{{PublicID: "gpt-test", Endpoints: []string{providerEndpointChatCompletions}}},
	}}})
	if err != nil {
		t.Fatalf("buildProviders() error = %v", err)
	}
	provider := providers["multi"]
	if provider == nil || len(provider.endpoints) != 2 || provider.selectorName != "weighted" {
		t.Fatalf("provider endpoints = %#v selector=%q, want two weighted endpoints", provider, provider.selectorName)
	}

	var gotURLs []string
	var gotAuth []string
	for range 3 {
		req, err := handler.newProviderJSONRequest(context.Background(), provider, http.MethodPost, providerEndpointChatCompletions, []byte(`{"model":"gpt-test"}`), nil, "")
		if err != nil {
			t.Fatalf("newProviderJSONRequest() error = %v", err)
		}
		gotURLs = append(gotURLs, req.URL.String())
		gotAuth = append(gotAuth, req.Header.Get("Authorization"))
	}
	wantURLs := []string{
		"https://east.example.test/v1/chat/completions",
		"https://east.example.test/v1/chat/completions",
		"https://west.example.test/v1/chat/completions",
	}
	for i := range wantURLs {
		if gotURLs[i] != wantURLs[i] {
			t.Fatalf("request URLs = %v, want %v", gotURLs, wantURLs)
		}
	}
	wantAuth := []string{"Bearer east-key", "Bearer east-key", "Bearer west-key"}
	for i := range wantAuth {
		if gotAuth[i] != wantAuth[i] {
			t.Fatalf("auth headers = %v, want %v", gotAuth, wantAuth)
		}
	}
}

func TestProviderEndpointHealthSkipsQuarantinedEndpoint(t *testing.T) {
	var eastHits atomic.Int32
	east := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eastHits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer east.Close()

	var westHits atomic.Int32
	west := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		westHits.Add(1)
		body, _ := io.ReadAll(r.Body)
		var payload map[string]json.RawMessage
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if got := strings.Trim(string(payload["model"]), `"`); got != "gpt-upstream" {
			t.Fatalf("upstream model = %q, want gpt-upstream", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-ok","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer west.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:       "multi",
			Type:     "openai-compatible",
			Default:  true,
			AuthType: "none",
			Endpoints: []ProviderEndpointConfig{
				{Name: "east", BaseURL: east.URL, Health: ProviderEndpointHealthConfig{ErrorBudget: "1/m", Cooldown: "1h"}},
				{Name: "west", BaseURL: west.URL, Health: ProviderEndpointHealthConfig{ErrorBudget: "1/m", Cooldown: "1h"}},
			},
			Models: []ProviderModelConfig{{PublicID: "gpt-public", Deployment: "gpt-upstream", Endpoints: []string{providerEndpointChatCompletions}}},
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.maxRetries = 2
	handler.retryBaseDelay = time.Millisecond

	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-public","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		handler.HandleOpenAIChatCompletions(w, req)
		if w.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, body = %s", i+1, w.Code, w.Body.String())
		}
	}

	if got := eastHits.Load(); got != 1 {
		t.Fatalf("east hits = %d, want exactly one quarantining hit", got)
	}
	if got := westHits.Load(); got != 2 {
		t.Fatalf("west hits = %d, want both successful requests", got)
	}
}

func TestProviderEndpointsYAMLConfig(t *testing.T) {
	providersPath := t.TempDir() + "/providers.yaml"
	body := []byte(`providers:
  - id: azure
    type: azure-openai
    default: true
    selector: least_latency
    api_version: 2025-04-01-preview
    endpoints:
      - name: east
        base_url: https://east.openai.azure.com
        api_key: east-key
        weight: 2
        health:
          error_budget: "2/s"
          cooldown: 5s
      - name: west
        base_url: https://west.openai.azure.com
        api_key: west-key
    models:
      - public_id: gpt-test
        deployment: gpt-test
        endpoints: [/chat/completions]
`)
	if err := os.WriteFile(providersPath, body, 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	cfg, err := LoadProvidersConfigFile(providersPath)
	if err != nil {
		t.Fatalf("LoadProvidersConfigFile() error = %v", err)
	}
	if got := cfg.Providers[0].Selector; got != "least_latency" {
		t.Fatalf("selector = %q, want least_latency", got)
	}
	if got := len(cfg.Providers[0].Endpoints); got != 2 {
		t.Fatalf("endpoints = %d, want 2", got)
	}
	handler := &ProxyHandler{copilotURL: "https://copilot.example.test"}
	providers, _, _, err := handler.buildProviders(cfg)
	if err != nil {
		t.Fatalf("buildProviders() error = %v", err)
	}
	provider := providers["azure"]
	if provider == nil || len(provider.endpoints) != 2 || provider.selectorName != "least_latency" {
		t.Fatalf("provider endpoints = %#v selector=%q", provider, provider.selectorName)
	}
}

func TestCopilotEndpointOverridesToken(t *testing.T) {
	handler := &ProxyHandler{auth: auth.NewTestAuthenticator("default-token"), copilotURL: "https://copilot.example.test"}
	providers, _, _, err := handler.buildProviders(ProvidersConfig{Providers: []ProviderConfig{{
		ID:       "copilot",
		Type:     "copilot",
		Default:  true,
		Selector: "round_robin",
		Endpoints: []ProviderEndpointConfig{
			{Name: "alice", APIKey: "alice-token"},
			{Name: "bob", APIKey: "bob-token"},
		},
	}}})
	if err != nil {
		t.Fatalf("buildProviders() error = %v", err)
	}
	provider := providers["copilot"]
	var got []string
	for range 2 {
		req, err := handler.newProviderJSONRequest(context.Background(), provider, http.MethodGet, "/models", nil, nil, "")
		if err != nil {
			t.Fatalf("newProviderJSONRequest() error = %v", err)
		}
		got = append(got, req.Header.Get("Authorization"))
	}
	want := []string{"Bearer alice-token", "Bearer bob-token"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Authorization headers = %v, want %v", got, want)
		}
	}
}
