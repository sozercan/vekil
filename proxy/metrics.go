package proxy

import (
	"encoding/json"
	"io"
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

// BuildInfo carries optional build metadata that can be injected via ldflags.
type BuildInfo struct {
	Version string
	Commit  string
}

type metricsRegistry struct {
	handler            http.Handler
	requests           *prometheus.CounterVec
	requestDuration    *prometheus.HistogramVec
	firstByteDuration  *prometheus.HistogramVec
	tokens             *prometheus.CounterVec
	retries            *prometheus.CounterVec
	upstreamErrors     *prometheus.CounterVec
	inflight           *prometheus.GaugeVec
	endpointHealthy    *prometheus.GaugeVec
}

type requestObservation struct {
	metrics     *metricsRegistry
	endpoint    string
	publicModel string
	provider    string
	started     time.Time
	firstByte   time.Duration
	hasFirstByte bool
	inflightSet bool
	firstByteOnce sync.Once
	finishOnce sync.Once
}

func newMetricsRegistry(info BuildInfo) *metricsRegistry {
	registry := prometheus.NewRegistry()

	m := &metricsRegistry{
		requests: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vekil_requests_total",
				Help: "Total proxy requests by provider, public model, endpoint, and final status.",
			},
			[]string{"provider", "public_model", "endpoint", "status"},
		),
		requestDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "vekil_request_duration_seconds",
				Help:    "End-to-end proxy request duration in seconds.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"provider", "public_model", "endpoint", "status"},
		),
		firstByteDuration: prometheus.NewHistogramVec(
			prometheus.HistogramOpts{
				Name:    "vekil_request_first_byte_duration_seconds",
				Help:    "Time to first upstream semantic streaming event in seconds.",
				Buckets: prometheus.DefBuckets,
			},
			[]string{"provider", "public_model", "endpoint", "status"},
		),
		tokens: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vekil_tokens_total",
				Help: "Token usage parsed from upstream usage blocks.",
			},
			[]string{"provider", "public_model", "direction"},
		),
		retries: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vekil_retries_total",
				Help: "Retries attempted for upstream requests.",
			},
			[]string{"provider", "public_model", "reason"},
		),
		upstreamErrors: prometheus.NewCounterVec(
			prometheus.CounterOpts{
				Name: "vekil_upstream_errors_total",
				Help: "Upstream errors observed before retry or final response handling.",
			},
			[]string{"provider", "public_model", "code"},
		),
		inflight: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "vekil_inflight_requests",
				Help: "Requests currently in flight by provider.",
			},
			[]string{"provider"},
		),
		endpointHealthy: prometheus.NewGaugeVec(
			prometheus.GaugeOpts{
				Name: "vekil_endpoint_healthy",
				Help: "Latest provider probe health for a specific upstream endpoint label.",
			},
			[]string{"provider", "endpoint"},
		),
	}

	buildInfo := prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "vekil_build_info",
			Help: "Build metadata for this Vekil binary.",
		},
		[]string{"version", "go_version", "commit"},
	)

	resolvedInfo := info.withDefaults()
	buildInfo.WithLabelValues(resolvedInfo.Version, runtime.Version(), resolvedInfo.Commit).Set(1)

	registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		m.requests,
		m.requestDuration,
		m.firstByteDuration,
		m.tokens,
		m.retries,
		m.upstreamErrors,
		m.inflight,
		m.endpointHealthy,
		buildInfo,
	)

	m.handler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	return m
}

func (i BuildInfo) withDefaults() BuildInfo {
	if info, ok := debug.ReadBuildInfo(); ok {
		if strings.TrimSpace(i.Version) == "" {
			version := strings.TrimSpace(info.Main.Version)
			if version != "" && version != "(devel)" {
				i.Version = version
			}
		}
		if strings.TrimSpace(i.Commit) == "" {
			for _, setting := range info.Settings {
				if setting.Key == "vcs.revision" {
					i.Commit = strings.TrimSpace(setting.Value)
					break
				}
			}
		}
	}
	if strings.TrimSpace(i.Version) == "" {
		i.Version = "dev"
	}
	if strings.TrimSpace(i.Commit) == "" {
		i.Commit = "unknown"
	}
	return i
}

func (m *metricsRegistry) Handler() http.Handler {
	if m == nil {
		return nil
	}
	return m.handler
}

func (m *metricsRegistry) beginRequest(endpoint, publicModel string) *requestObservation {
	if m == nil {
		return nil
	}
	return &requestObservation{
		metrics:     m,
		endpoint:    strings.TrimSpace(endpoint),
		publicModel: strings.TrimSpace(publicModel),
		started:     time.Now(),
	}
}

func (o *requestObservation) setPublicModel(publicModel string) {
	if o == nil {
		return
	}
	publicModel = strings.TrimSpace(publicModel)
	if publicModel == "" {
		return
	}
	o.publicModel = publicModel
}

