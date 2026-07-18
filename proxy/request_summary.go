package proxy

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

type requestSummaryContextKey struct{}
type upstreamAttemptObserverContextKey struct{}

type upstreamAttemptObserver struct {
	attempted atomic.Bool
}

// RequestSummary is a mutable per-request summary populated by handlers and
// emitted by the server-level request logging middleware.
type RequestSummary struct {
	mu sync.Mutex

	endpoint          string
	model             string
	metricModel       string
	provider          string
	providerKind      string
	modelKnown        bool
	operationID       string
	routeID           string
	finalTarget       string
	lastTarget        string
	lastProvider      string
	lastProviderKind  string
	upstreamSendCount int64
	targetSwitchCount int64
	routeExhausted    bool
	streamSet         bool
	stream            bool
	upstreamRequestID string
	upstreamAttempted bool
	promptTokens      *int
	completionTokens  *int
	totalTokens       *int
	cachedTokens      *int
	reasoningTokens   *int
	// extraPromptTokens / extraCompletionTokens accumulate out-of-band token
	// spend that is separate from the turn's own reported usage — e.g. an
	// internal /responses compaction call made while serving a 413 oversized-
	// replay fallback. The turn usage (setOpenAIUsage) overwrites, so this extra
	// spend is kept additively and folded into the totals by readSummaryForStats
	// rather than being clobbered by the final turn's usage.
	extraPromptTokens     int
	extraCompletionTokens int
	// failureStatus, when non-zero, is an error status the handler observed
	// out-of-band after the HTTP response status was already committed — e.g. a
	// streaming /responses turn that emits response.failed/incomplete after the
	// 200 header was sent. The stats middleware prefers it over the recorded HTTP
	// status so post-commit failures are not counted as successes.
	failureStatus int
	// statsSuppressed excludes local shutdown rejections and lifecycle-canceled
	// in-flight work from provider traffic accounting. A semantic provider failure
	// already recorded in failureStatus wins and cannot be suppressed afterward.
	statsSuppressed bool
}

// WithRequestSummary attaches a mutable request summary to ctx and returns both
// the derived context and the summary pointer. Handlers mutate the pointer in
// place so middleware can observe the final values after the handler returns.
func WithRequestSummary(ctx context.Context) (context.Context, *RequestSummary) {
	if ctx == nil {
		ctx = context.Background()
	}
	if existing := RequestSummaryFromContext(ctx); existing != nil {
		return ctx, existing
	}
	summary := &RequestSummary{}
	return context.WithValue(ctx, requestSummaryContextKey{}, summary), summary
}

// RequestSummaryFromContext returns the mutable request summary attached to ctx,
// if any.
func RequestSummaryFromContext(ctx context.Context) *RequestSummary {
	if ctx == nil {
		return nil
	}
	summary, _ := ctx.Value(requestSummaryContextKey{}).(*RequestSummary)
	return summary
}

func withUpstreamAttemptObserver(ctx context.Context) (context.Context, *upstreamAttemptObserver) {
	if ctx == nil {
		ctx = context.Background()
	}
	observer := &upstreamAttemptObserver{}
	return context.WithValue(ctx, upstreamAttemptObserverContextKey{}, observer), observer
}

func (o *upstreamAttemptObserver) Attempted() bool {
	return o != nil && o.attempted.Load()
}

func observeUpstreamAttempt(ctx context.Context) {
	if ctx == nil {
		return
	}
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		summary.markUpstreamAttempted()
	}
	if observer, _ := ctx.Value(upstreamAttemptObserverContextKey{}).(*upstreamAttemptObserver); observer != nil {
		observer.attempted.Store(true)
	}
}

// SetOperationID records the stable proxy-owned logical operation ID. The
// first non-empty value wins so retries and target switches cannot replace the
// identity established at admission.
func (s *RequestSummary) SetOperationID(operationID string) {
	if s == nil {
		return
	}
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.operationID == "" {
		s.operationID = operationID
	}
}

// OperationID returns the stable logical operation ID, if one was recorded.
func (s *RequestSummary) OperationID() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.operationID
}

// SetRouteID records the logical model-route ID. The first non-empty value wins
// because one client request must not move between public model routes.
func (s *RequestSummary) SetRouteID(routeID string) {
	if s == nil {
		return
	}
	routeID = strings.TrimSpace(routeID)
	if routeID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.routeID == "" {
		s.routeID = routeID
	}
}

