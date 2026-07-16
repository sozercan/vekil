package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
)

type requestDelivery string

const (
	requestDefinitelyNotDelivered requestDelivery = "definitely_not_delivered"
	requestExplicitlyRejected     requestDelivery = "explicitly_rejected"
	requestDeliveredOrAmbiguous   requestDelivery = "delivered_or_ambiguous"
)

type upstreamSemanticProgress string

const (
	upstreamProgressNone            upstreamSemanticProgress = "none"
	upstreamProgressAllowedPreamble upstreamSemanticProgress = "allowed_preamble"
	upstreamProgressSemanticOutput  upstreamSemanticProgress = "semantic_output"
	upstreamProgressToolActivity    upstreamSemanticProgress = "tool_activity"
	upstreamProgressTerminalSuccess upstreamSemanticProgress = "terminal_success"
	upstreamProgressTerminalFailure upstreamSemanticProgress = "terminal_failure"
	upstreamProgressUnknown         upstreamSemanticProgress = "unknown"
)

type downstreamCommitment string

const (
	downstreamCommitmentNone          downstreamCommitment = "none"
	downstreamCommitmentProtocolFrame downstreamCommitment = "headers_or_protocol_frame"
	downstreamCommitmentSemantic      downstreamCommitment = "semantic_output"
)

type routeAttemptKind string

const (
	routeAttemptNormal                routeAttemptKind = "normal"
	routeAttemptProtocolRecovery      routeAttemptKind = "protocol_recovery"
	routeAttemptFailover              routeAttemptKind = "route_failover"
	routeAttemptCompaction            routeAttemptKind = "compaction"
	routeAttemptCompatibilityFallback routeAttemptKind = "compatibility_fallback"
)

type routeRetryDecision string

const (
	routeRetryAccepted               routeRetryDecision = "accepted"
	routeRetrySwitchTarget           routeRetryDecision = "switch_target"
	routeRetrySuppressedMode         routeRetryDecision = "suppressed_mode"
	routeRetrySuppressedDelivery     routeRetryDecision = "suppressed_delivery"
	routeRetrySuppressedProgress     routeRetryDecision = "suppressed_progress"
	routeRetrySuppressedCommitment   routeRetryDecision = "suppressed_commitment"
	routeRetrySuppressedState        routeRetryDecision = "suppressed_state_binding"
	routeRetrySuppressedAdmission    routeRetryDecision = "suppressed_retry_admission"
	routeRetrySuppressedBudget       routeRetryDecision = "suppressed_budget"
	routeRetrySuppressedLifecycle    routeRetryDecision = "suppressed_lifecycle"
	routeRetrySuppressedNoTarget     routeRetryDecision = "suppressed_no_target"
	routeRetrySuppressedNonretryable routeRetryDecision = "suppressed_nonretryable"
)

type routeAttemptTrace struct {
	Sequence    int
	TargetID    string
	ProviderID  string
	Kind        routeAttemptKind
	StatusCode  int
	Delivery    requestDelivery
	Progress    upstreamSemanticProgress
	Commitment  downstreamCommitment
	Decision    routeRetryDecision
	UpstreamID  string
	CleanupDone bool
}

const maxRouteAttemptTrace = 16

type routeOperation struct {
	mu sync.Mutex

	id      string
	route   *modelRoute
	inbound context.Context

	remainingTargetAttempts int
	remainingUpstreamSends  int
	attemptedTargets        map[string]struct{}
	pinnedTargetID          string
	hardPinned              bool
	sequence                int
	upstreamSends           int
	targetSwitches          int
	commitment              downstreamCommitment
	exhaustionRecorded      bool
	trace                   []routeAttemptTrace
}

type routeOperationContextKey struct{}
type routeAttemptKindContextKey struct{}
type routeUpstreamModelOverrideContextKey struct{}
type routeAttemptStatsSuppressedContextKey struct{}

func newRouteOperation(route *modelRoute, inbound context.Context) *routeOperation {
	if route == nil || route.legacy {
		return nil
	}
	maxTargets := route.policy.maxTargetAttempts
	if maxTargets <= 0 {
		maxTargets = 1
	}
	maxSends := route.policy.maxUpstreamSends
	if maxSends <= 0 {
		maxSends = maxTargets
	}
	if inbound == nil {
		inbound = context.Background()
	}
	return &routeOperation{
		id:                      uuid.NewString(),
		route:                   route,
		inbound:                 inbound,
		remainingTargetAttempts: maxTargets,
		remainingUpstreamSends:  maxSends,
		attemptedTargets:        make(map[string]struct{}, maxTargets),
		commitment:              downstreamCommitmentNone,
		trace:                   make([]routeAttemptTrace, 0, min(maxTargets, maxRouteAttemptTrace)),
	}
}

func suppressRouteAttemptStats(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, routeAttemptStatsSuppressedContextKey{}, true)
}

