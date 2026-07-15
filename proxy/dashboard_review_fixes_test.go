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

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

// TestHandleDashboardInsightRejectsCrossSite covers the CSRF guard: the
// token-spending insight endpoint must reject a cross-site or merely same-site
// browser request before attempting any generation, while allowing same-origin
// requests and non-browser callers (absent header) through to the rate-limit gate.
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

	// same-site is still cross-origin and must be rejected.
	reqSameSite := httptest.NewRequest(http.MethodPost, "/dashboard/insight", nil)
	reqSameSite.Header.Set("Sec-Fetch-Site", "same-site")
	wSameSite := httptest.NewRecorder()
	h.HandleDashboardInsight(wSameSite, reqSameSite)
	if got := decode(t, wSameSite.Result().Body); !strings.Contains(got.Error, "cross-site") {
		t.Fatalf("expected same-site to be rejected, got error=%q", got.Error)
	}

	// A present Origin header must match Host even if Fetch Metadata says same-origin.
	reqBadOrigin := httptest.NewRequest(http.MethodPost, "http://dashboard.local/dashboard/insight", nil)
	reqBadOrigin.Header.Set("Sec-Fetch-Site", "same-origin")
	reqBadOrigin.Header.Set("Origin", "http://evil.local")
	wBadOrigin := httptest.NewRecorder()
	h.HandleDashboardInsight(wBadOrigin, reqBadOrigin)
	if got := decode(t, wBadOrigin.Result().Body); !strings.Contains(got.Error, "cross-site") {
		t.Fatalf("expected mismatched Origin to be rejected, got error=%q", got.Error)
	}

	// Older browser/webview requests may omit Sec-Fetch-Site; a cross-site Referer
	// still proves this is not the dashboard origin and must be blocked.
	reqBadReferer := httptest.NewRequest(http.MethodPost, "http://dashboard.local/dashboard/insight", nil)
	reqBadReferer.Header.Set("Referer", "http://evil.local/page")
	wBadReferer := httptest.NewRecorder()
	h.HandleDashboardInsight(wBadReferer, reqBadReferer)
	if got := decode(t, wBadReferer.Result().Body); !strings.Contains(got.Error, "cross-site") {
		t.Fatalf("expected mismatched Referer to be rejected, got error=%q", got.Error)
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
// request context is rebuilt from the proxy lifecycle root, which strips any
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
// probe retries stay uncounted) — the detached lifecycle root would otherwise
// strip it.
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
	StreamOpenAIToAnthropicWithFinalResponse(w, body, "claude-x", "msg_test", nil, nil, func(u *models.OpenAIUsage) {
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
	StreamOpenAIToGeminiWithFinalResponse(w, body, nil, nil, func(u *models.OpenAIUsage) {
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

// TestStreamAnthropicPassthroughMarksCleanEOFBeforeMessageStop covers direct
// Anthropic-compatible streams that close cleanly before the terminal
// message_stop event. Transport EOF is nil in this case, but the SSE protocol
// stream is truncated and should be recorded as a failed turn.
func TestStreamAnthropicPassthroughMarksCleanEOFBeforeMessageStop(t *testing.T) {
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","model":"claude-upstream","content":[],"usage":{"input_tokens":5,"output_tokens":0}}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}` + "\n\n"

	t.Run("byte-exact", func(t *testing.T) {
		ctx, summary := WithRequestSummary(context.Background())
		w := httptest.NewRecorder()
		streamAnthropicPassthroughBody(ctx, w, strings.NewReader(stream), "claude-upstream", "claude-upstream")
		if summary.FailureStatus() != http.StatusBadGateway {
			t.Fatalf("FailureStatus = %d want 502", summary.FailureStatus())
		}
		if got := w.Body.String(); got != stream {
			t.Fatalf("byte-exact stream changed: got %q want %q", got, stream)
		}
	})

	t.Run("model-rewrite", func(t *testing.T) {
		ctx, summary := WithRequestSummary(context.Background())
		w := httptest.NewRecorder()
		streamAnthropicPassthroughBody(ctx, w, strings.NewReader(stream), "claude-public", "claude-upstream")
		if summary.FailureStatus() != http.StatusBadGateway {
			t.Fatalf("FailureStatus = %d want 502", summary.FailureStatus())
		}
	})
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
			status, _, _ := responsesWebSocketStreamFailureDetails(tc.event, nil)
			if status != tc.want {
				t.Fatalf("status = %d want %d", status, tc.want)
			}
		})
	}
}

func TestResponsesWebSocketStreamFailureDetailsUsesQuotaHeadersForUncodedFailure(t *testing.T) {
	var event responsesWebSocketStreamEvent
	event.Type = "response.failed"
	event.Response.Error.Message = "Your requests have exceeded rate limit."
	headers := http.Header{
		"retry-after-ms":               []string{"2169"},
		"x-ratelimit-remaining-tokens": []string{"-36161"},
	}
	status, message, code := responsesWebSocketStreamFailureDetails(event, headers)
	if status != http.StatusTooManyRequests || message != event.Response.Error.Message || code != "" {
		t.Fatalf("failure details = (%d, %q, %q), want (429, %q, empty)", status, message, code, event.Response.Error.Message)
	}
}

func TestResponsesWebSocketStreamFailureDetailsPreservesExplicitFailureOverQuotaHeaders(t *testing.T) {
	var event responsesWebSocketStreamEvent
	event.Type = "response.failed"
	event.Response.Error.Type = "invalid_request_error"
	event.Response.Error.Code = "context_length_exceeded"
	event.Response.Error.Message = "too long"
	headers := http.Header{
		"retry-after-ms":               []string{"2169"},
		"x-ratelimit-remaining-tokens": []string{"-36161"},
	}
	status, message, code := responsesWebSocketStreamFailureDetails(event, headers)
	if status != http.StatusBadRequest || message != "too long" || code != "context_length_exceeded" {
		t.Fatalf("failure details = (%d, %q, %q), want (400, too long, context_length_exceeded)", status, message, code)
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

// TestStreamOpenAIChatPassthroughDetectsErrorEvent covers that a post-commit
// upstream stream error (event: error or data: {"error":...}) fires the onError
// callback so the chat request is recorded as a failure even after its 200 was
// committed (round-7).
func TestStreamOpenAIChatPassthroughDetectsErrorEvent(t *testing.T) {
	cases := []struct {
		name   string
		stream string
	}{
		{
			name:   "error event",
			stream: "event: error\ndata: {\"error\":{\"type\":\"server_error\",\"message\":\"boom\"}}\n\n",
		},
		{
			name:   "error in data",
			stream: "data: {\"error\":{\"message\":\"rate limited\",\"code\":\"too_many_requests\"}}\n\n",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := io.NopCloser(strings.NewReader(tc.stream))
			w := httptest.NewRecorder()
			errored := false
			StreamOpenAIChatPassthrough(w, body, "gpt-4", false, func(int) { errored = true }, nil, func(*models.OpenAIUsage) {})
			if !errored {
				t.Fatalf("expected onError to fire for %q", tc.name)
			}
		})
	}

	// A normal content stream must NOT fire onError.
	body := io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	w := httptest.NewRecorder()
	errored := false
	StreamOpenAIChatPassthrough(w, body, "gpt-4", false, func(int) { errored = true }, nil, func(*models.OpenAIUsage) {})
	if errored {
		t.Fatal("onError must not fire for a normal content stream")
	}
}

// TestExtractResponsesUsageObject covers recovering usage from a large
// response.completed buffer that exceeds the failure tap's buffer cap before its
// delimiter (round-7): the usage object embedded after a big output is found and
// parsed.
func TestExtractResponsesUsageObject(t *testing.T) {
	bigOutput := strings.Repeat("x", 4096)
	// Simulate a partially-buffered SSE data line: huge output then usage, no
	// closing of the outer object (delimiter not yet seen).
	buf := []byte(`data: {"type":"response.completed","response":{"id":"r1","output":"` + bigOutput +
		`","usage":{"input_tokens":1234,"output_tokens":56,"total_tokens":1290}}`)
	u, ok := extractResponsesUsageObject(buf)
	if !ok {
		t.Fatal("expected to extract usage from oversized buffer")
	}
	if u.InputTokens != 1234 || u.OutputTokens != 56 || u.TotalTokens != 1290 {
		t.Fatalf("usage = %+v want input=1234 output=56 total=1290", u)
	}

	// No usage object → ok=false.
	if _, ok := extractResponsesUsageObject([]byte(`{"type":"response.output_text.delta","delta":"hi"}`)); ok {
		t.Fatal("expected ok=false when no usage object present")
	}
	// Zero usage → ok=false (treated as no usage).
	if _, ok := extractResponsesUsageObject([]byte(`{"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`)); ok {
		t.Fatal("expected ok=false for zero usage")
	}
}

// TestBalancedJSONObject covers the brace matcher used by the usage extractor,
// including braces inside strings and escaped quotes.
func TestBalancedJSONObject(t *testing.T) {
	in := []byte(`{"a":{"b":"}{"},"c":"\"}"}TRAILER`)
	obj, end := balancedJSONObject(in)
	if string(obj) != `{"a":{"b":"}{"},"c":"\"}"}` {
		t.Fatalf("balanced object = %q", string(obj))
	}
	if string(in[end:]) != "TRAILER" {
		t.Fatalf("trailer = %q want TRAILER", string(in[end:]))
	}
	// Unclosed object → (nil, 0).
	if _, end := balancedJSONObject([]byte(`{"a":1`)); end != 0 {
		t.Fatalf("unclosed object should return end=0, got %d", end)
	}
}

// TestResponsesFailureTapRecoversUsageFromOversizedCompleted covers that the tap
// recovers usage from a response.completed event whose buffered bytes exceed the
// tap's buffer cap before its delimiter arrives (round-7): the overflow path
// best-effort sniffs the usage object instead of dropping it to zero.
func TestResponsesFailureTapRecoversUsageFromOversizedCompleted(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	tap := newResponsesFailureTap(ctx, &ProxyHandler{}, nil, nil, "")

	// A response.completed data line larger than the buffer cap, with usage near
	// the end, and no terminating blank line yet (delimiter not seen).
	bigOutput := strings.Repeat("y", responsesFailureTapMaxBuffer+8192)
	line := `data: {"type":"response.completed","response":{"id":"r1","output":"` + bigOutput +
		`","usage":{"input_tokens":900,"output_tokens":80,"total_tokens":980}}}` + "\n"
	if _, err := tap.Write([]byte(line)); err != nil {
		t.Fatalf("tap.Write: %v", err)
	}

	if summary.totalTokens == nil || *summary.totalTokens != 980 {
		t.Fatalf("totalTokens = %v want 980 (usage recovered from oversized completed)", summary.totalTokens)
	}
}

// TestStreamResponsesPipeMarksFailureOnCopyError covers that a broken upstream
// SSE stream (the io.Copy returns an error before any response.failed event)
// records an out-of-band failure status, so a truncated stream after a committed
// 200 is not counted as a 2xx success (round-7).
func TestStreamResponsesPipeMarksFailureOnCopyError(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	// A reader that yields a little data then errors (simulating a reset).
	r := &erroringReader{data: []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n")}
	w := httptest.NewRecorder()

	streamResponsesPipeWithFailureLog(ctx, &ProxyHandler{log: logger.New(logger.LevelError)}, w, r, nil, nil, "")

	if summary.FailureStatus() != http.StatusBadGateway {
		t.Fatalf("FailureStatus = %d want 502 (broken stream after commit)", summary.FailureStatus())
	}
}

// TestStreamResponsesPipeMarksFailureOnCleanEOFBeforeTerminal covers a cleanly
// closed upstream stream that never emits response.completed/failed/incomplete.
// Even though io.Copy returns nil, the committed Responses stream is truncated
// at the protocol level and must not be counted as a successful 200.
func TestStreamResponsesPipeMarksFailureOnCleanEOFBeforeTerminal(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	r := io.NopCloser(strings.NewReader("event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-clean-eof\"}}\n\n"))
	w := httptest.NewRecorder()

	streamResponsesPipeWithFailureLog(ctx, &ProxyHandler{log: logger.New(logger.LevelError)}, w, r, nil, nil, "")

	if summary.FailureStatus() != http.StatusBadGateway {
		t.Fatalf("FailureStatus = %d want 502 (clean EOF before terminal Responses event)", summary.FailureStatus())
	}
}

type erroringReader struct {
	data []byte
	done bool
}

func (e *erroringReader) Read(p []byte) (int, error) {
	if !e.done {
		e.done = true
		n := copy(p, e.data)
		return n, nil
	}
	return 0, io.ErrUnexpectedEOF
}

// TestTranslatedStreamsFireOnErrorForUpstreamError covers that the Anthropic and
// Gemini SSE translators invoke onError when the upstream stream carries an
// error event or ends before [DONE] after the 200 was committed (round-7
// sibling of the OpenAI chat onError fix), so those translated streams are also
// recorded as failures.
func TestTranslatedStreamsFireOnErrorForUpstreamError(t *testing.T) {
	t.Run("anthropic error event", func(t *testing.T) {
		body := io.NopCloser(strings.NewReader("event: error\ndata: {\"error\":{\"message\":\"boom\"}}\n\n"))
		w := httptest.NewRecorder()
		errored := false
		StreamOpenAIToAnthropicWithFinalResponse(w, body, "claude-x", "msg_1", func(int) { errored = true }, nil)
		if !errored {
			t.Fatal("expected onError to fire for anthropic upstream error event")
		}
	})
	t.Run("gemini error event", func(t *testing.T) {
		body := io.NopCloser(strings.NewReader("event: error\ndata: {\"error\":{\"message\":\"boom\"}}\n\n"))
		w := httptest.NewRecorder()
		errored := false
		StreamOpenAIToGeminiWithFinalResponse(w, body, func(int) { errored = true }, nil)
		if !errored {
			t.Fatal("expected onError to fire for gemini upstream error event")
		}
	})
	t.Run("anthropic missing DONE", func(t *testing.T) {
		// A content chunk but no [DONE] → ended-before-DONE failure.
		body := io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
		w := httptest.NewRecorder()
		errored := false
		StreamOpenAIToAnthropicWithFinalResponse(w, body, "claude-x", "msg_1", func(int) { errored = true }, nil)
		if !errored {
			t.Fatal("expected onError to fire when stream ends before [DONE]")
		}
	})
	t.Run("normal stream does not fire", func(t *testing.T) {
		body := io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
		w := httptest.NewRecorder()
		errored := false
		StreamOpenAIToAnthropicWithFinalResponse(w, body, "claude-x", "msg_1", func(int) { errored = true }, nil)
		if errored {
			t.Fatal("onError must not fire for a normal completed stream")
		}
	})
}

// errAfterReader yields data once, then a non-EOF error (simulating an upstream
// connection reset mid-stream).
type errAfterReader struct {
	data []byte
	done bool
}

func (e *errAfterReader) Read(p []byte) (int, error) {
	if !e.done {
		e.done = true
		return copy(p, e.data), nil
	}
	return 0, io.ErrUnexpectedEOF
}

func (e *errAfterReader) Close() error { return nil }

// TestStreamOpenAIChatPassthroughMarksTransportError covers round-8 [0]: an
// upstream read error (connection reset) after the 200 was committed fires
// onError so the chat request is recorded as a failure, not a 2xx success.
func TestStreamOpenAIChatPassthroughMarksTransportError(t *testing.T) {
	body := &errAfterReader{data: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")}
	w := httptest.NewRecorder()
	errored := false
	StreamOpenAIChatPassthrough(w, body, "gpt-4", false, func(int) { errored = true }, nil, func(*models.OpenAIUsage) {})
	if !errored {
		t.Fatal("expected onError to fire on a mid-stream transport error")
	}
}

// TestStreamOpenAIChatPassthroughMarksPrematureEnd covers round-8 [0]: a stream
// that ends (EOF) without a [DONE] sentinel fires onError.
func TestStreamOpenAIChatPassthroughMarksPrematureEnd(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")) // no [DONE]
	w := httptest.NewRecorder()
	errored := false
	StreamOpenAIChatPassthrough(w, body, "gpt-4", false, func(int) { errored = true }, nil, func(*models.OpenAIUsage) {})
	if !errored {
		t.Fatal("expected onError to fire when the stream ends before [DONE]")
	}
}

// TestStreamOpenAIChatPassthroughFailsOpenOnOversizedLine covers round-8 [1]: a
// valid SSE line larger than the parse buffer must still be forwarded to the
// client (fall back to raw copy) and must NOT be treated as a failure.
func TestStreamOpenAIChatPassthroughFailsOpenOnOversizedLine(t *testing.T) {
	huge := strings.Repeat("x", openAIStreamScannerMaxBuffer+4096)
	// A normal chunk, then an oversized line, then [DONE].
	stream := "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: " + huge + "\n\n" +
		"data: [DONE]\n\n"
	body := io.NopCloser(strings.NewReader(stream))
	w := httptest.NewRecorder()
	errored := false
	StreamOpenAIChatPassthrough(w, body, "gpt-4", false, func(int) { errored = true }, nil, func(*models.OpenAIUsage) {})

	if errored {
		t.Fatal("oversized line is not a failure; onError must not fire")
	}
	out := w.Body.String()
	if !strings.Contains(out, huge) {
		t.Fatal("oversized line must still be forwarded to the client (fail-open raw copy)")
	}
	if !strings.Contains(out, "data: [DONE]") {
		t.Fatal("remainder after the oversized line must still be forwarded")
	}
}

// TestStreamResponsesPipeIgnoresClientWriteError covers round-8 [2]: a client
// disconnect (write error to the gone client) after the 200 commit must NOT be
// recorded as a 502 upstream failure.
func TestStreamResponsesPipeIgnoresClientWriteError(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	// Upstream sends data fine; the client writer fails (disconnected).
	r := io.NopCloser(strings.NewReader("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
	w := &failingResponseWriter{header: http.Header{}}

	streamResponsesPipeWithFailureLog(ctx, &ProxyHandler{log: logger.New(logger.LevelError)}, w, r, nil, nil, "")

	if summary.FailureStatus() != 0 {
		t.Fatalf("client write error must not be recorded as an upstream failure, got status %d", summary.FailureStatus())
	}
}

// failingResponseWriter is an http.ResponseWriter whose Write always fails,
// simulating a disconnected client.
type failingResponseWriter struct {
	header http.Header
}

func (f *failingResponseWriter) Header() http.Header       { return f.header }
func (f *failingResponseWriter) WriteHeader(int)           {}
func (f *failingResponseWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// TestStreamOpenAIChatPassthroughErrorStatusClassified covers round-9 [1]: a
// post-commit error event with a classifiable code passes its mapped status to
// onError (e.g. 429), not a hardcoded 502.
func TestStreamOpenAIChatPassthroughErrorStatusClassified(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"too_many_requests\"}}\n\n"))
	w := httptest.NewRecorder()
	var gotStatus int
	StreamOpenAIChatPassthrough(w, body, "gpt-4", false, func(status int) { gotStatus = status }, nil, func(*models.OpenAIUsage) {})
	if gotStatus != http.StatusTooManyRequests {
		t.Fatalf("onError status = %d want 429 (classified)", gotStatus)
	}
}

// TestOpenAIStreamErrorHTTPStatus covers the chat stream error classifier.
func TestOpenAIStreamErrorHTTPStatus(t *testing.T) {
	cases := []struct {
		code, typ string
		want      int
	}{
		{"too_many_requests", "", http.StatusTooManyRequests},
		{"model_overloaded", "", http.StatusServiceUnavailable},
		{"", "rate_limit_error", http.StatusTooManyRequests},
		{"something_unknown", "server_error", http.StatusBadGateway},
		{"", "", http.StatusBadGateway},
	}
	for _, c := range cases {
		e := &openAIStreamError{Code: c.code, Type: c.typ}
		if got := e.httpStatus(); got != c.want {
			t.Errorf("httpStatus(code=%q type=%q) = %d want %d", c.code, c.typ, got, c.want)
		}
	}
	if (*openAIStreamError)(nil).httpStatus() != http.StatusBadGateway {
		t.Fatal("nil error should map to 502")
	}
}

// TestAnthropicStreamErrorStatus covers the direct-Anthropic error frame
// classifier used by round-9 [2].
func TestAnthropicStreamErrorStatus(t *testing.T) {
	if s, ok := anthropicStreamErrorStatus([]byte(`{"type":"error","error":{"type":"overloaded_error"}}`)); !ok || s != http.StatusServiceUnavailable {
		t.Fatalf("overloaded_error → %d ok=%v want 503", s, ok)
	}
	if s, ok := anthropicStreamErrorStatus([]byte(`{"type":"error","error":{"type":"rate_limit_error"}}`)); !ok || s != http.StatusTooManyRequests {
		t.Fatalf("rate_limit_error → %d ok=%v want 429", s, ok)
	}
	if _, ok := anthropicStreamErrorStatus([]byte(`{"type":"message_delta","usage":{"output_tokens":5}}`)); ok {
		t.Fatal("non-error frame must return ok=false")
	}
}

// TestStreamAnthropicPassthroughMarksUpstreamError covers round-9 [2]: the direct
// Anthropic passthrough records a failure status when the stream carries an
// Anthropic error frame.
func TestStreamAnthropicPassthroughMarksUpstreamError(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	// No model rewrite (fast path); an error frame mid-stream.
	stream := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n"
	w := httptest.NewRecorder()
	streamAnthropicPassthroughBody(ctx, w, strings.NewReader(stream), "claude-x", "claude-x")
	if summary.FailureStatus() != http.StatusServiceUnavailable {
		t.Fatalf("FailureStatus = %d want 503 (anthropic error frame)", summary.FailureStatus())
	}
	// The error frame is still forwarded to the client unchanged.
	if !strings.Contains(w.Body.String(), "overloaded_error") {
		t.Fatal("error frame must still be forwarded to the client")
	}
}

// TestResponsesFailureTapCapturesUsageAcrossChunkedOverflow covers round-9 [0]:
// a response.completed streamed in small Write chunks, whose buffer overflows
// before the trailing usage arrives, still records usage (the tail is retained).
func TestResponsesFailureTapCapturesUsageAcrossChunkedOverflow(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	tap := newResponsesFailureTap(ctx, &ProxyHandler{}, nil, nil, "")

	// Build a response.completed whose output far exceeds the buffer, with usage
	// at the very end — then feed it to the tap in small chunks (as io.Copy does).
	bigOutput := strings.Repeat("z", responsesFailureTapMaxBuffer*2)
	full := `data: {"type":"response.completed","response":{"id":"r1","output":"` + bigOutput +
		`","usage":{"input_tokens":321,"output_tokens":12,"total_tokens":333}}}` + "\n\n"
	for i := 0; i < len(full); i += 4096 {
		end := i + 4096
		if end > len(full) {
			end = len(full)
		}
		if _, err := tap.Write([]byte(full[i:end])); err != nil {
			t.Fatalf("tap.Write: %v", err)
		}
	}

	if summary.totalTokens == nil || *summary.totalTokens != 333 {
		t.Fatalf("totalTokens = %v want 333 (usage recovered across chunked overflow)", summary.totalTokens)
	}
}
