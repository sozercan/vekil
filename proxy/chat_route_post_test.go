package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestPostResolvedProviderRequestUsesCapturedRouteAfterCatalogReplacement(t *testing.T) {
	dropSampling := true
	parallel := false
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/custom/responses"; got != want {
			t.Fatalf("upstream path = %q, want %q", got, want)
		}
		if got := r.Header.Get("X-Test-Route"); got != "captured" {
			t.Fatalf("X-Test-Route = %q, want captured", got)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer fixture" {
			t.Fatalf("Authorization = %q, want generic bearer auth", got)
		}
		if got := r.Header.Get("X-Provider-Header"); got != "configured" {
			t.Fatalf("X-Provider-Header = %q, want configured", got)
		}
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-test","status":"completed","output":[]}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:            "generic",
			Type:          "openai-compatible",
			Default:       true,
			BaseURL:       upstream.URL,
			AuthType:      "bearer",
			APIKey:        "fixture",
			ExtraHeaders:  map[string]string{"X-Provider-Header": "configured"},
			ResponsesPath: "/custom/responses",
			Models: []ProviderModelConfig{{
				PublicID:           "public-model",
				Deployment:         "deployment-a",
				Endpoints:          []string{providerEndpointResponses},
				DropSamplingParams: &dropSampling,
				ParallelToolCalls:  &parallel,
			}},
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	route, err := handler.resolveChatRoute(context.Background(), "public-model")
	if err != nil {
		t.Fatalf("resolveChatRoute() error = %v", err)
	}
	if route.backend != chatBackendResponses {
		t.Fatalf("route.backend = %v, want Responses", route.backend)
	}

	if err := handler.providerSetup().replaceProviderModels("generic", []providerModel{{
		publicID:           "public-model",
		upstreamModel:      "deployment-b",
		providerID:         "generic",
		supportedEndpoints: []string{providerEndpointChatCompletions},
	}}); err != nil {
		t.Fatalf("replaceProviderModels() error = %v", err)
	}

	body := []byte(`{"model":"public-model","messages":[{"role":"user","content":"hello"}],"temperature":0.2,"top_p":0.9,"parallel_tool_calls":true}`)
	resp, err := handler.postResolvedProviderRequest(
		context.Background(),
		route.provider,
		route.owner,
		route.nativeEndpoint,
		body,
		http.Header{"X-Test-Route": []string{"captured"}},
	)
	if err != nil {
		t.Fatalf("postResolvedProviderRequest() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(upstreamBody, &payload); err != nil {
		t.Fatalf("decode upstream body: %v", err)
	}
	if got := rawJSONString(payload["model"]); got != "deployment-a" {
		t.Fatalf("upstream model = %q, want captured deployment-a", got)
	}
	for _, field := range []string{"temperature", "top_p", "parallel_tool_calls"} {
		if _, ok := payload[field]; ok {
			t.Fatalf("captured provider policy did not remove %q", field)
		}
	}
}

func TestPostResolvedProviderRequestPreservesAzureClassicChatDeployment(t *testing.T) {
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/openai/deployments/gpt-prod/chat/completions"; got != want {
			t.Fatalf("upstream path = %q, want %q", got, want)
		}
		if got := r.URL.Query().Get("api-version"); got != "2025-04-01-preview" {
			t.Fatalf("api-version = %q, want 2025-04-01-preview", got)
		}
		if got := r.Header.Get("api-key"); got != "fixture" {
			t.Fatalf("api-key = %q, want fixture", got)
		}
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chat-test","choices":[]}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:         "azure",
			Type:       "azure-openai",
			Default:    true,
			BaseURL:    upstream.URL + "/openai",
			APIVersion: "2025-04-01-preview",
			APIKey:     "fixture",
			Models: []ProviderModelConfig{{
				PublicID:   "gpt-public",
				Deployment: "gpt-prod",
				Endpoints:  []string{providerEndpointChatCompletions},
			}},
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	route, err := handler.resolveChatRoute(context.Background(), "gpt-public")
	if err != nil {
		t.Fatalf("resolveChatRoute() error = %v", err)
	}
	body := []byte(`{"model":"gpt-public","messages":[{"role":"user","content":"hello"}]}`)
	resp, err := handler.postResolvedProviderRequest(context.Background(), route.provider, route.owner, route.nativeEndpoint, body, nil)
	if err != nil {
		t.Fatalf("postResolvedProviderRequest() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := extractRequestModel(upstreamBody); got != "gpt-public" {
		t.Fatalf("upstream body model = %q, want unchanged public model", got)
	}
}

