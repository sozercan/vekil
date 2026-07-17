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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/sozercan/vekil/models"
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

type routeAttemptOutcome string

const (
	routeAttemptOutcomeInFlight       routeAttemptOutcome = "in_flight"
	routeAttemptOutcomeSucceeded      routeAttemptOutcome = "succeeded"
	routeAttemptOutcomeRejected       routeAttemptOutcome = "rejected"
	routeAttemptOutcomeFailed         routeAttemptOutcome = "failed"
	routeAttemptOutcomeTransportError routeAttemptOutcome = "transport_error"
	routeAttemptOutcomeCanceled       routeAttemptOutcome = "canceled"
	routeAttemptOutcomeIncomplete     routeAttemptOutcome = "incomplete"
)

func routeAttemptOutcomeRank(outcome routeAttemptOutcome) int {
	switch outcome {
	case routeAttemptOutcomeCanceled:
		return 6
	case routeAttemptOutcomeTransportError:
		return 5
	case routeAttemptOutcomeFailed:
		return 4
	case routeAttemptOutcomeRejected:
		return 3
	case routeAttemptOutcomeIncomplete:
		return 2
	case routeAttemptOutcomeSucceeded:
		return 1
	default:
		return 0
	}
}

func mergeRouteAttemptOutcome(current, next routeAttemptOutcome) routeAttemptOutcome {
	if routeAttemptOutcomeRank(next) > routeAttemptOutcomeRank(current) {
		return next
	}
	if current == "" {
		return next
	}
	return current
}

func routeAttemptOutcomeIsWasted(outcome routeAttemptOutcome) bool {
	switch outcome {
	case routeAttemptOutcomeRejected, routeAttemptOutcomeFailed, routeAttemptOutcomeTransportError,
		routeAttemptOutcomeCanceled, routeAttemptOutcomeIncomplete:
		return true
	default:
		return false
	}
}

func normalizedRouteAttemptStatus(statusCode int, outcome routeAttemptOutcome) string {
	if statusCode > 0 {
		return strconv.Itoa(statusCode)
	}
	if outcome != "" {
		return string(outcome)
	}
	return "unknown"
}

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

type routeOperationAttemptUpdate struct {
	record  *routeAttemptRecord
	trace   routeAttemptTrace
	version uint64
}

// routeOperationMutex publishes trace mutations to the physical-attempt record
// after releasing the operation lock. Several route-surface helpers live outside
// this file and mutate accepted traces directly while holding o.mu; keeping the
// notification at the lock boundary makes those late reclassifications part of
// the operation lifecycle without coupling them to stats polling.
type routeOperationMutex struct {
	sync.Mutex
	owner atomic.Pointer[routeOperation]
}

func (m *routeOperationMutex) bind(owner *routeOperation) {
	if m == nil || owner == nil {
		return
	}
	if current := m.owner.Load(); current == owner {
		return
	}
	_ = m.owner.CompareAndSwap(nil, owner)
}

func (m *routeOperationMutex) Unlock() {
	if m == nil {
		return
	}
	owner := m.owner.Load()
	var updates []routeOperationAttemptUpdate
	if owner != nil {
		updates = owner.attemptUpdatesLocked()
	}
	m.Mutex.Unlock()
	for _, update := range updates {
		update.record.reconcileTrace(update.trace, update.version)
	}
}

