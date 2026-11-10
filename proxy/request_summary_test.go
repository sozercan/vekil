package proxy

import (
	"context"
	"testing"
)

func TestAcquireRequestSummaryOwnershipAndReset(t *testing.T) {
	ctx, summary, owned := AcquireRequestSummary(context.Background())
	if !owned || summary == nil {
		t.Fatalf("AcquireRequestSummary() = summary:%p owned:%v, want owned summary", summary, owned)
	}
	_, existing, existingOwned := AcquireRequestSummary(ctx)
	if existingOwned || existing != summary {
		t.Fatalf("AcquireRequestSummary(existing) = summary:%p owned:%v, want %p false", existing, existingOwned, summary)
	}
	summary.setRoute("openai_chat", "gpt-5", false)
	summary.retryStatsTracked.Store(true)
	ReleaseRequestSummary(summary)
	if summary.endpoint != "" || summary.model != "" || summary.retryStatsTracked.Load() {
		t.Fatalf("released summary retained state: %#v", summary)
	}
}

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

func TestRequestSummaryPolicyIdentityOnlyRedactsTopology(t *testing.T) {
	summary := &RequestSummary{}
	summary.setRoute("responses", "policy-alias", false)
	summary.setProvider("light-provider", "openai-compatible")
	summary.SetFinalTarget("light-target")
	summary.setUpstreamRequestID("upstream-request")

	summary.SetPolicyIdentity("coding-economy")
	// Late error attribution must not reintroduce topology for a request rejected
	// before any policy decision or terminal send.
	summary.setProvider("power-provider", "azure-openai")
	summary.setFinalRouteResult("power-target", "power-provider", "azure-openai", "late-upstream-request")
	summary.setUpstreamRequestID("late-upstream-request")

	stats := readSummaryForStats(summary)
	if stats.model != "coding-economy" || stats.routeID != "coding-economy" || stats.finalTarget != "coding-economy" {
		t.Fatalf("public policy attribution = model:%q route:%q target:%q", stats.model, stats.routeID, stats.finalTarget)
	}
	if stats.provider != "" || stats.kind != "" || stats.upstreamID != "" {
		t.Fatalf("policy identity leaked topology = provider:%q kind:%q upstream:%q", stats.provider, stats.kind, stats.upstreamID)
	}

	fields := make(map[string]any)
	for _, field := range summary.LoggerFields() {
		fields[field.Key] = field.Value
	}
	for key, want := range map[string]any{
		"model":        "coding-economy",
		"route_id":     "coding-economy",
		"final_target": "coding-economy",
	} {
		if got := fields[key]; got != want {
			t.Errorf("LoggerFields[%q] = %#v, want %#v", key, got, want)
		}
	}
	for _, key := range []string{"provider", "provider_kind", "upstream_request_id"} {
		if got, ok := fields[key]; ok {
			t.Errorf("LoggerFields[%q] = %#v, want absent", key, got)
		}
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

func TestRequestSummaryPolicyDecisionPreservesOperatorTopologyWhileStatsStayPublic(t *testing.T) {
	summary := &RequestSummary{}
	summary.SetPolicyIdentity("coding-economy")
	summary.SetPolicyDecision(chatOperationPlan{
		publicID: "coding-economy",
		policyID: "coding-policy",
	})
	summary.recordUpstreamAttempt("op", "light-route", "light-target", "light-provider", "azure-openai")
	summary.setFinalRouteResult("light-target", "light-provider", "azure-openai", "upstream-request")

	fields := make(map[string]any)
	for _, field := range summary.LoggerFields() {
		fields[field.Key] = field.Value
	}
	for key, want := range map[string]any{
		"provider":            "light-provider",
		"provider_kind":       "azure-openai",
		"final_target":        "light-target",
		"upstream_request_id": "upstream-request",
	} {
		if got := fields[key]; got != want {
			t.Errorf("LoggerFields[%q] = %#v, want %#v", key, got, want)
		}
	}

	stats := readSummaryForStats(summary)
	if stats.model != "coding-economy" || stats.routeID != "coding-economy" || stats.finalTarget != "coding-economy" {
		t.Fatalf("public policy stats attribution = model:%q route:%q target:%q", stats.model, stats.routeID, stats.finalTarget)
	}
	if stats.provider != "" || stats.kind != "" || stats.upstreamID != "" {
		t.Fatalf("policy stats leaked topology = provider:%q kind:%q upstream:%q", stats.provider, stats.kind, stats.upstreamID)
	}
}

func TestRequestSummaryRouteObservabilityNilSafe(t *testing.T) {
	var summary *RequestSummary
	summary.SetOperationID("op")
	summary.SetRouteID("route")
	summary.SetPolicyIdentity("public-policy")
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

func TestObserveRequestSummaryDoesNotAttributeEmptyOrInternalModelsToProvider(t *testing.T) {
	h, err := NewProxyHandler(nil, nil,
		WithProvidersConfig(policyIntegrationConfig("https://light.example.test", "https://power.example.test", policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name  string
		model string
	}{
		{name: "empty model"},
		{name: "internal destination route", model: "light-route"},
		{name: "internal classifier route", model: "classifier-route"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			ctx, summary := WithRequestSummary(t.Context())
			h.observeRequestSummary(ctx, "responses", test.model, false, providerEndpointResponses)
			stats := readSummaryForStats(summary)
			if stats.provider != "" || stats.kind != "" || stats.upstreamID != "" {
				t.Fatalf("summary leaked provider topology: %+v", stats)
			}
			if test.model != "" && stats.model != test.model {
				t.Fatalf("model = %q, want caller-supplied %q", stats.model, test.model)
			}
		})
	}
}
