package proxy

import (
	"context"
	"encoding/json"
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

// TestLogRetryAttemptExcludesInsightRequests covers that retries on the
// in-process /dashboard/insight call are not counted in the dashboard's own
// retry stats (the insight path is self-excluded), while ordinary requests are.
func TestLogRetryAttemptExcludesInsightRequests(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}

	// Ordinary request context → counted.
	h.logRetryAttempt(context.Background(), 0, 503, "", 0, nil)
	if got := h.stats.snapshot().Retries; got != 1 {
		t.Fatalf("ordinary retry not counted: got %d want 1", got)
	}

	// Insight-marked context → not counted.
	insightCtx := markInsightRequest(context.Background())
	h.logRetryAttempt(insightCtx, 0, 503, "", 0, nil)
	if got := h.stats.snapshot().Retries; got != 1 {
		t.Fatalf("insight retry should be excluded: retries went to %d want 1", got)
	}
}

// TestObserveAnthropicUsageBody covers usage capture for the direct
// anthropic-compatible passthrough: Anthropic input/output tokens map onto the
// chat-shaped fields the dashboard reads, with cache-read mapped to cached.
func TestObserveAnthropicUsageBody(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	body := []byte(`{"id":"msg_1","type":"message","model":"claude-x","usage":{"input_tokens":120,"output_tokens":40,"cache_read_input_tokens":30}}`)

	observeAnthropicUsageBody(ctx, body)

	if summary.promptTokens == nil || *summary.promptTokens != 120 {
		t.Fatalf("promptTokens = %v want 120", summary.promptTokens)
	}
	if summary.completionTokens == nil || *summary.completionTokens != 40 {
		t.Fatalf("completionTokens = %v want 40", summary.completionTokens)
	}
	if summary.totalTokens == nil || *summary.totalTokens != 160 {
		t.Fatalf("totalTokens = %v want 160", summary.totalTokens)
	}
	if summary.cachedTokens == nil || *summary.cachedTokens != 30 {
		t.Fatalf("cachedTokens = %v want 30", summary.cachedTokens)
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
