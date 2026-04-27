package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const (
	metricNameBuildInfo    = "vekil_build_info"
	metricNameHTTPRequests = "vekil_http_requests_total"
)

type metrics struct {
	registry     *prometheus.Registry
	httpRequests *prometheus.CounterVec
}

func newMetrics(buildVersion string) *metrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: metricNameBuildInfo,
		Help: "Build information for the running Vekil process.",
	}, []string{"version", "goversion"})
	buildInfo.WithLabelValues(normalizeBuildVersion(buildVersion), runtime.Version()).Set(1)
	registry.MustRegister(buildInfo)

	httpRequests := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: metricNameHTTPRequests,
		Help: "Total HTTP requests handled by Vekil.",
	}, []string{"route", "method", "status_class"})
	registry.MustRegister(httpRequests)

	return &metrics{
		registry:     registry,
		httpRequests: httpRequests,
	}
}

func normalizeBuildVersion(buildVersion string) string {
	buildVersion = strings.TrimSpace(buildVersion)
	if buildVersion == "" {
		return "dev"
	}
	return buildVersion
}

func (m *metrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *metrics) instrument(route, method string, next http.HandlerFunc) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		wrapped := &metricsResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next(wrapped, r)
		m.httpRequests.WithLabelValues(route, method, statusClass(wrapped.statusCode)).Inc()
	})
}

type metricsResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *metricsResponseWriter) Write(p []byte) (int, error) {
	if w.statusCode == 0 {
		w.statusCode = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *metricsResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *metricsResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *metricsResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func (w *metricsResponseWriter) Push(target string, opts *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}

func (w *metricsResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		if w.statusCode == 0 {
			w.statusCode = http.StatusOK
		}
		return readerFrom.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

func (w *metricsResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func statusClass(statusCode int) string {
	if statusCode < 100 || statusCode > 999 {
		return "unknown"
	}
	return string(rune('0'+statusCode/100)) + "xx"
}
