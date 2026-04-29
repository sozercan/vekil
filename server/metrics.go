package server

import (
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
)

type serverMetrics struct {
	buildVersion string

	mu       sync.Mutex
	requests map[requestMetricKey]uint64
}

type requestMetricKey struct {
	method string
	route  string
	code   string
}

func newServerMetrics(buildVersion string) *serverMetrics {
	if strings.TrimSpace(buildVersion) == "" {
		buildVersion = "dev"
	}

	return &serverMetrics{
		buildVersion: buildVersion,
		requests:     make(map[requestMetricKey]uint64),
	}
}

func (m *serverMetrics) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

		var memStats runtime.MemStats
		runtime.ReadMemStats(&memStats)

		requests := m.snapshotRequests()

		_, _ = fmt.Fprintln(w, "# HELP vekil_build_info Build information for the running Vekil binary.")
		_, _ = fmt.Fprintln(w, "# TYPE vekil_build_info gauge")
		_, _ = fmt.Fprintf(w, "vekil_build_info{version=\"%s\"} 1\n", escapePrometheusLabelValue(m.buildVersion))
		_, _ = fmt.Fprintln(w, "# HELP go_goroutines Number of goroutines that currently exist.")
		_, _ = fmt.Fprintln(w, "# TYPE go_goroutines gauge")
		_, _ = fmt.Fprintf(w, "go_goroutines %d\n", runtime.NumGoroutine())
		_, _ = fmt.Fprintln(w, "# HELP go_memstats_alloc_bytes Number of bytes allocated and still in use.")
		_, _ = fmt.Fprintln(w, "# TYPE go_memstats_alloc_bytes gauge")
		_, _ = fmt.Fprintf(w, "go_memstats_alloc_bytes %d\n", memStats.Alloc)
		_, _ = fmt.Fprintln(w, "# HELP vekil_http_requests_total Total HTTP requests handled by Vekil, labeled by method, route pattern, and status code.")
		_, _ = fmt.Fprintln(w, "# TYPE vekil_http_requests_total counter")
		for _, sample := range requests {
			_, _ = fmt.Fprintf(
				w,
				"vekil_http_requests_total{method=\"%s\",route=\"%s\",code=\"%s\"} %d\n",
				escapePrometheusLabelValue(sample.method),
				escapePrometheusLabelValue(sample.route),
				escapePrometheusLabelValue(sample.code),
				sample.count,
			)
		}
	})
}

func (m *serverMetrics) wrap(route string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		recorder := &metricsResponseWriter{
			ResponseWriter: w,
			statusCode:     http.StatusOK,
		}

		next.ServeHTTP(recorder, r)
		m.recordRequest(r.Method, route, recorder.statusCode)
	})
}

type requestMetricSample struct {
	method string
	route  string
	code   string
	count  uint64
}

func (m *serverMetrics) recordRequest(method, route string, statusCode int) {
	m.mu.Lock()
	m.requests[requestMetricKey{
		method: method,
		route:  route,
		code:   strconv.Itoa(statusCode),
	}]++
	m.mu.Unlock()
}

func (m *serverMetrics) snapshotRequests() []requestMetricSample {
	m.mu.Lock()
	defer m.mu.Unlock()

	requests := make([]requestMetricSample, 0, len(m.requests))
	for key, count := range m.requests {
		requests = append(requests, requestMetricSample{
			method: key.method,
			route:  key.route,
			code:   key.code,
			count:  count,
		})
	}

	sort.Slice(requests, func(i, j int) bool {
		if requests[i].method != requests[j].method {
			return requests[i].method < requests[j].method
		}
		if requests[i].route != requests[j].route {
			return requests[i].route < requests[j].route
		}
		return requests[i].code < requests[j].code
	})

	return requests
}

func escapePrometheusLabelValue(value string) string {
	return strings.NewReplacer(
		"\\", "\\\\",
		"\n", "\\n",
		"\"", "\\\"",
	).Replace(value)
}

type metricsResponseWriter struct {
	http.ResponseWriter
	statusCode int
}

func (w *metricsResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.ResponseWriter.WriteHeader(statusCode)
}
