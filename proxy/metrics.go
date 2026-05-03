package proxy

import (
	"context"
	"encoding/json"
	"errors"
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
	"github.com/sozercan/vekil/models"
)

const metricsUnknownLabel = "unknown"

// BuildInfo describes the version metadata exported through Prometheus.
type BuildInfo struct {
	Version string
	Commit  string
}

// DefaultBuildInfo returns the fallback build metadata for local builds.
func DefaultBuildInfo() BuildInfo {
	return BuildInfo{
		Version: "dev",
		Commit:  "unknown",
	}
}

// WithMetricsEnabled enables or disables the Prometheus /metrics endpoint and
// all request instrumentation.
func WithMetricsEnabled(enabled bool) Option {
	return func(h *ProxyHandler) {
		h.metricsEnabled = enabled
	}
}

// WithBuildInfo overrides the build metadata exported via vekil_build_info.
func WithBuildInfo(info BuildInfo) Option {
	return func(h *ProxyHandler) {
		h.buildInfo = info.withDefaults()
	}
}

func (b BuildInfo) withDefaults() BuildInfo {
	defaults := DefaultBuildInfo()
	b.Version = strings.TrimSpace(b.Version)
	if b.Version == "" {
		b.Version = defaults.Version
	}
	b.Commit = strings.TrimSpace(b.Commit)
	if b.Commit == "" {
		b.Commit = defaults.Commit
	}
	return b
}

type proxyMetrics struct {
	registry       *prometheus.Registry
	handler        http.Handler
	requests       *prometheus.CounterVec
	duration       *prometheus.HistogramVec
	firstByte      *prometheus.HistogramVec
	tokens         *prometheus.CounterVec
	retries        *prometheus.CounterVec
	upstreamErrors *prometheus.CounterVec
	inflight       *prometheus.GaugeVec
	endpointHealth *prometheus.GaugeVec
	buildInfo      *prometheus.GaugeVec
}

func newProxyMetrics(buildInfo BuildInfo) *proxyMetrics {
	buildInfo = buildInfo.withDefaults()
	registry := prometheus.NewRegistry()

	metrics := &proxyMetrics{
		registry: registry,
		requests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vekil",
			Name:      "requests_total",
			Help:      "Total logical API requests handled by Vekil.",
		}, []string{"provider", "public_model", "endpoint", "status"}),
		duration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "vekil",
			Name:      "request_duration_seconds",
			Help:      "End-to-end logical request latency in seconds.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"provider", "public_model", "endpoint", "status"}),
		firstByte: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: "vekil",
			Name:      "request_first_byte_latency_seconds",
			Help:      "Time to first response byte for streaming requests in seconds.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"provider", "public_model", "endpoint"}),
		tokens: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vekil",
			Name:      "tokens_total",
			Help:      "Total upstream tokens consumed by logical requests.",
		}, []string{"provider", "public_model", "endpoint", "direction"}),
		retries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vekil",
			Name:      "retries_total",
			Help:      "Total retry attempts triggered for transient upstream failures.",
		}, []string{"provider", "public_model", "endpoint", "reason"}),
		upstreamErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vekil",
			Name:      "upstream_errors_total",
			Help:      "Total final upstream failures surfaced to clients.",
		}, []string{"provider", "public_model", "endpoint", "code"}),
		inflight: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "vekil",
			Name:      "inflight_requests",
			Help:      "Current in-flight logical requests by provider.",
		}, []string{"provider"}),
		endpointHealth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "vekil",
			Name:      "endpoint_healthy",
			Help:      "Provider endpoint health (reserved for future active health checks).",
		}, []string{"provider", "endpoint"}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Namespace: "vekil",
			Name:      "build_info",
			Help:      "Build metadata for this Vekil binary.",
		}, []string{"version", "go_version", "commit"}),
	}

	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		metrics.requests,
		metrics.duration,
		metrics.firstByte,
		metrics.tokens,
		metrics.retries,
		metrics.upstreamErrors,
		metrics.inflight,
		metrics.endpointHealth,
		metrics.buildInfo,
	)
	metrics.buildInfo.WithLabelValues(buildInfo.Version, runtime.Version(), buildInfo.Commit).Set(1)
	metrics.handler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	return metrics
}

