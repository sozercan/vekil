package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandleMetrics(t *testing.T) {
	h := &ProxyHandler{stats: newStatsCollector()}
	h.stats.record(newStatsRequestSummary("gpt-5", "openai", "openai", 11, 7, 18), http.StatusOK, "test-agent", 25)

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
		`vekil_requests_total{endpoint="",provider="",public_model="",status="total"} 1`,
		`vekil_tokens_total{direction="prompt",provider="",public_model=""} 11`,
		"vekil_request_latency_milliseconds",
		"vekil_inflight_requests",
		"vekil_build_info",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics body missing %q:\n%s", want, body)
		}
	}
}
