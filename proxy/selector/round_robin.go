package selector

import "sync/atomic"

// RoundRobin rotates through endpoints using an atomic counter.
// Healthy endpoints are preferred; if none are healthy it falls back to all.
type RoundRobin struct {
	counter atomic.Uint64
}

// NewRoundRobin creates a new round-robin selector.
func NewRoundRobin() *RoundRobin {
	return &RoundRobin{}
}

// Pick selects the next healthy endpoint in rotation order.
func (rr *RoundRobin) Pick(endpoints []*Endpoint) (*Endpoint, error) {
	if len(endpoints) == 0 {
		return nil, ErrNoEndpoints
	}

	healthy := filterHealthy(endpoints)
	candidates := healthy
	if len(candidates) == 0 {
		candidates = endpoints
	}

	idx := rr.counter.Add(1) - 1
	return candidates[idx%uint64(len(candidates))], nil
}

func filterHealthy(endpoints []*Endpoint) []*Endpoint {
	var out []*Endpoint
	for _, ep := range endpoints {
		if ep.Healthy {
			out = append(out, ep)
		}
	}
	return out
}