func routeAttemptStatsSuppressed(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	suppressed, _ := ctx.Value(routeAttemptStatsSuppressedContextKey{}).(bool)
	return suppressed
}

func withRouteOperation(ctx context.Context, operation *routeOperation) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if operation == nil {
		return ctx
	}
	return context.WithValue(ctx, routeOperationContextKey{}, operation)
}

func routeOperationFromContext(ctx context.Context) *routeOperation {
	if ctx == nil {
		return nil
	}
	operation, _ := ctx.Value(routeOperationContextKey{}).(*routeOperation)
	return operation
}

func withRouteAttemptKind(ctx context.Context, kind routeAttemptKind) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, routeAttemptKindContextKey{}, kind)
}

func withRouteUpstreamModelOverride(ctx context.Context, upstreamModel string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return ctx
	}
	return context.WithValue(ctx, routeUpstreamModelOverrideContextKey{}, upstreamModel)
}

func routeUpstreamModelOverrideFromContext(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	value, _ := ctx.Value(routeUpstreamModelOverrideContextKey{}).(string)
	return strings.TrimSpace(value)
}

func routeAttemptKindFromContext(ctx context.Context) routeAttemptKind {
	if ctx == nil {
		return routeAttemptNormal
	}
	kind, _ := ctx.Value(routeAttemptKindContextKey{}).(routeAttemptKind)
	if kind == "" {
		return routeAttemptNormal
	}
	return kind
}

func (o *routeOperation) operationID() string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.id
}

func (o *routeOperation) pinTarget(targetID string) {
	if o == nil || strings.TrimSpace(targetID) == "" {
		return
	}
	o.mu.Lock()
	if o.pinnedTargetID == "" {
		o.pinnedTargetID = targetID
	}
	o.mu.Unlock()
}

