package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	"github.com/sozercan/vekil/buildinfo"
	"github.com/sozercan/vekil/models"
)

const (
	unknownMetricLabel    = "unknown"
	metricsUsageBodyLimit = 8 << 20
)

type requestMetricsContextKey struct{}

type metricsCollector struct {
	registry               *prometheus.Registry
	handler                http.Handler
	requestsTotal          *prometheus.CounterVec
	requestDuration        *prometheus.HistogramVec
	streamFirstByteLatency *prometheus.HistogramVec
	tokensTotal            *prometheus.CounterVec
	retriesTotal           *prometheus.CounterVec
	upstreamErrorsTotal    *prometheus.CounterVec
	inflightRequests       *prometheus.GaugeVec
	endpointHealthy        *prometheus.GaugeVec
	buildInfo              *prometheus.GaugeVec
}

type requestMetricsObserver struct {
	metrics     *metricsCollector
	provider    string
	publicModel string
	endpoint    string
	streaming   bool
	startedAt   time.Time
	inflight    bool
	finishOnce  sync.Once
	firstByte   sync.Once
}

type tokenUsage struct {
	promptTokens     int
	completionTokens int
}

type metricsUsageFields struct {
	PromptTokens     *int `json:"prompt_tokens,omitempty"`
	CompletionTokens *int `json:"completion_tokens,omitempty"`
	InputTokens      *int `json:"input_tokens,omitempty"`
	OutputTokens     *int `json:"output_tokens,omitempty"`
}

func newMetricsCollector() (*metricsCollector, error) {
	m := &metricsCollector{
		registry: prometheus.NewRegistry(),
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vekil_requests_total",
				Help: "Total number of proxy requests by provider, model, endpoint, and final HTTP status.",
			},
			[]string{"provider", "public_model", "endpoint", "status"},
		),
		requestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "vekil_request_duration_seconds",
				Help: "End-to-end proxy request duration by provider, model, endpoint, and final HTTP status.",
			},
			[]string{"provider", "public_model", "endpoint", "status"},
		),
		streamFirstByteLatency: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name: "vekil_stream_first_byte_latency_seconds",
				Help: "Latency from request start until the first response byte is written for streaming endpoints.",
			},
			[]string{"provider", "public_model", "endpoint"},
		),
		tokensTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vekil_tokens_total",
				Help: "Upstream token usage parsed from response usage blocks.",
			},
			[]string{"provider", "public_model", "endpoint", "direction"},
		),
		retriesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vekil_retries_total",
				Help: "Total number of upstream retry attempts by provider, model, endpoint, and retry reason.",
			},
			[]string{"provider", "public_model", "endpoint", "reason"},
		),
		upstreamErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vekil_upstream_errors_total",
				Help: "Total number of upstream HTTP or transport errors by provider, model, endpoint, and code.",
			},
			[]string{"provider", "public_model", "endpoint", "code"},
		),
		inflightRequests: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "vekil_inflight_requests",
				Help: "Current number of in-flight proxy requests by provider.",
			},
			[]string{"provider"},
		),
		endpointHealthy: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "vekil_endpoint_healthy",
				Help: "Latest readiness result for each configured provider endpoint (1=healthy, 0=unhealthy).",
			},
			[]string{"provider", "endpoint"},
		),
		buildInfo: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "vekil_build_info",
				Help: "Static build metadata for this Vekil binary.",
			},
			[]string{"version", "go_version", "commit"},
		),
	}

	if err := m.registry.Register(collectors.NewGoCollector()); err != nil {
		return nil, err
	}
	if err := m.registry.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
		return nil, err
	}
	for _, collector := range []prometheus.Collector{
		m.requestsTotal,
		m.requestDuration,
		m.streamFirstByteLatency,
		m.tokensTotal,
		m.retriesTotal,
		m.upstreamErrorsTotal,
		m.inflightRequests,
		m.endpointHealthy,
		m.buildInfo,
	} {
		if err := m.registry.Register(collector); err != nil {
			return nil, err
		}
	}

	info := buildinfo.Current()
	m.buildInfo.WithLabelValues(info.Version, info.GoVersion, info.Commit).Set(1)
	m.handler = promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
	return m, nil
}

