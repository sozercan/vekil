package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validSchemaV3PolicyYAML() string {
	return `schema_version: 3
providers:
  - id: light-provider
    type: openai-compatible
    base_url: https://light.example.com/v1
    auth_type: none
    trust_domain: org-ai
    classifier_no_store_supported: true
  - id: powerful-provider
    type: openai-compatible
    base_url: https://powerful.example.com/v1
    auth_type: none
    trust_domain: org-ai
model_routes:
  - id: light-route
    exposure: internal
    name: Light
    endpoints: [/chat/completions]
    reasoning_effort: [low, medium]
    parallel_tool_calls: true
    context_window: 64000
    targets:
      - id: light-target
        provider: light-provider
        upstream_model: light-model
  - id: powerful-route
    exposure: internal
    name: Powerful
    endpoints: [/chat/completions]
    reasoning_effort: [medium, high]
    parallel_tool_calls: false
    context_window: 128000
    targets:
      - id: powerful-target
        provider: powerful-provider
        upstream_model: powerful-model
  - id: classifier-route
    exposure: internal
    internal_purpose: policy_classifier
    name: Classifier
    endpoints: [/chat/completions]
    targets:
      - id: classifier-target
        provider: light-provider
        upstream_model: classifier-model
policy_profiles:
  - id: coding-policy
    public_id: coding-policy-20260717
    lightweight_route: light-route
    powerful_route: powerful-route
    classifier:
      route: classifier-route
      recent_turns: 0
      observe_sample_rate: 0
    data_policy:
      content_forwarding_acknowledged: true
`
}

