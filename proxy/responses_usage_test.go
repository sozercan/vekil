package proxy

import (
	"context"
	"net/http"
	"testing"
)

func TestResponsesUsageToOpenAIUsage(t *testing.T) {
	u := responsesUsage{
		InputTokens:  100,
		OutputTokens: 40,
		TotalTokens:  140,
	}
	u.InputTokensDetails.CachedTokens = 30
	u.OutputTokensDetails.ReasoningTokens = 12

	got := u.toOpenAIUsage()
	if got.PromptTokens != 100 || got.CompletionTokens != 40 || got.TotalTokens != 140 {
		t.Fatalf("token mapping: %+v", got)
	}
	if got.PromptTokensDetails == nil || got.PromptTokensDetails.CachedTokens != 30 {
		t.Fatalf("cached mapping: %+v", got.PromptTokensDetails)
	}
	if got.CompletionTokensDetails == nil || got.CompletionTokensDetails.ReasoningTokens != 12 {
		t.Fatalf("reasoning mapping: %+v", got.CompletionTokensDetails)
	}
}

func TestResponsesUsageTotalDerived(t *testing.T) {
	// total_tokens omitted upstream → derived from input+output.
	u := responsesUsage{InputTokens: 70, OutputTokens: 30}
	if got := u.toOpenAIUsage(); got.TotalTokens != 100 {
		t.Fatalf("derived total: got %d want 100", got.TotalTokens)
	}
}

func TestSniffResponsesUsageBody(t *testing.T) {
	body := `{"id":"resp_1","object":"response","usage":{"input_tokens":120,"output_tokens":45,"total_tokens":165,"input_tokens_details":{"cached_tokens":60},"output_tokens_details":{"reasoning_tokens":15}}}`
	u := sniffResponsesUsageBody([]byte(body))
	if u.InputTokens != 120 || u.OutputTokens != 45 || u.TotalTokens != 165 {
		t.Fatalf("sniff totals: %+v", u)
	}
	if u.InputTokensDetails.CachedTokens != 60 || u.OutputTokensDetails.ReasoningTokens != 15 {
		t.Fatalf("sniff details: %+v", u)
	}
	// No usage → zero (and isZero true).
	if got := sniffResponsesUsageBody([]byte(`{"id":"x"}`)); !got.isZero() {
		t.Fatalf("expected zero usage, got %+v", got)
	}
	// Malformed JSON → zero, no panic.
	if got := sniffResponsesUsageBody([]byte(`not json`)); !got.isZero() {
		t.Fatalf("expected zero on bad json, got %+v", got)
	}
}

func TestObserveResponsesUsageWritesSummary(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	observeResponsesUsage(ctx, responsesUsage{InputTokens: 200, OutputTokens: 80, TotalTokens: 280})

	d := readSummaryForStats(summary)
	if d.prompt != 200 || d.completion != 80 || d.total != 280 {
		t.Fatalf("usage not recorded into summary: prompt=%d completion=%d total=%d", d.prompt, d.completion, d.total)
	}
	// Zero usage must not overwrite.
	ctx2, summary2 := WithRequestSummary(context.Background())
	observeResponsesUsage(ctx2, responsesUsage{})
	if d2 := readSummaryForStats(summary2); d2.prompt != 0 || d2.total != 0 {
		t.Fatalf("zero usage should be ignored, got %+v", d2)
	}
}

func TestRecordResponsesTurn(t *testing.T) {
	c := newStatsCollector()
	u := responsesUsage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150}
	u.InputTokensDetails.CachedTokens = 20
	u.OutputTokensDetails.ReasoningTokens = 10

	c.recordResponsesTurn("gpt-5.4-codex", "codex", "openai-codex", "Codex CLI", 200, u, 0)

	snap := c.snapshot()
	if snap.Totals.Requests != 1 {
		t.Fatalf("requests: got %d want 1", snap.Totals.Requests)
	}
	if snap.Totals.TotalTokens != 150 || snap.Totals.PromptTokens != 100 || snap.Totals.CompletionTokens != 50 {
		t.Fatalf("totals: %+v", snap.Totals)
	}
	if snap.Totals.CachedTokens != 20 || snap.Totals.ReasoningTokens != 10 {
		t.Fatalf("detail totals: cached=%d reasoning=%d", snap.Totals.CachedTokens, snap.Totals.ReasoningTokens)
	}
	// Stream turn → no latency sample.
	if snap.Totals.LatencyP50 != 0 {
		t.Fatalf("bridge turn should not contribute latency, got p50=%d", snap.Totals.LatencyP50)
	}
	if len(snap.ByModel) != 1 || snap.ByModel[0].Model != "gpt-5.4-codex" || snap.ByModel[0].Tokens != 150 {
		t.Fatalf("by_model: %+v", snap.ByModel)
	}
	if len(snap.ByAgent) != 1 || snap.ByAgent[0].Agent != "Codex CLI" {
		t.Fatalf("by_agent: %+v", snap.ByAgent)
	}
	if len(snap.ByProvider) != 1 || snap.ByProvider[0].Provider != "codex" {
		t.Fatalf("by_provider: %+v", snap.ByProvider)
	}
	// A zero-usage turn is still counted as a request (matching the HTTP path),
	// just with zero tokens.
	c.recordResponsesTurn("gpt-5.4-codex", "codex", "openai-codex", "Codex CLI", 200, responsesUsage{}, 0)
	after := c.snapshot()
	if after.Totals.Requests != 2 {
		t.Fatalf("zero-usage turn should still be counted: got %d requests want 2", after.Totals.Requests)
	}
	if after.Totals.TotalTokens != 150 {
		t.Fatalf("zero-usage turn should add no tokens: got %d want 150", after.Totals.TotalTokens)
	}
}

// TestRecordResponsesTurnCountsFailures covers that a websocket-bridge turn that
// failed before producing usage (e.g. upstream response.failed → 502) is still
// recorded, and lands in the error counts so the dashboard reflects it.
func TestRecordResponsesTurnCountsFailures(t *testing.T) {
	c := newStatsCollector()
	c.recordResponsesTurn("gpt-5.4-codex", "codex", "openai-codex", "Codex CLI", http.StatusBadGateway, responsesUsage{}, 0)

	snap := c.snapshot()
	if snap.Totals.Requests != 1 {
		t.Fatalf("failed turn should be counted as a request: got %d want 1", snap.Totals.Requests)
	}
	if snap.Totals.Errors != 1 {
		t.Fatalf("failed turn should be counted as an error: got %d want 1", snap.Totals.Errors)
	}
	if snap.Status["5xx"] != 1 {
		t.Fatalf("failed turn should land in 5xx status class: got %+v", snap.Status)
	}
	// Default status 0 is treated as a successful (200) turn, not an error.
	c.recordResponsesTurn("gpt-5.4-codex", "codex", "openai-codex", "Codex CLI", 0, responsesUsage{}, 0)
	snap2 := c.snapshot()
	if snap2.Totals.Errors != 1 {
		t.Fatalf("status 0 should default to success, errors stayed: got %d want 1", snap2.Totals.Errors)
	}
}
