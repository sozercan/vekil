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

func newServerMetrics() *serverMetrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector())

	buildInfoMetric := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "vekil",
			Name:      "build_info",
			Help:      "Build information for the running Vekil binary.",
		},
		[]string{"version", "revision", "goversion"},
	)

	info := currentBuildInfo()
	buildInfoMetric.WithLabelValues(info.version, info.revision, info.goVersion).Set(1)

	requests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "vekil",
			Name:      "http_requests_total",
			Help:      "Total HTTP requests handled by Vekil partitioned by handler, method, and response code.",
		},
		[]string{"handler", "code", "method"},
	)

	registry.MustRegister(buildInfoMetric, requests)

	return &serverMetrics{
		registry: registry,
		requests: requests,
	}
}

func (m *serverMetrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *serverMetrics) instrumentHandler(name string, handler http.Handler) http.Handler {
	return promhttp.InstrumentHandlerCounter(
		m.requests.MustCurryWith(prometheus.Labels{"handler": name}),
		handler,
	)
}

type buildInfo struct {
	version   string
	revision  string
	goVersion string
}

func currentBuildInfo() buildInfo {
	info := buildInfo{
		version:   "dev",
		revision:  "unknown",
		goVersion: runtime.Version(),
	}

	if build, ok := debug.ReadBuildInfo(); ok {
		if v := strings.TrimSpace(build.GoVersion); v != "" {
			info.goVersion = v
		}
		if v := strings.TrimSpace(build.Main.Version); v != "" && v != "(devel)" {
			info.version = v
		}

		var revision string
		var modified bool
		for _, setting := range build.Settings {
			switch setting.Key {
			case "vcs.revision":
				revision = strings.TrimSpace(setting.Value)
			case "vcs.modified":
				modified = setting.Value == "true"
			}
		}

		if revision != "" {
			if modified {
				revision += "-dirty"
			}
			info.revision = revision
		} else if modified {
			info.revision = "dirty"
		}
	}

	return info
}
