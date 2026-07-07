package proxy

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strings"
)

// metricsBuildVersion and metricsBuildCommit are variables so release builds can
// inject them with -ldflags -X github.com/sozercan/vekil/proxy.<name>=<value>.
var (
	metricsBuildVersion = "dev"
	metricsBuildCommit  = ""
)

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
	for _, row := range snap.RequestMetrics {
		labels := map[string]string{"provider": row.Provider, "public_model": row.Model, "endpoint": row.Endpoint, "status": "success"}
		writePromIntMetric(w, "vekil_requests_total", labels, row.Requests-row.Errors)
		writePromIntMetric(w, "vekil_requests_total", mapWithLabel(labels, "status", "error"), row.Errors)
	}

	_, _ = fmt.Fprintln(w, "# HELP vekil_request_duration_seconds Non-streaming request latency histogram in seconds.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_request_duration_seconds histogram")
	for _, row := range snap.RequestMetrics {
		latencyLabels := map[string]string{"provider": row.Provider, "public_model": row.Model, "endpoint": row.Endpoint}
		for _, bucket := range row.LatencyBuckets {
			writePromIntMetric(w, "vekil_request_duration_seconds_bucket", mapWithLabel(latencyLabels, "le", bucket.Le), bucket.Count)
		}
		writePromIntMetric(w, "vekil_request_duration_seconds_bucket", mapWithLabel(latencyLabels, "le", "+Inf"), row.LatencyCount)
		writePromFloatMetric(w, "vekil_request_duration_seconds_sum", latencyLabels, millisToSeconds(row.LatencySumMs))
		writePromIntMetric(w, "vekil_request_duration_seconds_count", latencyLabels, row.LatencyCount)
	}

	_, _ = fmt.Fprintln(w, "# HELP vekil_tokens_total Total observed upstream tokens by provider, public model, and disjoint direction.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_tokens_total counter")
	_, _ = fmt.Fprintln(w, "# HELP vekil_tokens_reported_total Total tokens reported by upstream usage blocks by provider and public model.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_tokens_reported_total counter")
	_, _ = fmt.Fprintln(w, "# HELP vekil_tokens_cached_total Total cached prompt tokens by provider and public model.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_tokens_cached_total counter")
	_, _ = fmt.Fprintln(w, "# HELP vekil_tokens_reasoning_total Total reasoning completion tokens by provider and public model.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_tokens_reasoning_total counter")
	_, _ = fmt.Fprintln(w, "# HELP vekil_cost_usd_total Estimated upstream cost in USD by provider and public model.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_cost_usd_total counter")
	for _, row := range snap.TokenMetrics {
		labels := map[string]string{"provider": row.Provider, "public_model": row.Model, "direction": "prompt"}
		componentLabels := map[string]string{"provider": row.Provider, "public_model": row.Model}
		writePromIntMetric(w, "vekil_tokens_total", labels, row.PromptTokens)
		writePromIntMetric(w, "vekil_tokens_total", mapWithLabel(labels, "direction", "completion"), row.CompletionTokens)
		writePromIntMetric(w, "vekil_tokens_reported_total", componentLabels, row.TotalTokens)
		writePromIntMetric(w, "vekil_tokens_cached_total", componentLabels, row.CachedTokens)
		writePromIntMetric(w, "vekil_tokens_reasoning_total", componentLabels, row.ReasoningTokens)
		if row.CostUSD > 0 {
			writePromFloatMetric(w, "vekil_cost_usd_total", componentLabels, row.CostUSD)
		}
	}

	_, _ = fmt.Fprintln(w, "# HELP vekil_retries_total Total upstream retry attempts.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_retries_total counter")
	writePromIntMetric(w, "vekil_retries_total", nil, snap.Retries)
	_, _ = fmt.Fprintln(w, "# HELP vekil_retries_by_reason_total Total upstream retry attempts by reason/status.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_retries_by_reason_total counter")
	for _, row := range snap.RetryMetrics {
		writePromIntMetric(w, "vekil_retries_by_reason_total", map[string]string{"provider": row.Provider, "public_model": row.Model, "reason": row.Reason}, row.Count)
	}

	_, _ = fmt.Fprintln(w, "# HELP vekil_upstream_errors_total Total upstream/proxy errors by provider, public model, and status code.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_upstream_errors_total counter")
	for _, row := range snap.UpstreamErrorMetrics {
		writePromIntMetric(w, "vekil_upstream_errors_total", map[string]string{"provider": row.Provider, "public_model": row.Model, "code": fmt.Sprint(row.Code)}, row.Count)
	}

	_, _ = fmt.Fprintln(w, "# HELP vekil_inflight_requests Current in-flight Vekil proxy requests by provider.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_inflight_requests gauge")
	for _, row := range snap.InflightMetrics {
		writePromIntMetric(w, "vekil_inflight_requests", map[string]string{"provider": row.Provider}, row.Count)
	}

	_, _ = fmt.Fprintln(w, "# HELP vekil_build_info Build and runtime information for Vekil.")
	_, _ = fmt.Fprintln(w, "# TYPE vekil_build_info gauge")
	writePromIntMetric(w, "vekil_build_info", map[string]string{"version": metricsBuildVersion, "go_version": runtime.Version(), "commit": metricsBuildCommit}, 1)

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

func mapWithLabel(labels map[string]string, key, value string) map[string]string {
	out := make(map[string]string, len(labels)+1)
	for k, v := range labels {
		out[k] = v
	}
	out[key] = value
	return out
}

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
		parts = append(parts, key+"=\""+promLabelValue(labels[key])+"\"")
	}
	return "{" + strings.Join(parts, ",") + "}"
}

func promLabelValue(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '%':
			b.WriteString("%25")
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		default:
			if r < 0x20 || r == 0x7f {
				_, _ = fmt.Fprintf(&b, "%%%02X", r)
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}
