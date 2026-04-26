package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
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
	metricsUnknownLabel      = "unknown"
	tokenDirectionPrompt     = "prompt"
	tokenDirectionCompletion = "completion"
)

type metricsContextKey struct{}

type requestMetricsContext struct {
	provider    string
	publicModel string
	endpoint    string
}

// Metrics owns the Prometheus registry and Vekil-specific collectors.
type Metrics struct {
	registry         *prometheus.Registry
	handler          http.Handler
	requestsTotal    *prometheus.CounterVec
	requestDuration  *prometheus.HistogramVec
	tokensTotal      *prometheus.CounterVec
	retriesTotal     *prometheus.CounterVec
	upstreamErrors   *prometheus.CounterVec
	inflightRequests *prometheus.GaugeVec
	buildInfo        *prometheus.GaugeVec
}

type requestObservation struct {
	metrics     *Metrics
	endpoint    string
	provider    string
	publicModel string
	start       time.Time
}

type statusCapturingResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
}

// NewMetrics creates a dedicated Prometheus registry for a Vekil server.
func NewMetrics(buildVersion string) (*Metrics, error) {
	registry := prometheus.NewRegistry()
	requestsTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_requests_total",
			Help: "Total HTTP requests handled by Vekil.",
		},
		[]string{"endpoint", "provider", "public_model", "status"},
	)
	requestDuration := prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "vekil_request_duration_seconds",
			Help:    "End-to-end request duration for Vekil HTTP handlers.",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"endpoint", "provider", "public_model", "status"},
	)
	tokensTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_tokens_total",
			Help: "Prompt and completion tokens observed in upstream usage payloads.",
		},
		[]string{"endpoint", "provider", "public_model", "direction"},
	)
	retriesTotal := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_retries_total",
			Help: "Retried upstream requests.",
		},
		[]string{"endpoint", "provider", "public_model", "reason", "code"},
	)
	upstreamErrors := prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "vekil_upstream_errors_total",
			Help: "Upstream transport, protocol, and non-success response errors.",
		},
		[]string{"endpoint", "provider", "public_model", "reason", "code"},
	)
	inflightRequests := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vekil_inflight_requests",
			Help: "Requests currently in flight inside instrumented Vekil handlers.",
		},
		[]string{"endpoint", "provider", "public_model"},
	)
	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vekil_build_info",
			Help: "Build information for the running Vekil binary.",
		},
		[]string{"version"},
	)

	if buildVersion == "" {
		buildVersion = "dev"
	}

	if err := registry.Register(collectors.NewGoCollector()); err != nil {
		return nil, err
	}
	if err := registry.Register(collectors.NewProcessCollector(collectors.ProcessCollectorOpts{})); err != nil {
		return nil, err
	}
	for _, collector := range []prometheus.Collector{
		requestsTotal,
		requestDuration,
		tokensTotal,
		retriesTotal,
		upstreamErrors,
		inflightRequests,
		buildInfo,
	} {
		if err := registry.Register(collector); err != nil {
			return nil, err
		}
	}
	buildInfo.WithLabelValues(buildVersion).Set(1)

	return &Metrics{
		registry:         registry,
		handler:          promhttp.HandlerFor(registry, promhttp.HandlerOpts{}),
		requestsTotal:    requestsTotal,
		requestDuration:  requestDuration,
		tokensTotal:      tokensTotal,
		retriesTotal:     retriesTotal,
		upstreamErrors:   upstreamErrors,
		inflightRequests: inflightRequests,
		buildInfo:        buildInfo,
	}, nil
}

func (m *Metrics) Handler() http.Handler {
	if m == nil {
		return nil
	}
	return m.handler
}

