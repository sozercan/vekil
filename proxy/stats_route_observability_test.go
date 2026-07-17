package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/models"
)

func routeStatsSummary(routeID string) *RequestSummary {
	summary := &RequestSummary{}
	summary.setRoute("/v1/responses", "gpt-route", false)
	summary.SetRouteID(routeID)
	summary.setOpenAIUsage(&models.OpenAIUsage{
		PromptTokens:     10,
		CompletionTokens: 5,
		TotalTokens:      15,
	})
	return summary
}

func TestRouteObservabilitySeparatesClientAndPhysicalLedgers(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	ctx, summary := WithRequestSummary(context.Background())
	summary.setRoute("/v1/responses", "gpt-route", false)
	summary.setOpenAIUsage(&models.OpenAIUsage{PromptTokens: 10, CompletionTokens: 5, TotalTokens: 15})

	h.RecordUpstreamAttempt(ctx, RouteAttemptObservation{
		OperationID:  "op-1",
		RouteID:      "route-gpt",
		TargetID:     "primary",
		ProviderID:   "azure-primary",
		ProviderKind: "azure",
	})
	h.RecordTargetSwitch(ctx)
	h.RecordUpstreamAttempt(ctx, RouteAttemptObservation{
		OperationID:  "op-1",
		RouteID:      "route-gpt",
		TargetID:     "secondary",
		ProviderID:   "azure-secondary",
		ProviderKind: "azure",
	})
	summary.setFinalRouteAttribution("secondary", "azure-secondary", "azure")

	// Physical ledger updates are independent from the one-per-client ledger.
	before := h.stats.snapshot()
	if before.UpstreamAttempts != 2 || before.TargetSwitches != 1 {
		t.Fatalf("physical counters before client record = attempts:%d switches:%d, want 2/1", before.UpstreamAttempts, before.TargetSwitches)
	}
	if before.Totals.Requests != 0 || before.Retries != 0 {
		t.Fatalf("physical attempts inflated legacy counters = requests:%d retries:%d, want 0/0", before.Totals.Requests, before.Retries)
	}

	h.RecordRequest(summary, http.StatusOK, "codex/1.0", 125*time.Millisecond)
	snap := h.stats.snapshot()

	if snap.Totals.Requests != 1 || snap.Totals.Errors != 0 || snap.Retries != 0 {
		t.Fatalf("legacy totals = requests:%d errors:%d retries:%d, want 1/0/0", snap.Totals.Requests, snap.Totals.Errors, snap.Retries)
	}
	if snap.UpstreamAttempts != 2 || snap.TargetSwitches != 1 || snap.RequestsWithFailover != 1 || snap.SuccessfulFailovers != 1 {
		t.Fatalf("route counters = attempts:%d switches:%d failover_requests:%d successful:%d, want 2/1/1/1",
			snap.UpstreamAttempts, snap.TargetSwitches, snap.RequestsWithFailover, snap.SuccessfulFailovers)
	}
	if len(snap.ByRoute) != 1 {
		t.Fatalf("by_route len = %d, want 1 (%+v)", len(snap.ByRoute), snap.ByRoute)
	}
	route := snap.ByRoute[0]
	if route.Route != "route-gpt" || route.Requests != 1 || route.Tokens != 15 || route.Errors != 0 || route.TargetSwitches != 1 || route.RequestsWithFailover != 1 || route.SuccessfulFailovers != 1 {
		t.Fatalf("by_route row = %+v", route)
	}
	if len(snap.ByTarget) != 2 {
		t.Fatalf("by_target len = %d, want 2 (%+v)", len(snap.ByTarget), snap.ByTarget)
	}
	byTarget := make(map[string]statsTargetBreakdown, len(snap.ByTarget))
	for _, target := range snap.ByTarget {
		byTarget[target.Target] = target
	}
	for targetID, providerID := range map[string]string{"primary": "azure-primary", "secondary": "azure-secondary"} {
		target, ok := byTarget[targetID]
		if !ok || target.Route != "route-gpt" || target.Provider != providerID || target.Kind != "azure" || target.Attempts != 1 {
			t.Errorf("by_target[%q] = %+v, present=%v", targetID, target, ok)
		}
	}
	recent := snap.Recent[0]
	if recent.OperationID != "op-1" || recent.RouteID != "route-gpt" || recent.FinalTarget != "secondary" || recent.UpstreamSends != 2 || recent.TargetSwitches != 1 {
		t.Fatalf("recent route fields = %+v", recent)
	}
	if recent.Provider != "azure-secondary" {
		t.Fatalf("recent final provider = %q, want azure-secondary", recent.Provider)
	}
}

