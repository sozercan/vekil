package server

import (
	"bufio"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type metricsConfig struct {
	enabled      bool
	buildVersion string
}

type requestMetricKey struct {
	handler string
	code    string
	method  string
}

type serverMetrics struct {
	buildVersion  string
	mu            sync.RWMutex
	requestsTotal map[requestMetricKey]uint64
}

func newServerMetrics(buildVersion string) (*serverMetrics, error) {
	return &serverMetrics{
		buildVersion:  normalizeBuildVersion(buildVersion),
		requestsTotal: make(map[requestMetricKey]uint64),
	}, nil
}

func (m *serverMetrics) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		bw := bufio.NewWriter(w)
		defer bw.Flush()

		fmt.Fprintln(bw, "# HELP go_goroutines Number of goroutines that currently exist.")
		fmt.Fprintln(bw, "# TYPE go_goroutines gauge")
		fmt.Fprintf(bw, "go_goroutines %d\n", runtime.NumGoroutine())

		fmt.Fprintln(bw, "# HELP vekil_build_info Build information for the running Vekil binary.")
		fmt.Fprintln(bw, "# TYPE vekil_build_info gauge")
		fmt.Fprintf(bw, "vekil_build_info{version=\"%s\"} 1\n", escapePrometheusLabelValue(m.buildVersion))

		fmt.Fprintln(bw, "# HELP vekil_http_requests_total Total number of HTTP requests handled by Vekil.")
		fmt.Fprintln(bw, "# TYPE vekil_http_requests_total counter")
		for _, sample := range m.requestSamples() {
			fmt.Fprintf(
				bw,
				"vekil_http_requests_total{handler=\"%s\",code=\"%s\",method=\"%s\"} %d\n",
				escapePrometheusLabelValue(sample.handler),
				escapePrometheusLabelValue(sample.code),
				escapePrometheusLabelValue(sample.method),
				sample.total,
			)
		}
	})
}

func (m *serverMetrics) instrument(pattern string, h http.Handler) http.Handler {
	if m == nil {
		return h
	}

	handlerLabel := routeLabel(pattern)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		iw := &instrumentedResponseWriter{ResponseWriter: w, status: http.StatusOK}
		h.ServeHTTP(iw, r)
		m.observeRequest(handlerLabel, r.Method, iw.status)
	})
}

func (m *serverMetrics) observeRequest(handler, method string, statusCode int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.requestsTotal[requestMetricKey{
		handler: handler,
		code:    strconv.Itoa(statusCode),
		method:  method,
	}]++
}

type requestMetricSample struct {
	handler string
	code    string
	method  string
	total   uint64
}

func (m *serverMetrics) requestSamples() []requestMetricSample {
	m.mu.RLock()
	defer m.mu.RUnlock()

	samples := make([]requestMetricSample, 0, len(m.requestsTotal))
	for key, total := range m.requestsTotal {
		samples = append(samples, requestMetricSample{
			handler: key.handler,
			code:    key.code,
			method:  key.method,
			total:   total,
		})
	}

	sort.Slice(samples, func(i, j int) bool {
		if samples[i].handler != samples[j].handler {
			return samples[i].handler < samples[j].handler
		}
		if samples[i].method != samples[j].method {
			return samples[i].method < samples[j].method
		}
		return samples[i].code < samples[j].code
	})

	return samples
}

type instrumentedResponseWriter struct {
	http.ResponseWriter
	status int
}

func (w *instrumentedResponseWriter) WriteHeader(statusCode int) {
	w.status = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *instrumentedResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *instrumentedResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *instrumentedResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	if w.status == 0 || w.status == http.StatusOK {
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

func (w *instrumentedResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		if w.status == 0 {
			w.status = http.StatusOK
		}
		return readerFrom.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

func normalizeBuildVersion(version string) string {
	version = strings.TrimSpace(version)
	if version == "" {
		return "dev"
	}
	return version
}

func routeLabel(pattern string) string {
	if idx := strings.IndexByte(pattern, ' '); idx >= 0 {
		return pattern[idx+1:]
	}
	return pattern
}

func escapePrometheusLabelValue(value string) string {
	var b strings.Builder
	for _, r := range value {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '"':
			b.WriteString(`\"`)
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}
