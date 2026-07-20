package proxy

import (
	"net/http"
	"strings"
	"testing"
)

func TestTargetRevisionStableForSameSemanticTarget(t *testing.T) {
	parallel := false
	classifierNoStore := true
	contractA := publicModelContract{
		id:        "public-model",
		endpoints: []string{providerEndpointResponses, providerEndpointChatCompletions, providerEndpointResponses},
		policy: providerRequestPolicy{
			parallelToolCalls:  &parallel,
			dropSamplingParams: true,
		},
	}
	contractB := clonePublicModelContract(contractA)
	contractB.endpoints = []string{providerEndpointChatCompletions, providerEndpointResponses}

	providerA := targetRevisionTestProvider()
	providerA.baseURL = "https://API.Example.invalid:443/v1/"
	providerA.extraHeaders = http.Header{
		"X-Tenant":  {"tenant-secret"},
		"X-Feature": {"enabled"},
	}
	providerA.classifierNoStoreSupported = &classifierNoStore

	providerB := targetRevisionTestProvider()
	providerB.baseURL = "https://api.example.invalid/v1"
	providerB.extraHeaders = http.Header{
		"x-feature": {"enabled"},
		"x-tenant":  {"tenant-secret"},
	}
	providerB.classifierNoStoreSupported = cloneBoolPtr(&classifierNoStore)

	targetA := targetBinding{
		provider:      providerA,
		upstreamModel: " physical-model ",
		wirePolicy: providerRequestPolicy{
			useMaxCompletionTokens: true,
		},
	}
	targetB := targetBinding{
		provider:      providerB,
		upstreamModel: "physical-model",
		wirePolicy: providerRequestPolicy{
			useMaxCompletionTokens: true,
		},
	}

	revisionA := deriveTargetRevision(contractA, targetA)
	revisionB := deriveTargetRevision(contractB, targetB)
	if revisionA == "" || !strings.HasPrefix(string(revisionA), targetRevisionPrefix) {
		t.Fatalf("revision = %q, want non-empty opaque target revision", revisionA)
	}
	if revisionA != revisionB {
		t.Fatalf("same semantic target revisions differ: %q != %q", revisionA, revisionB)
	}
}

func TestTargetRevisionChangesWithPhysicalOrRequestIdentity(t *testing.T) {
	parallel := false
	classifierNoStore := true
	baseContract := publicModelContract{
		id:        "public-model",
		endpoints: []string{providerEndpointResponses, providerEndpointChatCompletions},
		policy: providerRequestPolicy{
			parallelToolCalls:  &parallel,
			dropSamplingParams: true,
		},
	}
	baseTarget := targetBinding{
		provider:      targetRevisionTestProvider(),
		upstreamModel: "physical-model",
		wirePolicy: providerRequestPolicy{
			useMaxCompletionTokens: true,
		},
	}
	baseTarget.provider.classifierNoStoreSupported = &classifierNoStore
	baseRevision := deriveTargetRevision(baseContract, baseTarget)

	tests := []struct {
		name   string
		mutate func(*publicModelContract, *targetBinding)
	}{
		{
			name: "provider kind",
			mutate: func(_ *publicModelContract, target *targetBinding) {
				target.provider.kind = providerTypeAnthropicCompatible
			},
		},
		{
			name: "base URL",
			mutate: func(_ *publicModelContract, target *targetBinding) {
				target.provider.baseURL = "https://other.example.invalid/v1"
			},
		},
		{
			name: "credential",
			mutate: func(_ *publicModelContract, target *targetBinding) {
				target.provider.apiKey = "replacement-api-key"
			},
		},
		{
			name: "auth source",
			mutate: func(_ *publicModelContract, target *targetBinding) {
				target.provider.authType = providerAuthTypeNone
				target.provider.authHeader = ""
				target.provider.authPrefix = ""
				target.provider.apiKey = ""
			},
		},
		{
			name: "endpoint path",
			mutate: func(_ *publicModelContract, target *targetBinding) {
				target.provider.paths.responses = "/v2/responses"
			},
		},
		{
			name: "API version",
			mutate: func(_ *publicModelContract, target *targetBinding) {
				target.provider.apiVersion = "2026-07-20"
			},
		},
		{
			name: "token scope",
			mutate: func(_ *publicModelContract, target *targetBinding) {
				target.provider.tokenScope = "https://example.invalid/.default"
			},
		},
		{
			name: "auth header",
			mutate: func(_ *publicModelContract, target *targetBinding) {
				target.provider.authHeader = "X-API-Key"
			},
		},
		{
			name: "auth prefix",
			mutate: func(_ *publicModelContract, target *targetBinding) {
				target.provider.authPrefix = "Token"
			},
		},
		{
			name: "extra header",
			mutate: func(_ *publicModelContract, target *targetBinding) {
				target.provider.extraHeaders.Set("X-Tenant", "replacement-tenant")
			},
		},
		{
			name: "trust domain",
			mutate: func(_ *publicModelContract, target *targetBinding) {
				target.provider.trustDomain = "different-trust-domain"
			},
		},
		{
			name: "classifier capability",
			mutate: func(_ *publicModelContract, target *targetBinding) {
				value := false
				target.provider.classifierNoStoreSupported = &value
			},
		},
		{
			name: "upstream model",
			mutate: func(_ *publicModelContract, target *targetBinding) {
				target.upstreamModel = "other-physical-model"
			},
		},
		{
			name: "wire request policy",
			mutate: func(_ *publicModelContract, target *targetBinding) {
				target.wirePolicy.useMaxCompletionTokens = false
			},
		},
		{
			name: "public request policy",
			mutate: func(contract *publicModelContract, _ *targetBinding) {
				contract.policy.dropSamplingParams = false
			},
		},
		{
			name: "public endpoints",
			mutate: func(contract *publicModelContract, _ *targetBinding) {
				contract.endpoints = []string{providerEndpointResponses}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			contract := clonePublicModelContract(baseContract)
			target := cloneTargetBindingForRevisionTest(baseTarget)
			test.mutate(&contract, &target)
			if got := deriveTargetRevision(contract, target); got == baseRevision {
				t.Fatalf("revision did not change after %s mutation", test.name)
			}
		})
	}
}

