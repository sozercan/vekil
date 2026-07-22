package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestLoadProvidersConfigFileSchemaVersionsAndDefaults(t *testing.T) {
	tests := []struct {
		name        string
		ext         string
		body        string
		wantVersion int
		wantRoute   bool
		wantErr     string
	}{
		{
			name:        "omitted schema version is v1",
			ext:         ".json",
			body:        `{"providers":[]}`,
			wantVersion: ProvidersConfigSchemaVersion1,
		},
		{
			name:        "explicit schema version 1 remains compatible",
			ext:         ".yaml",
			body:        "schema_version: 1\nproviders: []\n",
			wantVersion: ProvidersConfigSchemaVersion1,
		},
		{
			name:    "model routes require version 2",
			ext:     ".json",
			body:    `{"schema_version":1,"providers":[],"model_routes":[]}`,
			wantErr: "model_routes: requires schema_version: 2",
		},
		{
			name:    "explicit schema version zero is invalid",
			ext:     ".json",
			body:    `{"schema_version":0,"providers":[]}`,
			wantErr: "schema_version: unsupported schema version 0",
		},
		{
			name:    "model routes null still requires version 2",
			ext:     ".yaml",
			body:    "providers: []\nmodel_routes: null\n",
			wantErr: "model_routes: requires schema_version: 2",
		},
		{
			name:    "schema version 3 is unsupported in JSON",
			ext:     ".json",
			body:    `{"schema_version":3,"providers":[]}`,
			wantErr: "schema_version: unsupported schema version 3",
		},
		{
			name:    "schema version 3 is unsupported in YAML",
			ext:     ".yaml",
			body:    "schema_version: 3\nproviders: []\n",
			wantErr: "schema_version: unsupported schema version 3",
		},
		{
			name:    "unsupported schema version",
			ext:     ".yaml",
			body:    "schema_version: 4\nproviders: []\n",
			wantErr: "schema_version: unsupported schema version 4",
		},
		{
			name: "version 2 defaults route metadata and budgets",
			ext:  ".yaml",
			body: `schema_version: 2
providers:
  - id: azure-west
    type: azure-openai
    base_url: https://west.example.openai.azure.com/openai/v1
    api_key: test-key
model_routes:
  - id: gpt-route
    public_id: gpt-public
    endpoints: [/responses]
    targets:
      - id: west
        provider: azure-west
        upstream_model: deployment-west
`,
			wantVersion: ProvidersConfigSchemaVersion2,
			wantRoute:   true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "providers"+tc.ext)
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			cfg, err := LoadProvidersConfigFile(path)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatal("LoadProvidersConfigFile() error = nil")
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("LoadProvidersConfigFile() error = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadProvidersConfigFile() error = %v", err)
			}
			if got := cfg.EffectiveSchemaVersion(); got != tc.wantVersion {
				t.Fatalf("EffectiveSchemaVersion() = %d, want %d", got, tc.wantVersion)
			}
			if !tc.wantRoute {
				return
			}
			if len(cfg.ModelRoutes) != 1 {
				t.Fatalf("model routes = %d, want 1", len(cfg.ModelRoutes))
			}
			route := cfg.ModelRoutes[0]
			if route.Name != "gpt-public" {
				t.Fatalf("route name = %q, want public id", route.Name)
			}
			if route.Exposure != modelRouteExposurePublic {
				t.Fatalf("route exposure = %q, want public for route-only schema v2 config", route.Exposure)
			}
			if route.ModelPickerEnabled == nil || !*route.ModelPickerEnabled {
				t.Fatalf("model_picker_enabled = %v, want true", route.ModelPickerEnabled)
			}
			if route.ModelPickerCategory != "versatile" {
				t.Fatalf("model_picker_category = %q, want versatile", route.ModelPickerCategory)
			}
			if route.Routing.Mode != string(routeModePrimaryOnly) || route.Routing.MaxTargetAttempts != 1 || route.Routing.MaxUpstreamSends != 1 {
				t.Fatalf("routing defaults = %+v", route.Routing)
			}
		})
	}
}

func TestValidateProvidersConfigRejectsProgrammaticSchemaVersion3(t *testing.T) {
	err := ValidateProvidersConfig(ProvidersConfig{SchemaVersion: 3})
	if err == nil || !strings.Contains(err.Error(), "schema_version: unsupported schema version 3") {
		t.Fatalf("ValidateProvidersConfig() error = %v", err)
	}
}

func TestSchemaV2RouteOnlyMultipleProvidersMayOmitDefaultWithoutLegacyCatalog(t *testing.T) {
	cfg := ProvidersConfig{
		SchemaVersion: ProvidersConfigSchemaVersion2,
		Providers: []ProviderConfig{
			{ID: "primary", Type: "openai-compatible", BaseURL: "https://primary.example.test/v1", AuthType: "none"},
			{ID: "secondary", Type: "openai-compatible", BaseURL: "https://secondary.example.test/v1", AuthType: "none"},
		},
		ModelRoutes: []ModelRouteConfig{{
			ID:        "route",
			PublicID:  "public-model",
			Endpoints: []string{providerEndpointChatCompletions},
			Targets: []ModelRouteTargetConfig{
				{ID: "primary-target", Provider: "primary", UpstreamModel: "deployment"},
				{ID: "secondary-target", Provider: "secondary", UpstreamModel: "deployment"},
			},
		}},
	}

	h, err := NewProxyHandler(nil, nil, WithProvidersConfig(cfg))
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	if got := h.providerSetup().defaultProviderID; got != "" {
		t.Fatalf("default provider = %q, want empty for closed route-only config", got)
	}
	if route, ok := h.providerSetup().lookupRouteAlias("public-model"); !ok || route == nil {
		t.Fatal("public route was not compiled")
	}
}

