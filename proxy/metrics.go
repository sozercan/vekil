package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"runtime"
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
	metricLabelUnknown                   = "unknown"
	metricEndpointAnthropicMessages      = "messages"
	metricEndpointChatCompletions        = "chat_completions"
	metricEndpointResponses              = "responses"
	metricEndpointResponsesCompact       = "responses_compact"
	metricEndpointMemoryTraceSummarize   = "memories_trace_summarize"
	metricEndpointGeminiGenerateContent  = "gemini_generate_content"
	metricEndpointGeminiStreamContent    = "gemini_stream_generate_content"
	metricEndpointGeminiCountTokens      = "gemini_count_tokens"
	metricsExpectedFallbackBodySubstring = "max_completion_tokens"
)

type proxyMetrics struct {
	registry            *prometheus.Registry
	handler             http.Handler
	requestsTotal       *prometheus.CounterVec
	requestDuration     *prometheus.HistogramVec
	tokensTotal         *prometheus.CounterVec
	retriesTotal        *prometheus.CounterVec
	upstreamErrorsTotal *prometheus.CounterVec
	inflightRequests    *prometheus.GaugeVec
	endpointHealthy     *prometheus.GaugeVec
	buildInfo           *prometheus.GaugeVec
}

type requestMetricLabels struct {
	provider    string
	publicModel string
	endpoint    string
}

type requestUsageMetrics struct {
	promptTokens     int
	completionTokens int
	set              bool
}

type requestMetricsTracker struct {
	metrics *proxyMetrics
	labels  requestMetricLabels
	start   time.Time
	usage   requestUsageMetrics
	once    sync.Once
}

type upstreamMetricsBehavior struct {
	suppressExpectedMaxCompletionTokens400 bool
}

type upstreamMetricsContext struct {
	labels   requestMetricLabels
	behavior upstreamMetricsBehavior
}

type upstreamMetricsContextKey struct{}

type metricsResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

func newProxyMetrics() (*proxyMetrics, error) {
	registry := prometheus.NewRegistry()
	metrics := &proxyMetrics{
		registry: registry,
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_requests_total",
			Help: "Total proxy requests handled by Vekil.",
		}, []string{"provider", "public_model", "endpoint", "status"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vekil_request_duration_seconds",
			Help:    "End-to-end proxy request latency in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"provider", "public_model", "endpoint", "status"}),
		tokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_tokens_total",
			Help: "Upstream token usage observed by Vekil.",
		}, []string{"provider", "public_model", "endpoint", "direction"}),
		retriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_retries_total",
			Help: "Retried upstream attempts triggered by transient failures.",
		}, []string{"provider", "public_model", "endpoint", "reason"}),
		upstreamErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_upstream_errors_total",
			Help: "Observed upstream transport and HTTP errors.",
		}, []string{"provider", "public_model", "endpoint", "code"}),
		inflightRequests: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vekil_inflight_requests",
			Help: "Currently in-flight proxy requests by provider.",
		}, []string{"provider"}),
		endpointHealthy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vekil_endpoint_healthy",
			Help: "Provider endpoint health status when available (1=healthy, 0=unhealthy).",
		}, []string{"provider", "endpoint"}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vekil_build_info",
			Help: "Build information for the running Vekil binary.",
		}, []string{"version", "go_version", "commit"}),
	}

	for _, collector := range []prometheus.Collector{
		metrics.requestsTotal,
		metrics.requestDuration,
		metrics.tokensTotal,
		metrics.retriesTotal,
		metrics.upstreamErrorsTotal,
		metrics.inflightRequests,
		metrics.endpointHealthy,
		metrics.buildInfo,
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	} {
		if err := registry.Register(collector); err != nil {
			return nil, err
		}
	}

	metrics.buildInfo.WithLabelValues(buildinfo.NormalizedVersion(), runtime.Version(), buildinfo.NormalizedCommit()).Set(1)
	metrics.handler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	return metrics, nil
}

func normalizeMetricLabel(value string) string {
	if trimmed := strings.TrimSpace(value); trimmed != "" {
		return trimmed
	}
	return metricLabelUnknown
}

func normalizeRequestMetricLabels(labels requestMetricLabels) requestMetricLabels {
	return requestMetricLabels{
		provider:    normalizeMetricLabel(labels.provider),
		publicModel: normalizeMetricLabel(labels.publicModel),
		endpoint:    normalizeMetricLabel(labels.endpoint),
	}
}

func metricsEndpointForUpstreamPath(path string) string {
	switch strings.TrimSpace(path) {
	case "/chat/completions":
		return metricEndpointChatCompletions
	case "/responses":
		return metricEndpointResponses
	default:
		trimmed := strings.Trim(strings.ReplaceAll(path, "/", "_"), "_")
		return normalizeMetricLabel(trimmed)
	}
}