func (m *Metrics) beginRequest(endpoint, provider, publicModel string) *requestObservation {
	if m == nil {
		return nil
	}
	endpoint = normalizeMetricsLabel(endpoint)
	provider = normalizeMetricsLabel(provider)
	publicModel = normalizeMetricsLabel(publicModel)
	m.inflightRequests.WithLabelValues(endpoint, provider, publicModel).Inc()
	return &requestObservation{
		metrics:     m,
		endpoint:    endpoint,
		provider:    provider,
		publicModel: publicModel,
		start:       time.Now(),
	}
}

func (o *requestObservation) finish(statusCode int) {
	if o == nil || o.metrics == nil {
		return
	}
	status := strconv.Itoa(statusCode)
	o.metrics.inflightRequests.WithLabelValues(o.endpoint, o.provider, o.publicModel).Dec()
	o.metrics.requestsTotal.WithLabelValues(o.endpoint, o.provider, o.publicModel, status).Inc()
	o.metrics.requestDuration.WithLabelValues(o.endpoint, o.provider, o.publicModel, status).Observe(time.Since(o.start).Seconds())
}

func (o *requestObservation) observeUsage(usage *models.OpenAIUsage) {
	if o == nil || usage == nil {
		return
	}
	o.metrics.observeTokens(o.endpoint, o.provider, o.publicModel, usage.PromptTokens, usage.CompletionTokens)
}

func (m *Metrics) observeTokens(endpoint, provider, publicModel string, promptTokens, completionTokens int) {
	if m == nil {
		return
	}
	endpoint = normalizeMetricsLabel(endpoint)
	provider = normalizeMetricsLabel(provider)
	publicModel = normalizeMetricsLabel(publicModel)
	if promptTokens > 0 {
		m.tokensTotal.WithLabelValues(endpoint, provider, publicModel, tokenDirectionPrompt).Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		m.tokensTotal.WithLabelValues(endpoint, provider, publicModel, tokenDirectionCompletion).Add(float64(completionTokens))
	}
}

func (m *Metrics) observeRetry(meta requestMetricsContext, reason string, code int) {
	if m == nil {
		return
	}
	m.retriesTotal.WithLabelValues(
		normalizeMetricsLabel(meta.endpoint),
		normalizeMetricsLabel(meta.provider),
		normalizeMetricsLabel(meta.publicModel),
		normalizeMetricsLabel(reason),
		strconv.Itoa(code),
	).Inc()
}

func (m *Metrics) observeUpstreamError(meta requestMetricsContext, reason string, code int) {
	if m == nil {
		return
	}
	m.upstreamErrors.WithLabelValues(
		normalizeMetricsLabel(meta.endpoint),
		normalizeMetricsLabel(meta.provider),
		normalizeMetricsLabel(meta.publicModel),
		normalizeMetricsLabel(reason),
		strconv.Itoa(code),
	).Inc()
}

func normalizeMetricsLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return metricsUnknownLabel
	}
	return value
}

func (h *ProxyHandler) boundedPublicModelLabel(publicModel, endpoint string) string {
	publicModel = strings.TrimSpace(publicModel)
	if publicModel == "" || h == nil {
		return metricsUnknownLabel
	}

	_, owner, known := h.resolveProviderModel(publicModel, endpoint)
	if !known {
		return metricsUnknownLabel
	}
	return normalizeMetricsLabel(owner.publicID)
}

func withRequestMetricsContext(ctx context.Context, meta requestMetricsContext) context.Context {
	return context.WithValue(ctx, metricsContextKey{}, requestMetricsContext{
		provider:    normalizeMetricsLabel(meta.provider),
		publicModel: normalizeMetricsLabel(meta.publicModel),
		endpoint:    normalizeMetricsLabel(meta.endpoint),
	})
}

func requestMetricsFromContext(ctx context.Context) requestMetricsContext {
	if ctx == nil {
		return requestMetricsContext{
			provider:    metricsUnknownLabel,
			publicModel: metricsUnknownLabel,
			endpoint:    metricsUnknownLabel,
		}
	}
	meta, _ := ctx.Value(metricsContextKey{}).(requestMetricsContext)
	meta.provider = normalizeMetricsLabel(meta.provider)
	meta.publicModel = normalizeMetricsLabel(meta.publicModel)
	meta.endpoint = normalizeMetricsLabel(meta.endpoint)
	return meta
}

