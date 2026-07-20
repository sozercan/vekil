package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	defaultChatRouteDiscoveryTimeout        = 2 * time.Second
	defaultChatRouteDiscoverySuccessTTL     = 5 * time.Minute
	defaultChatRouteDiscoveryFailureBackoff = 5 * time.Second
)

type chatRouteDiscoveryCache struct {
	mu        sync.Mutex
	providers map[string]*chatRouteProviderDiscovery
	now       func() time.Time

	timeout        time.Duration
	successTTL     time.Duration
	failureBackoff time.Duration
}

type chatRouteProviderDiscovery struct {
	inflight     *chatRouteDiscoveryFlight
	successUntil time.Time
	failureUntil time.Time
	hardFailure  error
}

type chatRouteDiscoveryFlight struct {
	done chan struct{}
	err  error
}

func newChatRouteDiscoveryCache() chatRouteDiscoveryCache {
	return chatRouteDiscoveryCache{
		providers:      make(map[string]*chatRouteProviderDiscovery),
		now:            time.Now,
		timeout:        defaultChatRouteDiscoveryTimeout,
		successTTL:     defaultChatRouteDiscoverySuccessTTL,
		failureBackoff: defaultChatRouteDiscoveryFailureBackoff,
	}
}

func (h *ProxyHandler) resolveChatRoute(ctx context.Context, model string) (resolvedChatRoute, error) {
	model = strings.TrimSpace(model)
	if !h.modelAllowedForContext(ctx, model, providerEndpointChatCompletions) {
		return resolvedChatRoute{}, modelNotAllowedRequestError(model)
	}
	setup := h.providerSetupForContext(ctx)
	provider, owner, known := h.resolveProviderModelForContext(ctx, model, providerEndpointChatCompletions)
	if provider == nil && !known && setup != nil && setup.hasConfiguredState && strings.TrimSpace(setup.defaultProviderID) == "" && setup.routeRegistry() != nil {
		return resolvedChatRoute{}, &providerRequestError{
			statusCode: http.StatusBadRequest,
			err:        fmt.Errorf("model %q does not support %s", model, providerEndpointChatCompletions),
		}
	}
	if model != "" && !known && providerUsesDynamicModels(provider) {
		if err := h.refreshUnknownChatRouteProvider(ctx, setup, provider); err != nil {
			return resolvedChatRoute{}, err
		}
		provider, owner, known = h.resolveProviderModelForContext(ctx, model, providerEndpointChatCompletions)
	}
	return chooseChatRoute(provider, owner, known, model)
}

func (h *ProxyHandler) refreshUnknownChatRouteProvider(waitCtx context.Context, setup *providerSetup, provider *providerRuntime) error {
	if waitCtx == nil {
		waitCtx = context.Background()
	}
	if err := waitCtx.Err(); err != nil {
		return err
	}
	if provider == nil {
		return nil
	}

	cache := h.chatRouteCacheForContext(waitCtx)
	if cache == nil {
		return fmt.Errorf("chat route discovery cache is unavailable")
	}
	cache.mu.Lock()
	cache.ensureDefaultsLocked()
	state := cache.providers[provider.id]
	if state == nil {
		state = &chatRouteProviderDiscovery{}
		cache.providers[provider.id] = state
	}
	now := cache.nowLocked()
	if now.Before(state.successUntil) {
		cache.mu.Unlock()
		return nil
	}
	if now.Before(state.failureUntil) {
		err := state.hardFailure
		cache.mu.Unlock()
		return err
	}
	if state.inflight != nil {
		flight := state.inflight
		cache.mu.Unlock()
		return waitForChatRouteDiscovery(waitCtx, flight)
	}

	flight := &chatRouteDiscoveryFlight{done: make(chan struct{})}
	state.inflight = flight
	timeout := cache.timeout
	cache.mu.Unlock()

	if !h.beginLifecycleWorker() {
		h.completeChatRouteProviderRefresh(cache, state, flight, errProxyLifecycleShutdown, nil)
		return waitForChatRouteDiscovery(waitCtx, flight)
	}
	go func() {
		defer h.endLifecycleWorker()
		h.runChatRouteProviderRefresh(setup, cache, provider, state, flight, timeout)
	}()
	return waitForChatRouteDiscovery(waitCtx, flight)
}

func waitForChatRouteDiscovery(ctx context.Context, flight *chatRouteDiscoveryFlight) error {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-flight.done:
		if err := ctx.Err(); err != nil {
			return err
		}
		return flight.err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (h *ProxyHandler) runChatRouteProviderRefresh(
	setup *providerSetup,
	cache *chatRouteDiscoveryCache,
	provider *providerRuntime,
	state *chatRouteProviderDiscovery,
	flight *chatRouteDiscoveryFlight,
	timeout time.Duration,
) {
	ctx, cancel := h.newLifecycleUpstreamContext(timeout)
	result, fetchErr := h.fetchProviderModels(ctx, provider, "", "")
	cancel()

	var installErr error
	if fetchErr == nil && !result.notModified {
		installErr = setup.replaceProviderModels(provider.id, result.models)
	}

	h.completeChatRouteProviderRefresh(cache, state, flight, fetchErr, installErr)
}

func (h *ProxyHandler) completeChatRouteProviderRefresh(
	cache *chatRouteDiscoveryCache,
	state *chatRouteProviderDiscovery,
	flight *chatRouteDiscoveryFlight,
	fetchErr error,
	installErr error,
) {
	if cache == nil {
		return
	}
	cache.mu.Lock()
	cache.ensureDefaultsLocked()
	now := cache.nowLocked()
	if fetchErr == nil && installErr == nil {
		state.successUntil = now.Add(cache.successTTL)
		state.failureUntil = time.Time{}
		state.hardFailure = nil
	} else {
		state.successUntil = time.Time{}
		state.failureUntil = now.Add(cache.failureBackoff)
		state.hardFailure = installErr
	}
	flight.err = installErr
	if state.inflight == flight {
		state.inflight = nil
	}
	close(flight.done)
	cache.mu.Unlock()
}

func (c *chatRouteDiscoveryCache) ensureDefaultsLocked() {
	if c.providers == nil {
		c.providers = make(map[string]*chatRouteProviderDiscovery)
	}
	if c.now == nil {
		c.now = time.Now
	}
	if c.timeout <= 0 {
		c.timeout = defaultChatRouteDiscoveryTimeout
	}
	if c.successTTL <= 0 {
		c.successTTL = defaultChatRouteDiscoverySuccessTTL
	}
	if c.failureBackoff <= 0 {
		c.failureBackoff = defaultChatRouteDiscoveryFailureBackoff
	}
}

func (c *chatRouteDiscoveryCache) nowLocked() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}
