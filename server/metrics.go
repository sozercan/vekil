package server

import (
	"net/http"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type metricsConfig struct {
	enabled      bool
	buildVersion string
}

type serverMetrics struct {
	registry      *prometheus.Registry
	requestsTotal *prometheus.CounterVec
}

func newServerMetrics(buildVersion string) (*serverMetrics, error) {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vekil_build_info",
		Help: "Build information for the running Vekil binary.",
	}, []string{"version"})
	buildInfo.WithLabelValues(normalizeBuildVersion(buildVersion)).Set(1)
	registry.MustRegister(buildInfo)

	requestsTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vekil_http_requests_total",
		Help: "Total number of HTTP requests handled by Vekil.",
	}, []string{"handler", "code", "method"})
	registry.MustRegister(requestsTotal)

	return &serverMetrics{
		registry:      registry,
		requestsTotal: requestsTotal,
	}, nil
}

func (m *serverMetrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *serverMetrics) instrument(pattern string, h http.Handler) http.Handler {
	if m == nil {
		return h
	}
	return promhttp.InstrumentHandlerCounter(
		m.requestsTotal.MustCurryWith(prometheus.Labels{"handler": routeLabel(pattern)}),
		h,
	)
}

func normalizeBuildVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "dev"
	}
	return version
}

func routeLabel(pattern string) string {
	if idx := strings.IndexByte(pattern, ' '); idx >= 0 {
		return pattern[idx+1:]
	}
	return pattern
}