func TestPostResolvedProviderRequestUsesAzureResponsesPathAndBodyRewrite(t *testing.T) {
	var upstreamBody []byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.Path, "/openai/responses"; got != want {
			t.Fatalf("upstream path = %q, want %q", got, want)
		}
		if got := r.URL.Query().Get("api-version"); got != "2025-04-01-preview" {
			t.Fatalf("api-version = %q, want 2025-04-01-preview", got)
		}
		var err error
		upstreamBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-test","status":"completed","output":[]}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:         "azure",
			Type:       "azure-openai",
			Default:    true,
			BaseURL:    upstream.URL + "/openai",
			APIVersion: "2025-04-01-preview",
			APIKey:     "fixture",
			Models: []ProviderModelConfig{{
				PublicID:   "gpt-public",
				Deployment: "gpt-responses-prod",
				Endpoints:  []string{providerEndpointResponses},
			}},
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	route, err := handler.resolveChatRoute(context.Background(), "gpt-public")
	if err != nil {
		t.Fatalf("resolveChatRoute() error = %v", err)
	}
	body := []byte(`{"model":"gpt-public","messages":[{"role":"user","content":"hello"}]}`)
	resp, err := handler.postResolvedProviderRequest(context.Background(), route.provider, route.owner, route.nativeEndpoint, body, nil)
	if err != nil {
		t.Fatalf("postResolvedProviderRequest() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := extractRequestModel(upstreamBody); got != "gpt-responses-prod" {
		t.Fatalf("upstream body model = %q, want deployment rewrite", got)
	}
}

func TestPostResolvedProviderRequestUsesSelectedCopilotHeaderProfileOnEveryRetry(t *testing.T) {
	var intents []string
	var requestIDs []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		intents = append(intents, r.Header.Get("openai-intent"))
		requestIDs = append(requestIDs, r.Header.Get("x-request-id"))
		if len(intents) == 1 {
			http.Error(w, "retry", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-test","status":"completed","output":[]}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.New(logger.LevelError),
		WithCopilotBaseURL(upstream.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.maxRetries = 2
	handler.retryBaseDelay = time.Millisecond
	provider := handler.providerSetup().defaultProvider()
	provider.headerProfiles = CopilotHeaderProfilesConfig{
		ChatCompletions: CopilotHeaderConfig{OpenAIIntent: "chat-intent"},
		Responses:       CopilotHeaderConfig{OpenAIIntent: "responses-intent"},
	}
	owner := providerModel{
		publicID:           "responses-model",
		upstreamModel:      "responses-model",
		providerID:         provider.id,
		supportedEndpoints: []string{providerEndpointResponses},
	}
	route, err := chooseChatRoute(provider, owner, true, owner.publicID)
	if err != nil {
		t.Fatalf("chooseChatRoute() error = %v", err)
	}

	body := []byte(`{"model":"responses-model","messages":[{"role":"user","content":"hello"}]}`)
	resp, err := handler.postResolvedProviderRequest(context.Background(), route.provider, route.owner, route.nativeEndpoint, body, nil)
	if err != nil {
		t.Fatalf("postResolvedProviderRequest() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if len(intents) != 2 || intents[0] != "responses-intent" || intents[1] != "responses-intent" {
		t.Fatalf("retry intents = %v, want Responses profile on both attempts", intents)
	}
	if len(requestIDs) != 2 || requestIDs[0] == "" || requestIDs[1] == "" || requestIDs[0] == requestIDs[1] {
		t.Fatalf("retry request IDs = %v, want fresh non-empty IDs", requestIDs)
	}
}

func TestPrepareResolvedProviderRequestBodyRewritesNonClassicProviderModels(t *testing.T) {
	tests := []struct {
		name     string
		provider *providerRuntime
		endpoint string
	}{
		{
			name: "Azure v1 Chat",
			provider: &providerRuntime{
				id:      "azure-v1",
				kind:    providerTypeAzureOpenAI,
				baseURL: "https://example.openai.azure.com/openai/v1",
			},
			endpoint: providerEndpointChatCompletions,
		},
		{
			name: "OpenAI Codex Responses",
			provider: &providerRuntime{
				id:   "codex",
				kind: providerTypeOpenAICodex,
			},
			endpoint: providerEndpointResponses,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"model":"public-model","messages":[{"role":"user","content":"hello"}]}`)
			owner := providerModel{
				publicID:      "public-model",
				upstreamModel: "upstream-model",
				providerID:    tt.provider.id,
			}
			prepared, err := prepareResolvedProviderRequestBody(body, "public-model", tt.endpoint, tt.provider, owner)
			if err != nil {
				t.Fatalf("prepareResolvedProviderRequestBody() error = %v", err)
			}
			if got := extractRequestModel(prepared); got != "upstream-model" {
				t.Fatalf("prepared model = %q, want upstream-model", got)
			}
		})
	}
}
