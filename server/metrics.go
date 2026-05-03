package server

import (
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type metrics struct {
	registry       *prometheus.Registry
	requestCounter *prometheus.CounterVec
}

func newMetrics() *metrics {
	registry := prometheus.NewRegistry()

	// Keep labels bounded to static route names plus method/status code.
	requestCounter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_http_requests_total",
			Help: "Total number of HTTP requests handled by the Vekil HTTP server.",
		},
		[]string{"route", "code", "method"},
	)

	version, revision, goVersion := currentBuildInfo()
	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vekil_build_info",
			Help: "Build information for the running Vekil server.",
		},
		[]string{"version", "revision", "go_version"},
	)
	buildInfo.WithLabelValues(version, revision, goVersion).Set(1)

	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		buildInfo,
		requestCounter,
	)

	return &metrics{
		registry:       registry,
		requestCounter: requestCounter,
	}
}

func (m *metrics) instrument(route string, next http.Handler) http.Handler {
	if m == nil {
		return next
	}

	return promhttp.InstrumentHandlerCounter(
		m.requestCounter.MustCurryWith(prometheus.Labels{"route": route}),
		next,
	)
}

func (m *metrics) handler() http.Handler {
	if m == nil {
		return http.NotFoundHandler()
	}

	return promhttp.InstrumentMetricHandler(
		m.registry,
		promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{}),
	)
}

func currentBuildInfo() (version, revision, goVersion string) {
	version = "dev"
	revision = "unknown"
	goVersion = runtime.Version()

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return version, revision, goVersion
	}

	if trimmed := strings.TrimSpace(info.GoVersion); trimmed != "" {
		goVersion = trimmed
	}
	if trimmed := strings.TrimSpace(info.Main.Version); trimmed != "" && trimmed != "(devel)" {
		version = trimmed
	}

	for _, setting := range info.Settings {
		if setting.Key != "vcs.revision" {
			continue
		}
		if trimmed := strings.TrimSpace(setting.Value); trimmed != "" {
			revision = shortRevision(trimmed)
		}
		break
	}

	return version, revision, goVersion
}

func shortRevision(revision string) string {
	const maxRevisionLength = 12
	if len(revision) > maxRevisionLength {
		return revision[:maxRevisionLength]
	}
	return revision
}
