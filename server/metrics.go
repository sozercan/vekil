package server

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

type metrics struct {
	requestsTotal uint64
	inFlight      int64
}

func (m *metrics) instrument(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddUint64(&m.requestsTotal, 1)
		atomic.AddInt64(&m.inFlight, 1)
		defer atomic.AddInt64(&m.inFlight, -1)

		next.ServeHTTP(w, r)
	})
}

func (m *metrics) handle(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "# HELP vekil_http_requests_total Total HTTP requests served by Vekil.\n")
	_, _ = fmt.Fprintf(w, "# TYPE vekil_http_requests_total counter\n")
	_, _ = fmt.Fprintf(w, "vekil_http_requests_total %d\n", atomic.LoadUint64(&m.requestsTotal))
	_, _ = fmt.Fprintf(w, "# HELP vekil_http_in_flight_requests Current in-flight HTTP requests served by Vekil.\n")
	_, _ = fmt.Fprintf(w, "# TYPE vekil_http_in_flight_requests gauge\n")
	_, _ = fmt.Fprintf(w, "vekil_http_in_flight_requests %d\n", atomic.LoadInt64(&m.inFlight))
}