func (o *requestObservation) setProvider(provider string) {
	if o == nil || o.metrics == nil {
		return
	}
	provider = metricLabelValue(provider, "unknown")
	if o.provider == provider {
		if !o.inflightSet {
			o.metrics.inflight.WithLabelValues(provider).Inc()
			o.inflightSet = true
		}
		return
	}
	if o.inflightSet {
		o.metrics.inflight.WithLabelValues(metricLabelValue(o.provider, "unknown")).Dec()
	}
	o.provider = provider
	o.metrics.inflight.WithLabelValues(provider).Inc()
	o.inflightSet = true
}

func (o *requestObservation) observeFirstByte() {
	if o == nil {
		return
	}
	o.firstByteOnce.Do(func() {
		o.firstByte = time.Since(o.started)
		o.hasFirstByte = true
	})
}

func (o *requestObservation) observeOpenAIUsage(usage *models.OpenAIUsage) {
	if o == nil || usage == nil {
		return
	}
	o.observeTokens(usage.PromptTokens, usage.CompletionTokens)
}

func (o *requestObservation) observeResponsesUsage(usage *responsesStreamUsage) {
	if o == nil || usage == nil {
		return
	}
	o.observeTokens(usage.InputTokens, usage.OutputTokens)
}

func (o *requestObservation) observeTokens(promptTokens, completionTokens int) {
	if o == nil || o.metrics == nil {
		return
	}
	provider := metricLabelValue(o.provider, "unknown")
	publicModel := metricLabelValue(o.publicModel, "unknown")
	if promptTokens > 0 {
		o.metrics.tokens.WithLabelValues(provider, publicModel, "prompt").Add(float64(promptTokens))
	}
	if completionTokens > 0 {
		o.metrics.tokens.WithLabelValues(provider, publicModel, "completion").Add(float64(completionTokens))
	}
}

func (o *requestObservation) observeRetry(reason string) {
	if o == nil || o.metrics == nil {
		return
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return
	}
	o.metrics.retries.WithLabelValues(
		metricLabelValue(o.provider, "unknown"),
		metricLabelValue(o.publicModel, "unknown"),
		reason,
	).Inc()
}

func (o *requestObservation) observeUpstreamError(code string) {
	if o == nil || o.metrics == nil {
		return
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return
	}
	o.metrics.upstreamErrors.WithLabelValues(
		metricLabelValue(o.provider, "unknown"),
		metricLabelValue(o.publicModel, "unknown"),
		code,
	).Inc()
}

func (o *requestObservation) finish(status int) {
	if o == nil || o.metrics == nil {
		return
	}
	statusLabel := strconv.Itoa(status)
	if status <= 0 {
		statusLabel = "0"
	}
	o.finishOnce.Do(func() {
		provider := metricLabelValue(o.provider, "unknown")
		publicModel := metricLabelValue(o.publicModel, "unknown")
		endpoint := metricLabelValue(o.endpoint, "unknown")
		o.metrics.requests.WithLabelValues(provider, publicModel, endpoint, statusLabel).Inc()
		o.metrics.requestDuration.WithLabelValues(provider, publicModel, endpoint, statusLabel).Observe(time.Since(o.started).Seconds())
		if o.hasFirstByte {
			o.metrics.firstByteDuration.WithLabelValues(provider, publicModel, endpoint, statusLabel).Observe(o.firstByte.Seconds())
		}
		if o.inflightSet {
			o.metrics.inflight.WithLabelValues(provider).Dec()
			o.inflightSet = false
		}
	})
}

func metricLabelValue(value, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return fallback
	}
	return value
}

func writeObservedJSONUpstreamResponse(w http.ResponseWriter, resp *http.Response, observeUsage func([]byte)) error {
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	copyPassthroughHeaders(w.Header(), resp.Header)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
	if resp.StatusCode == http.StatusOK && observeUsage != nil {
		observeUsage(body)
	}
	return nil
}

func writeObservedOpenAIJSONResponse(w http.ResponseWriter, resp *http.Response, obs *requestObservation) error {
	return writeObservedJSONUpstreamResponse(w, resp, func(body []byte) {
		obs.observeOpenAIUsage(extractOpenAIUsageFromBody(body))
	})
}

func writeObservedResponsesJSONResponse(w http.ResponseWriter, resp *http.Response, obs *requestObservation) error {
	return writeObservedJSONUpstreamResponse(w, resp, func(body []byte) {
		obs.observeResponsesUsage(extractResponsesUsageFromBody(body))
	})
}

func extractOpenAIUsageFromBody(body []byte) *models.OpenAIUsage {
	var payload struct {
		Usage *models.OpenAIUsage `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	return payload.Usage
}

func extractResponsesUsageFromBody(body []byte) *responsesStreamUsage {
	var payload struct {
		Usage *responsesStreamUsage `json:"usage,omitempty"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil
	}
	return payload.Usage
}
