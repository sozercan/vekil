package server

import (
	"net/http"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type serverMetrics struct {
	registry *prometheus.Registry
	requests *prometheus.CounterVec
}

func newServerMetrics(buildVersion string) (*serverMetrics, error) {
	registry := prometheus.NewRegistry()
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "vekil",
		Name:      "build_info",
		Help:      "Build information for the running Vekil binary.",
	}, []string{"version"})
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vekil",
		Subsystem: "http",
		Name:      "requests_total",
		Help:      "Total number of HTTP requests handled by Vekil.",
	}, []string{"route", "method", "code"})

	for _, collector := range []prometheus.Collector{
		collectors.NewGoCollector(),
		buildInfo,
		requests,
	} {
		if err := registry.Register(collector); err != nil {
			return nil, err
		}
	}

	buildInfo.WithLabelValues(normalizeBuildVersion(buildVersion)).Set(1)

	return &serverMetrics{
		registry: registry,
		requests: requests,
	}, nil
}

func (m *serverMetrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *serverMetrics) instrument(route string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		rw := &metricsResponseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		next(rw, r)

		m.requests.WithLabelValues(route, r.Method, strconv.Itoa(rw.statusCode)).Inc()
	}
}

func normalizeBuildVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "dev"
	}
	return version
}

type metricsResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func (w *metricsResponseWriter) WriteHeader(statusCode int) {
	if !w.wroteHeader {
		w.statusCode = statusCode
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *metricsResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.wroteHeader = true
	}
	return w.ResponseWriter.Write(p)
}

func (w *metricsResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *metricsResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}