type routeOperation struct {
	mu routeOperationMutex

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
	attemptRecords          map[int]*routeAttemptRecord
	attemptPublished        map[int]routeAttemptTrace
	attemptUpdateVersion    uint64
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
	operation := &routeOperation{
		id:                      uuid.NewString(),
		route:                   route,
		inbound:                 inbound,
		remainingTargetAttempts: maxTargets,
		remainingUpstreamSends:  maxSends,
		attemptedTargets:        make(map[string]struct{}, maxTargets),
		commitment:              downstreamCommitmentNone,
		trace:                   make([]routeAttemptTrace, 0, min(maxTargets, maxRouteAttemptTrace)),
	}
	operation.mu.bind(operation)
	return operation
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

func (o *routeOperation) registerAttemptRecord(sequence int, record *routeAttemptRecord) {
	if o == nil || sequence <= 0 || record == nil {
		return
	}
	o.mu.bind(o)
	o.mu.Lock()
	if o.attemptRecords == nil {
		o.attemptRecords = make(map[int]*routeAttemptRecord)
	}
	o.attemptRecords[sequence] = record
	delete(o.attemptPublished, sequence)
	o.mu.Unlock()
}

// attemptUpdatesLocked returns the newest retained trace for every registered
// physical attempt. The caller holds o.mu; the returned records are reconciled
// only after the lock is released by routeOperationMutex.Unlock.
func (o *routeOperation) attemptUpdatesLocked() []routeOperationAttemptUpdate {
	if o == nil || len(o.attemptRecords) == 0 || len(o.trace) == 0 {
		return nil
	}
	if o.attemptPublished == nil {
		o.attemptPublished = make(map[int]routeAttemptTrace, len(o.attemptRecords))
	}
	updates := make([]routeOperationAttemptUpdate, 0, len(o.attemptRecords))
	for sequence, record := range o.attemptRecords {
		if record == nil {
			continue
		}
		for i := len(o.trace) - 1; i >= 0; i-- {
			trace := o.trace[i]
			if trace.Sequence != sequence {
				continue
			}
			if published, ok := o.attemptPublished[sequence]; ok && published == trace {
				break
			}
			o.attemptPublished[sequence] = trace
			updates = append(updates, routeOperationAttemptUpdate{record: record, trace: trace})
			break
		}
	}
	if len(updates) == 0 {
		return nil
	}
	if o.attemptUpdateVersion == ^uint64(0) {
		panic("route operation attempt update sequence exhausted")
	}
	o.attemptUpdateVersion++
	for i := range updates {
		updates[i].version = o.attemptUpdateVersion
	}
	return updates
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

func (o *routeOperation) exhaustionDecision(endpoint string) (routeRetryDecision, bool) {
	if o == nil || o.route == nil {
		return "", false
	}

	o.mu.Lock()
	defer o.mu.Unlock()

	if o.remainingTargetAttempts <= 0 || o.remainingUpstreamSends <= 0 {
		return routeRetrySuppressedBudget, true
	}
	if o.pinnedTargetID != "" {
		target, ok := o.route.targetByID(o.pinnedTargetID)
		if !ok || target.provider == nil || !target.provider.supportsEndpoint(endpoint) {
			return routeRetrySuppressedNoTarget, true
		}
		return "", false
	}

	maxTargets := len(o.route.targets)
	if o.route.policy.mode == routeModePrimaryOnly {
		maxTargets = min(maxTargets, 1)
	}
	for i := 0; i < maxTargets; i++ {
		target := o.route.targets[i]
		if target.provider == nil || !target.provider.supportsEndpoint(endpoint) {
			continue
		}
		if _, attempted := o.attemptedTargets[target.id]; !attempted {
			return "", false
		}
	}
	return routeRetrySuppressedNoTarget, true
}

func (h *ProxyHandler) recordExplicitRouteExhaustion(operation *routeOperation, endpoint string) {
	if operation == nil || routeAttemptStatsSuppressed(operation.inbound) {
		return
	}
	decision, exhausted := operation.exhaustionDecision(endpoint)
	if !exhausted {
		return
	}
	h.recordManualRouteExhaustion(operation, decision)
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

func (o *routeOperation) commitmentSnapshot() downstreamCommitment {
	if o == nil {
		return downstreamCommitmentNone
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.commitment
}

func routeAttemptOutcomeFromTrace(trace routeAttemptTrace) routeAttemptOutcome {
	if trace.StatusCode >= http.StatusOK && trace.StatusCode < http.StatusBadRequest &&
		trace.Decision == routeRetryAccepted && trace.Progress != upstreamProgressTerminalFailure {
		if trace.Progress == upstreamProgressTerminalSuccess || trace.CleanupDone {
			return routeAttemptOutcomeSucceeded
		}
		return routeAttemptOutcomeInFlight
	}
	if trace.Delivery == requestExplicitlyRejected {
		return routeAttemptOutcomeRejected
	}
	if trace.Delivery == requestDefinitelyNotDelivered && trace.StatusCode == 0 {
		if trace.Decision == routeRetrySuppressedAdmission || trace.Decision == routeRetrySuppressedLifecycle {
			return routeAttemptOutcomeCanceled
		}
		return routeAttemptOutcomeTransportError
	}
	if trace.Progress == upstreamProgressTerminalFailure || trace.StatusCode >= http.StatusBadRequest {
		return routeAttemptOutcomeFailed
	}
	if trace.Decision == routeRetryAccepted {
		return routeAttemptOutcomeSucceeded
	}
	return routeAttemptOutcomeIncomplete
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
	firstByteNS  atomic.Int64
	startedAt    time.Time
	now          func() time.Time
}

func newRouteSendObservation(startedAt time.Time, now func() time.Time) *routeSendObservation {
	if now == nil {
		now = time.Now
	}
	if startedAt.IsZero() {
		startedAt = now()
	}
	o := &routeSendObservation{startedAt: startedAt, now: now}
	o.firstByteNS.Store(-1)
	return o
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

func (o *routeSendObservation) observeBodyBytes(n int) {
	if o == nil || o.now == nil || n <= 0 {
		return
	}
	elapsed := o.now().Sub(o.startedAt)
	if elapsed < 0 {
		elapsed = 0
	}
	o.firstByteNS.CompareAndSwap(-1, int64(elapsed))
}

func (o *routeSendObservation) ttftMillis() *int64 {
	if o == nil {
		return nil
	}
	ns := o.firstByteNS.Load()
	if ns < 0 {
		return nil
	}
	ms := time.Duration(ns).Milliseconds()
	return &ms
}

func (o *routeSendObservation) deliveryForError() requestDelivery {
	if o == nil || (!o.wroteHeaders.Load() && !o.wroteRequest.Load()) {
		return requestDefinitelyNotDelivered
	}
	return requestDeliveredOrAmbiguous
}

func sanitizeExplicitRouteResponseHeaders(headers http.Header) {
	for name := range headers {
		if strings.EqualFold(name, "X-Vekil-Request-ID") {
			delete(headers, name)
		}
	}
}

type routeAttemptTransportOwner struct {
	cancel     context.CancelFunc
	cancelOnce sync.Once
}

func (o *routeAttemptTransportOwner) cancelRequest() {
	if o == nil || o.cancel == nil {
		return
	}
	o.cancelOnce.Do(o.cancel)
}

type routeAttemptTransportOwnerCarrier interface {
	routeAttemptTransportOwnership() *routeAttemptTransportOwner
}

type routeAttemptCancelableBody interface {
	cancelRouteAttempt()
}

type routeAttemptTransportBody struct {
	inner       io.ReadCloser
	owner       *routeAttemptTransportOwner
	observation *routeSendObservation
	closeOnce   sync.Once
	closeErr    error
}

func (b *routeAttemptTransportBody) Read(p []byte) (int, error) {
	if b == nil || b.inner == nil {
		return 0, io.EOF
	}
	n, err := b.inner.Read(p)
	if n > 0 {
		b.observation.observeBodyBytes(n)
	}
	return n, err
}

func (b *routeAttemptTransportBody) Close() error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		b.owner.cancelRequest()
		if b.inner != nil {
			b.closeErr = b.inner.Close()
		}
	})
	return b.closeErr
}

func (b *routeAttemptTransportBody) cancelRouteAttempt() {
	if b != nil {
		b.owner.cancelRequest()
	}
}

func (b *routeAttemptTransportBody) routeAttemptTransportOwnership() *routeAttemptTransportOwner {
	if b == nil {
		return nil
	}
	return b.owner
}

func (b *routeAttemptTransportBody) canceledAtFailure() bool {
	if b == nil || b.inner == nil {
		return false
	}
	if observed, ok := b.inner.(interface{ canceledAtFailure() bool }); ok {
		return observed.canceledAtFailure()
	}
	return false
}

type routeAttemptCancellationBody struct {
	inner     io.ReadCloser
	owner     *routeAttemptTransportOwner
	closeOnce sync.Once
	closeErr  error
}

func (b *routeAttemptCancellationBody) Read(p []byte) (int, error) {
	if b == nil || b.inner == nil {
		return 0, io.EOF
	}
	return b.inner.Read(p)
}

func (b *routeAttemptCancellationBody) Close() error {
	if b == nil {
		return nil
	}
	b.closeOnce.Do(func() {
		b.owner.cancelRequest()
		if b.inner != nil {
			b.closeErr = b.inner.Close()
		}
	})
	return b.closeErr
}

func (b *routeAttemptCancellationBody) cancelRouteAttempt() {
	if b != nil {
		b.owner.cancelRequest()
	}
}

func (b *routeAttemptCancellationBody) routeAttemptTransportOwnership() *routeAttemptTransportOwner {
	if b == nil {
		return nil
	}
	return b.owner
}

func (b *routeAttemptCancellationBody) canceledAtFailure() bool {
	if b == nil || b.inner == nil {
		return false
	}
	if observed, ok := b.inner.(interface{ canceledAtFailure() bool }); ok {
		return observed.canceledAtFailure()
	}
	return false
}

func routeAttemptTransportOwnership(body io.ReadCloser) *routeAttemptTransportOwner {
	if body == nil {
		return nil
	}
	if carrier, ok := body.(routeAttemptTransportOwnerCarrier); ok {
		return carrier.routeAttemptTransportOwnership()
	}
	return nil
}

func retainRouteAttemptTransportOwnership(body io.ReadCloser, owner *routeAttemptTransportOwner) io.ReadCloser {
	if body == nil || owner == nil {
		return body
	}
	if routeAttemptTransportOwnership(body) == owner {
		return body
	}
	return &routeAttemptCancellationBody{inner: body, owner: owner}
}

func cancelRouteAttemptBody(body io.ReadCloser) bool {
	if body == nil {
		return false
	}
	cancelable, ok := body.(routeAttemptCancelableBody)
	if !ok {
		return false
	}
	cancelable.cancelRouteAttempt()
	return true
}

func (r *lifecycleAwareReadCloser) cancelRouteAttempt() {
	if r != nil {
		cancelRouteAttemptBody(r.ReadCloser)
	}
}

func (r *lifecycleAwareReadCloser) routeAttemptTransportOwnership() *routeAttemptTransportOwner {
	if r == nil {
		return nil
	}
	return routeAttemptTransportOwnership(r.ReadCloser)
}

func (b *normalizedResponsesStreamBody) cancelRouteAttempt() {
	if b != nil {
		cancelRouteAttemptBody(b.source)
	}
}

func (b *normalizedResponsesStreamBody) routeAttemptTransportOwnership() *routeAttemptTransportOwner {
	if b == nil {
		return nil
	}
	return routeAttemptTransportOwnership(b.source)
}

func (b *responsesPreparedBody) cancelRouteAttempt() {
	if b != nil && b.closeFn != nil {
		b.closeFn()
	}
}

func (b *explicitRoutePreparedBody) cancelRouteAttempt() {
	if b != nil && b.abort != nil {
		b.abort()
	}
}

type capturedRouteResponse struct {
	statusCode int
	header     http.Header
	body       []byte
	request    *http.Request
	upstreamID string
}

func captureRouteResponse(resp *http.Response) (*capturedRouteResponse, bool) {
	if resp == nil {
		return nil, true
	}
	// Preserve only a recognized upstream correlation ID before sanitizing the
	// captured response. The proxy-owned X-Vekil-Request-ID is deliberately not
	// eligible, so an upstream cannot spoof the logical operation ID in traces.
	upstreamID := responsesUpstreamRequestID(resp.Header)
	header := resp.Header.Clone()
	sanitizeExplicitRouteResponseHeaders(header)
	for _, name := range []string{
		"X-Codex-Turn-State",
		"X-Request-Id",
		"X-Azure-Request-Id",
		"Openai-Request-Id",
		"Authorization",
		"Proxy-Authorization",
		"Api-Key",
		"X-Api-Key",
	} {
		deleteHeaderCI(header, name)
	}
	captured := &capturedRouteResponse{
		statusCode: resp.StatusCode,
		header:     header,
		request:    resp.Request,
		upstreamID: upstreamID,
	}
	if resp.Body == nil {
		return captured, true
	}

	type readResult struct {
		body []byte
	}
	timer := time.NewTimer(upstreamErrorDetailDrainTimeout)
	defer timer.Stop()
	resultCh := make(chan readResult, 1)
	go func() {
		data, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorDetailMaxBodyBytes+upstreamErrorDetailDrainBytes+1))
		_ = resp.Body.Close()
		resultCh <- readResult{body: data}
	}()

	select {
	case result := <-resultCh:
		if len(result.body) > upstreamErrorDetailMaxBodyBytes {
			captured.body = append([]byte(nil), result.body[:upstreamErrorDetailMaxBodyBytes]...)
		} else {
			captured.body = append([]byte(nil), result.body...)
		}
		return captured, true
	case <-timer.C:
		// The read goroutine remains the sole response-body owner. Canceling the
		// dedicated request context unblocks a conforming net/http response body;
		// that same owner then performs the only Close.
		cancelRouteAttemptBody(resp.Body)
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

func (c *capturedRouteResponse) recordFinalUpstreamID(ctx context.Context) {
	if c == nil || c.upstreamID == "" {
		return
	}
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		summary.setUpstreamRequestID(c.upstreamID)
	}
}

const (
	routeAttemptObservationMaxLine = 256 * 1024
	routeAttemptObservationTail    = 64 * 1024

	routeAttemptObservationTailInitial  = 1024
	routeAttemptObservationJSONKeyMax   = 256
	routeAttemptObservationJSONFieldMax = 16 * 1024
	routeAttemptObservationJSONDepthMax = 128
)

// routeAttemptTailBuffer retains only the most recent diagnostic bytes. It
// grows geometrically up to the fixed bound, then becomes a ring so post-cap
// reads overwrite in place instead of allocating and copying a fresh 64 KiB
// suffix for every chunk.
type routeAttemptTailBuffer struct {
	buf      []byte
	start    int
	overflow bool
}

func (b *routeAttemptTailBuffer) append(p []byte) {
	if b == nil || len(p) == 0 {
		return
	}
	if len(p) >= routeAttemptObservationTail {
		hadData := len(b.buf) > 0
		b.ensureCapacity(routeAttemptObservationTail)
		b.buf = b.buf[:routeAttemptObservationTail]
		copy(b.buf, p[len(p)-routeAttemptObservationTail:])
		b.start = 0
		b.overflow = b.overflow || hadData || len(p) > routeAttemptObservationTail
		return
	}

	if !b.overflow {
		needed := len(b.buf) + len(p)
		if needed <= routeAttemptObservationTail {
			b.ensureCapacity(needed)
			b.buf = append(b.buf, p...)
			return
		}

		b.ensureCapacity(routeAttemptObservationTail)
		fill := routeAttemptObservationTail - len(b.buf)
		b.buf = append(b.buf, p[:fill]...)
		p = p[fill:]
		b.overflow = true
		b.start = 0
	}

	// Once full, appending k bytes drops the oldest k bytes. Writing at start
	// performs both operations without moving the retained suffix.
	written := copy(b.buf[b.start:], p)
	copy(b.buf, p[written:])
	b.start = (b.start + len(p)) % routeAttemptObservationTail
}

func (b *routeAttemptTailBuffer) ensureCapacity(needed int) {
	if b == nil || needed <= cap(b.buf) {
		return
	}
	capacity := cap(b.buf)
	if capacity == 0 {
		capacity = routeAttemptObservationTailInitial
	}
	for capacity < needed && capacity < routeAttemptObservationTail {
		capacity *= 2
	}
	if capacity > routeAttemptObservationTail {
		capacity = routeAttemptObservationTail
	}
	next := make([]byte, len(b.buf), capacity)
	copy(next, b.buf)
	b.buf = next
}

func (b *routeAttemptTailBuffer) bytes() []byte {
	if b == nil || len(b.buf) == 0 {
		return nil
	}
	if !b.overflow || b.start == 0 {
		return b.buf
	}
	out := make([]byte, len(b.buf))
	n := copy(out, b.buf[b.start:])
	copy(out[n:], b.buf[:b.start])
	return out
}

func (b *routeAttemptTailBuffer) overflowed() bool {
	return b != nil && b.overflow
}

type routeAttemptJSONState uint8

const (
	routeAttemptJSONBeforeRoot routeAttemptJSONState = iota
	routeAttemptJSONExpectKeyOrEnd
	routeAttemptJSONExpectKey
	routeAttemptJSONReadKey
	routeAttemptJSONExpectColon
	routeAttemptJSONExpectValue
	routeAttemptJSONReadString
	routeAttemptJSONReadComposite
	routeAttemptJSONReadScalar
	routeAttemptJSONAfterValue
	routeAttemptJSONDone
	routeAttemptJSONInvalid
)

type routeAttemptJSONContainerState uint8

const (
	routeAttemptJSONObjectKeyOrEnd routeAttemptJSONContainerState = iota
	routeAttemptJSONObjectKey
	routeAttemptJSONObjectColon
	routeAttemptJSONObjectValue
	routeAttemptJSONArrayValueOrEnd
	routeAttemptJSONArrayValue
	routeAttemptJSONContainerAfterValue
)

type routeAttemptJSONContainer struct {
	kind  byte
	state routeAttemptJSONContainerState
}

type routeAttemptJSONLexState uint8

const (
	routeAttemptJSONLexNone routeAttemptJSONLexState = iota
	routeAttemptJSONLexString
	routeAttemptJSONLexEscape
	routeAttemptJSONLexUnicode
	routeAttemptJSONLexNumber
	routeAttemptJSONLexLiteral
)

type routeAttemptJSONNumberState uint8

const (
	routeAttemptJSONNumberMinus routeAttemptJSONNumberState = iota
	routeAttemptJSONNumberZero
	routeAttemptJSONNumberInteger
	routeAttemptJSONNumberDecimalPoint
	routeAttemptJSONNumberFraction
	routeAttemptJSONNumberExponent
	routeAttemptJSONNumberExponentSign
	routeAttemptJSONNumberExponentDigits
)

// routeAttemptJSONValidator validates one JSON object without retaining its
// values. The envelope extractor intentionally skips large unselected fields;
// this parallel parser prevents balanced-but-malformed composites or scalars
// from being mistaken for a valid completed response.
type routeAttemptJSONValidator struct {
	stack [routeAttemptObservationJSONDepthMax]routeAttemptJSONContainer
	depth int

	started bool
	done    bool
	invalid bool

	lex              routeAttemptJSONLexState
	stringIsKey      bool
	unicodeRemaining int
	number           routeAttemptJSONNumberState
	literal          string
	literalIndex     int
}

func (v *routeAttemptJSONValidator) observe(p []byte) {
	if v == nil || len(p) == 0 || v.invalid {
		return
	}
	for _, c := range p {
		v.observeByte(c)
		if v.invalid {
			return
		}
	}
}

func (v *routeAttemptJSONValidator) observeByte(c byte) {
	if v == nil || v.invalid {
		return
	}
	if v.done {
		if !isRouteAttemptJSONSpace(c) {
			v.invalid = true
		}
		return
	}

	switch v.lex {
	case routeAttemptJSONLexString:
		v.observeStringByte(c)
		return
	case routeAttemptJSONLexEscape:
		v.observeEscapeByte(c)
		return
	case routeAttemptJSONLexUnicode:
		if !isRouteAttemptJSONHex(c) {
			v.invalid = true
			return
		}
		v.unicodeRemaining--
		if v.unicodeRemaining == 0 {
			v.lex = routeAttemptJSONLexString
		}
		return
	case routeAttemptJSONLexNumber:
		if v.observeNumberByte(c) {
			return
		}
		if v.invalid {
			return
		}
		v.lex = routeAttemptJSONLexNone
		v.valueComplete()
		v.observeByte(c)
		return
	case routeAttemptJSONLexLiteral:
		if v.literalIndex >= len(v.literal) || c != v.literal[v.literalIndex] {
			v.invalid = true
			return
		}
		v.literalIndex++
		if v.literalIndex == len(v.literal) {
			v.lex = routeAttemptJSONLexNone
			v.valueComplete()
		}
		return
	}

	if !v.started {
		if isRouteAttemptJSONSpace(c) {
			return
		}
		if c != '{' {
			v.invalid = true
			return
		}
		v.started = true
		v.pushContainer(c)
		return
	}
	if v.depth == 0 {
		v.invalid = true
		return
	}

	frame := &v.stack[v.depth-1]
	switch frame.state {
	case routeAttemptJSONObjectKeyOrEnd:
		if isRouteAttemptJSONSpace(c) {
			return
		}
		if c == '}' {
			v.closeContainer(c)
			return
		}
		if c != '"' {
			v.invalid = true
			return
		}
		v.startString(true)
	case routeAttemptJSONObjectKey:
		if isRouteAttemptJSONSpace(c) {
			return
		}
		if c != '"' {
			v.invalid = true
			return
		}
		v.startString(true)
	case routeAttemptJSONObjectColon:
		if isRouteAttemptJSONSpace(c) {
			return
		}
		if c != ':' {
			v.invalid = true
			return
		}
		frame.state = routeAttemptJSONObjectValue
	case routeAttemptJSONObjectValue, routeAttemptJSONArrayValue:
		if isRouteAttemptJSONSpace(c) {
			return
		}
		v.startValue(c)
	case routeAttemptJSONArrayValueOrEnd:
		if isRouteAttemptJSONSpace(c) {
			return
		}
		if c == ']' {
			v.closeContainer(c)
			return
		}
		v.startValue(c)
	case routeAttemptJSONContainerAfterValue:
		if isRouteAttemptJSONSpace(c) {
			return
		}
		if c == ',' {
			if frame.kind == '{' {
				frame.state = routeAttemptJSONObjectKey
			} else {
				frame.state = routeAttemptJSONArrayValue
			}
			return
		}
		if (frame.kind == '{' && c == '}') || (frame.kind == '[' && c == ']') {
			v.closeContainer(c)
			return
		}
		v.invalid = true
	default:
		v.invalid = true
	}
}

func (v *routeAttemptJSONValidator) startString(key bool) {
	v.lex = routeAttemptJSONLexString
	v.stringIsKey = key
	v.unicodeRemaining = 0
}

func (v *routeAttemptJSONValidator) observeStringByte(c byte) {
	switch {
	case c < 0x20:
		v.invalid = true
	case c == '\\':
		v.lex = routeAttemptJSONLexEscape
	case c == '"':
		v.lex = routeAttemptJSONLexNone
		if v.stringIsKey {
			if v.depth == 0 || v.stack[v.depth-1].kind != '{' {
				v.invalid = true
				return
			}
			v.stack[v.depth-1].state = routeAttemptJSONObjectColon
			return
		}
		v.valueComplete()
	}
}

func (v *routeAttemptJSONValidator) observeEscapeByte(c byte) {
	switch c {
	case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
		v.lex = routeAttemptJSONLexString
	case 'u':
		v.lex = routeAttemptJSONLexUnicode
		v.unicodeRemaining = 4
	default:
		v.invalid = true
	}
}

func (v *routeAttemptJSONValidator) startValue(c byte) {
	switch c {
	case '{', '[':
		v.pushContainer(c)
	case '"':
		v.startString(false)
	case 't':
		v.startLiteral("true")
	case 'f':
		v.startLiteral("false")
	case 'n':
		v.startLiteral("null")
	case '-':
		v.lex = routeAttemptJSONLexNumber
		v.number = routeAttemptJSONNumberMinus
	case '0':
		v.lex = routeAttemptJSONLexNumber
		v.number = routeAttemptJSONNumberZero
	default:
		if c >= '1' && c <= '9' {
			v.lex = routeAttemptJSONLexNumber
			v.number = routeAttemptJSONNumberInteger
			return
		}
		v.invalid = true
	}
}

func (v *routeAttemptJSONValidator) startLiteral(literal string) {
	v.lex = routeAttemptJSONLexLiteral
	v.literal = literal
	v.literalIndex = 1
}

// observeNumberByte returns true when c was consumed as part of the number.
// A false return with a valid terminal number asks the caller to finish the
// value and reprocess c as structural JSON.
func (v *routeAttemptJSONValidator) observeNumberByte(c byte) bool {
	switch v.number {
	case routeAttemptJSONNumberMinus:
		switch {
		case c == '0':
			v.number = routeAttemptJSONNumberZero
			return true
		case c >= '1' && c <= '9':
			v.number = routeAttemptJSONNumberInteger
			return true
		default:
			v.invalid = true
			return false
		}
	case routeAttemptJSONNumberZero:
		switch c {
		case '.':
			v.number = routeAttemptJSONNumberDecimalPoint
			return true
		case 'e', 'E':
			v.number = routeAttemptJSONNumberExponent
			return true
		default:
			if isRouteAttemptJSONValueDelimiter(c) {
				return false
			}
			v.invalid = true
			return false
		}
	case routeAttemptJSONNumberInteger:
		switch {
		case c >= '0' && c <= '9':
			return true
		case c == '.':
			v.number = routeAttemptJSONNumberDecimalPoint
			return true
		case c == 'e' || c == 'E':
			v.number = routeAttemptJSONNumberExponent
			return true
		case isRouteAttemptJSONValueDelimiter(c):
			return false
		default:
			v.invalid = true
			return false
		}
	case routeAttemptJSONNumberDecimalPoint:
		if c >= '0' && c <= '9' {
			v.number = routeAttemptJSONNumberFraction
			return true
		}
		v.invalid = true
		return false
	case routeAttemptJSONNumberFraction:
		switch {
		case c >= '0' && c <= '9':
			return true
		case c == 'e' || c == 'E':
			v.number = routeAttemptJSONNumberExponent
			return true
		case isRouteAttemptJSONValueDelimiter(c):
			return false
		default:
			v.invalid = true
			return false
		}
	case routeAttemptJSONNumberExponent:
		switch {
		case c == '+' || c == '-':
			v.number = routeAttemptJSONNumberExponentSign
			return true
		case c >= '0' && c <= '9':
			v.number = routeAttemptJSONNumberExponentDigits
			return true
		default:
			v.invalid = true
			return false
		}
	case routeAttemptJSONNumberExponentSign:
		if c >= '0' && c <= '9' {
			v.number = routeAttemptJSONNumberExponentDigits
			return true
		}
		v.invalid = true
		return false
	case routeAttemptJSONNumberExponentDigits:
		if c >= '0' && c <= '9' {
			return true
		}
		if isRouteAttemptJSONValueDelimiter(c) {
			return false
		}
		v.invalid = true
		return false
	default:
		v.invalid = true
		return false
	}
}

func (v *routeAttemptJSONValidator) pushContainer(kind byte) {
	if v.depth >= len(v.stack) {
		v.invalid = true
		return
	}
	state := routeAttemptJSONObjectKeyOrEnd
	if kind == '[' {
		state = routeAttemptJSONArrayValueOrEnd
	} else if kind != '{' {
		v.invalid = true
		return
	}
	v.stack[v.depth] = routeAttemptJSONContainer{kind: kind, state: state}
	v.depth++
}

func (v *routeAttemptJSONValidator) closeContainer(close byte) {
	if v.depth == 0 {
		v.invalid = true
		return
	}
	frame := v.stack[v.depth-1]
	if !routeAttemptJSONDelimitersMatch(frame.kind, close) {
		v.invalid = true
		return
	}
	v.depth--
	if v.depth == 0 {
		v.done = true
		return
	}
	v.valueComplete()
}

func (v *routeAttemptJSONValidator) valueComplete() {
	if v.depth == 0 {
		v.invalid = true
		return
	}
	frame := &v.stack[v.depth-1]
	switch frame.state {
	case routeAttemptJSONObjectValue, routeAttemptJSONArrayValue, routeAttemptJSONArrayValueOrEnd:
		frame.state = routeAttemptJSONContainerAfterValue
	default:
		v.invalid = true
	}
}

func (v *routeAttemptJSONValidator) complete() bool {
	return v != nil && v.started && v.done && !v.invalid && v.lex == routeAttemptJSONLexNone && v.depth == 0
}

func (v *routeAttemptJSONValidator) failed() bool {
	return v != nil && v.invalid
}

func isRouteAttemptJSONHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isRouteAttemptJSONValueDelimiter(c byte) bool {
	return isRouteAttemptJSONSpace(c) || c == ',' || c == '}' || c == ']'
}

type routeAttemptJSONField struct {
	name     string
	maxBytes int
	raw      []byte
	present  bool
	complete bool
	overflow bool
}

// routeAttemptEnvelopeExtractor incrementally captures only selected top-level
// JSON fields. Large output/content values are scanned but never retained, so a
// complete non-streaming envelope can be classified without buffering its body.
type routeAttemptEnvelopeExtractor struct {
	state     routeAttemptJSONState
	validator routeAttemptJSONValidator

	fields       []routeAttemptJSONField
	currentField int
	currentKey   string
	keyRaw       []byte
	keyOverflow  bool
	stack        [routeAttemptObservationJSONDepthMax]byte
	depth        int
	inString     bool
	escaped      bool
}

func newRouteAttemptEnvelopeExtractor(endpoint string) *routeAttemptEnvelopeExtractor {
	var names []string
	switch endpoint {
	case providerEndpointResponses:
		names = []string{"status", "usage"}
	case providerEndpointMessages:
		names = []string{"type", "error", "usage"}
	case providerEndpointChatCompletions:
		names = []string{"usage"}
	default:
		return nil
	}
	fields := make([]routeAttemptJSONField, 0, len(names))
	for _, name := range names {
		maxBytes := routeAttemptObservationJSONFieldMax
		if name == "status" || name == "type" {
			maxBytes = routeAttemptObservationJSONKeyMax
		}
		fields = append(fields, routeAttemptJSONField{name: name, maxBytes: maxBytes})
	}
	return &routeAttemptEnvelopeExtractor{
		state:        routeAttemptJSONBeforeRoot,
		fields:       fields,
		currentField: -1,
	}
}

func (e *routeAttemptEnvelopeExtractor) observe(p []byte) {
	if e == nil || len(p) == 0 {
		return
	}
	e.validator.observe(p)
	if e.state == routeAttemptJSONInvalid {
		return
	}
	for _, c := range p {
		e.observeByte(c)
		if e.state == routeAttemptJSONInvalid {
			return
		}
	}
}

func (e *routeAttemptEnvelopeExtractor) observeByte(c byte) {
	switch e.state {
	case routeAttemptJSONBeforeRoot:
		if isRouteAttemptJSONSpace(c) {
			return
		}
		if c != '{' {
			e.state = routeAttemptJSONInvalid
			return
		}
		e.stack[0] = c
		e.depth = 1
		e.state = routeAttemptJSONExpectKeyOrEnd
	case routeAttemptJSONExpectKeyOrEnd:
		if isRouteAttemptJSONSpace(c) {
			return
		}
		if c == '}' {
			e.depth = 0
			e.state = routeAttemptJSONDone
			return
		}
		e.startKey(c)
	case routeAttemptJSONExpectKey:
		if isRouteAttemptJSONSpace(c) {
			return
		}
		e.startKey(c)
	case routeAttemptJSONReadKey:
		e.readKeyByte(c)
	case routeAttemptJSONExpectColon:
		if isRouteAttemptJSONSpace(c) {
			return
		}
		if c != ':' {
			e.state = routeAttemptJSONInvalid
			return
		}
		e.state = routeAttemptJSONExpectValue
	case routeAttemptJSONExpectValue:
		if isRouteAttemptJSONSpace(c) {
			return
		}
		e.startValue(c)
	case routeAttemptJSONReadString:
		e.captureByte(c)
		if e.escaped {
			e.escaped = false
			return
		}
		if c == '\\' {
			e.escaped = true
			return
		}
		if c == '"' {
			e.inString = false
			e.finishValue()
		}
	case routeAttemptJSONReadComposite:
		e.captureByte(c)
		e.readCompositeByte(c)
	case routeAttemptJSONReadScalar:
		e.readScalarByte(c)
	case routeAttemptJSONAfterValue:
		e.readAfterValueByte(c)
	case routeAttemptJSONDone:
		if !isRouteAttemptJSONSpace(c) {
			e.state = routeAttemptJSONInvalid
		}
	}
}

func (e *routeAttemptEnvelopeExtractor) startKey(c byte) {
	if c != '"' {
		e.state = routeAttemptJSONInvalid
		return
	}
	e.keyRaw = append(e.keyRaw[:0], c)
	e.keyOverflow = false
	e.escaped = false
	e.state = routeAttemptJSONReadKey
}

func (e *routeAttemptEnvelopeExtractor) readKeyByte(c byte) {
	if !e.keyOverflow {
		if len(e.keyRaw) < routeAttemptObservationJSONKeyMax {
			e.keyRaw = append(e.keyRaw, c)
		} else {
			e.keyOverflow = true
		}
	}
	if e.escaped {
		e.escaped = false
		return
	}
	if c == '\\' {
		e.escaped = true
		return
	}
	if c != '"' {
		return
	}
	if e.keyOverflow {
		e.currentKey = ""
		e.state = routeAttemptJSONExpectColon
		return
	}
	if json.Unmarshal(e.keyRaw, &e.currentKey) != nil {
		e.state = routeAttemptJSONInvalid
		return
	}
	e.state = routeAttemptJSONExpectColon
}

func (e *routeAttemptEnvelopeExtractor) startValue(c byte) {
	e.currentField = e.fieldIndex(e.currentKey)
	if e.currentField >= 0 {
		field := &e.fields[e.currentField]
		field.raw = field.raw[:0]
		field.present = true
		field.complete = false
		field.overflow = false
	}
	e.currentKey = ""
	e.captureByte(c)
	switch c {
	case '"':
		e.inString = true
		e.escaped = false
		e.state = routeAttemptJSONReadString
	case '{', '[':
		if !e.push(c) {
			return
		}
		e.inString = false
		e.escaped = false
		e.state = routeAttemptJSONReadComposite
	case '}', ',':
		e.state = routeAttemptJSONInvalid
	default:
		e.state = routeAttemptJSONReadScalar
	}
}

func (e *routeAttemptEnvelopeExtractor) readCompositeByte(c byte) {
	if e.inString {
		if e.escaped {
			e.escaped = false
			return
		}
		if c == '\\' {
			e.escaped = true
			return
		}
		if c == '"' {
			e.inString = false
		}
		return
	}
	if c == '"' {
		e.inString = true
		return
	}
	if c == '{' || c == '[' {
		e.push(c)
		return
	}
	if c != '}' && c != ']' {
		return
	}
	if e.depth <= 1 || !routeAttemptJSONDelimitersMatch(e.stack[e.depth-1], c) {
		e.state = routeAttemptJSONInvalid
		return
	}
	e.depth--
	if e.depth == 1 {
		e.finishValue()
	}
}

func (e *routeAttemptEnvelopeExtractor) readScalarByte(c byte) {
	switch {
	case isRouteAttemptJSONSpace(c):
		e.finishValue()
	case c == ',' || c == '}':
		e.finishValue()
		e.readAfterValueByte(c)
	case c == '{' || c == '[' || c == ']' || c == '"' || c == ':':
		e.state = routeAttemptJSONInvalid
	default:
		e.captureByte(c)
	}
}

func (e *routeAttemptEnvelopeExtractor) readAfterValueByte(c byte) {
	if isRouteAttemptJSONSpace(c) {
		return
	}
	switch c {
	case ',':
		e.state = routeAttemptJSONExpectKey
	case '}':
		if e.depth != 1 || e.stack[0] != '{' {
			e.state = routeAttemptJSONInvalid
			return
		}
		e.depth = 0
		e.state = routeAttemptJSONDone
	default:
		e.state = routeAttemptJSONInvalid
	}
}

func (e *routeAttemptEnvelopeExtractor) finishValue() {
	if e.currentField >= 0 && e.currentField < len(e.fields) {
		e.fields[e.currentField].complete = true
	}
	e.currentField = -1
	e.state = routeAttemptJSONAfterValue
}

func (e *routeAttemptEnvelopeExtractor) captureByte(c byte) {
	if e.currentField < 0 || e.currentField >= len(e.fields) {
		return
	}
	field := &e.fields[e.currentField]
	if field.overflow {
		return
	}
	if len(field.raw) >= field.maxBytes {
		field.raw = field.raw[:0]
		field.overflow = true
		return
	}
	field.raw = append(field.raw, c)
}

func (e *routeAttemptEnvelopeExtractor) push(c byte) bool {
	if e.depth >= len(e.stack) {
		e.state = routeAttemptJSONInvalid
		return false
	}
	e.stack[e.depth] = c
	e.depth++
	return true
}

func (e *routeAttemptEnvelopeExtractor) fieldIndex(name string) int {
	for i := range e.fields {
		if e.fields[i].name == name {
			return i
		}
	}
	return -1
}

func (e *routeAttemptEnvelopeExtractor) field(name string) ([]byte, bool) {
	if e == nil {
		return nil, false
	}
	idx := e.fieldIndex(name)
	if idx < 0 {
		return nil, false
	}
	field := &e.fields[idx]
	if !field.present || !field.complete || field.overflow || len(field.raw) == 0 || !json.Valid(field.raw) {
		return nil, false
	}
	return field.raw, true
}

func (e *routeAttemptEnvelopeExtractor) complete() bool {
	return e != nil && e.state == routeAttemptJSONDone && e.validator.complete()
}

func (e *routeAttemptEnvelopeExtractor) invalid() bool {
	return e != nil && (e.state == routeAttemptJSONInvalid || e.validator.failed())
}

func isRouteAttemptJSONSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func routeAttemptJSONDelimitersMatch(open, close byte) bool {
	return (open == '{' && close == '}') || (open == '[' && close == ']')
}

func statsTokenUsageFromOpenAI(usage *models.OpenAIUsage) (statsTokenUsage, bool) {
	if usage == nil {
		return statsTokenUsage{}, false
	}
	out := statsTokenUsage{
		PromptTokens:     int64(usage.PromptTokens),
		CompletionTokens: int64(usage.CompletionTokens),
		TotalTokens:      int64(usage.TotalTokens),
	}
	if usage.PromptTokensDetails != nil {
		out.CachedTokens = int64(usage.PromptTokensDetails.CachedTokens)
	}
	if usage.CompletionTokensDetails != nil {
		out.ReasoningTokens = int64(usage.CompletionTokensDetails.ReasoningTokens)
	}
	return out.normalized(), true
}

func sanitizedRouteAttemptRetryAfter(headers http.Header, explicit string) *int64 {
	candidate := strings.TrimSpace(explicit)
	if candidate == "" {
		candidate, _ = selectResponsesRetryAfter(headers)
	}
	if candidate == "" {
		return nil
	}
	delay, ok := parseRetryAfter(candidate)
	if !ok || delay <= 0 {
		return nil
	}
	seconds := durationSecondsCeil(delay)
	if seconds <= 0 {
		return nil
	}
	return &seconds
}

func routeAttemptDiagnosticHeaders(headers http.Header) http.Header {
	if len(headers) == 0 {
		return nil
	}
	out := make(http.Header)
	for _, name := range []string{
		"Retry-After", "retry-after-ms",
		"x-ratelimit-remaining-tokens", "x-ratelimit-remaining-requests",
		"x-ratelimit-reset-tokens", "x-ratelimit-reset-requests",
		"x-request-id", "x-azure-request-id", "openai-request-id",
	} {
		if value := strings.TrimSpace(headerGetCI(headers, name)); value != "" {
			out.Set(name, boundStatLabelRunes(value, statOperationalLabelMaxLen))
		}
	}
	return out
}

type routeAttemptResponseObserver struct {
	mu sync.Mutex

	record          *routeAttemptRecord
	operation       *routeOperation
	inboundCtx      context.Context
	attemptCtx      context.Context
	trace           routeAttemptTrace
	send            *routeSendObservation
	endpoint        string
	streaming       bool
	headers         http.Header
	statusCode      int
	outcome         routeAttemptOutcome
	progress        upstreamSemanticProgress
	commitment      downstreamCommitment
	decision        routeRetryDecision
	retryAfter      *int64
	upstreamID      string
	usage           statsTokenUsage
	haveUsage       bool
	terminal        bool
	cleanupTimedOut bool
	line            []byte
	lineOverflow    bool
	linePendingCR   bool
	sse             sseDataAccumulator
	tail            routeAttemptTailBuffer
	envelope        *routeAttemptEnvelopeExtractor
	anthropic       anthropicStreamUsageAccumulator
}

func newRouteAttemptResponseObserver(record *routeAttemptRecord, operation *routeOperation, trace routeAttemptTrace, send *routeSendObservation, endpoint string, streaming bool, headers http.Header) *routeAttemptResponseObserver {
	diagnosticHeaders := routeAttemptDiagnosticHeaders(headers)
	observer := &routeAttemptResponseObserver{
		record:     record,
		operation:  operation,
		trace:      trace,
		send:       send,
		endpoint:   endpoint,
		streaming:  streaming,
		headers:    diagnosticHeaders,
		statusCode: trace.StatusCode,
		progress:   trace.Progress,
		commitment: trace.Commitment,
		decision:   trace.Decision,
		upstreamID: boundOperationalStatLabel(trace.UpstreamID),
		envelope:   newRouteAttemptEnvelopeExtractor(endpoint),
	}
	if operation != nil {
		observer.inboundCtx = operation.inbound
	}
	if observer.statusCode == 0 {
		observer.statusCode = http.StatusOK
	}
	observer.retryAfter = sanitizedRouteAttemptRetryAfter(diagnosticHeaders, "")
	switch {
	case trace.Delivery == requestExplicitlyRejected:
		observer.outcome = routeAttemptOutcomeRejected
	case observer.statusCode >= http.StatusBadRequest:
		observer.outcome = routeAttemptOutcomeFailed
	case streaming || (endpoint == providerEndpointChatCompletions && observer.statusCode >= http.StatusOK && observer.statusCode < http.StatusMultipleChoices):
		observer.outcome = routeAttemptOutcomeInFlight
	default:
		observer.outcome = routeAttemptOutcomeSucceeded
	}
	return observer
}

func (o *routeAttemptResponseObserver) requestContextError() error {
	if o == nil {
		return nil
	}
	if o.attemptCtx != nil {
		if err := o.attemptCtx.Err(); err != nil {
			return err
		}
	}
	if o.inboundCtx != nil {
		return o.inboundCtx.Err()
	}
	return nil
}

func (o *routeAttemptResponseObserver) observe(p []byte) {
	if o == nil || len(p) == 0 {
		return
	}
	o.mu.Lock()
	o.tail.append(p)
	if !o.streaming {
		o.envelope.observe(p)
		o.mu.Unlock()
		return
	}
	publish := false
	for _, b := range p {
		if o.linePendingCR {
			o.linePendingCR = false
			if b == '\n' {
				continue
			}
		}
		switch b {
		case '\r':
			if o.consumeSSELineLocked() {
				publish = true
			}
			o.linePendingCR = true
		case '\n':
			if o.consumeSSELineLocked() {
				publish = true
			}
		default:
			if o.lineOverflow {
				continue
			}
			if len(o.line) >= routeAttemptObservationMaxLine {
				o.line = nil
				o.lineOverflow = true
				continue
			}
			o.line = append(o.line, b)
		}
	}
	var completion routeAttemptCompletion
	if publish {
		completion = o.completionLocked(false)
	}
	o.mu.Unlock()
	if publish {
		o.record.complete(completion)
	}
}

func (o *routeAttemptResponseObserver) consumeSSELineLocked() bool {
	if o == nil {
		return false
	}
	if o.lineOverflow {
		o.lineOverflow = false
		o.line = nil
		if o.observeOverflowedSSEEvent() {
			return true
		}
		o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressUnknown)
		return false
	}
	line := string(append(append([]byte(nil), o.line...), '\n'))
	o.line = o.line[:0]
	publish := false
	if !o.sse.consumeLine(line, func(eventType, data string) bool {
		if o.observeSSEEvent(eventType, data) {
			publish = true
		}
		return true
	}) {
		o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressUnknown)
	}
	return publish
}