type requestMetricsTracker struct {
	metrics          *proxyMetrics
	provider         string
	publicModel      string
	requestedModel   string
	endpoint         string
	start            time.Time
	promptTokens     float64
	completionTokens float64
	inflightActive   bool
	finished         bool
	firstByteOnce    sync.Once
	firstByteLatency float64
	firstBytePending bool
	pendingRetries   map[string]uint64
	pendingErrors    map[string]uint64

	rememberResolvedPublicModel func(string)
}

type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

func normalizeMetricLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return metricsUnknownLabel
	}
	return value
}

// MetricsHandler returns the Prometheus exposition handler when metrics are
// enabled. A nil result means /metrics should not be mounted.
func (h *ProxyHandler) MetricsHandler() http.Handler {
	if h == nil || h.metrics == nil {
		return nil
	}
	return h.metrics.handler
}

func (h *ProxyHandler) beginRequestMetrics(metricEndpoint, providerEndpoint, publicModel string) *requestMetricsTracker {
	if h == nil || h.metrics == nil {
		return nil
	}

	model := strings.TrimSpace(publicModel)
	tracker := &requestMetricsTracker{
		metrics:        h.metrics,
		publicModel:    metricsUnknownLabel,
		requestedModel: model,
		endpoint:       normalizeMetricLabel(metricEndpoint),
		start:          time.Now(),
	}
	tracker.rememberResolvedPublicModel = func(publicModel string) {
		h.rememberMetricsPublicModel(tracker.provider, publicModel)
	}

	if model == "" {
		return tracker
	}

	provider, owner, known := h.resolveProviderModel(model, providerEndpoint)
	if provider != nil {
		tracker.setProvider(provider.id)
	}
	if known {
		tracker.setPublicModel(owner.publicID)
	}

	return tracker
}

func (h *ProxyHandler) rememberMetricsPublicModel(providerID, publicModel string) {
	if h == nil {
		return
	}
	providerID = strings.TrimSpace(providerID)
	publicModel = strings.TrimSpace(publicModel)
	if providerID == "" || providerID == metricsUnknownLabel || publicModel == "" {
		return
	}

	setup := h.providerSetup()
	provider := setup.providerByID(providerID)
	if !providerUsesDynamicModels(provider) {
		return
	}

	_ = setup.rememberModel(providerModel{
		publicID:      publicModel,
		upstreamModel: publicModel,
		providerID:    providerID,
	})
}

func (t *requestMetricsTracker) setPublicModel(publicModel string) {
	if t == nil {
		return
	}
	publicModel = strings.TrimSpace(publicModel)
	if normalizeMetricLabel(publicModel) == metricsUnknownLabel {
		return
	}
	if strings.TrimSpace(t.publicModel) == publicModel {
		return
	}
	t.publicModel = publicModel
	if t.rememberResolvedPublicModel != nil {
		t.rememberResolvedPublicModel(publicModel)
	}
}

func (t *requestMetricsTracker) setProvider(provider string) {
	if t == nil || t.metrics == nil {
		return
	}

	provider = normalizeMetricLabel(provider)
	if provider == t.provider {
		return
	}
	if t.inflightActive {
		t.metrics.inflight.WithLabelValues(t.providerLabel()).Dec()
		t.inflightActive = false
	}
	if provider == metricsUnknownLabel {
		t.provider = provider
		return
	}

	t.provider = provider
	t.metrics.inflight.WithLabelValues(t.providerLabel()).Inc()
	t.inflightActive = true
}

func (t *requestMetricsTracker) providerLabel() string {
	if t == nil {
		return metricsUnknownLabel
	}
	return normalizeMetricLabel(t.provider)
}

func (t *requestMetricsTracker) publicModelLabel() string {
	if t == nil {
		return metricsUnknownLabel
	}
	return normalizeMetricLabel(t.publicModel)
}

func (t *requestMetricsTracker) endpointLabel() string {
	if t == nil {
		return metricsUnknownLabel
	}
	return normalizeMetricLabel(t.endpoint)
}

