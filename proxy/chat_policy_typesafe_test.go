package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func TestPolicyTypeSafeResponseContract(t *testing.T) {
	expected := policyClassifierSignals{
		TurnType: policyTurnTypeEdit, CodeScope: policyCodeScopeFunction,
		ToolCallCountEstimate: 128, ModifyingToolCallCountEstimate: 1,
		RequiresCodebaseContext: false, RiskLevel: policyRiskLevelLow,
	}
	valid := string(policyTypeSafeTestResponse(t, expected))
	signals, err := parsePolicyTypeSafeResponse([]byte(valid))
	if err != nil || signals != expected {
		t.Fatalf("signals = %+v, error = %v", signals, err)
	}
	if tier := mapPolicySignals(signals, policyClassifierFacts{}); tier != policyTierLightweight {
		t.Fatalf("provider confidence must not override policy signals: tier=%s", tier)
	}
	for _, test := range []struct {
		name string
		body string
	}{
		{"missing answers", `{}`},
		{"null answers", `{"answers":null}`},
		{"missing fields", `{"answers":{}}`},
		{"unknown signal", strings.Replace(valid, `"risk_level":`, `"unknown_signal":`, 1)},
		{"duplicate answers", strings.Replace(valid, `"answers":`, `"answers":{},"answers":`, 1)},
		{"duplicate signal", strings.Replace(valid, `"answers":{`, `"answers":{"abstain":{},`, 1)},
		{"duplicate choice", strings.Replace(valid, `"choice":"false"`, `"choice":"true","choice":"false"`, 1)},
		{"extra answer field", strings.Replace(valid, `"choice":"false"`, `"rationale":"ignore","choice":"false"`, 1)},
		{"wrong type", strings.Replace(valid, `"type":"choice"`, `"type":"noul"`, 1)},
		{"null choice", strings.Replace(valid, `"choice":"false"`, `"choice":null`, 1)},
		{"probability instead of choice", strings.Replace(valid, `"choice":"false"`, `"noul":0.3`, 1)},
		{"boolean is not a label", strings.Replace(valid, `"choice":"false"`, `"choice":false`, 1)},
		{"boolean whitespace", strings.Replace(valid, `"choice":"false"`, `"choice":" false "`, 1)},
		{"invalid enum", strings.Replace(valid, `"choice":"edit"`, `"choice":"lightweight"`, 1)},
		{"fractional count", strings.Replace(valid, `"choice":"128"`, `"choice":"1.5"`, 1)},
		{"count overflow", strings.Replace(valid, `"choice":"128"`, `"choice":"129"`, 1)},
		{"negative count", strings.Replace(valid, `"choice":"128"`, `"choice":"-1"`, 1)},
		{"leading zero", strings.Replace(valid, `"choice":"128"`, `"choice":"01"`, 1)},
		{"count whitespace", strings.Replace(valid, `"choice":"128"`, `"choice":" 1 "`, 1)},
		{"trailing document", valid + `{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parsePolicyTypeSafeResponse([]byte(test.body))
			failure := policyClassifierFailureFromError(err)
			if err == nil || failure.Category != policyClassifierFailureInvalidOutput || !failure.HTTPAccepted || failure.AffectsBreaker {
				t.Fatalf("error = %v, failure = %+v", err, failure)
			}
		})
	}
}

func TestPolicyTypeSafeQuestionsAndBounds(t *testing.T) {
	questions := policyTypeSafeQuestions()
	for _, field := range []string{"tool_call_count_estimate", "modifying_tool_call_count_estimate"} {
		if len(questions[field].Criteria) != 129 {
			t.Fatalf("%s does not cover integer estimates 0..128", field)
		}
	}
	for _, field := range []string{"abstain", "requires_codebase_context"} {
		criteria := questions[field].Criteria
		_, hasFalse := criteria["false"]
		_, hasTrue := criteria["true"]
		if len(criteria) != 2 || !hasFalse || !hasTrue {
			t.Fatalf("%s does not use discrete boolean choices", field)
		}
	}
	for field, question := range questions {
		if strings.Contains(question.Instructions, "call emit_policy_signals") || !strings.Contains(question.Instructions, "untrusted data") {
			t.Fatalf("%s does not have evaluation-specific instructions", field)
		}
	}
	facts := policyClassifierFacts{
		SchemaVersion:   policyFactSchemaVersion,
		CurrentUserTask: &policyFactMessage{Role: policyFactRoleUser, Text: strings.Repeat("x", 2048)},
	}
	sends := 0
	classifier, err := newPolicyTypeSafeClassifier(policyHTTPClassifierOptions{Model: "any-model", MaxFactsBytes: 1024}, func(context.Context, []byte, http.Header) (policyClassifierHTTPResponse, error) {
		sends++
		return policyClassifierHTTPResponse{StatusCode: http.StatusOK}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := classifier.Classify(t.Context(), facts); err == nil || sends != 0 {
		t.Fatalf("oversized facts: error=%v sends=%d", err, sends)
	}
	classifier.options.MaxFactsBytes = 4096
	classifier.options.MaxResponseBytes = 1024
	classifier.send = func(_ context.Context, body []byte, _ http.Header) (policyClassifierHTTPResponse, error) {
		var request struct {
			State policyClassifierFacts `json:"state"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Fatal(err)
		}
		if request.State.CurrentUserTask.Text != facts.CurrentUserTask.Text {
			t.Fatal("bounded facts changed in the protocol adapter")
		}
		return policyClassifierHTTPResponse{StatusCode: http.StatusOK, Body: []byte(strings.Repeat("x", 1025))}, nil
	}
	_, err = classifier.Classify(t.Context(), facts)
	if failure := policyClassifierFailureFromError(err); failure.Category != policyClassifierFailureInvalidOutput || failure.AffectsBreaker {
		t.Fatalf("oversized response: error=%v failure=%+v", err, failure)
	}
}

func TestPolicyTypeSafeGeneration(t *testing.T) {
	route := &modelRoute{targets: []targetBinding{{id: "classifier", provider: &providerRuntime{id: "provider", kind: providerTypeOpenAICompatible}, upstreamModel: "model"}}}
	chatGeneration := policyClassifierGeneration(route, "")
	route.targets[0].provider.kind = providerTypeTypeSafeCompatible
	if got := policyClassifierGeneration(route, ""); got == chatGeneration {
		t.Fatal("classifier generation did not change with the wire protocol and questions")
	}
}
