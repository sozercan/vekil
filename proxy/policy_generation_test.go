package proxy

import (
	"encoding/json"
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

func TestPolicyGenerationHashesIgnoreProviderBaseURLUserinfo(t *testing.T) {
	const (
		firstLightURL  = "https://light-user:first-light-secret@light.example.test/v1"
		firstPowerURL  = "https://power-user:first-power-secret@power.example.test/v1"
		secondLightURL = "https://other-light-user:second-light-secret@light.example.test/v1"
		secondPowerURL = "https://other-power-user:second-power-secret@power.example.test/v1"
	)
	firstCfg := policyIntegrationConfig(firstLightURL, firstPowerURL, policyConfigModeEnforce)
	secondCfg := policyIntegrationConfig(secondLightURL, secondPowerURL, policyConfigModeEnforce)

	first := policyGenerationHashesForTest(t, firstCfg)
	second := policyGenerationHashesForTest(t, secondCfg)
	if first != second {
		t.Fatalf("userinfo-independent generation hashes = %q and %q", first, second)
	}

	validated, err := validateAndNormalizeProvidersConfig(firstCfg)
	if err != nil {
		t.Fatal(err)
	}
	sanitized := sanitizedPolicyGenerationConfig(validated.config)
	canonical, err := json.Marshal(sanitized)
	if err != nil {
		t.Fatal(err)
	}
	for _, credential := range []string{
		"light-user", "first-light-secret", "power-user", "first-power-secret",
	} {
		if strings.Contains(string(canonical), credential) {
			t.Fatalf("canonical config generation input contains credential %q: %s", credential, canonical)
		}
	}
	if got := sanitized.Providers[0].BaseURL; got != "https://light.example.test/v1" {
		t.Fatalf("sanitized light provider base URL = %q", got)
	}
	if got := sanitized.Providers[1].BaseURL; got != "https://power.example.test/v1" {
		t.Fatalf("sanitized power provider base URL = %q", got)
	}
	if got := firstCfg.Providers[0].BaseURL; got != firstLightURL {
		t.Fatalf("source config light provider base URL mutated to %q", got)
	}
	if got := firstCfg.Providers[1].BaseURL; got != firstPowerURL {
		t.Fatalf("source config power provider base URL mutated to %q", got)
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
	h := &ProxyHandler{copilotURL: "https://api.githubcopilot.com"}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(t.Context(), validated.config, false)
	if err != nil {
		t.Fatal(err)
	}
	lightweight, _ := setup.lookupTerminalRoute(validated.config.PolicyProfiles[0].LightweightRoute)
	powerful, _ := setup.lookupTerminalRoute(validated.config.PolicyProfiles[0].PowerfulRoute)
	profile := validated.config.PolicyProfiles[0]
	firstProfile := policyProfileGeneration(profile, entries[0].contract, lightweight, powerful)
	profile.Classifier.TimeoutMS++
	if second := policyProfileGeneration(profile, entries[0].contract, lightweight, powerful); second == firstProfile {
		t.Fatal("profile generation did not change")
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

func TestPolicyTierReasoningChangesConfigAndProfileGenerations(t *testing.T) {
	cfg := policyIntegrationConfig("https://light.example.test", "https://power.example.test", policyConfigModeEnforce)
	cfg.ModelRoutes[0].ReasoningEffort = []string{"low", "medium"}
	cfg.ModelRoutes[1].ReasoningEffort = []string{"medium", "max"}
	cfg.PolicyProfiles[0].TierReasoningEffort = &PolicyTierReasoningEffortConfig{Lightweight: " low ", Powerful: " max "}

	first := policyGenerationHashesForTest(t, cfg)
	if cfg.PolicyProfiles[0].TierReasoningEffort.Lightweight != " low " || cfg.PolicyProfiles[0].TierReasoningEffort.Powerful != " max " {
		t.Fatalf("generation hashing mutated source config: %+v", cfg.PolicyProfiles[0].TierReasoningEffort)
	}
	normalized := cloneProvidersConfigForValidation(cfg)
	normalized.PolicyProfiles[0].TierReasoningEffort = &PolicyTierReasoningEffortConfig{Lightweight: "low", Powerful: "max"}
	if got := policyGenerationHashesForTest(t, normalized); got != first {
		t.Fatalf("normalized-equivalent generation hashes = %q and %q", first, got)
	}
	firstEntries, err := compilePolicyPublicModelEntries(cfg)
	if err != nil {
		t.Fatal(err)
	}

	powerfulChanged := cloneProvidersConfigForValidation(cfg)
	powerfulChanged.PolicyProfiles[0].TierReasoningEffort = &PolicyTierReasoningEffortConfig{Lightweight: "low", Powerful: "medium"}
	powerfulHashes := policyGenerationHashesForTest(t, powerfulChanged)
	if first[0] == powerfulHashes[0] || first[1] == powerfulHashes[1] {
		t.Fatal("powerful tier reasoning did not change config and profile generations")
	}
	if first[2] != powerfulHashes[2] {
		t.Fatal("classifier generation changed with powerful tier reasoning")
	}

	lightweightChanged := cloneProvidersConfigForValidation(cfg)
	lightweightChanged.PolicyProfiles[0].TierReasoningEffort = &PolicyTierReasoningEffortConfig{Lightweight: "medium", Powerful: "max"}
	second := policyGenerationHashesForTest(t, lightweightChanged)
	secondEntries, err := compilePolicyPublicModelEntries(lightweightChanged)
	if err != nil {
		t.Fatal(err)
	}
	if first[0] == second[0] {
		t.Fatal("config generation did not change with lightweight tier reasoning")
	}
	if first[1] == second[1] {
		t.Fatal("profile generation did not change with lightweight tier reasoning")
	}
	if first[2] != second[2] {
		t.Fatal("classifier generation changed with lightweight tier reasoning")
	}
	if string(firstEntries[0].contract.raw) != string(secondEntries[0].contract.raw) {
		t.Fatal("public policy contract changed with private tier reasoning")
	}
}

func TestPolicyProfileGenerationChangesWithDropStopSequencesContract(t *testing.T) {
	cfg := policyIntegrationConfig("https://light.example.test", "https://power.example.test", policyConfigModeEnforce)
	validated, err := validateAndNormalizeProvidersConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := compilePolicyPublicModelEntries(validated.config)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("policy entries = %d, want 1", len(entries))
	}
	firstContract := entries[0].contract
	firstGeneration := policyGenerationHashesForTest(t, validated.config)[1]

	dropStopSequences := true
	cfg.ModelRoutes[0].DropStopSequences = &dropStopSequences
	cfg.ModelRoutes[1].DropStopSequences = &dropStopSequences
	validated, err = validateAndNormalizeProvidersConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	entries, err = compilePolicyPublicModelEntries(validated.config)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("policy entries = %d, want 1", len(entries))
	}
	secondContract := entries[0].contract
	if string(secondContract.raw) != string(firstContract.raw) {
		t.Fatalf("catalog contract changed with request policy:\nfirst:  %s\nsecond: %s", firstContract.raw, secondContract.raw)
	}
	if !secondContract.policy.dropStopSequences {
		t.Fatal("derived request policy did not enable drop_stop_sequences")
	}
	if secondGeneration := policyGenerationHashesForTest(t, validated.config)[1]; secondGeneration == firstGeneration {
		t.Fatal("profile generation did not change with drop_stop_sequences request policy")
	}
}

func policyGenerationHashesForTest(t *testing.T, cfg ProvidersConfig) [3]string {
	t.Helper()
	validated, err := validateAndNormalizeProvidersConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	entries, err := compilePolicyPublicModelEntries(validated.config)
	if err != nil {
		t.Fatal(err)
	}
	h := &ProxyHandler{copilotURL: "https://api.githubcopilot.com"}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(t.Context(), validated.config, false)
	if err != nil {
		t.Fatal(err)
	}
	classifierRoute, ok := setup.lookupTerminalRoute("classifier-route")
	if !ok {
		t.Fatal("classifier route not found")
	}
	configGeneration := policyConfigGeneration(cfg)
	classifierTarget, ok := classifierRoute.primaryTarget()
	if !ok || classifierTarget.provider == nil {
		t.Fatal("classifier target provider not found")
	}
	wantRuntimeBaseURL := strings.TrimRight(strings.TrimSpace(cfg.Providers[0].BaseURL), "/")
	if got := classifierTarget.provider.baseURL; got != wantRuntimeBaseURL {
		t.Fatalf("runtime classifier provider base URL = %q, want %q", got, wantRuntimeBaseURL)
	}
	profile := validated.config.PolicyProfiles[0]
	lightweight, ok := setup.lookupTerminalRoute(profile.LightweightRoute)
	if !ok {
		t.Fatal("lightweight route not found")
	}
	powerful, ok := setup.lookupTerminalRoute(profile.PowerfulRoute)
	if !ok {
		t.Fatal("powerful route not found")
	}
	return [3]string{
		configGeneration,
		policyProfileGeneration(profile, entries[0].contract, lightweight, powerful),
		policyClassifierGeneration(classifierRoute),
	}
}