func (o *routeOperation) forcePinnedTarget(targetID string) error {
	if o == nil || strings.TrimSpace(targetID) == "" {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.pinnedTargetID != "" && o.pinnedTargetID != targetID {
		return fmt.Errorf("route operation is bound to target %q, not %q", o.pinnedTargetID, targetID)
	}
	o.pinnedTargetID = targetID
	o.hardPinned = true
	return nil
}

func (o *routeOperation) pinnedTarget() string {
	if o == nil {
		return ""
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.pinnedTargetID
}

func (o *routeOperation) allowsAutomaticTargetSwitch(kind routeAttemptKind) bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.hardPinned {
		return false
	}
	switch kind {
	case routeAttemptProtocolRecovery, routeAttemptCompaction, routeAttemptCompatibilityFallback:
		return false
	default:
		return true
	}
}

func (o *routeOperation) reserveTarget(targetID string) (sequence int, switched bool, ok bool) {
	if o == nil {
		return 0, false, true
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, attempted := o.attemptedTargets[targetID]; attempted {
		o.sequence++
		return o.sequence, false, true
	}
	if o.remainingTargetAttempts <= 0 {
		return 0, false, false
	}
	switched = len(o.attemptedTargets) > 0
	o.remainingTargetAttempts--
	o.attemptedTargets[targetID] = struct{}{}
	if switched {
		o.targetSwitches++
	}
	o.sequence++
	return o.sequence, switched, true
}

func (o *routeOperation) reserveSendAtDispatch(ctx context.Context, shuttingDown bool) (bool, routeRetryDecision) {
	if o == nil {
		return true, routeRetryAccepted
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	// This lock-protected check is the dispatch linearization point. Cancellation
	// observed before it closes admission; cancellation after it races the active
	// detached attempt rather than authorizing another attempt.
	if shuttingDown || (ctx != nil && ctx.Err() != nil) {
		return false, routeRetrySuppressedLifecycle
	}
	if o.inbound != nil && o.inbound.Err() != nil {
		return false, routeRetrySuppressedAdmission
	}
	if o.remainingUpstreamSends <= 0 {
		return false, routeRetrySuppressedBudget
	}
	o.remainingUpstreamSends--
	o.upstreamSends++
	return true, routeRetryAccepted
}

func (o *routeOperation) retryAdmissionOpen(ctx context.Context, shuttingDown bool) bool {
	if o == nil {
		return true
	}
	if shuttingDown || (ctx != nil && ctx.Err() != nil) || (o.inbound != nil && o.inbound.Err() != nil) {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.commitment == downstreamCommitmentNone && o.remainingTargetAttempts > 0 && o.remainingUpstreamSends > 0
}

func (o *routeOperation) setCommitment(commitment downstreamCommitment) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if routeCommitmentRank(commitment) > routeCommitmentRank(o.commitment) {
		o.commitment = commitment
	}
}

func routeCommitmentRank(commitment downstreamCommitment) int {
	switch commitment {
	case downstreamCommitmentSemantic:
		return 2
	case downstreamCommitmentProtocolFrame:
		return 1
	default:
		return 0
	}
}

func (o *routeOperation) markExhausted() bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.exhaustionRecorded {
		return false
	}
	o.exhaustionRecorded = true
	return true
}

func (o *routeOperation) appendTrace(trace routeAttemptTrace) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if len(o.trace) >= maxRouteAttemptTrace {
		copy(o.trace, o.trace[1:])
		o.trace[len(o.trace)-1] = trace
		return
	}
	o.trace = append(o.trace, trace)
}

func (o *routeOperation) snapshot() (sends, switches int, trace []routeAttemptTrace) {
	if o == nil {
		return 0, 0, nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.upstreamSends, o.targetSwitches, append([]routeAttemptTrace(nil), o.trace...)
}

func orderedRouteTargets(route *modelRoute, operation *routeOperation, endpoint string) []targetBinding {
	if route == nil {
		return nil
	}
	if operation != nil {
		if pinned := operation.pinnedTarget(); pinned != "" {
			if target, ok := route.targetByID(pinned); ok && target.provider != nil && target.provider.supportsEndpoint(endpoint) {
				return []targetBinding{target}
			}
			return nil
		}
	}

	maxTargets := len(route.targets)
	if route.policy.mode == routeModePrimaryOnly {
		maxTargets = min(maxTargets, 1)
	}
	ordered := make([]targetBinding, 0, maxTargets)
	for i := 0; i < maxTargets; i++ {
		target := route.targets[i]
		if target.provider == nil || !target.provider.supportsEndpoint(endpoint) {
			continue
		}
		if operation != nil {
			operation.mu.Lock()
			_, attempted := operation.attemptedTargets[target.id]
			operation.mu.Unlock()
			if attempted {
				continue
			}
		}
		ordered = append(ordered, target)
	}
	return ordered
}

type routeSendObservation struct {
	wroteHeaders atomic.Bool
	wroteRequest atomic.Bool
}

func (o *routeSendObservation) trace() *httptrace.ClientTrace {
	return &httptrace.ClientTrace{
		WroteHeaders: func() { o.wroteHeaders.Store(true) },
		WroteRequest: func(info httptrace.WroteRequestInfo) {
			if info.Err == nil || o.wroteHeaders.Load() {
				o.wroteRequest.Store(true)
			}
		},
	}
}

func (o *routeSendObservation) deliveryForError() requestDelivery {
	if o == nil || (!o.wroteHeaders.Load() && !o.wroteRequest.Load()) {
		return requestDefinitelyNotDelivered
	}
	return requestDeliveredOrAmbiguous
}

type capturedRouteResponse struct {
	statusCode int
	header     http.Header
	body       []byte
	request    *http.Request
}

func captureRouteResponse(resp *http.Response) (*capturedRouteResponse, bool) {
	if resp == nil {
		return nil, true
	}
	header := resp.Header.Clone()
	for _, name := range []string{
		"X-Codex-Turn-State",
		"X-Request-Id",
		"X-Azure-Request-Id",
		"Openai-Request-Id",
		"Authorization",
		"Proxy-Authorization",
		"Api-Key",
	} {
		header.Del(name)
	}
	captured := &capturedRouteResponse{
		statusCode: resp.StatusCode,
		header:     header,
		request:    resp.Request,
	}
	if resp.Body == nil {
		return captured, true
	}

	type readResult struct {
		body []byte
	}
	resultCh := make(chan readResult, 1)
	go func() {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorDetailMaxBodyBytes+upstreamErrorDetailDrainBytes+1))
		_ = resp.Body.Close()
		resultCh <- readResult{body: data}
	}()

	timer := time.NewTimer(upstreamErrorDetailDrainTimeout)
	defer timer.Stop()
	select {
	case result := <-resultCh:
		if len(result.body) > upstreamErrorDetailMaxBodyBytes {
			captured.body = append([]byte(nil), result.body[:upstreamErrorDetailMaxBodyBytes]...)
		} else {
			captured.body = append([]byte(nil), result.body...)
		}
		return captured, true
	case <-timer.C:
		// Stop retry admission rather than overlap a target whose cleanup could
		// not be confirmed. Closing in a goroutine avoids extending the caller's
		// bound for a pathological response body implementation.
		go func() { _ = resp.Body.Close() }()
		return captured, false
	}
}

func (c *capturedRouteResponse) response() *http.Response {
	if c == nil {
		return nil
	}
	header := c.header.Clone()
	header.Del("Content-Length")
	return &http.Response{
		StatusCode: c.statusCode,
		Status:     fmt.Sprintf("%d %s", c.statusCode, http.StatusText(c.statusCode)),
		Header:     header,
		Body:       io.NopCloser(bytes.NewReader(c.body)),
		Request:    c.request,
	}
}