func (m *metricsCollector) startRequest(provider, publicModel, endpoint string, streaming bool) *requestMetricsObserver {
	if m == nil {
		return nil
	}

	observer := &requestMetricsObserver{
		metrics:     m,
		provider:    normalizeMetricLabel(provider),
		publicModel: normalizeMetricLabel(publicModel),
		endpoint:    normalizeMetricLabel(endpoint),
		streaming:   streaming,
		startedAt:   time.Now(),
		inflight:    true,
	}
	m.inflightRequests.WithLabelValues(observer.provider).Inc()
	return observer
}

func (m *metricsCollector) setEndpointHealthy(providerID, endpoint string, healthy bool) {
	if m == nil {
		return
	}
	value := 0.0
	if healthy {
		value = 1
	}
	m.endpointHealthy.WithLabelValues(normalizeMetricLabel(providerID), normalizeMetricLabel(endpoint)).Set(value)
}

func normalizeMetricLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return unknownMetricLabel
	}
	return value
}

func (o *requestMetricsObserver) observeFirstByte() {
	if o == nil || o.metrics == nil || !o.streaming {
		return
	}
	o.firstByte.Do(func() {
		o.metrics.streamFirstByteLatency.WithLabelValues(o.provider, o.publicModel, o.endpoint).Observe(time.Since(o.startedAt).Seconds())
	})
}

func (o *requestMetricsObserver) observeOpenAIUsage(usage *models.OpenAIUsage) {
	if usage == nil {
		return
	}
	o.observeUsage(&tokenUsage{
		promptTokens:     usage.PromptTokens,
		completionTokens: usage.CompletionTokens,
	})
}

func (o *requestMetricsObserver) observeUsageFromBody(body []byte) {
	o.observeUsage(extractUsageFromJSONBody(body))
}

func (o *requestMetricsObserver) observeUsage(usage *tokenUsage) {
	if o == nil || o.metrics == nil || usage == nil {
		return
	}
	o.metrics.tokensTotal.WithLabelValues(o.provider, o.publicModel, o.endpoint, "prompt").Add(float64(usage.promptTokens))
	o.metrics.tokensTotal.WithLabelValues(o.provider, o.publicModel, o.endpoint, "completion").Add(float64(usage.completionTokens))
}

func (o *requestMetricsObserver) observeRetry(reason string) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.retriesTotal.WithLabelValues(o.provider, o.publicModel, o.endpoint, normalizeMetricLabel(reason)).Inc()
}

func (o *requestMetricsObserver) observeUpstreamError(code string) {
	if o == nil || o.metrics == nil {
		return
	}
	o.metrics.upstreamErrorsTotal.WithLabelValues(o.provider, o.publicModel, o.endpoint, normalizeMetricLabel(code)).Inc()
}

func (o *requestMetricsObserver) finish(status int) {
	if o == nil || o.metrics == nil {
		return
	}
	o.finishOnce.Do(func() {
		statusLabel := strconv.Itoa(status)
		if status <= 0 {
			statusLabel = strconv.Itoa(http.StatusOK)
		}
		if o.inflight {
			o.metrics.inflightRequests.WithLabelValues(o.provider).Dec()
			o.inflight = false
		}
		o.metrics.requestsTotal.WithLabelValues(o.provider, o.publicModel, o.endpoint, statusLabel).Inc()
		o.metrics.requestDuration.WithLabelValues(o.provider, o.publicModel, o.endpoint, statusLabel).Observe(time.Since(o.startedAt).Seconds())
	})
}

func withRequestMetricsObserver(ctx context.Context, observer *requestMetricsObserver) context.Context {
	if ctx == nil || observer == nil {
		return ctx
	}
	return context.WithValue(ctx, requestMetricsContextKey{}, observer)
}

func requestMetricsObserverFromContext(ctx context.Context) *requestMetricsObserver {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(requestMetricsContextKey{}).(*requestMetricsObserver)
	return observer
}

func observeUsageFromBodyContext(ctx context.Context, body []byte) {
	if observer := requestMetricsObserverFromContext(ctx); observer != nil {
		observer.observeUsageFromBody(body)
	}
}

func observeOpenAIUsageContext(ctx context.Context, usage *models.OpenAIUsage) {
	if observer := requestMetricsObserverFromContext(ctx); observer != nil {
		observer.observeOpenAIUsage(usage)
	}
}

