package proxy

import (
	"testing"
	"time"
)

func TestEndpointHealthErrorBudgetParsing(t *testing.T) {
	tests := []struct {
		input   string
		want    ErrorBudget
		wantErr bool
	}{
		{"10/m", ErrorBudget{Count: 10, Window: time.Minute}, false},
		{"5/s", ErrorBudget{Count: 5, Window: time.Second}, false},
		{"1500/h", ErrorBudget{Count: 1500, Window: time.Hour}, false},
		{" 3 / m ", ErrorBudget{Count: 3, Window: time.Minute}, false},
		{"", ErrorBudget{}, true},
		{"10", ErrorBudget{}, true},
		{"/m", ErrorBudget{}, true},
		{"0/m", ErrorBudget{}, true},
		{"-1/m", ErrorBudget{}, true},
		{"10/x", ErrorBudget{}, true},
		{"abc/m", ErrorBudget{}, true},
	}

	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			got, err := ParseErrorBudget(tt.input)
			if (err != nil) != tt.wantErr {
				t.Fatalf("ParseErrorBudget(%q): err=%v, wantErr=%v", tt.input, err, tt.wantErr)
			}
			if !tt.wantErr && got != tt.want {
				t.Errorf("ParseErrorBudget(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}

func TestEndpointHealthErrorBudgetRoundTrip(t *testing.T) {
	tests := []struct {
		dsl string
	}{
		{"10/m"},
		{"5/s"},
		{"1500/h"},
	}

	for _, tt := range tests {
		t.Run(tt.dsl, func(t *testing.T) {
			eb, err := ParseErrorBudget(tt.dsl)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			text, err := eb.MarshalText()
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if string(text) != tt.dsl {
				t.Errorf("round-trip: got %q, want %q", string(text), tt.dsl)
			}
			var eb2 ErrorBudget
			if err := eb2.UnmarshalText(text); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if eb2 != eb {
				t.Errorf("unmarshal result %+v != original %+v", eb2, eb)
			}
		})
	}
}

func TestEndpointHealthTrackerBasicHealth(t *testing.T) {
	tracker := NewEndpointHealthTracker(ErrorBudget{Count: 3, Window: time.Minute}, 30*time.Second)

	if !tracker.IsHealthy() {
		t.Error("new tracker should be healthy")
	}

	// Record failures below budget.
	tracker.RecordFailure()
	tracker.RecordFailure()
	if !tracker.IsHealthy() {
		t.Error("should still be healthy with 2 failures (budget=3)")
	}
}

func TestEndpointHealthTrackerQuarantine(t *testing.T) {
	now := time.Now()
	tracker := NewEndpointHealthTracker(ErrorBudget{Count: 3, Window: time.Minute}, 30*time.Second)
	tracker.now = func() time.Time { return now }

	// Exhaust the budget.
	tracker.RecordFailure()
	tracker.RecordFailure()
	tracker.RecordFailure()

	if tracker.IsHealthy() {
		t.Error("should be quarantined after 3 failures")
	}
	if !tracker.IsQuarantined() {
		t.Error("IsQuarantined should be true")
	}
}

func TestEndpointHealthTrackerCooldownRelease(t *testing.T) {
	now := time.Now()
	tracker := NewEndpointHealthTracker(ErrorBudget{Count: 2, Window: time.Minute}, 30*time.Second)
	tracker.now = func() time.Time { return now }

	// Exhaust budget.
	tracker.RecordFailure()
	tracker.RecordFailure()

	if tracker.IsHealthy() {
		t.Error("should be quarantined")
	}

	// Advance past cooldown.
	now = now.Add(31 * time.Second)

	if !tracker.IsHealthy() {
		t.Error("should be healthy (half-open) after cooldown")
	}
}

func TestEndpointHealthTrackerHalfOpenSuccess(t *testing.T) {
	now := time.Now()
	tracker := NewEndpointHealthTracker(ErrorBudget{Count: 2, Window: time.Minute}, 30*time.Second)
	tracker.now = func() time.Time { return now }

	// Exhaust budget.
	tracker.RecordFailure()
	tracker.RecordFailure()

	// Advance past cooldown.
	now = now.Add(31 * time.Second)

	// Half-open: should allow one probe.
	if !tracker.IsHealthy() {
		t.Fatal("should be half-open")
	}

	// Successful probe restores health.
	tracker.RecordSuccess()
	if !tracker.IsHealthy() {
		t.Error("should be fully healthy after successful probe")
	}
	if tracker.IsQuarantined() {
		t.Error("should not be quarantined after successful probe")
	}
}

func TestEndpointHealthTrackerHalfOpenFailure(t *testing.T) {
	now := time.Now()
	tracker := NewEndpointHealthTracker(ErrorBudget{Count: 2, Window: time.Minute}, 30*time.Second)
	tracker.now = func() time.Time { return now }

	// Exhaust budget.
	tracker.RecordFailure()
	tracker.RecordFailure()

	// Advance past cooldown to half-open.
	now = now.Add(31 * time.Second)
	if !tracker.IsHealthy() {
		t.Fatal("should be half-open")
	}

	// Failed probe: re-quarantine.
	tracker.RecordFailure()
	if tracker.IsHealthy() {
		t.Error("should be re-quarantined after failed probe")
	}
}

func TestEndpointHealthTrackerRollingWindow(t *testing.T) {
	now := time.Now()
	tracker := NewEndpointHealthTracker(ErrorBudget{Count: 3, Window: time.Minute}, 30*time.Second)
	tracker.now = func() time.Time { return now }

	// Record 2 failures.
	tracker.RecordFailure()
	tracker.RecordFailure()

	// Advance past the window so old failures decay.
	now = now.Add(61 * time.Second)

	// Record 1 more failure - should NOT trigger quarantine since old ones decayed.
	tracker.RecordFailure()
	if !tracker.IsHealthy() {
		t.Error("should be healthy - old failures decayed outside window")
	}
}

func TestEndpointHealthTrackerSuccessWithoutHalfOpen(t *testing.T) {
	tracker := NewEndpointHealthTracker(ErrorBudget{Count: 5, Window: time.Minute}, 30*time.Second)

	// Recording success on a healthy endpoint is a no-op.
	tracker.RecordSuccess()
	if !tracker.IsHealthy() {
		t.Error("should remain healthy")
	}
}

func TestEndpointHealthTrackerPeekHealthyNoSideEffect(t *testing.T) {
	now := time.Now()
	tracker := NewEndpointHealthTracker(ErrorBudget{Count: 2, Window: time.Minute}, 30*time.Second)
	tracker.now = func() time.Time { return now }

	// Exhaust budget to quarantine.
	tracker.RecordFailure()
	tracker.RecordFailure()

	// Still quarantined.
	if tracker.PeekHealthy() {
		t.Error("should not be peek-healthy during quarantine")
	}

	// Advance past cooldown.
	now = now.Add(31 * time.Second)

	// PeekHealthy should report available without consuming probe.
	if !tracker.PeekHealthy() {
		t.Error("should be peek-healthy after cooldown")
	}

	// Call PeekHealthy multiple times - should remain available.
	if !tracker.PeekHealthy() {
		t.Error("PeekHealthy should be repeatable without consuming probe")
	}
	if !tracker.PeekHealthy() {
		t.Error("PeekHealthy should be repeatable without consuming probe (3rd call)")
	}

	// Now ClaimProbe should still succeed since PeekHealthy didn't consume it.
	if !tracker.ClaimProbe() {
		t.Error("ClaimProbe should succeed after PeekHealthy calls")
	}

	// Second ClaimProbe should fail (probe already consumed).
	if tracker.ClaimProbe() {
		t.Error("second ClaimProbe should fail")
	}

	// PeekHealthy should now return false (probe consumed, still quarantined).
	if tracker.PeekHealthy() {
		t.Error("PeekHealthy should be false after probe consumed")
	}
}

func TestEndpointHealthTrackerClaimProbeOnHealthy(t *testing.T) {
	tracker := NewEndpointHealthTracker(ErrorBudget{Count: 5, Window: time.Minute}, 30*time.Second)

	// ClaimProbe on a healthy (non-quarantined) endpoint returns false.
	if tracker.ClaimProbe() {
		t.Error("ClaimProbe should return false for healthy endpoint")
	}
}

func TestEndpointHealthTrackerClaimProbeDuringCooldown(t *testing.T) {
	now := time.Now()
	tracker := NewEndpointHealthTracker(ErrorBudget{Count: 2, Window: time.Minute}, 30*time.Second)
	tracker.now = func() time.Time { return now }

	// Exhaust budget to quarantine.
	tracker.RecordFailure()
	tracker.RecordFailure()

	// Still in cooldown - ClaimProbe should fail.
	if tracker.ClaimProbe() {
		t.Error("ClaimProbe should fail during cooldown")
	}
}

func TestEndpointHealthTrackerPeekAndClaimRecoveryFlow(t *testing.T) {
	now := time.Now()
	tracker := NewEndpointHealthTracker(ErrorBudget{Count: 2, Window: time.Minute}, 30*time.Second)
	tracker.now = func() time.Time { return now }

	// Exhaust budget to quarantine.
	tracker.RecordFailure()
	tracker.RecordFailure()

	// Advance past cooldown.
	now = now.Add(31 * time.Second)

	// Simulate the selector flow: PeekHealthy -> endpoint selected -> ClaimProbe.
	if !tracker.PeekHealthy() {
		t.Fatal("should be peek-healthy for selector sync")
	}
	if !tracker.ClaimProbe() {
		t.Fatal("should claim probe after selection")
	}

	// Simulate successful probe.
	tracker.RecordSuccess()

	// Should be fully restored.
	if !tracker.IsHealthy() {
		t.Error("should be fully healthy after successful probe")
	}
	if !tracker.PeekHealthy() {
		t.Error("should be peek-healthy after full recovery")
	}
}