type routeAttemptFailure struct {
	err        error
	response   *capturedRouteResponse
	delivery   requestDelivery
	progress   upstreamSemanticProgress
	commitment downstreamCommitment
	decision   routeRetryDecision
	statusCode int
}

func (f routeAttemptFailure) precedence() int {
	if f.delivery == requestDeliveredOrAmbiguous {
		return 4
	}
	if f.decision == routeRetrySuppressedNonretryable || f.decision == routeRetrySuppressedState ||
		f.decision == routeRetrySuppressedLifecycle || errors.Is(f.err, context.Canceled) || errors.Is(f.err, context.DeadlineExceeded) {
		return 3
	}
	if f.delivery == requestExplicitlyRejected && f.statusCode != 0 {
		return 2
	}
	return 1
}

type routeExecutionFailureError struct {
	failure routeAttemptFailure
}

func (e *routeExecutionFailureError) Error() string {
	if e == nil || e.failure.err == nil {
		return "route execution failed"
	}
	return e.failure.err.Error()
}

func (e *routeExecutionFailureError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.failure.err
}

func selectFinalRouteFailure(failures []routeAttemptFailure) routeAttemptFailure {
	if len(failures) == 0 {
		return routeAttemptFailure{err: fmt.Errorf("no eligible route target"), decision: routeRetrySuppressedNoTarget}
	}
	selected := failures[0]
	for _, failure := range failures[1:] {
		if failure.precedence() > selected.precedence() {
			selected = failure
		}
	}
	return selected
}

func (h *ProxyHandler) executeExplicitRouteRequest(ctx context.Context, route *modelRoute, endpoint string, body []byte, extraHeaders http.Header, requestedModel string, stream bool) (*http.Response, error) {
	return h.executeExplicitRouteRequestPath(ctx, route, endpoint, endpoint, body, extraHeaders, requestedModel, stream)
}

