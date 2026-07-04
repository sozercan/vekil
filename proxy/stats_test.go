package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/models"
)

// newStatsRequestSummary builds a populated RequestSummary using the in-package
// setters, as handlers do at runtime.
func newStatsRequestSummary(model, provider, kind string, prompt, completion, total int) *RequestSummary {
	s := &RequestSummary{}
	s.setRoute("/v1/chat/completions", model, false)
	s.setProvider(provider, kind)
	s.setOpenAIUsage(&models.OpenAIUsage{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
	})
	return s
}

func TestSeedRequestSummaryEndpointPreservesEarlyErrorRoute(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	SeedRequestSummaryEndpointForRoute(ctx, http.MethodPost, "/v1/chat/completions")
	if got := readSummaryForStats(summary).endpoint; got != "/v1/chat/completions" {
		t.Fatalf("seeded endpoint = %q, want route path", got)
	}
	summary.setRoute("openai_chat", "gpt-5", false)
	if got := readSummaryForStats(summary).endpoint; got != "openai_chat" {
		t.Fatalf("handler endpoint = %q, want semantic endpoint", got)
	}
	c := newStatsCollector()
	summary.setProvider("openai", "openai")
	c.record(summary, http.StatusOK, "test-agent", time.Millisecond)
	if got := c.snapshot().RequestMetrics[0].Endpoint; got != "/v1/chat/completions" {
		t.Fatalf("metric endpoint = %q, want normalized route path", got)
	}

	ctx, geminiSummary := WithRequestSummary(context.Background())
	SeedRequestSummaryEndpointForRoute(ctx, http.MethodPost, "/v1beta/models/gemini-unique:generateContent")
	if got := readSummaryForStats(geminiSummary).endpoint; got != "gemini" {
		t.Fatalf("gemini seeded endpoint = %q, want stable route label", got)
	}
}

func TestStatsCollectorRecordTotals(t *testing.T) {
	c := newStatsCollector()
	fixed := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return fixed }

	c.record(newStatsRequestSummary("claude-sonnet-4.5", "copilot", "copilot", 100, 50, 150), 200, "claude-cli/1.0", 0)
	c.record(newStatsRequestSummary("claude-sonnet-4.5", "copilot", "copilot", 200, 100, 300), 200, "claude-cli/1.0", 0)
	c.record(newStatsRequestSummary("gpt-5.4", "azure", "azure", 10, 0, 10), 500, "codex/2.0", 0)

	snap := c.snapshot()

	if snap.Totals.Requests != 3 {
		t.Fatalf("requests: got %d want 3", snap.Totals.Requests)
	}
	if snap.Totals.Errors != 1 {
		t.Fatalf("errors: got %d want 1", snap.Totals.Errors)
	}
	if snap.Totals.PromptTokens != 310 {
		t.Fatalf("prompt tokens: got %d want 310", snap.Totals.PromptTokens)
	}
	if snap.Totals.CompletionTokens != 150 {
		t.Fatalf("completion tokens: got %d want 150", snap.Totals.CompletionTokens)
	}
	if snap.Totals.TotalTokens != 460 {
		t.Fatalf("total tokens: got %d want 460", snap.Totals.TotalTokens)
	}
	if snap.Status["2xx"] != 2 || snap.Status["5xx"] != 1 {
		t.Fatalf("status classes: got %+v", snap.Status)
	}
}

