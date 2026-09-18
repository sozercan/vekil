package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	maxAzureTrafficEntries      = 256
	maxAzureTrafficWaiters      = 64
	maxAzureTrafficWaitingBytes = 64 << 20
	maxAzureTrafficWait         = 5 * time.Minute
	// Keep one deployment from consuming all shared recovery capacity.
	maxAzureDeploymentWaiters      = 16
	maxAzureDeploymentWaitingBytes = 16 << 20
)

type azureDeploymentKey [sha256.Size]byte

// A nonzero size gives FIFO waiters distinct pointer identities.
type azureTrafficWaiter struct{ _ byte }

type azureCooldown struct {
	until        time.Time
	changed      chan struct{}
	probe        bool
	queue        []*azureTrafficWaiter
	waitingBytes int64
}

func (e *azureCooldown) notify() {
	close(e.changed)
	e.changed = make(chan struct{})
}

// Azure quotas belong to the physical resource/deployment, not to a public
// route, reasoning tier, or access token. Only explicit Azure routes use this
// controller; legacy retries and classifier admission retain their contracts.
type azureTrafficController struct {
	mu           sync.Mutex
	cooldowns    map[azureDeploymentKey]*azureCooldown
	waiters      int
	waitingBytes int64
	now          func() time.Time
}

func (c *azureTrafficController) timeNow() time.Time {
	if c.now != nil {
		return c.now()
	}
	return time.Now()
}

type azureRouteTraffic struct {
	controller          *azureTrafficController
	key                 azureDeploymentKey
	permit              *azureTrafficPermit
	jsonFailureObserved *atomic.Bool
}

type azureRouteTrafficContextKey struct{}

