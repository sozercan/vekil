package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestInsightGateSingleFlight(t *testing.T) {
	g := newInsightGate()
	fixed := time.Unix(1_700_000_000, 0)
	g.now = func() time.Time { return fixed }

	ok, _ := g.tryAcquire()
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	// Second acquire while the first is still active is rejected.
	if ok2, reason := g.tryAcquire(); ok2 || reason == "" {
		t.Fatalf("concurrent acquire should be rejected, got ok=%v reason=%q", ok2, reason)
	}
	g.release()
}

func TestInsightGateCooldown(t *testing.T) {
	g := newInsightGate()
	now := time.Unix(1_700_000_000, 0)
	g.now = func() time.Time { return now }

	ok, _ := g.tryAcquire()
	if !ok {
		t.Fatal("first acquire should succeed")
	}
	g.release()

	// Immediately after release we are within the cooldown window.
	if ok2, reason := g.tryAcquire(); ok2 || !strings.Contains(reason, "cooling down") {
		t.Fatalf("acquire during cooldown should be rejected, got ok=%v reason=%q", ok2, reason)
	}

	// Advance past the cooldown; acquire is allowed again.
	now = now.Add(insightCooldown + time.Second)
	if ok3, reason := g.tryAcquire(); !ok3 {
		t.Fatalf("acquire after cooldown should succeed, got reason=%q", reason)
	}
	g.release()
}

func TestHandleDashboardInsightNotConfigured(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()} // no InsightModel
	req := httptest.NewRequest(http.MethodPost, "/dashboard/insight", nil)
	w := httptest.NewRecorder()

	h.HandleDashboardInsight(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status: got %d want 200 (fail-open)", resp.StatusCode)
	}
	var out insightResponse
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if out.Insight != "" {
		t.Fatalf("expected no insight when unconfigured, got %q", out.Insight)
	}
	if out.Error == "" {
		t.Fatal("expected an error message when no model configured")
	}
}

func TestStatsJSONInsightsEnabledFlag(t *testing.T) {
	// Unconfigured → flag false.
	h := &ProxyHandler{stats: newStatsCollector()}
	if got := snapshotInsightsFlag(t, h); got {
		t.Fatal("insights_enabled should be false with no model")
	}

	// Configured → flag true.
	h2 := &ProxyHandler{
		stats:           newStatsCollector(),
		providersConfig: ProvidersConfig{InsightModel: "claude-opus-4.8"},
	}
	if got := snapshotInsightsFlag(t, h2); !got {
		t.Fatal("insights_enabled should be true when a model is configured")
	}
}

func snapshotInsightsFlag(t *testing.T, h *ProxyHandler) bool {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/stats.json", nil)
	w := httptest.NewRecorder()
	h.HandleStatsJSON(w, req)
	var snap struct {
		InsightsEnabled bool `json:"insights_enabled"`
	}
	if err := json.NewDecoder(w.Result().Body).Decode(&snap); err != nil {
		t.Fatalf("decode: %v", err)
	}
	return snap.InsightsEnabled
}

func TestBuildInsightPrompt(t *testing.T) {
	c := newStatsCollector()
	c.record(newStatsRequestSummary("claude-sonnet-4.5", "copilot", "copilot", 100, 50, 150), 200, "claude-cli/1.0", 0)
	c.record(newStatsRequestSummary("gpt-5.4", "azure", "azure", 10, 5, 15), 500, "codex/2.0", 0)

	prompt := buildInsightPrompt(c.snapshot(), "Handling 2 req/s at 1.2k tok/s.")
	for _, want := range []string{"SNAPSHOT:", "top_models", "claude-sonnet-4.5", "providers:", "copilot", "ALREADY_SHOWN_TO_USER"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt missing %q\n---\n%s", want, prompt)
		}
	}
}

func TestExtractOpenAIReplyText(t *testing.T) {
	// Plain string content.
	body := `{"choices":[{"message":{"role":"assistant","content":"Traffic looks healthy."}}]}`
	if got := extractOpenAIReplyText([]byte(body)); got != "Traffic looks healthy." {
		t.Fatalf("plain content: got %q", got)
	}
	// Structured content parts.
	body2 := `{"choices":[{"message":{"role":"assistant","content":[{"type":"text","text":"Part A. "},{"type":"text","text":"Part B."}]}}]}`
	if got := extractOpenAIReplyText([]byte(body2)); got != "Part A. Part B." {
		t.Fatalf("structured content: got %q", got)
	}
	// No choices.
	if got := extractOpenAIReplyText([]byte(`{"choices":[]}`)); got != "" {
		t.Fatalf("empty: got %q", got)
	}
}
