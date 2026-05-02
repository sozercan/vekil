package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
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

const (
	metricsEndpointMessages              = "/v1/messages"
	metricsEndpointChatCompletions       = "/v1/chat/completions"
	metricsEndpointResponses             = "/v1/responses"
	metricsEndpointResponsesCompact      = "/v1/responses/compact"
	metricsEndpointResponsesWebSocket    = "GET /v1/responses"
	metricsEndpointMemoryTraceSummarize  = "/v1/memories/trace_summarize"
	metricsEndpointGeminiGenerateContent = "gemini:generateContent"
	metricsEndpointGeminiStreamGenerate  = "gemini:streamGenerateContent"
	metricsEndpointGeminiCountTokens     = "gemini:countTokens"
	metricsPublicModelUnresolved         = "unresolved"
)

// BuildInfo captures build metadata exposed via Prometheus and the CLI version
// surface.
type BuildInfo struct {
	Version   string
	GoVersion string
	Commit    string
}

// DefaultBuildInfo returns the default build metadata used when no explicit
// build-time values are injected.
func DefaultBuildInfo() BuildInfo {
	return BuildInfo{}.normalized()
}

func (info BuildInfo) normalized() BuildInfo {
	info.Version = strings.TrimSpace(info.Version)
	if info.Version == "" {
		info.Version = "dev"
	}

	info.GoVersion = strings.TrimSpace(info.GoVersion)
	if info.GoVersion == "" {
		info.GoVersion = runtime.Version()
	}

	info.Commit = strings.TrimSpace(info.Commit)
	if info.Commit == "" {
		info.Commit = "unknown"
	}

	return info
}

type responsesUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens,omitempty"`
}

type proxyMetrics struct {
	registry            *prometheus.Registry
	requestsTotal       *prometheus.CounterVec
	requestDuration     *prometheus.HistogramVec
	requestFirstByte    *prometheus.HistogramVec
	tokensTotal         *prometheus.CounterVec
	retriesTotal        *prometheus.CounterVec
	upstreamErrorsTotal *prometheus.CounterVec
	inflightRequests    *prometheus.GaugeVec
	endpointHealthy     *prometheus.GaugeVec
	buildInfo           *prometheus.GaugeVec
}

func newProxyMetrics(info BuildInfo) *proxyMetrics {
	registry := prometheus.NewRegistry()
	metrics := &proxyMetrics{
		registry: registry,
		requestsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vekil_requests_total",
				Help: "Total proxied client requests grouped by provider, public model, endpoint, and final status.",
			},
			[]string{"provider", "public_model", "endpoint", "status"},
		),
		requestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "vekil_request_duration_seconds",
				Help:    "End-to-end request duration grouped by provider, public model, endpoint, and final status.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"provider", "public_model", "endpoint", "status"},
		),
		requestFirstByte: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "vekil_request_first_byte_seconds",
				Help:    "Time to first response byte for streaming requests grouped by provider, public model, and endpoint.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"provider", "public_model", "endpoint"},
		),
		tokensTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vekil_tokens_total",
				Help: "Total upstream tokens reported by usage blocks, grouped by provider, public model, and direction.",
			},
			[]string{"provider", "public_model", "direction"},
		),
		retriesTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vekil_retries_total",
				Help: "Total upstream retry attempts grouped by provider, public model, and retry reason.",
			},
			[]string{"provider", "public_model", "reason"},
		),
		upstreamErrorsTotal: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vekil_upstream_errors_total",
				Help: "Total upstream errors grouped by provider, public model, and upstream error code.",
			},
			[]string{"provider", "public_model", "code"},
		),
		inflightRequests: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "vekil_inflight_requests",
				Help: "Current number of in-flight client requests grouped by provider.",
			},
			[]string{"provider"},
		),
		endpointHealthy: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "vekil_endpoint_healthy",
				Help: "Latest readiness result grouped by provider and redacted upstream endpoint label (1=healthy, 0=unhealthy).",
			},
			[]string{"provider", "endpoint"},
		),
		buildInfo: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "vekil_build_info",
				Help: "Build metadata for this Vekil binary.",
			},
			[]string{"version", "go_version", "commit"},
		),
	}

	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		metrics.requestsTotal,
		metrics.requestDuration,
		metrics.requestFirstByte,
		metrics.tokensTotal,
		metrics.retriesTotal,
		metrics.upstreamErrorsTotal,
		metrics.inflightRequests,
		metrics.endpointHealthy,
		metrics.buildInfo,
	)

	info = info.normalized()
	metrics.buildInfo.WithLabelValues(info.Version, info.GoVersion, info.Commit).Set(1)

	return metrics
}

