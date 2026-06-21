package proxy

import (
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
	requests int64
	tokens   int64
	errors   int64
	durMs    int64 // sum of request durations in ms, for avg latency
	kind     string
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
	recent     []recentRequest
	recentIdx  int
	recentSize int

	// latencies is a bounded ring of recent request durations (ms) used to
	// compute p50/p95/p99 without retaining every sample forever.
	latencies    []int64
	latencyIdx   int
	latencyCount int

	inflight atomic.Int64
}

// recentRequest is one row in the recent-requests drill-down log.
type recentRequest struct {
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

// record folds one completed request into the aggregates.
func (c *statsCollector) record(summary *RequestSummary, status int, userAgent string, dur time.Duration) {
	d := readSummaryForStats(summary)
	agent := classifyAgent(userAgent)
	sec := c.now().Unix()
	isErr := status >= http.StatusBadRequest
	durMs := dur.Milliseconds()

	c.mu.Lock()
	defer c.mu.Unlock()

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

	// Bounded latency reservoir (overwrite oldest once full).
	c.latencies[c.latencyIdx] = durMs
	c.latencyIdx = (c.latencyIdx + 1) % len(c.latencies)
	if c.latencyCount < len(c.latencies) {
		c.latencyCount++
	}

	c.status[statusClass(status)]++
	if isErr {
		c.statusCodes[status]++
		c.errTargets[capKey(c.errTargets, errorTargetLabel(d.provider, d.model))]++
	}

	if d.model != "" {
		addBreakdown(c.byModel, capKeyForCounter(c.byModel, d.model), int64(d.total), "", isErr, durMs)
	}
	if d.provider != "" {
		// Providers are configured, not client-controlled, so no cap needed.
		addBreakdown(c.byProvider, d.provider, int64(d.total), d.kind, isErr, durMs)
	}
	addBreakdown(c.byAgent, capKeyForCounter(c.byAgent, agent), int64(d.total), "", isErr, durMs)

	// Append to the recent-requests drill-down ring (newest overwrites oldest).
	c.recent[c.recentIdx] = recentRequest{
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
}

// capKey folds an overflow key into the "other" bucket once a map of plain
// counters reaches the cardinality cap. Existing keys always pass through.
func capKey(m map[string]int64, key string) string {
	if _, ok := m[key]; ok {
		return key
	}
	if len(m) >= statsMaxKeys {
		return statsOtherKey
	}
	return key
}

// capKeyForCounter is capKey for the breakdownCounter maps.
func capKeyForCounter(m map[string]*breakdownCounter, key string) string {
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

func addBreakdown(m map[string]*breakdownCounter, key string, tokens int64, kind string, isErr bool, durMs int64) {
	e := m[key]
	if e == nil {
		e = &breakdownCounter{}
		m[key] = e
	}
	e.requests++
	e.tokens += tokens
	e.durMs += durMs
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
		if e.requests > 0 {
			b.AvgMs = e.durMs / e.requests
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
	sort.Slice(out, func(i, j int) bool {
		if out[i].Requests != out[j].Requests {
			return out[i].Requests > out[j].Requests
		}
		if out[i].Tokens != out[j].Tokens {
			return out[i].Tokens > out[j].Tokens
		}
		return breakdownLabel(out[i]) < breakdownLabel(out[j])
	})
	if len(out) > statsBreakdownRows {
		out = out[:statsBreakdownRows]
	}
	return out
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
	model      string
	provider   string
	kind       string
	endpoint   string
	upstreamID string
	prompt     int
	completion int
	total      int
	cached     int
	reasoning  int
}

func readSummaryForStats(summary *RequestSummary) summaryStats {
	var d summaryStats
	if summary == nil {
		return d
	}
	summary.mu.Lock()
	defer summary.mu.Unlock()
	d.model = summary.model
	d.provider = summary.provider
	d.kind = summary.providerKind
	d.endpoint = summary.endpoint
	d.upstreamID = summary.upstreamRequestID
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
	return token
}

// isObservabilityPath reports whether a request path is proxy infrastructure
// (the dashboard, its assets, the stats feed, and health probes) that must be
// excluded from traffic stats so the dashboard does not measure itself.
func isObservabilityPath(path string) bool {
	switch path {
	case "/healthz", "/readyz", "/stats.json", "/dashboard":
		return true
	}
	return strings.HasPrefix(path, "/dashboard/")
}

// TracksRequest reports whether requests for the given path should be recorded
// in traffic stats. The server middleware uses this to gate inflight + record.
func (h *ProxyHandler) TracksRequest(path string) bool {
	if h == nil || h.stats == nil {
		return false
	}
	return !isObservabilityPath(path)
}

// IncInflight increments the live in-flight request gauge.
func (h *ProxyHandler) IncInflight() {
	if h != nil && h.stats != nil {
		h.stats.incInflight()
	}
}

// DecInflight decrements the live in-flight request gauge.
func (h *ProxyHandler) DecInflight() {
	if h != nil && h.stats != nil {
		h.stats.decInflight()
	}
}

// RecordRequest folds one completed request into the traffic stats.
func (h *ProxyHandler) RecordRequest(summary *RequestSummary, status int, userAgent string, dur time.Duration) {
	if h == nil || h.stats == nil {
		return
	}
	h.stats.record(summary, status, userAgent, dur)
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
