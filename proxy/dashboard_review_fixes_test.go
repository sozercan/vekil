package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/vekil/models"
)

// TestHandleDashboardInsightRejectsCrossSite covers the CSRF guard: the
// token-spending insight endpoint must reject a cross-site browser request
// (Sec-Fetch-Site: cross-site) before attempting any generation, while allowing
// same-origin requests and non-browser callers (absent header) through to the
// rate-limit gate.
func TestHandleDashboardInsightRejectsCrossSite(t *testing.T) {
	h := &ProxyHandler{
		stats:           newStatsCollector(),
		providersConfig: ProvidersConfig{InsightModel: "claude-opus-4.8"},
	}

	decode := func(t *testing.T, body io.Reader) insightResponse {
		t.Helper()
		var out insightResponse
		if err := json.NewDecoder(body).Decode(&out); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return out
	}

	// Cross-site → rejected with a clear error, always 200 (soft fail).
	req := httptest.NewRequest(http.MethodPost, "/dashboard/insight", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	w := httptest.NewRecorder()
	h.HandleDashboardInsight(w, req)
	if w.Result().StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200", w.Result().StatusCode)
	}
	out := decode(t, w.Result().Body)
	if !strings.Contains(out.Error, "cross-site") {
		t.Fatalf("expected cross-site rejection, got error=%q insight=%q", out.Error, out.Insight)
	}

	// cross-origin is also rejected.
	req2 := httptest.NewRequest(http.MethodPost, "/dashboard/insight", nil)
	req2.Header.Set("Sec-Fetch-Site", "cross-origin")
	w2 := httptest.NewRecorder()
	h.HandleDashboardInsight(w2, req2)
	if got := decode(t, w2.Result().Body); !strings.Contains(got.Error, "cross-site") {
		t.Fatalf("expected cross-origin to be rejected, got error=%q", got.Error)
	}

	// same-origin passes the CSRF guard. To prove it cleared the guard without
	// triggering a real (nil-upstream) generation, pre-acquire the rate-limit
	// gate that runs immediately after the guard: an allowed request then stops
	// at the gate with a "generation in progress" reason, never the cross-site
	// error.
	h.insightGateFor().tryAcquire() // hold the gate
	req3 := httptest.NewRequest(http.MethodPost, "/dashboard/insight", nil)
	req3.Header.Set("Sec-Fetch-Site", "same-origin")
	w3 := httptest.NewRecorder()
	h.HandleDashboardInsight(w3, req3)
	if got := decode(t, w3.Result().Body); strings.Contains(got.Error, "cross-site") {
		t.Fatalf("same-origin must not be rejected by CSRF guard, got error=%q", got.Error)
	}

	// Absent header (curl / non-browser) is allowed past the CSRF guard too (also
	// stopped at the held gate, not the CSRF guard).
	req4 := httptest.NewRequest(http.MethodPost, "/dashboard/insight", nil)
	w4 := httptest.NewRecorder()
	h.HandleDashboardInsight(w4, req4)
	if got := decode(t, w4.Result().Body); strings.Contains(got.Error, "cross-site") {
		t.Fatalf("absent Sec-Fetch-Site must not be rejected, got error=%q", got.Error)
	}
}

// TestLogRetryAttemptCountsOnlyTrackedRequests covers that retry accounting is
// gated to tracked inference requests via the positive retry-stats context
// marker: a marked (tracked) context counts, an unmarked one (insight call,
// model-catalog fetch, count-token probe, proxy shim) does not. The marker is a
// positive allow-signal rather than an exclusion signal because the upstream
// request context is rebuilt from context.Background(), which strips any
// exclusion marker set on the inbound request.
func TestLogRetryAttemptCountsOnlyTrackedRequests(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}

	// Unmarked context (non-tracked caller) → not counted.
	h.logRetryAttempt(context.Background(), 0, 503, "", 0, nil)
	if got := h.stats.snapshot().Retries; got != 0 {
		t.Fatalf("unmarked retry should not be counted: got %d want 0", got)
	}

	// Tracked-marked context → counted.
	trackedCtx := markRetryStatsTracked(context.Background())
	h.logRetryAttempt(trackedCtx, 0, 503, "", 0, nil)
	if got := h.stats.snapshot().Retries; got != 1 {
		t.Fatalf("tracked retry not counted: got %d want 1", got)
	}
}

