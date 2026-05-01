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
	handler  http.Handler
	requests *prometheus.CounterVec
}

type buildMetricInfo struct {
	version  string
	revision string
}

func newServerMetrics() *serverMetrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector())
	registry.MustRegister(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	registry.MustRegister(collectors.NewBuildInfoCollector())

	requests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_http_requests_total",
			Help: "Total HTTP requests handled by Vekil.",
		},
		[]string{"route", "code", "method"},
	)
	registry.MustRegister(requests)

	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vekil_build_info",
			Help: "Build information for the Vekil binary.",
		},
		[]string{"version", "revision", "go_version"},
	)
	registry.MustRegister(buildInfo)

	info := detectBuildMetricInfo()
	buildInfo.WithLabelValues(info.version, info.revision, runtime.Version()).Set(1)

	return &serverMetrics{
		handler:  promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		requests: requests,
	}
}

func (m *serverMetrics) instrument(route string, next http.Handler) http.Handler {
	if m == nil || next == nil {
		return next
	}

	return promhttp.InstrumentHandlerCounter(
		m.requests.MustCurryWith(prometheus.Labels{"route": route}),
		next,
	)
}

func detectBuildMetricInfo() buildMetricInfo {
	info := buildMetricInfo{
		version:  "dev",
		revision: "unknown",
	}

	buildInfo, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}

	if version := strings.TrimSpace(buildInfo.Main.Version); version != "" && version != "(devel)" {
		info.version = version
	}

	modified := false
	for _, setting := range buildInfo.Settings {
		switch setting.Key {
		case "vcs.revision":
			if revision := shortRevision(setting.Value); revision != "" {
				info.revision = revision
			}
		case "vcs.modified":
			modified = setting.Value == "true"
		}
	}

	if modified && info.revision != "unknown" {
		info.revision += "-dirty"
	}

	return info
}

func shortRevision(revision string) string {
	revision = strings.TrimSpace(revision)
	if len(revision) > 12 {
		return revision[:12]
	}
	return revision
}
