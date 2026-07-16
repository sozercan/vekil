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

// dashboardAssets holds the embedded browser dashboard (HTML + vendored uPlot).
//
//go:embed dashboard
var dashboardAssets embed.FS

const (
	// statsRingSeconds is the size of the per-second ring buffer. It is kept a
	// bit larger than the returned window so reads never collide with a write
	// at the wrap boundary.
	statsRingSeconds = 300
	// statsSeriesSeconds is the length of the time series returned to the
	// dashboard (most recent N seconds, time-aligned and zero-filled).
	statsSeriesSeconds = 180
	// statsTopN caps the number of rows returned per error list (status codes
	// and error targets).
	statsTopN = 10
	// statsBreakdownRows caps the rows returned per model/provider/agent
	// breakdown. It is larger than statsTopN so the dashboard can re-sort or
	// filter client-side (e.g. rank by errors) and still see rows that fall
	// outside the top-by-requests.
	statsBreakdownRows = 25
	// statsMaxKeys bounds the cardinality of each breakdown map so a client
	// sending random model names or User-Agents cannot grow memory without
	// limit. Keys beyond the cap fold into an "other" bucket.
	statsMaxKeys = 200
	// statsOtherKey is the catch-all label for overflow breakdown keys.
	statsOtherKey = "other"
)

// statsTotals is the cumulative session counters.
type statsTotals struct {
	Requests         int64 `json:"requests"`
	Errors           int64 `json:"errors"`
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
	CachedTokens     int64 `json:"cached_tokens"`
	ReasoningTokens  int64 `json:"reasoning_tokens"`
	// Latency percentiles in milliseconds over a bounded recent sample.
	LatencyP50 int64 `json:"latency_p50_ms"`
	LatencyP95 int64 `json:"latency_p95_ms"`
	LatencyP99 int64 `json:"latency_p99_ms"`
}

// statsSeriesPoint is one second of traffic in the live time series.
type statsSeriesPoint struct {
	T          int64 `json:"t"`
	Req        int64 `json:"req"`
	Err        int64 `json:"err"`
	Prompt     int64 `json:"prompt"`
	Completion int64 `json:"completion"`
}

// statsBreakdown is one row in a by-model / by-provider / by-agent list. Only
// the label field relevant to the list is populated (omitempty hides the rest).
type statsBreakdown struct {
	Model    string `json:"model,omitempty"`
	Provider string `json:"provider,omitempty"`
	Kind     string `json:"kind,omitempty"`
	Agent    string `json:"agent,omitempty"`
	Requests int64  `json:"requests"`
	Tokens   int64  `json:"tokens"`
	Errors   int64  `json:"errors"`
	AvgMs    int64  `json:"avg_ms"`
}

// statsErrorRow is one row in the error-by-status or error-by-target lists.
type statsErrorRow struct {
	Label string `json:"label"`
	Count int64  `json:"count"`
}

// statsSnapshot is the payload served at GET /stats.json.
type statsSnapshot struct {
	UptimeSeconds int64              `json:"uptime_seconds"`
	Inflight      int64              `json:"inflight"`
	Totals        statsTotals        `json:"totals"`
	Status        map[string]int64   `json:"status"`
	StatusCodes   []statsErrorRow    `json:"status_codes"`
	Errors        []statsErrorRow    `json:"errors"`
	Series        []statsSeriesPoint `json:"series"`
	ByModel       []statsBreakdown   `json:"by_model"`
	ByProvider    []statsBreakdown   `json:"by_provider"`
	ByAgent       []statsBreakdown   `json:"by_agent"`
	// Retries is the total upstream retry attempts and the breakdown by the
	// status that triggered them (surfaces flakiness the proxy absorbed).
	Retries       int64           `json:"retries"`
	RetriesByCode []statsErrorRow `json:"retries_by_code"`
	// Recent is the most-recent completed requests (newest first) for drill-down.
	Recent []recentRequest `json:"recent"`
	// InsightsEnabled reports whether a model is configured for on-demand
	// LLM-generated insights (drives the dashboard's "Generate insights" button).
	InsightsEnabled bool `json:"insights_enabled"`
}

type secondBucket struct {
	sec        int64
	req        int64
	err        int64
	prompt     int64
	completion int64
}

type breakdownCounter struct {
	requests   int64
	tokens     int64
	errors     int64
	durMs      int64 // sum of request durations in ms, for avg latency
	durSamples int64 // count of requests that contributed a duration (non-stream)
	kind       string
}

