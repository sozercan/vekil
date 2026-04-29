package server

import (
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const defaultBuildVersion = "dev"

type httpMetrics struct {
	registry       *prometheus.Registry
	requestCounter *prometheus.CounterVec
}

func newHTTPMetrics(buildVersion string) *httpMetrics {
	version := strings.TrimSpace(buildVersion)
	if version == "" {
		version = defaultBuildVersion
	}

	registry := prometheus.NewRegistry()
	requestCounter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_http_requests_total",
			Help: "Total number of HTTP requests handled by Vekil, partitioned by route, method, and status code.",
		},
		[]string{"route", "method", "code"},
	)
	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vekil_build_info",
			Help: "Build information for the running Vekil binary.",
		},
		[]string{"version"},
	)
	buildInfo.WithLabelValues(version).Set(1)

	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		requestCounter,
		buildInfo,
	)

	return &httpMetrics{
		registry:       registry,
		requestCounter: requestCounter,
	}
}

func (m *httpMetrics) instrumentHandler(route string, next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	return promhttp.InstrumentHandlerCounter(
		m.requestCounter.MustCurryWith(prometheus.Labels{"route": route}),
		next,
	)
}

func (m *httpMetrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