// RouteID returns the logical model-route ID, if one was recorded.
func (s *RequestSummary) RouteID() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.routeID
}

// SetFinalTarget records the final/canonical physical target selected for the
// logical operation. Unlike operation and route IDs, the target is intentionally
// replaceable because a later result-selection step can supersede an earlier
// provisional result.
func (s *RequestSummary) SetFinalTarget(targetID string) {
	if s == nil {
		return
	}
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalTarget = targetID
}

// setFinalRouteAttribution atomically records the target/provider attribution
// for the response or error the route executor actually returns. A result with
// no physical target leaves canonical attribution unchanged rather than
// promoting the most recently dispatched attempt.
func (s *RequestSummary) setFinalRouteAttribution(targetID, provider, kind string) {
	if s == nil {
		return
	}
	targetID = strings.TrimSpace(targetID)
	provider = strings.TrimSpace(provider)
	kind = strings.TrimSpace(kind)
	if targetID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalTarget = targetID
	s.provider = provider
	s.providerKind = kind
}

// setFinalRouteResult atomically restores the complete attribution for the
// response or error selected for return. Unlike setUpstreamRequestID, an empty
// upstream request ID is meaningful here: it clears correlation metadata left
// by a later attempt when an earlier canonical result wins precedence.
func (s *RequestSummary) setFinalRouteResult(targetID, provider, kind, upstreamRequestID string) {
	if s == nil {
		return
	}
	targetID = strings.TrimSpace(targetID)
	if targetID == "" {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.finalTarget = targetID
	s.provider = strings.TrimSpace(provider)
	s.providerKind = strings.TrimSpace(kind)
	s.upstreamRequestID = strings.TrimSpace(upstreamRequestID)
}

// FinalTarget returns the final/canonical physical target ID, if known.
func (s *RequestSummary) FinalTarget() string {
	if s == nil {
		return ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.finalTarget
}

// RecordUpstreamSend records one actual physical upstream dispatch. Target
// attempts that fail before network dispatch must not call this method.
func (s *RequestSummary) RecordUpstreamSend() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upstreamSendCount++
}

// recordUpstreamAttempt applies one physical-send observation atomically. The
// latest dispatched target/provider is kept separate from final attribution so
// a later prewrite failure cannot overwrite an earlier response selected for
// return by the route executor.
func (s *RequestSummary) recordUpstreamAttempt(operationID, routeID, targetID, provider, kind string) {
	if s == nil {
		return
	}
	operationID = strings.TrimSpace(operationID)
	routeID = strings.TrimSpace(routeID)
	targetID = strings.TrimSpace(targetID)
	provider = strings.TrimSpace(provider)
	kind = strings.TrimSpace(kind)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.operationID == "" && operationID != "" {
		s.operationID = operationID
	}
	if s.routeID == "" && routeID != "" {
		s.routeID = routeID
	}
	if targetID != "" {
		s.lastTarget = targetID
	}
	if provider != "" {
		s.lastProvider = provider
	}
	if kind != "" {
		s.lastProviderKind = kind
	}
	s.upstreamSendCount++
}

func (s *RequestSummary) lastUpstreamAttempt() (targetID, provider, kind string) {
	if s == nil {
		return "", "", ""
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastTarget, s.lastProvider, s.lastProviderKind
}

// UpstreamSendCount returns the number of physical upstream dispatches made for
// this client request or turn.
func (s *RequestSummary) UpstreamSendCount() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upstreamSendCount
}

// RecordTargetSwitch records one selection of a distinct target in the same
// logical model route. Same-target protocol recovery is not a target switch.
func (s *RequestSummary) RecordTargetSwitch() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.targetSwitchCount++
}

// TargetSwitchCount returns the number of distinct-target switches made for
// this client request or turn.
func (s *RequestSummary) TargetSwitchCount() int64 {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.targetSwitchCount
}

// MarkRouteExhausted marks this logical operation as having exhausted its
// eligible route targets. It returns true only for the first call so aggregate
// accounting can remain one-per-operation.
func (s *RequestSummary) MarkRouteExhausted() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.routeExhausted {
		return false
	}
	s.routeExhausted = true
	return true
}

