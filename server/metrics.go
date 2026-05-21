package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

type serverMetrics struct {
	requestsTotal atomic.Int64
	inFlight      atomic.Int64
}

func (m *serverMetrics) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.requestsTotal.Add(1)
		m.inFlight.Add(1)
		defer m.inFlight.Add(-1)
		next.ServeHTTP(w, r)
	})
}

func (m *serverMetrics) serveHTTP(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "# HELP vekil_http_requests_total Total HTTP requests handled by Vekil.\n")
	_, _ = fmt.Fprintf(w, "# TYPE vekil_http_requests_total counter\n")
	_, _ = fmt.Fprintf(w, "vekil_http_requests_total %d\n", m.requestsTotal.Load())
	_, _ = fmt.Fprintf(w, "# HELP vekil_http_in_flight_requests Current in-flight HTTP requests handled by Vekil.\n")
	_, _ = fmt.Fprintf(w, "# TYPE vekil_http_in_flight_requests gauge\n")
	_, _ = fmt.Fprintf(w, "vekil_http_in_flight_requests %d\n", m.inFlight.Load())
}
