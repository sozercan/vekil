package server

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type serverMetrics struct {
	registry        *prometheus.Registry
	requestTotal    *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
}

func newServerMetrics(buildVersion string) *serverMetrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vekil_build_info",
		Help: "Build information for the running Vekil binary.",
	}, []string{"version"})
	buildInfo.WithLabelValues(normalizeBuildVersion(buildVersion)).Set(1)
	registry.MustRegister(buildInfo)

	requestTotal := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vekil_http_requests_total",
		Help: "Total number of HTTP requests handled by Vekil.",
	}, []string{"route", "method", "code"})
	requestDuration := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vekil_http_request_duration_seconds",
		Help:    "HTTP request duration in seconds for Vekil handlers.",
		Buckets: prometheus.DefBuckets,
	}, []string{"route", "method"})
	registry.MustRegister(requestTotal, requestDuration)

	return &serverMetrics{
		registry:        registry,
		requestTotal:    requestTotal,
		requestDuration: requestDuration,
	}
}

func normalizeBuildVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "dev"
	}
	return version
}

func (m *serverMetrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *serverMetrics) instrument(route string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusCapturingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
		next(recorder, r)
		m.requestTotal.WithLabelValues(route, r.Method, strconv.Itoa(recorder.statusCode)).Inc()
		m.requestDuration.WithLabelValues(route, r.Method).Observe(time.Since(start).Seconds())
	}
}

type statusCapturingResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *statusCapturingResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *statusCapturingResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusCapturingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (w *statusCapturingResponseWriter) Push(target string, opts *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}

func (w *statusCapturingResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}
