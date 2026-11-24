package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/sozercan/vekil/models"
)

const (
	insightUpstreamTimeout = 30 * time.Second
	insightMaxTokens       = 320
	insightCooldown        = 5 * time.Second
)

type insightGate struct {
	mu       sync.Mutex
	active   bool
	lastDone time.Time
	now      func() time.Time
}

func newInsightGate() *insightGate {
	return &insightGate{now: time.Now}
}

func (g *insightGate) tryAcquire() (ok bool, reason string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.active {
		return false, "an insight is already being generated"
	}
	now := g.now()
	if !g.lastDone.IsZero() && now.Sub(g.lastDone) < insightCooldown {
		wait := insightCooldown - now.Sub(g.lastDone)
		return false, fmt.Sprintf("cooling down, try again in %ds", int(wait.Seconds())+1)
	}
	g.active = true
	return true, ""
}

func (g *insightGate) release() {
	g.mu.Lock()
	g.active = false
	g.lastDone = g.now()
	g.mu.Unlock()
}

func (h *ProxyHandler) insightGateFor() *insightGate {
	h.insightGateOnce.Do(func() { h.insightGate = newInsightGate() })
	return h.insightGate
}

type insightResponse struct {
	Insight string `json:"insight,omitempty"`
	Model   string `json:"model,omitempty"`
	Error   string `json:"error,omitempty"`
}

type captureResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func newCaptureResponseWriter() *captureResponseWriter {
	return &captureResponseWriter{header: make(http.Header), status: http.StatusOK}
}

func (c *captureResponseWriter) Header() http.Header { return c.header }
func (c *captureResponseWriter) Write(p []byte) (int, error) {
	return c.body.Write(p)
}
func (c *captureResponseWriter) WriteHeader(status int) { c.status = status }

func dashboardInsightRequestAllowed(r *http.Request) bool {
	if r == nil {
		return false
	}
	switch r.Header.Get("Sec-Fetch-Site") {
	case "", "same-origin", "none":
	case "same-site", "cross-site", "cross-origin":
		return false
	default:
		return false
	}
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin != "" {
		return requestOriginMatchesHost(origin, r.Host)
	}
	referer := strings.TrimSpace(r.Header.Get("Referer"))
	if referer != "" {
		return requestOriginMatchesHost(referer, r.Host)
	}
	return true
}

func requestOriginMatchesHost(originOrReferer, host string) bool {
	parsed, err := url.Parse(originOrReferer)
	if err != nil || parsed.Host == "" {
		return false
	}
	return strings.EqualFold(parsed.Host, host)
}

func (h *ProxyHandler) HandleDashboardInsight(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")

	if h == nil || h.stats == nil {
		writeInsightError(w, "stats unavailable")
		return
	}
	model := strings.TrimSpace(h.providersConfig.InsightModel)
	if model == "" {
		writeInsightError(w, "no insight model configured")
		return
	}

	if !dashboardInsightRequestAllowed(r) {
		writeInsightError(w, "cross-site insight requests are not allowed")
		return
	}

	gate := h.insightGateFor()
	if ok, reason := gate.tryAcquire(); !ok {
		writeInsightError(w, reason)
		return
	}

	var reqIn struct {
		Shown string `json:"shown"`
	}
	if r.Body != nil {
		dec := json.NewDecoder(io.LimitReader(r.Body, 8<<10))
		_ = dec.Decode(&reqIn)
	}

	snap := h.stats.snapshot()
	prompt := buildInsightPrompt(snap, strings.TrimSpace(reqIn.Shown))

	maxTokens := insightMaxTokens
	reqBody, err := json.Marshal(models.OpenAIRequest{
		Model: model,
		Messages: []models.OpenAIMessage{
			{Role: "system", Content: jsonString(insightSystemPrompt)},
			{Role: "user", Content: jsonString(prompt)},
		},
		MaxTokens: &maxTokens,
	})
	if err != nil {
		gate.release()
		writeInsightError(w, "failed to build insight request")
		return
	}

	innerReq, err := http.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(reqBody))
	if err != nil {
		gate.release()
		writeInsightError(w, "failed to build insight request")
		return
	}
	innerReq = innerReq.WithContext(withTaskInferenceKind(suppressRouteAttemptStats(innerReq.Context()), taskInsight))
	innerReq.Header.Set("Content-Type", "application/json")
	innerReq.Header.Set("User-Agent", "vekil-dashboard-insight")

	type insightResult struct {
		text   string
		status int
	}
	done := make(chan insightResult, 1)
	if !h.beginLifecycleWorker() {
		gate.release()
		writeInsightError(w, "server shutting down")
		return
	}
	go func() {
		defer h.endLifecycleWorker()
		defer gate.release()
		rec := newCaptureResponseWriter()
		h.HandleOpenAIChatCompletions(rec, innerReq)
		done <- insightResult{text: extractOpenAIReplyText(rec.body.Bytes()), status: rec.status}
	}()

	select {
	case res := <-done:
		if res.status != http.StatusOK {
			writeInsightError(w, fmt.Sprintf("insight model returned %d", res.status))
			return
		}
		if res.text == "" {
			writeInsightError(w, "insight model returned no text")
			return
		}
		_ = json.NewEncoder(w).Encode(insightResponse{Insight: res.text, Model: model})
	case <-time.After(insightUpstreamTimeout):
		writeInsightError(w, "insight timed out")
	case <-r.Context().Done():
		writeInsightError(w, "request cancelled")
	}
}

