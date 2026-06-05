package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

const (
	requestsMetricName = "vekil_http_requests_total"
	inflightMetricName = "vekil_http_in_flight_requests"
)

type metrics struct {
	requests atomic.Uint64
	inflight atomic.Int64
}

func newMetrics() *metrics {
	return &metrics{}
}

func (m *metrics) instrument(next http.Handler) http.Handler {
	if m == nil {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		m.requests.Add(1)
		m.inflight.Add(1)
		defer m.inflight.Add(-1)

		next.ServeHTTP(w, r)
	})
}

func (m *metrics) handle(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	_, _ = fmt.Fprintf(w, "# HELP %s Total HTTP requests handled by Vekil.\n", requestsMetricName)
	_, _ = fmt.Fprintf(w, "# TYPE %s counter\n", requestsMetricName)
	_, _ = fmt.Fprintf(w, "%s %d\n", requestsMetricName, m.requests.Load())
	_, _ = fmt.Fprintf(w, "# HELP %s Current in-flight HTTP requests.\n", inflightMetricName)
	_, _ = fmt.Fprintf(w, "# TYPE %s gauge\n", inflightMetricName)
	_, _ = fmt.Fprintf(w, "%s %d\n", inflightMetricName, m.inflight.Load())
}