func (h *ProxyHandler) withAzureRouteTraffic(req *http.Request, target targetBinding) *http.Request {
	if h == nil || req == nil || req.URL == nil || target.provider == nil || target.provider.kind != providerTypeAzureOpenAI {
		return req
	}
	scheme := strings.ToLower(req.URL.Scheme)
	host := strings.TrimSuffix(strings.ToLower(req.URL.Hostname()), ".")
	if address, err := netip.ParseAddr(host); err == nil {
		host = address.Unmap().String()
	}
	port := req.URL.Port()
	if number, err := strconv.ParseUint(port, 10, 16); err == nil {
		port = strconv.FormatUint(number, 10)
	}
	if scheme == "https" && port == "443" || scheme == "http" && port == "80" {
		port = ""
	}
	if port != "" {
		host = net.JoinHostPort(host, port)
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	origin := scheme + "://" + host
	metadata := azureRouteTraffic{
		controller:          &h.azureTraffic,
		key:                 sha256.Sum256([]byte(origin + "\x00" + target.upstreamModel)),
		jsonFailureObserved: new(atomic.Bool),
	}
	return req.WithContext(context.WithValue(req.Context(), azureRouteTrafficContextKey{}, metadata))
}

func azureRouteTrafficFromRequest(req *http.Request) azureRouteTraffic {
	if req == nil {
		return azureRouteTraffic{}
	}
	metadata, _ := req.Context().Value(azureRouteTrafficContextKey{}).(azureRouteTraffic)
	return metadata
}

// Preparation and body accounting may inspect the same JSON failure. Share
// one observation across request copies so later reads cannot restart its reset.
// New physical sends and streamed failure events retain independent resets.
func (a azureRouteTraffic) observeJSONFailure(status int, headers http.Header) {
	if a.jsonFailureObserved != nil && !a.jsonFailureObserved.CompareAndSwap(false, true) {
		return
	}
	a.observe(status, headers)
}

func azureRetryAfter(headers http.Header) string {
	retryAfter, source := selectResponsesRetryAfter(headers)
	if source == "Retry-After" || source == "retry-after-ms" {
		return retryAfter
	}
	// Azure negative balances are exhausted, including -1. Other providers
	// may use -1 for unlimited quota, so keep this policy out of the shared parser.
	for _, dimension := range []string{"tokens", "requests"} {
		if _, exhausted := responsesQuotaRemaining(headers, dimension); !exhausted {
			continue
		}
		if reset, valid := responsesQuotaResetRetryAfter(headers, dimension); valid && (retryAfter == "" || positiveDecimalGreater(reset, retryAfter)) {
			retryAfter = reset
		}
	}
	return retryAfter
}

func (a azureRouteTraffic) observe(status int, headers http.Header) bool {
	if a.controller == nil {
		return false
	}
	exhausted := false
	for _, dimension := range []string{"tokens", "requests"} {
		if _, empty := responsesQuotaRemaining(headers, dimension); !empty {
			continue
		}
		exhausted = true
	}
	if status != http.StatusTooManyRequests && (status < 200 || status >= 300 || !exhausted) {
		return false
	}
	delay, valid := parseRetryAfter(azureRetryAfter(headers))
	if !valid {
		return false
	}
	c := a.controller
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.timeNow()
	entry := c.cooldowns[a.key]
	if entry == nil {
		if c.cooldowns == nil {
			c.cooldowns = make(map[azureDeploymentKey]*azureCooldown)
		}
		if len(c.cooldowns) >= maxAzureTrafficEntries {
			for key, old := range c.cooldowns {
				if !old.probe && len(old.queue) == 0 && !old.until.After(now) {
					delete(c.cooldowns, key)
				}
			}
			if len(c.cooldowns) >= maxAzureTrafficEntries {
				return false
			}
		}
		entry = &azureCooldown{changed: make(chan struct{})}
		c.cooldowns[a.key] = entry
	}
	if until := now.Add(delay); until.After(entry.until) {
		entry.until = until
		entry.notify()
	}
	return true
}

type azureTrafficPermit struct {
	controller *azureTrafficController
	key        azureDeploymentKey
	entry      *azureCooldown
	// Protected by controller.mu. Requesting release does not end ownership
	// while an abandoned transport body is still being read or closed.
	bodyPending      bool
	activeReads      int
	closing          bool
	releaseRequested bool
	released         bool
}

func (p *azureTrafficPermit) holdResponseBody() {
	if p == nil || p.controller == nil {
		return
	}
	p.controller.mu.Lock()
	defer p.controller.mu.Unlock()
	p.bodyPending = true
}

func (p *azureTrafficPermit) beginBodyRead() bool {
	if p == nil || p.controller == nil {
		return true
	}
	p.controller.mu.Lock()
	defer p.controller.mu.Unlock()
	if p.closing {
		return false
	}
	p.activeReads++
	return true
}

func (p *azureTrafficPermit) endBodyRead() {
	if p == nil || p.controller == nil {
		return
	}
	p.controller.mu.Lock()
	defer p.controller.mu.Unlock()
	p.activeReads--
	p.releaseIfCompleteLocked()
}

func (p *azureTrafficPermit) beginBodyClose() {
	if p == nil || p.controller == nil {
		return
	}
	p.controller.mu.Lock()
	defer p.controller.mu.Unlock()
	p.closing = true
}

func (p *azureTrafficPermit) completeResponseBody() {
	if p == nil || p.controller == nil {
		return
	}
	p.controller.mu.Lock()
	defer p.controller.mu.Unlock()
	p.bodyPending = false
	p.releaseIfCompleteLocked()
}

func (p *azureTrafficPermit) release() {
	if p == nil || p.controller == nil {
		return
	}
	p.controller.mu.Lock()
	defer p.controller.mu.Unlock()
	p.releaseRequested = true
	p.releaseIfCompleteLocked()
}

func (p *azureTrafficPermit) releaseIfCompleteLocked() {
	if p.released || !p.releaseRequested || p.bodyPending || p.activeReads != 0 {
		return
	}
	p.released = true
	c := p.controller
	p.entry.probe = false
	// While a recovery queue exists, release one caller per completed
	// attempt. Deleting the entry and waking everyone would recreate the
	// burst that exhausted the token quota.
	if len(p.entry.queue) == 0 && !p.entry.until.After(c.timeNow()) && c.cooldowns[p.key] == p.entry {
		delete(c.cooldowns, p.key)
	}
	p.entry.notify()
}

func azureCooldownResponse(req *http.Request, delay time.Duration) *http.Response {
	body, _ := json.Marshal(map[string]any{"error": map[string]string{
		"type": "rate_limit_error", "code": "rate_limit_exceeded",
		"message": "Azure deployment rate limit is still active; retry after the indicated delay",
	}})
	return &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Status:     "429 Too Many Requests",
		Header: http.Header{
			"Content-Type": {"application/json"},
			"Retry-After":  {strconv.FormatInt(max(1, durationSecondsCeil(delay)), 10)},
		},
		Body:    io.NopCloser(strings.NewReader(string(body))),
		Request: req,
	}
}

