package proxy

import (
	"net/http"
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MetricsCollector holds all Prometheus metric objects for the proxy.
// Metrics are registered on a custom registry (not the global default) to avoid
// conflicts with other libraries and to control exactly what is exposed.
type MetricsCollector struct {
	registry *prometheus.Registry

	requestsTotal      *prometheus.CounterVec
	requestDuration    *prometheus.HistogramVec
	tokensTotal        *prometheus.CounterVec
	retriesTotal       *prometheus.CounterVec
	upstreamErrors     *prometheus.CounterVec
	inflightRequests   prometheus.Gauge
	buildInfo          *prometheus.GaugeVec
}

// NewMetricsCollector creates a MetricsCollector with all metrics registered on
// a custom prometheus.Registry. The registry also includes standard Go runtime
// and process metrics.
func NewMetricsCollector() *MetricsCollector {
	reg := prometheus.NewRegistry()

	m := &MetricsCollector{
		registry: reg,
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_requests_total",
			Help: "Total number of requests processed, labeled by provider, model, endpoint, and status.",
		}, []string{"provider", "public_model", "endpoint", "status"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vekil_request_duration_seconds",
			Help:    "Request duration in seconds (non-streaming requests only).",
			Buckets: prometheus.DefBuckets,
		}, []string{"provider", "public_model", "endpoint"}),
		tokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_tokens_total",
			Help: "Total tokens processed, labeled by direction (prompt or completion).",
		}, []string{"provider", "public_model", "direction"}),
		retriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_retries_total",
			Help: "Total upstream retry attempts, labeled by reason category.",
		}, []string{"provider", "public_model", "reason"}),
		upstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_upstream_errors_total",
			Help: "Total upstream errors, labeled by HTTP status code.",
		}, []string{"provider", "public_model", "code"}),
		inflightRequests: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "vekil_inflight_requests",
			Help: "Number of requests currently being processed.",
		}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vekil_build_info",
			Help: "Build information about the running vekil binary. Always 1.",
		}, []string{"version", "go_version", "commit"}),
	}

	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requestsTotal,
		m.requestDuration,
		m.tokensTotal,
		m.retriesTotal,
		m.upstreamErrors,
		m.inflightRequests,
		m.buildInfo,
	)

	return m
}

// SetBuildInfo sets the vekil_build_info gauge with the provided version metadata.
func (m *MetricsCollector) SetBuildInfo(version, commit, goVersion string) {
	if m == nil {
		return
	}
	m.buildInfo.WithLabelValues(version, goVersion, commit).Set(1)
}

// Record folds a completed request into Prometheus counters and histograms.
// It mirrors the signature used by statsCollector.record so both can be called
// from the same site.
func (m *MetricsCollector) Record(summary *RequestSummary, status int, dur time.Duration) {
	if m == nil {
		return
	}

	d := readSummaryForStats(summary)
	provider := d.provider
	model := d.model
	endpoint := d.endpoint
	statusStr := strconv.Itoa(status)

	m.requestsTotal.WithLabelValues(provider, model, endpoint, statusStr).Inc()

	// Only observe duration for non-streaming requests (streaming durations are
	// dominated by client hold time and would poison histograms).
	if !d.stream && dur > 0 {
		m.requestDuration.WithLabelValues(provider, model, endpoint).Observe(dur.Seconds())
	}

	if d.prompt > 0 {
		m.tokensTotal.WithLabelValues(provider, model, "prompt").Add(float64(d.prompt))
	}
	if d.completion > 0 {
		m.tokensTotal.WithLabelValues(provider, model, "completion").Add(float64(d.completion))
	}

	if status >= http.StatusBadRequest {
		m.upstreamErrors.WithLabelValues(provider, model, statusStr).Inc()
	}
}

// RecordResponsesTurn records a websocket-bridge turn into Prometheus metrics.
func (m *MetricsCollector) RecordResponsesTurn(model, provider string, status int, usage responsesUsage) {
	if m == nil {
		return
	}
	statusStr := strconv.Itoa(status)

	m.requestsTotal.WithLabelValues(provider, model, "responses_ws", statusStr).Inc()

	prompt := usage.InputTokens
	completion := usage.OutputTokens
	if prompt > 0 {
		m.tokensTotal.WithLabelValues(provider, model, "prompt").Add(float64(prompt))
	}
	if completion > 0 {
		m.tokensTotal.WithLabelValues(provider, model, "completion").Add(float64(completion))
	}

	if status >= http.StatusBadRequest {
		m.upstreamErrors.WithLabelValues(provider, model, statusStr).Inc()
	}
}

// RecordRetry records one upstream retry attempt.
func (m *MetricsCollector) RecordRetry(provider, model string, status int) {
	if m == nil {
		return
	}
	reason := retryReasonLabel(status)
	m.retriesTotal.WithLabelValues(provider, model, reason).Inc()
}

// IncInflight increments the in-flight request gauge.
func (m *MetricsCollector) IncInflight() {
	if m == nil {
		return
	}
	m.inflightRequests.Inc()
}

// DecInflight decrements the in-flight request gauge.
func (m *MetricsCollector) DecInflight() {
	if m == nil {
		return
	}
	m.inflightRequests.Dec()
}

// Handler returns an http.Handler that serves the Prometheus metrics exposition
// format from the custom registry.
func (m *MetricsCollector) Handler() http.Handler {
	if m == nil {
		return nil
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: true,
	})
}

// retryReasonLabel maps the upstream status code that triggered a retry to a
// human-readable reason label for the vekil_retries_total metric.
func retryReasonLabel(status int) string {
	switch {
	case status == 0:
		return "timeout"
	case status == http.StatusTooManyRequests:
		return "429"
	case status >= 500:
		return "5xx"
	default:
		return "other"
	}
}
