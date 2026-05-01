package server

import (
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type metricsHandler struct {
	requestCounter  *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	metrics         http.Handler
}

func newMetricsHandler(buildVersion string) *metricsHandler {
	registry := prometheus.NewRegistry()

	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "vekil",
			Name:      "build_info",
			Help:      "Build information for the running Vekil process.",
		},
		[]string{"version"},
	)
	buildInfo.WithLabelValues(normalizeBuildVersion(buildVersion)).Set(1)

	requestCounter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "vekil",
			Name:      "http_requests_total",
			Help:      "Total number of HTTP requests handled by Vekil.",
		},
		[]string{"handler", "code", "method"},
	)
	requestDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Namespace: "vekil",
			Name:      "http_request_duration_seconds",
			Help:      "Duration of HTTP requests handled by Vekil.",
			Buckets:   prometheus.DefBuckets,
		},
		[]string{"handler", "method"},
	)

	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo,
		requestCounter,
		requestDuration,
	)

	return &metricsHandler{
		requestCounter:  requestCounter,
		requestDuration: requestDuration,
		metrics:         promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
	}
}

func normalizeBuildVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "dev"
	}
	return version
}

func (m *metricsHandler) handler() http.Handler {
	return m.metrics
}

func (m *metricsHandler) instrument(name string, next http.Handler) http.Handler {
	if m == nil {
		return next
	}

	counter := m.requestCounter.MustCurryWith(prometheus.Labels{"handler": name})
	duration := m.requestDuration.MustCurryWith(prometheus.Labels{"handler": name})

	return promhttp.InstrumentHandlerDuration(
		duration,
		promhttp.InstrumentHandlerCounter(counter, next),
	)
}
