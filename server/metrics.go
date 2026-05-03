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
	requests       *prometheus.CounterVec
	metricsHandler http.Handler
}

func newServerMetrics() *serverMetrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector())

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "vekil",
		Name:      "build_info",
		Help:      "Build information about the running Vekil binary.",
	}, []string{"version", "revision", "go_version"})

	info := currentBuildMetricInfo()
	buildInfo.WithLabelValues(info.version, info.revision, info.goVersion).Set(1)

	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vekil",
		Name:      "http_requests_total",
		Help:      "Total number of HTTP requests handled by Vekil.",
	}, []string{"handler", "method"})

	registry.MustRegister(buildInfo, requests)

	return &serverMetrics{
		requests: requests,
		metricsHandler: promhttp.InstrumentMetricHandler(
			registry,
			promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		),
	}
}

func (m *serverMetrics) handler() http.Handler {
	if m == nil {
		return nil
	}
	return m.metricsHandler
}

func (m *serverMetrics) instrument(route, method string, next http.Handler) http.Handler {
	if m == nil || next == nil {
		return next
	}

	normalizedMethod := strings.ToLower(strings.TrimSpace(method))
	if normalizedMethod == "" {
		normalizedMethod = "unknown"
	}

	counter := m.requests.WithLabelValues(route, normalizedMethod)
	counter.Add(0)

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		counter.Inc()
		next.ServeHTTP(w, r)
	})
}

type buildMetricInfo struct {
	version   string
	revision  string
	goVersion string
}

func currentBuildMetricInfo() buildMetricInfo {
	info := buildMetricInfo{
		version:   "dev",
		revision:  "unknown",
		goVersion: runtime.Version(),
	}

	buildInfo, ok := debug.ReadBuildInfo()
	if !ok || buildInfo == nil {
		return info
	}

	if version := strings.TrimSpace(buildInfo.Main.Version); version != "" && version != "(devel)" {
		info.version = version
	}
	if goVersion := strings.TrimSpace(buildInfo.GoVersion); goVersion != "" {
		info.goVersion = goVersion
	}

	dirty := false
	for _, setting := range buildInfo.Settings {
		switch setting.Key {
		case "vcs.revision":
			if revision := strings.TrimSpace(setting.Value); revision != "" {
				info.revision = revision
			}
		case "vcs.modified":
			dirty = strings.EqualFold(strings.TrimSpace(setting.Value), "true")
		}
	}

	if dirty {
		if info.revision == "unknown" {
			info.revision = "dirty"
		} else if !strings.HasSuffix(info.revision, "-dirty") {
			info.revision += "-dirty"
		}
	}

	return info
}
