package proxy

import (
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const metricsUnroutedModel = "unrouted"

var llmRequestDurationBuckets = []float64{
	0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10,
	20, 30, 60, 120, 300,
}

type responsesTurnMetricsRecord struct {
	valid    bool
	provider string
	model    string
}

// MetricsCollector holds all Prometheus metric objects for the proxy.
// Metrics are registered on a custom registry (not the global default) to avoid
// conflicts with other libraries and to control exactly what is exposed.
type MetricsCollector struct {
	registry *prometheus.Registry

	requestsTotal    *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	usageTotal       *prometheus.CounterVec
	retriesTotal     *prometheus.CounterVec
	upstreamErrors   *prometheus.CounterVec
	inflightRequests prometheus.Gauge
	buildInfo        *prometheus.GaugeVec

	modelLabelsMu sync.Mutex
	modelLabels   map[string]struct{}
}

// NewMetricsCollector creates a MetricsCollector with all metrics registered on
// a custom prometheus.Registry. The registry also includes standard Go runtime
// and process metrics.
func NewMetricsCollector() *MetricsCollector {
	reg := prometheus.NewRegistry()

	m := &MetricsCollector{
		registry:    reg,
		modelLabels: make(map[string]struct{}, statsMaxKeys),
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_requests_total",
			Help: "Total number of requests processed, labeled by provider, model, endpoint, and status.",
		}, []string{"provider", "public_model", "endpoint", "status"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vekil_request_duration_seconds",
			Help:    "Request duration in seconds (non-streaming requests only).",
			Buckets: llmRequestDurationBuckets,
		}, []string{"provider", "public_model", "endpoint"}),
		usageTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_tokens_total",
			Help: "Total tokens processed, labeled by direction (prompt or completion).",
		}, []string{"provider", "public_model", "direction"}),
		retriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_retries_total",
			Help: "Total upstream retry attempts, labeled by reason category.",
		}, []string{"provider", "public_model", "reason"}),
		upstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_upstream_errors_total",
			Help: "Total errors from attempted upstream requests, labeled by final HTTP status code.",
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
		m.usageTotal,
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
	if status == 0 {
		status = http.StatusOK
	}

	d := readSummaryForStats(summary)
	provider := d.provider
	model := m.modelLabel(provider, d.metricModel, d.modelKnown)
	endpoint := d.endpoint
	statusStr := strconv.Itoa(status)

	m.requestsTotal.WithLabelValues(provider, model, endpoint, statusStr).Inc()

	// Only observe duration for non-streaming requests (streaming durations are
	// dominated by client hold time and would poison histograms).
	if !d.stream && dur > 0 {
		m.requestDuration.WithLabelValues(provider, model, endpoint).Observe(dur.Seconds())
	}

	m.addTokens(provider, model, d.prompt, d.completion)

	if d.upstreamAttempted && status >= http.StatusBadRequest {
		m.upstreamErrors.WithLabelValues(provider, model, statusStr).Inc()
	}
}

// RecordResponsesTurn records a websocket-bridge turn into Prometheus metrics.
// Websocket turn failures are recorded after the upstream request path starts.
func (m *MetricsCollector) RecordResponsesTurn(model, provider string, status int, usage responsesUsage) responsesTurnMetricsRecord {
	return m.recordResponsesTurn(model, provider, status, usage, true, true)
}

func (m *MetricsCollector) recordResponsesTurn(model, provider string, status int, usage responsesUsage, upstreamAttempted, modelKnown bool) responsesTurnMetricsRecord {
	if m == nil {
		return responsesTurnMetricsRecord{}
	}
	if status == 0 {
		status = http.StatusOK
	}
	model = m.modelLabel(provider, model, modelKnown)
	statusStr := strconv.Itoa(status)

	m.requestsTotal.WithLabelValues(provider, model, "responses_ws", statusStr).Inc()
	m.addTokens(provider, model, usage.InputTokens, usage.OutputTokens)

	if upstreamAttempted && status >= http.StatusBadRequest {
		m.upstreamErrors.WithLabelValues(provider, model, statusStr).Inc()
	}
	return responsesTurnMetricsRecord{valid: true, provider: provider, model: model}
}

// AddResponsesTurnUsage adds post-terminal internal usage to an existing
// websocket turn without incrementing its request or error counters again.
func (m *MetricsCollector) AddResponsesTurnUsage(record responsesTurnMetricsRecord, usage responsesUsage) {
	if m == nil || !record.valid || usage.isZero() {
		return
	}
	m.addTokens(record.provider, record.model, usage.InputTokens, usage.OutputTokens)
}

func (m *MetricsCollector) addTokens(provider, model string, prompt, completion int) {
	if prompt > 0 {
		m.usageTotal.WithLabelValues(provider, model, "prompt").Add(float64(prompt))
	}
	if completion > 0 {
		m.usageTotal.WithLabelValues(provider, model, "completion").Add(float64(completion))
	}
}

// RecordRetry records one upstream retry attempt.
func (m *MetricsCollector) RecordRetry(provider, model string, status int) {
	m.recordRetry(provider, model, status, true)
}

func (m *MetricsCollector) recordRetry(provider, model string, status int, modelKnown bool) {
	if m == nil {
		return
	}
	reason := retryReasonLabel(status)
	m.retriesTotal.WithLabelValues(provider, m.modelLabel(provider, model, modelKnown), reason).Inc()
}

// modelLabel bounds both the length and lifetime cardinality of the
// client-controlled public_model dimension. All metric families share this
// registry so overflow values consistently fold into one "other" series.
func (m *MetricsCollector) modelLabel(provider, model string, modelKnown bool) string {
	if m == nil {
		return ""
	}
	model = boundStatLabel(model)
	if model == "" {
		return ""
	}
	// Only models resolved from the configured/dynamic catalog enter the
	// persistent budget. Default-provider fallbacks and locally unrouted client
	// values share one stable label so invalid names cannot force configured
	// models into the overflow bucket until restart.
	if provider == "" || !modelKnown {
		return metricsUnroutedModel
	}

	m.modelLabelsMu.Lock()
	defer m.modelLabelsMu.Unlock()
	if _, ok := m.modelLabels[model]; ok {
		return model
	}
	if len(m.modelLabels) >= statsMaxKeys {
		return statsOtherKey
	}
	m.modelLabels[model] = struct{}{}
	return model
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