func extractUsageFromJSONBody(body []byte) *tokenUsage {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil
	}

	var payload struct {
		Usage    *metricsUsageFields `json:"usage,omitempty"`
		Response *struct {
			Usage *metricsUsageFields `json:"usage,omitempty"`
		} `json:"response,omitempty"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	if usage := usageFromFields(payload.Usage); usage != nil {
		return usage
	}
	if payload.Response != nil {
		return usageFromFields(payload.Response.Usage)
	}
	return nil
}

func usageFromFields(fields *metricsUsageFields) *tokenUsage {
	if fields == nil {
		return nil
	}

	usage := &tokenUsage{}
	hasUsage := false
	if fields.PromptTokens != nil {
		usage.promptTokens = *fields.PromptTokens
		hasUsage = true
	} else if fields.InputTokens != nil {
		usage.promptTokens = *fields.InputTokens
		hasUsage = true
	}
	if fields.CompletionTokens != nil {
		usage.completionTokens = *fields.CompletionTokens
		hasUsage = true
	} else if fields.OutputTokens != nil {
		usage.completionTokens = *fields.OutputTokens
		hasUsage = true
	}
	if !hasUsage {
		return nil
	}
	return usage
}

func writeUpstreamResponseObserved(w http.ResponseWriter, resp *http.Response, observer *requestMetricsObserver) error {
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 200 && resp.StatusCode < 300 && observer != nil && len(body) <= metricsUsageBodyLimit {
		observer.observeUsageFromBody(body)
	}

	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	return nil
}

type metricsResponseWriter struct {
	http.ResponseWriter
	observer    *requestMetricsObserver
	statusCode  int
	wroteHeader bool
}

func newMetricsResponseWriter(w http.ResponseWriter, observer *requestMetricsObserver) *metricsResponseWriter {
	return &metricsResponseWriter{
		ResponseWriter: w,
		observer:       observer,
		statusCode:     http.StatusOK,
	}
}

func (w *metricsResponseWriter) WriteHeader(statusCode int) {
	if !w.wroteHeader {
		w.statusCode = statusCode
		w.wroteHeader = true
	}
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *metricsResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if len(p) > 0 && w.observer != nil {
		w.observer.observeFirstByte()
	}
	return w.ResponseWriter.Write(p)
}

func (w *metricsResponseWriter) StatusCode() int {
	if w == nil {
		return http.StatusOK
	}
	if !w.wroteHeader {
		return http.StatusOK
	}
	return w.statusCode
}

func (w *metricsResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *metricsResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("http hijacker not supported")
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

func (h *ProxyHandler) MetricsHandler() http.Handler {
	if h == nil || h.metrics == nil {
		return nil
	}
	return h.metrics.handler
}

func (h *ProxyHandler) initializeMetrics() error {
	if h == nil {
		return nil
	}
	if h.metrics != nil {
		h.seedEndpointHealthMetrics()
		return nil
	}
	collector, err := newMetricsCollector()
	if err != nil {
		return err
	}
	h.metrics = collector
	h.seedEndpointHealthMetrics()
	return nil
}

func (h *ProxyHandler) seedEndpointHealthMetrics() {
	if h == nil || h.metrics == nil {
		return
	}
	setup := h.providerSetup()
	for _, providerID := range setup.providerOrder {
		provider := setup.providerByID(providerID)
		if provider == nil {
			continue
		}
		h.metrics.setEndpointHealthy(provider.id, provider.baseURL, false)
	}
}

func (h *ProxyHandler) startRequestMetrics(publicEndpoint, upstreamEndpoint, publicModel string, streaming bool) *requestMetricsObserver {
	if h == nil || h.metrics == nil {
		return nil
	}
	return h.metrics.startRequest(h.resolveMetricsProviderID(publicModel, upstreamEndpoint), publicModel, publicEndpoint, streaming)
}

func (h *ProxyHandler) resolveMetricsProviderID(publicModel, upstreamEndpoint string) string {
	publicModel = strings.TrimSpace(publicModel)
	if publicModel == "" {
		return unknownMetricLabel
	}
	provider, _, _ := h.resolveProviderModel(publicModel, upstreamEndpoint)
	if provider == nil {
		return unknownMetricLabel
	}
	return provider.id
}
