package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/models"
)

func TestHandleMetrics(t *testing.T) {
	oldVersion, oldCommit := metricsBuildVersion, metricsBuildCommit
	metricsBuildVersion, metricsBuildCommit = "test-version", "abc123"
	t.Cleanup(func() {
		metricsBuildVersion, metricsBuildCommit = oldVersion, oldCommit
	})

	h := &ProxyHandler{stats: newStatsCollector()}
	h.stats.record(newStatsRequestSummary("gpt-5", "openai", "openai", 11, 7, 18), http.StatusOK, "test-agent", 25*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	h.HandleMetrics(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/plain") {
		t.Fatalf("content-type = %q, want text/plain", ct)
	}
	body := w.Body.String()
	for _, want := range []string{
		"# HELP vekil_requests_total",
		`vekil_requests_total{endpoint="/v1/chat/completions",provider="openai",public_model="gpt-5",status="success"} 1`,
		`vekil_requests_total{endpoint="/v1/chat/completions",provider="openai",public_model="gpt-5",status="error"} 0`,
		`# TYPE vekil_request_duration_seconds histogram`,
		`vekil_request_duration_seconds_bucket{endpoint="/v1/chat/completions",le="0.025",provider="openai",public_model="gpt-5"} 1`,
		`vekil_request_duration_seconds_bucket{endpoint="/v1/chat/completions",le="+Inf",provider="openai",public_model="gpt-5"} 1`,
		`vekil_request_duration_seconds_sum{endpoint="/v1/chat/completions",provider="openai",public_model="gpt-5"} 0.025`,
		`vekil_request_duration_seconds_count{endpoint="/v1/chat/completions",provider="openai",public_model="gpt-5"} 1`,
		`vekil_tokens_total{direction="prompt",provider="openai",public_model="gpt-5"} 11`,
		`vekil_tokens_reported_total{provider="openai",public_model="gpt-5"} 18`,
		`vekil_tokens_cached_total{provider="openai",public_model="gpt-5"} 0`,
		`vekil_tokens_reasoning_total{provider="openai",public_model="gpt-5"} 0`,
		`vekil_build_info{commit="abc123"`,
		`version="test-version"`,
		"go_goroutines",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q:\n%s", want, body)
		}
	}
	for _, notWant := range []string{
		`vekil_requests_total{endpoint="",provider="",public_model="",status="success"}`,
		`vekil_request_duration_seconds{quantile=`,
		`vekil_tokens_total{direction="total"`,
		`vekil_tokens_total{direction="cached"`,
		`vekil_tokens_total{direction="reasoning"`,
		"\nvekil_inflight_requests ",
		"vekil_endpoint_healthy",
	} {
		if strings.Contains(body, notWant) {
			t.Fatalf("metrics body unexpectedly contains %q:\n%s", notWant, body)
		}
	}
}

func TestHandleMetricsExportsAllBoundedModelRequestSeries(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	for i := 0; i < statsBreakdownRows+5; i++ {
		h.stats.record(newStatsRequestSummary(fmt.Sprintf("model-%02d", i), "openai", "openai", 1, 1, 2), http.StatusOK, "test-agent", time.Millisecond)
	}

	w := httptest.NewRecorder()
	h.HandleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()

	for _, model := range []string{"model-00", "model-24", "model-29"} {
		want := fmt.Sprintf(`vekil_requests_total{endpoint="/v1/chat/completions",provider="openai",public_model="%s",status="success"} 1`, model)
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q:\n%s", want, body)
		}
	}
	if got := strings.Count(body, `vekil_requests_total{`); got != (statsBreakdownRows+5)*2 {
		t.Fatalf("request metric series count = %d, want %d\n%s", got, (statsBreakdownRows+5)*2, body)
	}
}