func (o *routeAttemptResponseObserver) observeOverflowedSSEEvent() bool {
	if o == nil || o.endpoint != providerEndpointResponses {
		return false
	}
	eventType := strings.TrimSpace(o.sse.eventType)
	if eventType != "response.completed" && eventType != "response.failed" && eventType != "response.incomplete" && eventType != "response.cancelled" && eventType != "response.canceled" && eventType != "error" {
		return false
	}
	event := responsesWebSocketStreamEvent{Type: eventType}
	tail := o.tail.bytes()
	updateResponsesTerminalEvent(&event, eventType, tail)
	if !event.Response.Usage.isZero() {
		o.usage = statsTokenUsageFromResponses(event.Response.Usage)
		o.haveUsage = true
	} else if usage, ok := extractResponsesUsageObject(tail); ok {
		o.usage = statsTokenUsageFromResponses(usage)
		o.haveUsage = true
	}
	switch eventType {
	case "response.completed":
		o.applyStreamingTerminal(routeAttemptOutcomeSucceeded, upstreamProgressTerminalSuccess)
	case "response.cancelled", "response.canceled":
		o.applyStreamingTerminal(routeAttemptOutcomeCanceled, upstreamProgressTerminalFailure)
	default:
		if o.applyStreamingTerminal(routeAttemptOutcomeFailed, upstreamProgressTerminalFailure) {
			o.statusCode = http.StatusBadGateway
		}
	}
	return true
}