// TestNewInferenceUpstreamContextPropagatesTrackedMarker covers that the upstream
// context inherits the retry-stats marker from a tracked inbound context (so
// retries are counted) but not from an unmarked one (so insight / catalog /
// probe retries stay uncounted) — the background root would otherwise strip it.
func TestNewInferenceUpstreamContextPropagatesTrackedMarker(t *testing.T) {
	h := &ProxyHandler{}

	tracked := markRetryStatsTracked(context.Background())
	up, cancel := h.newInferenceUpstreamContextFrom(tracked, false)
	defer cancel()
	if !isRetryStatsTracked(up) {
		t.Fatal("tracked marker should propagate to the upstream context")
	}

	up2, cancel2 := h.newInferenceUpstreamContextFrom(context.Background(), false)
	defer cancel2()
	if isRetryStatsTracked(up2) {
		t.Fatal("unmarked inbound context must not yield a tracked upstream context")
	}

	// The legacy no-arg constructor never marks (used by non-tracked probes).
	up3, cancel3 := h.newInferenceUpstreamContext(false)
	defer cancel3()
	if isRetryStatsTracked(up3) {
		t.Fatal("newInferenceUpstreamContext must not mark tracked")
	}
}

// TestMarkRetryStatsTrackedIfInference covers that only inference routes acquire
// the marker through the middleware helper.
func TestMarkRetryStatsTrackedIfInference(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	if ctx := h.MarkRetryStatsTrackedIfInference(context.Background(), http.MethodPost, "/v1/chat/completions"); !isRetryStatsTracked(ctx) {
		t.Fatal("inference route should be marked tracked")
	}
	if ctx := h.MarkRetryStatsTrackedIfInference(context.Background(), http.MethodGet, "/v1/models"); isRetryStatsTracked(ctx) {
		t.Fatal("GET /v1/models must not be marked tracked")
	}
	if ctx := h.MarkRetryStatsTrackedIfInference(context.Background(), http.MethodPost, "/v1/messages/count_tokens"); isRetryStatsTracked(ctx) {
		t.Fatal("count_tokens must not be marked tracked")
	}
}

// TestObserveAnthropicUsageBody covers usage capture for the direct
// anthropic-compatible passthrough: Anthropic input/output tokens map onto the
// chat-shaped fields the dashboard reads. input_tokens counts only non-cached
// tokens, so cache read + creation are folded into the prompt/total (cache-read
// also surfaced as the cached detail).
func TestObserveAnthropicUsageBody(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	body := []byte(`{"id":"msg_1","type":"message","model":"claude-x","usage":{"input_tokens":120,"output_tokens":40,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}}`)

	observeAnthropicUsageBody(ctx, body)

	// prompt = input(120) + cache_read(30) + cache_creation(10) = 160
	if summary.promptTokens == nil || *summary.promptTokens != 160 {
		t.Fatalf("promptTokens = %v want 160 (input + cache read + cache creation)", summary.promptTokens)
	}
	if summary.completionTokens == nil || *summary.completionTokens != 40 {
		t.Fatalf("completionTokens = %v want 40", summary.completionTokens)
	}
	if summary.totalTokens == nil || *summary.totalTokens != 200 {
		t.Fatalf("totalTokens = %v want 200", summary.totalTokens)
	}
	// cached detail must not exceed prompt total (the bug this fixes).
	if summary.cachedTokens == nil || *summary.cachedTokens != 30 {
		t.Fatalf("cachedTokens = %v want 30", summary.cachedTokens)
	}
	if *summary.cachedTokens > *summary.promptTokens {
		t.Fatalf("cached(%d) must not exceed prompt(%d)", *summary.cachedTokens, *summary.promptTokens)
	}
}

