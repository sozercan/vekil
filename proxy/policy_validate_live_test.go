package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestValidateProvidersConfigFileLiveSkipsUnrelatedDynamicDiscovery(t *testing.T) {
	var modelsCalls atomic.Int64
	var classifierCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			modelsCalls.Add(1)
			http.Error(w, "dynamic catalog must remain offline", http.StatusInternalServerError)
			return
		}
		classifierCalls.Add(1)
		arguments, _ := json.Marshal(policyClassifierSignals{
			TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow,
		})
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{
				"role": "assistant",
				"tool_calls": []any{map[string]any{"type": "function", "function": map[string]any{
					"name": policyClassifierToolName, "arguments": string(arguments),
				}}},
			}}},
		})
	}))
	defer upstream.Close()

	body := `schema_version: 3
providers:
  - id: dynamic
    type: openai-compatible
    default: true
    base_url: ` + upstream.URL + `
    auth_type: none
    model_discovery: openai
  - id: policy-provider
    type: openai-compatible
    base_url: ` + upstream.URL + `
    auth_type: none
    trust_domain: org
    classifier_no_store_supported: true
model_routes:
  - id: light-route
    exposure: internal
    endpoints: [/chat/completions]
    targets: [{id: light, provider: policy-provider, upstream_model: light}]
  - id: power-route
    exposure: internal
    endpoints: [/chat/completions]
    targets: [{id: power, provider: policy-provider, upstream_model: power}]
  - id: classifier-route
    exposure: internal
    internal_purpose: policy_classifier
    endpoints: [/chat/completions]
    targets: [{id: classifier, provider: policy-provider, upstream_model: classifier}]
policy_profiles:
  - id: policy
    public_id: policy
    mode: enforce
    lightweight_route: light-route
    powerful_route: power-route
    classifier: {route: classifier-route}
    data_policy: {content_forwarding_acknowledged: true}
`
	path := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ValidateProvidersConfigFileLive(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if modelsCalls.Load() != 0 {
		t.Fatalf("dynamic /models calls = %d, want zero", modelsCalls.Load())
	}
	if classifierCalls.Load() != 1 {
		t.Fatalf("classifier calls = %d, want one preflight", classifierCalls.Load())
	}
}
