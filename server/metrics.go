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
	registry      *prometheus.Registry
	counters      map[string]*prometheus.CounterVec
	scrapeHandler http.Handler
}

func newServerMetrics() *serverMetrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(collectors.NewGoCollector())

	build := currentBuildInfo()
	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "vekil",
			Name:      "build_info",
			Help:      "Build information for the running Vekil binary.",
		},
		[]string{"version", "revision", "goversion", "vcs_modified"},
	)
	buildInfo.WithLabelValues(build.version, build.revision, build.goVersion, build.vcsModified).Set(1)
	registry.MustRegister(buildInfo)

	return &serverMetrics{
		registry:      registry,
		counters:      make(map[string]*prometheus.CounterVec),
		scrapeHandler: promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
	}
}

func (m *serverMetrics) instrument(handlerName string, next http.Handler) http.Handler {
	counter, ok := m.counters[handlerName]
	if !ok {
		counter = prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Namespace:   "vekil",
				Name:        "http_requests_total",
				Help:        "Total HTTP requests handled by Vekil, partitioned by handler.",
				ConstLabels: prometheus.Labels{"handler": handlerName},
			},
			[]string{},
		)
		m.registry.MustRegister(counter)
		m.counters[handlerName] = counter
	}
	return promhttp.InstrumentHandlerCounter(counter, next)
}

type buildInfoLabels struct {
	version     string
	revision    string
	goVersion   string
	vcsModified string
}

func currentBuildInfo() buildInfoLabels {
	labels := buildInfoLabels{
		version:     "dev",
		revision:    "unknown",
		goVersion:   runtime.Version(),
		vcsModified: "unknown",
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return labels
	}

	if goVersion := strings.TrimSpace(info.GoVersion); goVersion != "" {
		labels.goVersion = goVersion
	}

	switch version := strings.TrimSpace(info.Main.Version); version {
	case "", "(devel)":
	default:
		labels.version = version
	}

	for _, setting := range info.Settings {
		value := strings.TrimSpace(setting.Value)
		if value == "" {
			continue
		}

		switch setting.Key {
		case "vcs.revision":
			labels.revision = value
		case "vcs.modified":
			labels.vcsModified = value
		}
	}

	return labels
}