func (m *proxyMetrics) handler() http.Handler {
	if m == nil || m.registry == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *proxyMetrics) initializeProviderHealth(setup *providerSetup) {
	if m == nil || setup == nil {
		return
	}

	for _, providerID := range setup.providerOrder {
		provider := setup.providerByID(providerID)
		if provider == nil {
			continue
		}
		m.endpointHealthy.WithLabelValues(provider.id, providerHealthEndpoint(provider)).Set(0)
	}
}

type requestObservation struct {
	metrics       *proxyMetrics
	endpoint      string
	start         time.Time
	firstByteOnce sync.Once
	mu            sync.Mutex
	provider      string
	publicModel   string
	inflight      bool
	finished      bool
}

func (h *ProxyHandler) newRequestObservation(endpoint string) *requestObservation {
	if h == nil || h.metrics == nil {
		return nil
	}
	return &requestObservation{
		metrics:     h.metrics,
		endpoint:    strings.TrimSpace(endpoint),
		start:       time.Now(),
		publicModel: metricsPublicModelUnresolved,
	}
}

func (o *requestObservation) bindProxyRequest(h *ProxyHandler, requestedModel, routingEndpoint string) {
	if o == nil || h == nil {
		return
	}

	requestedModel = strings.TrimSpace(requestedModel)
	provider, owner, known := h.resolveProviderModel(requestedModel, routingEndpoint)
	publicModel := metricsPublicModelUnresolved
	if known && strings.TrimSpace(owner.publicID) != "" {
		publicModel = normalizeKnownMetricModelLabel(owner.publicID)
	}

	if provider != nil {
		o.bindProvider(provider.id, publicModel)
		return
	}

	o.bindProvider("", publicModel)
}

func (o *requestObservation) bindProvider(providerID, publicModel string) {
	if o == nil || o.metrics == nil {
		return
	}

	providerID = normalizeMetricLabel(providerID)
	publicModel = normalizeMetricLabel(publicModel)

	o.mu.Lock()
	defer o.mu.Unlock()

	if publicModel != "" {
		o.publicModel = publicModel
	}

	if providerID == "" {
		return
	}

	if o.inflight && o.provider == providerID {
		o.provider = providerID
		return
	}

	if o.inflight && o.provider != "" && o.provider != providerID {
		o.metrics.inflightRequests.WithLabelValues(o.provider).Dec()
		o.inflight = false
	}

	o.provider = providerID
	if !o.inflight {
		o.metrics.inflightRequests.WithLabelValues(providerID).Inc()
		o.inflight = true
	}
}

func (o *requestObservation) withContext(ctx context.Context) context.Context {
	if ctx == nil || o == nil {
		return ctx
	}
	return context.WithValue(ctx, requestObservationContextKey{}, o)
}

func requestObservationFromContext(ctx context.Context) *requestObservation {
	if ctx == nil {
		return nil
	}
	obs, _ := ctx.Value(requestObservationContextKey{}).(*requestObservation)
	return obs
}

func (o *requestObservation) observeOpenAIUsage(usage *models.OpenAIUsage) {
	if o == nil || usage == nil {
		return
	}
	o.observeTokens(usage.PromptTokens, usage.CompletionTokens)
}

func (o *requestObservation) observeOpenAIUsageFromBody(body []byte) {
	if o == nil || len(body) == 0 {
		return
	}

	_, usage, err := parseOpenAIResponseMetadata(bytes.NewReader(body))
	if err != nil {
		return
	}
	o.observeOpenAIUsage(usage)
}

func (o *requestObservation) observeOpenAIUsageFromReader(r io.Reader) {
	if o == nil || r == nil {
		return
	}

	_, usage, err := parseOpenAIResponseMetadata(r)
	if err != nil {
		return
	}
	o.observeOpenAIUsage(usage)
}

func (o *requestObservation) observeResponsesUsage(usage *responsesUsage) {
	if o == nil || usage == nil {
		return
	}
	o.observeTokens(usage.InputTokens, usage.OutputTokens)
}

func (o *requestObservation) observeResponsesUsageFromBody(body []byte) {
	if o == nil || len(body) == 0 {
		return
	}

	_, usage, err := parseResponsesResponseMetadata(bytes.NewReader(body))
	if err != nil {
		return
	}
	o.observeResponsesUsage(usage)
}

func (o *requestObservation) observeResponsesUsageFromReader(r io.Reader) {
	if o == nil || r == nil {
		return
	}

	_, usage, err := parseResponsesResponseMetadata(r)
	if err != nil {
		return
	}
	o.observeResponsesUsage(usage)
}