func (t *requestMetricsTracker) ObserveFirstByte() {
	if t == nil || t.metrics == nil {
		return
	}

	t.firstByteOnce.Do(func() {
		latency := time.Since(t.start).Seconds()
		if t.publicModelLabel() == metricsUnknownLabel {
			t.firstByteLatency = latency
			t.firstBytePending = true
			return
		}
		t.metrics.firstByte.WithLabelValues(t.providerLabel(), t.publicModelLabel(), t.endpointLabel()).Observe(latency)
	})
}

func (t *requestMetricsTracker) ObserveOpenAIUsage(usage *models.OpenAIUsage) {
	if t == nil || usage == nil {
		return
	}
	t.promptTokens += float64(usage.PromptTokens)
	t.completionTokens += float64(usage.CompletionTokens)
}

func (t *requestMetricsTracker) ObserveResponsesUsage(usage *responsesUsage) {
	if t == nil || usage == nil {
		return
	}
	t.promptTokens += float64(usage.InputTokens)
	t.completionTokens += float64(usage.OutputTokens)
}

func (t *requestMetricsTracker) RecordRetry(reason string) {
	if t == nil || t.metrics == nil {
		return
	}
	reason = normalizeMetricLabel(reason)
	if t.publicModelLabel() == metricsUnknownLabel {
		if t.pendingRetries == nil {
			t.pendingRetries = make(map[string]uint64)
		}
		t.pendingRetries[reason]++
		return
	}
	t.metrics.retries.WithLabelValues(t.providerLabel(), t.publicModelLabel(), t.endpointLabel(), reason).Inc()
}

func (t *requestMetricsTracker) RecordUpstreamError(code string) {
	if t == nil || t.metrics == nil {
		return
	}
	code = normalizeMetricLabel(code)
	if t.publicModelLabel() == metricsUnknownLabel {
		if t.pendingErrors == nil {
			t.pendingErrors = make(map[string]uint64)
		}
		t.pendingErrors[code]++
		return
	}
	t.metrics.upstreamErrors.WithLabelValues(t.providerLabel(), t.publicModelLabel(), t.endpointLabel(), code).Inc()
}

func (t *requestMetricsTracker) ObserveTrustedUpstreamModel(model string) {
	if t == nil {
		return
	}
	if t.publicModelLabel() != metricsUnknownLabel {
		return
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return
	}
	if model == strings.TrimSpace(t.requestedModel) {
		t.setPublicModel(model)
	}
}

func (t *requestMetricsTracker) flushPendingMetrics() {
	if t == nil || t.metrics == nil {
		return
	}
	if t.firstBytePending {
		t.metrics.firstByte.WithLabelValues(t.providerLabel(), t.publicModelLabel(), t.endpointLabel()).Observe(t.firstByteLatency)
		t.firstBytePending = false
	}
	for reason, count := range t.pendingRetries {
		t.metrics.retries.WithLabelValues(t.providerLabel(), t.publicModelLabel(), t.endpointLabel(), reason).Add(float64(count))
		delete(t.pendingRetries, reason)
	}
	for code, count := range t.pendingErrors {
		t.metrics.upstreamErrors.WithLabelValues(t.providerLabel(), t.publicModelLabel(), t.endpointLabel(), code).Add(float64(count))
		delete(t.pendingErrors, code)
	}
}

func (t *requestMetricsTracker) Finish(statusCode int) {
	if t == nil || t.metrics == nil || t.finished {
		return
	}
	t.finished = true
	t.flushPendingMetrics()

	status := strconv.Itoa(statusCode)
	provider := t.providerLabel()
	publicModel := t.publicModelLabel()
	endpoint := t.endpointLabel()

	t.metrics.requests.WithLabelValues(provider, publicModel, endpoint, status).Inc()
	t.metrics.duration.WithLabelValues(provider, publicModel, endpoint, status).Observe(time.Since(t.start).Seconds())
	if t.promptTokens > 0 {
		t.metrics.tokens.WithLabelValues(provider, publicModel, endpoint, "prompt").Add(t.promptTokens)
	}
	if t.completionTokens > 0 {
		t.metrics.tokens.WithLabelValues(provider, publicModel, endpoint, "completion").Add(t.completionTokens)
	}
	if t.inflightActive {
		t.metrics.inflight.WithLabelValues(provider).Dec()
		t.inflightActive = false
	}
}