func mergeRouteAttemptStreamingProgress(current, next upstreamSemanticProgress) upstreamSemanticProgress {
	if current == upstreamProgressTerminalFailure {
		return current
	}
	if current == upstreamProgressTerminalSuccess && next == upstreamProgressTerminalFailure {
		return next
	}
	return mergeUpstreamSemanticProgress(current, next)
}

func (o *routeAttemptResponseObserver) applyStreamingTerminal(outcome routeAttemptOutcome, progress upstreamSemanticProgress) bool {
	if o == nil {
		return false
	}
	if o.terminal && routeAttemptOutcomeRank(outcome) <= routeAttemptOutcomeRank(o.outcome) {
		o.progress = mergeRouteAttemptStreamingProgress(o.progress, progress)
		return false
	}
	o.outcome = mergeRouteAttemptOutcome(o.outcome, outcome)
	o.progress = mergeRouteAttemptStreamingProgress(o.progress, progress)
	o.terminal = true
	return true
}

func (o *routeAttemptResponseObserver) observeSSEEvent(eventType, data string) bool {
	data = strings.TrimSpace(data)
	switch o.endpoint {
	case providerEndpointResponses:
		return o.observeResponsesEvent(eventType, data)
	case providerEndpointMessages:
		o.anthropic.observe([]byte(data))
		inspection := inspectAnthropicStreamEvent(eventType, data)
		if inspection.failure != nil {
			if !o.applyStreamingTerminal(routeAttemptOutcomeFailed, upstreamProgressTerminalFailure) {
				return false
			}
			o.statusCode = inspection.failure.statusCode
			return true
		}
		if inspection.terminalSuccess {
			return o.applyStreamingTerminal(routeAttemptOutcomeSucceeded, upstreamProgressTerminalSuccess)
		}
		if !o.terminal {
			o.progress = mergeUpstreamSemanticProgress(o.progress, inspection.progress)
		}
		return false
	default:
		inspection := inspectOpenAIChatStreamEvent(eventType, data)
		if inspection.chunk != nil && inspection.chunk.Usage != nil {
			if usage, ok := statsTokenUsageFromOpenAI(inspection.chunk.Usage); ok {
				o.usage = usage
				o.haveUsage = true
			}
		}
		if inspection.failure != nil {
			if !o.applyStreamingTerminal(routeAttemptOutcomeFailed, upstreamProgressTerminalFailure) {
				return false
			}
			o.statusCode = inspection.failure.statusCode
			return true
		}
		if inspection.terminalSuccess {
			return o.applyStreamingTerminal(routeAttemptOutcomeSucceeded, upstreamProgressTerminalSuccess)
		}
		if !o.terminal {
			o.progress = mergeUpstreamSemanticProgress(o.progress, inspection.progress)
		}
		return false
	}
}