// RouteExhausted reports whether the logical operation exhausted its eligible
// route targets.
func (s *RequestSummary) RouteExhausted() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.routeExhausted
}

func (s *RequestSummary) setRoute(endpoint, model string, stream bool) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
		s.endpoint = endpoint
	}
	if model = strings.TrimSpace(model); model != "" {
		s.model = model
		s.metricModel = model
	}
	s.stream = stream
	s.streamSet = true
}

func (s *RequestSummary) setProvider(provider, kind string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if provider = strings.TrimSpace(provider); provider != "" {
		s.provider = provider
	}
	if kind = strings.TrimSpace(kind); kind != "" {
		s.providerKind = kind
	}
}

func (s *RequestSummary) setProviderModel(provider, kind string, modelKnown bool, canonicalModel string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if provider = strings.TrimSpace(provider); provider != "" {
		s.provider = provider
	}
	if kind = strings.TrimSpace(kind); kind != "" {
		s.providerKind = kind
	}
	if canonicalModel = strings.TrimSpace(canonicalModel); modelKnown && canonicalModel != "" {
		s.metricModel = canonicalModel
	}
	s.modelKnown = modelKnown
}

func (s *RequestSummary) markUpstreamAttempted() {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.upstreamAttempted = true
	s.mu.Unlock()
}

func (s *RequestSummary) setUpstreamRequestID(requestID string) {
	if s == nil {
		return
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upstreamRequestID = requestID
}

func (s *RequestSummary) setOpenAIUsage(usage *models.OpenAIUsage) {
	if s == nil || usage == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.promptTokens = summaryIntPtr(usage.PromptTokens)
	s.completionTokens = summaryIntPtr(usage.CompletionTokens)
	s.totalTokens = summaryIntPtr(usage.TotalTokens)
	if usage.PromptTokensDetails != nil {
		s.cachedTokens = summaryIntPtr(usage.PromptTokensDetails.CachedTokens)
	}
	if usage.CompletionTokensDetails != nil {
		s.reasoningTokens = summaryIntPtr(usage.CompletionTokensDetails.ReasoningTokens)
	}
}

func summaryIntPtr(v int) *int { return &v }

// setFailureStatus records an out-of-band failure status (first one wins) for a
// request whose HTTP status was already committed before the failure was known.
func (s *RequestSummary) setFailureStatus(status int) {
	if s == nil || status == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failureStatus == 0 {
		s.failureStatus = status
	}
	// A semantic provider failure is authoritative even if lifecycle transport
	// cancellation raced ahead and tentatively suppressed the request.
	s.statsSuppressed = false
}

// FailureStatus returns the out-of-band failure status, or 0 if none. The stats
// middleware prefers a non-zero value over the committed HTTP status.
func (s *RequestSummary) FailureStatus() int {
	if s == nil {
		return 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failureStatus
}

// SuppressStats excludes this request from provider traffic accounting unless a
// semantic provider failure was already observed.
func (s *RequestSummary) SuppressStats() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failureStatus == 0 {
		s.statsSuppressed = true
	}
}

// StatsSuppressed reports whether provider traffic accounting should omit this request.
func (s *RequestSummary) StatsSuppressed() bool {
	if s == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.statsSuppressed
}

func suppressRequestStats(ctx context.Context) {
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		summary.SuppressStats()
	}
}

// addInternalUsage accumulates out-of-band token spend (e.g. an internal
// compaction call) that is separate from the turn's own reported usage, so it is
// not clobbered when setOpenAIUsage overwrites the turn usage. It is additive.
func (s *RequestSummary) addInternalUsage(promptTokens, completionTokens int) {
	if s == nil || (promptTokens == 0 && completionTokens == 0) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.extraPromptTokens += promptTokens
	s.extraCompletionTokens += completionTokens
}

// observeResponseFailureStatus records an out-of-band failure status onto the
// request summary attached to ctx, if any.
func observeResponseFailureStatus(ctx context.Context, status int) {
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		summary.setFailureStatus(status)
	}
}

// observeInternalResponsesUsage accumulates out-of-band /responses token spend
// (e.g. an internal compaction call) onto the request summary attached to ctx,
// if any, so it is counted in addition to the turn's own usage.
func observeInternalResponsesUsage(ctx context.Context, usage responsesUsage) {
	if usage.isZero() {
		return
	}
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		summary.addInternalUsage(usage.InputTokens, usage.OutputTokens)
	}
}