func TestSchemaV3PolicyConfigCompilesTerminalAndPublicRegistries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(path, []byte(validSchemaV3PolicyYAML()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProvidersConfigFile(path)
	if err != nil {
		t.Fatalf("LoadProvidersConfigFile() error = %v", err)
	}
	if got := cfg.EffectiveSchemaVersion(); got != ProvidersConfigSchemaVersion3 {
		t.Fatalf("schema version = %d, want 3", got)
	}
	profile := cfg.PolicyProfiles[0]
	if profile.Mode != policyConfigModeOff || profile.Name != profile.PublicID {
		t.Fatalf("profile defaults = %+v", profile)
	}
	if profile.Classifier.TimeoutMS != 3000 || profile.Classifier.MaxCompletionTokens != 256 || profile.Classifier.MaxRequestBytes != 16000 || profile.Classifier.MaxConcurrency != 4 {
		t.Fatalf("classifier defaults = %+v", profile.Classifier)
	}
	if profile.Classifier.RecentTurns != 0 || profile.Classifier.ObserveSampleRate != 0 {
		t.Fatalf("presence-aware zero values were not preserved: %+v", profile.Classifier)
	}

	h := &ProxyHandler{copilotURL: "https://api.githubcopilot.com"}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(t.Context(), cfg, false)
	if err != nil {
		t.Fatalf("buildConfiguredProviderSetupWithDynamicValidation() error = %v", err)
	}
	if setup.defaultProviderID != "" {
		t.Fatalf("default provider = %q, want empty for route-only schema v3 config", setup.defaultProviderID)
	}
	for _, routeID := range []string{"light-route", "powerful-route", "classifier-route"} {
		route, ok := setup.lookupTerminalRoute(routeID)
		if !ok || route == nil {
			t.Fatalf("lookupTerminalRoute(%q) = (%v, %v)", routeID, route, ok)
		}
		if route.isPublic() {
			t.Fatalf("terminal route %q unexpectedly public", routeID)
		}
		if _, ok := setup.lookupRoute(routeID); ok {
			t.Fatalf("internal route %q resolved through public static lookup", routeID)
		}
	}

	entry, ok := setup.lookupPublicModelEntry(profile.PublicID)
	if !ok || entry == nil || entry.kind != publicEntryPolicy || entry.policyID != profile.ID {
		t.Fatalf("policy entry = %+v, ok=%v", entry, ok)
	}
	if entry.policyConfig.Classifier.RecentTurns != 0 || entry.policyConfig.Classifier.ObserveSampleRate != 0 {
		t.Fatalf("normalized policy config = %+v", entry.policyConfig)
	}
	for _, alias := range configuredPublicModelAliases(profile.PublicID) {
		if got, ok := setup.lookupPublicModelEntry(alias); !ok || got != entry {
			t.Fatalf("lookupPublicModelEntry(%q) = (%p, %v), want (%p, true)", alias, got, ok, entry)
		}
	}
	if _, ok := setup.lookupRoute(profile.PublicID); ok {
		t.Fatal("policy entry resolved through static route lookup")
	}

	catalog := setup.routeRegistry().explicitRoutes()
	if len(catalog) != 1 || catalog[0].public.id != profile.PublicID {
		t.Fatalf("catalog routes = %+v, want policy only", catalog)
	}
	var raw struct {
		OwnedBy            string   `json:"owned_by"`
		SupportedEndpoints []string `json:"supported_endpoints"`
		Capabilities       struct {
			Limits struct {
				Context int64 `json:"max_context_window_tokens"`
			} `json:"limits"`
			Supports struct {
				Parallel  bool     `json:"parallel_tool_calls"`
				Reasoning []string `json:"reasoning_effort"`
				Vision    bool     `json:"vision"`
			} `json:"supports"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(entry.contract.raw, &raw); err != nil {
		t.Fatal(err)
	}
	if raw.OwnedBy != "vekil-policy" || len(raw.SupportedEndpoints) != 1 || raw.SupportedEndpoints[0] != providerEndpointChatCompletions {
		t.Fatalf("policy catalog contract = %+v", raw)
	}
	if raw.Capabilities.Limits.Context != 64000 || raw.Capabilities.Supports.Parallel || raw.Capabilities.Supports.Vision || strings.Join(raw.Capabilities.Supports.Reasoning, ",") != "medium" {
		t.Fatalf("conservative capabilities = %+v", raw.Capabilities)
	}
}

func TestPolicyPublicEntryLookupUsesRequestSideNormalization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(path, []byte(validSchemaV3PolicyYAML()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProvidersConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &ProxyHandler{copilotURL: "https://api.githubcopilot.com"}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(t.Context(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	entry, ok := setup.lookupPublicModelEntry("coding-policy-20260717-20250514")
	if !ok || entry == nil || entry.policyID != "coding-policy" {
		t.Fatalf("normalized policy lookup = (%+v, %v)", entry, ok)
	}
}

func TestSchemaV3PolicyValidationRejectsPrivacyAndExposureViolations(t *testing.T) {
	tests := []struct {
		name    string
		rewrite func(string) string
		want    string
	}{
		{
			name: "v3 field in v2",
			rewrite: func(body string) string {
				return strings.Replace(body, "schema_version: 3", "schema_version: 2", 1)
			},
			want: "policy_profiles: requires schema_version: 3",
		},
		{
			name: "internal public id",
			rewrite: func(body string) string {
				return strings.Replace(body, "exposure: internal\n    name: Light", "exposure: internal\n    public_id: forbidden\n    name: Light", 1)
			},
			want: "model_routes[0].public_id: must be omitted",
		},
		{
			name: "retention acknowledgement required",
			rewrite: func(body string) string {
				return strings.Replace(body, "classifier_no_store_supported: true", "classifier_no_store_supported: false", 1)
			},
			want: "data_policy.allow_provider_retention: must be true",
		},
		{
			name: "cross trust domain opt in required",
			rewrite: func(body string) string {
				return strings.Replace(body, "trust_domain: org-ai\nmodel_routes:", "trust_domain: other-domain\nmodel_routes:", 1)
			},
			want: "data_policy.allow_cross_trust_domain: must be true",
		},
		{
			name: "classifier must be internal purpose",
			rewrite: func(body string) string {
				return strings.Replace(body, "    internal_purpose: policy_classifier\n", "", 1)
			},
			want: "must set internal_purpose: policy_classifier",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "providers.yaml")
			if err := os.WriteFile(path, []byte(tc.rewrite(validSchemaV3PolicyYAML())), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadProvidersConfigFile(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadProvidersConfigFile() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSchemaV3RejectsMoreThanMaximumPolicyProfiles(t *testing.T) {
	cfg := ProvidersConfig{
		SchemaVersion:  ProvidersConfigSchemaVersion3,
		PolicyProfiles: make([]PolicyProfileConfig, maxPolicyProfiles+1),
	}
	if err := ValidateProvidersConfig(cfg); err == nil || !strings.Contains(err.Error(), "maximum is 128") {
		t.Fatalf("ValidateProvidersConfig() error = %v", err)
	}
}

func TestPolicyRoutingExampleConfigValid(t *testing.T) {
	t.Setenv("LIGHTWEIGHT_API_KEY", "test")
	t.Setenv("POWERFUL_API_KEY", "test")
	path := filepath.Join("..", "examples", "policy-routing-coding-economy.yaml")
	cfg, err := LoadProvidersConfigFile(path)
	if err != nil {
		t.Fatalf("LoadProvidersConfigFile(%q) error = %v", path, err)
	}
	if cfg.EffectiveSchemaVersion() != ProvidersConfigSchemaVersion3 || len(cfg.PolicyProfiles) != 1 {
		t.Fatalf("example config = schema %d profiles %d", cfg.EffectiveSchemaVersion(), len(cfg.PolicyProfiles))
	}
}

func TestSchemaV3RejectsPolicyPublicInsightModelOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.yaml")
	body := validSchemaV3PolicyYAML() + "insight_model: coding-policy-20260717\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProvidersConfigFile(path); err == nil || !strings.Contains(err.Error(), "insight_model") {
		t.Fatalf("LoadProvidersConfigFile() error = %v", err)
	}
}

func TestSchemaV3AllowsInsightPublicIDEqualToOperationalID(t *testing.T) {
	body := strings.Replace(validSchemaV3PolicyYAML(), "model_routes:\n", `model_routes:
  - id: public-insight-route
    public_id: classifier-route
    endpoints: [/chat/completions]
    targets:
      - id: public-insight-target
        provider: light-provider
        upstream_model: public-insight-model
`, 1)
	body += "insight_model: classifier-route\n"
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProvidersConfigFile(path); err != nil {
		t.Fatalf("LoadProvidersConfigFile() error = %v", err)
	}
}

func TestProgrammaticPolicyClassifierZeroSettersPreserveExplicitValues(t *testing.T) {
	cfg := policyIntegrationConfig("https://light.example.test", "https://power.example.test", policyConfigModeObserve)
	cfg.PolicyProfiles[0].Classifier.SetRecentTurns(0)
	cfg.PolicyProfiles[0].Classifier.SetObserveSampleRate(0)
	validated, err := validateAndNormalizeProvidersConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	classifier := validated.config.PolicyProfiles[0].Classifier
	if classifier.RecentTurns != 0 || classifier.ObserveSampleRate != 0 {
		t.Fatalf("explicit zeros were defaulted: %+v", classifier)
	}
}

func TestSharedClassifierRouteRequiresMatchingPreflightContract(t *testing.T) {
	cfg := policyIntegrationConfig("https://light.example.test", "https://power.example.test", policyConfigModeObserve)
	second := clonePolicyProfileConfig(cfg.PolicyProfiles[0])
	second.ID = "coding-policy-two"
	second.PublicID = "coding-economy-two"
	second.Classifier.TimeoutMS++
	cfg.PolicyProfiles = append(cfg.PolicyProfiles, second)
	if _, err := validateAndNormalizeProvidersConfig(cfg); err == nil || !strings.Contains(err.Error(), "same timeout_ms and max_completion_tokens") {
		t.Fatalf("validateAndNormalizeProvidersConfig() error = %v", err)
	}
}

func TestPolicyClassifierZeroValidFieldsRejectNull(t *testing.T) {
	for _, tc := range []struct {
		name string
		ext  string
		body string
	}{
		{
			name: "yaml recent turns",
			ext:  ".yaml",
			body: strings.Replace(validSchemaV3PolicyYAML(), "      recent_turns: 0", "      recent_turns: null", 1),
		},
		{
			name: "yaml sample rate",
			ext:  ".yaml",
			body: strings.Replace(validSchemaV3PolicyYAML(), "      observe_sample_rate: 0", "      observe_sample_rate: null", 1),
		},
		{
			name: "json recent turns",
			ext:  ".json",
			body: `{"schema_version":3,"providers":[{"id":"p","type":"openai-compatible","base_url":"https://example.test","auth_type":"none","trust_domain":"org","classifier_no_store_supported":true}],"model_routes":[{"id":"l","exposure":"internal","endpoints":["/chat/completions"],"targets":[{"id":"t","provider":"p","upstream_model":"l"}]},{"id":"h","exposure":"internal","endpoints":["/chat/completions"],"targets":[{"id":"t","provider":"p","upstream_model":"h"}]},{"id":"c","exposure":"internal","internal_purpose":"policy_classifier","endpoints":["/chat/completions"],"targets":[{"id":"t","provider":"p","upstream_model":"c"}]}],"policy_profiles":[{"id":"policy","public_id":"policy","lightweight_route":"l","powerful_route":"h","classifier":{"route":"c","recent_turns":null},"data_policy":{"content_forwarding_acknowledged":true}}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "providers"+tc.ext)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadProvidersConfigFile(path); err == nil || !strings.Contains(err.Error(), "must not be null") {
				t.Fatalf("LoadProvidersConfigFile() error = %v", err)
			}
		})
	}
}

func TestPolicyClassifierZeroValuesPreservedThroughYAMLAlias(t *testing.T) {
	body := `schema_version: 3
providers:
  - id: provider
    type: openai-compatible
    base_url: https://example.test/v1
    auth_type: none
    trust_domain: org
    classifier_no_store_supported: true
model_routes:
  - id: light-route
    exposure: internal
    endpoints: [/chat/completions]
    targets: [{id: light, provider: provider, upstream_model: light}]
  - id: power-route
    exposure: internal
    endpoints: [/chat/completions]
    targets: [{id: power, provider: provider, upstream_model: power}]
  - id: classifier-route
    exposure: internal
    internal_purpose: policy_classifier
    endpoints: [/chat/completions]
    targets: [{id: classifier, provider: provider, upstream_model: classifier}]
policy_profiles:
  - id: policy-one
    public_id: policy-one
    lightweight_route: light-route
    powerful_route: power-route
    classifier: &shared_classifier
      route: classifier-route
      recent_turns: 0
      observe_sample_rate: 0
    data_policy: {content_forwarding_acknowledged: true}
  - id: policy-two
    public_id: policy-two
    lightweight_route: light-route
    powerful_route: power-route
    classifier: *shared_classifier
    data_policy: {content_forwarding_acknowledged: true}
`
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProvidersConfigFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for index, profile := range cfg.PolicyProfiles {
		classifier := profile.Classifier
		if classifier.RecentTurns != 0 || classifier.ObserveSampleRate != 0 {
			t.Fatalf("profile %d alias zero values defaulted: %+v", index, classifier)
		}
	}
}

func TestProgrammaticPublicRouteRejectsInternalPurpose(t *testing.T) {
	cfg := policyIntegrationConfig("https://light.example.test", "https://power.example.test", policyConfigModeOff)
	cfg.ModelRoutes[0].Exposure = modelRouteExposurePublic
	cfg.ModelRoutes[0].PublicID = "public-light"
	cfg.ModelRoutes[0].InternalPurpose = modelRouteInternalPurposePolicyClassifier
	if err := ValidateProvidersConfig(cfg); err == nil || !strings.Contains(err.Error(), "internal_purpose") {
		t.Fatalf("ValidateProvidersConfig() error = %v", err)
	}
}
