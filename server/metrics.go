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
	handler  http.Handler
	requests *prometheus.CounterVec
}

func newServerMetrics(buildVersion string) *serverMetrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: "vekil",
		Name:      "build_info",
		Help:      "Build information for the running Vekil binary.",
	}, []string{"version"})
	requests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: "vekil",
		Name:      "http_requests_total",
		Help:      "Total HTTP requests handled by bounded server routes.",
	}, []string{"handler", "method", "code"})
	registry.MustRegister(buildInfo, requests)

	buildInfo.WithLabelValues(normalizeBuildVersion(buildVersion)).Set(1)

	return &serverMetrics{
		handler:  promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		requests: requests,
	}
}

func normalizeBuildVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "dev"
	}
	return version
}

func (m *serverMetrics) wrap(name string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w}
		next(recorder, r)
		m.requests.WithLabelValues(name, r.Method, strconv.Itoa(recorder.statusCode())).Inc()
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(p []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(p)
}

func (r *statusRecorder) statusCode() int {
	if r.status == 0 {
		return http.StatusOK
	}
	return r.status
}
