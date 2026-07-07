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
	next int
}

func NewWeighted() *Weighted { return &Weighted{} }

func (s *Weighted) Pick(endpoints []*Endpoint) (*Endpoint, error) {
	healthy := healthyEndpoints(endpoints)
	if len(healthy) == 0 {
		return nil, ErrNoHealthyEndpoints
	}
	weighted := make([]*Endpoint, 0, len(healthy))
	for _, endpoint := range healthy {
		weight := endpoint.Weight
		if weight == 0 {
			weight = 1
		}
		for range weight {
			weighted = append(weighted, endpoint)
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	idx := s.next % len(weighted)
	s.next = (s.next + 1) % len(weighted)
	return weighted[idx], nil
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
		case best.LatencyEWMA == 0 && candidate.LatencyEWMA != 0:
			// Keep never-used endpoints ahead of known-latency endpoints so they get probes.
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