// TestObserveAnthropicUsageBodyCacheOnly covers a pure cache hit (a large cached
// prefix with a tiny uncached suffix), which previously undercounted to near
// zero and could make cached% exceed 100%.
func TestObserveAnthropicUsageBodyCacheOnly(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	// 100k cache read, 50-token uncached suffix, no output yet.
	body := []byte(`{"usage":{"input_tokens":50,"output_tokens":0,"cache_read_input_tokens":100000}}`)
	observeAnthropicUsageBody(ctx, body)
	if summary.promptTokens == nil || *summary.promptTokens != 100050 {
		t.Fatalf("promptTokens = %v want 100050", summary.promptTokens)
	}
	if summary.cachedTokens == nil || *summary.cachedTokens != 100000 {
		t.Fatalf("cachedTokens = %v want 100000", summary.cachedTokens)
	}
	if *summary.cachedTokens > *summary.promptTokens {
		t.Fatalf("cached(%d) must not exceed prompt(%d)", *summary.cachedTokens, *summary.promptTokens)
	}
}

// TestObserveAnthropicUsageBodyIgnoresEmpty covers that a body with no usage (or
// all-zero usage) does not overwrite a prior observation with zeros.
func TestObserveAnthropicUsageBodyIgnoresEmpty(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	observeAnthropicUsageBody(ctx, []byte(`{"id":"msg_1","type":"message"}`))
	if summary.totalTokens != nil {
		t.Fatalf("expected no usage observed, got total=%v", *summary.totalTokens)
	}
	observeAnthropicUsageBody(ctx, []byte(`{not json`))
	if summary.totalTokens != nil {
		t.Fatal("malformed body must not record usage")
	}
}

// TestBoundStatLabel covers the client-supplied label length bound (model name
// / agent token) used to keep oversized strings out of the breakdown stats.
func TestBoundStatLabel(t *testing.T) {
	if got := boundStatLabel("claude-sonnet-4.5"); got != "claude-sonnet-4.5" {
		t.Fatalf("short label changed: %q", got)
	}
	long := strings.Repeat("z", statLabelMaxLen+50)
	got := boundStatLabel(long)
	if len([]rune(got)) != statLabelMaxLen+1 { // +1 for the ellipsis rune
		t.Fatalf("bounded label length = %d runes, want %d", len([]rune(got)), statLabelMaxLen+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("bounded label should end with ellipsis, got %q", got)
	}
	// A label exactly at the cap is left untouched.
	exact := strings.Repeat("a", statLabelMaxLen)
	if got := boundStatLabel(exact); got != exact {
		t.Fatalf("label at cap should be unchanged, got %q", got)
	}
}

// TestReadSummaryForStatsBoundsModel covers that a record carrying an oversized
// client-supplied model name is truncated before landing in the breakdown maps,
// while provider/kind (server-configured, trusted) pass through untouched.
func TestReadSummaryForStatsBoundsModel(t *testing.T) {
	c := newStatsCollector()
	hugeModel := strings.Repeat("m", 5000)
	s := &RequestSummary{}
	s.setRoute("/v1/chat/completions", hugeModel, false)
	s.setProvider("copilot", "copilot")
	s.setOpenAIUsage(&models.OpenAIUsage{PromptTokens: 1, CompletionTokens: 1, TotalTokens: 2})

	c.record(s, 200, "curl/8", 0)

	snap := c.snapshot()
	if len(snap.ByModel) != 1 {
		t.Fatalf("by_model rows: got %d want 1", len(snap.ByModel))
	}
	if got := len([]rune(snap.ByModel[0].Model)); got > statLabelMaxLen+1 {
		t.Fatalf("stored model label not bounded: %d runes", got)
	}
}

// TestStreamOpenAIToAnthropicCapturesUsage covers that the Anthropic streaming
// translation feeds the upstream usage chunk to the usage callback so streamed
// Anthropic traffic records tokens.
func TestStreamOpenAIToAnthropicCapturesUsage(t *testing.T) {
	content := models.OpenAIStreamChunk{
		ID:      "chatcmpl-a",
		Object:  "chat.completion.chunk",
		Model:   "gpt-4o",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{Content: json.RawMessage(`"hi"`)}}},
	}
	usageOnly := models.OpenAIStreamChunk{
		ID:     "chatcmpl-a",
		Object: "chat.completion.chunk",
		Model:  "gpt-4o",
		Usage:  &models.OpenAIUsage{PromptTokens: 8, CompletionTokens: 2, TotalTokens: 10},
	}
	body := buildSSEStream(mustMarshal(t, content), mustMarshal(t, usageOnly), "[DONE]")

	w := httptest.NewRecorder()
	var captured *models.OpenAIUsage
	StreamOpenAIToAnthropicWithFinalResponse(w, body, "claude-x", "msg_test", nil, func(u *models.OpenAIUsage) {
		captured = u
	})
	if captured == nil || captured.TotalTokens != 10 {
		t.Fatalf("anthropic stream usage callback = %#v, want total 10", captured)
	}
}