func TestStatsCollectorUsageDetails(t *testing.T) {
	c := newStatsCollector()
	fixed := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return fixed }

	withDetails := func(prompt, completion, total, cached, reasoning int) *RequestSummary {
		s := &RequestSummary{}
		s.setRoute("/v1/chat/completions", "gpt-5.4", false)
		s.setProvider("azure", "azure")
		s.setOpenAIUsage(&models.OpenAIUsage{
			PromptTokens:            prompt,
			CompletionTokens:        completion,
			TotalTokens:             total,
			PromptTokensDetails:     &models.OpenAIPromptTokensDetails{CachedTokens: cached},
			CompletionTokensDetails: &models.OpenAICompletionTokensDetails{ReasoningTokens: reasoning},
		})
		return s
	}

	c.record(withDetails(100, 50, 150, 40, 20), 200, "claude-cli/1.0", 0)
	c.record(withDetails(200, 80, 280, 60, 30), 200, "claude-cli/1.0", 0)

	snap := c.snapshot()
	if snap.Totals.CachedTokens != 100 {
		t.Fatalf("cached tokens: got %d want 100", snap.Totals.CachedTokens)
	}
	if snap.Totals.ReasoningTokens != 50 {
		t.Fatalf("reasoning tokens: got %d want 50", snap.Totals.ReasoningTokens)
	}
	if len(snap.TokenMetrics) != 1 {
		t.Fatalf("token metric rows: got %d want 1 (%+v)", len(snap.TokenMetrics), snap.TokenMetrics)
	}
	row := snap.TokenMetrics[0]
	if row.Provider != "azure" || row.Model != "gpt-5.4" || row.PromptTokens != 300 || row.CompletionTokens != 130 || row.TotalTokens != 430 || row.CachedTokens != 100 || row.ReasoningTokens != 50 {
		t.Fatalf("token metric row: got %+v", row)
	}
}

func TestStatsCollectorErrorClassification(t *testing.T) {
	c := newStatsCollector()
	fixed := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return fixed }

	c.record(newStatsRequestSummary("claude-sonnet-4.5", "copilot", "copilot", 10, 5, 15), 200, "claude-cli/1.0", 0)
	c.record(newStatsRequestSummary("claude-sonnet-4.5", "copilot", "copilot", 10, 5, 15), 429, "claude-cli/1.0", 0)
	c.record(newStatsRequestSummary("claude-sonnet-4.5", "copilot", "copilot", 10, 5, 15), 429, "claude-cli/1.0", 0)
	c.record(newStatsRequestSummary("gpt-5.4", "azure", "azure", 10, 5, 15), 503, "codex/2.0", 0)

	snap := c.snapshot()

	// Exact status codes, sorted by count desc.
	if len(snap.StatusCodes) != 2 {
		t.Fatalf("status codes rows: got %d want 2 (%+v)", len(snap.StatusCodes), snap.StatusCodes)
	}
	if snap.StatusCodes[0].Label != "429" || snap.StatusCodes[0].Count != 2 {
		t.Fatalf("top status code: got %+v want 429 x2", snap.StatusCodes[0])
	}
	if snap.StatusCodes[1].Label != "503" || snap.StatusCodes[1].Count != 1 {
		t.Fatalf("second status code: got %+v want 503 x1", snap.StatusCodes[1])
	}
	if len(snap.UpstreamErrorMetrics) != 2 {
		t.Fatalf("upstream error metrics: got %d want 2 (%+v)", len(snap.UpstreamErrorMetrics), snap.UpstreamErrorMetrics)
	}
	if got := snap.UpstreamErrorMetrics[0]; got.Provider != "azure" || got.Model != "gpt-5.4" || got.Code != 503 || got.Count != 1 {
		t.Fatalf("first upstream error metric: got %+v", got)
	}
	if got := snap.UpstreamErrorMetrics[1]; got.Provider != "copilot" || got.Model != "claude-sonnet-4.5" || got.Code != 429 || got.Count != 2 {
		t.Fatalf("second upstream error metric: got %+v", got)
	}

	// Error targets attribute provider/model.
	if len(snap.Errors) != 2 {
		t.Fatalf("error targets: got %d want 2 (%+v)", len(snap.Errors), snap.Errors)
	}
	if snap.Errors[0].Label != "copilot / claude-sonnet-4.5" || snap.Errors[0].Count != 2 {
		t.Fatalf("top error target: got %+v", snap.Errors[0])
	}

	// Per-model error counts surface in the breakdown rows.
	var claude statsBreakdown
	for _, m := range snap.ByModel {
		if m.Model == "claude-sonnet-4.5" {
			claude = m
		}
	}
	if claude.Errors != 2 || claude.Requests != 3 {
		t.Fatalf("claude breakdown: got requests=%d errors=%d want 3/2", claude.Requests, claude.Errors)
	}
}

