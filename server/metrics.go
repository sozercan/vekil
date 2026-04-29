package server

import (
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type serverMetrics struct {
	requests      *prometheus.CounterVec
	metricsHandle http.Handler
}

func newServerMetrics(buildVersion string) *serverMetrics {
	if strings.TrimSpace(buildVersion) == "" {
		buildVersion = "dev"
	}

	registry := prometheus.NewPedanticRegistry()
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
	buildInfo.WithLabelValues(buildVersion).Set(1)

	requests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_http_requests_total",
			Help: "Total HTTP requests handled by Vekil, labeled by method, route pattern, and status code.",
		},
		[]string{"method", "route", "code"},
	)

	registry.MustRegister(buildInfo, requests)

	return &serverMetrics{
		requests:      requests,
		metricsHandle: promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
	}
}

func (m *serverMetrics) handler() http.Handler {
	return m.metricsHandle
}

func (m *serverMetrics) wrap(route string, next http.Handler) http.Handler {
	return promhttp.InstrumentHandlerCounter(
		m.requests.MustCurryWith(prometheus.Labels{"route": route}),
		next,
	)
}
