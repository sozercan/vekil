package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/sozercan/vekil/models"
)

const unknownMetricsLabel = "unknown"

// BuildInfo labels the build_info metric and is populated from injected
// build metadata when available, then from Go's embedded build info.
type BuildInfo struct {
	Version   string
	GoVersion string
	Commit    string
}

// MetricsConfig controls Prometheus metrics exposure.
type MetricsConfig struct {
	Enabled   bool
	BuildInfo BuildInfo
}

// DefaultMetricsConfig enables metrics on /metrics by default.
func DefaultMetricsConfig() MetricsConfig {
	return MetricsConfig{Enabled: true}
}

// WithMetricsConfig configures Prometheus metrics exposure and build labels.
func WithMetricsConfig(cfg MetricsConfig) Option {
	return func(h *ProxyHandler) {
		h.metricsConfig = cfg
	}
}

type proxyMetrics struct {
	enabled                bool
	registry               *prometheus.Registry
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

type requestMetrics struct {
	metrics        *proxyMetrics
	start          time.Time
	endpoint       string
	provider       string
	publicModel    string
	streaming      bool
	inflight       bool
	inflightLabel  string
	openAIUsage    *models.OpenAIUsage
	responsesUsage *responsesTokenUsage
	finishOnce     sync.Once
}

type metricsResponseWriter struct {
	http.ResponseWriter
	statusCode  int
	wroteHeader bool
	firstWrite  time.Time
}

type upstreamRequestMetrics struct {
	metrics     *proxyMetrics
	provider    string
	publicModel string
}

type responsesTokenUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

type openAIStreamUsageTap struct {
	pending []byte
	onUsage func(*models.OpenAIUsage)
}

func newProxyMetrics(cfg MetricsConfig) (*proxyMetrics, error) {
	if !cfg.Enabled {
		return &proxyMetrics{}, nil
	}

	m := &proxyMetrics{
		enabled:  true,
		registry: prometheus.NewRegistry(),
		requestsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_requests_total",
			Help: "Total number of proxied inference requests.",
		}, []string{"provider", "public_model", "endpoint", "status"}),
		requestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vekil_request_duration_seconds",
			Help:    "End-to-end proxied request duration in seconds.",
			Buckets: prometheus.DefBuckets,
		}, []string{"provider", "public_model", "endpoint", "status"}),
		streamFirstByteLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "vekil_stream_first_byte_latency_seconds",
			Help:    "Latency until the first streamed response byte is written to the client.",
			Buckets: prometheus.DefBuckets,
		}, []string{"provider", "public_model", "endpoint"}),
		tokensTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_tokens_total",
			Help: "Token usage reported by upstream providers.",
		}, []string{"provider", "public_model", "direction"}),
		retriesTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_retries_total",
			Help: "Retries attempted against upstream providers.",
		}, []string{"provider", "public_model", "reason"}),
		upstreamErrorsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "vekil_upstream_errors_total",
			Help: "Upstream HTTP and timeout errors returned by providers.",
		}, []string{"provider", "public_model", "code"}),
		inflightRequests: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vekil_inflight_requests",
			Help: "Requests currently in flight, partitioned by provider.",
		}, []string{"provider"}),
		endpointHealthy: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vekil_endpoint_healthy",
			Help: "Latest readiness-probe result for provider endpoints.",
		}, []string{"provider", "endpoint"}),
		buildInfo: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "vekil_build_info",
			Help: "Build metadata for this Vekil binary.",
		}, []string{"version", "go_version", "commit"}),
	}

	collectorsToRegister := []prometheus.Collector{
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		collectors.NewGoCollector(),
		m.requestsTotal,
		m.requestDuration,
		m.streamFirstByteLatency,
		m.tokensTotal,
		m.retriesTotal,
		m.upstreamErrorsTotal,
		m.inflightRequests,
		m.endpointHealthy,
		m.buildInfo,
	}
	for _, collector := range collectorsToRegister {
		if err := m.registry.Register(collector); err != nil {
			return nil, fmt.Errorf("register prometheus collector: %w", err)
		}
	}

	build := normalizedBuildInfo(cfg.BuildInfo)
	m.buildInfo.WithLabelValues(build.Version, build.GoVersion, build.Commit).Set(1)

	return m, nil
}