func TestRouteObservabilityFailedFailoverAndExhaustion(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	ctx, summary := WithRequestSummary(context.Background())
	summary.setRoute("/v1/responses", "gpt-route", false)

	h.RecordUpstreamAttempt(ctx, RouteAttemptObservation{OperationID: "op-fail", RouteID: "route-gpt", TargetID: "primary", ProviderID: "azure-a"})
	h.RecordTargetSwitch(ctx)
	h.RecordUpstreamAttempt(ctx, RouteAttemptObservation{OperationID: "op-fail", RouteID: "route-gpt", TargetID: "secondary", ProviderID: "azure-b"})
	summary.setFinalRouteAttribution("secondary", "azure-b", "")
	h.RecordRouteExhaustion(ctx)
	h.RecordRouteExhaustion(ctx) // one logical operation, not two exhaustion events
	h.RecordRequest(summary, http.StatusServiceUnavailable, "curl/8", time.Millisecond)

	snap := h.stats.snapshot()
	if snap.RequestsWithFailover != 1 || snap.SuccessfulFailovers != 0 || snap.RouteExhaustions != 1 {
		t.Fatalf("failed failover counters = requests:%d successful:%d exhausted:%d, want 1/0/1",
			snap.RequestsWithFailover, snap.SuccessfulFailovers, snap.RouteExhaustions)
	}
	if snap.Totals.Requests != 1 || snap.Totals.Errors != 1 {
		t.Fatalf("client totals = requests:%d errors:%d, want 1/1", snap.Totals.Requests, snap.Totals.Errors)
	}
	if len(snap.ByRoute) != 1 || snap.ByRoute[0].RouteExhaustions != 1 || snap.ByRoute[0].Errors != 1 {
		t.Fatalf("by_route exhaustion row = %+v", snap.ByRoute)
	}
}

func TestRouteObservabilityStateBindingCounters(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	h.RecordStateBindingHit()
	h.RecordStateBindingHit()
	h.RecordStateBindingMiss()
	h.RecordStateBindingEviction()

	snap := h.stats.snapshot()
	if snap.StateBindingHits != 2 || snap.StateBindingMisses != 1 || snap.StateBindingEvictions != 1 {
		t.Fatalf("binding counters = hits:%d misses:%d evictions:%d, want 2/1/1",
			snap.StateBindingHits, snap.StateBindingMisses, snap.StateBindingEvictions)
	}
	if snap.Totals.Requests != 0 || snap.Retries != 0 {
		t.Fatalf("binding events inflated legacy counters = requests:%d retries:%d", snap.Totals.Requests, snap.Retries)
	}
}

