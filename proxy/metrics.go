package proxy

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// HandleMetrics renders the in-memory traffic stats as Prometheus-compatible
// text exposition. It intentionally reuses the dashboard stats collector so the
// scrape path stays dependency-free and matches /stats.json totals.
func (h *ProxyHandler) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	var snap statsSnapshot
	if h != nil && h.stats != nil {
		snap = h.stats.snapshot()
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")

	fmt.Fprintln(w, "# HELP vekil_requests_total Total Vekil proxy requests by provider, public model, endpoint, and status.")
	fmt.Fprintln(w, "# TYPE vekil_requests_total counter")
	writePromMetric(w, "vekil_requests_total", map[string]string{"provider": "", "public_model": "", "endpoint": "", "status": "total"}, snap.Totals.Requests)
	writePromMetric(w, "vekil_requests_total", map[string]string{"provider": "", "public_model": "", "endpoint": "", "status": "error"}, snap.Totals.Errors)
	for _, row := range snap.ByProvider {
		writePromMetric(w, "vekil_requests_total", map[string]string{"provider": row.Provider, "public_model": "", "endpoint": "", "status": "total"}, row.Requests)
		writePromMetric(w, "vekil_requests_total", map[string]string{"provider": row.Provider, "public_model": "", "endpoint": "", "status": "error"}, row.Errors)
	}
	for _, row := range snap.ByModel {
		writePromMetric(w, "vekil_requests_total", map[string]string{"provider": "", "public_model": row.Model, "endpoint": "", "status": "total"}, row.Requests)
		writePromMetric(w, "vekil_requests_total", map[string]string{"provider": "", "public_model": row.Model, "endpoint": "", "status": "error"}, row.Errors)
	}

	fmt.Fprintln(w, "# HELP vekil_request_latency_milliseconds Recent request latency quantiles in milliseconds.")
	fmt.Fprintln(w, "# TYPE vekil_request_latency_milliseconds gauge")
	writePromMetric(w, "vekil_request_latency_milliseconds", map[string]string{"quantile": "0.50"}, snap.Totals.LatencyP50)
	writePromMetric(w, "vekil_request_latency_milliseconds", map[string]string{"quantile": "0.95"}, snap.Totals.LatencyP95)
	writePromMetric(w, "vekil_request_latency_milliseconds", map[string]string{"quantile": "0.99"}, snap.Totals.LatencyP99)

	fmt.Fprintln(w, "# HELP vekil_tokens_total Total observed upstream tokens by direction.")
	fmt.Fprintln(w, "# TYPE vekil_tokens_total counter")
	writePromMetric(w, "vekil_tokens_total", map[string]string{"provider": "", "public_model": "", "direction": "prompt"}, snap.Totals.PromptTokens)
	writePromMetric(w, "vekil_tokens_total", map[string]string{"provider": "", "public_model": "", "direction": "completion"}, snap.Totals.CompletionTokens)
	writePromMetric(w, "vekil_tokens_total", map[string]string{"provider": "", "public_model": "", "direction": "total"}, snap.Totals.TotalTokens)

	fmt.Fprintln(w, "# HELP vekil_retries_total Total upstream retry attempts by reason/status.")
	fmt.Fprintln(w, "# TYPE vekil_retries_total counter")
	writePromMetric(w, "vekil_retries_total", map[string]string{"provider": "", "public_model": "", "reason": "all"}, snap.Retries)
	for _, row := range snap.RetriesByCode {
		writePromMetric(w, "vekil_retries_total", map[string]string{"provider": "", "public_model": "", "reason": row.Label}, row.Count)
	}

	fmt.Fprintln(w, "# HELP vekil_upstream_errors_total Total upstream/proxy errors by status code.")
	fmt.Fprintln(w, "# TYPE vekil_upstream_errors_total counter")
	for _, row := range snap.StatusCodes {
		writePromMetric(w, "vekil_upstream_errors_total", map[string]string{"provider": "", "public_model": "", "code": row.Label}, row.Count)
	}

	fmt.Fprintln(w, "# HELP vekil_inflight_requests Current in-flight Vekil proxy requests.")
	fmt.Fprintln(w, "# TYPE vekil_inflight_requests gauge")
	writePromMetric(w, "vekil_inflight_requests", map[string]string{"provider": ""}, snap.Inflight)

	fmt.Fprintln(w, "# HELP vekil_endpoint_healthy Endpoint health gauge. Currently reports aggregate proxy health until endpoint-level health checks land.")
	fmt.Fprintln(w, "# TYPE vekil_endpoint_healthy gauge")
	writePromMetric(w, "vekil_endpoint_healthy", map[string]string{"provider": "", "endpoint": ""}, 1)

	fmt.Fprintln(w, "# HELP vekil_build_info Build and runtime information for Vekil.")
	fmt.Fprintln(w, "# TYPE vekil_build_info gauge")
	writePromMetric(w, "vekil_build_info", map[string]string{"version": "dev", "go_version": runtime.Version(), "commit": ""}, 1)
}

func writePromMetric(w http.ResponseWriter, name string, labels map[string]string, value int64) {
	fmt.Fprintf(w, "%s%s %d\n", name, promLabels(labels), value)
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
