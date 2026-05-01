package server

import (
	"net/http"
	"runtime"
	"runtime/debug"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type metricsState struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
}

type buildInfoLabels struct {
	version   string
	revision  string
	goVersion string
	modified  string
}

func newMetricsState() *metricsState {
	registry := prometheus.NewRegistry()
	requests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "vekil",
			Name:      "http_requests_total",
			Help:      "Total HTTP requests handled by Vekil, labeled by route and response code.",
		},
		[]string{"route", "code"},
	)
	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "vekil",
			Name:      "build_info",
			Help:      "Build and runtime information about the running Vekil binary.",
		},
		[]string{"version", "revision", "go_version", "modified"},
	)

	registry.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
		requests,
		buildInfo,
	)

	info := currentBuildInfoLabels()
	buildInfo.WithLabelValues(info.version, info.revision, info.goVersion, info.modified).Set(1)

	return &metricsState{
		registry: registry,
		requests: requests,
	}
}

func (m *metricsState) instrument(route string, handler http.Handler) http.Handler {
	return promhttp.InstrumentHandlerCounter(
		m.requests.MustCurryWith(prometheus.Labels{"route": route}),
		handler,
	)
}

func (m *metricsState) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func currentBuildInfoLabels() buildInfoLabels {
	info := buildInfoLabels{
		version:   "dev",
		revision:  "unknown",
		goVersion: runtime.Version(),
		modified:  "unknown",
	}

	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}

	if version := strings.TrimSpace(buildInfo.GoVersion); version != "" {
		info.goVersion = version
	}
	if version := strings.TrimSpace(buildInfo.Main.Version); version != "" && version != "(devel)" {
		info.version = version
	}

	for _, setting := range buildInfo.Settings {
		value := strings.TrimSpace(setting.Value)
		if value == "" {
			continue
		}

		switch setting.Key {
		case "vcs.revision":
			info.revision = value
		case "vcs.modified":
			info.modified = value
		}
	}

	return info
}