func TestSchemaV2MultipleProvidersWithLegacyCatalogRequireDefault(t *testing.T) {
	cfg := ProvidersConfig{
		SchemaVersion: ProvidersConfigSchemaVersion2,
		Providers: []ProviderConfig{
			{
				ID:       "catalog-provider",
				Type:     "openai-compatible",
				BaseURL:  "https://catalog.example.test/v1",
				AuthType: "none",
				Models: []ProviderModelConfig{{
					PublicID:   "legacy-model",
					Deployment: "legacy-model",
					Endpoints:  []string{providerEndpointChatCompletions},
				}},
			},
			{ID: "route-provider", Type: "openai-compatible", BaseURL: "https://route.example.test/v1", AuthType: "none"},
		},
		ModelRoutes: []ModelRouteConfig{{
			ID:        "route",
			PublicID:  "public-model",
			Endpoints: []string{providerEndpointChatCompletions},
			Targets: []ModelRouteTargetConfig{{
				ID:            "route-target",
				Provider:      "route-provider",
				UpstreamModel: "deployment",
			}},
		}},
	}

	err := ValidateProvidersConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "multiple providers are configured but no default provider is selected") {
		t.Fatalf("ValidateProvidersConfig() error = %v", err)
	}
}

func TestLoadProvidersConfigFileRejectsNonMappingTopLevel(t *testing.T) {
	for _, tc := range []struct {
		name string
		ext  string
		body string
	}{
		{name: "JSON null", ext: ".json", body: "null"},
		{name: "YAML scalar", ext: ".yaml", body: "null\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "providers"+tc.ext)
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			_, err := LoadProvidersConfigFile(path)
			if err == nil || !strings.Contains(err.Error(), "top-level provider configuration") {
				t.Fatalf("LoadProvidersConfigFile() error = %v", err)
			}
		})
	}
}

func TestLoadProvidersConfigFileRejectsDuplicateMappingKeys(t *testing.T) {
	tests := []struct {
		name     string
		ext      string
		body     string
		wantPath string
	}{
		{
			name:     "JSON top level",
			ext:      ".json",
			body:     `{"providers":[],"providers":[]}`,
			wantPath: "providers",
		},
		{
			name: "JSON nested target",
			ext:  ".json",
			body: `{
  "schema_version": 2,
  "providers": [{"id":"azure","type":"azure-openai","base_url":"https://x.openai.azure.com/openai/v1","api_key":"key"}],
  "model_routes": [{"id":"route","public_id":"model","endpoints":["/responses"],"targets":[{"id":"target","provider":"azure","provider":"azure","upstream_model":"model"}]}]
}`,
			wantPath: "model_routes[0].targets[0].provider",
		},
		{
			name:     "YAML top level",
			ext:      ".yaml",
			body:     "providers: []\nproviders: []\n",
			wantPath: "providers",
		},
		{
			name: "YAML nested provider",
			ext:  ".yaml",
			body: `schema_version: 2
providers:
  - id: azure
    id: duplicate
    type: azure-openai
    base_url: https://x.openai.azure.com/openai/v1
    api_key: key
model_routes: []
`,
			wantPath: "providers[0].id",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "providers"+tc.ext)
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			_, err := LoadProvidersConfigFile(path)
			if err == nil {
				t.Fatal("LoadProvidersConfigFile() error = nil")
			}
			for _, want := range []string{"duplicate mapping key", tc.wantPath} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("LoadProvidersConfigFile() error = %v, want %q", err, want)
				}
			}
		})
	}
}

