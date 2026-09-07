package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPolicyDecisionEvidencePublicRequest(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		ceiling PolicyRoutingMode
		actual  string
		sends   int64
	}{
		{policyConfigModeOff, PolicyRoutingModeOff, "lightweight", 1},
		{policyConfigModeObserve, PolicyRoutingModeObserve, "lightweight", 3},
		{policyConfigModeEnforce, PolicyRoutingModeEnforce, "powerful", 3},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelHigh})
			powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, tc.mode)
			cfg.ModelRoutes[0].ReasoningEffort = []string{"low"}
			cfg.ModelRoutes[1].ReasoningEffort = []string{"max"}
			cfg.PolicyProfiles[0].Lightweight.ReasoningEffort = "low"
			cfg.PolicyProfiles[0].Powerful.ReasoningEffort = "max"
			h, err := NewProxyHandler(nil, nil, WithProvidersConfig(cfg), WithPolicyRoutingMode(tc.ceiling))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(h.BeginShutdown)
			if err := h.InitializePolicyRouting(t.Context()); err != nil {
				t.Fatal(err)
			}
			body := `{"model":"coding-economy","messages":[{"role":"user","content":"sensitive-task-text"}]}`
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
			req.Header.Set("X-Vekil-Request-ID", "sensitive-client-operation")
			w := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			if err := h.WaitLifecycleWorkers(t.Context()); err != nil {
				t.Fatal(err)
			}
			statsWriter := httptest.NewRecorder()
			h.HandleStatsJSON(statsWriter, httptest.NewRequest(http.MethodGet, "/stats.json", nil))
			var snapshot statsSnapshot
			if err := json.Unmarshal(statsWriter.Body.Bytes(), &snapshot); err != nil {
				t.Fatal(err)
			}
			rows := snapshot.PolicyRouting.RecentDecisions
			if len(rows) == 0 {
				t.Fatal("public stats omitted decision evidence")
			}
			foundClassifier := false
			for _, row := range rows {
				if row.Profile != "coding-economy" || row.Mode != tc.mode || row.ActualTier != tc.actual || row.OperationID == "" || row.OperationID == "sensitive-client-operation" {
					t.Fatalf("decision identity = %+v", row)
				}
				if row.MessageCount != 1 || row.InputBytes != len(body) || row.Generations.Config == "" || row.Generations.Classifier == "" {
					t.Fatalf("decision facts or generations missing: %+v", row)
				}
				if row.Signals != nil {
					foundClassifier = true
					if row.MappingReason != "complex_turn" || row.Signals.TurnType != policyTurnTypePlanning {
						t.Fatalf("classifier decision lost typed evidence: %+v", row)
					}
					if tc.mode == policyConfigModeObserve && row.ShadowTier != "powerful" {
						t.Fatalf("observe decision missing shadow tier: %+v", row)
					}
				}
			}
			if foundClassifier != (tc.mode != policyConfigModeOff) {
				t.Fatalf("classifier evidence for mode %s = %v", tc.mode, foundClassifier)
			}
			evidenceJSON, _ := json.Marshal(rows)
			for _, private := range []string{"sensitive-task-text", "sensitive-client-operation", "power-model", "light-model", "classifier-model", "power-provider", "classifier-route"} {
				if strings.Contains(string(evidenceJSON), private) {
					t.Fatalf("decision evidence retained %q", private)
				}
			}
			if snapshot.TaskUsage.Totals.Sends != tc.sends || snapshot.TaskUsage.Totals.Errors != 0 || snapshot.TaskUsage.Inflight != 0 {
				t.Fatalf("policy full-task sends = %+v, want %d", snapshot.TaskUsage, tc.sends)
			}
			wantPrompt := int64(2)
			if tc.mode != policyConfigModeOff {
				wantPrompt += 20
			}
			if snapshot.TaskUsage.Totals.Usage.PromptTokens != wantPrompt {
				t.Fatalf("classifier/preflight usage omitted: %+v", snapshot.TaskUsage)
			}
		})
	}
}