// statsCollector aggregates per-request traffic in memory for the dashboard.
// It is written on every tracked request and read by the dashboard poll; the
// per-second series uses a lazy ring buffer so no background goroutine is
// needed. The clock is injectable for deterministic tests.
type statsCollector struct {
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

	// retries counts upstream retry attempts the proxy made (transient 429/503,
	// transport errors). These are usually invisible to clients because the
	// retry succeeds, so they surface flakiness that the error counters miss.
	retries       int64
	retriesByCode map[int]int64 // upstream status that triggered the retry (0 = transport error)

	// recent is a bounded ring of the most recent completed requests for the
	// drill-down log. Newest writes overwrite oldest.
	recent         []recentRequest
	recentIdx      int
	recentSize     int
	recentSequence uint64

	// latencies is a bounded ring of recent request durations (ms) used to
	// compute p50/p95/p99 without retaining every sample forever.
	latencies    []int64
	latencyIdx   int
	latencyCount int

	inflight atomic.Int64
}

// recentRequest is one row in the recent-requests drill-down log.
type recentRequest struct {
	recordID          uint64
	T                 int64  `json:"t"`
	Endpoint          string `json:"endpoint,omitempty"`
	Model             string `json:"model,omitempty"`
	Provider          string `json:"provider,omitempty"`
	Agent             string `json:"agent"`
	Status            int    `json:"status"`
	DurMs             int64  `json:"dur_ms"`
	TotalTokens       int64  `json:"total_tokens"`
	UpstreamRequestID string `json:"upstream_request_id,omitempty"`
}

// responsesTurnStatsRecord identifies the aggregate buckets updated by one
// websocket create turn. It lets post-terminal internal compaction add token
// usage without incrementing request/error counts or attributing the delta to a
// provider/model that changed after the upstream route was selected.
type responsesTurnStatsRecord struct {
	valid       bool
	sec         int64
	modelKey    string
	providerKey string
	agentKey    string
	recentIndex int
	recordID    uint64
	metrics     responsesTurnMetricsRecord
}

const (
	// statsLatencySamples bounds the recent-latency reservoir used for percentiles.
	statsLatencySamples = 2048
	// statsRecentRequests bounds the recent-requests drill-down log.
	statsRecentRequests = 80
)

func newStatsCollector() *statsCollector {
	c := &statsCollector{
		now:           time.Now,
		ring:          make([]secondBucket, statsRingSeconds),
		status:        make(map[string]int64, 4),
		statusCodes:   make(map[int]int64),
		errTargets:    make(map[string]int64),
		byModel:       make(map[string]*breakdownCounter),
		byProvider:    make(map[string]*breakdownCounter),
		byAgent:       make(map[string]*breakdownCounter),
		retriesByCode: make(map[int]int64),
		recent:        make([]recentRequest, statsRecentRequests),
		latencies:     make([]int64, statsLatencySamples),
	}
	c.start = c.now()
	return c
}

func (c *statsCollector) incInflight() { c.inflight.Add(1) }
func (c *statsCollector) decInflight() { c.inflight.Add(-1) }

// incRetry records one upstream retry attempt. status is the upstream status
// that triggered it, or 0 for a transport-level error.
func (c *statsCollector) incRetry(status int) {
	c.mu.Lock()
	c.retries++
	c.retriesByCode[status]++
	c.mu.Unlock()
}

// record folds one completed HTTP request into the aggregates.
func (c *statsCollector) record(summary *RequestSummary, status int, userAgent string, dur time.Duration) {
	d := readSummaryForStats(summary)
	agent := classifyAgent(userAgent)
	durMs := dur.Milliseconds()

	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordLocked(d, agent, status, durMs)
}