func (h *ProxyHandler) executeExplicitRouteRequestPath(ctx context.Context, route *modelRoute, endpoint, dispatchPath string, body []byte, extraHeaders http.Header, requestedModel string, stream bool) (*http.Response, error) {
	if route == nil || route.legacy {
		return nil, fmt.Errorf("explicit model route is required")
	}
	operation := routeOperationFromContext(ctx)
	if operation == nil {
		operation = newRouteOperation(route, context.Background())
		ctx = withRouteOperation(ctx, operation)
	}
	if operation.route != route {
		return nil, fmt.Errorf("route operation for %q cannot execute route %q", operation.route.public.id, route.public.id)
	}

	kind := routeAttemptKindFromContext(ctx)
	failures := make([]routeAttemptFailure, 0, route.policy.maxTargetAttempts)
	for {
		targets := orderedRouteTargets(route, operation, endpoint)
		if len(targets) == 0 {
			break
		}
		target := targets[0]
		if override := routeUpstreamModelOverrideFromContext(ctx); override != "" {
			target.upstreamModel = override
		}
		sequence, switchedTarget, ok := operation.reserveTarget(target.id)
		if !ok {
			failures = append(failures, routeAttemptFailure{err: fmt.Errorf("route target-attempt budget exhausted"), decision: routeRetrySuppressedBudget})
			break
		}
		attemptKind := kind
		if switchedTarget {
			if !routeAttemptStatsSuppressed(operation.inbound) {
				h.RecordTargetSwitch(operation.inbound)
			}
			if attemptKind == routeAttemptNormal {
				attemptKind = routeAttemptFailover
			}
		}

		owner := providerModelFromRouteTarget(route, target)
		preparedBody, err := prepareRouteTargetBody(body, requestedModel, endpoint, route, target, owner)
		if err != nil {
			failure := routeAttemptFailure{err: err, delivery: requestDefinitelyNotDelivered, progress: upstreamProgressNone, commitment: downstreamCommitmentNone, decision: routeRetrySuppressedNonretryable}
			failures = append(failures, failure)
			operation.appendTrace(routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, Delivery: failure.delivery, Progress: failure.progress, Commitment: failure.commitment, Decision: failure.decision, CleanupDone: true})
			break
		}

		req, err := h.newProviderJSONRequest(ctx, target.provider, http.MethodPost, dispatchPath, preparedBody, cloneSanitizedRouteHeaders(extraHeaders), "", owner)
		if err != nil {
			failure := routeAttemptFailure{err: err, delivery: requestDefinitelyNotDelivered, progress: upstreamProgressNone, commitment: downstreamCommitmentNone, decision: routeRetrySuppressedNonretryable}
			failures = append(failures, failure)
			operation.appendTrace(routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, Delivery: failure.delivery, Progress: failure.progress, Commitment: failure.commitment, Decision: failure.decision, CleanupDone: true})
			break
		}
		req = req.WithContext(withExplicitRouteResponseInfo(req.Context(), explicitRouteResponseInfo{
			routeID:    route.public.routeID,
			publicID:   route.public.id,
			targetID:   target.id,
			providerID: target.provider.id,
		}))
		req.GetBody = nil

		observation := &routeSendObservation{}
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), observation.trace()))
		if reserved, decision := operation.reserveSendAtDispatch(ctx, h.ShuttingDown()); !reserved {
			message := "route upstream-send budget exhausted"
			if decision == routeRetrySuppressedAdmission {
				message = "client disconnected before upstream dispatch"
			} else if decision == routeRetrySuppressedLifecycle {
				message = "route operation ended before upstream dispatch"
			}
			failure := routeAttemptFailure{err: fmt.Errorf("%s", message), delivery: requestDefinitelyNotDelivered, progress: upstreamProgressNone, commitment: downstreamCommitmentNone, decision: decision}
			failures = append(failures, failure)
			operation.appendTrace(routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, Delivery: failure.delivery, Progress: failure.progress, Commitment: failure.commitment, Decision: failure.decision, CleanupDone: true})
			break
		}
		if !routeAttemptStatsSuppressed(operation.inbound) {
			h.RecordUpstreamAttempt(operation.inbound, RouteAttemptObservation{
				OperationID:  operation.operationID(),
				RouteID:      route.public.routeID,
				TargetID:     target.id,
				ProviderID:   target.provider.id,
				ProviderKind: string(target.provider.kind),
			})
		}
		resp, sendErr := h.singleInferenceSend(req)
		if sendErr == nil && resp != nil {
			normalizeExplicitModelHeaders(resp.Header, route.public.id)
		}
		if sendErr != nil {
			delivery := observation.deliveryForError()
			decision := routeRetrySuppressedDelivery
			if delivery == requestDefinitelyNotDelivered && operation.allowsAutomaticTargetSwitch(kind) && operation.retryAdmissionOpen(ctx, h.ShuttingDown()) && route.policy.mode == routeModePriorityFailover {
				decision = routeRetrySwitchTarget
			}
			failure := routeAttemptFailure{err: sendErr, delivery: delivery, progress: upstreamProgressNone, commitment: downstreamCommitmentNone, decision: decision}
			failures = append(failures, failure)
			operation.appendTrace(routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, Delivery: delivery, Progress: upstreamProgressNone, Commitment: downstreamCommitmentNone, Decision: decision, CleanupDone: true})
			if decision == routeRetrySwitchTarget {
				continue
			}
			break
		}

		if stream && resp.StatusCode == http.StatusOK {
			accepted, streamFailure := h.prepareExplicitResponsesStream(ctx, operation, route, target, resp)
			if streamFailure != nil {
				streamFailure.decision = routeRetrySuppressedProgress
				if streamFailure.delivery == requestExplicitlyRejected && operation.allowsAutomaticTargetSwitch(kind) && operation.retryAdmissionOpen(ctx, h.ShuttingDown()) && route.policy.mode == routeModePriorityFailover {
					streamFailure.decision = routeRetrySwitchTarget
				}
				failures = append(failures, *streamFailure)
				operation.appendTrace(routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, StatusCode: streamFailure.statusCode, Delivery: streamFailure.delivery, Progress: streamFailure.progress, Commitment: streamFailure.commitment, Decision: streamFailure.decision, UpstreamID: responsesUpstreamRequestID(resp.Header), CleanupDone: true})
				if streamFailure.decision == routeRetrySwitchTarget {
					continue
				}
				break
			}
			info, _ := explicitRouteResponseInfoFromResponse(accepted)
			if err := h.bindExplicitResponseHeaders(info, accepted.Header); err != nil {
				_ = accepted.Body.Close()
				failure := routeAttemptFailure{err: err, delivery: requestDeliveredOrAmbiguous, progress: upstreamProgressAllowedPreamble, commitment: downstreamCommitmentNone, decision: routeRetrySuppressedState, statusCode: accepted.StatusCode}
				failures = append(failures, failure)
				operation.appendTrace(routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, StatusCode: accepted.StatusCode, Delivery: failure.delivery, Progress: failure.progress, Commitment: failure.commitment, Decision: failure.decision, UpstreamID: responsesUpstreamRequestID(accepted.Header), CleanupDone: true})
				break
			}
			operation.pinTarget(target.id)
			operation.appendTrace(routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, StatusCode: accepted.StatusCode, Delivery: requestDeliveredOrAmbiguous, Progress: upstreamProgressAllowedPreamble, Commitment: downstreamCommitmentNone, Decision: routeRetryAccepted, UpstreamID: responsesUpstreamRequestID(accepted.Header), CleanupDone: true})
			return accepted, nil
		}

		if routeAdapterMayExplicitlyReject(target, endpoint, resp.StatusCode) {
			captured, cleanupDone := captureRouteResponse(resp)
			if !cleanupDone {
				failure := routeAttemptFailure{err: fmt.Errorf("route attempt cleanup did not complete"), delivery: requestDeliveredOrAmbiguous, progress: upstreamProgressUnknown, commitment: downstreamCommitmentNone, decision: routeRetrySuppressedLifecycle, statusCode: resp.StatusCode}
				failures = append(failures, failure)
				operation.appendTrace(routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, StatusCode: resp.StatusCode, Delivery: failure.delivery, Progress: failure.progress, Commitment: failure.commitment, Decision: failure.decision, UpstreamID: responsesUpstreamRequestID(resp.Header), CleanupDone: false})
				break
			}
			if routeAdapterCertifiesHTTPRejection(target, endpoint, captured) {
				decision := routeRetrySuppressedMode
				if route.policy.mode == routeModePriorityFailover && operation.allowsAutomaticTargetSwitch(kind) && operation.retryAdmissionOpen(ctx, h.ShuttingDown()) {
					decision = routeRetrySwitchTarget
				}
				failure := routeAttemptFailure{response: captured, delivery: requestExplicitlyRejected, progress: upstreamProgressNone, commitment: downstreamCommitmentNone, decision: decision, statusCode: captured.statusCode}
				failures = append(failures, failure)
				operation.appendTrace(routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, StatusCode: captured.statusCode, Delivery: failure.delivery, Progress: failure.progress, Commitment: failure.commitment, Decision: decision, UpstreamID: responsesUpstreamRequestID(captured.header), CleanupDone: true})
				if decision == routeRetrySwitchTarget {
					continue
				}
				break
			}
			// The status was potentially retryable but the adapter could not prove
			// pre-execution rejection. Return it as an ambiguous terminal response.
			resp = captured.response()
		}

		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices && (stream || endpoint != providerEndpointResponses) {
			info, _ := explicitRouteResponseInfoFromResponse(resp)
			if err := h.bindExplicitResponseHeaders(info, resp.Header); err != nil {
				_ = resp.Body.Close()
				failure := routeAttemptFailure{err: err, delivery: requestDeliveredOrAmbiguous, progress: upstreamProgressNone, commitment: downstreamCommitmentNone, decision: routeRetrySuppressedState, statusCode: resp.StatusCode}
				failures = append(failures, failure)
				operation.appendTrace(routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, StatusCode: resp.StatusCode, Delivery: failure.delivery, Progress: failure.progress, Commitment: failure.commitment, Decision: failure.decision, UpstreamID: responsesUpstreamRequestID(resp.Header), CleanupDone: true})
				break
			}
		}
		operation.pinTarget(target.id)
		operation.appendTrace(routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, StatusCode: resp.StatusCode, Delivery: requestDeliveredOrAmbiguous, Progress: upstreamProgressNone, Commitment: downstreamCommitmentNone, Decision: routeRetryAccepted, UpstreamID: responsesUpstreamRequestID(resp.Header), CleanupDone: true})
		return resp, nil
	}

	if operation.markExhausted() && !routeAttemptStatsSuppressed(operation.inbound) {
		h.RecordRouteExhaustion(operation.inbound)
	}
	failure := selectFinalRouteFailure(failures)
	if failure.response != nil {
		return failure.response.response(), nil
	}
	if failure.err != nil {
		return nil, &routeExecutionFailureError{failure: failure}
	}
	return nil, fmt.Errorf("model route %q has no eligible target", route.public.id)
}

