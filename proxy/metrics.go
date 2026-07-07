package proxy

import (
	"net/http"
	"runtime"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// metricsRegistry holds Prometheus collectors for the proxy's observability
// endpoint. A nil *metricsRegistry is a safe no-op (all emit methods check for
// nil) so metric emission adds zero overhead when metrics are disabled.
type metricsRegistry struct {
	registry *prometheus.Registry

	requestsTotal       *prometheus.CounterVec
	requestDuration     *prometheus.HistogramVec
	firstByteDuration   *prometheus.HistogramVec
	tokensTotal         *prometheus.CounterVec
	retriesTotal        *prometheus.CounterVec
	upstreamErrorsTotal *prometheus.CounterVec
	inflightRequests    *prometheus.GaugeVec
	endpointHealthy     *prometheus.GaugeVec
	buildInfo           *prometheus.GaugeVec
}

// newMetricsRegistry creates and registers all vekil_* collectors plus standard
// Go runtime and process collectors on a dedicated prometheus.Registry.
func newMetricsRegistry(version, commit, goVersion string) *metricsRegistry {
	reg := prometheus.NewRegistry()

	m := &metricsRegistry{
		registry: reg,
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_requests_total",
			Help: "Total number of proxy requests by provider, model, endpoint, and status.",
		}, []string{"provider", "public_model", "endpoint", "status"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vekil_request_duration_seconds",
			Help:    "End-to-end request duration in seconds (observed on stream close for streaming requests).",
			Buckets: prometheus.DefBuckets, // .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10
		}, []string{"provider", "public_model", "endpoint"}),
		firstByteDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vekil_first_byte_duration_seconds",
			Help:    "Time to first byte for streaming requests in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"provider", "public_model", "endpoint"}),
		tokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_tokens_total",
			Help: "Total tokens processed by provider, model, and direction (prompt or completion).",
		}, []string{"provider", "public_model", "direction"}),
		retriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_retries_total",
			Help: "Total upstream retry attempts by provider, model, and reason.",
		}, []string{"provider", "public_model", "reason"}),
		upstreamErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_upstream_errors_total",
			Help: "Total upstream error responses by provider, model, and HTTP status code.",
		}, []string{"provider", "public_model", "code"}),
		inflightRequests: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vekil_inflight_requests",
			Help: "Number of currently in-flight requests per provider.",
		}, []string{"provider"}),
		endpointHealthy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vekil_endpoint_healthy",
			Help: "Whether a provider endpoint is healthy (1) or unhealthy (0). Populated by the multi-endpoint selector.",
		}, []string{"provider", "endpoint"}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vekil_build_info",
			Help: "Build information gauge with version, go_version, and commit labels. Always 1.",
		}, []string{"version", "go_version", "commit"}),
	}

	// Register all vekil_* collectors.
	reg.MustRegister(
		m.requestsTotal,
		m.requestDuration,
		m.firstByteDuration,
		m.tokensTotal,
		m.retriesTotal,
		m.upstreamErrorsTotal,
		m.inflightRequests,
		m.endpointHealthy,
		m.buildInfo,
	)

	// Register standard Go runtime and process collectors.
	reg.MustRegister(collectors.NewGoCollector())
	reg.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))

	// Set build info gauge to 1.
	if version == "" {
		version = "dev"
	}
	if goVersion == "" {
		goVersion = runtime.Version()
	}
	if commit == "" {
		commit = "unknown"
	}
	m.buildInfo.WithLabelValues(version, goVersion, commit).Set(1)

	return m
}

// handler returns an http.Handler serving the /metrics endpoint from the
// dedicated registry.
func (m *metricsRegistry) handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		EnableOpenMetrics: false,
	})
}

// observeRequest records a completed request into Prometheus counters and
// histograms. It mirrors the statsCollector.record path.
func (m *metricsRegistry) observeRequest(d summaryStats, status int, durationSec float64) {
	if m == nil {
		return
	}
	provider := d.provider
	if provider == "" {
		provider = "unknown"
	}
	model := boundMetricModelLabel(d.model)
	endpoint := d.endpoint
	if endpoint == "" {
		endpoint = "unknown"
	}
	statusStr := strconv.Itoa(status)

	m.requestsTotal.WithLabelValues(provider, model, endpoint, statusStr).Inc()
	m.requestDuration.WithLabelValues(provider, model, endpoint).Observe(durationSec)

	if d.prompt > 0 {
		m.tokensTotal.WithLabelValues(provider, model, "prompt").Add(float64(d.prompt))
	}
	if d.completion > 0 {
		m.tokensTotal.WithLabelValues(provider, model, "completion").Add(float64(d.completion))
	}

	if status >= http.StatusBadRequest {
		m.upstreamErrorsTotal.WithLabelValues(provider, model, statusStr).Inc()
	}
}

// observeRetry records one upstream retry attempt.
func (m *metricsRegistry) observeRetry(provider, model string, status int) {
	if m == nil {
		return
	}
	if provider == "" {
		provider = "unknown"
	}
	model = boundMetricModelLabel(model)
	reason := retryReason(status)
	m.retriesTotal.WithLabelValues(provider, model, reason).Inc()
}

// incInflight increments the in-flight gauge for the given provider.
func (m *metricsRegistry) incInflight(provider string) {
	if m == nil {
		return
	}
	if provider == "" {
		provider = "unknown"
	}
	m.inflightRequests.WithLabelValues(provider).Inc()
}

// decInflight decrements the in-flight gauge for the given provider.
func (m *metricsRegistry) decInflight(provider string) {
	if m == nil {
		return
	}
	if provider == "" {
		provider = "unknown"
	}
	m.inflightRequests.WithLabelValues(provider).Dec()
}

// retryReason maps an upstream status code to a Prometheus-friendly reason label.
func retryReason(status int) string {
	switch {
	case status == 0:
		return "timeout"
	case status == http.StatusTooManyRequests:
		return "429"
	case status >= 500:
		return "5xx"
	default:
		return strconv.Itoa(status)
	}
}

// boundMetricModelLabel bounds model names for Prometheus label safety. Reuses
// the same statLabelMaxLen + capKey-style logic from the dashboard stats to
// prevent client-controlled model strings from creating unbounded cardinality.
func boundMetricModelLabel(model string) string {
	if model == "" {
		return "unknown"
	}
	return boundStatLabel(model)
}
