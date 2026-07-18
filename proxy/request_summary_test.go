package proxy

import "testing"

func TestRequestSummaryRouteObservability(t *testing.T) {
	summary := &RequestSummary{}

	summary.SetOperationID("  op-123  ")
	summary.SetOperationID("op-replacement")
	summary.SetRouteID("  route-gpt  ")
	summary.SetRouteID("route-replacement")
	summary.SetFinalTarget("target-primary")
	summary.RecordUpstreamSend()
	summary.RecordTargetSwitch()
	summary.SetFinalTarget(" target-secondary ")
	summary.RecordUpstreamSend()

	if got := summary.OperationID(); got != "op-123" {
		t.Fatalf("OperationID = %q, want op-123", got)
	}
	if got := summary.RouteID(); got != "route-gpt" {
		t.Fatalf("RouteID = %q, want route-gpt", got)
	}
	if got := summary.FinalTarget(); got != "target-secondary" {
		t.Fatalf("FinalTarget = %q, want target-secondary", got)
	}
	if got := summary.UpstreamSendCount(); got != 2 {
		t.Fatalf("UpstreamSendCount = %d, want 2", got)
	}
	if got := summary.TargetSwitchCount(); got != 1 {
		t.Fatalf("TargetSwitchCount = %d, want 1", got)
	}
	if !summary.MarkRouteExhausted() {
		t.Fatal("first MarkRouteExhausted call = false, want true")
	}
	if summary.MarkRouteExhausted() {
		t.Fatal("second MarkRouteExhausted call = true, want false")
	}
	if !summary.RouteExhausted() {
		t.Fatal("RouteExhausted = false, want true")
	}

	fields := make(map[string]any)
	for _, field := range summary.LoggerFields() {
		fields[field.Key] = field.Value
	}
	wantFields := map[string]any{
		"operation_id":    "op-123",
		"route_id":        "route-gpt",
		"final_target":    "target-secondary",
		"upstream_sends":  int64(2),
		"target_switches": int64(1),
		"route_exhausted": true,
	}
	for key, want := range wantFields {
		if got := fields[key]; got != want {
			t.Errorf("LoggerFields[%q] = %#v, want %#v", key, got, want)
		}
	}
}

func TestRequestSummaryPolicyDecisionProvenance(t *testing.T) {
	tests := []struct {
		name     string
		decision policyDecisionRecord
		want     map[string]any
	}{
		{
			name: "classifier fallback",
			decision: policyDecisionRecord{
				Category:          "unavailable_fallback",
				FailureCategory:   string(policyClassifierFailureTimeout),
				ActualTier:        policyTierPowerful,
				ClassifierLatency: 237,
				MessageCount:      11,
				ToolCount:         4,
				InputBytes:        8192,
				Truncated:         true,
			},
			want: map[string]any{
				"policy_decision":              "unavailable_fallback",
				"policy_failure_category":      string(policyClassifierFailureTimeout),
				"policy_classifier_latency_ms": int64(237),
				"policy_message_count":         11,
				"policy_tool_count":            4,
				"policy_input_bytes":           8192,
				"policy_truncated":             true,
			},
		},
		{
			name: "zero values remain explicit",
			decision: policyDecisionRecord{
				Category:   "baseline",
				ActualTier: policyTierLightweight,
			},
			want: map[string]any{
				"policy_decision":              "baseline",
				"policy_failure_category":      "",
				"policy_classifier_latency_ms": int64(0),
				"policy_message_count":         0,
				"policy_tool_count":            0,
				"policy_input_bytes":           0,
				"policy_truncated":             false,
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			summary := &RequestSummary{}
			summary.SetPolicyDecision(chatOperationPlan{
				policyID:             "coding-policy",
				selectedTier:         tt.decision.ActualTier,
				effectiveMode:        policyModeEnforce,
				configGeneration:     "config-generation",
				profileGeneration:    "profile-generation",
				classifierGeneration: "classifier-generation",
				binaryGeneration:     "binary-generation",
				decision:             tt.decision,
			})

			fields := make(map[string]any)
			for _, field := range summary.LoggerFields() {
				fields[field.Key] = field.Value
			}
			for key, want := range tt.want {
				if got, ok := fields[key]; !ok || got != want {
					t.Errorf("LoggerFields[%q] = %#v, %t; want %#v, true", key, got, ok, want)
				}
			}
		})
	}
}