func (h *ProxyHandler) singleInferenceSend(req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("upstream request is required")
	}
	client := h.client
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	clone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return clone.Do(req)
}

func cloneSanitizedRouteHeaders(headers http.Header) http.Header {
	cloned := headers.Clone()
	if cloned == nil {
		return nil
	}
	for _, name := range []string{
		"Authorization",
		"Proxy-Authorization",
		"Api-Key",
		"X-Api-Key",
		"ChatGPT-Account-ID",
		"X-OpenAI-Fedramp",
	} {
		cloned.Del(name)
	}
	return cloned
}

func prepareRouteTargetBody(body []byte, requestedModel, endpoint string, route *modelRoute, target targetBinding, owner providerModel) ([]byte, error) {
	requestedModel = strings.TrimSpace(requestedModel)
	if requestedModel == "" && route != nil {
		requestedModel = route.public.id
	}
	prepared := append([]byte(nil), body...)
	if !providerUsesAzureClassicDeploymentPath(target.provider, endpoint) {
		rewritten, _, err := rewriteRequestModelForProviderFromModel(prepared, requestedModel, target.upstreamModel)
		if err != nil {
			return nil, &providerRequestError{statusCode: http.StatusBadRequest, err: err}
		}
		prepared = rewritten
	}
	prepared = applyProviderModelRequestPolicy(prepared, endpoint, owner)
	return prepared, nil
}