func (o *routeAttemptResponseObserver) observeResponsesEvent(eventType, data string) bool {
	if data == "[DONE]" {
		return o.applyStreamingTerminal(routeAttemptOutcomeSucceeded, upstreamProgressTerminalSuccess)
	}
	event, err := parseResponsesStreamEvent(data)
	if err != nil {
		if !o.terminal {
			o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressUnknown)
		}
		return false
	}
	if strings.TrimSpace(event.Type) == "" {
		event.Type = strings.TrimSpace(eventType)
	}
	if !event.Response.Usage.isZero() {
		o.usage = statsTokenUsageFromResponses(event.Response.Usage)
		o.haveUsage = true
	}

	switch event.Type {
	case "response.completed":
		return o.applyStreamingTerminal(routeAttemptOutcomeSucceeded, upstreamProgressTerminalSuccess)
	case "response.cancelled", "response.canceled":
		return o.applyStreamingTerminal(routeAttemptOutcomeCanceled, upstreamProgressTerminalFailure)
	case "response.failed", "response.incomplete", "error":
		failureHeaders := responsesFailureHeaders(event, o.headers)
		status, _, ok := classifyResponsesFailure(event, failureHeaders)
		if !ok || status == 0 {
			status = http.StatusBadGateway
		}
		if !o.applyStreamingTerminal(routeAttemptOutcomeFailed, upstreamProgressTerminalFailure) {
			return false
		}
		o.statusCode = status
		o.retryAfter = sanitizedRouteAttemptRetryAfter(failureHeaders, "")
		if requestID := responsesUpstreamRequestID(failureHeaders); requestID != "" {
			o.upstreamID = boundOperationalStatLabel(requestID)
		}
		return true
	default:
		if !o.terminal {
			o.progress = mergeUpstreamSemanticProgress(o.progress, routeAttemptResponsesEventProgress(event))
		}
		return false
	}
}

func routeAttemptResponsesEventProgress(event responsesWebSocketStreamEvent) upstreamSemanticProgress {
	typeName := strings.ToLower(strings.TrimSpace(event.Type))
	switch typeName {
	case "response.created", "response.in_progress":
		if responsesOutputHasProgress(event.Response.Output) || !event.Response.Usage.isZero() {
			return upstreamProgressSemanticOutput
		}
		return upstreamProgressAllowedPreamble
	case "response.completed":
		return upstreamProgressTerminalSuccess
	case "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
		return upstreamProgressTerminalFailure
	}
	if strings.Contains(typeName, "function_call") || strings.Contains(typeName, "tool") {
		return upstreamProgressToolActivity
	}
	if strings.Contains(typeName, "output_text") || strings.Contains(typeName, "reasoning") || strings.Contains(typeName, "refusal") || strings.Contains(typeName, "audio") {
		return upstreamProgressSemanticOutput
	}
	if len(event.Item) > 0 {
		var item struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(event.Item, &item) != nil {
			return upstreamProgressUnknown
		}
		itemType := strings.ToLower(strings.TrimSpace(item.Type))
		if strings.Contains(itemType, "function_call") || strings.Contains(itemType, "tool") {
			return upstreamProgressToolActivity
		}
		if itemType != "" {
			return upstreamProgressSemanticOutput
		}
	}
	if responsesOutputHasProgress(event.Response.Output) || !event.Response.Usage.isZero() {
		return upstreamProgressSemanticOutput
	}
	return upstreamProgressUnknown
}

func (o *routeAttemptResponseObserver) publishCleanupTimeout() {
	if o == nil || o.record == nil {
		return
	}
	o.mu.Lock()
	o.cleanupTimedOut = true
	if o.streaming {
		o.finishStreamingUsageLocked()
	} else {
		o.inspectNonStreamingLocked()
	}
	completion := o.completionLocked(false)
	o.mu.Unlock()
	o.record.complete(completion)
}

func (o *routeAttemptResponseObserver) finish(cleanupComplete bool, readErr error) {
	if o == nil {
		return
	}
	o.mu.Lock()
	if o.streaming {
		o.linePendingCR = false
		if len(o.line) > 0 || o.lineOverflow {
			o.consumeSSELineLocked()
		}
		_ = o.sse.dispatch(func(eventType, data string) bool {
			o.observeSSEEvent(eventType, data)
			return true
		})
		o.finishStreamingUsageLocked()
		if !o.terminal {
			switch {
			case errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded):
				if o.cleanupTimedOut {
					o.outcome = routeAttemptOutcomeIncomplete
					o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressUnknown)
				} else {
					o.outcome = routeAttemptOutcomeCanceled
				}
			case errors.Is(readErr, errRouteAttemptBodyClosedEarly) || errors.Is(readErr, io.ErrUnexpectedEOF):
				o.outcome = routeAttemptOutcomeIncomplete
				o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressUnknown)
			case readErr != nil && !errors.Is(readErr, io.EOF):
				o.outcome = routeAttemptOutcomeFailed
				o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressUnknown)
			case cleanupComplete:
				o.outcome = routeAttemptOutcomeIncomplete
				o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressUnknown)
			default:
				o.outcome = routeAttemptOutcomeCanceled
			}
		}
	} else {
		o.inspectNonStreamingLocked()
		if !o.terminal {
			switch {
			case errors.Is(readErr, context.Canceled) || errors.Is(readErr, context.DeadlineExceeded):
				if o.cleanupTimedOut {
					o.outcome = routeAttemptOutcomeIncomplete
				} else {
					o.outcome = routeAttemptOutcomeCanceled
				}
				o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressUnknown)
			case errors.Is(readErr, errRouteAttemptBodyClosedEarly) || errors.Is(readErr, io.ErrUnexpectedEOF):
				o.outcome = routeAttemptOutcomeIncomplete
				o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressUnknown)
			case readErr != nil && !errors.Is(readErr, io.EOF):
				o.outcome = routeAttemptOutcomeFailed
				o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressUnknown)
			}
		}
	}
	completion := o.completionLocked(cleanupComplete)
	o.mu.Unlock()
	o.record.complete(completion)
}

func (o *routeAttemptResponseObserver) finishStreamingUsageLocked() {
	if o.haveUsage {
		return
	}
	if o.endpoint == providerEndpointMessages {
		if o.anthropic.haveInput || o.anthropic.haveOutput {
			prompt := o.anthropic.input + o.anthropic.cacheRead + o.anthropic.cacheCreation
			o.usage = statsTokenUsage{
				PromptTokens:     int64(prompt),
				CompletionTokens: int64(o.anthropic.output),
				TotalTokens:      int64(prompt + o.anthropic.output),
				CachedTokens:     int64(o.anthropic.cacheRead),
			}.normalized()
			o.haveUsage = true
		}
		return
	}
	if o.endpoint == providerEndpointResponses {
		if usage, ok := extractResponsesUsageObject(o.tail.bytes()); ok {
			o.usage = statsTokenUsageFromResponses(usage)
			o.haveUsage = true
		}
		return
	}
	if usage := sniffOpenAIUsageFromBuffer(o.tail.bytes()); usage != nil {
		if normalized, ok := statsTokenUsageFromOpenAI(usage); ok {
			o.usage = normalized
			o.haveUsage = true
		}
	}
}

func sniffOpenAIUsageFromBuffer(buf []byte) *models.OpenAIUsage {
	if usage := sniffOpenAIUsage(buf); usage != nil {
		return usage
	}
	key := []byte(`"usage"`)
	for search := buf; len(search) > 0; {
		idx := bytes.LastIndex(search, key)
		if idx < 0 {
			return nil
		}
		j := idx + len(key)
		for j < len(search) && (search[j] == ' ' || search[j] == '\t' || search[j] == ':' || search[j] == '\r' || search[j] == '\n') {
			j++
		}
		if j < len(search) && search[j] == '{' {
			if object, end := balancedJSONObject(search[j:]); end > 0 {
				var usage models.OpenAIUsage
				if json.Unmarshal(object, &usage) == nil {
					return &usage
				}
			}
		}
		search = search[:idx]
	}
	return nil
}

func (o *routeAttemptResponseObserver) classifyInvalidNonStreamingEnvelopeLocked() {
	if o == nil {
		return
	}
	o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressUnknown)
	if o.envelope != nil && !o.envelope.invalid() && (o.endpoint == providerEndpointChatCompletions || o.tail.overflowed()) {
		o.outcome = routeAttemptOutcomeIncomplete
		return
	}
	o.outcome = routeAttemptOutcomeFailed
}

func routeAttemptJSONHasObjectRoot(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) >= 2 && trimmed[0] == '{' && trimmed[len(trimmed)-1] == '}'
}

