package proxy

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

const metricsBuildVersion = "dev"

// HandleMetrics renders the in-memory traffic stats as Prometheus-compatible
// text exposition. It reuses the same collector as /stats.json so the metrics
// endpoint has no extra dependency or background goroutine.
func (h *ProxyHandler) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	var snap statsSnapshot
	if h != nil && h.stats != nil {
		snap = h.stats.snapshot()
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	_, _ = fmt.Fprintln(w, "# HELP vekil_requests_total Total Vekil proxy requests by provider, public model, endpoint, and status.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_requests_total counter")
	writePromIntMetric(w, "vekil_requests_total", map[string]string{"provider": "", "public_model": "", "endpoint": "", "status": "total"}, snap.Totals.Requests)
	writePromIntMetric(w, "vekil_requests_total", map[string]string{"provider": "", "public_model": "", "endpoint": "", "status": "error"}, snap.Totals.Errors)
	for _, row := range snap.ByProvider {
		writePromIntMetric(w, "vekil_requests_total", map[string]string{"provider": row.Provider, "public_model": "", "endpoint": "", "status": "total"}, row.Requests)
		writePromIntMetric(w, "vekil_requests_total", map[string]string{"provider": row.Provider, "public_model": "", "endpoint": "", "status": "error"}, row.Errors)
	}
	for _, row := range snap.ByModel {
		writePromIntMetric(w, "vekil_requests_total", map[string]string{"provider": "", "public_model": row.Model, "endpoint": "", "status": "total"}, row.Requests)
		writePromIntMetric(w, "vekil_requests_total", map[string]string{"provider": "", "public_model": row.Model, "endpoint": "", "status": "error"}, row.Errors)
	}

	_, _ = fmt.Fprintln(w, "# HELP vekil_request_duration_seconds Recent request latency summary in seconds.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_request_duration_seconds summary")
	writePromFloatMetric(w, "vekil_request_duration_seconds", map[string]string{"quantile": "0.50"}, millisToSeconds(snap.Totals.LatencyP50))
	writePromFloatMetric(w, "vekil_request_duration_seconds", map[string]string{"quantile": "0.95"}, millisToSeconds(snap.Totals.LatencyP95))
	writePromFloatMetric(w, "vekil_request_duration_seconds", map[string]string{"quantile": "0.99"}, millisToSeconds(snap.Totals.LatencyP99))
	writePromFloatMetric(w, "vekil_request_duration_seconds_sum", nil, 0)
	writePromIntMetric(w, "vekil_request_duration_seconds_count", nil, snap.Totals.Requests)

	_, _ = fmt.Fprintln(w, "# HELP vekil_tokens_total Total observed upstream tokens by direction.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_tokens_total counter")
	writePromIntMetric(w, "vekil_tokens_total", map[string]string{"provider": "", "public_model": "", "direction": "prompt"}, snap.Totals.PromptTokens)
	writePromIntMetric(w, "vekil_tokens_total", map[string]string{"provider": "", "public_model": "", "direction": "completion"}, snap.Totals.CompletionTokens)
	writePromIntMetric(w, "vekil_tokens_total", map[string]string{"provider": "", "public_model": "", "direction": "total"}, snap.Totals.TotalTokens)
	writePromIntMetric(w, "vekil_tokens_total", map[string]string{"provider": "", "public_model": "", "direction": "cached"}, snap.Totals.CachedTokens)
	writePromIntMetric(w, "vekil_tokens_total", map[string]string{"provider": "", "public_model": "", "direction": "reasoning"}, snap.Totals.ReasoningTokens)

	_, _ = fmt.Fprintln(w, "# HELP vekil_retries_total Total upstream retry attempts by reason/status.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_retries_total counter")
	writePromIntMetric(w, "vekil_retries_total", map[string]string{"provider": "", "public_model": "", "reason": "all"}, snap.Retries)
	for _, row := range snap.RetriesByCode {
		writePromIntMetric(w, "vekil_retries_total", map[string]string{"provider": "", "public_model": "", "reason": row.Label}, row.Count)
	}

	_, _ = fmt.Fprintln(w, "# HELP vekil_upstream_errors_total Total upstream/proxy errors by status code.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_upstream_errors_total counter")
	for _, row := range snap.StatusCodes {
		writePromIntMetric(w, "vekil_upstream_errors_total", map[string]string{"provider": "", "public_model": "", "code": row.Label}, row.Count)
	}

	_, _ = fmt.Fprintln(w, "# HELP vekil_inflight_requests Current in-flight Vekil proxy requests.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_inflight_requests gauge")
	writePromIntMetric(w, "vekil_inflight_requests", map[string]string{"provider": ""}, snap.Inflight)

	_, _ = fmt.Fprintln(w, "# HELP vekil_endpoint_healthy Endpoint health gauge. Currently reports aggregate proxy health until endpoint-level health checks land.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_endpoint_healthy gauge")
	writePromIntMetric(w, "vekil_endpoint_healthy", map[string]string{"provider": "", "endpoint": ""}, 1)

	_, _ = fmt.Fprintln(w, "# HELP vekil_build_info Build and runtime information for Vekil.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_build_info gauge")
	writePromIntMetric(w, "vekil_build_info", map[string]string{"version": metricsBuildVersion, "go_version": runtime.Version(), "commit": ""}, 1)

	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	_, _ = fmt.Fprintln(w, "# HELP go_goroutines Number of goroutines that currently exist.")
	_, _ = fmt.Fprintln(w, "# TYPE go_goroutines gauge")
	writePromIntMetric(w, "go_goroutines", nil, int64(runtime.NumGoroutine()))
	_, _ = fmt.Fprintln(w, "# HELP go_memstats_alloc_bytes Bytes of allocated heap objects.")
	_, _ = fmt.Fprintln(w, "# TYPE go_memstats_alloc_bytes gauge")
	writePromIntMetric(w, "go_memstats_alloc_bytes", nil, int64(mem.Alloc))
}

func millisToSeconds(ms int64) float64 { return float64(ms) / 1000 }

func writePromIntMetric(w http.ResponseWriter, name string, labels map[string]string, value int64) {
	_, _ = fmt.Fprintf(w, "%s%s %d\n", name, promLabels(labels), value)
}

func writePromFloatMetric(w http.ResponseWriter, name string, labels map[string]string, value float64) {
	_, _ = fmt.Fprintf(w, "%s%s %g\n", name, promLabels(labels), value)
}

func promLabels(labels map[string]string) string {
	if len(labels) == 0 {
		return ""
	}
	keys := make([]string, 0, len(labels))
	for key := range labels {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+strconv.Quote(promLabelValue(labels[key])))
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func promLabelValue(value string) string {
	value = strings.ReplaceAll(value, "\n", " ")
	value = strings.ReplaceAll(value, "\r", " ")
	return value
}
