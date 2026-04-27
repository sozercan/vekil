package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type proxyMetrics struct {
	handler             http.Handler
	requestsTotal       *prometheus.CounterVec
	requestDuration     *prometheus.HistogramVec
	tokensTotal         *prometheus.CounterVec
	retriesTotal        *prometheus.CounterVec
	upstreamErrorsTotal *prometheus.CounterVec
	inflightRequests    *prometheus.GaugeVec
}

func newProxyMetrics(buildVersion string) *proxyMetrics {
	registry := prometheus.NewRegistry()

	requestsTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_requests_total",
			Help: "Total proxied requests by endpoint, provider, model, and status.",
		},
		[]string{"endpoint", "provider", "public_model", "status", "code"},
	)
	requestDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vekil_request_duration_seconds",
			Help:    "Proxy request duration by endpoint, provider, model, and status.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"endpoint", "provider", "public_model", "status", "code"},
	)
	tokensTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_tokens_total",
			Help: "Token usage reported by upstream responses.",
		},
		[]string{"endpoint", "provider", "public_model", "direction"},
	)
	retriesTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_retries_total",
			Help: "Retry attempts against upstream providers.",
		},
		[]string{"endpoint", "provider", "public_model", "reason", "code"},
	)
	upstreamErrorsTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_upstream_errors_total",
			Help: "Upstream transport and HTTP errors observed by the proxy.",
		},
		[]string{"endpoint", "provider", "public_model", "reason", "code"},
	)
	inflightRequests := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vekil_inflight_requests",
			Help: "Current in-flight proxy requests.",
		},
		[]string{"endpoint"},
	)
	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vekil_build_info",
			Help: "Build metadata for the running vekil binary.",
		},
		[]string{"version"},
	)
	buildInfo.WithLabelValues(sanitizeMetricLabel(buildVersion, "dev")).Set(1)

	registry.MustRegister(
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
		requestsTotal,
		requestDuration,
		tokensTotal,
		retriesTotal,
		upstreamErrorsTotal,
		inflightRequests,
		buildInfo,
	)

	return &proxyMetrics{
		handler:             promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		requestsTotal:       requestsTotal,
		requestDuration:     requestDuration,
		tokensTotal:         tokensTotal,
		retriesTotal:        retriesTotal,
		upstreamErrorsTotal: upstreamErrorsTotal,
		inflightRequests:    inflightRequests,
	}
}

func (m *proxyMetrics) beginRequest(endpoint string) *requestMetricsScope {
	if m == nil {
		return nil
	}
	normalizedEndpoint := sanitizeMetricLabel(endpoint, "unknown")
	scope := &requestMetricsScope{
		metrics:          m,
		startedAt:        time.Now(),
		endpoint:         normalizedEndpoint,
		inflightEndpoint: normalizedEndpoint,
		provider:         "unknown",
		model:            "unknown",
	}
	m.inflightRequests.WithLabelValues(normalizedEndpoint).Inc()
	return scope
}

type requestMetricsScope struct {
	metrics   *proxyMetrics
	startedAt time.Time

	mu              sync.Mutex
	endpoint        string
	inflightEndpoint string
	provider        string
	model           string
	finalized       bool
}

type requestMetricsScopeKey struct{}

func withRequestMetricsScope(ctx context.Context, scope *requestMetricsScope) context.Context {
	if scope == nil {
		return ctx
	}
	return context.WithValue(ctx, requestMetricsScopeKey{}, scope)
}

func requestMetricsFromContext(ctx context.Context) *requestMetricsScope {
	if ctx == nil {
		return nil
	}
	scope, _ := ctx.Value(requestMetricsScopeKey{}).(*requestMetricsScope)
	return scope
}

func (s *requestMetricsScope) SetEndpoint(endpoint string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	normalized := sanitizeMetricLabel(endpoint, "unknown")
	if s.metrics != nil && !s.finalized && s.inflightEndpoint != normalized {
		s.metrics.inflightRequests.WithLabelValues(s.inflightEndpoint).Dec()
		s.metrics.inflightRequests.WithLabelValues(normalized).Inc()
		s.inflightEndpoint = normalized
	}
	s.endpoint = normalized
}