func (o *requestObservation) observeTokens(promptTokens, completionTokens int) {
	if o == nil || o.metrics == nil {
		return
	}

	provider, publicModel, _ := o.snapshot()
	if promptTokens > 0 {
		o.metrics.tokensTotal.WithLabelValues(provider, publicModel, "prompt").Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		o.metrics.tokensTotal.WithLabelValues(provider, publicModel, "completion").Add(float64(completionTokens))
	}
}

func (o *requestObservation) observeRetry(reason string) {
	if o == nil || o.metrics == nil {
		return
	}

	reason = normalizeMetricLabel(reason)
	if reason == "" {
		return
	}

	provider, publicModel, _ := o.snapshot()
	o.metrics.retriesTotal.WithLabelValues(provider, publicModel, reason).Inc()
}

func (o *requestObservation) observeUpstreamError(code string) {
	if o == nil || o.metrics == nil {
		return
	}

	code = normalizeMetricLabel(code)
	if code == "" {
		return
	}

	provider, publicModel, _ := o.snapshot()
	o.metrics.upstreamErrorsTotal.WithLabelValues(provider, publicModel, code).Inc()
}

func (o *requestObservation) observeFirstByte() {
	if o == nil || o.metrics == nil {
		return
	}

	o.firstByteOnce.Do(func() {
		provider, publicModel, endpoint := o.snapshot()
		o.metrics.requestFirstByte.WithLabelValues(provider, publicModel, endpoint).Observe(time.Since(o.start).Seconds())
	})
}

func (o *requestObservation) finish(statusCode int) {
	if o == nil || o.metrics == nil {
		return
	}

	o.mu.Lock()
	if o.finished {
		o.mu.Unlock()
		return
	}
	o.finished = true
	provider := o.provider
	publicModel := o.publicModel
	endpoint := o.endpoint
	inflight := o.inflight
	o.inflight = false
	o.mu.Unlock()

	if inflight && provider != "" {
		o.metrics.inflightRequests.WithLabelValues(provider).Dec()
	}

	status := requestStatusLabel(statusCode)
	o.metrics.requestsTotal.WithLabelValues(provider, publicModel, endpoint, status).Inc()
	o.metrics.requestDuration.WithLabelValues(provider, publicModel, endpoint, status).Observe(time.Since(o.start).Seconds())
}

func (o *requestObservation) snapshot() (provider, publicModel, endpoint string) {
	if o == nil {
		return "", "", ""
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	return o.provider, o.publicModel, o.endpoint
}

func (h *ProxyHandler) MetricsHandler() http.Handler {
	if h == nil || h.metrics == nil {
		return http.NotFoundHandler()
	}
	return h.metrics.handler()
}

func (h *ProxyHandler) recordProviderHealth(provider *providerRuntime, healthy bool) {
	if h == nil || h.metrics == nil || provider == nil {
		return
	}

	value := 0.0
	if healthy {
		value = 1
	}
	h.metrics.endpointHealthy.WithLabelValues(provider.id, providerHealthEndpoint(provider)).Set(value)
}

func providerHealthEndpoint(provider *providerRuntime) string {
	if provider == nil {
		return ""
	}
	if parsed, err := url.Parse(strings.TrimSpace(provider.baseURL)); err == nil {
		if host := normalizeMetricLabel(parsed.Hostname()); host != "" {
			return host
		}
	}
	if endpoint := normalizeMetricLabel(provider.id); endpoint != "" {
		return endpoint
	}
	return string(provider.kind)
}

func normalizeMetricLabel(value string) string {
	return strings.TrimSpace(value)
}

func normalizeKnownMetricModelLabel(value string) string {
	value = normalizeMetricLabel(value)
	if value == "" {
		return metricsPublicModelUnresolved
	}
	return value
}

var errExpectedJSONObject = errors.New("expected JSON object")

func parseOpenAIResponseMetadata(r io.Reader) (string, *models.OpenAIUsage, error) {
	var (
		model string
		usage *models.OpenAIUsage
	)

	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return "", nil, err
	}

	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return "", nil, errExpectedJSONObject
	}

	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return "", nil, err
		}

		key, ok := keyToken.(string)
		if !ok {
			return "", nil, errExpectedJSONObject
		}

		switch key {
		case "model":
			raw, err := decodeJSONRawValue(dec)
			if err != nil {
				return "", nil, err
			}
			_ = json.Unmarshal(raw, &model)
			model = strings.TrimSpace(model)
		case "usage":
			raw, err := decodeJSONRawValue(dec)
			if err != nil {
				return "", nil, err
			}
			_ = json.Unmarshal(raw, &usage)
		default:
			if err := skipJSONValue(dec); err != nil {
				return "", nil, err
			}
		}
	}

	if _, err := dec.Token(); err != nil {
		return "", nil, err
	}

	return model, usage, nil
}

