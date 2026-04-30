package server

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type httpMetrics struct {
	handler  http.Handler
	requests *prometheus.CounterVec
}

func newHTTPMetrics(buildVersion string) *httpMetrics {
	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vekil_build_info",
			Help: "Build information for the running Vekil binary.",
		},
		[]string{"version"},
	)
	buildInfo.WithLabelValues(buildVersion).Set(1)
	registry.MustRegister(buildInfo)

	requests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_http_requests_total",
			Help: "Count of HTTP requests handled by Vekil, labeled only by bounded route and status class.",
		},
		[]string{"handler", "status_class"},
	)
	registry.MustRegister(requests)

	return &httpMetrics{
		handler:  promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		requests: requests,
	}
}

func (m *httpMetrics) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	m.handler.ServeHTTP(w, r)
}

func (m *httpMetrics) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := newStatusCapturingResponseWriter(w)
		next.ServeHTTP(recorder, r)
		m.observe(r.Pattern, recorder.statusCode())
	})
}

func (m *httpMetrics) observe(pattern string, statusCode int) {
	if pattern == "" {
		pattern = "unmatched"
	}
	m.requests.WithLabelValues(pattern, statusClass(statusCode)).Inc()
}

func statusClass(statusCode int) string {
	if statusCode < 100 || statusCode > 999 {
		return "unknown"
	}
	return strconv.Itoa(statusCode/100) + "xx"
}

type statusCapturingResponseWriter struct {
	http.ResponseWriter
	status int
}

func newStatusCapturingResponseWriter(w http.ResponseWriter) *statusCapturingResponseWriter {
	return &statusCapturingResponseWriter{ResponseWriter: w}
}

func (w *statusCapturingResponseWriter) Header() http.Header {
	return w.ResponseWriter.Header()
}

func (w *statusCapturingResponseWriter) WriteHeader(statusCode int) {
	w.status = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *statusCapturingResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusCapturingResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		flusher.Flush()
	}
}

func (w *statusCapturingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func (w *statusCapturingResponseWriter) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := w.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

func (w *statusCapturingResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