func (m *proxyMetrics) observeRequest(labels requestMetricLabels, status int, duration time.Duration, usage requestUsageMetrics) {
	if m == nil {
		return
	}
	labels = normalizeRequestMetricLabels(labels)
	statusLabel := strconv.Itoa(status)
	m.requestsTotal.WithLabelValues(labels.provider, labels.publicModel, labels.endpoint, statusLabel).Inc()
	m.requestDuration.WithLabelValues(labels.provider, labels.publicModel, labels.endpoint, statusLabel).Observe(duration.Seconds())
	if usage.set {
		m.tokensTotal.WithLabelValues(labels.provider, labels.publicModel, labels.endpoint, "prompt").Add(float64(usage.promptTokens))
		m.tokensTotal.WithLabelValues(labels.provider, labels.publicModel, labels.endpoint, "completion").Add(float64(usage.completionTokens))
	}
}

func (m *proxyMetrics) observeRetry(labels requestMetricLabels, reason string) {
	if m == nil {
		return
	}
	labels = normalizeRequestMetricLabels(labels)
	m.retriesTotal.WithLabelValues(labels.provider, labels.publicModel, labels.endpoint, normalizeMetricLabel(reason)).Inc()
}

func (m *proxyMetrics) observeUpstreamError(labels requestMetricLabels, code string) {
	if m == nil {
		return
	}
	labels = normalizeRequestMetricLabels(labels)
	m.upstreamErrorsTotal.WithLabelValues(labels.provider, labels.publicModel, labels.endpoint, normalizeMetricLabel(code)).Inc()
}

func (m *proxyMetrics) observeEndpointHealth(provider, endpoint string, healthy bool) {
	if m == nil {
		return
	}
	value := 0.0
	if healthy {
		value = 1
	}
	m.endpointHealthy.WithLabelValues(normalizeMetricLabel(provider), normalizeMetricLabel(endpoint)).Set(value)
}

func (h *ProxyHandler) HandleMetrics(w http.ResponseWriter, r *http.Request) {
	if h == nil || h.metrics == nil || h.metrics.handler == nil {
		http.NotFound(w, r)
		return
	}
	h.metrics.handler.ServeHTTP(w, r)
}

func (h *ProxyHandler) beginRequestMetrics(endpoint, upstreamPath, publicModel string) *requestMetricsTracker {
	if h == nil || h.metrics == nil {
		return nil
	}

	provider, owner, _ := h.resolveProviderModel(publicModel, upstreamPath)
	labels := requestMetricLabels{
		provider:    metricLabelUnknown,
		publicModel: publicModel,
		endpoint:    endpoint,
	}
	if provider != nil {
		labels.provider = provider.id
	}
	if strings.TrimSpace(owner.publicID) != "" {
		labels.publicModel = owner.publicID
	}
	labels = normalizeRequestMetricLabels(labels)

	h.metrics.inflightRequests.WithLabelValues(labels.provider).Inc()
	return &requestMetricsTracker{
		metrics: h.metrics,
		labels:  labels,
		start:   time.Now(),
	}
}

func (t *requestMetricsTracker) WithContext(ctx context.Context) context.Context {
	if t == nil {
		return ctx
	}
	return withUpstreamMetrics(ctx, t.labels)
}

func (t *requestMetricsTracker) WithBehavior(ctx context.Context, behavior upstreamMetricsBehavior) context.Context {
	if t == nil {
		return ctx
	}
	return withUpstreamMetricsBehavior(t.WithContext(ctx), behavior)
}

func (t *requestMetricsTracker) observeUsage(promptTokens, completionTokens int) {
	if t == nil {
		return
	}
	t.usage = requestUsageMetrics{
		promptTokens:     promptTokens,
		completionTokens: completionTokens,
		set:              true,
	}
}

func (t *requestMetricsTracker) ObserveOpenAIUsage(usage *models.OpenAIUsage) {
	if usage == nil {
		return
	}
	t.observeUsage(usage.PromptTokens, usage.CompletionTokens)
}

func (t *requestMetricsTracker) ObserveResponsesUsage(promptTokens, completionTokens int) {
	t.observeUsage(promptTokens, completionTokens)
}

func (t *requestMetricsTracker) Finish(statusCode int) {
	if t == nil || t.metrics == nil {
		return
	}
	if statusCode == 0 {
		statusCode = http.StatusInternalServerError
	}
	t.once.Do(func() {
		t.metrics.observeRequest(t.labels, statusCode, time.Since(t.start), t.usage)
		t.metrics.inflightRequests.WithLabelValues(t.labels.provider).Dec()
	})
}