func writeInsightError(w http.ResponseWriter, msg string) {
	_ = json.NewEncoder(w).Encode(insightResponse{Error: msg})
}

func jsonString(s string) json.RawMessage {
	b, _ := json.Marshal(s)
	return b
}

func extractOpenAIReplyText(body []byte) string {
	var resp models.OpenAIResponse
	if err := json.Unmarshal(body, &resp); err != nil || len(resp.Choices) == 0 {
		return ""
	}
	content := resp.Choices[0].Message.Content
	if len(content) == 0 {
		return ""
	}
	var s string
	if err := json.Unmarshal(content, &s); err == nil {
		return strings.TrimSpace(s)
	}
	var parts []models.OpenAIContentPart
	if err := json.Unmarshal(content, &parts); err == nil {
		var b strings.Builder
		for _, p := range parts {
			if p.Type == "text" && p.Text != nil {
				b.WriteString(*p.Text)
			}
		}
		return strings.TrimSpace(b.String())
	}
	return ""
}

const insightSystemPrompt = "You are an observability analyst embedded in a local LLM reverse-proxy dashboard. " +
	"You are given a metrics snapshot and the one-line summary the dashboard is ALREADY showing the user. " +
	"Your job is to add what that summary does NOT say — not to rephrase it. " +
	"Do not restate headline numbers the user can already see (total throughput, p95 latency, overall cache %, overall error rate, the single busiest model). " +
	"Instead surface non-obvious analysis: which specific model or provider is disproportionately responsible for the errors or the latency, how output spend compares to input spend, whether reasoning or cached tokens are a meaningful share of cost, whether request volume is trending up or down over the window, and any imbalance between models/agents worth noticing. " +
	"End with one concrete, specific thing the operator should check or do. " +
	"Write 2-4 sentences of plain prose. Be specific with numbers, but only numbers that ADD information. No preamble, no markdown, no bullet lists, no headings."

