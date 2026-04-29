package server

import (
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// buildVersion is injected by release builds via ldflags.
var buildVersion = "dev"

type serverMetrics struct {
	requests *prometheus.CounterVec
	scrape   http.Handler
}

func newServerMetrics() *serverMetrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vekil_build_info",
		Help: "Build information for the running Vekil binary.",
	}, []string{"version"})
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vekil_http_requests_total",
		Help: "Total number of HTTP requests handled by Vekil.",
	}, []string{"route", "method", "code"})

	registry.MustRegister(buildInfo, requests)
	buildInfo.WithLabelValues(normalizeBuildVersion(buildVersion)).Set(1)

	return &serverMetrics{
		requests: requests,
		scrape:   promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
	}
}

func normalizeBuildVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "dev"
	}
	return version
}

func (m *serverMetrics) handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}
	return m.scrape
}

func (m *serverMetrics) instrument(route string, handler http.Handler) http.Handler {
	if m == nil || handler == nil {
		return handler
	}
	return promhttp.InstrumentHandlerCounter(
		m.requests.MustCurryWith(prometheus.Labels{"route": route}),
		handler,
	)
}
