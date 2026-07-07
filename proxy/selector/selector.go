package selector

import (
	"errors"
	"sync"
	"time"
)

// Endpoint is the selection-time view of one configured upstream endpoint.
type Endpoint struct {
	Name        string
	BaseURL     string
	Key         string
	Weight      uint
	Healthy     bool
	LatencyEWMA time.Duration
}

// Selector picks one endpoint from the currently configured candidates.
type Selector interface {
	Pick(endpoints []*Endpoint) (*Endpoint, error)
}

var ErrNoHealthyEndpoints = errors.New("no healthy endpoints")

type RoundRobin struct {
	mu   sync.Mutex
	next int
}

func NewRoundRobin() *RoundRobin { return &RoundRobin{} }

func (s *RoundRobin) Pick(endpoints []*Endpoint) (*Endpoint, error) {
	healthy := healthyEndpoints(endpoints)
	if len(healthy) == 0 {
		return nil, ErrNoHealthyEndpoints
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.next % len(healthy)
	s.next = (s.next + 1) % len(healthy)
	return healthy[idx], nil
}

type Weighted struct {
	mu   sync.Mutex
	next uint64
}

func NewWeighted() *Weighted { return &Weighted{} }

func (s *Weighted) Pick(endpoints []*Endpoint) (*Endpoint, error) {
	healthy := healthyEndpoints(endpoints)
	if len(healthy) == 0 {
		return nil, ErrNoHealthyEndpoints
	}
	var total uint64
	for _, endpoint := range healthy {
		weight := uint64(endpoint.Weight)
		if weight == 0 {
			weight = 1
		}
		total += weight
	}
	s.mu.Lock()
	pick := s.next % total
	s.next = (s.next + 1) % total
	s.mu.Unlock()
	var cumulative uint64
	for _, endpoint := range healthy {
		weight := uint64(endpoint.Weight)
		if weight == 0 {
			weight = 1
		}
		cumulative += weight
		if pick < cumulative {
			return endpoint, nil
		}
	}
	return healthy[len(healthy)-1], nil
}

type LeastLatency struct{}

func NewLeastLatency() *LeastLatency { return &LeastLatency{} }

func (s *LeastLatency) Pick(endpoints []*Endpoint) (*Endpoint, error) {
	healthy := healthyEndpoints(endpoints)
	if len(healthy) == 0 {
		return nil, ErrNoHealthyEndpoints
	}
	best := healthy[0]
	for _, candidate := range healthy[1:] {
		switch {
		case best.LatencyEWMA == 0:
			// Keep the earliest never-used endpoint ahead of known-latency endpoints
			// and other never-used endpoints so retries can deprioritize it after a
			// failure via the endpoint health tracker.
			continue
		case candidate.LatencyEWMA == 0:
			best = candidate
		case candidate.LatencyEWMA < best.LatencyEWMA:
			best = candidate
		}
	}
	return best, nil
}

func healthyEndpoints(endpoints []*Endpoint) []*Endpoint {
	healthy := make([]*Endpoint, 0, len(endpoints))
	for _, endpoint := range endpoints {
		if endpoint != nil && endpoint.Healthy {
			healthy = append(healthy, endpoint)
		}
	}
	return healthy
}
