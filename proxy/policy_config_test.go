package proxy

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func validSchemaV2PolicyYAML() string {
	return `schema_version: 2
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

func TestSchemaV2PolicyConfigCompilesTerminalAndPublicRegistries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(path, []byte(validSchemaV2PolicyYAML()), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadProvidersConfigFile(path)
	if err != nil {
		t.Fatalf("LoadProvidersConfigFile() error = %v", err)
	}
	if got := cfg.EffectiveSchemaVersion(); got != ProvidersConfigSchemaVersion2 {
		t.Fatalf("schema version = %d, want 2", got)
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
		t.Fatalf("default provider = %q, want empty for policy-owned schema v2 config", setup.defaultProviderID)
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

func TestSchemaV1RejectsSchemaV2PolicyFeatureFields(t *testing.T) {
	for _, tc := range []struct {
		name string
		ext  string
		body string
		want string
	}{
		{
			name: "yaml policy profiles",
			ext:  ".yaml",
			body: "schema_version: 1\nproviders: []\npolicy_profiles: []\n",
			want: "policy_profiles: requires schema_version: 2",
		},
		{
			name: "json policy profiles",
			ext:  ".json",
			body: `{"schema_version":1,"providers":[],"policy_profiles":[]}`,
			want: "policy_profiles: requires schema_version: 2",
		},
		{
			name: "provider trust domain",
			ext:  ".yaml",
			body: "schema_version: 1\nproviders:\n  - id: upstream\n    type: openai-compatible\n    base_url: https://example.test/v1\n    auth_type: none\n    trust_domain: org\n",
			want: "providers[0].trust_domain: requires schema_version: 2",
		},
		{
			name: "classifier no-store capability",
			ext:  ".yaml",
			body: "schema_version: 1\nproviders:\n  - id: upstream\n    type: openai-compatible\n    base_url: https://example.test/v1\n    auth_type: none\n    classifier_no_store_supported: true\n",
			want: "providers[0].classifier_no_store_supported: requires schema_version: 2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "providers"+tc.ext)
			if err := os.WriteFile(path, []byte(tc.body), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadProvidersConfigFile(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadProvidersConfigFile() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSchemaV2AllowsCopilotPolicyDestinationsAndClassifier(t *testing.T) {
	noStore := false
	cfg := policyIntegrationConfig("", "", policyConfigModeEnforce)
	cfg.Providers = []ProviderConfig{{
		ID:                         "copilot",
		Type:                       string(providerTypeCopilot),
		Default:                    true,
		TrustDomain:                "github-copilot",
		ClassifierNoStoreSupported: &noStore,
	}}
	for routeIndex := range cfg.ModelRoutes {
		cfg.ModelRoutes[routeIndex].Endpoints = []string{providerEndpointResponses}
		for targetIndex := range cfg.ModelRoutes[routeIndex].Targets {
			cfg.ModelRoutes[routeIndex].Targets[targetIndex].Provider = "copilot"
		}
	}
	cfg.PolicyProfiles[0].DataPolicy.AllowProviderRetention = true

	validated, err := validateAndNormalizeProvidersConfig(cfg)
	if err != nil {
		t.Fatalf("validateAndNormalizeProvidersConfig() error = %v", err)
	}
	if got := validated.config.ModelRoutes[0].Targets[0].Provider; got != "copilot" {
		t.Fatalf("lightweight target provider = %q, want copilot", got)
	}
}

func TestPolicyPublicEntryLookupUsesRequestSideNormalization(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(path, []byte(validSchemaV2PolicyYAML()), 0o600); err != nil {
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

func TestSchemaV2PolicyValidationRejectsPrivacyAndExposureViolations(t *testing.T) {
	tests := []struct {
		name    string
		rewrite func(string) string
		want    string
	}{
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
			if err := os.WriteFile(path, []byte(tc.rewrite(validSchemaV2PolicyYAML())), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := LoadProvidersConfigFile(path)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("LoadProvidersConfigFile() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestSchemaV2RejectsMoreThanMaximumPolicyProfiles(t *testing.T) {
	cfg := ProvidersConfig{
		SchemaVersion:  ProvidersConfigSchemaVersion2,
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
	if cfg.EffectiveSchemaVersion() != ProvidersConfigSchemaVersion2 || len(cfg.PolicyProfiles) != 1 {
		t.Fatalf("example config = schema %d profiles %d", cfg.EffectiveSchemaVersion(), len(cfg.PolicyProfiles))
	}
}

func TestCopilotPolicyRoutingExampleConfigValid(t *testing.T) {
	path := filepath.Join("..", "examples", "policy-routing-copilot.yaml")
	cfg, err := LoadProvidersConfigFile(path)
	if err != nil {
		t.Fatalf("LoadProvidersConfigFile(%q) error = %v", path, err)
	}
	if cfg.EffectiveSchemaVersion() != ProvidersConfigSchemaVersion2 || len(cfg.PolicyProfiles) != 1 {
		t.Fatalf("example config = schema %d profiles %d", cfg.EffectiveSchemaVersion(), len(cfg.PolicyProfiles))
	}
	if len(cfg.Providers) != 1 || providerType(cfg.Providers[0].Type) != providerTypeCopilot {
		t.Fatalf("example providers = %+v, want one Copilot provider", cfg.Providers)
	}
	for _, route := range cfg.ModelRoutes {
		if !configRouteSupportsEndpoint(&route, providerEndpointResponses) {
			t.Fatalf("route %q does not use Responses-backed Chat", route.ID)
		}
	}
}

func TestSchemaV2RejectsPolicyPublicInsightModelOnly(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.yaml")
	body := validSchemaV2PolicyYAML() + "insight_model: coding-policy-20260717\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProvidersConfigFile(path); err == nil || !strings.Contains(err.Error(), "insight_model") {
		t.Fatalf("LoadProvidersConfigFile() error = %v", err)
	}
}

func TestSchemaV2AllowsInsightPublicIDEqualToOperationalID(t *testing.T) {
	body := strings.Replace(validSchemaV2PolicyYAML(), "model_routes:\n", `model_routes:
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

func TestPolicyProfileRequiresMatchingDropStopSequences(t *testing.T) {
	body := strings.Replace(validSchemaV2PolicyYAML(), "    context_window: 64000\n", "    context_window: 64000\n    drop_stop_sequences: true\n", 1)
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProvidersConfigFile(path); err == nil || !strings.Contains(err.Error(), "drop_stop_sequences public Chat semantics") {
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
			body: strings.Replace(validSchemaV2PolicyYAML(), "      recent_turns: 0", "      recent_turns: null", 1),
		},
		{
			name: "yaml sample rate",
			ext:  ".yaml",
			body: strings.Replace(validSchemaV2PolicyYAML(), "      observe_sample_rate: 0", "      observe_sample_rate: null", 1),
		},
		{
			name: "json recent turns",
			ext:  ".json",
			body: `{"schema_version":2,"providers":[{"id":"p","type":"openai-compatible","base_url":"https://example.test","auth_type":"none","trust_domain":"org","classifier_no_store_supported":true}],"model_routes":[{"id":"l","exposure":"internal","endpoints":["/chat/completions"],"targets":[{"id":"t","provider":"p","upstream_model":"l"}]},{"id":"h","exposure":"internal","endpoints":["/chat/completions"],"targets":[{"id":"t","provider":"p","upstream_model":"h"}]},{"id":"c","exposure":"internal","internal_purpose":"policy_classifier","endpoints":["/chat/completions"],"targets":[{"id":"t","provider":"p","upstream_model":"c"}]}],"policy_profiles":[{"id":"policy","public_id":"policy","lightweight_route":"l","powerful_route":"h","classifier":{"route":"c","recent_turns":null},"data_policy":{"content_forwarding_acknowledged":true}}]}`,
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
	body := `schema_version: 2
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

func TestPolicyProfilePublicIDIsBoundedForMetricsIdentity(t *testing.T) {
	cfg := policyIntegrationConfig("https://light.example.test", "https://power.example.test", policyConfigModeOff)
	cfg.PolicyProfiles[0].PublicID = strings.Repeat("a", policyStatsLabelMaxLen+1)
	if err := ValidateProvidersConfig(cfg); err == nil || !strings.Contains(err.Error(), "public_id") || !strings.Contains(err.Error(), "at most") {
		t.Fatalf("ValidateProvidersConfig() error = %v, want bounded public_id error", err)
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

func TestPolicyReferencedInternalCopilotRoutesValidate(t *testing.T) {
	parallel := true
	cfg := ProvidersConfig{
		SchemaVersion: ProvidersConfigSchemaVersion2,
		Providers: []ProviderConfig{{
			ID:                         "copilot",
			Type:                       string(providerTypeCopilot),
			TrustDomain:                "github-copilot",
			ClassifierNoStoreSupported: boolPointer(true),
		}},
		ModelRoutes: []ModelRouteConfig{
			{
				ID: "light-route", Exposure: modelRouteExposureInternal,
				Endpoints: []string{providerEndpointResponses}, ParallelToolCalls: &parallel,
				Targets: []ModelRouteTargetConfig{{ID: "light", Provider: "copilot", UpstreamModel: "gpt-light"}},
			},
			{
				ID: "power-route", Exposure: modelRouteExposureInternal,
				Endpoints: []string{providerEndpointResponses}, ParallelToolCalls: &parallel,
				Targets: []ModelRouteTargetConfig{{ID: "power", Provider: "copilot", UpstreamModel: "gpt-power"}},
			},
			{
				ID: "classifier-route", Exposure: modelRouteExposureInternal, InternalPurpose: modelRouteInternalPurposePolicyClassifier,
				Endpoints: []string{providerEndpointResponses}, ParallelToolCalls: &parallel,
				Targets: []ModelRouteTargetConfig{{ID: "classifier", Provider: "copilot", UpstreamModel: "gpt-classifier"}},
			},
		},
		PolicyProfiles: []PolicyProfileConfig{{
			ID: "policy", PublicID: "policy-model",
			LightweightRoute: "light-route", PowerfulRoute: "power-route",
			Classifier: PolicyClassifierConfig{Route: "classifier-route"},
			DataPolicy: PolicyDataPolicyConfig{ContentForwardingAcknowledged: true},
		}},
	}
	if err := ValidateProvidersConfig(cfg); err != nil {
		t.Fatalf("ValidateProvidersConfig() error = %v", err)
	}
}