// TestStreamOpenAIToGeminiCapturesUsage covers the same for Gemini streaming.
func TestStreamOpenAIToGeminiCapturesUsage(t *testing.T) {
	content := models.OpenAIStreamChunk{
		ID:      "chatcmpl-g",
		Object:  "chat.completion.chunk",
		Model:   "gpt-4o",
		Choices: []models.OpenAIStreamChoice{{Index: 0, Delta: models.OpenAIMessage{Content: json.RawMessage(`"hi"`)}}},
	}
	usageOnly := models.OpenAIStreamChunk{
		ID:     "chatcmpl-g",
		Object: "chat.completion.chunk",
		Model:  "gpt-4o",
		Usage:  &models.OpenAIUsage{PromptTokens: 6, CompletionTokens: 3, TotalTokens: 9},
	}
	body := buildSSEStream(mustMarshal(t, content), mustMarshal(t, usageOnly), "[DONE]")

	w := httptest.NewRecorder()
	var captured *models.OpenAIUsage
	StreamOpenAIToGeminiWithFinalResponse(w, body, nil, func(u *models.OpenAIUsage) {
		captured = u
	})
	if captured == nil || captured.TotalTokens != 9 {
		t.Fatalf("gemini stream usage callback = %#v, want total 9", captured)
	}
}