// recordResponsesTurn folds one client /v1/responses websocket create turn into
// the aggregates. The bridge does not flow through the HTTP request middleware
// (one upgrade serves many turns), so turns are recorded here directly. Internal
// compaction usage available before the terminal event is folded into usage;
// post-terminal compaction amends the returned record without another request.
// status is the client turn outcome (200 for completed,
// or an upstream error status for failed/non-200) so failures show up in error
// counts and the recent log. Streamed turns carry no latency sample. Every turn
// is counted as one request even with zero/absent usage, matching the HTTP path.
func (c *statsCollector) recordResponsesTurn(model, provider, kind, agentLabel string, status int, usage responsesUsage) responsesTurnStatsRecord {
	total := usage.totalTokens()
	d := summaryStats{
		model:      boundStatLabel(model),
		provider:   provider,
		kind:       kind,
		endpoint:   "responses_ws",
		stream:     true, // streamed: excluded from latency
		prompt:     usage.InputTokens,
		completion: usage.OutputTokens,
		total:      total,
		cached:     usage.InputTokensDetails.CachedTokens,
		reasoning:  usage.OutputTokensDetails.ReasoningTokens,
	}
	agent := agentLabel
	if agent == "" {
		agent = "unknown"
	}
	if status == 0 {
		status = http.StatusOK
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	return c.recordLocked(d, agent, status, 0)
}

// addResponsesTurnUsage adds post-terminal internal usage to an already-recorded
// websocket turn without changing request, error, status, or latency counters.
// If the time-series or recent-request slot has since wrapped, cumulative and
// still-addressable breakdown totals remain correct without corrupting a newer
// slot.
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

// recordLocked folds one already-resolved request/turn into the aggregates.
// Caller must hold c.mu. agent is the already-classified label.
func (c *statsCollector) recordLocked(d summaryStats, agent string, status int, durMs int64) responsesTurnStatsRecord {
	sec := c.now().Unix()
	isErr := status >= http.StatusBadRequest

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

	// Latency: only non-streaming requests carry a meaningful end-to-end
	// duration. A streamed response (SSE chat, or a GET /v1/responses websocket
	// session) has a wall-clock time dominated by how long the client kept the
	// connection open, which would poison the percentiles and per-key averages.
	measureLatency := !d.stream
	if measureLatency {
		// Bounded latency reservoir (overwrite oldest once full).
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
		// Providers are configured, not client-controlled, so no cap needed.
		addBreakdown(c.byProvider, providerKey, int64(d.total), d.kind, isErr, durMs, measureLatency)
	}
	agentKey := capKey(c.byAgent, agent)
	addBreakdown(c.byAgent, agentKey, int64(d.total), "", isErr, durMs, measureLatency)

	// Append to the recent-requests drill-down ring (newest overwrites oldest).
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

// capKey folds an overflow key into the "other" bucket once a map reaches the
// cardinality cap. Existing keys always pass through, so a bounded set of
// distinct keys is never disturbed.
func capKey[V any](m map[string]V, key string) string {
	if _, ok := m[key]; ok {
		return key
	}
	if len(m) >= statsMaxKeys {
		return statsOtherKey
	}
	return key
}

// errorTargetLabel names the provider/model an error is attributed to, falling
// back gracefully when one or both are unknown (e.g. a request rejected before
// routing resolves a provider).
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

// snapshot builds the dashboard payload: a time-aligned, zero-filled series plus
// cumulative totals and sorted breakdowns.
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
		UptimeSeconds: int64(now.Sub(c.start).Seconds()),
		Inflight:      c.inflight.Load(),
		Totals:        totals,
		Status:        status,
		StatusCodes:   topStatusCodes(c.statusCodes),
		Errors:        topErrorTargets(c.errTargets),
		Series:        series,
		ByModel:       topBreakdowns(c.byModel, breakdownKindModel),
		ByProvider:    topBreakdowns(c.byProvider, breakdownKindProvider),
		ByAgent:       topBreakdowns(c.byAgent, breakdownKindAgent),
		Retries:       c.retries,
		RetriesByCode: retriesRows(c.retriesByCode),
		Recent:        c.recentSnapshot(),
	}
}

// retriesRows renders the retry-by-status map as sorted rows. Status 0 (a
// transport-level error) is labeled "transport".
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

// recentSnapshot returns the recent-requests ring newest-first. Caller holds c.mu.
func (c *statsCollector) recentSnapshot() []recentRequest {
	out := make([]recentRequest, 0, c.recentSize)
	// recentIdx points at the next write slot; walk backwards from the most
	// recent entry, wrapping around the ring.
	for i := 0; i < c.recentSize; i++ {
		idx := (c.recentIdx - 1 - i + len(c.recent)) % len(c.recent)
		out = append(out, c.recent[idx])
	}
	return out
}

// latencyPercentiles returns p50/p95/p99 over the bounded recent-latency
// reservoir. Caller must hold c.mu.
func (c *statsCollector) latencyPercentiles() (p50, p95, p99 int64) {
	if c.latencyCount == 0 {
		return 0, 0, 0
	}
	sample := make([]int64, c.latencyCount)
	copy(sample, c.latencies[:c.latencyCount])
	sort.Slice(sample, func(i, j int) bool { return sample[i] < sample[j] })
	return percentile(sample, 0.50), percentile(sample, 0.95), percentile(sample, 0.99)
}

