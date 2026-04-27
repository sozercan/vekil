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
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sozercan/vekil/models"
)

const (
	unknownMetricLabelValue = "unknown"
	buildVersionDefault     = "dev"
)

var defaultRequestDurationBuckets = []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}

type requestMetricLabels struct {
	provider    string
	publicModel string
	endpoint    string
}

type responsesUsage struct {
	promptTokens     int
	completionTokens int
	ok               bool
}

type proxyMetrics struct {
	enabled         bool
	registry        *prometheus.Registry
	requestsTotal   *prometheus.CounterVec
	requestDuration *prometheus.HistogramVec
	inflight        *prometheus.GaugeVec
	tokensTotal     *prometheus.CounterVec
	retriesTotal    *prometheus.CounterVec
	upstreamErrors  *prometheus.CounterVec
	buildInfo       *prometheus.GaugeVec
}

type requestMetricsObserver struct {
	metrics  *proxyMetrics
	started  time.Time
	labels   requestMetricLabels
	inflight bool
}

func newProxyMetrics(enabled bool, buildVersion string) *proxyMetrics {
	m := &proxyMetrics{enabled: enabled}
	if !enabled {
		return m
	}

	registry := prometheus.NewRegistry()
	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)

	m.registry = registry
	m.requestsTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vekil_requests_total",
		Help: "Total proxy requests handled by endpoint, provider, model, and final status.",
	}, []string{"provider", "public_model", "endpoint", "status"})
	m.requestDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "vekil_request_duration_seconds",
		Help:    "End-to-end proxy request duration in seconds.",
		Buckets: defaultRequestDurationBuckets,
	}, []string{"provider", "public_model", "endpoint", "status"})
	m.inflight = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vekil_inflight_requests",
		Help: "Current in-flight proxy requests.",
	}, []string{"provider", "public_model", "endpoint"})
	m.tokensTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vekil_tokens_total",
		Help: "Total prompt and completion tokens observed in upstream usage payloads.",
	}, []string{"provider", "public_model", "endpoint", "direction"})
	m.retriesTotal = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vekil_retries_total",
		Help: "Total upstream retry attempts by provider, model, endpoint, and reason.",
	}, []string{"provider", "public_model", "endpoint", "reason"})
	m.upstreamErrors = prometheus.NewCounterVec(prometheus.CounterOpts{
		Name: "vekil_upstream_errors_total",
		Help: "Total upstream failures observed before returning to the client.",
	}, []string{"provider", "public_model", "endpoint", "code", "reason"})
	m.buildInfo = prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Name: "vekil_build_info",
		Help: "Build information for the running vekil binary.",
	}, []string{"version"})

	registry.MustRegister(
		m.requestsTotal,
		m.requestDuration,
		m.inflight,
		m.tokensTotal,
		m.retriesTotal,
		m.upstreamErrors,
		m.buildInfo,
	)
	m.buildInfo.WithLabelValues(normalizeMetricLabel(buildVersion)).Set(1)

	return m
}

