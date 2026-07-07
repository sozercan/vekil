// Package selector provides pluggable load-balancing strategies for
// multi-endpoint provider configurations.
package selector

import (
	"errors"
	"sync/atomic"
	"time"
)

// ErrNoEndpoints is returned when Pick is called with an empty list.
var ErrNoEndpoints = errors.New("no endpoints available")

// Endpoint represents a single upstream target within a provider. The Healthy
// flag is maintained externally by the health tracker and read by selectors at
// pick time. LatencyEWMA is updated atomically via accessor methods.
type Endpoint struct {
	Name    string
	BaseURL string
	Key     string
	Weight  uint
	Healthy bool
	// latencyEWMA stores the EWMA as int64 nanoseconds for atomic access.
	latencyEWMA atomic.Int64
}

// LoadLatencyEWMA returns the current EWMA latency, safe for concurrent use.
func (e *Endpoint) LoadLatencyEWMA() time.Duration {
	return time.Duration(e.latencyEWMA.Load())
}

// StoreLatencyEWMA sets the EWMA latency, safe for concurrent use.
func (e *Endpoint) StoreLatencyEWMA(d time.Duration) {
	e.latencyEWMA.Store(int64(d))
}

// SetLatencyEWMA sets the initial EWMA latency. Convenience for test setup
// and initialization. Equivalent to StoreLatencyEWMA.
func (e *Endpoint) SetLatencyEWMA(d time.Duration) {
	e.latencyEWMA.Store(int64(d))
}

// Selector picks the next endpoint to use from a list of candidates.
// Implementations must be safe for concurrent use.
type Selector interface {
	// Pick selects an endpoint from the provided list. The list may contain
	// both healthy and unhealthy endpoints; implementations should prefer
	// healthy ones but may fall back to unhealthy if none are healthy.
	Pick(endpoints []*Endpoint) (*Endpoint, error)
}

// New creates a Selector by name. Known names: "round_robin" (default),
// "weighted", "least_latency". An empty string yields round-robin.
func New(name string) Selector {
	switch name {
	case "weighted":
		return NewWeighted()
	case "least_latency":
		return NewLeastLatency()
	default:
		return NewRoundRobin()
	}
}