func TestStatsCollectorPreRoutingErrorsExportUpstreamErrorMetric(t *testing.T) {
	c := newStatsCollector()
	c.record(&RequestSummary{}, http.StatusBadRequest, "test-agent", time.Millisecond)

	snap := c.snapshot()
	if len(snap.UpstreamErrorMetrics) != 1 {
		t.Fatalf("upstream error metrics: got %d want 1 (%+v)", len(snap.UpstreamErrorMetrics), snap.UpstreamErrorMetrics)
	}
	got := snap.UpstreamErrorMetrics[0]
	if got.Provider != "unrouted" || got.Model != "unknown" || got.Code != http.StatusBadRequest || got.Count != 1 {
		t.Fatalf("upstream error metric = %+v, want unrouted/unknown/400 count 1", got)
	}
}

func TestStatsCollectorLatencyPercentiles(t *testing.T) {
	c := newStatsCollector()
	fixed := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return fixed }

	// 100 requests with durations 1..100 ms.
	for i := 1; i <= 100; i++ {
		c.record(newStatsRequestSummary("m", "p", "k", 1, 1, 2), 200, "curl/8", time.Duration(i)*time.Millisecond)
	}

	snap := c.snapshot()
	// Nearest-rank on a sorted 1..100 sample.
	if got := snap.Totals.LatencyP50; got < 49 || got > 52 {
		t.Fatalf("p50: got %d want ~50", got)
	}
	if got := snap.Totals.LatencyP95; got < 94 || got > 97 {
		t.Fatalf("p95: got %d want ~95", got)
	}
	if got := snap.Totals.LatencyP99; got < 98 || got > 100 {
		t.Fatalf("p99: got %d want ~99", got)
	}

	// Per-model average latency surfaces in the breakdown (mean of 1..100 = 50).
	if len(snap.ByModel) != 1 || snap.ByModel[0].AvgMs < 49 || snap.ByModel[0].AvgMs > 51 {
		t.Fatalf("avg ms: got %+v want ~50", snap.ByModel)
	}
}

func TestStatsCollectorLatencySummaryTotalsExcludeStreaming(t *testing.T) {
	c := newStatsCollector()

	nonStreaming := newStatsRequestSummary("gpt-5", "openai", "openai", 1, 1, 2)
	streaming := newStatsRequestSummary("gpt-5", "openai", "openai", 1, 1, 2)
	streaming.setRoute("/v1/chat/completions", "gpt-5", true)

	c.record(nonStreaming, http.StatusOK, "curl/8", 25*time.Millisecond)
	c.record(streaming, http.StatusOK, "curl/8", 5*time.Second)

	snap := c.snapshot()
	if snap.Totals.Requests != 2 {
		t.Fatalf("requests: got %d want 2", snap.Totals.Requests)
	}
	if snap.Totals.LatencyCount != 1 {
		t.Fatalf("latency count: got %d want 1", snap.Totals.LatencyCount)
	}
	if snap.Totals.LatencySumMs != 25 {
		t.Fatalf("latency sum ms: got %d want 25", snap.Totals.LatencySumMs)
	}
}

func TestStatsCollectorCapsCardinality(t *testing.T) {
	c := newStatsCollector()
	fixed := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return fixed }

	// Far more distinct agents than the cap; memory must stay bounded.
	for i := 0; i < statsMaxKeys*3; i++ {
		ua := "agent-" + strconv.Itoa(i) + "/1.0"
		c.record(newStatsRequestSummary("m", "p", "k", 1, 1, 2), 200, ua, time.Millisecond)
	}

	c.mu.Lock()
	agents := len(c.byAgent)
	c.mu.Unlock()
	if agents > statsMaxKeys+1 { // +1 for the "other" bucket
		t.Fatalf("byAgent cardinality unbounded: got %d want <= %d", agents, statsMaxKeys+1)
	}

	// The overflow bucket must exist and carry the spillover.
	c.mu.Lock()
	other := c.byAgent[statsOtherKey]
	c.mu.Unlock()
	if other == nil || other.requests == 0 {
		t.Fatalf("expected overflow folded into %q bucket", statsOtherKey)
	}
}

