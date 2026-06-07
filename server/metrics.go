package server

import (
	"net/http"
	"strconv"
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

func (m *metrics) handle(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte("# HELP vekil_http_requests_total Total HTTP requests handled by Vekil.\n"))
	_, _ = w.Write([]byte("# TYPE vekil_http_requests_total counter\n"))
	_, _ = w.Write([]byte("vekil_http_requests_total " + strconv.FormatUint(atomic.LoadUint64(&m.requestsTotal), 10) + "\n"))
	_, _ = w.Write([]byte("# HELP vekil_http_in_flight_requests HTTP requests currently being handled by Vekil.\n"))
	_, _ = w.Write([]byte("# TYPE vekil_http_in_flight_requests gauge\n"))
	_, _ = w.Write([]byte("vekil_http_in_flight_requests " + strconv.FormatInt(atomic.LoadInt64(&m.inFlight), 10) + "\n"))
}