func (s *requestMetricsScope) SetPublicModel(model string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.model = sanitizeMetricLabel(model, "unknown")
}

func (s *requestMetricsScope) SetProvider(provider string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.provider = sanitizeMetricLabel(provider, "unknown")
}

func (s *requestMetricsScope) observeTokens(direction string, count int) {
	if s == nil || s.metrics == nil || count <= 0 {
		return
	}
	endpoint, provider, model := s.snapshot()
	s.metrics.tokensTotal.WithLabelValues(endpoint, provider, model, sanitizeMetricLabel(direction, "unknown")).Add(float64(count))
}

func (s *requestMetricsScope) observeOpenAIUsage(usage interface {
	GetPromptTokens() int
	GetCompletionTokens() int
}) {
	if s == nil || usage == nil {
		return
	}
	s.observeTokens("input", usage.GetPromptTokens())
	s.observeTokens("output", usage.GetCompletionTokens())
}

func (s *requestMetricsScope) observeResponsesUsage(inputTokens, outputTokens int) {
	s.observeTokens("input", inputTokens)
	s.observeTokens("output", outputTokens)
}

func (s *requestMetricsScope) observeRetry(reason, code string) {
	if s == nil || s.metrics == nil {
		return
	}
	endpoint, provider, model := s.snapshot()
	s.metrics.retriesTotal.WithLabelValues(
		endpoint,
		provider,
		model,
		sanitizeMetricLabel(reason, "unknown"),
		sanitizeMetricLabel(code, "unknown"),
	).Inc()
}

func (s *requestMetricsScope) observeUpstreamError(reason, code string) {
	if s == nil || s.metrics == nil {
		return
	}
	endpoint, provider, model := s.snapshot()
	s.metrics.upstreamErrorsTotal.WithLabelValues(
		endpoint,
		provider,
		model,
		sanitizeMetricLabel(reason, "unknown"),
		sanitizeMetricLabel(code, "unknown"),
	).Inc()
}

func (s *requestMetricsScope) finish(statusCode int) {
	if s == nil || s.metrics == nil {
		return
	}

	s.mu.Lock()
	if s.finalized {
		s.mu.Unlock()
		return
	}
	s.finalized = true
	endpoint := s.endpoint
	inflightEndpoint := s.inflightEndpoint
	provider := s.provider
	model := s.model
	s.mu.Unlock()

	s.metrics.inflightRequests.WithLabelValues(inflightEndpoint).Dec()

	status := "error"
	if statusCode >= 200 && statusCode < 400 {
		status = "success"
	}
	code := strconv.Itoa(statusCode)
	labels := []string{endpoint, provider, model, status, code}
	s.metrics.requestsTotal.WithLabelValues(labels...).Inc()
	s.metrics.requestDuration.WithLabelValues(labels...).Observe(time.Since(s.startedAt).Seconds())
}

func (s *requestMetricsScope) snapshot() (endpoint, provider, model string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.endpoint, s.provider, s.model
}

func sanitizeMetricLabel(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func classifyUpstreamMetricsReason(err error) string {
	if err == nil {
		return "unknown"
	}
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) {
		return "status_code"
	}
	return "transport"
}

func classifyUpstreamMetricsCode(err error) string {
	if err == nil {
		return "unknown"
	}
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) {
		return strconv.Itoa(upstreamErr.statusCode)
	}
	return "transport"
}

type openAIUsageAdapter struct {
	promptTokens     int
	completionTokens int
}

func (u openAIUsageAdapter) GetPromptTokens() int     { return u.promptTokens }
func (u openAIUsageAdapter) GetCompletionTokens() int { return u.completionTokens }

type metricsResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func newMetricsResponseWriter(w http.ResponseWriter) *metricsResponseWriter {
	return &metricsResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
}

func (w *metricsResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *metricsResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *metricsResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if rf, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return rf.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

func (w *metricsResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (w *metricsResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
	}
	return hijacker.Hijack()
}

func (w *metricsResponseWriter) Push(target string, opts *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}