func extractOpenAIUsageFromBody(body []byte) *models.OpenAIUsage {
	return extractOpenAIResponseMetricsFromBody(body).Usage
}

func extractOpenAIUsageFromReader(body io.Reader) *models.OpenAIUsage {
	return extractOpenAIResponseMetricsFromReader(body).Usage
}

type openAIResponseMetrics struct {
	Model string
	Usage *models.OpenAIUsage
}

func extractOpenAIResponseMetricsFromBody(body []byte) openAIResponseMetrics {
	var payload struct {
		Model string              `json:"model,omitempty"`
		Usage *models.OpenAIUsage `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return openAIResponseMetrics{}
	}
	return openAIResponseMetrics{
		Model: strings.TrimSpace(payload.Model),
		Usage: payload.Usage,
	}
}

func extractOpenAIResponseMetricsFromReader(body io.Reader) openAIResponseMetrics {
	var payload struct {
		Model string              `json:"model,omitempty"`
		Usage *models.OpenAIUsage `json:"usage,omitempty"`
	}
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		return openAIResponseMetrics{}
	}
	return openAIResponseMetrics{
		Model: strings.TrimSpace(payload.Model),
		Usage: payload.Usage,
	}
}

func extractResponsesUsageFromBody(body []byte) *responsesUsage {
	return extractResponsesResponseMetricsFromBody(body).Usage
}

func extractResponsesUsageFromReader(body io.Reader) *responsesUsage {
	return extractResponsesResponseMetricsFromReader(body).Usage
}

type responsesResponseMetrics struct {
	Model string
	Usage *responsesUsage
}

func extractResponsesResponseMetricsFromBody(body []byte) responsesResponseMetrics {
	var payload struct {
		Model string          `json:"model,omitempty"`
		Usage *responsesUsage `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return responsesResponseMetrics{}
	}
	return responsesResponseMetrics{
		Model: strings.TrimSpace(payload.Model),
		Usage: payload.Usage,
	}
}

func extractResponsesResponseMetricsFromReader(body io.Reader) responsesResponseMetrics {
	var payload struct {
		Model string          `json:"model,omitempty"`
		Usage *responsesUsage `json:"usage,omitempty"`
	}
	if err := json.NewDecoder(body).Decode(&payload); err != nil {
		return responsesResponseMetrics{}
	}
	return responsesResponseMetrics{
		Model: strings.TrimSpace(payload.Model),
		Usage: payload.Usage,
	}
}

type firstByteObserverResponseWriter struct {
	http.ResponseWriter
	onFirstByte func()
	once        sync.Once
}

func newFirstByteObserverResponseWriter(w http.ResponseWriter, onFirstByte func()) http.ResponseWriter {
	if w == nil || onFirstByte == nil {
		return w
	}
	return &firstByteObserverResponseWriter{ResponseWriter: w, onFirstByte: onFirstByte}
}

func (w *firstByteObserverResponseWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.once.Do(func() {
			if w.onFirstByte != nil {
				w.onFirstByte()
			}
		})
	}
	return w.ResponseWriter.Write(p)
}