// A known cooldown may reject a fresh operation locally so priority failover
// can use another target. Pinned/single-target operations wait without spending
// a send. The queue and the total wait are bounded independently of the usually
// much longer streaming-inference deadline.
func (h *ProxyHandler) acquireAzureRouteInference(req *http.Request, canSwitch bool) (*azureTrafficPermit, *http.Response, error) {
	metadata := azureRouteTrafficFromRequest(req)
	if metadata.controller == nil {
		return nil, nil, nil
	}
	c := metadata.controller
	c.mu.Lock()
	tracked := c.cooldowns[metadata.key] != nil
	c.mu.Unlock()
	if !tracked {
		return nil, nil, nil
	}
	ctx, cancelAdmission := copilotInferenceAdmissionContext(req)
	defer cancelAdmission()
	if operation := routeOperationFromContext(req.Context()); operation != nil && operation.inbound != nil {
		if deadline, ok := operation.inbound.Deadline(); ok {
			var cancelDeadline context.CancelFunc
			ctx, cancelDeadline = context.WithDeadline(ctx, deadline)
			defer cancelDeadline()
		}
	}
	waitCtx, cancelWait := context.WithTimeout(ctx, maxAzureTrafficWait)
	defer cancelWait()
	weight := max(0, req.ContentLength)
	var waiter *azureTrafficWaiter
	var queued *azureCooldown
	defer func() {
		if waiter == nil {
			return
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		for i, candidate := range queued.queue {
			if candidate == waiter {
				queued.queue = append(queued.queue[:i], queued.queue[i+1:]...)
				c.waiters--
				c.waitingBytes -= weight
				queued.waitingBytes -= weight
				queued.notify()
				break
			}
		}
	}()
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if h.ShuttingDown() {
			return nil, nil, context.Canceled
		}
		c.mu.Lock()
		entry := c.cooldowns[metadata.key]
		if entry == nil {
			c.mu.Unlock()
			return nil, nil, nil
		}
		delay := max(time.Duration(0), entry.until.Sub(c.timeNow()))
		if canSwitch && (delay > 0 || entry.probe || len(entry.queue) > 0) || waitCtx.Err() != nil || !retryDelayFitsBudget(waitCtx, delay) {
			c.mu.Unlock()
			return nil, azureCooldownResponse(req, delay), nil
		}
		first := len(entry.queue) == 0 || waiter != nil && entry.queue[0] == waiter
		if delay == 0 && !entry.probe && first {
			if waiter != nil {
				entry.queue = entry.queue[1:]
				c.waiters--
				c.waitingBytes -= weight
				entry.waitingBytes -= weight
				waiter = nil
			}
			entry.probe = true
			c.mu.Unlock()
			return &azureTrafficPermit{controller: c, key: metadata.key, entry: entry}, nil, nil
		}
		if waiter == nil {
			if len(entry.queue) >= maxAzureDeploymentWaiters || weight > maxAzureDeploymentWaitingBytes-entry.waitingBytes ||
				c.waiters >= maxAzureTrafficWaiters || weight > maxAzureTrafficWaitingBytes-c.waitingBytes {
				c.mu.Unlock()
				return nil, nil, &providerRequestError{statusCode: http.StatusServiceUnavailable, code: "rate_limit_queue_full", err: fmt.Errorf("azure rate-limit recovery queue is full")}
			}
			waiter = &azureTrafficWaiter{}
			queued = entry
			entry.queue = append(entry.queue, waiter)
			c.waiters++
			c.waitingBytes += weight
			entry.waitingBytes += weight
		}
		changed := entry.changed
		c.mu.Unlock()
		var timer *time.Timer
		var reset <-chan time.Time
		if delay > 0 {
			timer = time.NewTimer(delay)
			reset = timer.C
		}
		select {
		case <-waitCtx.Done():
		case <-h.lifecycleContext().Done():
		case <-changed:
		case <-reset:
		}
		if timer != nil {
			timer.Stop()
		}
	}
}