func buildInsightPrompt(snap statsSnapshot, shown string) string {
	t := snap.Totals
	var b strings.Builder

	if shown != "" {
		b.WriteString("ALREADY_SHOWN_TO_USER (do not repeat this — add to it):\n")
		b.WriteString("  " + sanitizeLabel(shown) + "\n\n")
	}

	b.WriteString("SNAPSHOT:\n")
	fmt.Fprintf(&b, "uptime_seconds: %d\n", snap.UptimeSeconds)
	fmt.Fprintf(&b, "inflight: %d\n", snap.Inflight)
	fmt.Fprintf(&b, "totals: requests=%d errors=%d prompt_tokens=%d completion_tokens=%d total_tokens=%d cached_tokens=%d reasoning_tokens=%d\n",
		t.Requests, t.Errors, t.PromptTokens, t.CompletionTokens, t.TotalTokens, t.CachedTokens, t.ReasoningTokens)
	fmt.Fprintf(&b, "latency_ms: p50=%d p95=%d p99=%d\n", t.LatencyP50, t.LatencyP95, t.LatencyP99)

	b.WriteString("derived:\n")
	if t.Requests > 0 {
		fmt.Fprintf(&b, "  error_rate_pct: %.1f\n", pct(t.Errors, t.Requests))
		fmt.Fprintf(&b, "  avg_total_tokens_per_request: %d\n", t.TotalTokens/t.Requests)
	}
	if t.PromptTokens > 0 {
		fmt.Fprintf(&b, "  output_to_input_ratio: %.2f\n", ratio(t.CompletionTokens, t.PromptTokens))
		fmt.Fprintf(&b, "  cached_pct_of_prompt: %.1f\n", pct(t.CachedTokens, t.PromptTokens))
	}
	if t.CompletionTokens > 0 {
		fmt.Fprintf(&b, "  reasoning_pct_of_completion: %.1f\n", pct(t.ReasoningTokens, t.CompletionTokens))
	}
	if trend := requestTrend(snap.Series); trend != "" {
		fmt.Fprintf(&b, "  request_rate_trend: %s\n", trend)
	}

	b.WriteString("top_models (share_pct = % of all requests):\n")
	for _, m := range topN(snap.ByModel, 5) {
		fmt.Fprintf(&b, "  - %s: requests=%d share_pct=%.0f tokens=%d errors=%d error_rate_pct=%.1f avg_ms=%d\n",
			sanitizeLabel(m.Model), m.Requests, pct(m.Requests, t.Requests), m.Tokens, m.Errors, pct(m.Errors, m.Requests), m.AvgMs)
	}
	b.WriteString("top_agents:\n")
	for _, a := range topN(snap.ByAgent, 5) {
		fmt.Fprintf(&b, "  - %s: requests=%d share_pct=%.0f tokens=%d\n", sanitizeLabel(a.Agent), a.Requests, pct(a.Requests, t.Requests), a.Tokens)
	}
	b.WriteString("providers:\n")
	for _, p := range snap.ByProvider {
		fmt.Fprintf(&b, "  - %s (%s): requests=%d tokens=%d errors=%d error_rate_pct=%.1f\n",
			sanitizeLabel(p.Provider), sanitizeLabel(p.Kind), p.Requests, p.Tokens, p.Errors, pct(p.Errors, p.Requests))
	}
	if len(snap.StatusCodes) > 0 {
		b.WriteString("error_status_codes:\n")
		for _, e := range snap.StatusCodes {
			fmt.Fprintf(&b, "  - %s: %d\n", sanitizeLabel(e.Label), e.Count)
		}
	}
	if len(snap.Errors) > 0 {
		b.WriteString("errors_by_target:\n")
		for _, e := range snap.Errors {
			fmt.Fprintf(&b, "  - %s: %d\n", sanitizeLabel(e.Label), e.Count)
		}
	}
	return b.String()
}

func sanitizeLabel(s string) string {
	const maxLen = 80
	s = strings.Map(func(r rune) rune {
		if r == '\n' || r == '\r' || r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.TrimSpace(s)
	if r := []rune(s); len(r) > maxLen {
		s = string(r[:maxLen]) + "…"
	}
	return s
}

func pct(part, whole int64) float64 {
	if whole <= 0 {
		return 0
	}
	return float64(part) / float64(whole) * 100
}

func ratio(a, b int64) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b)
}

func requestTrend(series []statsSeriesPoint) string {
	if len(series) < 4 {
		return ""
	}
	half := len(series) / 2
	var first, second int64
	for i, p := range series {
		if i < half {
			first += p.Req
		} else {
			second += p.Req
		}
	}
	if first == 0 && second == 0 {
		return ""
	}
	if first == 0 {
		return "rising"
	}
	change := (float64(second) - float64(first)) / float64(first) * 100
	switch {
	case change > 25:
		return fmt.Sprintf("rising (+%.0f%% over window)", change)
	case change < -25:
		return fmt.Sprintf("falling (%.0f%% over window)", change)
	default:
		return "steady"
	}
}

func topN(rows []statsBreakdown, n int) []statsBreakdown {
	if len(rows) > n {
		return rows[:n]
	}
	return rows
}
