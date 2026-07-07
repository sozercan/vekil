package selector

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestRoundRobinSkipsUnhealthy(t *testing.T) {
	endpoints := []*Endpoint{{Name: "a", Healthy: true}, {Name: "b", Healthy: false}, {Name: "c", Healthy: true}}
	sel := NewRoundRobin()
	var got []string
	for range 4 {
		ep, err := sel.Pick(endpoints)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		got = append(got, ep.Name)
	}
	want := []string{"a", "c", "a", "c"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("round robin = %v, want %v", got, want)
	}
}

func TestWeightedRoundRobin(t *testing.T) {
	endpoints := []*Endpoint{{Name: "east", Healthy: true, Weight: 2}, {Name: "west", Healthy: true, Weight: 1}}
	sel := NewWeighted()
	var got []string
	for range 6 {
		ep, err := sel.Pick(endpoints)
		if err != nil {
			t.Fatalf("Pick() error = %v", err)
		}
		got = append(got, ep.Name)
	}
	want := []string{"east", "east", "west", "east", "east", "west"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("weighted = %v, want %v", got, want)
	}
}

func TestLeastLatencyPrefersUnusedThenFastest(t *testing.T) {
	sel := NewLeastLatency()
	ep, err := sel.Pick([]*Endpoint{{Name: "slow", Healthy: true, LatencyEWMA: 200 * time.Millisecond}, {Name: "fresh", Healthy: true}})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if ep.Name != "fresh" {
		t.Fatalf("first pick = %q, want fresh", ep.Name)
	}
	ep, err = sel.Pick([]*Endpoint{{Name: "slow", Healthy: true, LatencyEWMA: 200 * time.Millisecond}, {Name: "fast", Healthy: true, LatencyEWMA: 50 * time.Millisecond}})
	if err != nil {
		t.Fatalf("Pick() error = %v", err)
	}
	if ep.Name != "fast" {
		t.Fatalf("second pick = %q, want fast", ep.Name)
	}
}

func TestNoHealthyEndpoints(t *testing.T) {
	_, err := NewRoundRobin().Pick([]*Endpoint{{Name: "a", Healthy: false}})
	if !errors.Is(err, ErrNoHealthyEndpoints) {
		t.Fatalf("Pick() error = %v, want ErrNoHealthyEndpoints", err)
	}
}