func routeAdapterMayExplicitlyReject(target targetBinding, endpoint string, statusCode int) bool {
	if target.provider == nil {
		return false
	}
	if statusCode == http.StatusTooManyRequests {
		return true
	}
	if statusCode != http.StatusServiceUnavailable && statusCode != 529 {
		return false
	}
	switch target.provider.kind {
	case providerTypeAzureOpenAI, providerTypeOpenAICompatible:
		return endpoint == providerEndpointResponses || endpoint == providerEndpointChatCompletions
	case providerTypeAnthropicCompatible:
		return endpoint == providerEndpointMessages
	default:
		return false
	}
}

func routeAdapterCertifiesHTTPRejection(target targetBinding, endpoint string, response *capturedRouteResponse) bool {
	if response == nil || target.provider == nil {
		return false
	}
	if response.statusCode == http.StatusTooManyRequests {
		return true
	}
	if response.statusCode != http.StatusServiceUnavailable && response.statusCode != 529 {
		return false
	}
	supported := false
	switch target.provider.kind {
	case providerTypeAzureOpenAI, providerTypeOpenAICompatible:
		supported = endpoint == providerEndpointResponses || endpoint == providerEndpointChatCompletions
	case providerTypeAnthropicCompatible:
		supported = endpoint == providerEndpointMessages
	}
	if !supported {
		return false
	}
	var envelope struct {
		Error struct {
			Code string `json:"code"`
			Type string `json:"type"`
		} `json:"error"`
		Code string `json:"code"`
		Type string `json:"type"`
	}
	if json.Unmarshal(response.body, &envelope) != nil {
		return false
	}
	code := strings.ToLower(strings.TrimSpace(envelope.Error.Code))
	if code == "" {
		code = strings.ToLower(strings.TrimSpace(envelope.Code))
	}
	errType := strings.ToLower(strings.TrimSpace(envelope.Error.Type))
	if errType == "" {
		errType = strings.ToLower(strings.TrimSpace(envelope.Type))
	}
	switch code {
	case "model_overloaded", "engine_overloaded", "server_overloaded", "overloaded_error":
		return true
	}
	return errType == "overloaded_error"
}

func routeAdapterCertifiesStreamFailure(target targetBinding, event responsesWebSocketStreamEvent) (int, bool) {
	if target.provider == nil {
		return 0, false
	}
	code := strings.ToLower(strings.TrimSpace(event.Response.Error.Code))
	errType := strings.ToLower(strings.TrimSpace(event.Response.Error.Type))
	switch code {
	case "too_many_requests", "rate_limit_exceeded":
		return http.StatusTooManyRequests, true
	case "model_overloaded", "engine_overloaded", "server_overloaded":
		if target.provider.kind == providerTypeAzureOpenAI || target.provider.kind == providerTypeOpenAICompatible {
			return http.StatusServiceUnavailable, true
		}
	case "overloaded_error":
		if target.provider.kind == providerTypeAnthropicCompatible {
			return 529, true
		}
	}
	if errType == "rate_limit_error" {
		return http.StatusTooManyRequests, true
	}
	if errType == "overloaded_error" && target.provider.kind == providerTypeAnthropicCompatible {
		return 529, true
	}
	return 0, false
}

func (h *ProxyHandler) prepareExplicitResponsesStream(ctx context.Context, operation *routeOperation, route *modelRoute, target targetBinding, resp *http.Response) (*http.Response, *routeAttemptFailure) {
	prepared := newResponsesPreparedStreamWithPolicy(resp, responsesPrecommitMaxPeekBytes, true, true)
	result, hasResult, _, err := prepared.await(operation.inbound, ctx, responsesPrecommitPeekTimeout)
	if err != nil {
		prepared.abort()
		delivery := requestDeliveredOrAmbiguous
		if !hasResult {
			return nil, &routeAttemptFailure{err: err, delivery: delivery, progress: upstreamProgressUnknown, commitment: downstreamCommitmentNone, statusCode: resp.StatusCode}
		}
	}
	if hasResult && result.failure != nil {
		if status, safe := routeAdapterCertifiesStreamFailure(target, *result.failure); safe && result.precommitReplaySafe {
			if !prepared.abortAndWait(upstreamErrorDetailDrainTimeout) {
				return nil, &routeAttemptFailure{
					err:        fmt.Errorf("responses attempt cleanup did not complete before failover"),
					delivery:   requestDeliveredOrAmbiguous,
					progress:   upstreamProgressUnknown,
					commitment: downstreamCommitmentNone,
					statusCode: status,
				}
			}
			body, _ := json.Marshal(map[string]any{"error": result.failure.Response.Error})
			return nil, &routeAttemptFailure{
				err:        &upstreamError{statusCode: status, body: body, retryAfter: result.retryAfter, headers: resp.Header.Clone()},
				delivery:   requestExplicitlyRejected,
				progress:   upstreamProgressAllowedPreamble,
				commitment: downstreamCommitmentNone,
				statusCode: status,
			}
		}
	}
	if hasResult && result.decision == responsesPeekDecisionTranslate && result.failure != nil {
		prepared.abort()
		body, _ := json.Marshal(map[string]any{"error": result.failure.Response.Error})
		return nil, &routeAttemptFailure{
			err:        &upstreamError{statusCode: result.status, body: body, retryAfter: result.retryAfter, headers: resp.Header.Clone()},
			delivery:   requestDeliveredOrAmbiguous,
			progress:   upstreamProgressTerminalFailure,
			commitment: downstreamCommitmentNone,
			statusCode: result.status,
		}
	}
	prepared.stopTerminalObservation()
	return prepared.commitResponse(), nil
}