func newStatusCapturingResponseWriter(w http.ResponseWriter) *statusCapturingResponseWriter {
	return &statusCapturingResponseWriter{ResponseWriter: w, statusCode: http.StatusOK}
}

func (w *statusCapturingResponseWriter) Unwrap() http.ResponseWriter {
	if w == nil {
		return nil
	}
	return w.ResponseWriter
}

func (w *statusCapturingResponseWriter) WriteHeader(statusCode int) {
	w.statusCode = statusCode
	w.wroteHeader = true
	w.ResponseWriter.WriteHeader(statusCode)
}

func (w *statusCapturingResponseWriter) Write(p []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusCapturingResponseWriter) Flush() {
	flusher, ok := w.ResponseWriter.(http.Flusher)
	if !ok {
		return
	}
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	flusher.Flush()
}

func (w *statusCapturingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	hijacker, ok := w.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, http.ErrNotSupported
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

func (w *statusCapturingResponseWriter) ReadFrom(r io.Reader) (int64, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	if readerFrom, ok := w.ResponseWriter.(io.ReaderFrom); ok {
		return readerFrom.ReadFrom(r)
	}
	return io.Copy(w.ResponseWriter, r)
}

func (w *statusCapturingResponseWriter) StatusCode() int {
	if w == nil {
		return http.StatusOK
	}
	return w.statusCode
}

func (h *ProxyHandler) beginRequestObservation(w http.ResponseWriter, endpointLabel, providerEndpoint, publicModel string) (*statusCapturingResponseWriter, *requestObservation) {
	wrapped := newStatusCapturingResponseWriter(w)
	if h == nil || h.metrics == nil {
		return wrapped, nil
	}
	provider := metricsUnknownLabel
	if providerRuntime, _, _ := h.resolveProviderModel(publicModel, providerEndpoint); providerRuntime != nil {
		provider = providerRuntime.id
	}
	return wrapped, h.metrics.beginRequest(endpointLabel, provider, h.boundedPublicModelLabel(publicModel, providerEndpoint))
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

func extractUsageFromBody(body []byte) *models.OpenAIUsage {
	type usagePayload struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		InputTokens      int `json:"input_tokens"`
		OutputTokens     int `json:"output_tokens"`
	}
	type bodyPayload struct {
		Usage    *usagePayload `json:"usage"`
		Response *struct {
			Usage *usagePayload `json:"usage"`
		} `json:"response"`
	}

	var payload bodyPayload
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}

	usage := payload.Usage
	if usage == nil && payload.Response != nil {
		usage = payload.Response.Usage
	}
	if usage == nil {
		return nil
	}

	promptTokens := usage.PromptTokens
	if promptTokens == 0 {
		promptTokens = usage.InputTokens
	}
	completionTokens := usage.CompletionTokens
	if completionTokens == 0 {
		completionTokens = usage.OutputTokens
	}
	if promptTokens == 0 && completionTokens == 0 {
		return nil
	}

	return &models.OpenAIUsage{
		PromptTokens:     promptTokens,
		CompletionTokens: completionTokens,
		TotalTokens:      promptTokens + completionTokens,
	}
}

func observeInvalidPayload(h *ProxyHandler, obs *requestObservation, statusCode int) {
	if h == nil || h.metrics == nil || obs == nil {
		return
	}
	h.metrics.observeUpstreamError(requestMetricsContext{
		endpoint:    obs.endpoint,
		provider:    obs.provider,
		publicModel: obs.publicModel,
	}, "invalid_payload", statusCode)
}

func cloneBody(body []byte) io.ReadCloser {
	return io.NopCloser(bytes.NewReader(body))
}