// percentile returns the q-quantile of a pre-sorted slice using
// nearest-rank. q is in [0,1].
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

// topStatusCodes returns error status codes (>=400) sorted by count desc, then
// by code asc for stable ordering.
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

// topErrorTargets returns provider/model error attributions sorted by count.
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
)

func topBreakdowns(m map[string]*breakdownCounter, kind string) []statsBreakdown {
	out := make([]statsBreakdown, 0, len(m))
	for key, e := range m {
		b := statsBreakdown{Requests: e.requests, Tokens: e.tokens, Errors: e.errors}
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
		}
		out = append(out, b)
	}
	sortBreakdownsByRequests(out)
	if len(out) <= statsBreakdownRows {
		return out
	}
	// The dashboard re-sorts and filters these rows client-side by errors,
	// latency, and tokens — not just requests. Truncating to the top-N by
	// requests alone would drop a low-volume model that holds all the errors, or
	// the slowest model, from those alternate views. Keep the union of the top-N
	// by each sort dimension the client offers so every client-side sort sees its
	// rows, while still bounding the response size.
	return unionTopBreakdowns(out, statsBreakdownRows)
}

// sortBreakdownsByRequests orders rows by requests desc, then tokens desc, then
// label asc, the default order the dashboard shows and a stable tiebreak.
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

// unionTopBreakdowns returns the union of the top-n rows by each metric the
// dashboard can sort by (requests, tokens, errors, latency). The result is
// deduplicated by label and ordered by the default requests-desc sort, so it is
// at most 4*n rows but usually far fewer (the same heavy hitters top multiple
// metrics). Rows with a zero value for a metric never enter that metric's top
// set, so error/latency dimensions only contribute rows that actually have
// errors/latency.
func unionTopBreakdowns(rows []statsBreakdown, n int) []statsBreakdown {
	if n <= 0 || len(rows) == 0 {
		return nil
	}
	metrics := []func(statsBreakdown) int64{
		func(b statsBreakdown) int64 { return b.Requests },
		func(b statsBreakdown) int64 { return b.Tokens },
		func(b statsBreakdown) int64 { return b.Errors },
		func(b statsBreakdown) int64 { return b.AvgMs },
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
				break // sorted desc: no further nonzero rows for this metric
			}
			keep[breakdownLabel(r)] = struct{}{}
			added++
		}
	}
	result := make([]statsBreakdown, 0, len(keep))
	for _, r := range rows { // rows is already in requests-desc order
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
	}
	return ""
}

// summaryStats is the subset of a RequestSummary the collector folds in.
type summaryStats struct {
	model             string
	metricModel       string
	provider          string
	kind              string
	endpoint          string
	upstreamID        string
	upstreamAttempted bool
	modelKnown        bool
	stream            bool
	prompt            int
	completion        int
	total             int
	cached            int
	reasoning         int
}