func (m *proxyMetrics) handler() http.Handler {
	if m == nil || !m.enabled || m.registry == nil {
		return nil
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *proxyMetrics) startRequest(labels requestMetricLabels) *requestMetricsObserver {
	if m == nil || !m.enabled {
		return nil
	}
	obs := &requestMetricsObserver{
		metrics: m,
		started: time.Now(),
	}
	obs.SetLabels(labels)
	return obs
}

func (o *requestMetricsObserver) SetLabels(labels requestMetricLabels) {
	if o == nil || o.metrics == nil || !o.metrics.enabled {
		return
	}
	labels = normalizedRequestMetricLabels(labels)
	if o.inflight {
		o.metrics.inflight.WithLabelValues(o.labels.provider, o.labels.publicModel, o.labels.endpoint).Dec()
		o.inflight = false
	}
	o.labels = labels
	o.metrics.inflight.WithLabelValues(o.labels.provider, o.labels.publicModel, o.labels.endpoint).Inc()
	o.inflight = true
}

func (o *requestMetricsObserver) ObserveOpenAIUsage(usage *models.OpenAIUsage) {
	if usage == nil {
		return
	}
	o.ObserveUsageCounts(usage.PromptTokens, usage.CompletionTokens)
}

func (o *requestMetricsObserver) ObserveUsageCounts(promptTokens, completionTokens int) {
	if o == nil || o.metrics == nil || !o.metrics.enabled {
		return
	}
	if promptTokens > 0 {
		o.metrics.tokensTotal.WithLabelValues(o.labels.provider, o.labels.publicModel, o.labels.endpoint, "prompt").Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		o.metrics.tokensTotal.WithLabelValues(o.labels.provider, o.labels.publicModel, o.labels.endpoint, "completion").Add(float64(completionTokens))
	}
}

func (o *requestMetricsObserver) Finish(status int) {
	if o == nil || o.metrics == nil || !o.metrics.enabled {
		return
	}
	if !o.inflight {
		o.SetLabels(o.labels)
	}
	if o.inflight {
		o.metrics.inflight.WithLabelValues(o.labels.provider, o.labels.publicModel, o.labels.endpoint).Dec()
		o.inflight = false
	}
	statusLabel := normalizeMetricLabel(strconv.Itoa(status))
	o.metrics.requestsTotal.WithLabelValues(o.labels.provider, o.labels.publicModel, o.labels.endpoint, statusLabel).Inc()
	o.metrics.requestDuration.WithLabelValues(o.labels.provider, o.labels.publicModel, o.labels.endpoint, statusLabel).Observe(time.Since(o.started).Seconds())
}

func (m *proxyMetrics) observeRetry(labels requestMetricLabels, reason string) {
	if m == nil || !m.enabled {
		return
	}
	labels = normalizedRequestMetricLabels(labels)
	m.retriesTotal.WithLabelValues(labels.provider, labels.publicModel, labels.endpoint, normalizeMetricLabel(reason)).Inc()
}

func (m *proxyMetrics) observeUpstreamError(labels requestMetricLabels, code, reason string) {
	if m == nil || !m.enabled {
		return
	}
	labels = normalizedRequestMetricLabels(labels)
	m.upstreamErrors.WithLabelValues(labels.provider, labels.publicModel, labels.endpoint, normalizeMetricLabel(code), normalizeMetricLabel(reason)).Inc()
}

func normalizedRequestMetricLabels(labels requestMetricLabels) requestMetricLabels {
	return requestMetricLabels{
		provider:    normalizeMetricLabel(labels.provider),
		publicModel: normalizeMetricLabel(labels.publicModel),
		endpoint:    normalizeMetricLabel(labels.endpoint),
	}
}

func normalizeMetricLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return unknownMetricLabelValue
	}
	return value
}

func retryReasonFromStatus(statusCode int) string {
	return "http_" + strconv.Itoa(statusCode)
}

func metricCodeFromStatus(statusCode int) string {
	return strconv.Itoa(statusCode)
}

func metricReasonFromError(err error) string {
	switch {
	case err == nil:
		return "error"
	case errors.Is(err, context.Canceled):
		return "context_canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	}

	var providerErr *providerRequestError
	if errors.As(err, &providerErr) {
		return "provider_request"
	}
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) {
		return retryReasonFromStatus(upstreamErr.statusCode)
	}
	return "transport"
}

func metricCodeFromError(err error) string {
	var providerErr *providerRequestError
	if errors.As(err, &providerErr) {
		return strconv.Itoa(providerErr.statusCode)
	}
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) {
		return strconv.Itoa(upstreamErr.statusCode)
	}
	return "error"
}

func (h *ProxyHandler) MetricsHandler() http.Handler {
	if h == nil || h.metrics == nil {
		return nil
	}
	return h.metrics.handler()
}

func metricEndpointFromUpstreamPath(path string) string {
	path = strings.TrimSpace(path)
	path = strings.Trim(path, "/")
	if path == "" {
		return unknownMetricLabelValue
	}
	return strings.ReplaceAll(path, "/", "_")
}

func (h *ProxyHandler) startRequestObserver(model, upstreamEndpoint, metricEndpoint string) *requestMetricsObserver {
	if h == nil || h.metrics == nil {
		return nil
	}
	return h.metrics.startRequest(h.requestMetricsLabels(model, upstreamEndpoint, metricEndpoint))
}

func (h *ProxyHandler) observeUpstreamErrorForLabels(labels requestMetricLabels, err error) {
	if h == nil || h.metrics == nil || err == nil {
		return
	}
	h.metrics.observeUpstreamError(labels, metricCodeFromError(err), metricReasonFromError(err))
}

