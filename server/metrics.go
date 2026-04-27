package server

import (
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type serverMetrics struct {
	registry      *prometheus.Registry
	requestsTotal *prometheus.CounterVec
}

func newServerMetrics(buildVersion string) *serverMetrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
		prometheus.NewGoCollector(),
	)

	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vekil_build_info",
			Help: "Build information for the running Vekil binary.",
		},
		[]string{"version"},
	)
	buildInfo.WithLabelValues(normalizeBuildVersion(buildVersion)).Set(1)
	registry.MustRegister(buildInfo)

	requestsTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_http_requests_total",
			Help: "Total HTTP requests handled by Vekil, labeled by bounded route, method, and status code.",
		},
		[]string{"handler", "method", "code"},
	)
	registry.MustRegister(requestsTotal)

	return &serverMetrics{
		registry:      registry,
		requestsTotal: requestsTotal,
	}
}

func (m *serverMetrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *serverMetrics) instrument(handlerLabel string, next http.HandlerFunc) http.HandlerFunc {
	counter := promhttp.InstrumentHandlerCounter(
		m.requestsTotal.MustCurryWith(prometheus.Labels{"handler": handlerLabel}),
		http.HandlerFunc(next),
	)
	return func(w http.ResponseWriter, r *http.Request) {
		counter.ServeHTTP(w, r)
	}
}

func normalizeBuildVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "dev"
	}
	return version
}