func TestLoadProvidersConfigFileRejectsYAMLMergeKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.yaml")
	body := `schema_version: 2
defaults: &defaults
  type: openai-compatible
  base_url: https://example.test/v1
  auth_type: none
providers:
  - id: local
    <<: *defaults
    models:
      - public_id: model
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	_, err := LoadProvidersConfigFile(path)
	if err == nil || !strings.Contains(err.Error(), "YAML merge keys are not supported") {
		t.Fatalf("LoadProvidersConfigFile() error = %v", err)
	}
}

func TestLoadProvidersConfigFileRejectsUnknownModelRouteFields(t *testing.T) {
	tests := []struct {
		name string
		ext  string
		body string
		want string
	}{
		{
			name: "JSON",
			ext:  ".json",
			body: `{"schema_version":2,"providers":[],"model_routes":[{"id":"r","public_id":"m","endpoints":["/responses"],"targets":[],"weight":1}]}`,
			want: "model_routes[0].weight",
		},
		{
			name: "YAML",
			ext:  ".yaml",
			body: "schema_version: 2\nproviders: []\nmodel_routes:\n  - id: r\n    public_id: m\n    endpoints: [/responses]\n    targets: []\n    weight: 1\n",
			want: "model_routes[0].weight",
		},
		{
			name: "JSON routing field",
			ext:  ".json",
			body: `{"schema_version":2,"providers":[],"model_routes":[{"id":"r","public_id":"m","endpoints":["/responses"],"targets":[],"routing":{"weight":1}}]}`,
			want: "model_routes[0].routing.weight",
		},
		{
			name: "YAML routing field",
			ext:  ".yaml",
			body: "schema_version: 2\nproviders: []\nmodel_routes:\n  - id: r\n    public_id: m\n    endpoints: [/responses]\n    targets: []\n    routing:\n      weight: 1\n",
			want: "model_routes[0].routing.weight",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "providers"+tc.ext)
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			_, err := LoadProvidersConfigFile(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadProvidersConfigFile() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLoadProvidersConfigFileRejectsExplicitZeroRouteBudgets(t *testing.T) {
	tests := []struct {
		name string
		ext  string
		body string
		want string
	}{
		{
			name: "JSON max target attempts",
			ext:  ".json",
			body: `{"schema_version":2,"providers":[{"id":"azure","type":"azure-openai","base_url":"https://x.openai.azure.com/openai/v1","api_key":"key"}],"model_routes":[{"id":"route","public_id":"model","endpoints":["/responses"],"targets":[{"id":"target","provider":"azure","upstream_model":"model"}],"routing":{"max_target_attempts":0}}]}`,
			want: "model_routes[0].routing.max_target_attempts",
		},
		{
			name: "YAML max upstream sends",
			ext:  ".yaml",
			body: `schema_version: 2
providers:
  - id: azure
    type: azure-openai
    base_url: https://x.openai.azure.com/openai/v1
    api_key: key
model_routes:
  - id: route
    public_id: model
    endpoints: [/responses]
    targets:
      - id: target
        provider: azure
        upstream_model: model
    routing:
      max_upstream_sends: 0
