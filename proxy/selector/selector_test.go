package selector

import (
	"fmt"
	"testing"
	"time"
)

// newEndpoint creates an Endpoint with the given latency for test convenience.
func newEndpoint(name string, healthy bool, latency time.Duration) *Endpoint {
	ep := NewEndpoint(name, "", "", 0, healthy)
	ep.SetLatencyEWMA(latency)
	return ep
}

func TestRoundRobinBasic(t *testing.T) {
	rr := NewRoundRobin()
	eps := []*Endpoint{
		NewEndpoint("a", "", "", 0, true),
		NewEndpoint("b", "", "", 0, true),
		NewEndpoint("c", "", "", 0, true),
	}

	// Should cycle through a, b, c, a, b, c...
	names := make([]string, 6)
	for i := range names {
		ep, err := rr.Pick(eps)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		names[i] = ep.Name
	}
	want := []string{"a", "b", "c", "a", "b", "c"}
	for i, name := range names {
		if name != want[i] {
			t.Errorf("pick %d: got %q, want %q", i, name, want[i])
		}
	}
}

func TestRoundRobinSkipsUnhealthy(t *testing.T) {
	rr := NewRoundRobin()
	eps := []*Endpoint{
		NewEndpoint("a", "", "", 0, false),
		NewEndpoint("b", "", "", 0, true),
		NewEndpoint("c", "", "", 0, true),
	}

	for range 4 {
		ep, err := rr.Pick(eps)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ep.Name == "a" {
			t.Error("should not pick unhealthy endpoint a")
		}
	}
}

func TestRoundRobinFallsBackWhenAllUnhealthy(t *testing.T) {
	rr := NewRoundRobin()
	eps := []*Endpoint{
		NewEndpoint("a", "", "", 0, false),
		NewEndpoint("b", "", "", 0, false),
	}

	ep, err := rr.Pick(eps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.Name != "a" && ep.Name != "b" {
		t.Errorf("expected one of a/b, got %q", ep.Name)
	}
}

func TestRoundRobinSingleEndpoint(t *testing.T) {
	rr := NewRoundRobin()
	eps := []*Endpoint{NewEndpoint("only", "", "", 0, true)}

	for range 5 {
		ep, err := rr.Pick(eps)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ep.Name != "only" {
			t.Errorf("got %q, want only", ep.Name)
		}
	}
}

func TestRoundRobinEmpty(t *testing.T) {
	rr := NewRoundRobin()
	_, err := rr.Pick(nil)
	if err != ErrNoEndpoints {
		t.Errorf("expected ErrNoEndpoints, got %v", err)
	}
}

func TestWeightedDistribution(t *testing.T) {
	w := NewWeighted()
	eps := []*Endpoint{
		NewEndpoint("heavy", "", "", 2, true),
		NewEndpoint("light", "", "", 1, true),
	}

	counts := map[string]int{}
	for range 9 {
		ep, err := w.Pick(eps)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		counts[ep.Name]++
	}
	// Over 9 picks with 2:1 weight ratio, expect 6 heavy, 3 light.
	if counts["heavy"] != 6 {
		t.Errorf("heavy: got %d, want 6", counts["heavy"])
	}
	if counts["light"] != 3 {
		t.Errorf("light: got %d, want 3", counts["light"])
	}
}

func TestWeightedZeroWeight(t *testing.T) {
	w := NewWeighted()
	eps := []*Endpoint{
		NewEndpoint("a", "", "", 0, true),
		NewEndpoint("b", "", "", 0, true),
	}

	// Zero weight should be treated as 1 (equal distribution).
	counts := map[string]int{}
	for range 6 {
		ep, err := w.Pick(eps)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		counts[ep.Name]++
	}
	if counts["a"] != 3 || counts["b"] != 3 {
		t.Errorf("expected equal distribution, got a=%d, b=%d", counts["a"], counts["b"])
	}
}

func TestWeightedSkipsUnhealthy(t *testing.T) {
	w := NewWeighted()
	eps := []*Endpoint{
		NewEndpoint("a", "", "", 5, false),
		NewEndpoint("b", "", "", 1, true),
	}

	for range 5 {
		ep, err := w.Pick(eps)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ep.Name == "a" {
			t.Error("should not pick unhealthy endpoint a")
		}
	}
}

func TestWeightedEmpty(t *testing.T) {
	w := NewWeighted()
	_, err := w.Pick(nil)
	if err != ErrNoEndpoints {
		t.Errorf("expected ErrNoEndpoints, got %v", err)
	}
}

func TestLeastLatencyBasic(t *testing.T) {
	ll := NewLeastLatency()
	eps := []*Endpoint{
		newEndpoint("slow", true, 200*time.Millisecond),
		newEndpoint("fast", true, 50*time.Millisecond),
		newEndpoint("medium", true, 100*time.Millisecond),
	}

	ep, err := ll.Pick(eps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.Name != "fast" {
		t.Errorf("got %q, want fast", ep.Name)
	}
}

func TestLeastLatencyPrefersUnprobed(t *testing.T) {
	ll := NewLeastLatency()
	eps := []*Endpoint{
		newEndpoint("probed", true, 50*time.Millisecond),
		newEndpoint("unprobed", true, 0),
	}

	ep, err := ll.Pick(eps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.Name != "unprobed" {
		t.Errorf("got %q, want unprobed", ep.Name)
	}
}

func TestLeastLatencySkipsUnhealthy(t *testing.T) {
	ll := NewLeastLatency()
	eps := []*Endpoint{
		newEndpoint("fast-down", false, 10*time.Millisecond),
		newEndpoint("slow-up", true, 200*time.Millisecond),
	}

	ep, err := ll.Pick(eps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ep.Name != "slow-up" {
		t.Errorf("got %q, want slow-up", ep.Name)
	}
}

func TestLeastLatencyAllUnhealthy(t *testing.T) {
	ll := NewLeastLatency()
	eps := []*Endpoint{
		newEndpoint("a", false, 100*time.Millisecond),
		newEndpoint("b", false, 50*time.Millisecond),
	}

	ep, err := ll.Pick(eps)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Falls back to all endpoints, picks lowest latency.
	if ep.Name != "b" {
		t.Errorf("got %q, want b", ep.Name)
	}
}

func TestLeastLatencyEmpty(t *testing.T) {
	ll := NewLeastLatency()
	_, err := ll.Pick(nil)
	if err != ErrNoEndpoints {
		t.Errorf("expected ErrNoEndpoints, got %v", err)
	}
}

func TestNewSelectorByName(t *testing.T) {
	tests := []struct {
		name     string
		wantType string
	}{
		{"round_robin", "*selector.RoundRobin"},
		{"weighted", "*selector.Weighted"},
		{"least_latency", "*selector.LeastLatency"},
		{"", "*selector.RoundRobin"},
		{"unknown", "*selector.RoundRobin"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := New(tt.name)
			got := fmt.Sprintf("%T", s)
			if got != tt.wantType {
				t.Errorf("New(%q) type = %s, want %s", tt.name, got, tt.wantType)
			}
		})
	}
}