func parseResponsesResponseMetadata(r io.Reader) (string, *responsesUsage, error) {
	var (
		model string
		usage *responsesUsage
	)

	dec := json.NewDecoder(r)
	tok, err := dec.Token()
	if err != nil {
		return "", nil, err
	}

	delim, ok := tok.(json.Delim)
	if !ok || delim != '{' {
		return "", nil, errExpectedJSONObject
	}

	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return "", nil, err
		}

		key, ok := keyToken.(string)
		if !ok {
			return "", nil, errExpectedJSONObject
		}

		switch key {
		case "model":
			raw, err := decodeJSONRawValue(dec)
			if err != nil {
				return "", nil, err
			}
			_ = json.Unmarshal(raw, &model)
			model = strings.TrimSpace(model)
		case "usage":
			raw, err := decodeJSONRawValue(dec)
			if err != nil {
				return "", nil, err
			}
			_ = json.Unmarshal(raw, &usage)
		default:
			if err := skipJSONValue(dec); err != nil {
				return "", nil, err
			}
		}
	}

	if _, err := dec.Token(); err != nil {
		return "", nil, err
	}

	return model, usage, nil
}

func decodeJSONRawValue(dec *json.Decoder) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return nil, err
	}
	return raw, nil
}

func requestStatusLabel(statusCode int) string {
	if statusCode <= 0 {
		return "unknown"
	}
	return strconv.Itoa(statusCode)
}

func retryReasonFromStatus(statusCode int) string {
	switch {
	case statusCode == http.StatusTooManyRequests:
		return "429"
	case statusCode >= 500 && statusCode <= 599:
		return "5xx"
	default:
		return strconv.Itoa(statusCode)
	}
}

func retryReasonFromError(err error) string {
	if isTimeoutError(err) {
		return "timeout"
	}
	return "transport"
}

func upstreamErrorCode(err error) string {
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) && upstreamErr.statusCode > 0 {
		return strconv.Itoa(upstreamErr.statusCode)
	}

	var providerErr *providerRequestError
	if errors.As(err, &providerErr) {
		if providerErr.statusCode > 0 {
			return strconv.Itoa(providerErr.statusCode)
		}
		return "provider"
	}

	if isTimeoutError(err) {
		return "timeout"
	}
	return "transport"
}

func responsesStreamErrorMetricCode(event responsesWebSocketStreamEvent) string {
	switch strings.TrimSpace(event.Type) {
	case "response.failed":
		if code := normalizeMetricLabel(event.Response.Error.Code); code != "" {
			return code
		}
		if errType := normalizeMetricLabel(event.Response.Error.Type); errType != "" {
			return errType
		}
		return "response.failed"
	case "response.incomplete":
		if reason := normalizeMetricLabel(event.Response.IncompleteDetails.Reason); reason != "" {
			return reason
		}
		return "response.incomplete"
	default:
		if eventType := normalizeMetricLabel(event.Type); eventType != "" {
			return eventType
		}
		return "stream_error"
	}
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

type requestObservationContextKey struct{}

type firstByteResponseWriter struct {
	http.ResponseWriter
	observer *requestObservation
}

func wrapFirstByteResponseWriter(w http.ResponseWriter, observer *requestObservation) http.ResponseWriter {
	if w == nil || observer == nil {
		return w
	}
	if _, ok := w.(*firstByteResponseWriter); ok {
		return w
	}
	return &firstByteResponseWriter{ResponseWriter: w, observer: observer}
}

func (w *firstByteResponseWriter) Write(p []byte) (int, error) {
	if w.observer != nil {
		w.observer.observeFirstByte()
	}
	return w.ResponseWriter.Write(p)
}

func (w *firstByteResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

type openAIStreamUsageTap struct {
	observer *requestObservation
	pending  []byte
}

func (t *openAIStreamUsageTap) Write(p []byte) (int, error) {
	if t == nil || t.observer == nil {
		return len(p), nil
	}

	t.pending = append(t.pending, p...)
	for {
		newline := bytes.IndexByte(t.pending, '\n')
		if newline < 0 {
			break
		}

		line := bytes.TrimRight(t.pending[:newline], "\r")
		t.pending = t.pending[newline+1:]

		data, ok := parseSSELine(string(line))
		if !ok || data == "" || data == "[DONE]" {
			continue
		}

		var chunk models.OpenAIStreamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		t.observer.observeOpenAIUsage(chunk.Usage)
	}

	if len(t.pending) > openAIStreamScannerMaxBuffer {
		t.pending = nil
	}

	return len(p), nil
}
