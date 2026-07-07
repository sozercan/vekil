package proxy

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrorBudget represents an allowance of N errors per time unit (e.g. "10/m").
// It supports the DSL format "N/{s,m,h}" and implements text marshaling for
// YAML/JSON serialization.
type ErrorBudget struct {
	Count  int
	Window time.Duration
}

// ParseErrorBudget parses a DSL string like "10/m", "5/s", or "1500/h".
func ParseErrorBudget(s string) (ErrorBudget, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return ErrorBudget{}, errors.New("empty error budget")
	}

	parts := strings.SplitN(s, "/", 2)
	if len(parts) != 2 {
		return ErrorBudget{}, fmt.Errorf("invalid error budget %q: expected format N/{s,m,h}", s)
	}

	count, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || count <= 0 {
		return ErrorBudget{}, fmt.Errorf("invalid error budget count %q: must be a positive integer", parts[0])
	}

	unit := strings.TrimSpace(parts[1])
	var window time.Duration
	switch unit {
	case "s":
		window = time.Second
	case "m":
		window = time.Minute
	case "h":
		window = time.Hour
	default:
		return ErrorBudget{}, fmt.Errorf("invalid error budget unit %q: must be s, m, or h", unit)
	}

	return ErrorBudget{Count: count, Window: window}, nil
}

// String returns the DSL representation (e.g. "10/m").
func (eb ErrorBudget) String() string {
	var unit string
	switch eb.Window {
	case time.Second:
		unit = "s"
	case time.Minute:
		unit = "m"
	case time.Hour:
		unit = "h"
	default:
		unit = fmt.Sprintf("%ds", int(eb.Window.Seconds()))
	}
	return fmt.Sprintf("%d/%s", eb.Count, unit)
}

// MarshalText implements encoding.TextMarshaler for YAML/JSON serialization.
func (eb ErrorBudget) MarshalText() ([]byte, error) {
	return []byte(eb.String()), nil
}

// UnmarshalText implements encoding.TextUnmarshaler for YAML/JSON deserialization.
func (eb *ErrorBudget) UnmarshalText(text []byte) error {
	parsed, err := ParseErrorBudget(string(text))
	if err != nil {
		return err
	}
	*eb = parsed
	return nil
}

// DefaultErrorBudget is the default error budget when none is configured.
var DefaultErrorBudget = ErrorBudget{Count: 10, Window: time.Minute}

// DefaultCooldown is the default quarantine duration when an endpoint's error
// budget is exhausted.
const DefaultCooldown = 30 * time.Second

// EndpointHealthTracker monitors the health of a single endpoint using a
// rolling-window error budget. When the budget is exhausted the endpoint is
// quarantined for a cooldown period, then enters half-open state for probing.
type EndpointHealthTracker struct {
	mu       sync.Mutex
	budget   ErrorBudget
	cooldown time.Duration
	// Rolling window of failure timestamps.
	failures []time.Time
	// Quarantine state.
	quarantined    bool
	quarantineEnd  time.Time
	halfOpen       bool
	halfOpenProbed bool
	// Clock function for testing (defaults to time.Now).
	now func() time.Time
}

// NewEndpointHealthTracker creates a tracker with the given budget and cooldown.
func NewEndpointHealthTracker(budget ErrorBudget, cooldown time.Duration) *EndpointHealthTracker {
	return &EndpointHealthTracker{
		budget:   budget,
		cooldown: cooldown,
		now:      time.Now,
	}
}

// IsHealthy returns true if the endpoint is available for requests.
// An endpoint is healthy if it is not quarantined, or if the cooldown has
// elapsed (half-open state allows one probe).
func (t *EndpointHealthTracker) IsHealthy() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.isHealthyLocked()
}

func (t *EndpointHealthTracker) isHealthyLocked() bool {
	if !t.quarantined {
		return true
	}
	now := t.now()
	if now.After(t.quarantineEnd) || now.Equal(t.quarantineEnd) {
		// Cooldown elapsed: enter half-open state.
		if !t.halfOpen {
			t.halfOpen = true
			t.halfOpenProbed = false
		}
		// In half-open state, allow one probe request.
		if t.halfOpen && !t.halfOpenProbed {
			return true
		}
	}
	return false
}

// RecordSuccess records a successful request. If in half-open state, it
// fully restores the endpoint.
func (t *EndpointHealthTracker) RecordSuccess() {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.halfOpen {
		// Successful probe: restore to fully healthy.
		t.quarantined = false
		t.halfOpen = false
		t.halfOpenProbed = false
		t.failures = t.failures[:0]
	}
}

// RecordFailure records a failed request. If the rolling-window error budget
// is exhausted, the endpoint is quarantined.
func (t *EndpointHealthTracker) RecordFailure() {
	t.mu.Lock()
	defer t.mu.Unlock()

	now := t.now()

	if t.halfOpen {
		// Failed probe: re-quarantine.
		t.halfOpenProbed = true
		t.quarantineEnd = now.Add(t.cooldown)
		t.halfOpen = false
		return
	}

	// Add failure to rolling window.
	t.failures = append(t.failures, now)

	// Prune failures outside the window.
	windowStart := now.Add(-t.budget.Window)
	pruned := t.failures[:0]
	for _, ts := range t.failures {
		if ts.After(windowStart) {
			pruned = append(pruned, ts)
		}
	}
	t.failures = pruned

	// Check if budget is exhausted.
	if len(t.failures) >= t.budget.Count {
		t.quarantined = true
		t.quarantineEnd = now.Add(t.cooldown)
		t.halfOpen = false
		t.halfOpenProbed = false
	}
}

// IsQuarantined returns true if the endpoint is currently quarantined
// (not including half-open state which allows probes).
func (t *EndpointHealthTracker) IsQuarantined() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.quarantined && !t.halfOpen
}
