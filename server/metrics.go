package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

const (
	requestsTotalMetricName    = "vekil_http_requests_total"
	requestsInflightMetricName = "vekil_http_inflight_requests"
)

type metrics struct {
	requestsTotal    atomic.Uint64
	requestsInflight atomic.Int64
}

func (m *metrics) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/metrics" {
			next.ServeHTTP(w, r)
			return
		}

		m.requestsInflight.Add(1)
		defer m.requestsInflight.Add(-1)
		m.requestsTotal.Add(1)

		next.ServeHTTP(w, r)
	})
}

func (m *metrics) handle(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w,
		"# HELP %s Total HTTP requests handled by Vekil.\n"+
			"# TYPE %s counter\n"+
			"%s %d\n"+
			"# HELP %s Current in-flight HTTP requests handled by Vekil.\n"+
			"# TYPE %s gauge\n"+
			"%s %d\n",
		requestsTotalMetricName,
		requestsTotalMetricName,
		requestsTotalMetricName,
		m.requestsTotal.Load(),
		requestsInflightMetricName,
		requestsInflightMetricName,
		requestsInflightMetricName,
		m.requestsInflight.Load(),
	)
}