func TestRequestSummarySeparatesLastAttemptFromFinalRouteAttribution(t *testing.T) {
	summary := &RequestSummary{}

	summary.recordUpstreamAttempt("op-123", "route-gpt", "target-primary", "provider-primary", "azure-openai")
	summary.recordUpstreamAttempt("op-123", "route-gpt", "target-secondary", "provider-secondary", "openai-compatible")

	if got := summary.FinalTarget(); got != "" {
		t.Fatalf("FinalTarget before result selection = %q, want empty", got)
	}
	lastTarget, lastProvider, lastKind := summary.lastUpstreamAttempt()
	if lastTarget != "target-secondary" || lastProvider != "provider-secondary" || lastKind != "openai-compatible" {
		t.Fatalf("last attempt = %q/%q/%q, want secondary attribution", lastTarget, lastProvider, lastKind)
	}

	// The executor can select an earlier, higher-precedence result even though a
	// later target was dispatched. Canonical attribution must follow that result.
	summary.setFinalRouteAttribution("target-primary", "provider-primary", "azure-openai")
	stats := readSummaryForStats(summary)
	if stats.finalTarget != "target-primary" || stats.provider != "provider-primary" || stats.kind != "azure-openai" {
		t.Fatalf("final attribution = %q/%q/%q, want primary attribution", stats.finalTarget, stats.provider, stats.kind)
	}
	if stats.upstreamSends != 2 {
		t.Fatalf("upstream sends = %d, want 2", stats.upstreamSends)
	}
	lastTarget, lastProvider, lastKind = summary.lastUpstreamAttempt()
	if lastTarget != "target-secondary" || lastProvider != "provider-secondary" || lastKind != "openai-compatible" {
		t.Fatalf("last attempt after final selection = %q/%q/%q, want secondary attribution", lastTarget, lastProvider, lastKind)
	}
}

func TestRequestSummaryFinalRouteResultRestoresCanonicalAttribution(t *testing.T) {
	summary := &RequestSummary{}
	summary.setFinalRouteResult("secondary-target", "secondary", "openai-compatible", "secondary-request")
	summary.setFinalRouteResult(" primary-target ", " primary ", " azure-openai ", "")

	stats := readSummaryForStats(summary)
	if stats.finalTarget != "primary-target" || stats.provider != "primary" || stats.kind != "azure-openai" {
		t.Fatalf("final attribution = %q/%q/%q, want primary attribution", stats.finalTarget, stats.provider, stats.kind)
	}
	if stats.upstreamID != "" {
		t.Fatalf("upstream request ID = %q, want cleared with canonical result", stats.upstreamID)
	}
}

func TestRequestSummaryRouteObservabilityNilSafe(t *testing.T) {
	var summary *RequestSummary
	summary.SetOperationID("op")
	summary.SetRouteID("route")
	summary.SetPolicyDecision(chatOperationPlan{policyID: "policy"})
	summary.SetFinalTarget("target")
	summary.setFinalRouteAttribution("target", "provider", "kind")
	summary.recordUpstreamAttempt("op", "route", "target", "provider", "kind")
	summary.RecordUpstreamSend()
	summary.RecordTargetSwitch()
	if target, provider, kind := summary.lastUpstreamAttempt(); target != "" || provider != "" || kind != "" {
		t.Fatalf("nil last attempt = %q/%q/%q, want empty", target, provider, kind)
	}
	if summary.MarkRouteExhausted() {
		t.Fatal("nil MarkRouteExhausted = true, want false")
	}
	if summary.OperationID() != "" || summary.RouteID() != "" || summary.FinalTarget() != "" {
		t.Fatal("nil request summary returned non-empty identifiers")
	}
	if summary.UpstreamSendCount() != 0 || summary.TargetSwitchCount() != 0 || summary.RouteExhausted() {
		t.Fatal("nil request summary returned non-zero route counters")
	}
}
