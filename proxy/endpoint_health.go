package proxy

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

type endpointHealthConfig struct {
	errorBudget endpointErrorBudget
	cooldown    time.Duration
}

type endpointErrorBudget struct {
	Limit  int
	Window time.Duration
}

func defaultEndpointHealthConfig() endpointHealthConfig {
	return endpointHealthConfig{errorBudget: endpointErrorBudget{Limit: 10, Window: time.Minute}, cooldown: 30 * time.Second}
}

func parseEndpointErrorBudget(raw string) (endpointErrorBudget, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return defaultEndpointHealthConfig().errorBudget, nil
	}
	parts := strings.Split(raw, "/")
	if len(parts) != 2 {
		return endpointErrorBudget{}, fmt.Errorf("invalid error_budget %q: expected N/{ms,s,m,h}", raw)
	}
	limit, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || limit <= 0 {
		return endpointErrorBudget{}, fmt.Errorf("invalid error_budget %q: count must be positive", raw)
	}
	var window time.Duration
	switch strings.TrimSpace(parts[1]) {
	case "ms":
		window = time.Millisecond
	case "s":
		window = time.Second
	case "m":
		window = time.Minute
	case "h":
		window = time.Hour
	default:
		return endpointErrorBudget{}, fmt.Errorf("invalid error_budget %q: unit must be one of ms,s,m,h", raw)
	}
	return endpointErrorBudget{Limit: limit, Window: window}, nil
}

type endpointHealthTracker struct {
	mu                 sync.Mutex
	cfg                endpointHealthConfig
	failures           []time.Time
	quarantinedUntil   time.Time
	deprioritizedUntil time.Time
	latencyEWMA        time.Duration
}

func newEndpointHealthTracker(cfg endpointHealthConfig) *endpointHealthTracker {
	if cfg.errorBudget.Limit <= 0 || cfg.errorBudget.Window <= 0 {
		cfg.errorBudget = defaultEndpointHealthConfig().errorBudget
	}
	if cfg.cooldown <= 0 {
		cfg.cooldown = defaultEndpointHealthConfig().cooldown
	}
	return &endpointHealthTracker{cfg: cfg}
}

func (h *endpointHealthTracker) healthy(now time.Time) bool {
	if h == nil {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.quarantinedUntil.IsZero() {
		if now.Before(h.quarantinedUntil) {
			return false
		}
		h.quarantinedUntil = time.Time{}
	}
	if !h.deprioritizedUntil.IsZero() {
		if now.Before(h.deprioritizedUntil) {
			return false
		}
		h.deprioritizedUntil = time.Time{}
		if h.latencyEWMA >= time.Hour {
			h.latencyEWMA = 0
		}
	}
	return true
}

func (h *endpointHealthTracker) latency() time.Duration {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.deprioritizedUntil.IsZero() && !time.Now().Before(h.deprioritizedUntil) {
		h.deprioritizedUntil = time.Time{}
		if h.latencyEWMA >= time.Hour {
			h.latencyEWMA = 0
		}
	}
	return h.latencyEWMA
}

func (h *endpointHealthTracker) recordSuccess(latency time.Duration) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.quarantinedUntil.IsZero() {
		if time.Now().Before(h.quarantinedUntil) {
			return
		}
		h.quarantinedUntil = time.Time{}
		h.deprioritizedUntil = time.Time{}
		if h.latencyEWMA >= time.Hour {
			h.latencyEWMA = 0
		}
	}
	if latency <= 0 {
		return
	}
	if h.latencyEWMA <= 0 || h.latencyEWMA > time.Minute {
		h.latencyEWMA = latency
		return
	}
	h.latencyEWMA = (h.latencyEWMA*7 + latency*3) / 10
}

func (h *endpointHealthTracker) recordFailure(now time.Time) bool {
	if h == nil {
		return false
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	cutoff := now.Add(-h.cfg.errorBudget.Window)
	kept := h.failures[:0]
	for _, failure := range h.failures {
		if failure.After(cutoff) {
			kept = append(kept, failure)
		}
	}
	h.failures = append(kept, now)
	// Deprioritize failed endpoints for least-latency selection immediately, not
	// only after quarantine, so retries can move to healthy siblings within the
	// same request even when the error budget is larger than the retry count.
	h.latencyEWMA = time.Hour
	h.deprioritizedUntil = now.Add(h.cfg.cooldown)
	if len(h.failures) >= h.cfg.errorBudget.Limit {
		h.quarantinedUntil = now.Add(h.cfg.cooldown)
		h.deprioritizedUntil = h.quarantinedUntil
		h.failures = nil
		return true
	}
	return false
}

func (h *endpointHealthTracker) usableIgnoringPenalty(now time.Time) bool {
	if h == nil {
		return true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if !h.quarantinedUntil.IsZero() && now.Before(h.quarantinedUntil) {
		return false
	}
	if !h.quarantinedUntil.IsZero() && !now.Before(h.quarantinedUntil) {
		h.quarantinedUntil = time.Time{}
	}
	if !h.deprioritizedUntil.IsZero() && !now.Before(h.deprioritizedUntil) {
		h.deprioritizedUntil = time.Time{}
		if h.latencyEWMA >= time.Hour {
			h.latencyEWMA = 0
		}
	}
	return true
}