func TestStatsCollectorRetries(t *testing.T) {
	c := newStatsCollector()
	c.incRetry(context.Background(), 429)
	c.incRetry(context.Background(), 429)
	c.incRetry(context.Background(), 503)
	c.incRetry(context.Background(), 0) // transport error

	snap := c.snapshot()
	if snap.Retries != 4 {
		t.Fatalf("retries total: got %d want 4", snap.Retries)
	}
	if len(snap.RetriesByCode) == 0 || snap.RetriesByCode[0].Label != "429" || snap.RetriesByCode[0].Count != 2 {
		t.Fatalf("top retry code: got %+v want 429x2", snap.RetriesByCode)
	}
	// Transport errors are labeled, not shown as "0".
	var sawTransport bool
	for _, r := range snap.RetriesByCode {
		if r.Label == "transport" {
			sawTransport = true
		}
		if r.Label == "0" {
			t.Fatalf("status 0 should render as 'transport', got raw 0")
		}
	}
	if !sawTransport {
		t.Fatal("expected a 'transport' retry row")
	}
	if len(snap.RetryMetrics) != 3 {
		t.Fatalf("retry metric rows: got %d want 3 (%+v)", len(snap.RetryMetrics), snap.RetryMetrics)
	}
}

func TestStatsCollectorRetryMetricsCapsModelCardinality(t *testing.T) {
	c := newStatsCollector()
	for i := 0; i < statsMaxKeys+10; i++ {
		ctx := markRetryStatsTrackedWithLabels(context.Background(), "openai", fmt.Sprintf("model-%03d", i))
		c.incRetry(ctx, http.StatusTooManyRequests)
	}

	snap := c.snapshot()
	if got, want := len(snap.RetryMetrics), statsMaxKeys+1; got != want {
		t.Fatalf("retry metric rows = %d, want %d", got, want)
	}
	var overflow statsRetryMetric
	for _, row := range snap.RetryMetrics {
		if row.Model == statsOtherKey {
			overflow = row
			break
		}
	}
	if overflow.Provider != "openai" || overflow.Reason != "429" || overflow.Count != 10 {
		t.Fatalf("overflow retry metric = %+v, want openai/other/429 count 10", overflow)
	}
}

func TestStatsCollectorRecentRing(t *testing.T) {
	c := newStatsCollector()
	fixed := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return fixed }

	// Record more than the ring holds; only the newest statsRecentRequests survive.
	total := statsRecentRequests + 20
	for i := 0; i < total; i++ {
		s := newStatsRequestSummary("m"+strconv.Itoa(i), "copilot", "copilot", 10, 5, 15)
		c.record(s, 200, "claude-cli/1.0", time.Duration(i)*time.Millisecond)
	}

	snap := c.snapshot()
	if len(snap.Recent) != statsRecentRequests {
		t.Fatalf("recent len: got %d want %d", len(snap.Recent), statsRecentRequests)
	}
	// Newest first: the last-recorded model should be at index 0.
	if snap.Recent[0].Model != "m"+strconv.Itoa(total-1) {
		t.Fatalf("newest-first: got %q want %q", snap.Recent[0].Model, "m"+strconv.Itoa(total-1))
	}
	// Oldest retained is total-statsRecentRequests.
	if snap.Recent[len(snap.Recent)-1].Model != "m"+strconv.Itoa(total-statsRecentRequests) {
		t.Fatalf("oldest retained: got %q", snap.Recent[len(snap.Recent)-1].Model)
	}
}

func TestStatsCollectorRecentCapturesFields(t *testing.T) {
	c := newStatsCollector()
	s := &RequestSummary{}
	s.setRoute("/v1/chat/completions", "claude-sonnet-4.5", true)
	s.setProvider("copilot", "copilot")
	s.setUpstreamRequestID("req-abc-123")
	s.setOpenAIUsage(&models.OpenAIUsage{PromptTokens: 100, CompletionTokens: 50, TotalTokens: 150})

	c.record(s, 503, "codex_cli_rs/0.5.0", 1234*time.Millisecond)
	r := c.snapshot().Recent[0]

	if r.Model != "claude-sonnet-4.5" || r.Provider != "copilot" || r.Agent != "Codex CLI" {
		t.Fatalf("recent labels: %+v", r)
	}
	if r.Status != 503 || r.DurMs != 1234 || r.TotalTokens != 150 {
		t.Fatalf("recent metrics: %+v", r)
	}
	if r.UpstreamRequestID != "req-abc-123" {
		t.Fatalf("recent upstream id: got %q", r.UpstreamRequestID)
	}
}

