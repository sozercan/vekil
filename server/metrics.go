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

type metrics struct {
	registry        *prometheus.Registry
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
}

func newMetrics(buildVersion string) *metrics {
	version := strings.TrimSpace(buildVersion)
	if version == "" {
		version = "dev"
	}

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
	buildInfo.WithLabelValues(version).Set(1)

	requestsTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_http_requests_total",
			Help: "HTTP requests handled by Vekil.",
		},
		[]string{"route", "method", "code"},
	)

	requestDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vekil_http_request_duration_seconds",
			Help:    "HTTP request duration for Vekil handlers.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"route", "method", "code"},
	)

	registry.MustRegister(buildInfo, requestsTotal, requestDuration)

	return &metrics{
		registry:        registry,
		requestsTotal:   requestsTotal,
		requestDuration: requestDuration,
	}
}

func (m *metrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *metrics) wrap(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(recorder, r)

		code := strconv.Itoa(recorder.status)
		labels := []string{route, r.Method, code}
		m.requestsTotal.WithLabelValues(labels...).Inc()
		m.requestDuration.WithLabelValues(labels...).Observe(time.Since(start).Seconds())
	})
}

type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (r *statusRecorder) ReadFrom(src io.Reader) (int64, error) {
	if readerFrom, ok := r.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(src)
	}
	return io.Copy(r.ResponseWriter, src)
}

func (r *statusRecorder) Push(target string, opts *http.PushOptions) error {
	pusher, ok := r.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}
