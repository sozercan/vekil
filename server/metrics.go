package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type serverMetrics struct {
	metricsHandler http.Handler
	requests       *prometheus.CounterVec
}

type buildLabels struct {
	version   string
	revision  string
	goVersion string
}

func newServerMetrics() (*serverMetrics, error) {
	registry := prometheus.NewRegistry()

	if err := registry.Register(collectors.NewGoCollector()); err != nil {
		return nil, fmt.Errorf("register go collector: %w", err)
	}

	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Namespace: "vekil",
			Name:      "build_info",
			Help:      "Build information about the running Vekil binary.",
		},
		[]string{"version", "revision", "goversion"},
	)
	if err := registry.Register(buildInfo); err != nil {
		return nil, fmt.Errorf("register build info metric: %w", err)
	}

	requests := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Namespace: "vekil",
			Subsystem: "http",
			Name:      "requests_total",
			Help:      "HTTP requests handled by the Vekil server.",
		},
		[]string{"handler", "method", "code"},
	)
	if err := registry.Register(requests); err != nil {
		return nil, fmt.Errorf("register request counter: %w", err)
	}

	labels := currentBuildLabels()
	buildInfo.WithLabelValues(labels.version, labels.revision, labels.goVersion).Set(1)

	return &serverMetrics{
		metricsHandler: promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		requests:       requests,
	}, nil
}

func (m *serverMetrics) handler() http.Handler {
	return m.metricsHandler
}

func (m *serverMetrics) instrument(name string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		recorder := &instrumentedResponseWriter{ResponseWriter: w}
		next(recorder, r)

		m.requests.WithLabelValues(
			name,
			r.Method,
			strconv.Itoa(recorder.statusCode()),
		).Inc()
	}
}

func currentBuildLabels() buildLabels {
	labels := buildLabels{
		version:   "dev",
		revision:  "unknown",
		goVersion: runtime.Version(),
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return labels
	}

	if version := strings.TrimSpace(info.Main.Version); version != "" && version != "(devel)" {
		labels.version = version
	}
	if goVersion := strings.TrimSpace(info.GoVersion); goVersion != "" {
		labels.goVersion = goVersion
	}
	for _, setting := range info.Settings {
		if setting.Key != "vcs.revision" {
			continue
		}
		if revision := strings.TrimSpace(setting.Value); revision != "" {
			labels.revision = revision
		}
		break
	}

	return labels
}

type instrumentedResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *instrumentedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
	w.ResponseWriter.WriteHeader(status)
}

func (w *instrumentedResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *instrumentedResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

func (w *instrumentedResponseWriter) Flush() {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *instrumentedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	if w.status == 0 {
		w.status = http.StatusSwitchingProtocols
	}
	return hijacker.Hijack()
}

func (w *instrumentedResponseWriter) Push(target string, opts *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}

func (w *instrumentedResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *instrumentedResponseWriter) statusCode() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}
