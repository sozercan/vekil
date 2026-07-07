package selector

import "sync"

// Weighted implements smooth weighted round-robin (Nginx-style).
// Each endpoint accumulates its weight per pick; the one with the highest
// current weight is selected and then reduced by the total weight sum.
type Weighted struct {
	mu             sync.Mutex
	currentWeights map[string]int64
}

// NewWeighted creates a new weighted round-robin selector.
func NewWeighted() *Weighted {
	return &Weighted{
		currentWeights: make(map[string]int64),
	}
}

// Pick selects the endpoint with the highest accumulated weight.
// Zero-weight endpoints are treated as weight=1 to avoid starvation.
// Healthy endpoints are preferred; if none are healthy it falls back to all.
func (w *Weighted) Pick(endpoints []*Endpoint) (*Endpoint, error) {
	if len(endpoints) == 0 {
		return nil, ErrNoEndpoints
	}

	healthy := filterHealthy(endpoints)
	candidates := healthy
	if len(candidates) == 0 {
		candidates = endpoints
	}

	w.mu.Lock()
	defer w.mu.Unlock()

	var totalWeight int64
	for _, ep := range candidates {
		totalWeight += effectiveWeight(ep)
	}

	var best *Endpoint
	var bestWeight int64

	for _, ep := range candidates {
		ew := effectiveWeight(ep)
		w.currentWeights[ep.Name] += ew
		cw := w.currentWeights[ep.Name]
		if best == nil || cw > bestWeight {
			best = ep
			bestWeight = cw
		}
	}

	if best != nil {
		w.currentWeights[best.Name] -= totalWeight
	}

	return best, nil
}

func effectiveWeight(ep *Endpoint) int64 {
	if ep.Weight == 0 {
		return 1
	}
	return int64(ep.Weight)
}