func TestRouteObservabilityCapsRouteAndTargetCardinality(t *testing.T) {
	c := newStatsCollector()
	for i := 0; i < statsMaxKeys*3; i++ {
		id := strconv.Itoa(i)
		c.recordUpstreamAttempt("route-"+id, "target-"+id, "provider-"+id, "azure")
		summary := routeStatsSummary("route-" + id)
		c.record(summary, http.StatusOK, "curl/8", time.Millisecond)
	}

	c.mu.Lock()
	routeKeys := len(c.byRoute)
	targetKeys := len(c.byTarget)
	routeOther := c.byRoute[statsOtherKey]
	targetOther := c.byTarget[statsOtherKey]
	c.mu.Unlock()
	if routeKeys > statsMaxKeys+1 || targetKeys > statsMaxKeys+1 {
		t.Fatalf("route/target cardinality unbounded: routes=%d targets=%d max=%d", routeKeys, targetKeys, statsMaxKeys+1)
	}
	if routeOther == nil || routeOther.requests == 0 {
		t.Fatalf("route overflow was not folded into %q", statsOtherKey)
	}
	if targetOther == nil || targetOther.attempts == 0 || targetOther.target != statsOtherKey {
		t.Fatalf("target overflow was not folded into %q: %+v", statsOtherKey, targetOther)
	}

	snap := c.snapshot()
	if len(snap.ByTarget) > statsBreakdownRows {
		t.Fatalf("by_target response rows = %d, want <= %d", len(snap.ByTarget), statsBreakdownRows)
	}
	if len(snap.ByRoute) > statsMaxKeys+1 {
		t.Fatalf("by_route response rows = %d, want <= %d", len(snap.ByRoute), statsMaxKeys+1)
	}
}

func TestRouteObservabilityBoundsOperationalLabels(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	ctx, summary := WithRequestSummary(context.Background())
	longID := strings.Repeat("x", statOperationalLabelMaxLen*3)
	h.RecordUpstreamAttempt(ctx, RouteAttemptObservation{
		OperationID: longID,
		RouteID:     longID,
		TargetID:    longID,
		ProviderID:  longID,
	})
	summary.setFinalRouteAttribution(longID, longID, "")
	h.RecordRequest(summary, http.StatusOK, "curl/8", time.Millisecond)

	snap := h.stats.snapshot()
	recent := snap.Recent[0]
	for label, value := range map[string]string{
		"operation_id": recent.OperationID,
		"route_id":     recent.RouteID,
		"final_target": recent.FinalTarget,
	} {
		if got := len([]rune(value)); got > statOperationalLabelMaxLen+1 {
			t.Errorf("%s retained %d runes, want <= %d", label, got, statOperationalLabelMaxLen+1)
		}
		if value == longID {
			t.Errorf("%s was not bounded", label)
		}
	}
	if len(snap.ByRoute) != 1 || len([]rune(snap.ByRoute[0].Route)) > statOperationalLabelMaxLen+1 {
		t.Fatalf("bounded by_route row = %+v", snap.ByRoute)
	}
	if len(snap.ByTarget) != 1 || len([]rune(snap.ByTarget[0].Target)) > statOperationalLabelMaxLen+1 {
		t.Fatalf("bounded by_target row = %+v", snap.ByTarget)
	}
}