// LoggerFields returns the structured fields populated for this request.
func (s *RequestSummary) LoggerFields() []logger.Field {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	fields := make([]logger.Field, 0, 14)
	if s.operationID != "" {
		fields = append(fields, logger.F("operation_id", s.operationID))
	}
	if s.routeID != "" {
		fields = append(fields, logger.F("route_id", s.routeID))
	}
	if s.finalTarget != "" {
		fields = append(fields, logger.F("final_target", s.finalTarget))
	}
	if s.upstreamSendCount != 0 {
		fields = append(fields, logger.F("upstream_sends", s.upstreamSendCount))
	}
	if s.targetSwitchCount != 0 {
		fields = append(fields, logger.F("target_switches", s.targetSwitchCount))
	}
	if s.routeExhausted {
		fields = append(fields, logger.F("route_exhausted", true))
	}
	if s.endpoint != "" {
		fields = append(fields, logger.F("endpoint", s.endpoint))
	}
	if s.model != "" {
		fields = append(fields, logger.F("model", s.model))
	}
	if s.provider != "" {
		fields = append(fields, logger.F("provider", s.provider))
	}
	if s.providerKind != "" {
		fields = append(fields, logger.F("provider_kind", s.providerKind))
	}
	if s.streamSet {
		fields = append(fields, logger.F("stream", s.stream))
	}
	if s.upstreamRequestID != "" {
		fields = append(fields, logger.F("upstream_request_id", s.upstreamRequestID))
	}
	if s.promptTokens != nil {
		fields = append(fields, logger.F("prompt_tokens", *s.promptTokens))
	}
	if s.completionTokens != nil {
		fields = append(fields, logger.F("completion_tokens", *s.completionTokens))
	}
	if s.totalTokens != nil {
		fields = append(fields, logger.F("total_tokens", *s.totalTokens))
	}
	if s.statsSuppressed {
		fields = append(fields, logger.F("stats_suppressed", true))
	}
	return fields
}

func (h *ProxyHandler) observeRequestSummary(ctx context.Context, endpoint, model string, stream bool, providerEndpoint string) {
	h.observeRequestSummaryWithProviderModel(ctx, endpoint, model, model, stream, providerEndpoint)
}

func (h *ProxyHandler) observeRequestSummaryWithProviderModel(ctx context.Context, endpoint, model, providerModel string, stream bool, providerEndpoint string) {
	summary := RequestSummaryFromContext(ctx)
	if summary == nil {
		return
	}
	summary.setRoute(endpoint, model, stream)
	provider, owner, modelKnown := h.resolveProviderModelForRequest(providerModel, providerEndpoint)
	if provider == nil {
		return
	}
	summary.setProviderModel(provider.id, string(provider.kind), modelKnown, owner.publicID)
}

func observeOpenAIUsage(ctx context.Context, usage *models.OpenAIUsage) {
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		summary.setOpenAIUsage(usage)
	}
}

func observeUpstreamHeaders(ctx context.Context, headers http.Header) {
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		summary.setUpstreamRequestID(UpstreamRequestID(headers))
	}
}

func observeResolvedChatRoute(summary *RequestSummary, route resolvedChatRoute) {
	if summary == nil || route.provider == nil {
		return
	}
	providerID, providerKind := route.provider.id, string(route.provider.kind)
	if route.known {
		if canonicalModel := strings.TrimSpace(route.owner.publicID); canonicalModel != "" {
			summary.setProviderModel(providerID, providerKind, true, canonicalModel)
			return
		}
	}
	// Unknown models may still be served by a permissive default provider. Keep
	// the final provider attribution without promoting a client-controlled model
	// into the bounded persistent label set.
	summary.setProvider(providerID, providerKind)
}

func observeChatExecutionRoute(ctx context.Context, result chatExecutionResult) {
	observeResolvedChatRoute(RequestSummaryFromContext(ctx), result.route)
}

func observeChatExecutionError(ctx context.Context, executionErr *chatExecutionError) {
	if executionErr == nil {
		return
	}
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		observeResolvedChatRoute(summary, executionErr.route)
		if len(executionErr.Headers) > 0 {
			summary.setUpstreamRequestID(UpstreamRequestID(executionErr.Headers))
		}
	}
}
