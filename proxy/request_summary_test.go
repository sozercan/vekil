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

func TestRequestSummaryRouteObservabilityNilSafe(t *testing.T) {
	var summary *RequestSummary
	summary.SetOperationID("op")
	summary.SetRouteID("route")
	summary.SetFinalTarget("target")
	summary.RecordUpstreamSend()
	summary.RecordTargetSwitch()
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
