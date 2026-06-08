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

func (m *metrics) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddUint64(&m.requestsTotal, 1)
		atomic.AddInt64(&m.inFlight, 1)
		defer atomic.AddInt64(&m.inFlight, -1)

		next.ServeHTTP(w, r)
	})
}

func (m *metrics) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(w, "# TYPE vekil_http_requests_total counter\n")
	_, _ = fmt.Fprintf(w, "vekil_http_requests_total %d\n", atomic.LoadUint64(&m.requestsTotal))
	_, _ = fmt.Fprintf(w, "# TYPE vekil_http_requests_in_flight gauge\n")
	_, _ = fmt.Fprintf(w, "vekil_http_requests_in_flight %d\n", atomic.LoadInt64(&m.inFlight))
}