// TestAnthropicStreamUsageAccumulator covers usage capture from an Anthropic
// Messages SSE stream (finding: direct-Anthropic streaming recorded zero
// tokens). Input + cache read/write come from message_start; the running output
// total comes from the last message_delta. input_tokens is non-cached only, so
// cache tokens are folded into the prompt total.
func TestAnthropicStreamUsageAccumulator(t *testing.T) {
	acc := &anthropicStreamUsageAccumulator{}
	acc.observe([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":120,"cache_read_input_tokens":30,"cache_creation_input_tokens":10,"output_tokens":1}}}`))
	acc.observe([]byte(`{"type":"content_block_delta","delta":{"text":"hi"}}`))
	acc.observe([]byte(`{"type":"message_delta","usage":{"output_tokens":7}}`))
	acc.observe([]byte(`{"type":"message_delta","usage":{"output_tokens":42}}`)) // last wins

	ctx, summary := WithRequestSummary(context.Background())
	acc.flush(ctx)

	// prompt = input(120) + cache_read(30) + cache_creation(10) = 160
	if summary.promptTokens == nil || *summary.promptTokens != 160 {
		t.Fatalf("promptTokens = %v want 160 (input + cache read + cache creation)", summary.promptTokens)
	}
	if summary.completionTokens == nil || *summary.completionTokens != 42 {
		t.Fatalf("completionTokens = %v want 42 (last message_delta wins)", summary.completionTokens)
	}
	if summary.totalTokens == nil || *summary.totalTokens != 202 {
		t.Fatalf("totalTokens = %v want 202", summary.totalTokens)
	}
	if summary.cachedTokens == nil || *summary.cachedTokens != 30 {
		t.Fatalf("cachedTokens = %v want 30", summary.cachedTokens)
	}
	if *summary.cachedTokens > *summary.promptTokens {
		t.Fatalf("cached(%d) must not exceed prompt(%d)", *summary.cachedTokens, *summary.promptTokens)
	}
}

// TestStreamAnthropicPassthroughObservesUsageOnFastPath covers that usage is
// captured even on the byte-exact no-rewrite path (publicModel == upstreamModel),
// and that the streamed bytes are unchanged.
func TestStreamAnthropicPassthroughObservesUsageOnFastPath(t *testing.T) {
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":9}}` + "\n\n"

	ctx, summary := WithRequestSummary(context.Background())
	w := httptest.NewRecorder()
	// publicModel == upstreamModel => no rewrite, byte-exact fast path.
	streamAnthropicPassthroughBody(ctx, w, strings.NewReader(stream), "claude-x", "claude-x")

	if got := w.Body.String(); got != stream {
		t.Fatalf("fast-path bytes changed:\n got=%q\nwant=%q", got, stream)
	}
	if summary.totalTokens == nil || *summary.totalTokens != 59 {
		t.Fatalf("totalTokens = %v want 59 (usage observed on fast path)", summary.totalTokens)
	}
}

// TestCompactBudgetUsageTotals covers the accessor used to record
// compaction-trigger usage onto the request summary (finding: compaction-trigger
// turns recorded zero tokens).
func TestCompactBudgetUsageTotals(t *testing.T) {
	b := newCompactBudget(3)
	b.addResponsesUsage([]byte(`{"usage":{"input_tokens":100,"output_tokens":40,"total_tokens":140}}`))
	b.addResponsesUsage([]byte(`{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`))

	u := b.usageTotals()
	if u.InputTokens != 110 || u.OutputTokens != 45 || u.TotalTokens != 155 {
		t.Fatalf("usageTotals = %+v want input=110 output=45 total=155", u)
	}

	// Observe onto a summary to confirm it maps onto the chat-shaped fields.
	ctx, summary := WithRequestSummary(context.Background())
	observeResponsesUsage(ctx, u)
	if summary.totalTokens == nil || *summary.totalTokens != 155 {
		t.Fatalf("observed totalTokens = %v want 155", summary.totalTokens)
	}
}

// TestRequestSummaryFailureStatus covers the out-of-band failure status used to
// record post-commit streaming failures (finding: response.failed/incomplete
// after a committed 200 was counted as success).
func TestRequestSummaryFailureStatus(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	if summary.FailureStatus() != 0 {
		t.Fatal("fresh summary should have no failure status")
	}
	observeResponseFailureStatus(ctx, http.StatusBadGateway)
	if summary.FailureStatus() != http.StatusBadGateway {
		t.Fatalf("FailureStatus = %d want 502", summary.FailureStatus())
	}
	// First one wins; a later status does not overwrite.
	observeResponseFailureStatus(ctx, http.StatusInternalServerError)
	if summary.FailureStatus() != http.StatusBadGateway {
		t.Fatalf("FailureStatus changed to %d, want first-wins 502", summary.FailureStatus())
	}
	// Zero is ignored.
	ctx2, summary2 := WithRequestSummary(context.Background())
	observeResponseFailureStatus(ctx2, 0)
	if summary2.FailureStatus() != 0 {
		t.Fatal("zero status should be ignored")
	}
}

// TestStreamAnthropicPassthroughFastPathPreservesOversizedLine guards against a
// regression: the byte-exact no-rewrite path must stream an SSE line larger than
// the usage-sniffer's buffer cap unchanged (usage capture degrades, the client
// copy does not truncate).
func TestStreamAnthropicPassthroughFastPathPreservesOversizedLine(t *testing.T) {
	huge := strings.Repeat("x", openAIStreamScannerMaxBuffer+4096)
	stream := "data: " + huge + "\n\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":5,"output_tokens":2}}}` + "\n\n"

	ctx, summary := WithRequestSummary(context.Background())
	w := httptest.NewRecorder()
	streamAnthropicPassthroughBody(ctx, w, strings.NewReader(stream), "claude-x", "claude-x")

	if got := w.Body.String(); got != stream {
		t.Fatalf("oversized line not preserved on fast path: got %d bytes want %d", len(got), len(stream))
	}
	// Usage from the normal-sized frame after the oversized one is still captured.
	if summary.totalTokens == nil || *summary.totalTokens != 7 {
		t.Fatalf("totalTokens = %v want 7 (usage after oversized line still sniffed)", summary.totalTokens)
	}
}

// TestStreamFailureStatusCarriesClassifiedStatus covers that a stream-failure
// error carrying a classified status (e.g. 429) is recovered by
// streamFailureStatus, and that errors.Is still matches the sentinel so existing
// branching keeps working (round-4: ws/HTTP failures must keep their exact
// status, not collapse to 502).
func TestStreamFailureStatusCarriesClassifiedStatus(t *testing.T) {
	err := &streamFailedUpstreamError{status: http.StatusTooManyRequests}
	if !errors.Is(err, errStreamFailedUpstream) {
		t.Fatal("streamFailedUpstreamError must unwrap to the sentinel")
	}
	if got := streamFailureStatus(err); got != http.StatusTooManyRequests {
		t.Fatalf("streamFailureStatus = %d want 429", got)
	}
	// Bare sentinel (no carried status) falls back to 502.
	if got := streamFailureStatus(errStreamFailedUpstream); got != http.StatusBadGateway {
		t.Fatalf("streamFailureStatus(bare) = %d want 502", got)
	}
	// A zero-status wrapper also falls back to 502.
	if got := streamFailureStatus(&streamFailedUpstreamError{}); got != http.StatusBadGateway {
		t.Fatalf("streamFailureStatus(zero) = %d want 502", got)
	}
}

// TestResponsesWebSocketStreamFailureDetailsStatus covers the classifier shared
// by the websocket and HTTP post-commit failure paths: rate limits map to 429,
// overloads to 503, and incomplete to 409 (round-4: exact status preserved).
func TestResponsesWebSocketStreamFailureDetailsStatus(t *testing.T) {
	mk := func(typ, code string) responsesWebSocketStreamEvent {
		var e responsesWebSocketStreamEvent
		e.Type = typ
		e.Response.Error.Code = code
		return e
	}
	cases := []struct {
		name  string
		event responsesWebSocketStreamEvent
		want  int
	}{
		{"rate-limit", mk("response.failed", "too_many_requests"), http.StatusTooManyRequests},
		{"overload", mk("response.failed", "model_overloaded"), http.StatusServiceUnavailable},
		{"incomplete", mk("response.incomplete", ""), http.StatusConflict},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			status, _, _ := responsesWebSocketStreamFailureDetails(tc.event)
			if status != tc.want {
				t.Fatalf("status = %d want %d", status, tc.want)
			}
		})
	}
}

// TestObserveInternalResponsesUsageAddsToTurnUsage covers that out-of-band
// internal /responses spend (e.g. a 413-fallback compaction call) is folded into
// the stats totals additively, on top of the turn's own usage, rather than being
// clobbered by setOpenAIUsage's overwrite (round-6).
func TestObserveInternalResponsesUsageAddsToTurnUsage(t *testing.T) {
	c := newStatsCollector()
	ctx, summary := WithRequestSummary(context.Background())
	summary.setRoute("/v1/responses", "gpt-5.4", false)
	summary.setProvider("codex", "openai-codex")

	// Internal compaction spend recorded first...
	observeInternalResponsesUsage(ctx, responsesUsage{InputTokens: 700, OutputTokens: 90, TotalTokens: 790})
	// ...then the turn's own usage overwrites the base usage fields.
	observeResponsesUsage(ctx, responsesUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120})

	c.record(summary, 200, "Codex CLI", 0)
	snap := c.snapshot()
	// totals = turn (100+20) + internal (700+90) = 910
	if snap.Totals.TotalTokens != 910 {
		t.Fatalf("total tokens = %d want 910 (turn 120 + internal 790)", snap.Totals.TotalTokens)
	}
	if snap.Totals.PromptTokens != 800 {
		t.Fatalf("prompt tokens = %d want 800 (turn 100 + internal 700)", snap.Totals.PromptTokens)
	}
	if snap.Totals.CompletionTokens != 110 {
		t.Fatalf("completion tokens = %d want 110 (turn 20 + internal 90)", snap.Totals.CompletionTokens)
	}
}

// TestObserveInternalResponsesUsageIgnoresZero covers that a zero internal usage
// is a no-op (no spurious accumulation).
func TestObserveInternalResponsesUsageIgnoresZero(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	observeInternalResponsesUsage(ctx, responsesUsage{})
	if summary.extraPromptTokens != 0 || summary.extraCompletionTokens != 0 {
		t.Fatalf("zero internal usage should not accumulate: prompt=%d completion=%d", summary.extraPromptTokens, summary.extraCompletionTokens)
	}
}