func TestRouteObservabilityJSONFields(t *testing.T) {
	snap := statsSnapshot{
		UpstreamAttempts:      2,
		TargetSwitches:        1,
		RequestsWithFailover:  1,
		SuccessfulFailovers:   1,
		RouteExhaustions:      3,
		StateBindingHits:      4,
		StateBindingMisses:    5,
		StateBindingEvictions: 6,
		ByRoute:               []statsBreakdown{{Route: "route-a", Requests: 1}},
		ByTarget:              []statsTargetBreakdown{{Route: "route-a", Target: "target-a", Attempts: 2}},
		PhysicalUsage:         statsTokenUsage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10},
		WastedUsage:           statsTokenUsage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4},
		RecentAttempts: []recentRouteAttempt{{
			OperationID:          "op-a",
			RouteID:              "route-a",
			TargetID:             "target-a",
			ProviderID:           "provider-a",
			Sequence:             1,
			AttemptKind:          routeAttemptNormal,
			Status:               "429",
			StatusCode:           http.StatusTooManyRequests,
			Outcome:              routeAttemptOutcomeRejected,
			Delivery:             requestExplicitlyRejected,
			SemanticProgress:     upstreamProgressNone,
			DownstreamCommitment: downstreamCommitmentNone,
			RetryDecision:        routeRetrySwitchTarget,
			CleanupComplete:      true,
		}},
		Recent: []recentRequest{{
			OperationID:    "op-a",
			RouteID:        "route-a",
			FinalTarget:    "target-a",
			UpstreamSends:  2,
			TargetSwitches: 1,
		}},
	}

	encoded, err := json.Marshal(snap)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	var root map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &root); err != nil {
		t.Fatalf("json.Unmarshal root: %v", err)
	}
	for _, key := range []string{
		"upstream_attempts", "target_switches", "requests_with_failover", "successful_failovers", "route_exhaustions",
		"state_binding_hits", "state_binding_misses", "state_binding_evictions", "by_route", "by_target",
		"physical_usage", "wasted_usage", "recent_attempts", "recent",
	} {
		if _, ok := root[key]; !ok {
			t.Errorf("stats JSON missing %q: %s", key, encoded)
		}
	}
	var byTarget []map[string]any
	if err := json.Unmarshal(root["by_target"], &byTarget); err != nil {
		t.Fatalf("json.Unmarshal by_target: %v", err)
	}
	if len(byTarget) != 1 || byTarget[0]["attempts"] != float64(2) {
		t.Fatalf("by_target attempts JSON = %+v, want attempts=2", byTarget)
	}

	var recentAttempts []map[string]any
	if err := json.Unmarshal(root["recent_attempts"], &recentAttempts); err != nil {
		t.Fatalf("json.Unmarshal recent_attempts: %v", err)
	}
	if len(recentAttempts) != 1 || recentAttempts[0]["outcome"] != string(routeAttemptOutcomeRejected) || recentAttempts[0]["retry_decision"] != string(routeRetrySwitchTarget) {
		t.Fatalf("recent_attempts JSON = %+v", recentAttempts)
	}

	var recent []map[string]any
	if err := json.Unmarshal(root["recent"], &recent); err != nil {
		t.Fatalf("json.Unmarshal recent: %v", err)
	}
	if len(recent) != 1 {
		t.Fatalf("recent len = %d, want 1", len(recent))
	}
	for key, want := range map[string]any{
		"operation_id":    "op-a",
		"route_id":        "route-a",
		"final_target":    "target-a",
		"upstream_sends":  float64(2),
		"target_switches": float64(1),
	} {
		if got := recent[0][key]; got != want {
			t.Errorf("recent JSON %s = %#v, want %#v (payload %s)", key, got, want, string(encoded))
		}
	}
}

func TestRouteAttemptConcurrentPublicationDoesNotOverwriteNewerRow(t *testing.T) {
	collector := newStatsCollector()
	record := collector.beginRouteAttempt(nil, RouteAttemptObservation{
		OperationID:  "operation-publication-order",
		RouteID:      "route-publication-order",
		TargetID:     "target-publication-order",
		ProviderID:   "provider-publication-order",
		ProviderKind: "test",
		Sequence:     1,
		AttemptKind:  routeAttemptNormal,
	})
	state := record.state

	older := state.row
	older.Status = http.StatusText(http.StatusOK)
	older.StatusCode = http.StatusOK
	older.Outcome = routeAttemptOutcomeSucceeded
	older.SemanticProgress = upstreamProgressTerminalSuccess
	older.UpstreamRequestID = "request-older"
	olderUsage := statsTokenUsage{PromptTokens: 3, CompletionTokens: 1, TotalTokens: 4}
	older.ReportedUsage = &olderUsage

	newer := older
	newer.Status = strconv.Itoa(http.StatusBadGateway)
	newer.StatusCode = http.StatusBadGateway
	newer.Outcome = routeAttemptOutcomeFailed
	newer.SemanticProgress = upstreamProgressTerminalFailure
	newer.RetryDecision = routeRetrySuppressedProgress
	newer.UpstreamRequestID = "request-newer"
	newerUsage := statsTokenUsage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10}
	newer.ReportedUsage = &newerUsage

	olderReady := make(chan struct{})
	publishOlder := make(chan struct{})
	olderDone := make(chan struct{})
	go func() {
		close(olderReady)
		<-publishOlder
		collector.applyRouteAttemptCompletion(state, routeAttemptPublication{
			version:           1,
			row:               older,
			physicalAccounted: olderUsage,
		})
		close(olderDone)
	}()
	<-olderReady

	collector.applyRouteAttemptCompletion(state, routeAttemptPublication{
		version:           2,
		row:               newer,
		physicalAccounted: newerUsage,
		wastedAccounted:   newerUsage,
	})
	close(publishOlder)
	<-olderDone

	snap := collector.snapshot()
	attempt := routeAttemptBySequence(t, snap.RecentAttempts, 1)
	if attempt.Outcome != routeAttemptOutcomeFailed || attempt.StatusCode != http.StatusBadGateway || attempt.UpstreamRequestID != "request-newer" {
		t.Fatalf("concurrent publication regressed newer row = %+v", attempt)
	}
	if snap.PhysicalUsage != newerUsage || snap.WastedUsage != newerUsage {
		t.Fatalf("concurrent publication accounting = physical:%+v wasted:%+v, want %+v", snap.PhysicalUsage, snap.WastedUsage, newerUsage)
	}
}