func TestStatsCollectorBreakdowns(t *testing.T) {
	c := newStatsCollector()
	fixed := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return fixed }

	c.record(newStatsRequestSummary("claude-sonnet-4.5", "copilot", "copilot", 100, 50, 150), 200, "claude-cli/1.0", 0)
	c.record(newStatsRequestSummary("claude-sonnet-4.5", "copilot", "copilot", 100, 50, 150), 200, "claude-cli/1.0", 0)
	c.record(newStatsRequestSummary("gpt-5.4", "azure", "azure", 80, 20, 100), 200, "codex/2.0", 0)

	snap := c.snapshot()

	if len(snap.ByModel) != 2 {
		t.Fatalf("by_model rows: got %d want 2", len(snap.ByModel))
	}
	// Sorted desc by requests: claude (2) first.
	if snap.ByModel[0].Model != "claude-sonnet-4.5" || snap.ByModel[0].Requests != 2 {
		t.Fatalf("top model: got %+v", snap.ByModel[0])
	}
	if snap.ByModel[0].Tokens != 300 {
		t.Fatalf("top model tokens: got %d want 300", snap.ByModel[0].Tokens)
	}

	if snap.ByProvider[0].Provider != "copilot" || snap.ByProvider[0].Kind != "copilot" {
		t.Fatalf("top provider: got %+v", snap.ByProvider[0])
	}

	if snap.ByAgent[0].Agent != "Claude Code" || snap.ByAgent[0].Requests != 2 {
		t.Fatalf("top agent: got %+v", snap.ByAgent[0])
	}
}

func TestTopBreakdownsPreservesAlternateSortRows(t *testing.T) {
	// statsBreakdownRows+ heavy hitters by requests, plus one low-request model
	// that holds all the errors and one that is by far the slowest. The dashboard
	// re-sorts these rows client-side by errors/latency, so both outliers must
	// survive truncation even though they rank low by request count.
	m := make(map[string]*breakdownCounter)
	for i := 0; i < statsBreakdownRows+5; i++ {
		m[fmt.Sprintf("busy-%02d", i)] = &breakdownCounter{
			requests: int64(1000 - i), // all rank above the outliers by requests
			tokens:   int64(1000 - i),
		}
	}
	// Low request count → ranked well outside the top-N by requests.
	m["rare-but-broken"] = &breakdownCounter{requests: 1, tokens: 1, errors: 999}
	m["rare-but-slow"] = &breakdownCounter{requests: 1, tokens: 1, durMs: 9_000_000, durSamples: 1}

	rows := topBreakdowns(m, breakdownKindModel, true)

	find := func(label string) *statsBreakdown {
		for i := range rows {
			if rows[i].Model == label {
				return &rows[i]
			}
		}
		return nil
	}
	if find("rare-but-broken") == nil {
		t.Fatal("high-error model outside top-by-requests was dropped; client error sort would miss it")
	}
	slow := find("rare-but-slow")
	if slow == nil {
		t.Fatal("slowest model outside top-by-requests was dropped; client latency sort would miss it")
	}
	if slow.AvgMs != 9_000_000 {
		t.Fatalf("slow model avg ms: got %d want 9000000", slow.AvgMs)
	}
	// Default order is still requests-desc (the busiest model leads).
	if rows[0].Model != "busy-00" {
		t.Fatalf("default order should be requests-desc, got leader %q", rows[0].Model)
	}
}

func TestClassifyAgent(t *testing.T) {
	tests := []struct {
		ua   string
		want string
	}{
		{"claude-cli/1.0.83 (external, cli)", "Claude Code"},
		{"codex_cli_rs/0.5.0", "Codex CLI"},
		{"google-genai-sdk/1.2 gemini-cli", "Gemini CLI"},
		{"GitHubCopilotChat/0.26.7", "GitHub Copilot"},
		{"curl/8.4.0", "curl"},
		{"python-requests/2.31.0", "python"},
		{"node-fetch/3.0", "node"},
		{"", "unknown"},
		{"MysteryClient/9.9 (extra)", "MysteryClient"},
		{"singleword", "singleword"},
	}
	for _, tt := range tests {
		if got := classifyAgent(tt.ua); got != tt.want {
			t.Errorf("classifyAgent(%q): got %q want %q", tt.ua, got, tt.want)
		}
	}
}

