package proxy

import (
	"testing"
	"time"
)

func TestParseEndpointErrorBudget(t *testing.T) {
	budget, err := parseEndpointErrorBudget("10/m")
	if err != nil {
		t.Fatalf("parseEndpointErrorBudget() error = %v", err)
	}
	if budget.Limit != 10 || budget.Window != time.Minute {
		t.Fatalf("budget = %#v, want 10/m", budget)
	}
	if _, err := parseEndpointErrorBudget("0/m"); err == nil {
		t.Fatal("parseEndpointErrorBudget(0/m) error = nil, want error")
	}
	if _, err := parseEndpointErrorBudget("10/day"); err == nil {
		t.Fatal("parseEndpointErrorBudget(10/day) error = nil, want error")
	}
}

func TestEndpointHealthQuarantineAndRelease(t *testing.T) {
	tracker := newEndpointHealthTracker(endpointHealthConfig{errorBudget: endpointErrorBudget{Limit: 2, Window: time.Minute}, cooldown: time.Second})
	now := time.Unix(100, 0)
	if !tracker.healthy(now) {
		t.Fatal("new endpoint should be healthy")
	}
	if quarantined := tracker.recordFailure(now); quarantined {
		t.Fatal("first failure should not quarantine")
	}
	if quarantined := tracker.recordFailure(now.Add(100 * time.Millisecond)); !quarantined {
		t.Fatal("second failure should quarantine")
	}
	if tracker.healthy(now.Add(500 * time.Millisecond)) {
		t.Fatal("endpoint should be quarantined during cooldown")
	}
	if !tracker.healthy(now.Add(2 * time.Second)) {
		t.Fatal("endpoint should become healthy after cooldown")
	}
	tracker.recordSuccess(100 * time.Millisecond)
	if got := tracker.latency(); got != 100*time.Millisecond {
		t.Fatalf("latency = %v, want 100ms", got)
	}
}

func TestEndpointHealthSuccessDoesNotClearActiveQuarantine(t *testing.T) {
	tracker := newEndpointHealthTracker(endpointHealthConfig{errorBudget: endpointErrorBudget{Limit: 1, Window: time.Minute}, cooldown: time.Hour})
	now := time.Now()
	if quarantined := tracker.recordFailure(now); !quarantined {
		t.Fatal("failure should quarantine")
	}
	tracker.recordSuccess(10 * time.Millisecond)
	if tracker.healthy(now.Add(time.Second)) {
		t.Fatal("success while cooldown is active should not clear quarantine")
	}
}

func TestEndpointHealthFailurePenaltyExpires(t *testing.T) {
	tracker := newEndpointHealthTracker(endpointHealthConfig{errorBudget: endpointErrorBudget{Limit: 10, Window: time.Minute}, cooldown: time.Millisecond})
	now := time.Now()
	tracker.recordFailure(now)
	if got := tracker.latency(); got < time.Hour {
		t.Fatalf("latency immediately after failure = %v, want penalty", got)
	}
	time.Sleep(2 * time.Millisecond)
	if got := tracker.latency(); got != 0 {
		t.Fatalf("latency after penalty expiry = %v, want reset probe latency", got)
	}
}