func TestRouteAttemptTraceReconciliationPreservesStrongerTerminalDiagnostics(t *testing.T) {
	collector := newStatsCollector()
	record := collector.beginRouteAttempt(nil, RouteAttemptObservation{
		OperationID:  "operation-terminal-diagnostics",
		RouteID:      "route-terminal-diagnostics",
		TargetID:     "target-terminal-diagnostics",
		ProviderID:   "provider-terminal-diagnostics",
		ProviderKind: "test",
		Sequence:     1,
		AttemptKind:  routeAttemptNormal,
	})
	record.complete(routeAttemptCompletion{
		StatusCode:           http.StatusBadGateway,
		Outcome:              routeAttemptOutcomeFailed,
		Delivery:             requestDeliveredOrAmbiguous,
		SemanticProgress:     upstreamProgressTerminalFailure,
		DownstreamCommitment: downstreamCommitmentNone,
		RetryDecision:        routeRetrySuppressedProgress,
		UpstreamRequestID:    "terminal-event-request",
		CleanupComplete:      true,
	})

	weakTrace := routeAttemptTrace{
		Sequence:    1,
		TargetID:    "target-terminal-diagnostics",
		ProviderID:  "provider-terminal-diagnostics",
		Kind:        routeAttemptNormal,
		StatusCode:  http.StatusOK,
		Delivery:    requestDeliveredOrAmbiguous,
		Progress:    upstreamProgressAllowedPreamble,
		Commitment:  downstreamCommitmentNone,
		Decision:    routeRetryAccepted,
		CleanupDone: false,
	}
	record.reconcileTrace(weakTrace, 1)
	weakTrace.UpstreamID = "preamble-header-request"
	record.reconcileTrace(weakTrace, 2)

	snap := collector.snapshot()
	attempt := routeAttemptBySequence(t, snap.RecentAttempts, 1)
	if attempt.Outcome != routeAttemptOutcomeFailed || attempt.StatusCode != http.StatusBadGateway || attempt.Status != strconv.Itoa(http.StatusBadGateway) {
		t.Fatalf("weak trace downgraded terminal status = %+v", attempt)
	}
	if attempt.UpstreamRequestID != "terminal-event-request" {
		t.Fatalf("weak trace downgraded terminal request ID = %+v", attempt)
	}
	if attempt.SemanticProgress != upstreamProgressTerminalFailure || attempt.RetryDecision != routeRetrySuppressedProgress || !attempt.CleanupComplete {
		t.Fatalf("weak trace downgraded terminal diagnostics = %+v", attempt)
	}
}