func (o *routeAttemptResponseObserver) inspectNonStreamingLocked() {
	tail := o.tail.bytes()
	switch o.endpoint {
	case providerEndpointResponses:
		usage := responsesUsage{}
		if raw, ok := o.envelope.field("usage"); ok {
			var extracted responsesUsage
			if json.Unmarshal(raw, &extracted) == nil {
				usage = extracted
			}
		}
		if usage.isZero() {
			usage = sniffResponsesUsageBody(tail)
		}
		if usage.isZero() {
			if recovered, ok := extractResponsesUsageObject(tail); ok {
				usage = recovered
			}
		}
		if !usage.isZero() {
			o.usage = statsTokenUsageFromResponses(usage)
			o.haveUsage = true
		}
		var envelope struct {
			Status string          `json:"status"`
			Output json.RawMessage `json:"output"`
		}
		parsedWholeEnvelope := !o.tail.overflowed() && routeAttemptJSONHasObjectRoot(tail) && json.Unmarshal(tail, &envelope) == nil
		status := envelope.Status
		if raw, ok := o.envelope.field("status"); ok {
			_ = json.Unmarshal(raw, &status)
		}
		envelopeComplete := parsedWholeEnvelope || o.envelope.complete()
		if envelopeComplete {
			switch strings.ToLower(strings.TrimSpace(status)) {
			case "completed":
				o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressTerminalSuccess)
				o.outcome = routeAttemptOutcomeSucceeded
				o.terminal = true
			case "failed", "incomplete":
				o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressTerminalFailure)
				o.outcome = routeAttemptOutcomeFailed
				o.terminal = true
			case "cancelled", "canceled":
				o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressTerminalFailure)
				o.outcome = routeAttemptOutcomeCanceled
				o.terminal = true
			}
			if parsedWholeEnvelope && responsesOutputHasProgress(envelope.Output) && o.progress != upstreamProgressTerminalSuccess {
				o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressSemanticOutput)
			}
		} else if o.statusCode >= http.StatusOK && o.statusCode < http.StatusBadRequest {
			o.classifyInvalidNonStreamingEnvelopeLocked()
		}
	case providerEndpointMessages:
		var usage models.AnthropicUsage
		if raw, ok := o.envelope.field("usage"); ok {
			var extracted models.AnthropicUsage
			if json.Unmarshal(raw, &extracted) == nil {
				usage = extracted
			}
		}
		var envelope struct {
			Type  string                `json:"type"`
			Error json.RawMessage       `json:"error"`
			Usage models.AnthropicUsage `json:"usage"`
		}
		parsedWholeEnvelope := !o.tail.overflowed() && routeAttemptJSONHasObjectRoot(tail) && json.Unmarshal(tail, &envelope) == nil
		if usage == (models.AnthropicUsage{}) && parsedWholeEnvelope {
			usage = envelope.Usage
		}
		prompt := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens
		o.usage = statsTokenUsage{
			PromptTokens:     int64(prompt),
			CompletionTokens: int64(usage.OutputTokens),
			TotalTokens:      int64(prompt + usage.OutputTokens),
			CachedTokens:     int64(usage.CacheReadInputTokens),
		}.normalized()
		o.haveUsage = !o.usage.isZero()

		envelopeComplete := parsedWholeEnvelope || o.envelope.complete()
		typeName := envelope.Type
		if raw, ok := o.envelope.field("type"); ok {
			_ = json.Unmarshal(raw, &typeName)
		}
		if envelopeComplete {
			switch strings.ToLower(strings.TrimSpace(typeName)) {
			case "message":
				o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressTerminalSuccess)
				o.outcome = routeAttemptOutcomeSucceeded
				o.terminal = true
			case "error":
				o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressTerminalFailure)
				o.outcome = routeAttemptOutcomeFailed
				o.terminal = true
				errorRaw := envelope.Error
				if raw, ok := o.envelope.field("error"); ok {
					errorRaw = raw
				}
				if status, ok := routeAttemptAnthropicErrorStatus(errorRaw); ok {
					o.statusCode = status
				}
			default:
				o.classifyInvalidNonStreamingEnvelopeLocked()
			}
		} else if o.statusCode >= http.StatusOK && o.statusCode < http.StatusBadRequest {
			o.classifyInvalidNonStreamingEnvelopeLocked()
		}
	default:
		var usage *models.OpenAIUsage
		if raw, ok := o.envelope.field("usage"); ok {
			var extracted models.OpenAIUsage
			if json.Unmarshal(raw, &extracted) == nil {
				usage = &extracted
			}
		}
		if usage == nil {
			usage = sniffOpenAIUsageFromBuffer(tail)
		}
		if usage != nil {
			o.usage, o.haveUsage = statsTokenUsageFromOpenAI(usage)
		}
		if o.statusCode >= http.StatusOK && o.statusCode < http.StatusMultipleChoices {
			if o.envelope != nil && o.envelope.complete() {
				o.progress = mergeUpstreamSemanticProgress(o.progress, upstreamProgressTerminalSuccess)
				o.outcome = routeAttemptOutcomeSucceeded
				o.terminal = true
			} else {
				o.classifyInvalidNonStreamingEnvelopeLocked()
			}
		}
	}
	if o.statusCode >= http.StatusBadRequest {
		if o.trace.Delivery == requestExplicitlyRejected {
			o.outcome = routeAttemptOutcomeRejected
		} else {
			o.outcome = routeAttemptOutcomeFailed
		}
	}
}

func routeAttemptAnthropicErrorStatus(errorRaw []byte) (int, bool) {
	if len(errorRaw) == 0 || !json.Valid(errorRaw) {
		return 0, false
	}
	payload := make([]byte, 0, len(errorRaw)+32)
	payload = append(payload, `{"type":"error","error":`...)
	payload = append(payload, errorRaw...)
	payload = append(payload, '}')
	return anthropicStreamErrorStatus(payload)
}

func (o *routeAttemptResponseObserver) completionLocked(cleanupComplete bool) routeAttemptCompletion {
	commitment := o.commitment
	if current := o.operation.commitmentSnapshot(); routeCommitmentRank(current) > routeCommitmentRank(commitment) {
		commitment = current
	}
	decision := o.decision
	if routeAttemptOutcomeIsWasted(o.outcome) && decision == routeRetryAccepted {
		switch {
		case commitment != downstreamCommitmentNone:
			decision = routeRetrySuppressedCommitment
		case !upstreamProgressAllowsTargetSwitch(o.progress):
			decision = routeRetrySuppressedProgress
		}
	}
	completion := routeAttemptCompletion{
		StatusCode:           o.statusCode,
		Outcome:              o.outcome,
		Delivery:             o.trace.Delivery,
		SemanticProgress:     o.progress,
		DownstreamCommitment: commitment,
		RetryDecision:        decision,
		TTFTMs:               o.send.ttftMillis(),
		RetryAfterSeconds:    o.retryAfter,
		UpstreamRequestID:    o.upstreamID,
		CleanupComplete:      cleanupComplete,
		Wasted:               routeAttemptOutcomeIsWasted(o.outcome),
	}
	if o.haveUsage {
		usage := o.usage.normalized()
		completion.ReportedUsage = &usage
	}
	return completion
}

var errRouteAttemptBodyClosedEarly = errors.New("route attempt response body closed before EOF")

type routeAttemptObservedBody struct {
	inner    io.ReadCloser
	observer *routeAttemptResponseObserver
	eof      atomic.Bool
	closed   atomic.Bool
}

func (b *routeAttemptObservedBody) Read(p []byte) (int, error) {
	if b == nil || b.inner == nil {
		return 0, io.EOF
	}
	n, err := b.inner.Read(p)
	if n > 0 && b.observer != nil {
		b.observer.observe(p[:n])
	}
	if err != nil && b.observer != nil {
		if errors.Is(err, io.EOF) {
			b.eof.Store(true)
		}
		b.observer.finish(errors.Is(err, io.EOF), err)
	}
	return n, err
}

func (b *routeAttemptObservedBody) Close() error {
	if b == nil || b.inner == nil {
		return nil
	}
	if !b.closed.CompareAndSwap(false, true) {
		return nil
	}
	var contextErr error
	if b.observer != nil {
		contextErr = b.observer.requestContextError()
	}
	err := b.inner.Close()
	if b.observer != nil && !b.eof.Load() {
		finishErr := error(errRouteAttemptBodyClosedEarly)
		if contextErr != nil {
			finishErr = contextErr
		}
		b.observer.finish(true, finishErr)
	}
	return err
}

func (b *routeAttemptObservedBody) cancelRouteAttempt() {
	if b != nil {
		cancelRouteAttemptBody(b.inner)
	}
}

func (b *routeAttemptObservedBody) routeAttemptTransportOwnership() *routeAttemptTransportOwner {
	if b == nil {
		return nil
	}
	return routeAttemptTransportOwnership(b.inner)
}

func (b *routeAttemptObservedBody) routeAttemptCleanupTimedOut() {
	if b == nil || b.observer == nil {
		return
	}
	b.observer.publishCleanupTimeout()
}

func (b *routeAttemptObservedBody) canceledAtFailure() bool {
	if b == nil || b.inner == nil {
		return false
	}
	if observed, ok := b.inner.(interface{ canceledAtFailure() bool }); ok {
		return observed.canceledAtFailure()
	}
	return false
}

func observeRouteAttemptResponse(resp *http.Response, record *routeAttemptRecord, operation *routeOperation, trace routeAttemptTrace, send *routeSendObservation, endpoint string, streaming bool) *http.Response {
	if resp == nil || record == nil {
		return resp
	}
	if resp.Body == nil {
		completedTrace := trace
		completedTrace.CleanupDone = true
		outcome := routeAttemptOutcomeFromTrace(completedTrace)
		progress := trace.Progress
		if !streaming && endpoint == providerEndpointChatCompletions && resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
			outcome = routeAttemptOutcomeIncomplete
			progress = upstreamProgressUnknown
		}
		completion := routeAttemptCompletion{
			StatusCode:           resp.StatusCode,
			Outcome:              outcome,
			Delivery:             trace.Delivery,
			SemanticProgress:     progress,
			DownstreamCommitment: operation.commitmentSnapshot(),
			RetryDecision:        trace.Decision,
			TTFTMs:               send.ttftMillis(),
			RetryAfterSeconds:    sanitizedRouteAttemptRetryAfter(resp.Header, ""),
			UpstreamRequestID:    responsesUpstreamRequestID(resp.Header),
			CleanupComplete:      true,
		}
		completion.Wasted = routeAttemptOutcomeIsWasted(completion.Outcome)
		record.complete(completion)
		return resp
	}
	streaming = streaming || strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream")
	observer := newRouteAttemptResponseObserver(record, operation, trace, send, endpoint, streaming, resp.Header)
	if resp.Request != nil {
		observer.attemptCtx = resp.Request.Context()
	}
	if terminal, ok := routeAttemptPreparedTerminalFromResponse(resp); ok && endpoint == providerEndpointResponses && !terminal.Response.Usage.isZero() {
		observer.mu.Lock()
		observer.usage = statsTokenUsageFromResponses(terminal.Response.Usage)
		observer.haveUsage = true
		observer.mu.Unlock()
	}
	resp.Body = &routeAttemptObservedBody{inner: resp.Body, observer: observer}
	return resp
}

func completeCapturedRouteAttempt(record *routeAttemptRecord, operation *routeOperation, trace routeAttemptTrace, send *routeSendObservation, endpoint string, captured *capturedRouteResponse, cleanupComplete bool, outcome routeAttemptOutcome) {
	if record == nil {
		return
	}
	status := trace.StatusCode
	headers := http.Header(nil)
	body := []byte(nil)
	if captured != nil {
		status = captured.statusCode
		headers = captured.header
		body = captured.body
	}
	observer := newRouteAttemptResponseObserver(record, operation, trace, send, endpoint, false, headers)
	observer.statusCode = status
	observer.tail.append(body)
	observer.envelope.observe(body)
	observer.mu.Lock()
	observer.inspectNonStreamingLocked()
	observer.outcome = outcome
	completion := observer.completionLocked(cleanupComplete)
	observer.mu.Unlock()
	record.complete(completion)
}

type routeAttemptFailure struct {
	err           error
	response      *capturedRouteResponse
	attribution   routeResultAttribution
	delivery      requestDelivery
	progress      upstreamSemanticProgress
	commitment    downstreamCommitment
	decision      routeRetryDecision
	statusCode    int
	retryAfter    string
	upstreamID    string
	cleanupDone   bool
	usage         statsTokenUsage
	usageReported bool
	outcome       routeAttemptOutcome
}

type routeResultAttribution struct {
	targetID     string
	providerID   string
	providerKind string
}

func routeResultAttributionForTarget(target targetBinding) routeResultAttribution {
	attribution := routeResultAttribution{targetID: target.id}
	if target.provider != nil {
		attribution.providerID = target.provider.id
		attribution.providerKind = string(target.provider.kind)
	}
	return attribution
}

