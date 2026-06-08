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

func (m *metrics) wrap(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddUint64(&m.requestsTotal, 1)
		atomic.AddInt64(&m.inFlight, 1)
		defer atomic.AddInt64(&m.inFlight, -1)

		next.ServeHTTP(w, r)
	})
}

func (m *metrics) handle(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = w.Write([]byte(
		"# TYPE vekil_http_requests_total counter\n" +
			"vekil_http_requests_total " + strconv.FormatUint(atomic.LoadUint64(&m.requestsTotal), 10) + "\n" +
			"# TYPE vekil_http_requests_in_flight gauge\n" +
			"vekil_http_requests_in_flight " + strconv.FormatInt(atomic.LoadInt64(&m.inFlight), 10) + "\n",
	))
}