func TestStatsCollectorSeriesZeroFill(t *testing.T) {
	c := newStatsCollector()
	base := time.Unix(1_700_000_000, 0)
	cur := base
	c.now = func() time.Time { return cur }

	// Two requests at t0.
	c.record(newStatsRequestSummary("m", "p", "k", 5, 5, 10), 200, "curl/8", 0)
	c.record(newStatsRequestSummary("m", "p", "k", 5, 5, 10), 200, "curl/8", 0)
	// One request 3 seconds later (a 2-second gap in between).
	cur = base.Add(3 * time.Second)
	c.record(newStatsRequestSummary("m", "p", "k", 1, 1, 2), 200, "curl/8", 0)

	snap := c.snapshot()
	if len(snap.Series) != statsSeriesSeconds {
		t.Fatalf("series length: got %d want %d", len(snap.Series), statsSeriesSeconds)
	}

	byT := make(map[int64]statsSeriesPoint, len(snap.Series))
	var sumReq int64
	for _, p := range snap.Series {
		byT[p.T] = p
		sumReq += p.Req
	}
	if sumReq != 3 {
		t.Fatalf("series total req: got %d want 3", sumReq)
	}
	if p, ok := byT[base.Unix()]; !ok || p.Req != 2 {
		t.Fatalf("t0 bucket: got %+v ok=%v want req=2", p, ok)
	}
	if p, ok := byT[base.Add(time.Second).Unix()]; !ok || p.Req != 0 {
		t.Fatalf("t+1 gap bucket should be zero-filled: got %+v ok=%v", p, ok)
	}
	if p, ok := byT[base.Add(3*time.Second).Unix()]; !ok || p.Req != 1 {
		t.Fatalf("t+3 bucket: got %+v ok=%v want req=1", p, ok)
	}
	// Series must be time-ordered ascending and contiguous by 1s.
	for i := 1; i < len(snap.Series); i++ {
		if snap.Series[i].T != snap.Series[i-1].T+1 {
			t.Fatalf("series not contiguous at %d: %d then %d", i, snap.Series[i-1].T, snap.Series[i].T)
		}
	}
}

func TestStatsCollectorInflight(t *testing.T) {
	c := newStatsCollector()
	c.incInflightForProvider("azure")
	c.incInflight()
	c.decInflight()
	snap := c.snapshot()
	if got := snap.Inflight; got != 1 {
		t.Fatalf("inflight: got %d want 1", got)
	}
	if len(snap.InflightMetrics) != 1 || snap.InflightMetrics[0].Provider != "azure" || snap.InflightMetrics[0].Count != 1 {
		t.Fatalf("inflight metrics: got %+v want azure=1", snap.InflightMetrics)
	}
}

