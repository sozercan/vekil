package proxy

import (
	"strings"
	"testing"
)

func TestPolicyGenerationHashesIgnoreConfiguredHeaderValues(t *testing.T) {
	cfg := policyIntegrationConfig("https://light.example.test", "https://power.example.test", policyConfigModeEnforce)
	cfg.Providers[0].ExtraHeaders = map[string]string{"X-Test-Metadata": "header-one"}
	first := policyConfigGeneration(cfg)
	cfg.Providers[0].ExtraHeaders["X-Test-Metadata"] = "header-two"
	second := policyConfigGeneration(cfg)
	if first == "" || first != second {
		t.Fatalf("header-value-independent config hashes = %q and %q", first, second)
	}
	if len(first) != 64 || strings.Trim(first, "0123456789abcdef") != "" {
		t.Fatalf("config generation is not lowercase SHA-256: %q", first)
	}
	cfg.PolicyProfiles[0].Classifier.MaxRequestBytes++
	if changed := policyConfigGeneration(cfg); changed == first {
		t.Fatal("relevant classifier change did not change config generation")
	}
}

func TestPolicyProfileAndClassifierGenerationsChangeWithRelevantInputs(t *testing.T) {
	cfg := policyIntegrationConfig("https://light.example.test", "https://power.example.test", policyConfigModeEnforce)
	validated, err := validateAndNormalizeProvidersConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := compilePolicyPublicModelEntries(validated.config)
	if err != nil {
		t.Fatal(err)
	}
	profile := validated.config.PolicyProfiles[0]
	firstProfile := policyProfileGeneration(profile, entries[0].contract)
	profile.Classifier.TimeoutMS++
	if second := policyProfileGeneration(profile, entries[0].contract); second == firstProfile {
		t.Fatal("profile generation did not change")
	}

	h := &ProxyHandler{copilotURL: "https://api.githubcopilot.com"}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(t.Context(), validated.config, false)
	if err != nil {
		t.Fatal(err)
	}
	route, _ := setup.lookupTerminalRoute("classifier-route")
	firstClassifier := policyClassifierGeneration(route)
	cloned := *route
	cloned.targets = append([]targetBinding(nil), route.targets...)
	cloned.targets[0].upstreamModel = "other-classifier"
	if second := policyClassifierGeneration(&cloned); second == firstClassifier {
		t.Fatal("classifier generation did not change")
	}
	if binary := policyBinaryGeneration(); len(binary) != 64 {
		t.Fatalf("binary generation length = %d", len(binary))
	}
}
