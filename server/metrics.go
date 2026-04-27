package server

import (
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type metrics struct {
	registry *prometheus.Registry
	handler  http.Handler
	requests *prometheus.CounterVec
}

func newMetrics(version string) *metrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	requests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_http_requests_total",
			Help: "Total HTTP requests handled by Vekil.",
		},
		[]string{"handler", "code", "method"},
	)

	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vekil_build_info",
			Help: "Build information for the running Vekil binary.",
		},
		[]string{"version"},
	)

	registry.MustRegister(requests, buildInfo)
	buildInfo.WithLabelValues(normalizeBuildVersion(version)).Set(1)

	handler := promhttp.HandlerFor(registry, promhttp.HandlerOpts{})

	return &metrics{
		registry: registry,
		handler:  promhttp.InstrumentMetricHandler(registry, handler),
		requests: requests,
	}
}

func (m *metrics) instrument(pattern string, next http.HandlerFunc) http.Handler {
	if m == nil {
		return next
	}

	handler := m.requests.MustCurryWith(prometheus.Labels{
		"handler": metricsHandlerLabel(pattern),
	})

	return promhttp.InstrumentHandlerCounter(handler, next)
}

func normalizeBuildVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "dev"
	}
	return version
}

func metricsHandlerLabel(pattern string) string {
	if _, path, ok := strings.Cut(pattern, " "); ok {
		pattern = path
	}
	if strings.HasSuffix(pattern, "/") {
		return pattern + "*"
	}
	return pattern
}