func TestPolicyDecisionEvidenceBoundsAndSnapshotIsolation(t *testing.T) {
	controller := &chatPolicyRoutingController{stats: newPolicyStatsCollector()}
	profile := &compiledPolicyProfile{entry: &publicModelEntry{id: "public-policy"}, configGeneration: "0123456789abcdef"}
	profile.setEffectiveMode(policyModeEnforce)
	signals := policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow}
	for index := range 300 {
		controller.recordDecisionEvidence(profile, chatPolicyInput{OperationID: fmt.Sprintf("operation-%d", index), OriginalBody: []byte("sensitive-body")}, policyDecisionRecord{
			Category: "classified", ActualTier: policyTierLightweight, MappingReason: "bounded_task", Signals: signals, HasSignals: true,
			MessageCount: 100001, ToolCount: -1, InputBytes: maxLargeRequestBodySize + 1, ClassifierLatency: time.Hour.Milliseconds(),
		})
	}
	snapshot := controller.stats.snapshot()
	if len(snapshot.RecentDecisions) != policyDecisionEvidenceLimit || snapshot.RecentDecisions[0].OperationID != "operation-299" || snapshot.RecentDecisions[255].OperationID != "operation-44" {
		t.Fatalf("decision ring order/bounds = %+v", snapshot.RecentDecisions)
	}
	row := snapshot.RecentDecisions[0]
	if row.MessageCount != 100000 || row.ToolCount != 0 || row.InputBytes != maxLargeRequestBodySize || row.ClassifierLatencyMS > policyStatsMaxClassifierLatencyMs {
		t.Fatalf("decision bounds = %+v", row)
	}
	row.Signals.RiskLevel = policyRiskLevelHigh
	if controller.stats.snapshot().RecentDecisions[0].Signals.RiskLevel != policyRiskLevelLow {
		t.Fatal("snapshot mutation changed retained evidence")
	}
	controller.recordDecisionEvidence(profile, chatPolicyInput{}, policyDecisionRecord{
		Category: "sensitive-category", MappingReason: "sensitive-reason", FailureCategory: "sensitive-failure",
		HasSignals: true, Signals: policyClassifierSignals{TurnType: "sensitive-turn"},
	})
	last := controller.stats.snapshot().RecentDecisions[0]
	encoded, _ := json.Marshal(last)
	if last.Signals != nil || strings.Contains(string(encoded), "sensitive-") {
		t.Fatalf("invalid classifier fields reached evidence: %s", encoded)
	}
}

func TestPolicyMappingReasonsPreserveTierPrecedence(t *testing.T) {
	base := policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeFile, RiskLevel: policyRiskLevelLow}
	for _, tc := range []struct {
		name, reason string
		change       func(*policyClassifierSignals)
		truncated    bool
		want         policyTier
	}{
		{"bounded", "bounded_task", func(*policyClassifierSignals) {}, false, policyTierLightweight},
		{"abstain", "uncertain_signals", func(s *policyClassifierSignals) { s.Abstain = true }, false, policyTierPowerful},
		{"planning precedence", "complex_turn", func(s *policyClassifierSignals) {
			s.TurnType = policyTurnTypePlanning
			s.CodeScope = policyCodeScopeMultiFile
			s.RiskLevel = policyRiskLevelHigh
		}, true, policyTierPowerful},
		{"broad scope", "broad_scope", func(s *policyClassifierSignals) { s.CodeScope = policyCodeScopeMultiFile }, false, policyTierPowerful},
		{"risk", "high_risk", func(s *policyClassifierSignals) { s.RiskLevel = policyRiskLevelHigh }, false, policyTierPowerful},
		{"modifications", "multiple_modifications", func(s *policyClassifierSignals) { s.ModifyingToolCallCountEstimate = 2; s.ToolCallCountEstimate = 2 }, false, policyTierPowerful},
		{"code context", "codebase_context", func(s *policyClassifierSignals) { s.RequiresCodebaseContext = true }, false, policyTierPowerful},
		{"truncated", "truncated_context", func(*policyClassifierSignals) {}, true, policyTierPowerful},
	} {
		t.Run(tc.name, func(t *testing.T) {
			signals := base
			tc.change(&signals)
			facts := policyClassifierFacts{Truncation: policyFactTruncation{FirstUserTask: tc.truncated}}
			tier, reason := mapPolicySignalsWithReason(signals, facts)
			if tier != tc.want || reason != tc.reason || mapPolicySignals(signals, facts) != tc.want {
				t.Fatalf("tier/reason = %s/%s, want %s/%s", tier, reason, tc.want, tc.reason)
			}
		})
	}
}