func (a routeResultAttribution) recordFinal(ctx context.Context) {
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		summary.setFinalRouteAttribution(a.targetID, a.providerID, a.providerKind)
	}
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

	attemptNow := time.Now
	if h != nil && h.stats != nil && h.stats.now != nil {
		attemptNow = h.stats.now
	}
	kind := routeAttemptKindFromContext(ctx)
	failures := make([]routeAttemptFailure, 0, route.policy.maxTargetAttempts)
	for {
		targets := orderedRouteTargets(route, operation, endpoint)
		if len(targets) == 0 {
			break
		}
		target := targets[0]
		attribution := routeResultAttributionForTarget(target)
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
			failure := routeAttemptFailure{err: err, attribution: attribution, delivery: requestDefinitelyNotDelivered, progress: upstreamProgressNone, commitment: downstreamCommitmentNone, decision: routeRetrySuppressedNonretryable}
			failures = append(failures, failure)
			operation.appendTrace(routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, Delivery: failure.delivery, Progress: failure.progress, Commitment: failure.commitment, Decision: failure.decision, CleanupDone: true})
			break
		}

		req, err := h.newProviderJSONRequest(ctx, target.provider, http.MethodPost, dispatchPath, preparedBody, cloneSanitizedRouteHeaders(extraHeaders), "", owner)
		if err != nil {
			failure := routeAttemptFailure{err: err, attribution: attribution, delivery: requestDefinitelyNotDelivered, progress: upstreamProgressNone, commitment: downstreamCommitmentNone, decision: routeRetrySuppressedNonretryable}
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

		if reserved, decision := operation.reserveSendAtDispatch(ctx, h.ShuttingDown()); !reserved {
			message := "route upstream-send budget exhausted"
			switch decision {
			case routeRetrySuppressedAdmission:
				message = "client disconnected before upstream dispatch"
			case routeRetrySuppressedLifecycle:
				message = "route operation ended before upstream dispatch"
			}
			failure := routeAttemptFailure{err: fmt.Errorf("%s", message), attribution: attribution, delivery: requestDefinitelyNotDelivered, progress: upstreamProgressNone, commitment: downstreamCommitmentNone, decision: decision}
			failures = append(failures, failure)
			operation.appendTrace(routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, Delivery: failure.delivery, Progress: failure.progress, Commitment: failure.commitment, Decision: failure.decision, CleanupDone: true})
			break
		}

		startedAt := attemptNow()
		observation := newRouteSendObservation(startedAt, attemptNow)
		req = req.WithContext(httptrace.WithClientTrace(req.Context(), observation.trace()))
		var attemptRecord *routeAttemptRecord
		if !routeAttemptStatsSuppressed(operation.inbound) {
			attemptRecord = h.RecordUpstreamAttempt(operation.inbound, RouteAttemptObservation{
				OperationID:  operation.operationID(),
				RouteID:      route.public.routeID,
				TargetID:     target.id,
				ProviderID:   target.provider.id,
				ProviderKind: string(target.provider.kind),
				Sequence:     sequence,
				AttemptKind:  attemptKind,
				operation:    operation,
				startedAt:    startedAt,
			})
		}

		resp, sendErr := h.singleInferenceSend(req, observation)
		if resp != nil {
			sanitizeExplicitRouteResponseHeaders(resp.Header)
		}
		if sendErr == nil && resp != nil {
			normalizeExplicitModelHeaders(resp.Header, route.public.id)
		}
		if sendErr != nil {
			delivery := observation.deliveryForError()
			outcome := routeAttemptOutcomeTransportError
			if routeAttemptSendWasCanceled(ctx, sendErr) {
				outcome = routeAttemptOutcomeCanceled
			}
			decision := routeRetrySuppressedDelivery
			if delivery == requestDefinitelyNotDelivered && operation.allowsAutomaticTargetSwitch(kind) && operation.retryAdmissionOpen(ctx, h.ShuttingDown()) && route.policy.mode == routeModePriorityFailover {
				decision = routeRetrySwitchTarget
			}
			var captured *capturedRouteResponse
			cleanupDone := true
			statusCode := 0
			upstreamID := ""
			if resp != nil {
				captured, cleanupDone = captureRouteResponse(resp)
				if captured != nil {
					statusCode = captured.statusCode
					upstreamID = captured.upstreamID
				}
			}
			failure := routeAttemptFailure{err: sendErr, attribution: attribution, delivery: delivery, progress: upstreamProgressNone, commitment: downstreamCommitmentNone, decision: decision, statusCode: statusCode, upstreamID: upstreamID, cleanupDone: cleanupDone, outcome: outcome}
			failures = append(failures, failure)
			trace := routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, StatusCode: statusCode, Delivery: delivery, Progress: upstreamProgressNone, Commitment: downstreamCommitmentNone, Decision: decision, UpstreamID: upstreamID, CleanupDone: cleanupDone}
			operation.appendTrace(trace)
			if captured != nil {
				completeCapturedRouteAttempt(attemptRecord, operation, trace, observation, endpoint, captured, cleanupDone, outcome)
			} else if attemptRecord != nil {
				attemptRecord.complete(routeAttemptCompletion{
					StatusCode:           statusCode,
					Outcome:              outcome,
					Delivery:             delivery,
					SemanticProgress:     upstreamProgressNone,
					DownstreamCommitment: downstreamCommitmentNone,
					RetryDecision:        decision,
					TTFTMs:               observation.ttftMillis(),
					UpstreamRequestID:    upstreamID,
					CleanupComplete:      cleanupDone,
					Wasted:               true,
				})
			}
			if decision == routeRetrySwitchTarget {
				continue
			}
			break
		}

		if stream && resp.StatusCode == http.StatusOK {
			accepted, streamFailure := h.prepareExplicitResponsesStream(ctx, operation, route, target, resp)
			if streamFailure != nil {
				streamFailure.attribution = attribution
				if streamFailure.decision == "" {
					streamFailure.decision = routeRetrySuppressedProgress
				}
				if streamFailure.delivery == requestExplicitlyRejected && operation.allowsAutomaticTargetSwitch(kind) && operation.retryAdmissionOpen(ctx, h.ShuttingDown()) && route.policy.mode == routeModePriorityFailover {
					streamFailure.decision = routeRetrySwitchTarget
				}
				failures = append(failures, *streamFailure)
				upstreamID := streamFailure.upstreamID
				if upstreamID == "" {
					upstreamID = responsesUpstreamRequestID(resp.Header)
				}
				trace := routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, StatusCode: streamFailure.statusCode, Delivery: streamFailure.delivery, Progress: streamFailure.progress, Commitment: streamFailure.commitment, Decision: streamFailure.decision, UpstreamID: upstreamID, CleanupDone: streamFailure.cleanupDone}
				operation.appendTrace(trace)
				if attemptRecord != nil {
					completion := routeAttemptCompletion{
						StatusCode:           streamFailure.statusCode,
						Outcome:              streamFailure.outcome,
						Delivery:             streamFailure.delivery,
						SemanticProgress:     streamFailure.progress,
						DownstreamCommitment: streamFailure.commitment,
						RetryDecision:        streamFailure.decision,
						TTFTMs:               observation.ttftMillis(),
						RetryAfterSeconds:    sanitizedRouteAttemptRetryAfter(nil, streamFailure.retryAfter),
						UpstreamRequestID:    upstreamID,
						CleanupComplete:      streamFailure.cleanupDone,
						Wasted:               true,
					}
					if streamFailure.usageReported {
						usage := streamFailure.usage.normalized()
						completion.ReportedUsage = &usage
					}
					attemptRecord.complete(completion)
				}
				if streamFailure.decision == routeRetrySwitchTarget {
					continue
				}
				break
			}
			info, _ := explicitRouteResponseInfoFromResponse(accepted)
			if err := h.bindExplicitResponseHeaders(info, accepted.Header); err != nil {
				failure := routeAttemptFailure{err: err, attribution: attribution, delivery: requestDeliveredOrAmbiguous, progress: upstreamProgressAllowedPreamble, commitment: downstreamCommitmentNone, decision: routeRetrySuppressedState, statusCode: accepted.StatusCode, upstreamID: responsesUpstreamRequestID(accepted.Header), outcome: routeAttemptOutcomeFailed}
				trace := routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, StatusCode: accepted.StatusCode, Delivery: failure.delivery, Progress: failure.progress, Commitment: failure.commitment, Decision: failure.decision, UpstreamID: failure.upstreamID}
				// The prepared stream can already contain a terminal usage event even
				// though its response-header state is invalid. Observe and drain it
				// internally before discarding the response so physical and wasted
				// spend is retained without exposing any prepared bytes downstream.
				accepted = observeRouteAttemptResponse(accepted, attemptRecord, operation, trace, observation, endpoint, true)
				cleanupDone := drainRouteAttemptBodyWithTimeout(accepted.Body, upstreamErrorDetailDrainTimeout)
				failure.cleanupDone = cleanupDone
				trace.CleanupDone = cleanupDone
				failures = append(failures, failure)
				operation.appendTrace(trace)
				if attemptRecord != nil {
					attemptRecord.complete(routeAttemptCompletion{StatusCode: accepted.StatusCode, Outcome: routeAttemptOutcomeFailed, Delivery: failure.delivery, SemanticProgress: failure.progress, DownstreamCommitment: failure.commitment, RetryDecision: failure.decision, TTFTMs: observation.ttftMillis(), UpstreamRequestID: failure.upstreamID, CleanupComplete: cleanupDone, Wasted: true})
				}
				break
			}
			operation.pinTarget(target.id)
			trace := routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, StatusCode: accepted.StatusCode, Delivery: requestDeliveredOrAmbiguous, Progress: upstreamProgressAllowedPreamble, Commitment: downstreamCommitmentNone, Decision: routeRetryAccepted, UpstreamID: responsesUpstreamRequestID(accepted.Header), CleanupDone: false}
			operation.appendTrace(trace)
			accepted = observeRouteAttemptResponse(accepted, attemptRecord, operation, trace, observation, endpoint, true)
			attribution.recordFinal(operation.inbound)
			return accepted, nil
		}

		responseUpstreamID := responsesUpstreamRequestID(resp.Header)
		var capturedObservation *capturedRouteResponse
		if routeAdapterMayExplicitlyReject(target, endpoint, resp.StatusCode) {
			captured, cleanupDone := captureRouteResponse(resp)
			responseUpstreamID = captured.upstreamID
			if !cleanupDone {
				failure := routeAttemptFailure{err: fmt.Errorf("route attempt cleanup did not complete"), attribution: attribution, delivery: requestDeliveredOrAmbiguous, progress: upstreamProgressUnknown, commitment: downstreamCommitmentNone, decision: routeRetrySuppressedLifecycle, statusCode: resp.StatusCode, upstreamID: captured.upstreamID, cleanupDone: false, outcome: routeAttemptOutcomeFailed}
				failures = append(failures, failure)
				trace := routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, StatusCode: resp.StatusCode, Delivery: failure.delivery, Progress: failure.progress, Commitment: failure.commitment, Decision: failure.decision, UpstreamID: captured.upstreamID, CleanupDone: false}
				operation.appendTrace(trace)
				completeCapturedRouteAttempt(attemptRecord, operation, trace, observation, endpoint, captured, false, routeAttemptOutcomeFailed)
				break
			}
			if routeAdapterCertifiesHTTPRejection(target, endpoint, captured) {
				decision := routeRetrySuppressedMode
				if route.policy.mode == routeModePriorityFailover && operation.allowsAutomaticTargetSwitch(kind) && operation.retryAdmissionOpen(ctx, h.ShuttingDown()) {
					decision = routeRetrySwitchTarget
				}
				failure := routeAttemptFailure{response: captured, attribution: attribution, delivery: requestExplicitlyRejected, progress: upstreamProgressNone, commitment: downstreamCommitmentNone, decision: decision, statusCode: captured.statusCode, retryAfter: headerGetCI(captured.header, "Retry-After"), upstreamID: captured.upstreamID, cleanupDone: true, outcome: routeAttemptOutcomeRejected}
				failures = append(failures, failure)
				trace := routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, StatusCode: captured.statusCode, Delivery: failure.delivery, Progress: failure.progress, Commitment: failure.commitment, Decision: decision, UpstreamID: captured.upstreamID, CleanupDone: true}
				operation.appendTrace(trace)
				completeCapturedRouteAttempt(attemptRecord, operation, trace, observation, endpoint, captured, true, routeAttemptOutcomeRejected)
				if decision == routeRetrySwitchTarget {
					continue
				}
				break
			}
			// The status was potentially retryable but the adapter could not prove
			// pre-execution rejection. Return it as an ambiguous terminal response.
			captured.recordFinalUpstreamID(operation.inbound)
			capturedObservation = captured
			resp = captured.response()
		}

		if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices && (stream || endpoint != providerEndpointResponses) {
			info, _ := explicitRouteResponseInfoFromResponse(resp)
			if err := h.bindExplicitResponseHeaders(info, resp.Header); err != nil {
				failure := routeAttemptFailure{err: err, attribution: attribution, delivery: requestDeliveredOrAmbiguous, progress: upstreamProgressNone, commitment: downstreamCommitmentNone, decision: routeRetrySuppressedState, statusCode: resp.StatusCode, upstreamID: responseUpstreamID, outcome: routeAttemptOutcomeFailed}
				trace := routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, StatusCode: resp.StatusCode, Delivery: failure.delivery, Progress: failure.progress, Commitment: failure.commitment, Decision: failure.decision, UpstreamID: responseUpstreamID}
				cleanupDone := false
				if capturedObservation != nil {
					cleanupDone = true
				} else {
					// A 2xx Chat/Messages response can report usage even when its
					// response-header state is invalid. Observe and drain through the
					// bounded attempt body before discarding it so that provider spend is
					// retained as physical and wasted usage. The drain is time-bounded;
					// a stalled body is closed asynchronously and cannot authorize failover.
					resp = observeRouteAttemptResponse(resp, attemptRecord, operation, trace, observation, endpoint, stream)
					cleanupDone = drainRouteAttemptBodyWithTimeout(resp.Body, upstreamErrorDetailDrainTimeout)
				}
				failure.cleanupDone = cleanupDone
				trace.CleanupDone = cleanupDone
				failures = append(failures, failure)
				operation.appendTrace(trace)
				if capturedObservation != nil {
					completeCapturedRouteAttempt(attemptRecord, operation, trace, observation, endpoint, capturedObservation, cleanupDone, routeAttemptOutcomeFailed)
				} else if attemptRecord != nil {
					attemptRecord.complete(routeAttemptCompletion{StatusCode: resp.StatusCode, Outcome: routeAttemptOutcomeFailed, Delivery: failure.delivery, SemanticProgress: failure.progress, DownstreamCommitment: failure.commitment, RetryDecision: failure.decision, TTFTMs: observation.ttftMillis(), UpstreamRequestID: responseUpstreamID, CleanupComplete: cleanupDone, Wasted: true})
				}
				break
			}
		}
		operation.pinTarget(target.id)
		trace := routeAttemptTrace{Sequence: sequence, TargetID: target.id, ProviderID: target.provider.id, Kind: attemptKind, StatusCode: resp.StatusCode, Delivery: requestDeliveredOrAmbiguous, Progress: upstreamProgressNone, Commitment: downstreamCommitmentNone, Decision: routeRetryAccepted, UpstreamID: responseUpstreamID, CleanupDone: capturedObservation != nil}
		operation.appendTrace(trace)
		if capturedObservation != nil {
			outcome := routeAttemptOutcomeSucceeded
			if resp.StatusCode >= http.StatusBadRequest {
				outcome = routeAttemptOutcomeFailed
			}
			completeCapturedRouteAttempt(attemptRecord, operation, trace, observation, endpoint, capturedObservation, true, outcome)
		} else {
			resp = observeRouteAttemptResponse(resp, attemptRecord, operation, trace, observation, endpoint, stream)
		}
		attribution.recordFinal(operation.inbound)
		return resp, nil
	}

	h.recordExplicitRouteExhaustion(operation, endpoint)
	failure := selectFinalRouteFailure(failures)
	failure.attribution.recordFinal(operation.inbound)
	if failure.response != nil {
		failure.response.recordFinalUpstreamID(operation.inbound)
		return failure.response.response(), nil
	}
	if failure.err != nil {
		return nil, &routeExecutionFailureError{failure: failure}
	}
	return nil, fmt.Errorf("model route %q has no eligible target", route.public.id)
}

