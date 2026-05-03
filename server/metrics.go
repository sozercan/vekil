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

type serverMetrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
}

type buildInfo struct {
	version   string
	revision  string
	goVersion string
}

func newServerMetrics() *serverMetrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector())

	info := currentBuildInfo()
	buildMetric := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "vekil",
			Name:      "build_info",
			Help:      "A metric with a constant '1' value labeled by Vekil build information.",
		},
		[]string{"version", "revision", "go_version"},
	)
	buildMetric.WithLabelValues(info.version, info.revision, info.goVersion).Set(1)

	requests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "vekil",
			Subsystem: "http",
			Name:      "requests_total",
			Help:      "Total number of completed HTTP requests handled by Vekil.",
		},
		[]string{"handler", "code", "method"},
	)

	registry.MustRegister(buildMetric, requests)
	return &serverMetrics{
		registry: registry,
		requests: requests,
	}
}

func (m *serverMetrics) metricsHandler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *serverMetrics) instrument(label string, next http.HandlerFunc) http.Handler {
	counter := m.requests.MustCurryWith(prometheus.Labels{"handler": label})
	return promhttp.InstrumentHandlerCounter(counter, http.HandlerFunc(next))
}

func currentBuildInfo() buildInfo {
	info := buildInfo{
		version:   "dev",
		revision:  "unknown",
		goVersion: runtime.Version(),
	}

	if bi, ok := debug.ReadBuildInfo(); ok {
		if version := strings.TrimSpace(bi.Main.Version); version != "" && version != "(devel)" {
			info.version = version
		}
		if goVersion := strings.TrimSpace(bi.GoVersion); goVersion != "" {
			info.goVersion = goVersion
		}

		modified := false
		for _, setting := range bi.Settings {
			switch setting.Key {
			case "vcs.revision":
				if revision := strings.TrimSpace(setting.Value); revision != "" {
					info.revision = revision
				}
			case "vcs.modified":
				modified = strings.EqualFold(strings.TrimSpace(setting.Value), "true")
			}
		}
		if modified && info.revision != "unknown" && !strings.HasSuffix(info.revision, "-dirty") {
			info.revision += "-dirty"
		}
	}

	return info
}