func normalizedBuildInfo(info BuildInfo) BuildInfo {
	info.Version = strings.TrimSpace(info.Version)
	info.GoVersion = strings.TrimSpace(info.GoVersion)
	info.Commit = strings.TrimSpace(info.Commit)

	if embedded, ok := debug.ReadBuildInfo(); ok {
		if info.GoVersion == "" {
			info.GoVersion = strings.TrimSpace(embedded.GoVersion)
		}
		if info.Version == "" {
			version := strings.TrimSpace(embedded.Main.Version)
			if version != "" && version != "(devel)" {
				info.Version = version
			}
		}
		if info.Commit == "" {
			for _, setting := range embedded.Settings {
				if setting.Key == "vcs.revision" {
					info.Commit = strings.TrimSpace(setting.Value)
					break
				}
			}
		}
	}

	if info.Version == "" {
		info.Version = "dev"
	}
	if info.GoVersion == "" {
		info.GoVersion = runtime.Version()
	}
	if info.Commit == "" {
		info.Commit = "unknown"
	}
	return info
}

func (m *proxyMetrics) handler() http.Handler {
	if m == nil || !m.enabled || m.registry == nil {
		return http.NotFoundHandler()
	}
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *proxyMetrics) observeRetry(provider, publicModel, reason string) {
	if m == nil || !m.enabled || reason == "" {
		return
	}
	m.retriesTotal.WithLabelValues(sanitizeMetricsLabel(provider), sanitizeMetricsLabel(publicModel), sanitizeMetricsLabel(reason)).Inc()
}

func (m *proxyMetrics) observeUpstreamError(provider, publicModel, code string) {
	if m == nil || !m.enabled || code == "" {
		return
	}
	m.upstreamErrorsTotal.WithLabelValues(sanitizeMetricsLabel(provider), sanitizeMetricsLabel(publicModel), sanitizeMetricsLabel(code)).Inc()
}

func (m *proxyMetrics) setEndpointHealthy(provider, endpoint string, healthy bool) {
	if m == nil || !m.enabled {
		return
	}
	value := 0.0
	if healthy {
		value = 1
	}
	m.endpointHealthy.WithLabelValues(sanitizeMetricsLabel(provider), sanitizeMetricsLabel(endpoint)).Set(value)
}

func sanitizeMetricsLabel(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return unknownMetricsLabel
	}
	return value
}

func (h *ProxyHandler) newRequestMetrics(endpoint string) *requestMetrics {
	return &requestMetrics{
		metrics:  h.metrics,
		start:    time.Now(),
		endpoint: endpoint,
	}
}

func (r *requestMetrics) setEndpoint(endpoint string) {
	if r == nil {
		return
	}
	r.endpoint = strings.TrimSpace(endpoint)
}

func (r *requestMetrics) setRouting(h *ProxyHandler, publicModel, upstreamEndpoint string, streaming bool) {
	if r == nil {
		return
	}

	r.publicModel = strings.TrimSpace(publicModel)
	r.streaming = streaming

	providerID := unknownMetricsLabel
	if h != nil {
		provider, _, _ := h.resolveProviderModel(publicModel, upstreamEndpoint)
		if provider != nil {
			providerID = provider.id
		}
	}
	r.setProvider(providerID)
}

func (r *requestMetrics) setProvider(provider string) {
	if r == nil {
		return
	}

	provider = sanitizeMetricsLabel(provider)
	r.provider = provider
	if r.metrics == nil || !r.metrics.enabled {
		return
	}

	if r.inflight {
		if r.inflightLabel == provider {
			return
		}
		r.metrics.inflightRequests.WithLabelValues(r.inflightLabel).Dec()
	}

	r.metrics.inflightRequests.WithLabelValues(provider).Inc()
	r.inflight = true
	r.inflightLabel = provider
}

func (r *requestMetrics) setOpenAIUsage(usage *models.OpenAIUsage) {
	if r == nil || usage == nil {
		return
	}
	copy := *usage
	r.openAIUsage = &copy
}

func (r *requestMetrics) setResponsesUsage(usage *responsesTokenUsage) {
	if r == nil || usage == nil {
		return
	}
	copy := *usage
	r.responsesUsage = &copy
}

