package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

type metrics struct {
	requestsTotal    atomic.Int64
	requestsInFlight atomic.Int64
}

func (m *metrics) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.requestsTotal.Add(1)
		m.requestsInFlight.Add(1)
		defer m.requestsInFlight.Add(-1)

		next.ServeHTTP(w, r)
	})
}

func (m *metrics) handleMetrics(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(
		w,
		"# HELP vekil_http_requests_total Total HTTP requests served.\n"+
			"# TYPE vekil_http_requests_total counter\n"+
			"vekil_http_requests_total %d\n"+
			"# HELP vekil_http_requests_in_flight Current in-flight HTTP requests.\n"+
			"# TYPE vekil_http_requests_in_flight gauge\n"+
			"vekil_http_requests_in_flight %d\n",
		m.requestsTotal.Load(),
		m.requestsInFlight.Load(),
	)
}
