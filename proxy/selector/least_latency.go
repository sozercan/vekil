package selector

import "time"

// LeastLatency selects the endpoint with the lowest latency EWMA.
// Endpoints with zero latency (no observations yet) are treated as lowest
// to allow initial probing.
type LeastLatency struct{}

// NewLeastLatency creates a new least-latency selector.
func NewLeastLatency() *LeastLatency {
	return &LeastLatency{}
}

// Pick selects the healthy endpoint with the lowest latency EWMA.
func (ll *LeastLatency) Pick(endpoints []*Endpoint) (*Endpoint, error) {
	if len(endpoints) == 0 {
		return nil, ErrNoEndpoints
	}

	healthy := filterHealthy(endpoints)
	candidates := healthy
	if len(candidates) == 0 {
		candidates = endpoints
	}

	var best *Endpoint
	bestLatency := time.Duration(0)

	for _, ep := range candidates {
		latency := ep.LoadLatencyEWMA()
		if best == nil {
			best = ep
			bestLatency = latency
			continue
		}
		// Prefer zero-latency endpoints (unprobed) to allow discovery.
		if latency == 0 && bestLatency != 0 {
			best = ep
			bestLatency = 0
			continue
		}
		if bestLatency == 0 {
			continue
		}
		if latency < bestLatency {
			best = ep
			bestLatency = latency
		}
	}

	return best, nil
}
