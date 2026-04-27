package server

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"strconv"

	"github.com/prometheus/client_golang/prometheus"
	collectors "github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type metrics struct {
	registry     *prometheus.Registry
	buildInfo    *prometheus.GaugeVec
	requestCount *prometheus.CounterVec
}

func newMetrics(version string) (*metrics, error) {
	registry := prometheus.NewRegistry()
	if err := registry.Register(collectors.NewGoCollector()); err != nil {
		return nil, err
	}
	if err := registry.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
		return nil, err
	}

	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vekil_build_info",
		Help: "Build metadata for the running Vekil binary.",
	}, []string{"version"})
	requestCount := prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vekil_http_requests_total",
		Help: "Total number of HTTP requests handled by Vekil's public endpoints.",
	}, []string{"method", "route", "code"})

	if err := registry.Register(buildInfo); err != nil {
		return nil, err
	}
	if err := registry.Register(requestCount); err != nil {
		return nil, err
	}

	buildInfo.WithLabelValues(version).Set(1)

	return &metrics{
		registry:     registry,
		buildInfo:    buildInfo,
		requestCount: requestCount,
	}, nil
}

func (m *metrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *metrics) instrument(method, route string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recorder := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next(recorder, r)
		m.requestCount.WithLabelValues(method, route, strconv.Itoa(recorder.status)).Inc()
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

func (r *statusRecorder) Flush() {
	if flusher, ok := r.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (r *statusRecorder) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := r.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func (r *statusRecorder) Push(target string, opts *http.PushOptions) error {
	if pusher, ok := r.ResponseWriter.(http.Pusher); ok {
		return pusher.Push(target, opts)
	}
	return http.ErrNotSupported
}

func (r *statusRecorder) Unwrap() http.ResponseWriter {
	return r.ResponseWriter
}
