package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
	"github.com/sozercan/vekil/server"
)

const (
	integrationRouteID     = "integration-route"
	integrationPublicModel = "integration-public-model"
	integrationPrimary     = "physical-primary-model"
	integrationSecondary   = "physical-secondary-model"
)

type integrationUpstreamRequest struct {
	method      string
	path        string
	model       string
	contentType string
	decodeErr   error
}

func TestSchemaV2ProviderRoutesFileIntegration(t *testing.T) {
	tests := []struct {
		name string
		ext  string
		body func(primaryURL, secondaryURL string) string
	}{
		{
			name: "strict JSON",
			ext:  ".json",
			body: schemaV2ProviderRoutesJSON,
		},
		{
			name: "strict YAML",
			ext:  ".yaml",
			body: schemaV2ProviderRoutesYAML,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var primaryCalls atomic.Int32
			var secondaryCalls atomic.Int32
			primaryRequests := make(chan integrationUpstreamRequest, 2)
			secondaryRequests := make(chan integrationUpstreamRequest, 2)

			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				primaryRequests <- captureIntegrationUpstreamRequest(r)
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = io.WriteString(w, `{"error":{"message":"primary quota","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
			}))
			t.Cleanup(primary.Close)

			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondaryCalls.Add(1)
				secondaryRequests <- captureIntegrationUpstreamRequest(r)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("OpenAI-Model", integrationSecondary)
				w.Header().Set("X-OpenAI-Model", integrationSecondary)
				_, _ = fmt.Fprintf(w, `{"id":"resp-secondary","object":"response","model":%q,"status":"completed","output":[]}`, integrationSecondary)
			}))
			t.Cleanup(secondary.Close)

			configPath := filepath.Join(t.TempDir(), "providers"+tc.ext)
			if err := os.WriteFile(configPath, []byte(tc.body(primary.URL, secondary.URL)), 0o600); err != nil {
				t.Fatalf("write providers config: %v", err)
			}

			validateConfigThroughRealCommand(t, configPath)

			cfg, err := proxy.LoadProvidersConfigFile(configPath)
			if err != nil {
				t.Fatalf("LoadProvidersConfigFile() error = %v", err)
			}
			assertDecodedIntegrationConfig(t, cfg)

			srv, err := server.New(
				auth.NewTestAuthenticator("unused-test-token"),
				logger.NewWithWriter(logger.LevelError, io.Discard),
				"127.0.0.1",
				"0",
				server.WithProxyOptions(proxy.WithProvidersConfig(cfg)),
			)
			if err != nil {
				t.Fatalf("server.New() error = %v", err)
			}
			if err := srv.Start(); err != nil {
				t.Fatalf("Server.Start() error = %v", err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				defer cancel()
				if err := srv.Stop(ctx); err != nil {
					t.Errorf("Server.Stop() error = %v", err)
				}
			})

			client := &http.Client{Timeout: 3 * time.Second}
			baseURL := "http://" + srv.Addr()
			assertIntegrationServerReady(t, client, baseURL)
			assertIntegrationCatalogIdentity(t, client, baseURL)
			assertIntegrationResponsesFailover(t, client, baseURL)

			if got := primaryCalls.Load(); got != 1 {
				t.Fatalf("primary upstream calls = %d, want 1", got)
			}
			if got := secondaryCalls.Load(); got != 1 {
				t.Fatalf("secondary upstream calls = %d, want 1", got)
			}
			assertIntegrationUpstreamRequest(t, <-primaryRequests, integrationPrimary)
			assertIntegrationUpstreamRequest(t, <-secondaryRequests, integrationSecondary)
		})
	}
}

func TestConfigValidateSchemaV2FailoverExampleIntegration(t *testing.T) {
	t.Setenv("AZURE_PRIMARY_API_KEY", "dummy-primary-key")
	t.Setenv("AZURE_SECONDARY_API_KEY", "dummy-secondary-key")

	path := filepath.Join("examples", "provider-routing-failover.yaml")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat schema-v2 example: %v", err)
	}
	validateConfigThroughRealCommand(t, path)
}

func validateConfigThroughRealCommand(t *testing.T, path string) {
	t.Helper()

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	deps := defaultConfigValidateDeps()
	deps.stdout = &stdout
	deps.stderr = &stderr

	code := runConfigWithDeps([]string{"validate", "--providers-config", path}, deps)
	if code != 0 {
		t.Fatalf("config validate exit code = %d, want 0\nstderr: %s", code, stderr.String())
	}
	if got := stderr.String(); got != "" {
		t.Fatalf("config validate stderr = %q, want empty", got)
	}
	if got := stdout.String(); !strings.Contains(got, "Providers config is valid: "+path) {
		t.Fatalf("config validate stdout = %q, want success for %s", got, path)
	}
}

func assertDecodedIntegrationConfig(t *testing.T, cfg proxy.ProvidersConfig) {
	t.Helper()

	if got := cfg.EffectiveSchemaVersion(); got != proxy.ProvidersConfigSchemaVersion2 {
		t.Fatalf("EffectiveSchemaVersion() = %d, want %d", got, proxy.ProvidersConfigSchemaVersion2)
	}
	if len(cfg.Providers) != 2 {
		t.Fatalf("providers = %d, want 2", len(cfg.Providers))
	}
	if !cfg.Providers[0].Default || cfg.Providers[0].ID != "primary" || cfg.Providers[1].ID != "secondary" {
		t.Fatalf("decoded providers = %+v", cfg.Providers)
	}
	if len(cfg.ModelRoutes) != 1 {
		t.Fatalf("model routes = %d, want 1", len(cfg.ModelRoutes))
	}
	route := cfg.ModelRoutes[0]
	if route.ID != integrationRouteID || route.PublicID != integrationPublicModel || route.Name != "Integration Public Model" {
		t.Fatalf("decoded route identity = %+v", route)
	}
	if !reflect.DeepEqual(route.Endpoints, []string{"/responses"}) {
		t.Fatalf("decoded route endpoints = %v", route.Endpoints)
	}
	if !reflect.DeepEqual(route.ReasoningEffort, []string{"low", "medium", "high"}) {
		t.Fatalf("decoded reasoning effort = %v", route.ReasoningEffort)
	}
	if route.ParallelToolCalls == nil || !*route.ParallelToolCalls {
		t.Fatalf("decoded parallel_tool_calls = %v, want true", route.ParallelToolCalls)
	}
	if route.Vision == nil || *route.Vision {
		t.Fatalf("decoded vision = %v, want false", route.Vision)
	}
	if route.ContextWindow == nil || *route.ContextWindow != 123456 {
		t.Fatalf("decoded context_window = %v, want 123456", route.ContextWindow)
	}
	if route.ModelPickerEnabled == nil || !*route.ModelPickerEnabled || route.ModelPickerCategory != "powerful" {
		t.Fatalf("decoded model picker fields = enabled:%v category:%q", route.ModelPickerEnabled, route.ModelPickerCategory)
	}
	if len(route.Targets) != 2 || route.Targets[0].UpstreamModel != integrationPrimary || route.Targets[1].UpstreamModel != integrationSecondary {
		t.Fatalf("decoded route targets = %+v", route.Targets)
	}
	if route.Routing.Mode != "priority_failover" || route.Routing.MaxTargetAttempts != 2 || route.Routing.MaxUpstreamSends != 2 {
		t.Fatalf("decoded routing policy = %+v", route.Routing)
	}
}

func assertIntegrationServerReady(t *testing.T, client *http.Client, baseURL string) {
	t.Helper()

	for _, path := range []string{"/healthz", "/readyz"} {
		resp, err := client.Get(baseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read %s response: %v", path, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200: %s", path, resp.StatusCode, body)
		}
	}
}

func assertIntegrationCatalogIdentity(t *testing.T, client *http.Client, baseURL string) {
	t.Helper()

	resp, err := client.Get(baseURL + "/v1/models")
	if err != nil {
		t.Fatalf("GET /v1/models: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /v1/models response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /v1/models status = %d, want 200: %s", resp.StatusCode, body)
	}

	var catalog struct {
		Object string `json:"object"`
		Data   []struct {
			ID                 string   `json:"id"`
			Object             string   `json:"object"`
			OwnedBy            string   `json:"owned_by"`
			Name               string   `json:"name"`
			SupportedEndpoints []string `json:"supported_endpoints"`
			Capabilities       struct {
				Limits struct {
					MaxContextWindowTokens int64 `json:"max_context_window_tokens"`
				} `json:"limits"`
				Supports struct {
					ParallelToolCalls bool     `json:"parallel_tool_calls"`
					ReasoningEffort   []string `json:"reasoning_effort"`
					Vision            bool     `json:"vision"`
				} `json:"supports"`
			} `json:"capabilities"`
			ModelPickerEnabled  bool   `json:"model_picker_enabled"`
			ModelPickerCategory string `json:"model_picker_category"`
		} `json:"data"`
		Models []struct {
			Slug                      string `json:"slug"`
			DisplayName               string `json:"display_name"`
			SupportsParallelToolCalls bool   `json:"supports_parallel_tool_calls"`
			ContextWindow             *int64 `json:"context_window"`
			MaxContextWindow          *int64 `json:"max_context_window"`
		} `json:"models"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		t.Fatalf("decode /v1/models response: %v\nbody: %s", err, body)
	}
	if catalog.Object != "list" || len(catalog.Data) != 1 || len(catalog.Models) != 1 {
		t.Fatalf("catalog shape = object:%q data:%d models:%d body:%s", catalog.Object, len(catalog.Data), len(catalog.Models), body)
	}

	model := catalog.Data[0]
	if model.ID != integrationPublicModel || model.Object != "model" || model.OwnedBy != integrationRouteID || model.Name != "Integration Public Model" {
		t.Fatalf("catalog route identity = %+v", model)
	}
	if !reflect.DeepEqual(model.SupportedEndpoints, []string{"/responses"}) {
		t.Fatalf("catalog supported_endpoints = %v", model.SupportedEndpoints)
	}
	if !model.Capabilities.Supports.ParallelToolCalls || model.Capabilities.Supports.Vision {
		t.Fatalf("catalog supports = %+v", model.Capabilities.Supports)
	}
	if !reflect.DeepEqual(model.Capabilities.Supports.ReasoningEffort, []string{"low", "medium", "high"}) {
		t.Fatalf("catalog reasoning_effort = %v", model.Capabilities.Supports.ReasoningEffort)
	}
	if model.Capabilities.Limits.MaxContextWindowTokens != 123456 {
		t.Fatalf("catalog max_context_window_tokens = %d, want 123456", model.Capabilities.Limits.MaxContextWindowTokens)
	}
	if !model.ModelPickerEnabled || model.ModelPickerCategory != "powerful" {
		t.Fatalf("catalog model picker fields = enabled:%v category:%q", model.ModelPickerEnabled, model.ModelPickerCategory)
	}

	codexModel := catalog.Models[0]
	if codexModel.Slug != integrationPublicModel || codexModel.DisplayName != "Integration Public Model" || !codexModel.SupportsParallelToolCalls {
		t.Fatalf("Codex catalog identity = %+v", codexModel)
	}
	if codexModel.ContextWindow == nil || *codexModel.ContextWindow != 123456 || codexModel.MaxContextWindow == nil || *codexModel.MaxContextWindow != 123456 {
		t.Fatalf("Codex catalog context windows = context:%v max:%v", codexModel.ContextWindow, codexModel.MaxContextWindow)
	}
}

func assertIntegrationResponsesFailover(t *testing.T, client *http.Client, baseURL string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodPost, baseURL+"/v1/responses", strings.NewReader(`{"model":"`+integrationPublicModel+`","input":"hello"}`))
	if err != nil {
		t.Fatalf("create /v1/responses request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/responses: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /v1/responses response: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /v1/responses status = %d, want 200: %s", resp.StatusCode, body)
	}
	if requestID := resp.Header.Get("X-Vekil-Request-ID"); requestID == "" {
		t.Fatal("POST /v1/responses missing X-Vekil-Request-ID")
	}
	if got := resp.Header.Get("OpenAI-Model"); got != integrationPublicModel {
		t.Fatalf("OpenAI-Model = %q, want %q", got, integrationPublicModel)
	}
	if got := resp.Header.Get("X-OpenAI-Model"); got != integrationPublicModel {
		t.Fatalf("X-OpenAI-Model = %q, want %q", got, integrationPublicModel)
	}

	var result struct {
		ID     string `json:"id"`
		Model  string `json:"model"`
		Status string `json:"status"`
	}
	if err := json.Unmarshal(body, &result); err != nil {
		t.Fatalf("decode /v1/responses response: %v\nbody: %s", err, body)
	}
	if result.ID != "resp-secondary" || result.Model != integrationPublicModel || result.Status != "completed" {
		t.Fatalf("normalized failover response = %+v, body: %s", result, body)
	}
	if bytes.Contains(body, []byte(integrationSecondary)) || bytes.Contains(body, []byte(integrationPrimary)) {
		t.Fatalf("physical model identity leaked in response: %s", body)
	}
}

func captureIntegrationUpstreamRequest(r *http.Request) integrationUpstreamRequest {
	captured := integrationUpstreamRequest{
		method:      r.Method,
		path:        r.URL.Path,
		contentType: r.Header.Get("Content-Type"),
	}
	var payload struct {
		Model string `json:"model"`
	}
	captured.decodeErr = json.NewDecoder(r.Body).Decode(&payload)
	captured.model = payload.Model
	return captured
}

func assertIntegrationUpstreamRequest(t *testing.T, got integrationUpstreamRequest, wantModel string) {
	t.Helper()

	if got.decodeErr != nil {
		t.Fatalf("decode upstream request: %v", got.decodeErr)
	}
	if got.method != http.MethodPost || got.path != "/v1/responses" || got.model != wantModel {
		t.Fatalf("upstream request = method:%q path:%q model:%q, want POST /v1/responses %q", got.method, got.path, got.model, wantModel)
	}
	if !strings.HasPrefix(got.contentType, "application/json") {
		t.Fatalf("upstream Content-Type = %q, want application/json", got.contentType)
	}
}

func schemaV2ProviderRoutesJSON(primaryURL, secondaryURL string) string {
	return fmt.Sprintf(`{
  "schema_version": 2,
  "providers": [
    {
      "id": "primary",
      "type": "openai-compatible",
      "default": true,
      "base_url": %q,
      "auth_type": "none"
    },
    {
      "id": "secondary",
      "type": "openai-compatible",
      "base_url": %q,
      "auth_type": "none"
    }
  ],
  "model_routes": [
    {
      "id": %q,
      "public_id": %q,
      "name": "Integration Public Model",
      "endpoints": ["/responses"],
      "reasoning_effort": ["low", "medium", "high"],
      "parallel_tool_calls": true,
      "vision": false,
      "context_window": 123456,
      "model_picker_enabled": true,
      "model_picker_category": "powerful",
      "targets": [
        {
          "id": "primary-target",
          "provider": "primary",
          "upstream_model": %q
        },
        {
          "id": "secondary-target",
          "provider": "secondary",
          "upstream_model": %q
        }
      ],
      "routing": {
        "mode": "priority_failover",
        "max_target_attempts": 2,
        "max_upstream_sends": 2
      }
    }
  ]
}
`, primaryURL+"/v1", secondaryURL+"/v1", integrationRouteID, integrationPublicModel, integrationPrimary, integrationSecondary)
}

func schemaV2ProviderRoutesYAML(primaryURL, secondaryURL string) string {
	return fmt.Sprintf(`schema_version: 2
providers:
  - id: primary
    type: openai-compatible
    default: true
    base_url: %q
    auth_type: none
  - id: secondary
    type: openai-compatible
    base_url: %q
    auth_type: none
model_routes:
  - id: %s
    public_id: %s
    name: Integration Public Model
    endpoints:
      - /responses
    reasoning_effort:
      - low
      - medium
      - high
    parallel_tool_calls: true
    vision: false
    context_window: 123456
    model_picker_enabled: true
    model_picker_category: powerful
    targets:
      - id: primary-target
        provider: primary
        upstream_model: %s
      - id: secondary-target
        provider: secondary
        upstream_model: %s
    routing:
      mode: priority_failover
      max_target_attempts: 2
      max_upstream_sends: 2
`, primaryURL+"/v1", secondaryURL+"/v1", integrationRouteID, integrationPublicModel, integrationPrimary, integrationSecondary)
}