`,
			want: "model_routes[0].routing.max_upstream_sends",
		},
		{
			name: "empty explicit mode",
			ext:  ".json",
			body: `{"schema_version":2,"providers":[{"id":"azure","type":"azure-openai","base_url":"https://x.openai.azure.com/openai/v1","api_key":"key"}],"model_routes":[{"id":"route","public_id":"model","endpoints":["/responses"],"targets":[{"id":"target","provider":"azure","upstream_model":"model"}],"routing":{"mode":""}}]}`,
			want: "model_routes[0].routing.mode",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "providers"+tc.ext)
			if err := os.WriteFile(path, []byte(tc.body), 0o644); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			_, err := LoadProvidersConfigFile(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadProvidersConfigFile() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateModelRoutesPathSpecificErrors(t *testing.T) {
	zero := int64(0)

	tests := []struct {
		name   string
		mutate func(*ProvidersConfig)
		want   string
	}{
		{name: "missing route id", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].ID = "" }, want: "model_routes[0].id"},
		{name: "invalid route id", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].ID = "bad\nid" }, want: "model_routes[0].id"},
		{name: "route id too long", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].ID = strings.Repeat("a", 129) }, want: "maximum is 128"},
		{name: "missing public id", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].PublicID = "" }, want: "model_routes[0].public_id"},
		{name: "missing endpoints", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].Endpoints = nil }, want: "model_routes[0].endpoints"},
		{name: "unknown endpoint", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].Endpoints = []string{"/realtime"} }, want: "model_routes[0].endpoints[0]"},
		{name: "duplicate endpoint", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].Endpoints = []string{"/responses", " /responses "} }, want: "model_routes[0].endpoints[1]"},
		{name: "duplicate reasoning", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].ReasoningEffort = []string{"high", " high "} }, want: "model_routes[0].reasoning_effort[1]"},
		{name: "invalid context", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].ContextWindow = &zero }, want: "model_routes[0].context_window"},
		{name: "missing targets", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].Targets = nil }, want: "model_routes[0].targets"},
		{name: "missing target id", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].Targets[0].ID = "" }, want: "model_routes[0].targets[0].id"},
		{name: "invalid target id", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].Targets[0].ID = "target\x00one" }, want: "model_routes[0].targets[0].id"},
		{name: "target id too long", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].Targets[0].ID = strings.Repeat("t", 129) }, want: "maximum is 128"},
		{name: "duplicate target id", mutate: func(c *ProvidersConfig) {
			c.ModelRoutes[0].Targets = append(c.ModelRoutes[0].Targets, c.ModelRoutes[0].Targets[0])
		}, want: "model_routes[0].targets[1].id"},
		{name: "missing provider", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].Targets[0].Provider = "" }, want: "model_routes[0].targets[0].provider"},
		{name: "unknown provider", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].Targets[0].Provider = "missing" }, want: "model_routes[0].targets[0].provider"},
		{name: "missing upstream model", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].Targets[0].UpstreamModel = "" }, want: "model_routes[0].targets[0].upstream_model"},
		{name: "chat-only route policy on responses route", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].DropStopSequences = boolPointer(true) }, want: "model_routes[0].drop_stop_sequences"},
		{name: "chat-only wire policy on responses route", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].Targets[0].UseMaxCompletionTokens = boolPointer(true) }, want: "model_routes[0].targets[0].use_max_completion_tokens"},
		{name: "unsupported mode", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].Routing.Mode = "weighted" }, want: "model_routes[0].routing.mode"},
		{name: "negative target attempts", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].Routing.MaxTargetAttempts = -1 }, want: "model_routes[0].routing.max_target_attempts"},
		{name: "negative sends", mutate: func(c *ProvidersConfig) { c.ModelRoutes[0].Routing.MaxUpstreamSends = -1 }, want: "model_routes[0].routing.max_upstream_sends"},
		{name: "attempts exceed targets", mutate: func(c *ProvidersConfig) {
			c.ModelRoutes[0].Routing.MaxTargetAttempts = 2
			c.ModelRoutes[0].Routing.MaxUpstreamSends = 2
		}, want: "route has only 1 targets"},
		{name: "primary only attempts must be one", mutate: func(c *ProvidersConfig) {
			c.ModelRoutes[0].Targets = append(c.ModelRoutes[0].Targets, ModelRouteTargetConfig{ID: "second", Provider: "azure", UpstreamModel: "second"})
			c.ModelRoutes[0].Routing.MaxTargetAttempts = 2
			c.ModelRoutes[0].Routing.MaxUpstreamSends = 2
		}, want: "must be 1 when routing.mode is"},
		{name: "sends below attempts", mutate: func(c *ProvidersConfig) {
			c.ModelRoutes[0].Targets = append(c.ModelRoutes[0].Targets, ModelRouteTargetConfig{ID: "second", Provider: "azure", UpstreamModel: "second"})
			c.ModelRoutes[0].Routing.Mode = string(routeModePriorityFailover)
			c.ModelRoutes[0].Routing.MaxTargetAttempts = 2
			c.ModelRoutes[0].Routing.MaxUpstreamSends = 1
		}, want: "must be at least max_target_attempts"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validAzureRouteConfig()
			tc.mutate(&cfg)
			_, err := validateAndNormalizeProvidersConfig(cfg)
			if err == nil {
				t.Fatal("validateAndNormalizeProvidersConfig() error = nil")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateModelRoutesProviderCompatibility(t *testing.T) {
	tests := []struct {
		name string
		cfg  ProvidersConfig
		want string
	}{
		{
			name: "public copilot route is not a policy destination",
			cfg: ProvidersConfig{SchemaVersion: 2, Providers: []ProviderConfig{{ID: "copilot", Type: "copilot"}}, ModelRoutes: []ModelRouteConfig{{
				ID: "route", PublicID: "model", Endpoints: []string{providerEndpointResponses}, Targets: []ModelRouteTargetConfig{{ID: "target", Provider: "copilot", UpstreamModel: "model"}},
			}}},
			want: "Copilot explicit targets require an internal policy route",
		},
		{
			name: "unreferenced internal copilot failover route is not a policy destination",
			cfg: ProvidersConfig{SchemaVersion: 2, Providers: []ProviderConfig{
				{ID: "copilot", Type: "copilot", Default: true},
				{ID: "azure", Type: "azure-openai", BaseURL: "https://x.openai.azure.com/openai/v1", APIKey: "key"},
			}, ModelRoutes: []ModelRouteConfig{{
				ID: "internal-route", Exposure: modelRouteExposureInternal, Endpoints: []string{providerEndpointResponses},
				Targets: []ModelRouteTargetConfig{
					{ID: "copilot", Provider: "copilot", UpstreamModel: "model"},
					{ID: "azure", Provider: "azure", UpstreamModel: "deployment"},
				},
				Routing: ModelRouteRoutingConfig{Mode: string(routeModePriorityFailover), MaxTargetAttempts: 2, MaxUpstreamSends: 2},
			}}},
			want: "must be referenced by policy_profiles",
		},
		{
			name: "dynamic generic provider cannot be explicit target",
			cfg: ProvidersConfig{SchemaVersion: 2, Providers: []ProviderConfig{{ID: "dynamic", Type: "openai-compatible", BaseURL: "https://example.test/v1", AuthType: "none", ModelDiscovery: "openai"}}, ModelRoutes: []ModelRouteConfig{{
				ID: "route", PublicID: "model", Endpoints: []string{providerEndpointResponses}, Targets: []ModelRouteTargetConfig{{ID: "target", Provider: "dynamic", UpstreamModel: "model"}},
			}}},
			want: "explicit route targets must be static",
		},
		{
			name: "anthropic provider rejects responses endpoint",
			cfg: ProvidersConfig{SchemaVersion: 2, Providers: []ProviderConfig{{ID: "anthropic", Type: "anthropic-compatible", BaseURL: "https://example.test", AuthType: "none"}}, ModelRoutes: []ModelRouteConfig{{
				ID: "route", PublicID: "model", Endpoints: []string{providerEndpointResponses}, Targets: []ModelRouteTargetConfig{{ID: "target", Provider: "anthropic", UpstreamModel: "model"}},
			}}},
			want: "model_routes[0].endpoints[0]",
		},
		{
			name: "native anthropic and openai adapters cannot mix",
			cfg: ProvidersConfig{SchemaVersion: 2, Providers: []ProviderConfig{
				{ID: "azure", Type: "azure-openai", Default: true, BaseURL: "https://x.openai.azure.com/openai/v1", APIKey: "key"},
				{ID: "anthropic", Type: "anthropic-compatible", BaseURL: "https://example.test", AuthType: "none"},
			}, ModelRoutes: []ModelRouteConfig{{
				ID: "route", PublicID: "model", Endpoints: []string{providerEndpointResponses}, Targets: []ModelRouteTargetConfig{
					{ID: "first", Provider: "azure", UpstreamModel: "model"},
					{ID: "second", Provider: "anthropic", UpstreamModel: "model"},
				},
			}}},
			want: "not adapter-compatible",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := validateAndNormalizeProvidersConfig(tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateModelRoutesRejectsDuplicatesAndCollisions(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*ProvidersConfig)
		want   string
	}{
		{name: "duplicate route id", mutate: func(c *ProvidersConfig) {
			c.ModelRoutes = append(c.ModelRoutes, cloneModelRouteConfig(c.ModelRoutes[0]))
			c.ModelRoutes[1].PublicID = "other"
		}, want: "model_routes[1].id"},
		{name: "duplicate public id", mutate: func(c *ProvidersConfig) {
			c.ModelRoutes = append(c.ModelRoutes, cloneModelRouteConfig(c.ModelRoutes[0]))
			c.ModelRoutes[1].ID = "other"
		}, want: "model_routes[1].public_id"},
		{name: "normalized public id collision", mutate: func(c *ProvidersConfig) {
			c.ModelRoutes[0].PublicID = "claude-sonnet-4-5"
			second := cloneModelRouteConfig(c.ModelRoutes[0])
			second.ID = "other"
			second.PublicID = "claude-sonnet-4.5"
			c.ModelRoutes = append(c.ModelRoutes, second)
		}, want: "after model normalization"},
		{name: "public id collides with static model", mutate: func(c *ProvidersConfig) {
			c.Providers[0].Models = []ProviderModelConfig{{PublicID: c.ModelRoutes[0].PublicID, Endpoints: []string{providerEndpointResponses}}}
		}, want: "providers[0].models[0].public_id"},
		{name: "route id collides with legacy provider owner", mutate: func(c *ProvidersConfig) {
			c.Providers[0].Models = []ProviderModelConfig{{PublicID: "legacy", Endpoints: []string{providerEndpointResponses}}}
			c.ModelRoutes[0].ID = c.Providers[0].ID
		}, want: "collides with legacy provider id"},
		{name: "route id collides with dynamic provider owner despite filters", mutate: func(c *ProvidersConfig) {
			c.Providers[0].Default = true
			c.Providers = append(c.Providers, ProviderConfig{
				ID: "dynamic", Type: "openai-compatible", BaseURL: "https://example.test/v1", AuthType: "none",
				ModelDiscovery: "openai", IncludeModels: []string{"other-model"},
			})
			c.ModelRoutes[0].ID = "dynamic"
		}, want: "collides with legacy provider id"},
		{name: "duplicate provider model endpoints", mutate: func(c *ProvidersConfig) {
			c.Providers[0].Models = []ProviderModelConfig{{PublicID: "legacy", Endpoints: []string{providerEndpointResponses, " /responses "}}}
		}, want: "providers[0].models[0].endpoints[1]"},
		{name: "duplicate provider model reasoning", mutate: func(c *ProvidersConfig) {
			c.Providers[0].Models = []ProviderModelConfig{{PublicID: "legacy", ReasoningEffort: []string{"high", " high "}}}
		}, want: "providers[0].models[0].reasoning_effort[1]"},
		{name: "chat-only provider model policy on responses model", mutate: func(c *ProvidersConfig) {
			c.Providers[0].Models = []ProviderModelConfig{{
				PublicID:          "legacy",
				Endpoints:         []string{providerEndpointResponses},
				DropStopSequences: boolPointer(true),
			}}
		}, want: "providers[0].models[0].drop_stop_sequences"},
		{name: "duplicate provider public aliases", mutate: func(c *ProvidersConfig) {
			c.Providers[0].Models = []ProviderModelConfig{{PublicID: "claude-sonnet-4-5"}, {PublicID: "claude-sonnet-4.5"}}
		}, want: "providers[0].models[1].public_id"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validAzureRouteConfig()
			tc.mutate(&cfg)
			_, err := validateAndNormalizeProvidersConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validation error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateModelRoutesHonorsLegacyProviderModelFilters(t *testing.T) {
	tests := []struct {
		name          string
		modelPublicID string
		routePublicID string
		includeModels []string
		excludeModels []string
		wantErr       string
	}{
		{
			name:          "excluded model does not reserve public id",
			modelPublicID: "gpt-public",
			routePublicID: "gpt-public",
			excludeModels: []string{" gpt-public "},
		},
		{
			name:          "model omitted from include does not reserve public id",
			modelPublicID: "gpt-public",
			routePublicID: "gpt-public",
			includeModels: []string{"other-model"},
		},
		{
			name:          "included model still collides",
			modelPublicID: "gpt-public",
			routePublicID: "gpt-public",
			includeModels: []string{" gpt-public "},
			wantErr:       "providers[0].models[0].public_id",
		},
		{
			name:          "excluded model does not reserve normalized aliases",
			modelPublicID: "claude-sonnet-4-5",
			routePublicID: "claude-sonnet-4.5",
			excludeModels: []string{" claude-sonnet-4-5 "},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := validAzureRouteConfig()
			cfg.Providers[0].Models = []ProviderModelConfig{{
				PublicID:  tc.modelPublicID,
				Endpoints: []string{providerEndpointResponses},
			}}
			cfg.Providers[0].IncludeModels = tc.includeModels
			cfg.Providers[0].ExcludeModels = tc.excludeModels
			cfg.ModelRoutes[0].PublicID = tc.routePublicID

			err := ValidateProvidersConfig(cfg)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("ValidateProvidersConfig() error = %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("ValidateProvidersConfig() error = %v, want %q", err, tc.wantErr)
			}
		})
	}
}

func TestValidateModelRoutesLimits(t *testing.T) {
	t.Run("route limit", func(t *testing.T) {
		cfg := validAzureRouteConfig()
		cfg.ModelRoutes = make([]ModelRouteConfig, maxExplicitModelRoutes+1)
		_, err := validateAndNormalizeProvidersConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "maximum is 256") {
			t.Fatalf("validation error = %v", err)
		}
	})

	t.Run("targets per route limit", func(t *testing.T) {
		cfg := validAzureRouteConfig()
		cfg.ModelRoutes[0].Targets = makeTargets(maxExplicitTargetsPerRoute + 1)
		_, err := validateAndNormalizeProvidersConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "maximum is 32") {
			t.Fatalf("validation error = %v", err)
		}
	})

	t.Run("total target limit", func(t *testing.T) {
		cfg := validAzureRouteConfig()
		cfg.ModelRoutes = make([]ModelRouteConfig, 33)
		for routeIndex := range cfg.ModelRoutes {
			cfg.ModelRoutes[routeIndex] = ModelRouteConfig{
				ID:        fmt.Sprintf("route-%d", routeIndex),
				PublicID:  fmt.Sprintf("model-%d", routeIndex),
				Endpoints: []string{providerEndpointResponses},
				Targets:   makeTargets(maxExplicitTargetsPerRoute),
			}
			for targetIndex := range cfg.ModelRoutes[routeIndex].Targets {
				cfg.ModelRoutes[routeIndex].Targets[targetIndex].ID = fmt.Sprintf("target-%d", targetIndex)
			}
		}
		_, err := validateAndNormalizeProvidersConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "more than 1024 total targets") {
			t.Fatalf("validation error = %v", err)
		}
	})
}

func TestRouteReferencedStaticProvidersMayOmitModels(t *testing.T) {
	tests := []struct {
		name     string
		provider ProviderConfig
		endpoint string
	}{
		{
			name:     "azure-openai",
			provider: ProviderConfig{ID: "provider", Type: "azure-openai", BaseURL: "https://x.openai.azure.com/openai/v1", APIKey: "key"},
			endpoint: providerEndpointResponses,
		},
		{
			name:     "openai-compatible",
			provider: ProviderConfig{ID: "provider", Type: "openai-compatible", BaseURL: "https://example.test/v1", AuthType: "none"},
			endpoint: providerEndpointResponses,
		},
		{
			name:     "anthropic-compatible",
			provider: ProviderConfig{ID: "provider", Type: "anthropic-compatible", BaseURL: "https://example.test", AuthType: "none"},
			endpoint: providerEndpointMessages,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := ProvidersConfig{
				SchemaVersion: ProvidersConfigSchemaVersion2,
				Providers:     []ProviderConfig{tc.provider},
				ModelRoutes: []ModelRouteConfig{{
					ID:        "route",
					PublicID:  "model",
					Endpoints: []string{tc.endpoint},
					Targets:   []ModelRouteTargetConfig{{ID: "target", Provider: "provider", UpstreamModel: "upstream"}},
				}},
			}
			if err := ValidateProvidersConfig(cfg); err != nil {
				t.Fatalf("ValidateProvidersConfig() error = %v", err)
			}
			h := &ProxyHandler{copilotURL: "https://api.githubcopilot.com"}
			providers, _, _, err := h.buildProviders(cfg)
			if err != nil {
				t.Fatalf("buildProviders() error = %v", err)
			}
			if got := len(providers["provider"].staticModels); got != 0 {
				t.Fatalf("static models = %d, want 0", got)
			}
		})
	}
}

func TestUnreferencedStaticProvidersStillRequireModels(t *testing.T) {
	tests := []ProviderConfig{
		{ID: "azure", Type: "azure-openai", BaseURL: "https://x.openai.azure.com/openai/v1", APIKey: "key"},
		{ID: "openai", Type: "openai-compatible", BaseURL: "https://example.test/v1", AuthType: "none"},
		{ID: "anthropic", Type: "anthropic-compatible", BaseURL: "https://example.test", AuthType: "none"},
	}
	for _, provider := range tests {
		provider := provider
		t.Run(provider.Type, func(t *testing.T) {
			cfg := ProvidersConfig{SchemaVersion: 2, Providers: []ProviderConfig{provider}}
			_, err := validateAndNormalizeProvidersConfig(cfg)
			if err == nil || !strings.Contains(err.Error(), "providers[0].models") {
				t.Fatalf("validation error = %v, want providers[0].models", err)
			}
		})
	}

	t.Run("unreferenced dynamic provider retains omission rule", func(t *testing.T) {
		cfg := ProvidersConfig{SchemaVersion: 2, Providers: []ProviderConfig{{
			ID: "dynamic", Type: "openai-compatible", BaseURL: "https://example.test/v1", AuthType: "none", ModelDiscovery: "openai",
		}}}
		if err := ValidateProvidersConfig(cfg); err != nil {
			t.Fatalf("ValidateProvidersConfig() error = %v", err)
		}
	})
}

func TestCompileExplicitModelRouteUsesValidatedContractAndBudgets(t *testing.T) {
	cfg := validAzureRouteConfig()
	cfg.Providers[0].Default = true
	cfg.Providers = append(cfg.Providers, ProviderConfig{
		ID: "azure-east", Type: "azure-openai", BaseURL: "https://east.openai.azure.com/openai/v1", APIKey: "east-key",
	})
	cfg.ModelRoutes[0].Name = "Public GPT"
	cfg.ModelRoutes[0].Endpoints = []string{providerEndpointChatCompletions}
	cfg.ModelRoutes[0].ReasoningEffort = []string{"low", "high"}
	cfg.ModelRoutes[0].DropStopSequences = boolPointer(true)
	cfg.ModelRoutes[0].Targets = append(cfg.ModelRoutes[0].Targets, ModelRouteTargetConfig{
		ID: "east", Provider: "azure-east", UpstreamModel: "east-deployment", UseMaxCompletionTokens: boolPointer(true),
	})
	cfg.ModelRoutes[0].Routing = ModelRouteRoutingConfig{
		Mode: string(routeModePriorityFailover), MaxTargetAttempts: 2, MaxUpstreamSends: 3,
	}

	h := &ProxyHandler{copilotURL: "https://api.githubcopilot.com"}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(context.Background(), cfg, false)
	if err != nil {
		t.Fatalf("buildConfiguredProviderSetupWithDynamicValidation() error = %v", err)
	}
	route, ok := setup.lookupRoute("gpt-public")
	if !ok {
		t.Fatal("explicit route was not published")
	}
	if route.public.routeID != "gpt-route" || route.public.name != "Public GPT" {
		t.Fatalf("public contract = %+v", route.public)
	}
	if route.policy.mode != routeModePriorityFailover || route.policy.maxTargetAttempts != 2 || route.policy.maxUpstreamSends != 3 {
		t.Fatalf("route policy = %+v", route.policy)
	}
	if !route.public.policy.dropStopSequences {
		t.Fatal("route public policy did not retain drop_stop_sequences")
	}
	if len(route.targets) != 2 || route.targets[1].provider.id != "azure-east" || !route.targets[1].wirePolicy.useMaxCompletionTokens {
		t.Fatalf("targets = %+v", route.targets)
	}
	if owner := providerModelFromRouteTarget(route, route.targets[0]); !owner.dropStopSequences {
		t.Fatal("route target owner did not inherit drop_stop_sequences")
	}
	var catalog struct {
		ID      string `json:"id"`
		OwnedBy string `json:"owned_by"`
	}
	if err := json.Unmarshal(route.public.raw, &catalog); err != nil {
		t.Fatalf("Unmarshal(route.public.raw) error = %v", err)
	}
	if catalog.ID != "gpt-public" || catalog.OwnedBy != "gpt-route" {
		t.Fatalf("catalog = %+v", catalog)
	}
}

func TestValidateProvidersConfigFileReportsMissingAPIKeyEnvironmentPath(t *testing.T) {
	const envName = "VEKIL_TEST_MISSING_ROUTE_API_KEY"
	t.Setenv(envName, "")
	path := filepath.Join(t.TempDir(), "providers.yaml")
	body := `schema_version: 2