func (h *ProxyHandler) singleInferenceSend(req *http.Request, observation *routeSendObservation) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("upstream request is required")
	}
	attemptCtx, attemptCancel := context.WithCancel(req.Context())
	owner := &routeAttemptTransportOwner{cancel: attemptCancel}
	req = req.WithContext(attemptCtx)

	client := h.client
	if client == nil {
		client = http.DefaultClient
	}
	clone := *client
	clone.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := clone.Do(req)
	if resp == nil || resp.Body == nil {
		owner.cancelRequest()
		return resp, err
	}
	if resp.Request == nil {
		resp.Request = req
	}
	resp.Body = &routeAttemptTransportBody{inner: resp.Body, owner: owner, observation: observation}
	return resp, err
}

func routeAttemptSendWasCanceled(ctx context.Context, err error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	if ctx == nil {
		return false
	}
	ctxErr := ctx.Err()
	return errors.Is(ctxErr, context.Canceled) || errors.Is(ctxErr, context.DeadlineExceeded)
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

	// Every target attempt starts from its own copy of the immutable logical
	// request. Provider-specific Responses policy must run only after target
	// selection so one target's unsupported fields cannot leak into failover.
	prepared := append([]byte(nil), body...)
	if endpoint == providerEndpointResponses {
		prepared, _ = stripUnsupportedResponsesRequestFields(prepared, target.provider)
	}
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
	streamErr := responsesStreamEventError(event)
	code := strings.ToLower(strings.TrimSpace(streamErr.Code))
	errType := strings.ToLower(strings.TrimSpace(streamErr.Type))
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

type routeAttemptPreparedTerminalContextKey struct{}

func withRouteAttemptPreparedTerminal(resp *http.Response, terminal responsesWebSocketStreamEvent) *http.Response {
	if resp == nil || resp.Request == nil {
		return resp
	}
	clone := new(http.Response)
	*clone = *resp
	clone.Request = resp.Request.WithContext(context.WithValue(resp.Request.Context(), routeAttemptPreparedTerminalContextKey{}, terminal))
	return clone
}

func routeAttemptPreparedTerminalFromResponse(resp *http.Response) (responsesWebSocketStreamEvent, bool) {
	if resp == nil || resp.Request == nil {
		return responsesWebSocketStreamEvent{}, false
	}
	terminal, ok := resp.Request.Context().Value(routeAttemptPreparedTerminalContextKey{}).(responsesWebSocketStreamEvent)
	return terminal, ok && strings.TrimSpace(terminal.Type) != ""
}

func (h *ProxyHandler) prepareExplicitResponsesStream(ctx context.Context, operation *routeOperation, route *modelRoute, target targetBinding, resp *http.Response) (*http.Response, *routeAttemptFailure) {
	transportOwner := routeAttemptTransportOwnership(resp.Body)
	prepared := newResponsesPreparedStreamWithPolicy(resp, responsesPrecommitMaxPeekBytes, true, true)
	result, hasResult, awaitSource, err := prepared.await(operation.inbound, ctx, responsesPrecommitPeekTimeout)
	if err != nil {
		cleanupDone := prepared.abortAndWait(upstreamErrorDetailDrainTimeout)
		delivery := requestDeliveredOrAmbiguous
		failure := &routeAttemptFailure{
			err:         err,
			delivery:    delivery,
			progress:    upstreamProgressUnknown,
			commitment:  downstreamCommitmentNone,
			statusCode:  resp.StatusCode,
			upstreamID:  responsesUpstreamRequestID(resp.Header),
			cleanupDone: cleanupDone,
			outcome:     routeAttemptOutcomeFailed,
		}
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			failure.outcome = routeAttemptOutcomeCanceled
			if awaitSource == responsesPreparedAwaitInbound {
				failure.decision = routeRetrySuppressedAdmission
			} else {
				failure.decision = routeRetrySuppressedLifecycle
			}
		}
		if !cleanupDone {
			failure.decision = routeRetrySuppressedLifecycle
		}
		if !hasResult {
			return nil, failure
		}
	}
	if hasResult && result.failure != nil {
		failureHeaders := responsesFailureHeaders(*result.failure, resp.Header)
		streamErr := responsesStreamEventError(*result.failure)
		if status, safe := routeAdapterCertifiesStreamFailure(target, *result.failure); safe && result.precommitReplaySafe {
			if !prepared.abortAndWait(upstreamErrorDetailDrainTimeout) {
				return nil, &routeAttemptFailure{
					err:         fmt.Errorf("responses attempt cleanup did not complete before failover"),
					delivery:    requestDeliveredOrAmbiguous,
					progress:    upstreamProgressUnknown,
					commitment:  downstreamCommitmentNone,
					decision:    routeRetrySuppressedLifecycle,
					statusCode:  status,
					upstreamID:  responsesUpstreamRequestID(failureHeaders),
					cleanupDone: false,
					outcome:     routeAttemptOutcomeFailed,
				}
			}
			body, _ := json.Marshal(map[string]any{"error": streamErr})
			return nil, &routeAttemptFailure{
				err:         &upstreamError{statusCode: status, body: body, retryAfter: result.retryAfter, headers: failureHeaders.Clone()},
				delivery:    requestExplicitlyRejected,
				progress:    upstreamProgressAllowedPreamble,
				commitment:  downstreamCommitmentNone,
				statusCode:  status,
				retryAfter:  result.retryAfter,
				upstreamID:  responsesUpstreamRequestID(failureHeaders),
				cleanupDone: true,
				outcome:     routeAttemptOutcomeRejected,
			}
		}
	}
	if hasResult && result.decision == responsesPeekDecisionTranslate && result.failure != nil {
		// This terminal failure is intentionally not replay-safe (for example, it
		// carries partial usage), so the route must not switch targets. Preserve the
		// existing client failure-turn accounting while also recording the same
		// reported spend in the physical/wasted ledger.
		observeResponsesUsage(operation.inbound, result.failure.Response.Usage)
		cleanupDone := prepared.abortAndWait(upstreamErrorDetailDrainTimeout)
		failureHeaders := responsesFailureHeaders(*result.failure, resp.Header)
		body, _ := json.Marshal(map[string]any{"error": responsesStreamEventError(*result.failure)})
		usage := statsTokenUsageFromResponses(result.failure.Response.Usage)
		failure := &routeAttemptFailure{
			err:           &upstreamError{statusCode: result.status, body: body, retryAfter: result.retryAfter, headers: failureHeaders.Clone()},
			delivery:      requestDeliveredOrAmbiguous,
			progress:      upstreamProgressTerminalFailure,
			commitment:    downstreamCommitmentNone,
			statusCode:    result.status,
			retryAfter:    result.retryAfter,
			upstreamID:    responsesUpstreamRequestID(failureHeaders),
			cleanupDone:   cleanupDone,
			usage:         usage,
			usageReported: !result.failure.Response.Usage.isZero(),
			outcome:       routeAttemptOutcomeFailed,
		}
		if !cleanupDone {
			failure.decision = routeRetrySuppressedLifecycle
		}
		return nil, failure
	}
	prepared.stopTerminalObservation()
	accepted := prepared.commitResponse()
	if accepted != nil {
		if hasResult && result.terminal != nil {
			accepted = withRouteAttemptPreparedTerminal(accepted, *result.terminal)
		}
		accepted.Body = retainRouteAttemptTransportOwnership(accepted.Body, transportOwner)
	}
	return accepted, nil
}

// newResponsesPreparedStreamWithPolicy is the route-aware variant. Legacy
// callers retain their current header-sensitive preamble behavior; explicit
// priority routes always hold response.created/response.in_progress until a
// semantic event, terminal event, timeout, or byte bound makes the target
// irrevocable.
func newResponsesPreparedStreamWithPolicy(resp *http.Response, maxPeekBytes int, observeTerminal, holdPreamble bool) *responsesPreparedStream {
	// Explicit priority routes commit at the configured peek byte bound. Legacy
	// quota-aware preamble holding may use the larger compatibility cap.
	return newResponsesPreparedStreamConfigured(resp, maxPeekBytes, observeTerminal, holdPreamble, maxPeekBytes)
}

func (s *responsesPreparedStream) abortAndWait(timeout time.Duration) bool {
	if s == nil {
		return true
	}
	if s.resp != nil {
		cancelRouteAttemptBody(s.resp.Body)
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
	body := resp.Body
	for body != nil {
		switch wrapped := body.(type) {
		case *responsesPreparedBody:
			return true
		case *routeAttemptObservedBody:
			body = wrapped.inner
		case *routeAttemptCancellationBody:
			body = wrapped.inner
		case *lifecycleAwareReadCloser:
			body = wrapped.ReadCloser
		default:
			return false
		}
	}
	return false
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

func publishRouteAttemptBodyCleanupTimeout(body io.ReadCloser) {
	if observed, ok := body.(interface{ routeAttemptCleanupTimedOut() }); ok {
		observed.routeAttemptCleanupTimedOut()
	}
}

func drainRouteAttemptBodyWithTimeout(body io.ReadCloser, timeout time.Duration) bool {
	if body == nil {
		return true
	}

	var timer *time.Timer
	if timeout > 0 {
		// Start the bound before scheduling the owner so cleanup admission can never
		// add unbounded backpressure. The owner is the only goroutine that reads or
		// closes this response body.
		timer = time.NewTimer(timeout)
		defer timer.Stop()
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(io.Discard, body)
		_ = body.Close()
		close(done)
	}()

	if timer == nil {
		<-done
		return true
	}
	select {
	case <-done:
		return true
	case <-timer.C:
		publishRouteAttemptBodyCleanupTimeout(body)
		// Request-context cancellation interrupts Read for conforming net/http
		// transports. Ownership stays with the drain goroutine, which performs the
		// sole Close after Read returns.
		cancelRouteAttemptBody(body)
		return false
	}
}

func closeResponseBodyWithTimeout(body io.ReadCloser, timeout time.Duration) bool {
	if body == nil {
		return true
	}
	var timer *time.Timer
	if timeout > 0 {
		timer = time.NewTimer(timeout)
		defer timer.Stop()
	}
	done := make(chan struct{})
	go func() {
		_ = body.Close()
		close(done)
	}()
	if timer == nil {
		<-done
		return true
	}
	select {
	case <-done:
		return true
	case <-timer.C:
		cancelRouteAttemptBody(body)
		return false
	}
}