func readSummaryForStats(summary *RequestSummary) summaryStats {
	var d summaryStats
	if summary == nil {
		return d
	}
	summary.mu.Lock()
	defer summary.mu.Unlock()
	// model comes from the client request body; bound its length so a client
	// sending huge distinct model strings cannot bloat the breakdown/recent
	// stats (the cardinality cap limits count, not size). provider/kind are
	// configured server-side, so they are trusted.
	d.model = boundStatLabel(summary.model)
	d.metricModel = boundStatLabel(summary.metricModel)
	if d.metricModel == "" {
		d.metricModel = d.model
	}
	d.provider = summary.provider
	d.kind = summary.providerKind
	d.endpoint = summary.endpoint
	d.upstreamID = summary.upstreamRequestID
	d.upstreamAttempted = summary.upstreamAttempted
	d.modelKnown = summary.modelKnown
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
	// Fold in out-of-band internal spend (e.g. a 413-fallback compaction call)
	// that is tracked separately from the turn usage so it is not clobbered by
	// the final turn's setOpenAIUsage overwrite.
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

// classifyAgent maps a request User-Agent to a friendly client label. Only the
// label is retained, never the raw User-Agent.
func classifyAgent(userAgent string) string {
	ua := strings.TrimSpace(userAgent)
	if ua == "" {
		return "unknown"
	}
	lower := strings.ToLower(ua)
	switch {
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

// statLabelMaxLen bounds the length of a client-controlled stats label (model
// name, agent token) so a client cannot retain very large strings in the
// breakdown/recent stats and have them served back via /stats.json.
const statLabelMaxLen = 64

// boundStatLabel truncates an over-long label to statLabelMaxLen runes.
func boundStatLabel(s string) string {
	if len(s) <= statLabelMaxLen {
		return s
	}
	r := []rune(s)
	if len(r) <= statLabelMaxLen {
		return s
	}
	return string(r[:statLabelMaxLen]) + "…"
}

// isInferenceRoute reports whether method+path is one of the inference /
// compatibility endpoints whose traffic belongs in the dashboard's LLM-usage
// metrics. It is an allowlist matched against the server's registered routes, so
// non-inference requests — GET /v1/models catalog refreshes, the GET
// /v1/responses websocket upgrade, health/observability probes, and unmatched
// (404) paths — are excluded by construction rather than by enumerating things
// to skip. The websocket bridge records each turn directly via
// RecordResponsesTurn instead of through the middleware.
//
// Deliberately excluded even though they are registered routes: token-counting
// probes (POST /v1/messages/count_tokens, Gemini :countTokens) are non-
// generating sizing calls that are frequently served from cache or a local
// estimate with no upstream spend, and the proxy-owned compatibility shims
// (POST /v1/responses/compact, /v1/memories/trace_summarize) do not surface a
// single per-request model completion. Tracking any of these would add
// zero-token requests (and, for count_tokens, latency samples) that skew the
// dashboard's average-tokens/request and latency metrics. Keep this in sync with
// the inference routes registered in server/server.go.
func isInferenceRoute(method, path string) bool {
	switch method {
	case http.MethodPost:
		switch path {
		case "/v1/messages",
			"/v1/chat/completions",
			"/v1/responses":
			return true
		}
		// Gemini routes are registered as path prefixes ({model}:{action}). Track
		// only the generating actions HandleGeminiModels actually serves; the
		// action is the suffix after the last colon (parseGeminiPath), and
		// r.URL.Path carries no query string, so a suffix match is exact. This
		// excludes :countTokens (a sizing probe) and any unsupported/typo action
		// (e.g. :embedContent, :generateContentt) that the handler rejects with a
		// 400 before any upstream completion — those must not skew the metrics.
		if isGeminiModelsPath(path) {
			return strings.HasSuffix(path, ":generateContent") ||
				strings.HasSuffix(path, ":streamGenerateContent")
		}
		return false
	default:
		return false
	}
}

// isGeminiModelsPath reports whether path is one of the registered Gemini
// model-action route prefixes.
func isGeminiModelsPath(path string) bool {
	return strings.HasPrefix(path, "/v1beta/models/") ||
		strings.HasPrefix(path, "/v1/models/") ||
		strings.HasPrefix(path, "/models/")
}

// retryStatsTrackedContextKey marks a context that belongs to a tracked
// inference request, so upstream retries made on its behalf are counted in the
// dashboard's retry stats. It is a positive allow-marker rather than an
// exclusion marker because the upstream request context is rebuilt from the
// detached proxy lifecycle root (newInferenceUpstreamContext), which strips
// inherited values — an "is this excluded?" marker on the inbound context
// would simply be lost. The middleware sets it only when TracksRequest is true,
// and newInferenceUpstreamContext copies it onto the upstream context, so
// retries from non-tracked callers (the in-process /dashboard/insight call, the
// GET /v1/models catalog fetch, count-token probes, and the proxy shims) carry
// no marker and are not counted.
type retryStatsTrackedContextKey struct{}

func markRetryStatsTracked(ctx context.Context) context.Context {
	if ctx == nil {
		return context.WithValue(context.Background(), retryStatsTrackedContextKey{}, true)
	}
	return context.WithValue(ctx, retryStatsTrackedContextKey{}, true)
}

func isRetryStatsTracked(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	v, _ := ctx.Value(retryStatsTrackedContextKey{}).(bool)
	return v
}

// MarkRetryStatsTrackedIfInference returns a context carrying the retry-stats
// marker when method+path is a tracked inference route, otherwise the context
// unchanged. The server middleware calls this on the inbound request context so
// the marker propagates (via newInferenceUpstreamContext) to the upstream
// request whose retries should be counted.
func (h *ProxyHandler) MarkRetryStatsTrackedIfInference(ctx context.Context, method, path string) context.Context {
	if h == nil || h.stats == nil || !isInferenceRoute(method, path) {
		return ctx
	}
	return markRetryStatsTracked(ctx)
}

// TracksRequest reports whether requests for the given method+path should be
// recorded in traffic stats. The server middleware uses this to gate inflight +
// record. Only inference/compatibility endpoints are tracked (see
// isInferenceRoute): non-inference traffic such as GET /v1/models, the GET
// /v1/responses websocket upgrade (one long-lived connection serving many turns,
// recorded per-turn elsewhere), observability probes, and unmatched 404 paths
// are excluded so they do not skew request rate, latency percentiles, or average
// tokens/request.
func (h *ProxyHandler) TracksRequest(method, path string) bool {
	if h == nil || h.stats == nil {
		return false
	}
	return isInferenceRoute(method, path)
}

// IncInflight increments the live in-flight request gauge.
func (h *ProxyHandler) IncInflight() {
	if h != nil && h.stats != nil {
		h.stats.incInflight()
	}
	if h != nil && h.metrics != nil {
		h.metrics.IncInflight()
	}
}

// DecInflight decrements the live in-flight request gauge.
func (h *ProxyHandler) DecInflight() {
	if h != nil && h.stats != nil {
		h.stats.decInflight()
	}
	if h != nil && h.metrics != nil {
		h.metrics.DecInflight()
	}
}

// RecordRequest folds one completed request into the traffic stats.
func (h *ProxyHandler) RecordRequest(summary *RequestSummary, status int, userAgent string, dur time.Duration) {
	if h == nil || (summary != nil && summary.StatsSuppressed()) {
		return
	}
	if h.stats != nil {
		h.stats.record(summary, status, userAgent, dur)
	}
	if h.metrics != nil {
		h.metrics.Record(summary, status, dur)
	}
}

// RecordResponsesTurn folds one client /v1/responses websocket create turn into
// traffic stats. The bridge does not flow through the HTTP request middleware,
// so turns are recorded directly here. usage includes internal compaction spend
// accumulated before the terminal outcome; the returned record accepts bounded
// post-terminal usage amendments without creating synthetic requests. status is
// the client turn outcome so failed turns appear in error counts.
func (h *ProxyHandler) RecordResponsesTurn(model, provider, kind, agentLabel string, status int, usage responsesUsage) responsesTurnStatsRecord {
	return h.recordResponsesTurn(model, model, provider, kind, agentLabel, status, usage, true, true)
}

func (h *ProxyHandler) recordResponsesTurn(model, metricModel, provider, kind, agentLabel string, status int, usage responsesUsage, upstreamAttempted, modelKnown bool) responsesTurnStatsRecord {
	if h == nil {
		return responsesTurnStatsRecord{}
	}

	var record responsesTurnStatsRecord
	if h.stats != nil {
		record = h.stats.recordResponsesTurn(model, provider, kind, agentLabel, status, usage)
	}
	if h.metrics != nil {
		record.metrics = h.metrics.recordResponsesTurn(metricModel, provider, status, usage, upstreamAttempted, modelKnown)
	}
	return record
}

func (h *ProxyHandler) AddResponsesTurnUsage(record responsesTurnStatsRecord, usage responsesUsage) {
	if h == nil {
		return
	}
	if h.stats != nil {
		h.stats.addResponsesTurnUsage(record, usage)
	}
	if h.metrics != nil {
		h.metrics.AddResponsesTurnUsage(record.metrics, usage)
	}
}

// HandleStatsJSON handles GET /stats.json with the current traffic snapshot.
func (h *ProxyHandler) HandleStatsJSON(w http.ResponseWriter, r *http.Request) {
	var snap statsSnapshot
	if h != nil && h.stats != nil {
		snap = h.stats.snapshot()
	}
	if h != nil {
		snap.InsightsEnabled = strings.TrimSpace(h.providersConfig.InsightModel) != ""
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(snap)
}

// HandleDashboard handles GET /dashboard and serves the embedded dashboard page.
func (h *ProxyHandler) HandleDashboard(w http.ResponseWriter, r *http.Request) {
	serveDashboardFile(w, "dashboard/dashboard.html", "text/html; charset=utf-8", false)
}

// HandleFavicon answers GET /favicon.ico with 204 No Content so a browser
// opening the dashboard does not generate a 404 (GET /favicon.ico is not an
// inference route, so it is also excluded from traffic stats).
func (h *ProxyHandler) HandleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=86400")
	w.WriteHeader(http.StatusNoContent)
}

// HandleDashboardAsset serves the embedded, vendored dashboard assets (uPlot).
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
