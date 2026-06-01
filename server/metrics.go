package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

type metrics struct {
	requestsTotal   atomic.Uint64
	inflightRequest atomic.Int64
}

func newMetrics() *metrics {
	return &metrics{}
}

func (m *metrics) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		m.requestsTotal.Add(1)
		m.inflightRequest.Add(1)
		defer m.inflightRequest.Add(-1)
		next.ServeHTTP(w, r)
	})
}

func (m *metrics) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")

	_, _ = fmt.Fprintf(w, "# TYPE vekil_http_requests_total counter\nvekil_http_requests_total %d\n", m.requestsTotal.Load())
	_, _ = fmt.Fprintf(w, "# TYPE vekil_http_inflight_requests gauge\nvekil_http_inflight_requests %d\n", m.inflightRequest.Load())
}
