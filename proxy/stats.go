package proxy

import (
	"context"
	"embed"
	"encoding/json"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

//go:embed dashboard
var dashboardAssets embed.FS

const (
	statsRingSeconds   = 300
	statsSeriesSeconds = 180
	statsTopN          = 10
	statsBreakdownRows = 25
	statsMaxKeys       = 200
	statsOtherKey      = "other"
)

type statsTotals struct {
	Requests         int64 `json:"requests"`
	Errors           int64 `json:"errors"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	LatencyP50       int64 `json:"latency_p50_ms"`
	LatencyP95       int64 `json:"latency_p95_ms"`
	LatencyP99       int64 `json:"latency_p99_ms"`
}

type statsSeriesPoint struct {
	T          int64 `json:"t"`
	Req        int64 `json:"req"`
	Err        int64 `json:"err"`
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
}

type statsBreakdown struct {
	Model                string `json:"model,omitempty"`
	Provider             string `json:"provider,omitempty"`
	Kind                 string `json:"kind,omitempty"`
	Agent                string `json:"agent,omitempty"`
	Route                string `json:"route,omitempty"`
	Requests             int64  `json:"requests"`
	Tokens               int64  `json:"tokens"`
	Errors               int64  `json:"errors"`
	AvgMs                int64  `json:"avg_ms"`
	TargetSwitches       int64  `json:"target_switches,omitempty"`
	RequestsWithFailover int64  `json:"requests_with_failover,omitempty"`
	SuccessfulFailovers  int64  `json:"successful_failovers,omitempty"`
	RouteExhaustions     int64  `json:"route_exhaustions,omitempty"`
}

type statsTargetBreakdown struct {
	Route         string          `json:"route,omitempty"`
	Target        string          `json:"target"`
	Provider      string          `json:"provider,omitempty"`
	Kind          string          `json:"kind,omitempty"`
	Attempts      int64           `json:"attempts"`
	PhysicalUsage statsTokenUsage `json:"physical_usage"`
	WastedUsage   statsTokenUsage `json:"wasted_usage"`
}

type statsTokenUsage struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
}

func (u statsTokenUsage) isZero() bool {
	return u.PromptTokens == 0 && u.CompletionTokens == 0 && u.TotalTokens == 0 &&
		u.CachedTokens == 0 && u.ReasoningTokens == 0
}

func (u statsTokenUsage) normalized() statsTokenUsage {
	u.PromptTokens = max(u.PromptTokens, 0)
	u.CompletionTokens = max(u.CompletionTokens, 0)
	u.CachedTokens = max(u.CachedTokens, 0)
	u.ReasoningTokens = max(u.ReasoningTokens, 0)
	if u.TotalTokens < 0 {
		u.TotalTokens = 0
	}
	if u.TotalTokens == 0 {
		u.TotalTokens = policyStatsSaturatingAdd(u.PromptTokens, u.CompletionTokens)
	}
	return u
}

func (u *statsTokenUsage) add(other statsTokenUsage) {
	if u == nil {
		return
	}
	other = other.normalized()
	u.PromptTokens = policyStatsSaturatingAdd(u.PromptTokens, other.PromptTokens)
	u.CompletionTokens = policyStatsSaturatingAdd(u.CompletionTokens, other.CompletionTokens)
	u.TotalTokens = policyStatsSaturatingAdd(u.TotalTokens, other.TotalTokens)
	u.CachedTokens = policyStatsSaturatingAdd(u.CachedTokens, other.CachedTokens)
	u.ReasoningTokens = policyStatsSaturatingAdd(u.ReasoningTokens, other.ReasoningTokens)
}

func statsTokenUsageFromResponses(usage responsesUsage) statsTokenUsage {
	return statsTokenUsage{
		PromptTokens:     int64(usage.InputTokens),
		CompletionTokens: int64(usage.OutputTokens),
		TotalTokens:      int64(usage.totalTokens()),
		CachedTokens:     int64(usage.InputTokensDetails.CachedTokens),
		ReasoningTokens:  int64(usage.OutputTokensDetails.ReasoningTokens),
	}.normalized()
}

func statsTokenUsageDelta(current, accounted statsTokenUsage) statsTokenUsage {
	current = current.normalized()
	accounted = accounted.normalized()
	return statsTokenUsage{
		PromptTokens:     max(current.PromptTokens-accounted.PromptTokens, 0),
		CompletionTokens: max(current.CompletionTokens-accounted.CompletionTokens, 0),
		TotalTokens:      max(current.TotalTokens-accounted.TotalTokens, 0),
		CachedTokens:     max(current.CachedTokens-accounted.CachedTokens, 0),
		ReasoningTokens:  max(current.ReasoningTokens-accounted.ReasoningTokens, 0),
	}
}

type statsErrorRow struct {
	Label string `json:"label"`
	Count int64  `json:"count"`
}

type statsSnapshot struct {
	TaskUsage             taskUsageSnapshot         `json:"task_usage"`
	UptimeSeconds         int64                     `json:"uptime_seconds"`
	Inflight              int64                     `json:"inflight"`
	AuxiliaryInflight     int64                     `json:"auxiliary_inflight"`
	Totals                statsTotals               `json:"totals"`
	Status                map[string]int64          `json:"status"`
	StatusCodes           []statsErrorRow           `json:"status_codes"`
	Errors                []statsErrorRow           `json:"errors"`
	Series                []statsSeriesPoint        `json:"series"`
	ByModel               []statsBreakdown          `json:"by_model"`
	ByProvider            []statsBreakdown          `json:"by_provider"`
	ByAgent               []statsBreakdown          `json:"by_agent"`
	ByRoute               []statsBreakdown          `json:"by_route"`
	ByTarget              []statsTargetBreakdown    `json:"by_target"`
	PhysicalUsage         statsTokenUsage           `json:"physical_usage"`
	WastedUsage           statsTokenUsage           `json:"wasted_usage"`
	UpstreamAttempts      int64                     `json:"upstream_attempts"`
	TargetSwitches        int64                     `json:"target_switches"`
	RequestsWithFailover  int64                     `json:"requests_with_failover"`
	SuccessfulFailovers   int64                     `json:"successful_failovers"`
	RouteExhaustions      int64                     `json:"route_exhaustions"`
	StateBindingHits      int64                     `json:"state_binding_hits"`
	StateBindingMisses    int64                     `json:"state_binding_misses"`
	StateBindingEvictions int64                     `json:"state_binding_evictions"`
	StateBindings         stateBindingStatsSnapshot `json:"state_bindings"`
	Retries               int64                     `json:"retries"`
	RetriesByCode         []statsErrorRow           `json:"retries_by_code"`
	Recent                []recentRequest           `json:"recent"`
	RecentAttempts        []recentRouteAttempt      `json:"recent_attempts"`
	InsightsEnabled       bool                      `json:"insights_enabled"`
	PolicyRouting         policyStatsSnapshot       `json:"policy_routing"`
}

type secondBucket struct {
	sec        int64
	req        int64
	err        int64
	prompt     int64
	completion int64
}

type breakdownCounter struct {
	requests             int64
	tokens               int64
	errors               int64
	durMs                int64
	durSamples           int64
	kind                 string
	targetSwitches       int64
	requestsWithFailover int64
	successfulFailovers  int64
	routeExhaustions     int64
}

type targetAttemptCounter struct {
	route         string
	target        string
	provider      string
	kind          string
	attempts      int64
	physicalUsage statsTokenUsage
	wastedUsage   statsTokenUsage
}

type statsCollector struct {
	taskUsage   taskUsageCollector
	mu          sync.Mutex
	start       time.Time
	now         func() time.Time
	ring        []secondBucket
	totals      statsTotals
	status      map[string]int64
	statusCodes map[int]int64
	errTargets  map[string]int64
	byModel     map[string]*breakdownCounter
	byProvider  map[string]*breakdownCounter
	byAgent     map[string]*breakdownCounter
	byRoute     map[string]*breakdownCounter
	byTarget    map[string]*targetAttemptCounter

	upstreamAttempts      int64
	targetSwitches        int64
	requestsWithFailover  int64
	successfulFailovers   int64
	routeExhaustions      int64
	stateBindingHits      int64
	stateBindingMisses    int64
	stateBindingEvictions int64
	physicalUsage         statsTokenUsage
	wastedUsage           statsTokenUsage

	retries       int64
	retriesByCode map[int]int64

	recent         []recentRequest
	recentIdx      int
	recentSize     int
	recentSequence uint64

	recentAttempts        []recentRouteAttempt
	recentAttemptIdx      int
	recentAttemptSize     int
	recentAttemptSequence uint64

	latencies    []int64
	latencyIdx   int
	latencyCount int

	inflight atomic.Int64
}

type recentRequest struct {
	recordID          uint64
	T                 int64  `json:"t"`
	Endpoint          string `json:"endpoint,omitempty"`
	Model             string `json:"model,omitempty"`
	Provider          string `json:"provider,omitempty"`
	OperationID       string `json:"operation_id,omitempty"`
	RouteID           string `json:"route_id,omitempty"`
	FinalTarget       string `json:"final_target,omitempty"`
	UpstreamSends     int64  `json:"upstream_sends,omitempty"`
	TargetSwitches    int64  `json:"target_switches,omitempty"`
	Agent             string `json:"agent"`
	Status            int    `json:"status"`
	DurMs             int64  `json:"dur_ms"`
	TotalTokens       int64  `json:"total_tokens"`
	UpstreamRequestID string `json:"upstream_request_id,omitempty"`
}

type recentRouteAttempt struct {
	recordID             uint64
	state                *routeAttemptRecordState
	T                    int64                    `json:"t"`
	OperationID          string                   `json:"operation_id,omitempty"`
	RouteID              string                   `json:"route_id,omitempty"`
	TargetID             string                   `json:"target_id,omitempty"`
	ProviderID           string                   `json:"provider_id,omitempty"`
	ProviderKind         string                   `json:"provider_kind,omitempty"`
	Sequence             int                      `json:"sequence"`
	AttemptKind          routeAttemptKind         `json:"attempt_kind"`
	Status               string                   `json:"status"`
	StatusCode           int                      `json:"status_code,omitempty"`
	Outcome              routeAttemptOutcome      `json:"outcome"`
	Delivery             requestDelivery          `json:"delivery,omitempty"`
	SemanticProgress     upstreamSemanticProgress `json:"semantic_progress,omitempty"`
	DownstreamCommitment downstreamCommitment     `json:"downstream_commitment,omitempty"`
	RetryDecision        routeRetryDecision       `json:"retry_decision,omitempty"`
	TTFTMs               *int64                   `json:"ttft_ms,omitempty"`
	RetryAfterSeconds    *int64                   `json:"retry_after_seconds,omitempty"`
	UpstreamRequestID    string                   `json:"upstream_request_id,omitempty"`
	CleanupComplete      bool                     `json:"cleanup_complete"`
	ReportedUsage        *statsTokenUsage         `json:"reported_usage,omitempty"`
}

type routeAttemptRecordState struct {
	mu sync.Mutex

	collector      *statsCollector
	targetKey      string
	recentIdx      int
	recordID       uint64
	policyRedacted bool

	row                recentRouteAttempt
	physicalAccounted  statsTokenUsage
	wastedAccounted    statsTokenUsage
	traceVersion       uint64
	publicationVersion uint64

	publishedVersion  uint64
	physicalPublished statsTokenUsage
	wastedPublished   statsTokenUsage
}

type routeAttemptRecord struct {
	state *routeAttemptRecordState
}

type routeAttemptPublication struct {
	version           uint64
	row               recentRouteAttempt
	physicalAccounted statsTokenUsage
	wastedAccounted   statsTokenUsage
}

type routeAttemptCompletion struct {
	StatusCode           int
	Outcome              routeAttemptOutcome
	Delivery             requestDelivery
	SemanticProgress     upstreamSemanticProgress
	DownstreamCommitment downstreamCommitment
	RetryDecision        routeRetryDecision
	TTFTMs               *int64
	RetryAfterSeconds    *int64
	UpstreamRequestID    string
	CleanupComplete      bool
	ReportedUsage        *statsTokenUsage
	Wasted               bool
}

type responsesTurnStatsRecord struct {
	valid       bool
	sec         int64
	modelKey    string
	providerKey string
	agentKey    string
	recentIndex int
	recordID    uint64
}

const (
	statsLatencySamples      = 2048
	statsRecentRequests      = 80
	statsRecentRouteAttempts = 256
)

func newStatsCollector() *statsCollector {
	c := &statsCollector{
		now:            time.Now,
		ring:           make([]secondBucket, statsRingSeconds),
		status:         make(map[string]int64, 4),
		statusCodes:    make(map[int]int64),
		errTargets:     make(map[string]int64),
		byModel:        make(map[string]*breakdownCounter),
		byProvider:     make(map[string]*breakdownCounter),
		byAgent:        make(map[string]*breakdownCounter),
		byRoute:        make(map[string]*breakdownCounter),
		byTarget:       make(map[string]*targetAttemptCounter),
		retriesByCode:  make(map[int]int64),
		recent:         make([]recentRequest, statsRecentRequests),
		recentAttempts: make([]recentRouteAttempt, statsRecentRouteAttempts),
		latencies:      make([]int64, statsLatencySamples),
	}
	c.start = c.now()
	return c
}

func (c *statsCollector) incInflight() { c.inflight.Add(1) }
func (c *statsCollector) decInflight() { c.inflight.Add(-1) }

func (c *statsCollector) incRetry(status int) {
	c.mu.Lock()
	c.retries++
	c.retriesByCode[status]++
	c.mu.Unlock()
}

type RouteAttemptObservation struct {
	OperationID  string
	RouteID      string
	TargetID     string
	ProviderID   string
	ProviderKind string
	Sequence     int
	AttemptKind  routeAttemptKind

	operation      *routeOperation
	startedAt      time.Time
	policyRedacted bool
}

func (h *ProxyHandler) RecordUpstreamAttempt(ctx context.Context, attempt RouteAttemptObservation) *routeAttemptRecord {
	summary := RequestSummaryFromContext(ctx)
	if publicID := summary.policyPublicIDForStats(); publicID != "" {
		attempt.RouteID = publicID
		attempt.TargetID = publicID
		attempt.ProviderID = ""
		attempt.ProviderKind = ""
		attempt.policyRedacted = true
	}
	if summary != nil {
		summary.recordUpstreamAttempt(attempt.OperationID, attempt.RouteID, attempt.TargetID, attempt.ProviderID, attempt.ProviderKind)
	}
	var record *routeAttemptRecord
	if h == nil || h.stats == nil {
		record = &routeAttemptRecord{state: &routeAttemptRecordState{}}
	} else {
		record = h.stats.beginRouteAttempt(summary, attempt)
	}
	if attempt.operation != nil {
		attempt.operation.registerAttemptRecord(attempt.Sequence, record)
	}
	return record
}

func (c *statsCollector) beginRouteAttempt(summary *RequestSummary, attempt RouteAttemptObservation) *routeAttemptRecord {
	if c == nil {
		return &routeAttemptRecord{state: &routeAttemptRecordState{}}
	}

	attempt.OperationID = boundOperationalStatLabel(attempt.OperationID)
	attempt.RouteID = boundOperationalStatLabel(attempt.RouteID)
	attempt.TargetID = boundOperationalStatLabel(attempt.TargetID)
	attempt.ProviderID = boundOperationalStatLabel(attempt.ProviderID)
	attempt.ProviderKind = boundOperationalStatLabel(attempt.ProviderKind)
	if attempt.Sequence < 0 {
		attempt.Sequence = 0
	}
	if attempt.AttemptKind == "" {
		attempt.AttemptKind = routeAttemptNormal
	}
	startedAt := attempt.startedAt
	if startedAt.IsZero() {
		startedAt = c.now()
	}

	state := &routeAttemptRecordState{
		collector:      c,
		policyRedacted: attempt.policyRedacted,
	}

	c.mu.Lock()
	state.targetKey = c.recordUpstreamAttemptLocked(attempt.RouteID, attempt.TargetID, attempt.ProviderID, attempt.ProviderKind)
	if c.recentAttemptSequence == ^uint64(0) {
		c.mu.Unlock()
		panic("stats recent attempt sequence exhausted")
	}
	c.recentAttemptSequence++
	state.recentIdx = c.recentAttemptIdx
	state.recordID = c.recentAttemptSequence
	state.row = recentRouteAttempt{
		recordID:     state.recordID,
		state:        state,
		T:            startedAt.Unix(),
		OperationID:  attempt.OperationID,
		RouteID:      attempt.RouteID,
		TargetID:     attempt.TargetID,
		ProviderID:   attempt.ProviderID,
		ProviderKind: attempt.ProviderKind,
		Sequence:     attempt.Sequence,
		AttemptKind:  attempt.AttemptKind,
		Status:       string(routeAttemptOutcomeInFlight),
		Outcome:      routeAttemptOutcomeInFlight,
	}
	c.recentAttempts[state.recentIdx] = state.row
	c.recentAttemptIdx = (c.recentAttemptIdx + 1) % len(c.recentAttempts)
	if c.recentAttemptSize < len(c.recentAttempts) {
		c.recentAttemptSize++
	}
	c.mu.Unlock()

	return &routeAttemptRecord{state: state}
}

func (r *routeAttemptRecord) complete(completion routeAttemptCompletion) {
	if r == nil || r.state == nil {
		return
	}
	state := r.state
	completion.UpstreamRequestID = boundOperationalStatLabel(completion.UpstreamRequestID)
	if completion.StatusCode < 0 || completion.StatusCode > 999 {
		completion.StatusCode = 0
	}
	if completion.ReportedUsage != nil {
		normalized := completion.ReportedUsage.normalized()
		completion.ReportedUsage = &normalized
	}

	state.mu.Lock()
	row := state.row
	classificationNotWeaker := completion.Outcome == "" || routeAttemptOutcomeRank(completion.Outcome) >= routeAttemptOutcomeRank(row.Outcome)
	if completion.StatusCode != 0 && classificationNotWeaker {
		row.StatusCode = completion.StatusCode
	}
	if completion.Outcome != "" {
		row.Outcome = mergeRouteAttemptOutcome(row.Outcome, completion.Outcome)
	}
	row.Status = normalizedRouteAttemptStatus(row.StatusCode, row.Outcome)
	if completion.Delivery != "" && classificationNotWeaker {
		row.Delivery = completion.Delivery
	}
	row.SemanticProgress = mergeUpstreamSemanticProgress(row.SemanticProgress, completion.SemanticProgress)
	if routeCommitmentRank(completion.DownstreamCommitment) > routeCommitmentRank(row.DownstreamCommitment) {
		row.DownstreamCommitment = completion.DownstreamCommitment
	}
	if completion.RetryDecision != "" && (row.RetryDecision == "" || row.RetryDecision == routeRetryAccepted || completion.RetryDecision != routeRetryAccepted) {
		row.RetryDecision = completion.RetryDecision
	}
	if row.TTFTMs == nil && completion.TTFTMs != nil {
		value := max(*completion.TTFTMs, 0)
		row.TTFTMs = &value
	}
	if completion.RetryAfterSeconds != nil {
		value := max(*completion.RetryAfterSeconds, 0)
		if value > 0 {
			row.RetryAfterSeconds = &value
		}
	}
	if completion.UpstreamRequestID != "" && !state.policyRedacted {
		row.UpstreamRequestID = completion.UpstreamRequestID
	}
	row.CleanupComplete = row.CleanupComplete || completion.CleanupComplete
	if completion.ReportedUsage != nil {
		usage := completion.ReportedUsage.normalized()
		row.ReportedUsage = &usage
	}
	state.row = row

	if row.ReportedUsage != nil {
		physicalDelta := statsTokenUsageDelta(*row.ReportedUsage, state.physicalAccounted)
		state.physicalAccounted.add(physicalDelta)
		if completion.Wasted || routeAttemptOutcomeIsWasted(row.Outcome) {
			wastedDelta := statsTokenUsageDelta(*row.ReportedUsage, state.wastedAccounted)
			state.wastedAccounted.add(wastedDelta)
		}
	}
	publication := state.nextPublicationLocked(row)
	collector := state.collector
	state.mu.Unlock()

	if collector != nil {
		collector.applyRouteAttemptCompletion(state, publication)
	}
}

func (r *routeAttemptRecord) reconcileTrace(trace routeAttemptTrace, version uint64) {
	if r == nil || r.state == nil {
		return
	}
	state := r.state
	state.mu.Lock()
	if version <= state.traceVersion {
		state.mu.Unlock()
		return
	}
	state.traceVersion = version
	row := applyRouteAttemptTrace(state.row, trace)
	if state.policyRedacted {
		row.UpstreamRequestID = ""
	}
	if row.ReportedUsage != nil && routeAttemptOutcomeIsWasted(row.Outcome) {
		wastedDelta := statsTokenUsageDelta(*row.ReportedUsage, state.wastedAccounted)
		state.wastedAccounted.add(wastedDelta)
	}
	state.row = row
	publication := state.nextPublicationLocked(row)
	collector := state.collector
	state.mu.Unlock()

	if collector != nil {
		collector.applyRouteAttemptCompletion(state, publication)
	}
}

func (s *routeAttemptRecordState) nextPublicationLocked(row recentRouteAttempt) routeAttemptPublication {
	if s.publicationVersion == ^uint64(0) {
		panic("route attempt publication sequence exhausted")
	}
	s.publicationVersion++
	return routeAttemptPublication{
		version:           s.publicationVersion,
		row:               row,
		physicalAccounted: s.physicalAccounted,
		wastedAccounted:   s.wastedAccounted,
	}
}

func (c *statsCollector) applyRouteAttemptCompletion(state *routeAttemptRecordState, publication routeAttemptPublication) {
	if c == nil || state == nil || publication.version == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if publication.version <= state.publishedVersion {
		return
	}
	physicalDelta := statsTokenUsageDelta(publication.physicalAccounted, state.physicalPublished)
	wastedDelta := statsTokenUsageDelta(publication.wastedAccounted, state.wastedPublished)
	state.physicalPublished.add(physicalDelta)
	state.wastedPublished.add(wastedDelta)
	state.publishedVersion = publication.version

	c.physicalUsage.add(physicalDelta)
	c.wastedUsage.add(wastedDelta)
	if counter := c.byTarget[state.targetKey]; counter != nil {
		counter.physicalUsage.add(physicalDelta)
		counter.wastedUsage.add(wastedDelta)
	}
	if state.recentIdx >= 0 && state.recentIdx < len(c.recentAttempts) && c.recentAttempts[state.recentIdx].recordID == state.recordID {
		publication.row.state = state
		c.recentAttempts[state.recentIdx] = publication.row
	}
}

func (h *ProxyHandler) RecordTargetSwitch(ctx context.Context) {
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		summary.RecordTargetSwitch()
	}
	if h == nil || h.stats == nil {
		return
	}
	h.stats.recordTargetSwitch()
}

func (h *ProxyHandler) RecordRouteExhaustion(ctx context.Context) {
	if summary := RequestSummaryFromContext(ctx); summary != nil && !summary.MarkRouteExhausted() {
		return
	}
	if h == nil || h.stats == nil {
		return
	}
	h.stats.recordRouteExhaustion()
}

func (h *ProxyHandler) RecordStateBindingHit() {
	if h != nil && h.stats != nil {
		h.stats.recordStateBindingEvent(stateBindingHit)
	}
}

func (h *ProxyHandler) RecordStateBindingMiss() {
	if h != nil && h.stats != nil {
		h.stats.recordStateBindingEvent(stateBindingMiss)
	}
}

func (h *ProxyHandler) RecordStateBindingEviction() {
	if h != nil && h.stats != nil {
		h.stats.recordStateBindingEvent(stateBindingEviction)
	}
}

func (c *statsCollector) recordUpstreamAttempt(routeID, targetID, providerID, providerKind string) {
	if c == nil {
		return
	}
	routeID = boundOperationalStatLabel(strings.TrimSpace(routeID))
	targetID = boundOperationalStatLabel(strings.TrimSpace(targetID))
	providerID = boundOperationalStatLabel(strings.TrimSpace(providerID))
	providerKind = boundOperationalStatLabel(strings.TrimSpace(providerKind))

	c.mu.Lock()
	c.recordUpstreamAttemptLocked(routeID, targetID, providerID, providerKind)
	c.mu.Unlock()
}

func (c *statsCollector) recordUpstreamAttemptLocked(routeID, targetID, providerID, providerKind string) string {
	c.upstreamAttempts++
	if targetID == "" {
		return ""
	}
	key := capKey(c.byTarget, targetAttemptKey(routeID, targetID))
	counter := c.byTarget[key]
	if counter == nil {
		counter = &targetAttemptCounter{}
		if key == statsOtherKey {
			counter.target = statsOtherKey
		} else {
			counter.route = routeID
			counter.target = targetID
			counter.provider = providerID
			counter.kind = providerKind
		}
		c.byTarget[key] = counter
	}
	if key != statsOtherKey {
		if providerID != "" {
			counter.provider = providerID
		}
		if providerKind != "" {
			counter.kind = providerKind
		}
	}
	counter.attempts++
	return key
}

func targetAttemptKey(routeID, targetID string) string {
	return routeID + "\x00" + targetID
}

func (c *statsCollector) recordTargetSwitch() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.targetSwitches++
	c.mu.Unlock()
}

func (c *statsCollector) recordRouteExhaustion() {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.routeExhaustions++
	c.mu.Unlock()
}

type stateBindingStatsEvent uint8

const (
	stateBindingHit stateBindingStatsEvent = iota + 1
	stateBindingMiss
	stateBindingEviction
)

func (c *statsCollector) recordStateBindingEvent(event stateBindingStatsEvent) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch event {
	case stateBindingHit:
		c.stateBindingHits++
	case stateBindingMiss:
		c.stateBindingMisses++
	case stateBindingEviction:
		c.stateBindingEvictions++
	}
}

func (c *statsCollector) record(summary *RequestSummary, status int, userAgent string, dur time.Duration) {
	d := readSummaryForStats(summary)
	agent := classifyAgent(userAgent)
	durMs := dur.Milliseconds()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordLocked(d, agent, status, durMs)
}

func (c *statsCollector) recordResponsesTurn(model, provider, kind, agentLabel string, status int, usage responsesUsage, policyPublicID string, operationIDs ...string) responsesTurnStatsRecord {
	if status == 0 {
		status = http.StatusOK
	}
	operationID := ""
	if len(operationIDs) > 0 {
		operationID = strings.TrimSpace(operationIDs[0])
	}
	total := usage.totalTokens()
	d := summaryStats{
		model:       boundStatLabel(model),
		operationID: boundOperationalStatLabel(operationID),
		provider:    provider,
		kind:        kind,
		endpoint:    "responses_ws",
		stream:      true,
		prompt:      usage.InputTokens,
		completion:  usage.OutputTokens,
		total:       total,
		cached:      usage.InputTokensDetails.CachedTokens,
		reasoning:   usage.OutputTokensDetails.ReasoningTokens,
	}
	if policyPublicID = boundStatLabel(policyPublicID); policyPublicID != "" {
		d.model = policyPublicID
		d.routeID = policyPublicID
		d.finalTarget = policyPublicID
		d.provider = ""
		d.kind = ""
	}
	agent := agentLabel
	if agent == "" {
		agent = "unknown"
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.recordLocked(d, agent, status, 0)
}

func (c *statsCollector) addResponsesTurnUsage(record responsesTurnStatsRecord, usage responsesUsage) {
	if c == nil || !record.valid || usage.isZero() {
		return
	}
	total := int64(usage.totalTokens())
	prompt := int64(usage.InputTokens)
	completion := int64(usage.OutputTokens)

	c.mu.Lock()
	defer c.mu.Unlock()
	if bucket := &c.ring[ringIndex(record.sec, len(c.ring))]; bucket.sec == record.sec {
		bucket.prompt += prompt
		bucket.completion += completion
	}
	c.totals.PromptTokens += prompt
	c.totals.CompletionTokens += completion
	c.totals.TotalTokens += total
	c.totals.CachedTokens += int64(usage.InputTokensDetails.CachedTokens)
	c.totals.ReasoningTokens += int64(usage.OutputTokensDetails.ReasoningTokens)
	if record.modelKey != "" {
		if counter := c.byModel[record.modelKey]; counter != nil {
			counter.tokens += total
		}
	}
	if record.providerKey != "" {
		if counter := c.byProvider[record.providerKey]; counter != nil {
			counter.tokens += total
		}
	}
	if record.agentKey != "" {
		if counter := c.byAgent[record.agentKey]; counter != nil {
			counter.tokens += total
		}
	}
	if record.recentIndex >= 0 && record.recentIndex < len(c.recent) && c.recent[record.recentIndex].recordID == record.recordID {
		c.recent[record.recentIndex].TotalTokens += total
	}
}

func (c *statsCollector) recordLocked(d summaryStats, agent string, status int, durMs int64) responsesTurnStatsRecord {
	sec := c.now().Unix()
	isErr := status >= http.StatusBadRequest
	hasFailover := d.targetSwitches > 0
	successfulFailover := hasFailover && !isErr

	idx := ringIndex(sec, len(c.ring))
	b := &c.ring[idx]
	if b.sec != sec {
		*b = secondBucket{sec: sec}
	}
	b.req++
	if isErr {
		b.err++
	}
	b.prompt += int64(d.prompt)
	b.completion += int64(d.completion)

	c.totals.Requests++
	if isErr {
		c.totals.Errors++
	}
	c.totals.PromptTokens += int64(d.prompt)
	c.totals.CompletionTokens += int64(d.completion)
	c.totals.TotalTokens += int64(d.total)
	c.totals.CachedTokens += int64(d.cached)
	c.totals.ReasoningTokens += int64(d.reasoning)
	if hasFailover {
		c.requestsWithFailover++
		if successfulFailover {
			c.successfulFailovers++
		}
	}

	measureLatency := !d.stream
	if measureLatency {
		c.latencies[c.latencyIdx] = durMs
		c.latencyIdx = (c.latencyIdx + 1) % len(c.latencies)
		if c.latencyCount < len(c.latencies) {
			c.latencyCount++
		}
	}

	c.status[statusClass(status)]++
	if isErr {
		c.statusCodes[status]++
		c.errTargets[capKey(c.errTargets, errorTargetLabel(d.provider, d.model))]++
	}

	modelKey := ""
	if d.model != "" {
		modelKey = capKey(c.byModel, d.model)
		addBreakdown(c.byModel, modelKey, int64(d.total), "", isErr, durMs, measureLatency)
	}
	providerKey := d.provider
	if providerKey != "" {
		addBreakdown(c.byProvider, providerKey, int64(d.total), d.kind, isErr, durMs, measureLatency)
	}
	agentKey := capKey(c.byAgent, agent)
	addBreakdown(c.byAgent, agentKey, int64(d.total), "", isErr, durMs, measureLatency)
	if d.routeID != "" {
		routeKey := capKey(c.byRoute, d.routeID)
		addBreakdown(c.byRoute, routeKey, int64(d.total), "", isErr, durMs, measureLatency)
		counter := c.byRoute[routeKey]
		counter.targetSwitches += d.targetSwitches
		if hasFailover {
			counter.requestsWithFailover++
			if successfulFailover {
				counter.successfulFailovers++
			}
		}
		if d.routeExhausted {
			counter.routeExhaustions++
		}
	}

	if c.recentSequence == ^uint64(0) {
		panic("stats recent request sequence exhausted")
	}
	c.recentSequence++
	recentIndex := c.recentIdx
	c.recent[recentIndex] = recentRequest{
		recordID:          c.recentSequence,
		T:                 sec,
		Endpoint:          d.endpoint,
		Model:             d.model,
		Provider:          d.provider,
		OperationID:       d.operationID,
		RouteID:           d.routeID,
		FinalTarget:       d.finalTarget,
		UpstreamSends:     d.upstreamSends,
		TargetSwitches:    d.targetSwitches,
		Agent:             agent,
		Status:            status,
		DurMs:             durMs,
		TotalTokens:       int64(d.total),
		UpstreamRequestID: d.upstreamID,
	}
	c.recentIdx = (c.recentIdx + 1) % len(c.recent)
	if c.recentSize < len(c.recent) {
		c.recentSize++
	}
	return responsesTurnStatsRecord{
		valid:       true,
		sec:         sec,
		modelKey:    modelKey,
		providerKey: providerKey,
		agentKey:    agentKey,
		recentIndex: recentIndex,
		recordID:    c.recentSequence,
	}
}

func capKey[V any](m map[string]V, key string) string {
	if _, ok := m[key]; ok {
		return key
	}
	if len(m) >= statsMaxKeys {
		return statsOtherKey
	}
	return key
}

func errorTargetLabel(provider, model string) string {
	switch {
	case provider != "" && model != "":
		return provider + " / " + model
	case model != "":
		return model
	case provider != "":
		return provider
	}
	return "unrouted"
}

func (c *statsCollector) snapshot() statsSnapshot {
	now := c.now()
	nowSec := now.Unix()

	c.mu.Lock()
	defer c.mu.Unlock()

	series := make([]statsSeriesPoint, statsSeriesSeconds)
	for i := 0; i < statsSeriesSeconds; i++ {
		sec := nowSec - int64(statsSeriesSeconds-1-i)
		p := statsSeriesPoint{T: sec}
		b := c.ring[ringIndex(sec, len(c.ring))]
		if b.sec == sec {
			p.Req = b.req
			p.Err = b.err
			p.Prompt = b.prompt
			p.Completion = b.completion
		}
		series[i] = p
	}

	status := make(map[string]int64, len(c.status))
	for k, v := range c.status {
		status[k] = v
	}

	totals := c.totals
	totals.LatencyP50, totals.LatencyP95, totals.LatencyP99 = c.latencyPercentiles()

	return statsSnapshot{
		TaskUsage:             c.taskUsage.snapshot(),
		UptimeSeconds:         int64(now.Sub(c.start).Seconds()),
		Inflight:              c.inflight.Load(),
		Totals:                totals,
		Status:                status,
		StatusCodes:           topStatusCodes(c.statusCodes),
		Errors:                topErrorTargets(c.errTargets),
		Series:                series,
		ByModel:               topBreakdowns(c.byModel, breakdownKindModel),
		ByProvider:            topBreakdowns(c.byProvider, breakdownKindProvider),
		ByAgent:               topBreakdowns(c.byAgent, breakdownKindAgent),
		ByRoute:               topBreakdowns(c.byRoute, breakdownKindRoute),
		ByTarget:              topTargetBreakdowns(c.byTarget),
		PhysicalUsage:         c.physicalUsage,
		WastedUsage:           c.wastedUsage,
		UpstreamAttempts:      c.upstreamAttempts,
		TargetSwitches:        c.targetSwitches,
		RequestsWithFailover:  c.requestsWithFailover,
		SuccessfulFailovers:   c.successfulFailovers,
		RouteExhaustions:      c.routeExhaustions,
		StateBindingHits:      c.stateBindingHits,
		StateBindingMisses:    c.stateBindingMisses,
		StateBindingEvictions: c.stateBindingEvictions,
		Retries:               c.retries,
		RetriesByCode:         retriesRows(c.retriesByCode),
		Recent:                c.recentSnapshot(),
		RecentAttempts:        c.recentAttemptsSnapshot(),
	}
}

func retriesRows(m map[int]int64) []statsErrorRow {
	out := make([]statsErrorRow, 0, len(m))
	for code, count := range m {
		label := strconv.Itoa(code)
		if code == 0 {
			label = "transport"
		}
		out = append(out, statsErrorRow{Label: label, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	if len(out) > statsTopN {
		out = out[:statsTopN]
	}
	return out
}

func (c *statsCollector) recentSnapshot() []recentRequest {
	out := make([]recentRequest, 0, c.recentSize)
	for i := 0; i < c.recentSize; i++ {
		idx := (c.recentIdx - 1 - i + len(c.recent)) % len(c.recent)
		out = append(out, c.recent[idx])
	}
	return out
}

func (c *statsCollector) recentAttemptsSnapshot() []recentRouteAttempt {
	out := make([]recentRouteAttempt, 0, c.recentAttemptSize)
	for i := 0; i < c.recentAttemptSize; i++ {
		idx := (c.recentAttemptIdx - 1 - i + len(c.recentAttempts)) % len(c.recentAttempts)
		row := c.recentAttempts[idx]
		row.state = nil
		row.recordID = 0
		out = append(out, row)
	}
	return out
}

func applyRouteAttemptTrace(row recentRouteAttempt, trace routeAttemptTrace) recentRouteAttempt {
	traceOutcome := routeAttemptOutcomeFromTrace(trace)
	diagnosticsNotWeaker := routeAttemptDiagnosticsCompare(
		traceOutcome, trace.Progress, trace.StatusCode,
		row.Outcome, row.SemanticProgress, row.StatusCode,
	) >= 0
	if trace.StatusCode != 0 && diagnosticsNotWeaker {
		row.StatusCode = trace.StatusCode
	}
	if trace.Delivery != "" && diagnosticsNotWeaker {
		row.Delivery = trace.Delivery
	}
	row.SemanticProgress = mergeUpstreamSemanticProgress(row.SemanticProgress, trace.Progress)
	if routeCommitmentRank(trace.Commitment) > routeCommitmentRank(row.DownstreamCommitment) {
		row.DownstreamCommitment = trace.Commitment
	}
	if trace.Decision != "" && (row.RetryDecision == "" || row.RetryDecision == routeRetryAccepted || trace.Decision != routeRetryAccepted) {
		row.RetryDecision = trace.Decision
	}
	if trace.UpstreamID != "" && diagnosticsNotWeaker {
		row.UpstreamRequestID = boundOperationalStatLabel(trace.UpstreamID)
	}
	row.CleanupComplete = row.CleanupComplete || trace.CleanupDone
	row.Outcome = mergeRouteAttemptOutcome(row.Outcome, traceOutcome)
	row.Status = normalizedRouteAttemptStatus(row.StatusCode, row.Outcome)
	return row
}

func routeAttemptDiagnosticsCompare(leftOutcome routeAttemptOutcome, leftProgress upstreamSemanticProgress, leftStatus int, rightOutcome routeAttemptOutcome, rightProgress upstreamSemanticProgress, rightStatus int) int {
	if left, right := routeAttemptOutcomeRank(leftOutcome), routeAttemptOutcomeRank(rightOutcome); left != right {
		return left - right
	}
	if left, right := routeAttemptProgressDiagnosticRank(leftProgress), routeAttemptProgressDiagnosticRank(rightProgress); left != right {
		return left - right
	}
	return routeAttemptStatusDiagnosticRank(leftStatus) - routeAttemptStatusDiagnosticRank(rightStatus)
}

func routeAttemptProgressDiagnosticRank(progress upstreamSemanticProgress) int {
	switch progress {
	case upstreamProgressUnknown:
		return 6
	case upstreamProgressToolActivity:
		return 5
	case upstreamProgressSemanticOutput:
		return 4
	case upstreamProgressTerminalSuccess:
		return 3
	case upstreamProgressTerminalFailure:
		return 2
	case upstreamProgressAllowedPreamble:
		return 1
	default:
		return 0
	}
}

func routeAttemptStatusDiagnosticRank(status int) int {
	switch {
	case status >= http.StatusBadRequest:
		return 2
	case status > 0:
		return 1
	default:
		return 0
	}
}

func (c *statsCollector) latencyPercentiles() (p50, p95, p99 int64) {
	if c.latencyCount == 0 {
		return 0, 0, 0
	}
	sample := make([]int64, c.latencyCount)
	copy(sample, c.latencies[:c.latencyCount])
	sort.Slice(sample, func(i, j int) bool { return sample[i] < sample[j] })
	return percentile(sample, 0.50), percentile(sample, 0.95), percentile(sample, 0.99)
}

func percentile(sorted []int64, q float64) int64 {
	n := len(sorted)
	if n == 0 {
		return 0
	}
	rank := int(q*float64(n-1) + 0.5)
	if rank < 0 {
		rank = 0
	}
	if rank >= n {
		rank = n - 1
	}
	return sorted[rank]
}

func topStatusCodes(m map[int]int64) []statsErrorRow {
	out := make([]statsErrorRow, 0, len(m))
	for code, count := range m {
		out = append(out, statsErrorRow{Label: strconv.Itoa(code), Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	if len(out) > statsTopN {
		out = out[:statsTopN]
	}
	return out
}

func topErrorTargets(m map[string]int64) []statsErrorRow {
	out := make([]statsErrorRow, 0, len(m))
	for label, count := range m {
		out = append(out, statsErrorRow{Label: label, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Label < out[j].Label
	})
	if len(out) > statsTopN {
		out = out[:statsTopN]
	}
	return out
}

func topTargetBreakdowns(m map[string]*targetAttemptCounter) []statsTargetBreakdown {
	out := make([]statsTargetBreakdown, 0, len(m))
	for _, counter := range m {
		if counter == nil {
			continue
		}
		out = append(out, statsTargetBreakdown{
			Route:         counter.route,
			Target:        counter.target,
			Provider:      counter.provider,
			Kind:          counter.kind,
			Attempts:      counter.attempts,
			PhysicalUsage: counter.physicalUsage,
			WastedUsage:   counter.wastedUsage,
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Attempts != out[j].Attempts {
			return out[i].Attempts > out[j].Attempts
		}
		if out[i].Route != out[j].Route {
			return out[i].Route < out[j].Route
		}
		if out[i].Target != out[j].Target {
			return out[i].Target < out[j].Target
		}
		return out[i].Provider < out[j].Provider
	})
	if len(out) > statsBreakdownRows {
		out = out[:statsBreakdownRows]
	}
	return out
}

func ringIndex(sec int64, n int) int {
	if n <= 0 {
		return 0
	}
	return int(((sec % int64(n)) + int64(n)) % int64(n))
}

func statusClass(status int) string {
	switch {
	case status >= 500:
		return "5xx"
	case status >= 400:
		return "4xx"
	case status >= 300:
		return "3xx"
	default:
		return "2xx"
	}
}

func addBreakdown(m map[string]*breakdownCounter, key string, tokens int64, kind string, isErr bool, durMs int64, measureLatency bool) {
	e := m[key]
	if e == nil {
		e = &breakdownCounter{}
		m[key] = e
	}
	e.requests++
	e.tokens += tokens
	if measureLatency {
		e.durMs += durMs
		e.durSamples++
	}
	if isErr {
		e.errors++
	}
	if kind != "" {
		e.kind = kind
	}
}

const (
	breakdownKindModel    = "model"
	breakdownKindProvider = "provider"
	breakdownKindAgent    = "agent"
	breakdownKindRoute    = "route"
)

func topBreakdowns(m map[string]*breakdownCounter, kind string) []statsBreakdown {
	out := make([]statsBreakdown, 0, len(m))
	for key, e := range m {
		b := statsBreakdown{
			Requests:             e.requests,
			Tokens:               e.tokens,
			Errors:               e.errors,
			TargetSwitches:       e.targetSwitches,
			RequestsWithFailover: e.requestsWithFailover,
			SuccessfulFailovers:  e.successfulFailovers,
			RouteExhaustions:     e.routeExhaustions,
		}
		if e.durSamples > 0 {
			b.AvgMs = e.durMs / e.durSamples
		}
		switch kind {
		case breakdownKindModel:
			b.Model = key
		case breakdownKindProvider:
			b.Provider = key
			b.Kind = e.kind
		case breakdownKindAgent:
			b.Agent = key
		case breakdownKindRoute:
			b.Route = key
		}
		out = append(out, b)
	}
	sortBreakdownsByRequests(out)
	if len(out) <= statsBreakdownRows {
		return out
	}
	return unionTopBreakdowns(out, statsBreakdownRows)
}

func sortBreakdownsByRequests(out []statsBreakdown) {
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		if out[i].Tokens != out[j].Tokens {
			return out[i].Tokens > out[j].Tokens
		}
		return breakdownLabel(out[i]) < breakdownLabel(out[j])
	})
}

func unionTopBreakdowns(rows []statsBreakdown, n int) []statsBreakdown {
	if n <= 0 || len(rows) == 0 {
		return nil
	}
	metrics := []func(statsBreakdown) int64{
		func(b statsBreakdown) int64 { return b.Requests },
		func(b statsBreakdown) int64 { return b.Tokens },
		func(b statsBreakdown) int64 { return b.Errors },
		func(b statsBreakdown) int64 { return b.AvgMs },
		func(b statsBreakdown) int64 { return b.TargetSwitches },
		func(b statsBreakdown) int64 { return b.RequestsWithFailover },
		func(b statsBreakdown) int64 { return b.SuccessfulFailovers },
		func(b statsBreakdown) int64 { return b.RouteExhaustions },
	}
	keep := make(map[string]struct{}, n)
	ordered := make([]statsBreakdown, len(rows))
	copy(ordered, rows)
	for _, metric := range metrics {
		sort.SliceStable(ordered, func(i, j int) bool { return metric(ordered[i]) > metric(ordered[j]) })
		added := 0
		for _, r := range ordered {
			if added >= n {
				break
			}
			if metric(r) == 0 {
				break
			}
			keep[breakdownLabel(r)] = struct{}{}
			added++
		}
	}
	result := make([]statsBreakdown, 0, len(keep))
	for _, r := range rows {
		if _, ok := keep[breakdownLabel(r)]; ok {
			result = append(result, r)
		}
	}
	return result
}

func breakdownLabel(b statsBreakdown) string {
	switch {
	case b.Model != "":
		return b.Model
	case b.Provider != "":
		return b.Provider
	case b.Agent != "":
		return b.Agent
	case b.Route != "":
		return b.Route
	}
	return ""
}

type summaryStats struct {
	model          string
	provider       string
	kind           string
	endpoint       string
	upstreamID     string
	operationID    string
	routeID        string
	finalTarget    string
	upstreamSends  int64
	targetSwitches int64
	routeExhausted bool
	stream         bool
	prompt         int
	completion     int
	total          int
	cached         int
	reasoning      int
}

func readSummaryForStats(summary *RequestSummary) summaryStats {
	var d summaryStats
	if summary == nil {
		return d
	}
	summary.mu.Lock()
	defer summary.mu.Unlock()
	d.model = boundStatLabel(summary.model)
	d.provider = boundOperationalStatLabel(summary.provider)
	d.kind = boundOperationalStatLabel(summary.providerKind)
	d.endpoint = summary.endpoint
	d.upstreamID = summary.upstreamRequestID
	d.operationID = boundOperationalStatLabel(summary.operationID)
	d.routeID = boundOperationalStatLabel(summary.routeID)
	d.finalTarget = boundOperationalStatLabel(summary.finalTarget)
	if publicID := boundStatLabel(summary.policyPublicIDForStatsLocked()); publicID != "" {
		d.model = publicID
		d.routeID = publicID
		d.finalTarget = publicID
		d.provider = ""
		d.kind = ""
		d.upstreamID = ""
	}
	d.upstreamSends = summary.upstreamSendCount
	d.targetSwitches = summary.targetSwitchCount
	d.routeExhausted = summary.routeExhausted
	d.stream = summary.stream
	if summary.promptTokens != nil {
		d.prompt = *summary.promptTokens
	}
	if summary.completionTokens != nil {
		d.completion = *summary.completionTokens
	}
	if summary.totalTokens != nil {
		d.total = *summary.totalTokens
	}
	if d.total == 0 {
		d.total = d.prompt + d.completion
	}
	if summary.extraPromptTokens != 0 || summary.extraCompletionTokens != 0 {
		d.prompt += summary.extraPromptTokens
		d.completion += summary.extraCompletionTokens
		d.total += summary.extraPromptTokens + summary.extraCompletionTokens
	}
	if summary.cachedTokens != nil {
		d.cached = *summary.cachedTokens
	}
	if summary.reasoningTokens != nil {
		d.reasoning = *summary.reasoningTokens
	}
	return d
}

func classifyAgent(userAgent string) string {
	ua := strings.TrimSpace(userAgent)
	if ua == "" {
		return "unknown"
	}
	lower := strings.ToLower(ua)
	switch {
	case strings.Contains(lower, "antigravity"):
		return "Antigravity CLI"
	case strings.Contains(lower, "claude"):
		return "Claude Code"
	case strings.Contains(lower, "codex"):
		return "Codex CLI"
	case strings.Contains(lower, "gemini"):
		return "Gemini CLI"
	case strings.Contains(lower, "copilot"):
		return "GitHub Copilot"
	case strings.Contains(lower, "curl"):
		return "curl"
	case strings.Contains(lower, "python"):
		return "python"
	case strings.Contains(lower, "node"):
		return "node"
	}
	token := ua
	if i := strings.IndexAny(token, "/ "); i > 0 {
		token = token[:i]
	}
	if token == "" {
		return "unknown"
	}
	return boundStatLabel(token)
}

const statLabelMaxLen = 64

const statOperationalLabelMaxLen = 128

func boundStatLabel(s string) string {
	return boundStatLabelRunes(s, statLabelMaxLen)
}

func boundOperationalStatLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	return boundStatLabelRunes(s, statOperationalLabelMaxLen)
}

func boundStatLabelRunes(s string, maxLen int) string {
	if maxLen <= 0 {
		return ""
	}
	if len(s) <= maxLen {
		return s
	}
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return string(r[:maxLen]) + "…"
}

func isInferenceRoute(method, path string) bool {
	switch method {
	case http.MethodPost:
		switch path {
		case "/v1/messages",
			"/v1/chat/completions",
			"/v1/responses",
			copilotEndpointDecisions:
			return true
		}
		if isGeminiModelsPath(path) {
			return strings.HasSuffix(path, ":generateContent") ||
				strings.HasSuffix(path, ":streamGenerateContent")
		}
		return false
	default:
		return false
	}
}

func isGeminiModelsPath(path string) bool {
	return strings.HasPrefix(path, "/v1beta/models/") ||
		strings.HasPrefix(path, "/v1/models/") ||
		strings.HasPrefix(path, "/models/")
}

type retryStatsTrackedContextKey struct{}

func markRetryStatsTracked(ctx context.Context) context.Context {
	if ctx == nil {
		return context.WithValue(context.Background(), retryStatsTrackedContextKey{}, true)
	}
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		summary.retryStatsTracked.Store(true)
		return ctx
	}
	return context.WithValue(ctx, retryStatsTrackedContextKey{}, true)
}

func isRetryStatsTracked(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if summary := RequestSummaryFromContext(ctx); summary != nil && summary.retryStatsTracked.Load() {
		return true
	}
	v, _ := ctx.Value(retryStatsTrackedContextKey{}).(bool)
	return v
}

func (h *ProxyHandler) MarkRetryStatsTrackedIfInference(ctx context.Context, method, path string) context.Context {
	if h == nil || h.stats == nil || !isInferenceRoute(method, path) {
		return ctx
	}
	return markRetryStatsTracked(ctx)
}

func (h *ProxyHandler) TracksRequest(method, path string) bool {
	if h == nil || h.stats == nil {
		return false
	}
	return isInferenceRoute(method, path)
}

func (h *ProxyHandler) IncInflight() {
	if h != nil && h.stats != nil {
		h.stats.incInflight()
	}
}

func (h *ProxyHandler) DecInflight() {
	if h != nil && h.stats != nil {
		h.stats.decInflight()
	}
}

func (h *ProxyHandler) RecordRequest(summary *RequestSummary, status int, userAgent string, dur time.Duration) {
	if h == nil || h.stats == nil || (summary != nil && summary.StatsSuppressed()) {
		return
	}
	h.stats.record(summary, status, userAgent, dur)
}

func (h *ProxyHandler) RecordResponsesTurn(model, provider, kind, agentLabel string, status int, usage responsesUsage, operationIDs ...string) responsesTurnStatsRecord {
	if h == nil || h.stats == nil {
		return responsesTurnStatsRecord{}
	}
	policyPublicID := ""
	if publicID, ok := h.policyPublicModelID(model); ok {
		policyPublicID = publicID
		model = publicID
		provider = ""
		kind = ""
	}
	return h.stats.recordResponsesTurn(model, provider, kind, agentLabel, status, usage, policyPublicID, operationIDs...)
}

func (h *ProxyHandler) AddResponsesTurnUsage(record responsesTurnStatsRecord, usage responsesUsage) {
	if h == nil || h.stats == nil {
		return
	}
	h.stats.addResponsesTurnUsage(record, usage)
}

func (h *ProxyHandler) HandleStatsJSON(w http.ResponseWriter, r *http.Request) {
	var snap statsSnapshot
	if h != nil && h.stats != nil {
		inflight := h.stats.inflight.Load()
		h.lifecycleWorkersMu.Lock()
		auxiliaryInflight := int64(h.lifecycleWorkersActive)
		h.lifecycleWorkersMu.Unlock()
		snap = h.stats.snapshot()
		snap.Inflight = inflight
		snap.AuxiliaryInflight = auxiliaryInflight
	}
	snap.PolicyRouting = emptyPolicyStatsSnapshot()
	snap.StateBindings = (*stateBindingStore)(nil).dashboardStats()
	if h != nil {
		snap.StateBindings = h.stateBindings.dashboardStats()
		snap.InsightsEnabled = strings.TrimSpace(h.providersConfig.InsightModel) != ""
		if controller, ok := h.policyRoutingController.(*chatPolicyRoutingController); ok {
			snap.PolicyRouting = controller.PolicyStatsSnapshot()
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(snap)
}

func (h *ProxyHandler) HandleDashboard(w http.ResponseWriter, r *http.Request) {
	serveDashboardFile(w, "dashboard/dashboard.html", "text/html; charset=utf-8", false)
}

func (h *ProxyHandler) HandleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusNoContent)
}

func (h *ProxyHandler) HandleDashboardAsset(w http.ResponseWriter, r *http.Request) {
	switch path.Base(r.URL.Path) {
	case "uPlot.min.js":
		serveDashboardFile(w, "dashboard/uPlot.min.js", "text/javascript; charset=utf-8", true)
	case "uPlot.min.css":
		serveDashboardFile(w, "dashboard/uPlot.min.css", "text/css; charset=utf-8", true)
	default:
		http.NotFound(w, r)
	}
}

func serveDashboardFile(w http.ResponseWriter, name, contentType string, cacheable bool) {
	data, err := dashboardAssets.ReadFile(name)
	if err != nil {
		http.Error(w, "asset not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", contentType)
	if cacheable {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	} else {
		w.Header().Set("Cache-Control", "no-store")
	}
	_, _ = w.Write(data)
}
