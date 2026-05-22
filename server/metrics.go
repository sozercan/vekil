package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

type serverMetrics struct {
	requestsTotal atomic.Uint64
	inFlight      atomic.Int64
}

func newServerMetrics() *serverMetrics {
	return &serverMetrics{}
}

func (m *serverMetrics) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		m.requestsTotal.Add(1)
		m.inFlight.Add(1)
		defer m.inFlight.Add(-1)

		next.ServeHTTP(w, r)
	})
}

func (m *serverMetrics) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", metricsContentType)
	_, _ = fmt.Fprintf(w, "# HELP vekil_http_requests_total Total HTTP requests handled.\n")
	_, _ = fmt.Fprintf(w, "# TYPE vekil_http_requests_total counter\n")
	_, _ = fmt.Fprintf(w, "vekil_http_requests_total %d\n", m.requestsTotal.Load())
	_, _ = fmt.Fprintf(w, "# HELP vekil_http_in_flight_requests Current in-flight HTTP requests.\n")
	_, _ = fmt.Fprintf(w, "# TYPE vekil_http_in_flight_requests gauge\n")
	_, _ = fmt.Fprintf(w, "vekil_http_in_flight_requests %d\n", m.inFlight.Load())
}