// newResponsesPreparedStreamWithPolicy is the route-aware variant. Legacy
// callers retain their current header-sensitive preamble behavior; explicit
// priority routes always hold response.created/response.in_progress until a
// semantic event, terminal event, timeout, or byte bound makes the target
// irrevocable.
func newResponsesPreparedStreamWithPolicy(resp *http.Response, maxPeekBytes int, observeTerminal, holdPreamble bool) *responsesPreparedStream {
	return newResponsesPreparedStreamConfigured(resp, maxPeekBytes, observeTerminal, holdPreamble)
}

func (s *responsesPreparedStream) abortAndWait(timeout time.Duration) bool {
	if s == nil {
		return true
	}
	s.abort()
	if s.doneCh == nil {
		return true
	}
	if timeout <= 0 {
		<-s.doneCh
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-s.doneCh:
		return true
	case <-timer.C:
		return false
	}
}

func isExplicitRoutePreparedResponse(resp *http.Response) bool {
	if resp == nil {
		return false
	}
	_, ok := resp.Body.(*responsesPreparedBody)
	return ok
}

func routeErrorStatus(err error) int {
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) {
		return upstreamErr.statusCode
	}
	var providerErr *providerRequestError
	if errors.As(err, &providerErr) {
		return providerErr.statusCode
	}
	return 0
}

func (h *ProxyHandler) withExplicitRouteOperation(ctx, inbound context.Context, model, endpoint string) (context.Context, *routeOperation, *modelRoute, error) {
	route, known := h.resolveModelRouteForRequest(model, endpoint)
	if !known || route == nil || route.legacy {
		return ctx, nil, route, nil
	}
	if !route.supportsEndpoint(endpoint) {
		return ctx, nil, route, &providerRequestError{
			statusCode: http.StatusBadRequest,
			err:        fmt.Errorf("model %q does not support %s", model, endpoint),
		}
	}
	if existing := routeOperationFromContext(ctx); existing != nil {
		if existing.route != route {
			return ctx, nil, route, fmt.Errorf("route operation for %q cannot execute route %q", existing.route.public.id, route.public.id)
		}
		return ctx, existing, route, nil
	}
	operation := newRouteOperation(route, inbound)
	if summary := RequestSummaryFromContext(inbound); summary != nil {
		summary.SetOperationID(operation.operationID())
		summary.SetRouteID(route.public.routeID)
	}
	return withRouteOperation(ctx, operation), operation, route, nil
}

func forwardExplicitPreparedResponses(h *ProxyHandler, w http.ResponseWriter, r *http.Request, resp *http.Response, upstreamCtx context.Context, upstreamCancel context.CancelFunc, toolScope string) {
	if upstreamCancel != nil {
		defer upstreamCancel()
	}
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.Header().Del("Content-Length")
	setSSEHeaders(w)
	w.WriteHeader(http.StatusOK)
	if operation := routeOperationFromContext(upstreamCtx); operation != nil {
		operation.setCommitment(downstreamCommitmentProtocolFrame)
	}
	var store *ToolExecutionContextStore
	if h != nil {
		store = h.toolContexts
	}
	streamBody := resp.Body
	if info, ok := explicitRouteResponseInfoFromResponse(resp); ok {
		streamBody = normalizeResponsesStreamBodyWithBinding(h, streamBody, info)
	}
	body := newLifecycleAwareReadCloser(streamBody, upstreamCtx)
	streamResponsesPipeWithFailureLog(r.Context(), h, w, body, resp.Header, store, toolScope, h.lifecycleStreamHooks(r.Context(), body.canceledAtFailure))
}

func closeResponseBodyWithTimeout(body io.ReadCloser, timeout time.Duration) bool {
	if body == nil {
		return true
	}
	done := make(chan struct{})
	go func() {
		_ = body.Close()
		close(done)
	}()
	if timeout <= 0 {
		<-done
		return true
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-done:
		return true
	case <-timer.C:
		return false
	}
}
