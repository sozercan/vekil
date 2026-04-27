package server

import (
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type serverMetrics struct {
	handler      http.Handler
	requestTotal *prometheus.CounterVec
}

func newServerMetrics(version string) *serverMetrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vekil_build_info",
			Help: "Build information for the running Vekil binary.",
		},
		[]string{"version"},
	)
	buildInfo.WithLabelValues(normalizeBuildVersion(version)).Set(1)

	requestTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_http_requests_total",
			Help: "Total HTTP requests handled by Vekil, partitioned by route, method, and status code.",
		},
		[]string{"route", "code", "method"},
	)

	registry.MustRegister(buildInfo, requestTotal)

	return &serverMetrics{
		handler:      promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		requestTotal: requestTotal,
	}
}

func (m *serverMetrics) instrument(route string, handler http.HandlerFunc) http.Handler {
	if m == nil {
		return http.HandlerFunc(handler)
	}

	return promhttp.InstrumentHandlerCounter(
		m.requestTotal.MustCurryWith(prometheus.Labels{"route": route}),
		http.HandlerFunc(handler),
	)
}

func normalizeBuildVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "dev"
	}
	return version
}
