package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
		`vekil_tokens_total{direction="prompt",provider="openai",public_model="gpt-5"} 11`,
		`vekil_request_duration_seconds_sum 0.025`,
		`vekil_request_duration_seconds_count 1`,
		"vekil_inflight_requests",
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
	h.stats.incRetry(http.StatusTooManyRequests)

	w := httptest.NewRecorder()
	h.HandleMetrics(w, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	body := w.Body.String()
	for _, want := range []string{
		`vekil_requests_total{endpoint="/v1/chat/completions",provider="azure",public_model="gpt-5.4",status="success"} 0`,
		`vekil_requests_total{endpoint="/v1/chat/completions",provider="azure",public_model="gpt-5.4",status="error"} 1`,
		`vekil_tokens_total{direction="prompt",provider="azure",public_model="gpt-5.4"} 5`,
		`vekil_tokens_total{direction="completion",provider="azure",public_model="gpt-5.4"} 3`,
		`vekil_retries_total 1`,
		`vekil_retries_by_reason_total{reason="429"} 1`,
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

func TestPromLabelsEscapesTextFormatValues(t *testing.T) {
	got := promLabels(map[string]string{"model": "a\tb\\c\"d\ne\x00f"})
	want := `{model="a b\\c\"d\ne f"}`
	if got != want {
		t.Fatalf("promLabels() = %q, want %q", got, want)
	}
}