func (w *firstByteObserverResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func writeUpstreamResponseWithObserver(w http.ResponseWriter, resp *http.Response, observeBody func(io.Reader)) {
	defer func() { _ = resp.Body.Close() }()
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	if observeBody == nil {
		_, _ = io.Copy(w, resp.Body)
		return
	}

	tee := io.TeeReader(resp.Body, w)
	observeBody(tee)
	_, _ = io.Copy(io.Discard, tee)
}

type openAIStreamUsageTap struct {
	parser responsesSSEParser
	usage  *models.OpenAIUsage
	model  string
}

func newOpenAIStreamUsageTap() *openAIStreamUsageTap {
	return &openAIStreamUsageTap{parser: responsesSSEParser{allowBOM: true}}
}

func (t *openAIStreamUsageTap) Write(p []byte) (int, error) {
	t.parser.push(p)
	for {
		msg, ok := t.parser.nextSemantic()
		if !ok {
			break
		}
		if model := extractOpenAIModelFromChunk(msg.data); model != "" {
			t.model = model
		}
		if usage := extractOpenAIUsageFromChunk(msg.data); usage != nil {
			t.usage = usage
		}
	}
	if len(t.parser.pending) > responsesFailureTapMaxBuffer {
		t.parser.pending = nil
		t.parser.allowBOM = false
	}
	return len(p), nil
}

func (t *openAIStreamUsageTap) usageCopy() *models.OpenAIUsage {
	if t == nil || t.usage == nil {
		return nil
	}
	copied := *t.usage
	return &copied
}

func (t *openAIStreamUsageTap) modelCopy() string {
	if t == nil {
		return ""
	}
	return strings.TrimSpace(t.model)
}

func extractOpenAIUsageFromChunk(data string) *models.OpenAIUsage {
	data = strings.TrimSpace(data)
	if data == "" || data == "[DONE]" {
		return nil
	}

	var chunk models.OpenAIStreamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil || chunk.Usage == nil {
		return nil
	}
	copied := *chunk.Usage
	return &copied
}

func extractOpenAIModelFromChunk(data string) string {
	data = strings.TrimSpace(data)
	if data == "" || data == "[DONE]" {
		return ""
	}

	var chunk models.OpenAIStreamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return ""
	}
	return strings.TrimSpace(chunk.Model)
}

type responsesStreamMetricsTap struct {
	parser     responsesSSEParser
	usage      *responsesUsage
	statusCode int
	model      string
}

func newResponsesStreamMetricsTap() *responsesStreamMetricsTap {
	return &responsesStreamMetricsTap{parser: responsesSSEParser{allowBOM: true}}
}

func (t *responsesStreamMetricsTap) Write(p []byte) (int, error) {
	t.parser.push(p)
	for {
		msg, ok := t.parser.nextSemantic()
		if !ok {
			break
		}
		data := strings.TrimSpace(msg.data)
		if data == "" || data == "[DONE]" {
			continue
		}
		event, err := parseResponsesStreamEvent(data)
		if err != nil {
			continue
		}
		if event.Response.Usage != nil {
			copied := *event.Response.Usage
			t.usage = &copied
		}
		if model := strings.TrimSpace(event.Response.Model); model != "" {
			t.model = model
		}
		switch strings.TrimSpace(event.Type) {
		case "response.completed":
			if t.statusCode == 0 {
				t.statusCode = http.StatusOK
			}
		case "response.failed", "response.incomplete":
			status, _, _ := responsesWebSocketStreamFailureDetails(event)
			if status != 0 {
				t.statusCode = status
			}
		}
	}
	if len(t.parser.pending) > responsesFailureTapMaxBuffer {
		t.parser.pending = nil
		t.parser.allowBOM = false
	}
	return len(p), nil
}

func (t *responsesStreamMetricsTap) usageCopy() *responsesUsage {
	if t == nil || t.usage == nil {
		return nil
	}
	copied := *t.usage
	return &copied
}

func (t *responsesStreamMetricsTap) modelCopy() string {
	if t == nil {
		return ""
	}
	return strings.TrimSpace(t.model)
}

func (t *responsesStreamMetricsTap) logicalStatusCode() int {
	if t == nil || t.statusCode == 0 {
		return http.StatusOK
	}
	return t.statusCode
}

func retryMetricReason(statusCode int) string {
	if statusCode == http.StatusTooManyRequests {
		return "429"
	}
	return "5xx"
}

func retryMetricReasonFromError(err error) string {
	if isTimeoutError(err) {
		return "timeout"
	}
	return "transport"
}

func upstreamErrorMetricCode(err error) (string, bool) {
	if err == nil {
		return "", false
	}

	var providerErr *providerRequestError
	if errors.As(err, &providerErr) {
		return "", false
	}

	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) {
		return strconv.Itoa(upstreamErr.statusCode), true
	}

	if errors.Is(err, context.Canceled) {
		return "", false
	}
	if isTimeoutError(err) {
		return "timeout", true
	}
	return "transport", true
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr) && netErr.Timeout()
}