func TestHandleStatsJSON(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	h.stats.record(newStatsRequestSummary("claude-sonnet-4.5", "copilot", "copilot", 10, 5, 15), 200, "claude-cli/1.0", 0)

	req := httptest.NewRequest(http.MethodGet, "/stats.json", nil)
	w := httptest.NewRecorder()
	h.HandleStatsJSON(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "application/json" {
		t.Fatalf("content-type: got %q", ct)
	}

	var snap statsSnapshot
	if err := json.NewDecoder(resp.Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if snap.Totals.Requests != 1 {
		t.Fatalf("requests: got %d want 1", snap.Totals.Requests)
	}
	if len(snap.Series) != statsSeriesSeconds {
		t.Fatalf("series length: got %d want %d", len(snap.Series), statsSeriesSeconds)
	}
}

func TestHandleDashboard(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	req := httptest.NewRequest(http.MethodGet, "/dashboard", nil)
	w := httptest.NewRecorder()
	h.HandleDashboard(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type: got %q", ct)
	}
	if w.Body.Len() == 0 {
		t.Fatal("dashboard body is empty")
	}
}

func TestHandleDashboardAsset(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	tests := []struct {
		path string
		ct   string
	}{
		{"/dashboard/uPlot.min.js", "text/javascript; charset=utf-8"},
		{"/dashboard/uPlot.min.css", "text/css; charset=utf-8"},
	}
	for _, tt := range tests {
		req := httptest.NewRequest(http.MethodGet, tt.path, nil)
		w := httptest.NewRecorder()
		h.HandleDashboardAsset(w, req)
		resp := w.Result()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status: got %d want 200", tt.path, resp.StatusCode)
		}
		if ct := resp.Header.Get("Content-Type"); ct != tt.ct {
			t.Fatalf("%s content-type: got %q want %q", tt.path, ct, tt.ct)
		}
		if w.Body.Len() == 0 {
			t.Fatalf("%s body is empty", tt.path)
		}
	}

	// Unknown asset → 404.
	req := httptest.NewRequest(http.MethodGet, "/dashboard/evil.js", nil)
	w := httptest.NewRecorder()
	h.HandleDashboardAsset(w, req)
	if w.Result().StatusCode != http.StatusNotFound {
		t.Fatalf("unknown asset: got %d want 404", w.Result().StatusCode)
	}
}

func TestTracksRequest(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	tracked := []string{"/v1/chat/completions", "/v1/messages", "/v1/responses"}
	for _, p := range tracked {
		if !h.TracksRequest(http.MethodPost, p) {
			t.Errorf("expected POST %s to be tracked", p)
		}
	}
	// Gemini generate verbs are prefix-registered; track them.
	geminiPrefixes := []string{"/v1beta/models/gemini-2.5-pro:generateContent", "/v1/models/gemini-2.5-pro:streamGenerateContent", "/models/gemini-2.5-pro:generateContent"}
	for _, p := range geminiPrefixes {
		if !h.TracksRequest(http.MethodPost, p) {
			t.Errorf("expected POST %s (gemini) to be tracked", p)
		}
	}
	skipped := []string{"/healthz", "/readyz", "/stats.json", "/metrics", "/dashboard", "/dashboard/uPlot.min.js", "/favicon.ico"}
	for _, p := range skipped {
		if h.TracksRequest(http.MethodGet, p) {
			t.Errorf("expected %s to be skipped", p)
		}
	}
	// Token-counting probes and proxy-owned shims are non-generating or have no
	// single per-request completion, so they are excluded from LLM-usage stats
	// even though they are registered POST routes (tracking them would add
	// zero-token requests that skew avg-tokens/request and latency).
	nonInference := []string{"/v1/messages/count_tokens", "/v1/responses/compact", "/v1/memories/trace_summarize"}
	for _, p := range nonInference {
		if h.TracksRequest(http.MethodPost, p) {
			t.Errorf("expected POST %s (non-generating / shim) to be skipped", p)
		}
	}
	// Gemini :countTokens is the sizing probe — skipped — while the generate
	// verbs above stay tracked, even though all three share the same path prefix.
	geminiCount := []string{"/v1beta/models/gemini-2.5-pro:countTokens", "/v1/models/gemini-2.5-pro:countTokens", "/models/gemini-2.5-pro:countTokens"}
	for _, p := range geminiCount {
		if h.TracksRequest(http.MethodPost, p) {
			t.Errorf("expected POST %s (gemini countTokens) to be skipped", p)
		}
	}
	// Unsupported or typoed Gemini actions hit the registered handler but 400
	// before any upstream completion, so they must not be folded into stats —
	// only the two generating actions are tracked, by explicit suffix.
	geminiNonGenerating := []string{
		"/v1beta/models/gemini-2.5-pro:embedContent",
		"/v1/models/gemini-2.5-pro:batchEmbedContents",
		"/models/gemini-2.5-pro:generateContentt", // typo
		"/v1beta/models/gemini-2.5-pro",           // no action
	}
	for _, p := range geminiNonGenerating {
		if h.TracksRequest(http.MethodPost, p) {
			t.Errorf("expected POST %s (non-generating gemini action) to be skipped", p)
		}
	}
	// GET /v1/models (catalog refresh) is not inference traffic — excluded so it
	// doesn't skew latency/avg-tokens. GET /v1/responses is the websocket-bridge
	// upgrade — excluded so it doesn't pin inflight or poison latency; POST
	// /v1/responses stays tracked.
	if h.TracksRequest(http.MethodGet, "/v1/models") {
		t.Error("expected GET /v1/models (catalog) to be skipped")
	}
	if h.TracksRequest(http.MethodGet, "/v1/responses") {
		t.Error("expected GET /v1/responses (websocket) to be skipped")
	}
	if !h.TracksRequest(http.MethodPost, "/v1/responses") {
		t.Error("expected POST /v1/responses to be tracked")
	}
	// Unmatched/typoed paths (which the mux 404s) must not be folded into stats.
	if h.TracksRequest(http.MethodPost, "/v1/typo") || h.TracksRequest(http.MethodGet, "/totally/unknown") {
		t.Error("expected unmatched paths to be skipped")
	}
}

// TestStatsCollectorStreamExcludedFromLatency verifies that streaming requests
// (whose wall-clock is dominated by how long the client held the connection)
// do not pollute the latency percentiles or per-breakdown average latency.
func TestStatsCollectorStreamExcludedFromLatency(t *testing.T) {
	c := newStatsCollector()
	fixed := time.Unix(1_700_000_000, 0)
	c.now = func() time.Time { return fixed }

	streamSummary := func(model string) *RequestSummary {
		s := &RequestSummary{}
		s.setRoute("/v1/chat/completions", model, true) // stream = true
		s.setProvider("copilot", "copilot")
		return s
	}
	nonStream := func(model string) *RequestSummary {
		s := &RequestSummary{}
		s.setRoute("/v1/chat/completions", model, false)
		s.setProvider("copilot", "copilot")
		return s
	}

	// One fast non-stream request and one 10-minute stream on the same model.
	c.record(nonStream("m"), 200, "curl/8", 50*time.Millisecond)
	c.record(streamSummary("m"), 200, "curl/8", 10*time.Minute)

	snap := c.snapshot()
	// Percentiles must reflect only the 50ms sample, not the 600000ms stream.
	if snap.Totals.LatencyP95 > 100 {
		t.Fatalf("stream duration leaked into p95: got %d ms (want ~50)", snap.Totals.LatencyP95)
	}
	// Per-model avg should also exclude the stream sample.
	if len(snap.ByModel) != 1 || snap.ByModel[0].AvgMs > 100 {
		t.Fatalf("stream duration leaked into per-model avg_ms: %+v", snap.ByModel)
	}
	// But both requests are still counted.
	if snap.ByModel[0].Requests != 2 {
		t.Fatalf("expected both requests counted, got %d", snap.ByModel[0].Requests)
	}
}

// TestNonStreamingPassthroughRecordsUsage is a regression test for a bug where
// the near-zero-copy OpenAI chat passthrough branch (non-streaming, no tools)
// never recorded token usage, leaving the dashboard's tokens/sec at zero for
// the most common request shape. The response must stay byte-for-byte identical
// while usage is sniffed into the RequestSummary.
func TestNonStreamingPassthroughRecordsUsage(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		resp := models.OpenAIResponse{
			ID:     "chatcmpl-usage",
			Object: "chat.completion",
			Model:  "gpt-5.4",
			Usage: &models.OpenAIUsage{
				PromptTokens:     123,
				CompletionTokens: 77,
				TotalTokens:      200,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})

	// Non-streaming, no tools => the passthrough branch.
	body := `{"model":"gpt-5.4","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	ctx, summary := WithRequestSummary(req.Context())
	req = req.WithContext(ctx)
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Result().StatusCode)
	}

	// Usage landed in the summary.
	d := readSummaryForStats(summary)
	if d.prompt != 123 || d.completion != 77 || d.total != 200 {
		t.Fatalf("usage not recorded from passthrough: prompt=%d completion=%d total=%d", d.prompt, d.completion, d.total)
	}

	// Response body still carries the upstream usage block (not stripped).
	var got models.OpenAIResponse
	respBody, _ := io.ReadAll(w.Result().Body)
	if err := json.Unmarshal(respBody, &got); err != nil {
		t.Fatalf("response not valid JSON: %v", err)
	}
	if got.Usage == nil || got.Usage.TotalTokens != 200 {
		t.Fatalf("response usage altered: %+v", got.Usage)
	}
}
