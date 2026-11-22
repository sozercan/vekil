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

	req2 := httptest.NewRequest(http.MethodPost, "/dashboard/insight", nil)
	req2.Header.Set("Sec-Fetch-Site", "cross-origin")
	w2 := httptest.NewRecorder()
	h.HandleDashboardInsight(w2, req2)
	if got := decode(t, w2.Result().Body); !strings.Contains(got.Error, "cross-site") {
		t.Fatalf("expected cross-origin to be rejected, got error=%q", got.Error)
	}

	reqSameSite := httptest.NewRequest(http.MethodPost, "/dashboard/insight", nil)
	reqSameSite.Header.Set("Sec-Fetch-Site", "same-site")
	wSameSite := httptest.NewRecorder()
	h.HandleDashboardInsight(wSameSite, reqSameSite)
	if got := decode(t, wSameSite.Result().Body); !strings.Contains(got.Error, "cross-site") {
		t.Fatalf("expected same-site to be rejected, got error=%q", got.Error)
	}

	reqBadOrigin := httptest.NewRequest(http.MethodPost, "http://dashboard.local/dashboard/insight", nil)
	reqBadOrigin.Header.Set("Sec-Fetch-Site", "same-origin")
	reqBadOrigin.Header.Set("Origin", "http://evil.local")
	wBadOrigin := httptest.NewRecorder()
	h.HandleDashboardInsight(wBadOrigin, reqBadOrigin)
	if got := decode(t, wBadOrigin.Result().Body); !strings.Contains(got.Error, "cross-site") {
		t.Fatalf("expected mismatched Origin to be rejected, got error=%q", got.Error)
	}

	reqBadReferer := httptest.NewRequest(http.MethodPost, "http://dashboard.local/dashboard/insight", nil)
	reqBadReferer.Header.Set("Referer", "http://evil.local/page")
	wBadReferer := httptest.NewRecorder()
	h.HandleDashboardInsight(wBadReferer, reqBadReferer)
	if got := decode(t, wBadReferer.Result().Body); !strings.Contains(got.Error, "cross-site") {
		t.Fatalf("expected mismatched Referer to be rejected, got error=%q", got.Error)
	}

	h.insightGateFor().tryAcquire()
	req3 := httptest.NewRequest(http.MethodPost, "/dashboard/insight", nil)
	req3.Header.Set("Sec-Fetch-Site", "same-origin")
	w3 := httptest.NewRecorder()
	h.HandleDashboardInsight(w3, req3)
	if got := decode(t, w3.Result().Body); strings.Contains(got.Error, "cross-site") {
		t.Fatalf("same-origin must not be rejected by CSRF guard, got error=%q", got.Error)
	}

	req4 := httptest.NewRequest(http.MethodPost, "/dashboard/insight", nil)
	w4 := httptest.NewRecorder()
	h.HandleDashboardInsight(w4, req4)
	if got := decode(t, w4.Result().Body); strings.Contains(got.Error, "cross-site") {
		t.Fatalf("absent Sec-Fetch-Site must not be rejected, got error=%q", got.Error)
	}
}

func TestLogRetryAttemptCountsOnlyTrackedRequests(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}

	h.logRetryAttempt(context.Background(), 0, 503, "", 0, nil)
	if got := h.stats.snapshot().Retries; got != 0 {
		t.Fatalf("unmarked retry should not be counted: got %d want 0", got)
	}

	trackedCtx := markRetryStatsTracked(context.Background())
	h.logRetryAttempt(trackedCtx, 0, 503, "", 0, nil)
	if got := h.stats.snapshot().Retries; got != 1 {
		t.Fatalf("tracked retry not counted: got %d want 1", got)
	}
}

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

	up3, cancel3 := h.newInferenceUpstreamContext(false)
	defer cancel3()
	if isRetryStatsTracked(up3) {
		t.Fatal("newInferenceUpstreamContext must not mark tracked")
	}
}

func TestRetryStatsTrackedUsesExistingRequestSummaryContext(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	tracked := markRetryStatsTracked(ctx)
	if tracked != ctx {
		t.Fatal("markRetryStatsTracked added a context layer despite an existing request summary")
	}
	if !isRetryStatsTracked(tracked) || !summary.retryStatsTracked.Load() {
		t.Fatal("request summary did not retain the retry-stats marker")
	}
}

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