func TestHandleMetricsAttributesErrorRetryAndTokenSeries(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	h.stats.record(newStatsRequestSummary("gpt-5.4", "azure", "azure", 5, 3, 8), http.StatusBadGateway, "test-agent", 10*time.Millisecond)
	h.stats.incRetry(context.Background(), http.StatusTooManyRequests)

	w := httptest.NewRecorder()
	h.HandleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{
		`vekil_requests_total{endpoint="/v1/chat/completions",provider="azure",public_model="gpt-5.4",status="success"} 0`,
		`vekil_requests_total{endpoint="/v1/chat/completions",provider="azure",public_model="gpt-5.4",status="error"} 1`,
		`vekil_tokens_total{direction="prompt",provider="azure",public_model="gpt-5.4"} 5`,
		`vekil_tokens_total{direction="completion",provider="azure",public_model="gpt-5.4"} 3`,
		`vekil_tokens_reported_total{provider="azure",public_model="gpt-5.4"} 8`,
		`vekil_tokens_cached_total{provider="azure",public_model="gpt-5.4"} 0`,
		`vekil_tokens_reasoning_total{provider="azure",public_model="gpt-5.4"} 0`,
		`vekil_retries_total 1`,
		`vekil_retries_by_reason_total{provider="unrouted",public_model="unknown",reason="429"} 1`,
		`vekil_upstream_errors_total{code="502",provider="azure",public_model="gpt-5.4"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, `vekil_retries_total{reason="all"}`) {
		t.Fatalf("retry aggregate should not share the per-reason metric family:\n%s", body)
	}
}

func TestHandleMetricsPreservesTotalOnlyTokenUsage(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	s := &RequestSummary{}
	s.setRoute("/v1/chat/completions", "total-only", false)
	s.setProvider("openai", "openai")
	s.setOpenAIUsage(&models.OpenAIUsage{TotalTokens: 42})
	h.stats.record(s, http.StatusOK, "test-agent", time.Millisecond)

	w := httptest.NewRecorder()
	h.HandleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{
		`vekil_tokens_total{direction="prompt",provider="openai",public_model="total-only"} 0`,
		`vekil_tokens_total{direction="completion",provider="openai",public_model="total-only"} 0`,
		`vekil_tokens_reported_total{provider="openai",public_model="total-only"} 42`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q:\n%s", want, body)
		}
	}
}

func TestHandleMetricsExportsInflightByProvider(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	summary := &RequestSummary{}
	h.IncInflight(summary)
	h.MoveInflightProvider(summary, "azure")

	w := httptest.NewRecorder()
	h.HandleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	want := `vekil_inflight_requests{provider="azure"} 1`
	if !strings.Contains(body, want) {
		t.Fatalf("metrics body missing %q:\n%s", want, body)
	}
	if strings.Contains(body, `vekil_inflight_requests{provider="unrouted"}`) {
		t.Fatalf("inflight request should have moved from unrouted to azure:\n%s", body)
	}

	h.DecInflight(summary)
	w = httptest.NewRecorder()
	h.HandleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if body = w.Body.String(); strings.Contains(body, `vekil_inflight_requests{provider="azure"}`) {
		t.Fatalf("finished in-flight request should not remain exported:\n%s", body)
	}
}

func TestPromLabelsEscapesTextFormatValues(t *testing.T) {
	got := promLabels(map[string]string{"model": "a\tb\\c\"d\ne\x00f"})
	want := `{model="a%09b\\c\"d%0Ae%00f"}`
	if got != want {
		t.Fatalf("promLabels() = %q, want %q", got, want)
	}
}

func TestHandleMetricsPreservesStructuredMetricLabels(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	s := &RequestSummary{}
	s.setRoute("/v1/chat/completions", "bad\x00x", false)
	s.setProvider("openai", "openai")
	s.setOpenAIUsage(&models.OpenAIUsage{PromptTokens: 1, CompletionTokens: 2, TotalTokens: 3})
	h.stats.record(s, http.StatusOK, "test-agent", time.Millisecond)

	w := httptest.NewRecorder()
	h.HandleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	want := `vekil_requests_total{endpoint="/v1/chat/completions",provider="openai",public_model="bad%00x",status="success"} 1`
	if !strings.Contains(body, want) {
		t.Fatalf("metrics body missing %q:\n%s", want, body)
	}
	if strings.Contains(body, `endpoint="x"`) {
		t.Fatalf("NUL-containing model corrupted endpoint label:\n%s", body)
	}
}

func TestHandleMetricsExportsAllRetryReasonCounters(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	for i := 400; i < 400+statsTopN+5; i++ {
		h.stats.incRetry(context.Background(), i)
	}

	w := httptest.NewRecorder()
	h.HandleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	want := `vekil_retries_by_reason_total{provider="unrouted",public_model="unknown",reason="414"} 1`
	if !strings.Contains(body, want) {
		t.Fatalf("metrics body missing non-top retry reason %q:\n%s", want, body)
	}
}

func TestHandleMetricsExportsEstimatedCost(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector(), prices: newPriceCatalog(map[string]PriceEntry{
		"priced-model": {InputPer1K: 0.01, OutputPer1K: 0.02},
	})}
	s := newStatsRequestSummary("priced-model", "openai", "openai", 1000, 500, 1500)
	h.RecordRequest(s, http.StatusOK, "test-agent", time.Millisecond)

	w := httptest.NewRecorder()
	h.HandleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	want := `vekil_cost_usd_total{provider="openai",public_model="priced-model"} 0.02`
	if !strings.Contains(body, want) {
		t.Fatalf("metrics body missing %q:\n%s", want, body)
	}
	fields := s.LoggerFields()
	found := false
	for _, field := range fields {
		if field.Key == "cost_usd" && field.Value == 0.02 {
			found = true
		}
	}
	if !found {
		t.Fatalf("LoggerFields missing cost_usd=0.02: %#v", fields)
	}
}

func TestHandleMetricsOmitsCostForUnpricedModel(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector(), prices: newPriceCatalog(nil)}
	s := newStatsRequestSummary("unpriced", "openai", "openai", 1000, 500, 1500)
	h.RecordRequest(s, http.StatusOK, "test-agent", time.Millisecond)

	w := httptest.NewRecorder()
	h.HandleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	if strings.Contains(body, `vekil_cost_usd_total{`) {
		t.Fatalf("unpriced model should not emit cost metric:\n%s", body)
	}
}

func TestRecordRequestCostUsesRawModelBeforeStatsTruncation(t *testing.T) {
	longModel := strings.Repeat("model-", 20)
	h := &ProxyHandler{stats: newStatsCollector(), prices: newPriceCatalog(map[string]PriceEntry{
		longModel: {InputPer1K: 0.01, OutputPer1K: 0.02},
	})}
	s := newStatsRequestSummary(longModel, "openai", "openai", 1000, 500, 1500)
	h.RecordRequest(s, http.StatusOK, "test-agent", time.Millisecond)
	fields := s.LoggerFields()
	for _, field := range fields {
		if field.Key == "cost_usd" && field.Value == 0.02 {
			return
		}
	}
	t.Fatalf("LoggerFields missing cost for long raw model: %#v", fields)
}