providers:
  - id: azure
    type: azure-openai
    base_url: https://x.openai.azure.com/openai/v1
    api_key_env: VEKIL_TEST_MISSING_ROUTE_API_KEY
model_routes:
  - id: route
    public_id: model
    endpoints: [/responses]
    targets:
      - id: target
        provider: azure
        upstream_model: model
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := LoadProvidersConfigFile(path); err != nil {
		t.Fatalf("LoadProvidersConfigFile() structural validation error = %v", err)
	}
	err := ValidateProvidersConfigFile(path)
	if err == nil || !strings.Contains(err.Error(), "providers[0].api_key_env") {
		t.Fatalf("ValidateProvidersConfigFile() error = %v", err)
	}
}

func TestValidateProvidersConfigFileDoesNotContactUpstreams(t *testing.T) {
	if err := ValidateProvidersConfigFile("  "); err == nil || !strings.Contains(err.Error(), "providers_config") {
		t.Fatalf("ValidateProvidersConfigFile(empty) error = %v", err)
	}

	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()

	path := filepath.Join(t.TempDir(), "providers.json")
	body := fmt.Sprintf(`{
  "schema_version": 2,
  "providers": [{
    "id": "local",
    "type": "openai-compatible",
    "base_url": %q,
    "auth_type": "none",
    "models": [{"public_id":"legacy","endpoints":["/chat/completions"]}]
  }]
}`, server.URL)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := ValidateProvidersConfigFile(path); err != nil {
		t.Fatalf("ValidateProvidersConfigFile() error = %v", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("upstream requests = %d, want 0", got)
	}
}

func validAzureRouteConfig() ProvidersConfig {
	return ProvidersConfig{
		SchemaVersion: ProvidersConfigSchemaVersion2,
		Providers: []ProviderConfig{{
			ID: "azure", Type: "azure-openai", BaseURL: "https://x.openai.azure.com/openai/v1", APIKey: "key",
		}},
		ModelRoutes: []ModelRouteConfig{{
			ID:        "gpt-route",
			PublicID:  "gpt-public",
			Endpoints: []string{providerEndpointResponses},
			Targets: []ModelRouteTargetConfig{{
				ID: "primary", Provider: "azure", UpstreamModel: "deployment",
			}},
		}},
	}
}

func makeTargets(count int) []ModelRouteTargetConfig {
	targets := make([]ModelRouteTargetConfig, count)
	for index := range targets {
		targets[index] = ModelRouteTargetConfig{
			ID:            fmt.Sprintf("target-%d", index),
			Provider:      "azure",
			UpstreamModel: fmt.Sprintf("deployment-%d", index),
		}
	}
	return targets
}

func boolPointer(value bool) *bool {
	return &value
}

func TestLoadProvidersConfigFileVersion1PreservesYAMLMergeKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.yaml")
	body := `providers:
  - &defaults
    id: base
    type: openai-compatible
    base_url: http://localhost:11434/v1
    auth_type: none
    models:
      - public_id: base-model
  - <<: *defaults
    id: local
    default: true
    models:
      - public_id: local-model
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProvidersConfigFile(path)
	if err != nil {
		t.Fatalf("LoadProvidersConfigFile() error = %v", err)
	}
	if len(cfg.Providers) != 2 || cfg.Providers[1].ID != "local" || cfg.Providers[1].Type != "openai-compatible" {
		t.Fatalf("providers = %+v", cfg.Providers)
	}
	h := &ProxyHandler{copilotURL: "https://api.githubcopilot.com"}
	if _, _, _, err := h.buildProviders(cfg); err != nil {
		t.Fatalf("buildProviders() error = %v", err)
	}
}

func TestSchemaVersion2ProviderIDsAreBoundedOperationalIDs(t *testing.T) {
	for _, id := range []string{"bad\nprovider", strings.Repeat("p", maxModelRouteOperationalIDBytes+1)} {
		cfg := ProvidersConfig{
			SchemaVersion: 2,
			Providers: []ProviderConfig{{
				ID: id, Type: string(providerTypeAzureOpenAI), Default: true,
				BaseURL: "https://x.openai.azure.com/openai/v1", APIKey: "key",
			}},
		}
		if err := ValidateProvidersConfig(cfg); err == nil || !strings.Contains(err.Error(), "providers[0].id") {
			t.Fatalf("id %q error = %v", id, err)
		}
	}
}

func TestSchemaVersion2RejectsGeminiAliasCollision(t *testing.T) {
	cfg := ProvidersConfig{
		SchemaVersion: 2,
		Providers: []ProviderConfig{{
			ID: "openai", Type: string(providerTypeOpenAICompatible), Default: true,
			BaseURL: "http://localhost:11434/v1", AuthType: "none",
			Models: []ProviderModelConfig{{PublicID: "chat-compression-3-pro", Endpoints: []string{providerEndpointChatCompletions}}},
		}},
		ModelRoutes: []ModelRouteConfig{{
			ID: "gemini-route", PublicID: "gemini-3-pro-preview", Endpoints: []string{providerEndpointChatCompletions},
			Targets: []ModelRouteTargetConfig{{ID: "target", Provider: "openai", UpstreamModel: "physical"}},
		}},
	}
	err := ValidateProvidersConfig(cfg)
	if err == nil || !strings.Contains(err.Error(), "gemini-3-pro-preview") || !strings.Contains(err.Error(), "providers[0].models[0].public_id") {
		t.Fatalf("error = %v, want Gemini alias collision", err)
	}
}

func TestLoadProvidersConfigFileAllowsDropStopSequences(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.yaml")
	body := `schema_version: 2
providers:
  - id: azure
    type: azure-openai
    base_url: https://example.openai.azure.com/openai/v1
    api_key: test-key
model_routes:
  - id: sol-route
    public_id: gpt-5.6-sol
    endpoints: [/chat/completions]
    drop_stop_sequences: true
    targets:
      - id: west
        provider: azure
        upstream_model: gpt-5.6-sol
        use_max_completion_tokens: true
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if _, err := LoadProvidersConfigFile(path); err != nil {
		t.Fatalf("LoadProvidersConfigFile() error = %v", err)
	}
}