func TestObserveAnthropicUsageBody(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	body := []byte(`{"id":"msg_1","type":"message","model":"claude-x","usage":{"input_tokens":120,"output_tokens":40,"cache_read_input_tokens":30,"cache_creation_input_tokens":10}}`)

	observeAnthropicUsageBody(ctx, body)

	if summary.promptTokens == nil || *summary.promptTokens != 160 {
		t.Fatalf("promptTokens = %v want 160 (input + cache read + cache creation)", summary.promptTokens)
	}
	if summary.completionTokens == nil || *summary.completionTokens != 40 {
		t.Fatalf("completionTokens = %v want 40", summary.completionTokens)
	}
	if summary.totalTokens == nil || *summary.totalTokens != 200 {
		t.Fatalf("totalTokens = %v want 200", summary.totalTokens)
	}
	if summary.cachedTokens == nil || *summary.cachedTokens != 30 {
		t.Fatalf("cachedTokens = %v want 30", summary.cachedTokens)
	}
	if *summary.cachedTokens > *summary.promptTokens {
		t.Fatalf("cached(%d) must not exceed prompt(%d)", *summary.cachedTokens, *summary.promptTokens)
	}
}

func TestObserveAnthropicUsageBodyCacheOnly(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
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

func TestBoundStatLabel(t *testing.T) {
	if got := boundStatLabel("claude-sonnet-4.5"); got != "claude-sonnet-4.5" {
		t.Fatalf("short label changed: %q", got)
	}
	long := strings.Repeat("z", statLabelMaxLen+50)
	got := boundStatLabel(long)
	if len([]rune(got)) != statLabelMaxLen+1 {
		t.Fatalf("bounded label length = %d runes, want %d", len([]rune(got)), statLabelMaxLen+1)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("bounded label should end with ellipsis, got %q", got)
	}
	exact := strings.Repeat("a", statLabelMaxLen)
	if got := boundStatLabel(exact); got != exact {
		t.Fatalf("label at cap should be unchanged, got %q", got)
	}
}

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

func TestAnthropicStreamUsageAccumulator(t *testing.T) {
	acc := &anthropicStreamUsageAccumulator{}
	acc.observe([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":120,"cache_read_input_tokens":30,"cache_creation_input_tokens":10,"output_tokens":1}}}`))
	acc.observe([]byte(`{"type":"content_block_delta","delta":{"text":"hi"}}`))
	acc.observe([]byte(`{"type":"message_delta","usage":{"output_tokens":7}}`))
	acc.observe([]byte(`{"type":"message_delta","usage":{"output_tokens":42}}`))

	ctx, summary := WithRequestSummary(context.Background())
	acc.flush(ctx)

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

func TestStreamAnthropicPassthroughObservesUsageOnFastPath(t *testing.T) {
	stream := "event: message_start\n" +
		`data: {"type":"message_start","message":{"usage":{"input_tokens":50,"output_tokens":1}}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","usage":{"output_tokens":9}}` + "\n\n"

	ctx, summary := WithRequestSummary(context.Background())
	w := httptest.NewRecorder()
	streamAnthropicPassthroughBody(ctx, w, strings.NewReader(stream), "claude-x", "claude-x")

	if got := w.Body.String(); got != stream {
		t.Fatalf("fast-path bytes changed:\n got=%q\nwant=%q", got, stream)
	}
	if summary.totalTokens == nil || *summary.totalTokens != 59 {
		t.Fatalf("totalTokens = %v want 59 (usage observed on fast path)", summary.totalTokens)
	}
}

func TestCompactBudgetUsageTotals(t *testing.T) {
	b := newCompactBudget(3)
	b.addResponsesUsage([]byte(`{"usage":{"input_tokens":100,"output_tokens":40,"total_tokens":140}}`))
	b.addResponsesUsage([]byte(`{"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}`))

	u := b.usageTotals()
	if u.InputTokens != 110 || u.OutputTokens != 45 || u.TotalTokens != 155 {
		t.Fatalf("usageTotals = %+v want input=110 output=45 total=155", u)
	}

	ctx, summary := WithRequestSummary(context.Background())
	observeResponsesUsage(ctx, u)
	if summary.totalTokens == nil || *summary.totalTokens != 155 {
		t.Fatalf("observed totalTokens = %v want 155", summary.totalTokens)
	}
}

func TestRequestSummaryFailureStatus(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	if summary.FailureStatus() != 0 {
		t.Fatal("fresh summary should have no failure status")
	}
	observeResponseFailureStatus(ctx, http.StatusBadGateway)
	if summary.FailureStatus() != http.StatusBadGateway {
		t.Fatalf("FailureStatus = %d want 502", summary.FailureStatus())
	}
	observeResponseFailureStatus(ctx, http.StatusInternalServerError)
	if summary.FailureStatus() != http.StatusBadGateway {
		t.Fatalf("FailureStatus changed to %d, want first-wins 502", summary.FailureStatus())
	}
	ctx2, summary2 := WithRequestSummary(context.Background())
	observeResponseFailureStatus(ctx2, 0)
	if summary2.FailureStatus() != 0 {
		t.Fatal("zero status should be ignored")
	}
}

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
	if summary.totalTokens == nil || *summary.totalTokens != 7 {
		t.Fatalf("totalTokens = %v want 7 (usage after oversized line still sniffed)", summary.totalTokens)
	}
}

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

func TestStreamFailureStatusCarriesClassifiedStatus(t *testing.T) {
	err := &streamFailedUpstreamError{status: http.StatusTooManyRequests}
	if !errors.Is(err, errStreamFailedUpstream) {
		t.Fatal("streamFailedUpstreamError must unwrap to the sentinel")
	}
	if got := streamFailureStatus(err); got != http.StatusTooManyRequests {
		t.Fatalf("streamFailureStatus = %d want 429", got)
	}
	if got := streamFailureStatus(errStreamFailedUpstream); got != http.StatusBadGateway {
		t.Fatalf("streamFailureStatus(bare) = %d want 502", got)
	}
	if got := streamFailureStatus(&streamFailedUpstreamError{}); got != http.StatusBadGateway {
		t.Fatalf("streamFailureStatus(zero) = %d want 502", got)
	}
}

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
			status, _, _, _ := responsesWebSocketStreamFailureDetails(tc.event, nil)
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
	status, message, code, _ := responsesWebSocketStreamFailureDetails(event, headers)
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
	status, message, code, _ := responsesWebSocketStreamFailureDetails(event, headers)
	if status != http.StatusBadRequest || message != "too long" || code != "context_length_exceeded" {
		t.Fatalf("failure details = (%d, %q, %q), want (400, too long, context_length_exceeded)", status, message, code)
	}
}

func TestObserveInternalResponsesUsageAddsToTurnUsage(t *testing.T) {
	c := newStatsCollector()
	ctx, summary := WithRequestSummary(context.Background())
	summary.setRoute("/v1/responses", "gpt-5.4", false)
	summary.setProvider("codex", "openai-codex")

	observeInternalResponsesUsage(ctx, responsesUsage{InputTokens: 700, OutputTokens: 90, TotalTokens: 790})
	observeResponsesUsage(ctx, responsesUsage{InputTokens: 100, OutputTokens: 20, TotalTokens: 120})

	c.record(summary, 200, "Codex CLI", 0)
	snap := c.snapshot()
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

func TestObserveInternalResponsesUsageIgnoresZero(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	observeInternalResponsesUsage(ctx, responsesUsage{})
	if summary.extraPromptTokens != 0 || summary.extraCompletionTokens != 0 {
		t.Fatalf("zero internal usage should not accumulate: prompt=%d completion=%d", summary.extraPromptTokens, summary.extraCompletionTokens)
	}
}

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

	body := io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"))
	w := httptest.NewRecorder()
	errored := false
	StreamOpenAIChatPassthrough(w, body, "gpt-4", false, func(int) { errored = true }, nil, func(*models.OpenAIUsage) {})
	if errored {
		t.Fatal("onError must not fire for a normal content stream")
	}
}

func TestExtractResponsesUsageObject(t *testing.T) {
	bigOutput := strings.Repeat("x", 4096)
	buf := []byte(`data: {"type":"response.completed","response":{"id":"r1","output":"` + bigOutput +
		`","usage":{"input_tokens":1234,"output_tokens":56,"total_tokens":1290}}`)
	u, ok := extractResponsesUsageObject(buf)
	if !ok {
		t.Fatal("expected to extract usage from oversized buffer")
	}
	if u.InputTokens != 1234 || u.OutputTokens != 56 || u.TotalTokens != 1290 {
		t.Fatalf("usage = %+v want input=1234 output=56 total=1290", u)
	}

	if _, ok := extractResponsesUsageObject([]byte(`{"type":"response.output_text.delta","delta":"hi"}`)); ok {
		t.Fatal("expected ok=false when no usage object present")
	}
	if _, ok := extractResponsesUsageObject([]byte(`{"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`)); ok {
		t.Fatal("expected ok=false for zero usage")
	}
}

func TestBalancedJSONObject(t *testing.T) {
	in := []byte(`{"a":{"b":"}{"},"c":"\"}"}TRAILER`)
	obj, end := balancedJSONObject(in)
	if string(obj) != `{"a":{"b":"}{"},"c":"\"}"}` {
		t.Fatalf("balanced object = %q", string(obj))
	}
	if string(in[end:]) != "TRAILER" {
		t.Fatalf("trailer = %q want TRAILER", string(in[end:]))
	}
	if _, end := balancedJSONObject([]byte(`{"a":1`)); end != 0 {
		t.Fatalf("unclosed object should return end=0, got %d", end)
	}
}

func TestResponsesFailureTapRecoversUsageFromOversizedCompleted(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	tap := newResponsesFailureTap(ctx, &ProxyHandler{}, nil, nil, "")

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

func TestStreamResponsesPipeMarksFailureOnCopyError(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	r := &erroringReader{data: []byte("event: response.created\ndata: {\"type\":\"response.created\"}\n\n")}
	w := httptest.NewRecorder()

	streamResponsesPipeWithFailureLog(ctx, &ProxyHandler{log: logger.New(logger.LevelError)}, w, r, nil, nil, "")

	if summary.FailureStatus() != http.StatusBadGateway {
		t.Fatalf("FailureStatus = %d want 502 (broken stream after commit)", summary.FailureStatus())
	}
}

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

func TestStreamOpenAIChatPassthroughMarksTransportError(t *testing.T) {
	body := &errAfterReader{data: []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n")}
	w := httptest.NewRecorder()
	errored := false
	StreamOpenAIChatPassthrough(w, body, "gpt-4", false, func(int) { errored = true }, nil, func(*models.OpenAIUsage) {})
	if !errored {
		t.Fatal("expected onError to fire on a mid-stream transport error")
	}
}

func TestStreamOpenAIChatPassthroughMarksPrematureEnd(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\n"))
	w := httptest.NewRecorder()
	errored := false
	StreamOpenAIChatPassthrough(w, body, "gpt-4", false, func(int) { errored = true }, nil, func(*models.OpenAIUsage) {})
	if !errored {
		t.Fatal("expected onError to fire when the stream ends before [DONE]")
	}
}

func TestStreamOpenAIChatPassthroughFailsOpenOnOversizedLine(t *testing.T) {
	huge := strings.Repeat("x", openAIStreamScannerMaxBuffer+4096)
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

func TestStreamResponsesPipeIgnoresClientWriteError(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	r := io.NopCloser(strings.NewReader("event: response.created\ndata: {\"type\":\"response.created\"}\n\n"))
	w := &failingResponseWriter{header: http.Header{}}

	streamResponsesPipeWithFailureLog(ctx, &ProxyHandler{log: logger.New(logger.LevelError)}, w, r, nil, nil, "")

	if summary.FailureStatus() != 0 {
		t.Fatalf("client write error must not be recorded as an upstream failure, got status %d", summary.FailureStatus())
	}
}

type failingResponseWriter struct {
	header http.Header
}

func (f *failingResponseWriter) Header() http.Header       { return f.header }
func (f *failingResponseWriter) WriteHeader(int)           {}
func (f *failingResponseWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestStreamOpenAIChatPassthroughErrorStatusClassified(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"too_many_requests\"}}\n\n"))
	w := httptest.NewRecorder()
	var gotStatus int
	StreamOpenAIChatPassthrough(w, body, "gpt-4", false, func(status int) { gotStatus = status }, nil, func(*models.OpenAIUsage) {})
	if gotStatus != http.StatusTooManyRequests {
		t.Fatalf("onError status = %d want 429 (classified)", gotStatus)
	}
}

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

func TestStreamAnthropicPassthroughMarksUpstreamError(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	stream := "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"overloaded_error\"}}\n\n"
	w := httptest.NewRecorder()
	streamAnthropicPassthroughBody(ctx, w, strings.NewReader(stream), "claude-x", "claude-x")
	if summary.FailureStatus() != http.StatusServiceUnavailable {
		t.Fatalf("FailureStatus = %d want 503 (anthropic error frame)", summary.FailureStatus())
	}
	if !strings.Contains(w.Body.String(), "overloaded_error") {
		t.Fatal("error frame must still be forwarded to the client")
	}
}

func TestResponsesFailureTapCapturesUsageAcrossChunkedOverflow(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	tap := newResponsesFailureTap(ctx, &ProxyHandler{}, nil, nil, "")

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
