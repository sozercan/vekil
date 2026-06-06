package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

type httpMetrics struct {
	requestsTotal atomic.Uint64
	inFlight      atomic.Int64
}

func (m *httpMetrics) instrument(next http.Handler) http.Handler {
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

func (m *httpMetrics) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w,
		"# HELP vekil_http_requests_total Total HTTP requests handled by the server.\n"+
			"# TYPE vekil_http_requests_total counter\n"+
			"vekil_http_requests_total %d\n"+
			"# HELP vekil_http_in_flight_requests Current in-flight HTTP requests.\n"+
			"# TYPE vekil_http_in_flight_requests gauge\n"+
			"vekil_http_in_flight_requests %d\n",
		m.requestsTotal.Load(),
		m.inFlight.Load(),
	)
}
