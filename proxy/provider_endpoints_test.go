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

func TestProviderEndpointKeysDefaultToBearerAuth(t *testing.T) {
	handler := &ProxyHandler{copilotURL: "https://copilot.example.test"}
	providers, _, _, err := handler.buildProviders(ProvidersConfig{Providers: []ProviderConfig{{
		ID:      "multi",
		Type:    "openai-compatible",
		Default: true,
		Endpoints: []ProviderEndpointConfig{{
			Name:    "east",
			BaseURL: "https://east.example.test/v1",
			APIKey:  "east-key",
		}},
		Models: []ProviderModelConfig{{PublicID: "gpt-test", Endpoints: []string{providerEndpointChatCompletions}}},
	}}})
	if err != nil {
		t.Fatalf("buildProviders() error = %v", err)
	}
	provider := providers["multi"]
	if provider.authType != providerAuthTypeBearer {
		t.Fatalf("authType = %q, want bearer", provider.authType)
	}
	req, err := handler.newProviderJSONRequest(context.Background(), provider, http.MethodPost, providerEndpointChatCompletions, []byte(`{"model":"gpt-test"}`), nil, "")
	if err != nil {
		t.Fatalf("newProviderJSONRequest() error = %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer east-key" {
		t.Fatalf("Authorization = %q, want endpoint bearer", got)
	}
}

func TestAzureEndpointOnlyOpenAIV1DoesNotRequireAPIVersion(t *testing.T) {
	handler := &ProxyHandler{copilotURL: "https://copilot.example.test"}
	providers, _, _, err := handler.buildProviders(ProvidersConfig{Providers: []ProviderConfig{{
		ID:      "azure",
		Type:    "azure-openai",
		Default: true,
		Endpoints: []ProviderEndpointConfig{{
			Name:    "east",
			BaseURL: "https://east.openai.azure.com/openai/v1",
			APIKey:  "east-key",
		}},
		Models: []ProviderModelConfig{{PublicID: "gpt-public", Deployment: "gpt-deployment", Endpoints: []string{providerEndpointChatCompletions}}},
	}}})
	if err != nil {
		t.Fatalf("buildProviders() error = %v", err)
	}
	provider := providers["azure"]
	req, err := handler.newProviderJSONRequest(context.Background(), provider, http.MethodPost, providerEndpointChatCompletions, []byte(`{"model":"gpt-public"}`), nil, "", provider.staticModels["gpt-public"])
	if err != nil {
		t.Fatalf("newProviderJSONRequest() error = %v", err)
	}
	if got := req.URL.String(); got != "https://east.openai.azure.com/openai/v1/chat/completions" {
		t.Fatalf("URL = %q, want Azure v1 chat path", got)
	}
	body, _ := io.ReadAll(req.Body)
	if got := extractRequestModel(body); got != "gpt-deployment" {
		t.Fatalf("body model = %q, want deployment rewrite", got)
	}
}

func TestAzureMixedEndpointShapesRewritePerSelectedEndpoint(t *testing.T) {
	handler := &ProxyHandler{copilotURL: "https://copilot.example.test"}
	providers, _, _, err := handler.buildProviders(ProvidersConfig{Providers: []ProviderConfig{{
		ID:         "azure",
		Type:       "azure-openai",
		Default:    true,
		APIVersion: "2025-04-01-preview",
		Endpoints: []ProviderEndpointConfig{
			{Name: "classic", BaseURL: "https://classic.openai.azure.com", APIKey: "classic-key"},
			{Name: "v1", BaseURL: "https://v1.openai.azure.com/openai/v1", APIKey: "v1-key"},
		},
		Models: []ProviderModelConfig{{PublicID: "gpt-public", Deployment: "gpt-deployment", Endpoints: []string{providerEndpointChatCompletions}}},
	}}})
	if err != nil {
		t.Fatalf("buildProviders() error = %v", err)
	}
	provider := providers["azure"]
	owner := provider.staticModels["gpt-public"]
	classicReq, err := handler.newProviderJSONRequest(context.Background(), provider, http.MethodPost, providerEndpointChatCompletions, []byte(`{"model":"gpt-public"}`), nil, "", owner)
	if err != nil {
		t.Fatalf("classic newProviderJSONRequest() error = %v", err)
	}
	classicBody, _ := io.ReadAll(classicReq.Body)
	if got := extractRequestModel(classicBody); got != "gpt-public" {
		t.Fatalf("classic body model = %q, want public model", got)
	}
	if !strings.Contains(classicReq.URL.String(), "/deployments/gpt-deployment/chat/completions") {
		t.Fatalf("classic URL = %q, want deployment path", classicReq.URL.String())
	}
	v1Req, err := handler.newProviderJSONRequest(context.Background(), provider, http.MethodPost, providerEndpointChatCompletions, []byte(`{"model":"gpt-public"}`), nil, "", owner)
	if err != nil {
		t.Fatalf("v1 newProviderJSONRequest() error = %v", err)
	}
	v1Body, _ := io.ReadAll(v1Req.Body)
	if got := extractRequestModel(v1Body); got != "gpt-deployment" {
		t.Fatalf("v1 body model = %q, want deployment rewrite", got)
	}
	if got := v1Req.URL.String(); got != "https://v1.openai.azure.com/openai/v1/chat/completions" {
		t.Fatalf("v1 URL = %q", got)
	}
}

func TestCopilotReadyzProbeUsesEndpointToken(t *testing.T) {
	handler := &ProxyHandler{auth: auth.NewTestAuthenticator("default-token"), copilotURL: "https://copilot.example.test"}
	providers, _, _, err := handler.buildProviders(ProvidersConfig{Providers: []ProviderConfig{{
		ID:        "copilot",
		Type:      "copilot",
		Default:   true,
		Endpoints: []ProviderEndpointConfig{{Name: "alice", APIKey: "alice-token"}},
	}}})
	if err != nil {
		t.Fatalf("buildProviders() error = %v", err)
	}
	req, err := handler.newProviderProbeRequest(context.Background(), providers["copilot"])
	if err != nil {
		t.Fatalf("newProviderProbeRequest() error = %v", err)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer alice-token" {
		t.Fatalf("Authorization = %q, want endpoint token", got)
	}
}

func TestLeastLatencyRotatesAfterRetryableFailure(t *testing.T) {
	var eastHits atomic.Int32
	east := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		eastHits.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited"}}`))
	}))
	defer east.Close()
	var westHits atomic.Int32
	west := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		westHits.Add(1)
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
			Selector: "least_latency",
			Endpoints: []ProviderEndpointConfig{
				{Name: "east", BaseURL: east.URL},
				{Name: "west", BaseURL: west.URL},
			},
			Models: []ProviderModelConfig{{PublicID: "gpt-public", Deployment: "gpt-upstream", Endpoints: []string{providerEndpointChatCompletions}}},
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.maxRetries = 2
	handler.retryBaseDelay = time.Millisecond
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-public","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	handler.HandleOpenAIChatCompletions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if eastHits.Load() != 1 || westHits.Load() != 1 {
		t.Fatalf("hits east/west = %d/%d, want one retry failover to west", eastHits.Load(), westHits.Load())
	}
}