func (r *requestMetrics) finish(w *metricsResponseWriter) {
	if r == nil {
		return
	}

	r.finishOnce.Do(func() {
		if r.metrics == nil || !r.metrics.enabled {
			return
		}

		provider := sanitizeMetricsLabel(r.provider)
		publicModel := sanitizeMetricsLabel(r.publicModel)
		endpoint := sanitizeMetricsLabel(r.endpoint)
		status := strconv.Itoa(http.StatusOK)
		if w != nil {
			status = strconv.Itoa(w.StatusCode())
		}

		r.metrics.requestsTotal.WithLabelValues(provider, publicModel, endpoint, status).Inc()
		r.metrics.requestDuration.WithLabelValues(provider, publicModel, endpoint, status).Observe(time.Since(r.start).Seconds())
		if r.streaming && w != nil {
			if latency, ok := w.firstByteLatency(r.start); ok {
				r.metrics.streamFirstByteLatency.WithLabelValues(provider, publicModel, endpoint).Observe(latency.Seconds())
			}
		}

		if r.openAIUsage != nil {
			r.metrics.tokensTotal.WithLabelValues(provider, publicModel, "prompt").Add(float64(r.openAIUsage.PromptTokens))
			r.metrics.tokensTotal.WithLabelValues(provider, publicModel, "completion").Add(float64(r.openAIUsage.CompletionTokens))
		} else if r.responsesUsage != nil {
			r.metrics.tokensTotal.WithLabelValues(provider, publicModel, "prompt").Add(float64(r.responsesUsage.InputTokens))
			r.metrics.tokensTotal.WithLabelValues(provider, publicModel, "completion").Add(float64(r.responsesUsage.OutputTokens))
		}

		if r.inflight {
			r.metrics.inflightRequests.WithLabelValues(r.inflightLabel).Dec()
			r.inflight = false
		}
	})
}

func newMetricsResponseWriter(w http.ResponseWriter) *metricsResponseWriter {
	if existing, ok := w.(*metricsResponseWriter); ok {
		return existing
	}
	return &metricsResponseWriter{ResponseWriter: w}
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
	if w.firstWrite.IsZero() {
		w.firstWrite = time.Now()
	}
	return w.ResponseWriter.Write(p)
}

func (w *metricsResponseWriter) StatusCode() int {
	if w == nil || !w.wroteHeader {
		return http.StatusOK
	}
	return w.statusCode
}

func (w *metricsResponseWriter) firstByteLatency(start time.Time) (time.Duration, bool) {
	if w == nil || w.firstWrite.IsZero() {
		return 0, false
	}
	return w.firstWrite.Sub(start), true
}

func (w *metricsResponseWriter) Flush() {
	if flusher, ok := w.ResponseWriter.(http.Flusher); ok {
		flusher.Flush()
	}
}

func (h *ProxyHandler) newUpstreamRequestMetrics(provider *providerRuntime, publicModel string) *upstreamRequestMetrics {
	if h == nil || h.metrics == nil || !h.metrics.enabled {
		return nil
	}
	providerID := unknownMetricsLabel
	if provider != nil {
		providerID = provider.id
	}
	return &upstreamRequestMetrics{
		metrics:     h.metrics,
		provider:    providerID,
		publicModel: publicModel,
	}
}

func (m *upstreamRequestMetrics) observeRetry(reason string) {
	if m == nil {
		return
	}
	m.metrics.observeRetry(m.provider, m.publicModel, reason)
}

func (m *upstreamRequestMetrics) observeUpstreamError(code string) {
	if m == nil {
		return
	}
	m.metrics.observeUpstreamError(m.provider, m.publicModel, code)
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

func extractResponsesUsageFromBody(body []byte) *responsesTokenUsage {
	var payload struct {
		Usage *responsesTokenUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	return payload.Usage
}

func newOpenAIStreamUsageTap(onUsage func(*models.OpenAIUsage)) *openAIStreamUsageTap {
	if onUsage == nil {
		return nil
	}
	return &openAIStreamUsageTap{onUsage: onUsage}
}

func (t *openAIStreamUsageTap) Write(p []byte) (int, error) {
	if t == nil || t.onUsage == nil {
		return len(p), nil
	}

	t.pending = append(t.pending, p...)
	for {
		idx := bytes.IndexByte(t.pending, '\n')
		if idx < 0 {
			break
		}
		line := t.pending[:idx]
		t.pending = t.pending[idx+1:]
		if len(line) > 0 && line[len(line)-1] == '\r' {
			line = line[:len(line)-1]
		}
		t.maybeObserveLine(string(line))
	}

	if len(t.pending) > openAIStreamScannerMaxBuffer {
		t.pending = nil
	}

	return len(p), nil
}

func (t *openAIStreamUsageTap) maybeObserveLine(line string) {
	data, ok := parseSSELine(line)
	if !ok || data == "[DONE]" {
		return
	}

	var chunk models.OpenAIStreamChunk
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return
	}
	if chunk.Usage != nil {
		t.onUsage(chunk.Usage)
	}
}