func TestTargetRevisionDoesNotContainCredentialMaterial(t *testing.T) {
	provider := targetRevisionTestProvider()
	provider.apiKey = "super-secret-api-key"
	provider.extraHeaders = http.Header{
		"X-Tenant":        {"tenant-secret-value"},
		"X-Secondary-Key": {"secondary-secret-value"},
	}
	target := targetBinding{provider: provider, upstreamModel: "physical-model"}
	revision := string(deriveTargetRevision(publicModelContract{endpoints: []string{providerEndpointResponses}}, target))

	for _, secret := range []string{
		provider.apiKey,
		"tenant-secret-value",
		"secondary-secret-value",
		targetCredentialFingerprint(provider.apiKey),
		targetCredentialFingerprint("tenant-secret-value"),
	} {
		if secret != "" && strings.Contains(revision, secret) {
			t.Fatalf("revision %q contains credential-derived material %q", revision, secret)
		}
	}
}

func TestProductionAndLazyRoutesMaterializeTargetRevision(t *testing.T) {
	provider := targetRevisionTestProvider()
	cfg := ProvidersConfig{
		SchemaVersion: ProvidersConfigSchemaVersion2,
		Providers: []ProviderConfig{{
			ID:       "provider-a",
			Type:     string(providerTypeOpenAICompatible),
			BaseURL:  "https://api.example.invalid/v1",
			AuthType: string(providerAuthTypeNone),
		}},
		ModelRoutes: []ModelRouteConfig{{
			ID:        "route-a",
			PublicID:  "public-model",
			Endpoints: []string{providerEndpointResponses},
			Targets: []ModelRouteTargetConfig{{
				ID:            "target-a",
				Provider:      "provider-a",
				UpstreamModel: "physical-model",
			}},
		}},
	}
	routes, err := compileExplicitModelRoutes(cfg, map[string]*providerRuntime{"provider-a": provider})
	if err != nil {
		t.Fatalf("compileExplicitModelRoutes() error = %v", err)
	}
	if len(routes) != 1 || len(routes[0].targets) != 1 || routes[0].targets[0].revision == "" {
		t.Fatalf("explicit target revision was not materialized: %#v", routes)
	}

	legacy, err := compileLegacyModelRoute(providerModel{
		publicID:           "legacy-public",
		upstreamModel:      "legacy-physical",
		providerID:         "provider-a",
		supportedEndpoints: []string{providerEndpointResponses},
	}, provider)
	if err != nil {
		t.Fatalf("compileLegacyModelRoute() error = %v", err)
	}
	if legacy.targets[0].revision == "" {
		t.Fatal("legacy target revision was not materialized")
	}

	fixture := &modelRoute{
		public: publicModelContract{
			id:        "fixture-public",
			routeID:   "fixture-route",
			endpoints: []string{providerEndpointResponses},
		},
		targets: []targetBinding{{
			id:            "fixture-target",
			provider:      provider,
			upstreamModel: "fixture-physical",
		}},
	}
	if fixture.targets[0].revision != "" {
		t.Fatal("fixture unexpectedly started with a revision")
	}
	if _, err := newModelRouteRegistry([]*modelRoute{fixture}); err != nil {
		t.Fatalf("newModelRouteRegistry() error = %v", err)
	}
	if fixture.targets[0].revision == "" {
		t.Fatal("registry did not lazily normalize a hand-built fixture revision")
	}
}

func targetRevisionTestProvider() *providerRuntime {
	return &providerRuntime{
		id:         "provider-a",
		kind:       providerTypeOpenAICompatible,
		baseURL:    "https://api.example.invalid/v1",
		authType:   providerAuthTypeBearer,
		authHeader: "Authorization",
		authPrefix: "Bearer",
		apiKey:     "initial-api-key",
		extraHeaders: http.Header{
			"X-Tenant": {"tenant-secret"},
		},
		paths: providerEndpointPaths{
			chatCompletions: providerEndpointChatCompletions,
			responses:       providerEndpointResponses,
			messages:        providerEndpointMessages,
			models:          providerEndpointModels,
		},
		modelDiscovery: providerModelDiscoveryStatic,
		trustDomain:    "example-trust-domain",
	}
}

func cloneTargetBindingForRevisionTest(target targetBinding) targetBinding {
	cloned := target
	if target.provider != nil {
		provider := *target.provider
		provider.extraHeaders = target.provider.extraHeaders.Clone()
		provider.classifierNoStoreSupported = cloneBoolPtr(target.provider.classifierNoStoreSupported)
		cloned.provider = &provider
	}
	cloned.revision = ""
	cloned.wirePolicy.parallelToolCalls = cloneBoolPtr(target.wirePolicy.parallelToolCalls)
	return cloned
}
