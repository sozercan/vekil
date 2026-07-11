package proxy

import (
	"context"
	"time"
)

type modelsCacheFlightResult struct {
	entry    cachedModelsResponse
	hasEntry bool
	err      error
}

type modelsCacheFlight struct {
	done   chan struct{}
	result modelsCacheFlightResult
}

func (c *modelsCache) nowTime() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

// doFlight shares one model-catalog refresh per cache key. The leader performs
// work on its caller-supplied lifecycle context; waiters may stop waiting on
// their own request context without canceling that shared refresh.
func (c *modelsCache) doFlight(waitCtx context.Context, key string, fn func() modelsCacheFlightResult) modelsCacheFlightResult {
	if waitCtx == nil {
		waitCtx = context.Background()
	}

	c.flightMu.Lock()
	if c.flights == nil {
		c.flights = make(map[string]*modelsCacheFlight)
	}
	if flight, ok := c.flights[key]; ok {
		c.flightMu.Unlock()
		if err := waitCtx.Err(); err != nil {
			return modelsCacheFlightResult{err: err}
		}
		select {
		case <-flight.done:
			if err := waitCtx.Err(); err != nil {
				return modelsCacheFlightResult{err: err}
			}
			return flight.result
		case <-waitCtx.Done():
			return modelsCacheFlightResult{err: waitCtx.Err()}
		}
	}

	flight := &modelsCacheFlight{done: make(chan struct{})}
	c.flights[key] = flight
	c.flightMu.Unlock()

	result := fn()

	c.flightMu.Lock()
	flight.result = result
	delete(c.flights, key)
	close(flight.done)
	c.flightMu.Unlock()

	return result
}