func (t *requestMetricsTracker) FinishFromResponseWriter(w *metricsResponseWriter) {
	if t == nil {
		return
	}
	if w == nil {
		t.Finish(http.StatusInternalServerError)
		return
	}
	t.Finish(w.StatusCode())
}

func newMetricsResponseWriter(w http.ResponseWriter) *metricsResponseWriter {
	return &metricsResponseWriter{ResponseWriter: w}
}

func (w *metricsResponseWriter) Header() http.Header {
	return w.ResponseWriter.Header()
}

func (w *metricsResponseWriter) WriteHeader(statusCode int) {
	if w.wroteHeader {
		return
	}
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
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *metricsResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
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

func (w *metricsResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *metricsResponseWriter) StatusCode() int {
	if !w.wroteHeader {
		return 0
	}
	return w.statusCode
}

func withUpstreamMetrics(ctx context.Context, labels requestMetricLabels) context.Context {
	labels = normalizeRequestMetricLabels(labels)
	if ctx == nil {
		ctx = context.Background()
	}
	meta, _ := upstreamMetricsFromContext(ctx)
	meta.labels = labels
	return context.WithValue(ctx, upstreamMetricsContextKey{}, meta)
}

func withUpstreamMetricsBehavior(ctx context.Context, behavior upstreamMetricsBehavior) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	meta, _ := upstreamMetricsFromContext(ctx)
	meta.behavior = behavior
	return context.WithValue(ctx, upstreamMetricsContextKey{}, meta)
}

func upstreamMetricsFromContext(ctx context.Context) (upstreamMetricsContext, bool) {
	if ctx == nil {
		return upstreamMetricsContext{}, false
	}
	meta, ok := ctx.Value(upstreamMetricsContextKey{}).(upstreamMetricsContext)
	return meta, ok
}

func (h *ProxyHandler) ensureUpstreamMetricsContext(ctx context.Context, provider *providerRuntime, publicModel, endpoint string) context.Context {
	if h == nil || h.metrics == nil {
		return ctx
	}
	if meta, ok := upstreamMetricsFromContext(ctx); ok {
		if meta.labels.provider != "" && meta.labels.publicModel != "" && meta.labels.endpoint != "" {
			return ctx
		}
		if meta.labels.provider == "" && provider != nil {
			meta.labels.provider = provider.id
		}
		if meta.labels.publicModel == "" {
			meta.labels.publicModel = publicModel
		}
		if meta.labels.endpoint == "" {
			meta.labels.endpoint = endpoint
		}
		meta.labels = normalizeRequestMetricLabels(meta.labels)
		return context.WithValue(ctx, upstreamMetricsContextKey{}, meta)
	}

	labels := requestMetricLabels{
		provider:    metricLabelUnknown,
		publicModel: publicModel,
		endpoint:    endpoint,
	}
	if provider != nil {
		labels.provider = provider.id
	}
	return withUpstreamMetrics(ctx, labels)
}

func readyzMetricsEndpoint(provider *providerRuntime) string {
	if provider == nil {
		return "/models"
	}
	switch provider.kind {
	case providerTypeCopilot, providerTypeAzureOpenAI, providerTypeOpenAICodex:
		return "/models"
	default:
		return "/models"
	}
}

func classifyUpstreamTransportMetric(err error) (code, reason string) {
	if err == nil {
		return "", ""
	}
	var netErr net.Error
	if errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &netErr) && netErr.Timeout()) {
		return "timeout", "timeout"
	}
	return "transport", "transport"
}

func retryReasonForStatus(statusCode int) string {
	if statusCode == http.StatusTooManyRequests {
		return "429"
	}
	if statusCode >= http.StatusInternalServerError {
		return "5xx"
	}
	return strconv.Itoa(statusCode)
}

func expectedMaxCompletionTokensFallback(resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusBadRequest || resp.Body == nil {
		return false
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return false
	}
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return strings.Contains(strings.ToLower(string(body)), metricsExpectedFallbackBodySubstring)
}

func extractOpenAIUsageFromBody(body []byte) *models.OpenAIUsage {
	var payload struct {
		Usage *models.OpenAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	return payload.Usage
}

func extractResponsesUsageFromBody(body []byte) (int, int, bool) {
	type responsesUsage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	}
	var payload struct {
		Usage    *responsesUsage `json:"usage"`
		Response struct {
			Usage *responsesUsage `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, 0, false
	}
	if payload.Usage != nil {
		return payload.Usage.InputTokens, payload.Usage.OutputTokens, true
	}
	if payload.Response.Usage != nil {
		return payload.Response.Usage.InputTokens, payload.Response.Usage.OutputTokens, true
	}
	return 0, 0, false
}

func writeBufferedUpstreamResponse(w http.ResponseWriter, resp *http.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	return body, nil
}
