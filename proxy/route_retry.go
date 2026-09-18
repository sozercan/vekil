package proxy

import (
	"context"
	"errors"
	"net/http"
)

// A retry stopped before dispatch has no new upstream result. Keep the last
// rejection and its correlation ID, while recording why recovery stopped.
func suppressPendingRouteRetry(operation *routeOperation, failures []routeAttemptFailure, decision routeRetryDecision, blocked *http.Response) {
	if len(failures) == 0 {
		return
	}
	failure := &failures[len(failures)-1]
	failure.decision = decision
	if blocked != nil {
		retryAfter := blocked.Header.Get("Retry-After")
		delay, valid := parseRetryAfter(retryAfter)
		previous, _ := parseRetryAfter(failure.retryAfter)
		if valid && delay > previous {
			failure.retryAfter = retryAfter
			if failure.response != nil {
				failure.response.header.Set("Retry-After", retryAfter)
			}
			var upstreamErr *upstreamError
			if errors.As(failure.err, &upstreamErr) {
				upstreamErr.retryAfter = retryAfter
				if upstreamErr.headers == nil {
					upstreamErr.headers = make(http.Header)
				}
				upstreamErr.headers.Set("Retry-After", retryAfter)
			}
		}
		_ = blocked.Body.Close()
	}
	operation.mu.Lock()
	defer operation.mu.Unlock()
	if n := len(operation.trace); n > 0 && operation.trace[n-1].Decision == routeRetrySameTarget {
		operation.trace[n-1].Decision = decision
	}
}

// Target ownership restricts migration, not replay of a request that was
// authoritatively rejected before execution. A websocket's prior protocol
// frames also retain the owner without making a new, rejected turn unsafe.
func (o *routeOperation) sameTargetRetryDecision(ctx context.Context, kind routeAttemptKind, shuttingDown bool) routeRetryDecision {
	if shuttingDown || ctx.Err() != nil {
		return routeRetrySuppressedLifecycle
	}
	if o.inbound != nil && o.inbound.Err() != nil {
		return routeRetrySuppressedAdmission
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.commitment != downstreamCommitmentNone && (!o.hardPinned || o.commitment != downstreamCommitmentProtocolFrame) {
		return routeRetrySuppressedCommitment
	}
	if kind != routeAttemptNormal {
		return routeRetrySuppressedMode
	}
	if o.remainingUpstreamSends <= 0 {
		return routeRetrySuppressedBudget
	}
	return routeRetrySameTarget
}

func routeCanSwitchAfterAttempt(ctx context.Context, operation *routeOperation, endpoint string, kind routeAttemptKind, shuttingDown bool) bool {
	return operation.route.policy.mode == routeModePriorityFailover &&
		operation.allowsAutomaticTargetSwitch(kind) && operation.retryAdmissionOpen(ctx, shuttingDown) &&
		len(orderedRouteTargets(operation.route, operation, endpoint)) > 0
}

func (h *ProxyHandler) explicitRouteRejectionDecision(ctx context.Context, operation *routeOperation, target targetBinding, endpoint string, kind routeAttemptKind, failure routeAttemptFailure, traffic azureRouteTraffic) routeRetryDecision {
	delay, validReset := parseRetryAfter(failure.retryAfter)
	azureRecovery := target.provider.kind == providerTypeAzureOpenAI && failure.statusCode == http.StatusTooManyRequests && validReset
	decision := routeRetrySuppressedMode
	if !operation.allowsAutomaticTargetSwitch(kind) {
		decision = routeRetrySuppressedState
	}
	if operation.route.policy.mode == routeModePriorityFailover && operation.allowsAutomaticTargetSwitch(kind) && operation.retryAdmissionOpen(ctx, h.ShuttingDown()) {
		// Preserve legacy explicit-route exhaustion accounting. Azure can use
		// spare sends on the last eligible target after other targets reject.
		if !azureRecovery || len(orderedRouteTargets(operation.route, operation, endpoint)) > 0 {
			return routeRetrySwitchTarget
		}
	}
	if !azureRecovery ||
		failure.delivery != requestExplicitlyRejected || !failure.cleanupDone ||
		failure.commitment != downstreamCommitmentNone || !upstreamProgressAllowsTargetSwitch(failure.progress) {
		return decision
	}
	if next := operation.sameTargetRetryDecision(ctx, kind, h.ShuttingDown()); next != routeRetrySameTarget {
		return next
	}
	// A valid provider reset is required. Do not add an unbounded or immediate
	// same-target loop when Azure supplies no usable recovery timing.
	if delay > maxAzureTrafficWait || !retryDelayFitsBudget(ctx, delay) {
		return decision
	}
	if operation.inbound != nil && !retryDelayFitsBudget(operation.inbound, delay) {
		return decision
	}
	if traffic.controller == nil {
		return decision
	}
	traffic.controller.mu.Lock()
	tracked := traffic.controller.cooldowns[traffic.key] != nil
	traffic.controller.mu.Unlock()
	if !tracked {
		return decision
	}
	return routeRetrySameTarget
}
