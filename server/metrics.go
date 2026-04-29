package server

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type httpMetrics struct {
	registry       *prometheus.Registry
	requestCounter *prometheus.CounterVec
}

func newHTTPMetrics() *httpMetrics {
	registry := prometheus.NewRegistry()
	requestCounter := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_http_requests_total",
			Help: "Total number of HTTP requests handled by Vekil, partitioned by route, method, and status code.",
		},
		[]string{"route", "method", "code"},
	)
	registry.MustRegister(requestCounter)

	return &httpMetrics{
		registry:       registry,
		requestCounter: requestCounter,
	}
}

func (m *httpMetrics) instrumentHandler(route string, next http.Handler) http.Handler {
	if m == nil {
		return next
	}
	return promhttp.InstrumentHandlerCounter(
		m.requestCounter.MustCurryWith(prometheus.Labels{"route": route}),
		next,
	)
}

func (m *httpMetrics) handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}
