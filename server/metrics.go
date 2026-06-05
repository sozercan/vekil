package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

const metricsContentType = "text/plain; version=0.0.4; charset=utf-8"

type httpMetrics struct {
	requestsTotal atomic.Uint64
	inFlight      atomic.Int64
}

func (m *httpMetrics) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.requestsTotal.Add(1)
		m.inFlight.Add(1)
		defer m.inFlight.Add(-1)

		next.ServeHTTP(w, r)
	})
}

func (m *httpMetrics) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", metricsContentType)
		_, _ = fmt.Fprintf(w, `# HELP vekil_http_requests_total Total HTTP requests handled by the Vekil server.
# TYPE vekil_http_requests_total counter
vekil_http_requests_total %d
# HELP vekil_http_in_flight_requests Current in-flight HTTP requests handled by the Vekil server.
# TYPE vekil_http_in_flight_requests gauge
vekil_http_in_flight_requests %d
`, m.requestsTotal.Load(), m.inFlight.Load())
	})
}
