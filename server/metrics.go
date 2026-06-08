package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

type serverMetrics struct {
	requestsTotal    atomic.Uint64
	requestsInFlight atomic.Int64
}

func (m *serverMetrics) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		m.requestsTotal.Add(1)
		m.requestsInFlight.Add(1)
		defer m.requestsInFlight.Add(-1)

		next.ServeHTTP(w, r)
	})
}

func (m *serverMetrics) handle(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, `# HELP vekil_http_requests_total Total HTTP requests handled by the server.
# TYPE vekil_http_requests_total counter
vekil_http_requests_total %d
# HELP vekil_http_requests_in_flight Current in-flight HTTP requests.
# TYPE vekil_http_requests_in_flight gauge
vekil_http_requests_in_flight %d
`, m.requestsTotal.Load(), m.requestsInFlight.Load())
}