func (h *ProxyHandler) observeUpstreamStatusForLabels(labels requestMetricLabels, statusCode int) {
	if h == nil || h.metrics == nil || statusCode < http.StatusBadRequest {
		return
	}
	h.metrics.observeUpstreamError(labels, metricCodeFromStatus(statusCode), retryReasonFromStatus(statusCode))
}

func (h *ProxyHandler) requestMetricsLabels(model, upstreamEndpoint, metricEndpoint string) requestMetricLabels {
	labels := requestMetricLabels{
		publicModel: unknownMetricLabelValue,
		endpoint:    metricEndpoint,
	}
	if h == nil {
		return normalizedRequestMetricLabels(labels)
	}
	provider, owner, known := h.resolveProviderModel(model, upstreamEndpoint)
	if provider != nil {
		labels.provider = provider.id
	}
	if known && strings.TrimSpace(owner.publicID) != "" {
		labels.publicModel = owner.publicID
	}
	return normalizedRequestMetricLabels(labels)
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

func extractResponsesUsageFromBody(body []byte) responsesUsage {
	var payload struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return responsesUsage{}
	}
	return responsesUsage{
		promptTokens:     payload.Usage.InputTokens,
		completionTokens: payload.Usage.OutputTokens,
		ok:               payload.Usage.InputTokens > 0 || payload.Usage.OutputTokens > 0,
	}
}

func extractResponsesCompletedUsage(body []byte) responsesUsage {
	var payload struct {
		Response struct {
			Usage struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		} `json:"response"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return responsesUsage{}
	}
	return responsesUsage{
		promptTokens:     payload.Response.Usage.InputTokens,
		completionTokens: payload.Response.Usage.OutputTokens,
		ok:               payload.Response.Usage.InputTokens > 0 || payload.Response.Usage.OutputTokens > 0,
	}
}

const bufferedUpstreamResponseCaptureLimit = 1024 * 1024

type cappedResponseBuffer struct {
	buf       bytes.Buffer
	limit     int
	truncated bool
}

func newCappedResponseBuffer(limit int) *cappedResponseBuffer {
	return &cappedResponseBuffer{limit: limit}
}

func (b *cappedResponseBuffer) Write(p []byte) (int, error) {
	if b == nil {
		return len(p), nil
	}
	if b.limit <= 0 {
		b.truncated = true
		return len(p), nil
	}
	remaining := b.limit - b.buf.Len()
	if remaining <= 0 {
		b.truncated = true
		return len(p), nil
	}
	if len(p) > remaining {
		_, _ = b.buf.Write(p[:remaining])
		b.truncated = true
		return len(p), nil
	}
	_, _ = b.buf.Write(p)
	return len(p), nil
}

func (b *cappedResponseBuffer) Bytes() []byte {
	if b == nil || b.truncated {
		return nil
	}
	return b.buf.Bytes()
}

func writeBufferedUpstreamResponse(w http.ResponseWriter, resp *http.Response) ([]byte, error) {
	defer func() { _ = resp.Body.Close() }()
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	buf := newCappedResponseBuffer(bufferedUpstreamResponseCaptureLimit)
	if _, err := io.Copy(io.MultiWriter(w, buf), resp.Body); err != nil {
		return nil, err
	}
	body := buf.Bytes()
	if body == nil {
		return nil, nil
	}
	return append([]byte(nil), body...), nil
}

type statusCapturingResponseWriter struct {
	http.ResponseWriter
	status      int
	wroteHeader bool
}

func newStatusCapturingResponseWriter(w http.ResponseWriter) *statusCapturingResponseWriter {
	return &statusCapturingResponseWriter{ResponseWriter: w}
}

func (w *statusCapturingResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.status = status
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(status)
}

func (w *statusCapturingResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusCapturingResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (w *statusCapturingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, fmt.Errorf("response writer does not support hijacking")
	}
	return hijacker.Hijack()
}

func (w *statusCapturingResponseWriter) Push(target string, opts *http.PushOptions) error {
	pusher, ok := w.ResponseWriter.(http.Pusher)
	if !ok {
		return http.ErrNotSupported
	}
	return pusher.Push(target, opts)
}

func (w *statusCapturingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *statusCapturingResponseWriter) Status() int {
	if w.wroteHeader {
		return w.status
	}
	return http.StatusOK
}
