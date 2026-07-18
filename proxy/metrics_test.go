package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestMetricsCollector_Record(t *testing.T) {
	tests := []struct {
		name              string
		provider          string
		model             string
		wantModel         string
		endpoint          string
		status            int
		stream            bool
		upstreamAttempted bool
		prompt            int
		completion        int
		dur               time.Duration
		wantRequests      float64
		wantErrors        float64
		wantPrompt        float64
		wantCompletion    float64
	}{
		{
			name:           "successful non-streaming request",
			provider:       "copilot",
			model:          "gpt-4o",
			endpoint:       "openai_chat",
			status:         200,
			stream:         false,
			prompt:         100,
			completion:     50,
			dur:            500 * time.Millisecond,
			wantRequests:   1,
			wantErrors:     0,
			wantPrompt:     100,
			wantCompletion: 50,
		},
		{
			name:              "error request",
			provider:          "azure",
			model:             "gpt-4",
			endpoint:          "responses",
			status:            429,
			stream:            false,
			upstreamAttempted: true,
			prompt:            0,
			completion:        0,
			dur:               100 * time.Millisecond,
			wantRequests:      1,
			wantErrors:        1,
			wantPrompt:        0,
			wantCompletion:    0,
		},
		{
			name:           "local validation error is not an upstream error",
			provider:       "",
			model:          "bad-model",
			wantModel:      metricsUnroutedModel,
			endpoint:       "openai_chat",
			status:         400,
			stream:         false,
			dur:            10 * time.Millisecond,
			wantRequests:   1,
			wantErrors:     0,
			wantPrompt:     0,
			wantCompletion: 0,
		},
		{
			name:           "streaming request skips duration",
			provider:       "copilot",
			model:          "claude-sonnet-4",
			endpoint:       "anthropic",
			status:         200,
			stream:         true,
			prompt:         200,
			completion:     300,
			dur:            10 * time.Second,
			wantRequests:   1,
			wantErrors:     0,
			wantPrompt:     200,
			wantCompletion: 300,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := NewMetricsCollector()

			summary := &RequestSummary{}
			summary.mu.Lock()
			summary.provider = tt.provider
			summary.model = tt.model
			summary.endpoint = tt.endpoint
			summary.stream = tt.stream
			summary.upstreamAttempted = tt.upstreamAttempted
			summary.modelKnown = tt.provider != ""
			if tt.prompt > 0 {
				p := tt.prompt
				summary.promptTokens = &p
			}
			if tt.completion > 0 {
				c := tt.completion
				summary.completionTokens = &c
			}
			total := tt.prompt + tt.completion
			if total > 0 {
				summary.totalTokens = &total
			}
			summary.mu.Unlock()

			m.Record(summary, tt.status, tt.dur)

			wantModel := tt.wantModel
			if wantModel == "" {
				wantModel = tt.model
			}

			// Use the registry to gather all metrics
			metrics, err := m.registry.Gather()
			if err != nil {
				t.Fatalf("failed to gather metrics: %v", err)
			}

			gotRequests := getCounterValue(metrics, "vekil_requests_total", map[string]string{
				"provider":     tt.provider,
				"public_model": wantModel,
				"endpoint":     tt.endpoint,
			})
			if gotRequests != tt.wantRequests {
				t.Errorf("requests_total = %v, want %v", gotRequests, tt.wantRequests)
			}

			if tt.wantPrompt > 0 {
				gotPrompt := getCounterValue(metrics, "vekil_tokens_total", map[string]string{
					"provider":     tt.provider,
					"public_model": wantModel,
					"direction":    "prompt",
				})
				if gotPrompt != tt.wantPrompt {
					t.Errorf("tokens_total(prompt) = %v, want %v", gotPrompt, tt.wantPrompt)
				}
			}

			if tt.wantCompletion > 0 {
				gotCompletion := getCounterValue(metrics, "vekil_tokens_total", map[string]string{
					"provider":     tt.provider,
					"public_model": wantModel,
					"direction":    "completion",
				})
				if gotCompletion != tt.wantCompletion {
					t.Errorf("tokens_total(completion) = %v, want %v", gotCompletion, tt.wantCompletion)
				}
			}

			gotErrors := getCounterValue(metrics, "vekil_upstream_errors_total", map[string]string{
				"provider":     tt.provider,
				"public_model": wantModel,
			})
			if gotErrors != tt.wantErrors {
				t.Errorf("upstream_errors_total = %v, want %v", gotErrors, tt.wantErrors)
			}

			// Verify histogram has an observation for non-streaming requests
			if !tt.stream && tt.dur > 0 {
				gotHistCount := getHistogramCount(metrics, "vekil_request_duration_seconds", map[string]string{
					"provider":     tt.provider,
					"public_model": wantModel,
					"endpoint":     tt.endpoint,
				})
				if gotHistCount != 1 {
					t.Errorf("request_duration_seconds sample count = %v, want 1", gotHistCount)
				}
			}
			if tt.stream {
				gotHistCount := getHistogramCount(metrics, "vekil_request_duration_seconds", map[string]string{
					"provider":     tt.provider,
					"public_model": wantModel,
					"endpoint":     tt.endpoint,
				})
				if gotHistCount != 0 {
					t.Errorf("request_duration_seconds sample count = %v for streaming, want 0", gotHistCount)
				}
			}
		})
	}
}

func TestMetricsCollector_RecordResponsesTurn(t *testing.T) {
	m := NewMetricsCollector()

	record := m.RecordResponsesTurn("gpt-5.4", "azure", http.StatusTooManyRequests, responsesUsage{
		InputTokens:  12,
		OutputTokens: 3,
	})
	m.AddResponsesTurnUsage(record, responsesUsage{InputTokens: 5, OutputTokens: 2})

	metrics, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	labels := map[string]string{
		"provider":     "azure",
		"public_model": "gpt-5.4",
	}
	if got := getCounterValue(metrics, "vekil_requests_total", map[string]string{
		"provider":     labels["provider"],
		"public_model": labels["public_model"],
		"endpoint":     "responses_ws",
		"status":       "429",
	}); got != 1 {
		t.Fatalf("responses websocket requests = %v, want 1", got)
	}
	if got := getCounterValue(metrics, "vekil_tokens_total", map[string]string{
		"provider":     labels["provider"],
		"public_model": labels["public_model"],
		"direction":    "prompt",
	}); got != 17 {
		t.Errorf("prompt tokens = %v, want 17", got)
	}
	if got := getCounterValue(metrics, "vekil_tokens_total", map[string]string{
		"provider":     labels["provider"],
		"public_model": labels["public_model"],
		"direction":    "completion",
	}); got != 5 {
		t.Errorf("completion tokens = %v, want 5", got)
	}
	if got := getCounterValue(metrics, "vekil_upstream_errors_total", map[string]string{
		"provider":     labels["provider"],
		"public_model": labels["public_model"],
		"code":         "429",
	}); got != 1 {
		t.Errorf("upstream errors = %v, want 1", got)
	}
}

func TestMetricsCollector_UnroutedModelsDoNotConsumeBudget(t *testing.T) {
	m := NewMetricsCollector()
	for i := 0; i < statsMaxKeys+5; i++ {
		summary := &RequestSummary{}
		summary.setRoute("openai_chat", fmt.Sprintf("invalid-%03d", i), false)
		m.Record(summary, http.StatusBadRequest, time.Millisecond)
	}
	if got := len(m.modelLabels); got != 0 {
		t.Fatalf("unrouted requests consumed %d model budget entries, want 0", got)
	}

	valid := &RequestSummary{}
	valid.setRoute("openai_chat", "valid-after-invalid", false)
	valid.setProviderModel("copilot", "copilot", true, "valid-after-invalid")
	m.Record(valid, http.StatusOK, time.Millisecond)
	if _, ok := m.modelLabels["valid-after-invalid"]; !ok {
		t.Fatal("routed model was not admitted after invalid-model traffic")
	}

	metrics, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	if got := getCounterValue(metrics, "vekil_requests_total", map[string]string{
		"provider":     "",
		"public_model": metricsUnroutedModel,
		"endpoint":     "openai_chat",
		"status":       "400",
	}); got != statsMaxKeys+5 {
		t.Errorf("unrouted request count = %v, want %d", got, statsMaxKeys+5)
	}
	if got := getCounterValue(metrics, "vekil_requests_total", map[string]string{
		"provider":     "copilot",
		"public_model": "valid-after-invalid",
		"endpoint":     "openai_chat",
		"status":       "200",
	}); got != 1 {
		t.Errorf("routed request count = %v, want 1", got)
	}
}

func TestMetricsCollector_CanonicalizesKnownAnthropicAliases(t *testing.T) {
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithMetricsEnabled(false),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:       "alias-provider",
			Type:     "openai-compatible",
			Default:  true,
			BaseURL:  "http://upstream.test",
			AuthType: "none",
			Models: []ProviderModelConfig{{
				PublicID:  "claude-sonnet-4",
				Endpoints: []string{"/chat/completions"},
			}},
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	m := NewMetricsCollector()
	for i := 0; i < statsMaxKeys+5; i++ {
		rawModel := fmt.Sprintf("claude-sonnet-4-%08d", 20250000+i)
		ctx, summary := WithRequestSummary(context.Background())
		handler.observeRequestSummaryWithProviderModel(
			ctx,
			"anthropic",
			rawModel,
			NormalizeModelName(rawModel),
			false,
			providerEndpointChatCompletions,
		)
		d := readSummaryForStats(summary)
		if !d.modelKnown || d.metricModel != "claude-sonnet-4" {
			t.Fatalf("alias %q resolved to known=%t metric_model=%q", rawModel, d.modelKnown, d.metricModel)
		}
		m.Record(summary, http.StatusOK, time.Millisecond)
	}

	if got := len(m.modelLabels); got != 1 {
		t.Fatalf("canonical aliases consumed %d model budget entries, want 1", got)
	}
	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	if got := getCounterValue(families, "vekil_requests_total", map[string]string{
		"provider":     "alias-provider",
		"public_model": "claude-sonnet-4",
		"endpoint":     "anthropic",
		"status":       "200",
	}); got != statsMaxKeys+5 {
		t.Fatalf("canonical alias request count = %v, want %d", got, statsMaxKeys+5)
	}
}

func TestMetricsCollector_ModelCardinalityBoundedAcrossFamilies(t *testing.T) {
	m := NewMetricsCollector()
	for i := 0; i < statsMaxKeys+5; i++ {
		summary := &RequestSummary{}
		summary.setRoute("openai_chat", fmt.Sprintf("model-%03d", i), false)
		summary.setProviderModel("copilot", "copilot", true, fmt.Sprintf("model-%03d", i))
		m.Record(summary, http.StatusOK, time.Millisecond)
	}

	wsRecord := m.RecordResponsesTurn("websocket-overflow", "copilot", http.StatusOK, responsesUsage{})
	m.RecordRetry("copilot", "retry-overflow", http.StatusTooManyRequests)

	if got, want := wsRecord.model, statsOtherKey; got != want {
		t.Fatalf("websocket model label = %q, want %q", got, want)
	}
	metrics, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	if got, want := countMetricSeries(metrics, "vekil_requests_total"), statsMaxKeys+2; got != want {
		// 200 named HTTP models, one HTTP overflow series, and one websocket
		// overflow series (same model label, distinct endpoint/status set).
		t.Fatalf("request series = %d, want %d", got, want)
	}
	if got := getCounterValue(metrics, "vekil_requests_total", map[string]string{
		"provider":     "copilot",
		"public_model": statsOtherKey,
		"endpoint":     "openai_chat",
		"status":       "200",
	}); got != 5 {
		t.Errorf("HTTP overflow request count = %v, want 5", got)
	}
	if got := getCounterValue(metrics, "vekil_requests_total", map[string]string{
		"provider":     "copilot",
		"public_model": statsOtherKey,
		"endpoint":     "responses_ws",
		"status":       "200",
	}); got != 1 {
		t.Errorf("websocket overflow request count = %v, want 1", got)
	}
	if got := getCounterValue(metrics, "vekil_retries_total", map[string]string{
		"provider":     "copilot",
		"public_model": statsOtherKey,
		"reason":       "429",
	}); got != 1 {
		t.Errorf("retry overflow count = %v, want 1", got)
	}
}

func TestMetricsCollector_EscapesSyntheticModelLabelCollisions(t *testing.T) {
	m := NewMetricsCollector()
	tests := []struct {
		model string
		want  string
	}{
		{model: metricsUnroutedModel, want: metricsModelEscapePrefix + metricsUnroutedModel},
		{model: statsOtherKey, want: metricsModelEscapePrefix + statsOtherKey},
		{model: metricsModelEscapePrefix + statsOtherKey, want: metricsModelEscapePrefix + metricsModelEscapePrefix + statsOtherKey},
		{model: "gpt-5.4", want: "gpt-5.4"},
	}
	for _, tt := range tests {
		if got := m.modelLabel("provider", tt.model, true); got != tt.want {
			t.Errorf("modelLabel(%q) = %q, want %q", tt.model, got, tt.want)
		}
	}
	if got := m.modelLabel("provider", "anything", false); got != metricsUnroutedModel {
		t.Errorf("unknown model label = %q, want %q", got, metricsUnroutedModel)
	}
}

func TestMetricsCollector_WebSocketClientDisconnectIsNotUpstreamError(t *testing.T) {
	m := NewMetricsCollector()
	m.recordResponsesTurn("gpt-5.4", "copilot", 499, responsesUsage{}, true, true)

	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	if got := getCounterValue(families, "vekil_requests_total", map[string]string{
		"provider":     "copilot",
		"public_model": "gpt-5.4",
		"endpoint":     "responses_ws",
		"status":       "499",
	}); got != 1 {
		t.Fatalf("client-disconnect request metric = %v, want 1", got)
	}
	if got := getCounterValue(families, "vekil_upstream_errors_total", map[string]string{
		"provider":     "copilot",
		"public_model": "gpt-5.4",
		"code":         "499",
	}); got != 0 {
		t.Fatalf("client-disconnect upstream error metric = %v, want 0", got)
	}
}

func TestMetricsCollector_DurationBucketsCoverLongInference(t *testing.T) {
	m := NewMetricsCollector()
	summary := &RequestSummary{}
	summary.setRoute("openai_chat", "gpt-5.4", false)
	summary.setProviderModel("azure", "azure", true, "gpt-5.4")
	m.Record(summary, http.StatusOK, 2*time.Minute)

	metrics, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	labels := map[string]string{
		"provider":     "azure",
		"public_model": "gpt-5.4",
		"endpoint":     "openai_chat",
	}
	if got := getHistogramBucketCount(metrics, "vekil_request_duration_seconds", labels, 60); got != 0 {
		t.Errorf("60s bucket count = %d, want 0", got)
	}
	if got := getHistogramBucketCount(metrics, "vekil_request_duration_seconds", labels, 120); got != 1 {
		t.Errorf("120s bucket count = %d, want 1", got)
	}
	if got := getHistogramBucketCount(metrics, "vekil_request_duration_seconds", labels, 300); got != 1 {
		t.Errorf("300s bucket count = %d, want 1", got)
	}
}

func TestDoWithRetryMarksUpstreamAttemptForMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
	}))
	defer upstream.Close()

	h := &ProxyHandler{client: upstream.Client(), maxRetries: 1}
	ctx, summary := WithRequestSummary(context.Background())
	resp, err := h.doWithRetry(func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodPost, upstream.URL, nil)
	})
	if err != nil {
		t.Fatalf("doWithRetry() error = %v", err)
	}
	_ = resp.Body.Close()
	if got := readSummaryForStats(summary).upstreamAttempted; !got {
		t.Fatal("upstream attempt was not recorded on the request summary")
	}
}

func TestMetricsCollector_InflightGauge(t *testing.T) {
	m := NewMetricsCollector()

	m.IncInflight()
	m.IncInflight()
	m.IncInflight()

	metrics, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	inflight := getPlainGaugeValue(metrics, "vekil_inflight_requests")
	if inflight != 3 {
		t.Errorf("inflight = %v, want 3", inflight)
	}

	m.DecInflight()
	metrics, err = m.registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	inflight = getPlainGaugeValue(metrics, "vekil_inflight_requests")
	if inflight != 2 {
		t.Errorf("inflight after dec = %v, want 2", inflight)
	}
}

func TestMetricsCollector_RecordRetry(t *testing.T) {
	m := NewMetricsCollector()

	m.RecordRetry("copilot", "gpt-4o", 429)
	m.RecordRetry("copilot", "gpt-4o", 429)
	m.RecordRetry("copilot", "gpt-4o", 503)
	m.recordRetry("azure", "gpt-4", 0, context.DeadlineExceeded, true)
	m.recordRetry("copilot", "gpt-4o", 0, fmt.Errorf("connection reset"), true)

	metrics, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	got429 := getCounterValue(metrics, "vekil_retries_total", map[string]string{
		"provider":     "copilot",
		"public_model": "gpt-4o",
		"reason":       "429",
	})
	if got429 != 2 {
		t.Errorf("retries(429) = %v, want 2", got429)
	}

	got5xx := getCounterValue(metrics, "vekil_retries_total", map[string]string{
		"provider":     "copilot",
		"public_model": "gpt-4o",
		"reason":       "5xx",
	})
	if got5xx != 1 {
		t.Errorf("retries(5xx) = %v, want 1", got5xx)
	}

	gotTimeout := getCounterValue(metrics, "vekil_retries_total", map[string]string{
		"provider":     "azure",
		"public_model": "gpt-4",
		"reason":       "timeout",
	})
	if gotTimeout != 1 {
		t.Errorf("retries(timeout) = %v, want 1", gotTimeout)
	}

	gotTransport := getCounterValue(metrics, "vekil_retries_total", map[string]string{
		"provider":     "copilot",
		"public_model": "gpt-4o",
		"reason":       "transport",
	})
	if gotTransport != 1 {
		t.Errorf("retries(transport) = %v, want 1", gotTransport)
	}
}

func TestDoWithRetryKeepsFailingAttemptMetricLabels(t *testing.T) {
	m := NewMetricsCollector()
	h := &ProxyHandler{
		metrics:        m,
		maxRetries:     2,
		retryBaseDelay: time.Nanosecond,
	}
	h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		route, _ := req.Context().Value(providerRouteContextKey{}).(providerRouteInfo)
		status := http.StatusOK
		if route.id == "first-provider" {
			status = http.StatusServiceUnavailable
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    req,
		}, nil
	})}

	baseCtx := markRetryStatsTracked(context.Background())
	baseCtx = withRetryPublicModel(baseCtx, "gpt-5.4", true)
	attempt := 0
	resp, err := h.doWithRetry(func() (*http.Request, error) {
		attempt++
		provider := "first-provider"
		if attempt > 1 {
			provider = "second-provider"
		}
		ctx := context.WithValue(baseCtx, providerRouteContextKey{}, providerRouteInfo{id: provider, kind: "test"})
		return http.NewRequestWithContext(ctx, http.MethodPost, "http://upstream.test/responses", nil)
	})
	if err != nil {
		t.Fatalf("doWithRetry() error = %v", err)
	}
	_ = resp.Body.Close()

	metrics, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	if got := getCounterValue(metrics, "vekil_retries_total", map[string]string{
		"provider":     "first-provider",
		"public_model": "gpt-5.4",
		"reason":       "5xx",
	}); got != 1 {
		t.Fatalf("failing-attempt retry metric = %v, want 1", got)
	}
	if got := getCounterValue(metrics, "vekil_retries_total", map[string]string{
		"provider":     "second-provider",
		"public_model": "gpt-5.4",
		"reason":       "5xx",
	}); got != 0 {
		t.Fatalf("next-attempt retry metric = %v, want 0", got)
	}
}

func TestLogRetryAttemptRecordsResolvedMetricLabels(t *testing.T) {
	m := NewMetricsCollector()
	h := &ProxyHandler{metrics: m}
	ctx, summary := WithRequestSummary(context.Background())
	summary.setRoute("openai_chat", "gpt-5.4", false)
	summary.setProviderModel("azure-prod", "azure-openai", true, "gpt-5.4")
	ctx = markRetryStatsTracked(ctx)

	h.logRetryAttempt(ctx, 0, http.StatusServiceUnavailable, "", time.Second, nil)

	metrics, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}
	if got := getCounterValue(metrics, "vekil_retries_total", map[string]string{
		"provider":     "azure-prod",
		"public_model": "gpt-5.4",
		"reason":       "5xx",
	}); got != 1 {
		t.Fatalf("resolved retry metric = %v, want 1", got)
	}
	if got := getCounterValue(metrics, "vekil_retries_total", map[string]string{
		"provider":     "",
		"public_model": "",
		"reason":       "5xx",
	}); got != 0 {
		t.Fatalf("empty-label retry metric = %v, want 0", got)
	}
}

func TestMetricsCollector_BuildInfo(t *testing.T) {
	m := NewMetricsCollector()
	m.SetBuildInfo("1.2.3", "abc1234", "go1.22.0")

	metrics, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("failed to gather metrics: %v", err)
	}

	got := getGaugeValue(metrics, "vekil_build_info", map[string]string{
		"version":    "1.2.3",
		"commit":     "abc1234",
		"go_version": "go1.22.0",
	})
	if got != 1 {
		t.Errorf("build_info = %v, want 1", got)
	}
}

func TestMetricsCollector_NilSafety(t *testing.T) {
	var m *MetricsCollector

	// All methods must be nil-safe.
	m.Record(nil, 200, time.Second)
	record := m.RecordResponsesTurn("model", "provider", 200, responsesUsage{})
	m.AddResponsesTurnUsage(record, responsesUsage{})
	m.RecordRetry("p", "m", 429)
	m.IncInflight()
	m.DecInflight()
	m.SetBuildInfo("v", "c", "g")

	if m.Handler() != nil {
		t.Error("nil MetricsCollector.Handler() should return nil")
	}
}

// Helper functions to extract metric values from gathered metric families.

func countMetricSeries(families []*io_prometheus_client.MetricFamily, name string) int {
	for _, mf := range families {
		if mf.GetName() == name {
			return len(mf.GetMetric())
		}
	}
	return 0
}

func getHistogramBucketCount(families []*io_prometheus_client.MetricFamily, name string, labels map[string]string, upperBound float64) uint64 {
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, metric := range mf.GetMetric() {
			if !matchLabels(metric.GetLabel(), labels) {
				continue
			}
			for _, bucket := range metric.GetHistogram().GetBucket() {
				if bucket.GetUpperBound() == upperBound {
					return bucket.GetCumulativeCount()
				}
			}
		}
	}
	return 0
}

func getCounterValue(families []*io_prometheus_client.MetricFamily, name string, labels map[string]string) float64 {
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if matchLabels(m.GetLabel(), labels) {
				return m.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func getGaugeValue(families []*io_prometheus_client.MetricFamily, name string, labels map[string]string) float64 {
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if matchLabels(m.GetLabel(), labels) {
				return m.GetGauge().GetValue()
			}
		}
	}
	return 0
}

func getPlainGaugeValue(families []*io_prometheus_client.MetricFamily, name string) float64 {
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			return m.GetGauge().GetValue()
		}
	}
	return 0
}

func getHistogramCount(families []*io_prometheus_client.MetricFamily, name string, labels map[string]string) uint64 {
	for _, mf := range families {
		if mf.GetName() != name {
			continue
		}
		for _, m := range mf.GetMetric() {
			if matchLabels(m.GetLabel(), labels) {
				return m.GetHistogram().GetSampleCount()
			}
		}
	}
	return 0
}

func matchLabels(metricLabels []*io_prometheus_client.LabelPair, want map[string]string) bool {
	if len(want) == 0 {
		return true
	}
	labelMap := make(map[string]string, len(metricLabels))
	for _, lp := range metricLabels {
		labelMap[lp.GetName()] = lp.GetValue()
	}
	for k, v := range want {
		if labelMap[k] != v {
			return false
		}
	}
	return true
}

// Ensure prometheus package is used in test (compilation check).
var _ prometheus.Registerer = (*prometheus.Registry)(nil)

func TestDoWithRetryKeepsExplicitRouteModelLabel(t *testing.T) {
	m := NewMetricsCollector()
	provider := explicitRouteTestProvider("route-provider", "http://upstream.test", "key")
	h, route := explicitRouteTestHandler(t, nil, routeModePrimaryOnly, 1, 1, provider)
	h.metrics = m
	h.maxRetries = 2
	h.retryBaseDelay = time.Nanosecond

	attempt := 0
	h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		attempt++
		status := http.StatusServiceUnavailable
		if attempt > 1 {
			status = http.StatusOK
		}
		return &http.Response{
			StatusCode: status,
			Header:     make(http.Header),
			Body:       http.NoBody,
			Request:    req,
		}, nil
	})}

	ctx, summary := WithRequestSummary(markRetryStatsTracked(context.Background()))
	h.observeRequestSummary(ctx, "responses", route.public.id, false, providerEndpointResponses)
	if got := readSummaryForStats(summary); !got.modelKnown || got.metricModel != route.public.id {
		t.Fatalf("explicit route summary = known:%t model:%q, want true/%q", got.modelKnown, got.metricModel, route.public.id)
	}

	target, ok := route.primaryTarget()
	if !ok {
		t.Fatal("explicit route has no primary target")
	}
	owner := providerModelFromRouteTarget(route, target)
	resp, err := h.doWithRetry(func() (*http.Request, error) {
		return h.newProviderJSONRequest(ctx, provider, http.MethodPost, providerEndpointResponses, []byte(`{"model":"deployment-a"}`), nil, "", owner)
	})
	if err != nil {
		t.Fatalf("doWithRetry() error = %v", err)
	}
	_ = resp.Body.Close()

	families, err := m.registry.Gather()
	if err != nil {
		t.Fatalf("gather metrics: %v", err)
	}
	labels := map[string]string{
		"provider":     provider.id,
		"public_model": route.public.id,
		"reason":       "5xx",
	}
	if got := getCounterValue(families, "vekil_retries_total", labels); got != 1 {
		t.Fatalf("explicit route retry metric = %v, want 1", got)
	}
	labels["public_model"] = metricsUnroutedModel
	if got := getCounterValue(families, "vekil_retries_total", labels); got != 0 {
		t.Fatalf("unrouted explicit route retry metric = %v, want 0", got)
	}
}

func TestChatMetricsPromoteColdDiscoveredModel(t *testing.T) {
	tests := []struct {
		name              string
		model             string
		catalogModels     string
		upstreamStatus    int
		wantMetricModel   string
		wantUpstreamError float64
	}{
		{
			name:            "discovered success",
			model:           "dynamic-model",
			catalogModels:   `{"object":"list","data":[{"id":"dynamic-model","supported_endpoints":["/chat/completions"]}]}`,
			upstreamStatus:  http.StatusOK,
			wantMetricModel: "dynamic-model",
		},
		{
			name:              "discovered upstream failure",
			model:             "dynamic-model",
			catalogModels:     `{"object":"list","data":[{"id":"dynamic-model","supported_endpoints":["/chat/completions"]}]}`,
			upstreamStatus:    http.StatusServiceUnavailable,
			wantMetricModel:   "dynamic-model",
			wantUpstreamError: 1,
		},
		{
			name:            "undiscovered model stays unrouted",
			model:           "client-controlled-unknown",
			catalogModels:   `{"object":"list","data":[]}`,
			upstreamStatus:  http.StatusOK,
			wantMetricModel: metricsUnroutedModel,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var modelHits atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case providerEndpointModels:
					modelHits.Add(1)
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, tt.catalogModels)
				case providerEndpointChatCompletions:
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(tt.upstreamStatus)
					if tt.upstreamStatus == http.StatusOK {
						_, _ = io.WriteString(w, `{"id":"chatcmpl-dynamic","object":"chat.completion","model":"`+tt.model+`","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
					} else {
						_, _ = io.WriteString(w, `{"error":{"message":"unavailable","type":"server_error"}}`)
					}
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()

			handler, err := NewProxyHandler(
				auth.NewTestAuthenticator("fixture"),
				logger.New(logger.LevelError),
				WithDeferredDynamicProviderModelValidation(true),
				WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
					ID:             "dynamic-provider",
					Type:           "openai-compatible",
					Default:        true,
					BaseURL:        upstream.URL,
					AuthType:       "none",
					ModelDiscovery: "openai",
				}}}),
			)
			if err != nil {
				t.Fatalf("NewProxyHandler() error = %v", err)
			}
			defer handler.BeginShutdown()
			handler.maxRetries = 1

			ctx, summary := WithRequestSummary(context.Background())
			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+tt.model+`","messages":[{"role":"user","content":"hello"}]}`)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			recorder := httptest.NewRecorder()
			handler.HandleOpenAIChatCompletions(recorder, req)
			status := recorder.Code
			if status != tt.upstreamStatus {
				t.Fatalf("response status = %d, want %d; body=%s", status, tt.upstreamStatus, recorder.Body.String())
			}
			handler.RecordRequest(summary, status, "test-client", time.Millisecond)

			if got := modelHits.Load(); got != 1 {
				t.Fatalf("model discovery hits = %d, want 1", got)
			}
			stats := readSummaryForStats(summary)
			wantKnown := tt.wantMetricModel != metricsUnroutedModel
			if stats.modelKnown != wantKnown {
				t.Fatalf("summary modelKnown = %t, want %t (metric model %q)", stats.modelKnown, wantKnown, stats.metricModel)
			}
			if wantKnown && stats.metricModel != tt.wantMetricModel {
				t.Fatalf("summary metric model = %q, want %q", stats.metricModel, tt.wantMetricModel)
			}

			families, err := handler.metrics.registry.Gather()
			if err != nil {
				t.Fatalf("gather metrics: %v", err)
			}
			labels := map[string]string{
				"provider":     "dynamic-provider",
				"public_model": tt.wantMetricModel,
				"endpoint":     "openai_chat",
				"status":       strconv.Itoa(tt.upstreamStatus),
			}
			if got := getCounterValue(families, "vekil_requests_total", labels); got != 1 {
				t.Fatalf("request metric = %v, want 1 for labels %+v", got, labels)
			}
			if tt.wantMetricModel != metricsUnroutedModel {
				labels["public_model"] = metricsUnroutedModel
				if got := getCounterValue(families, "vekil_requests_total", labels); got != 0 {
					t.Fatalf("unrouted request metric = %v, want 0", got)
				}
			} else {
				labels["public_model"] = tt.model
				if got := getCounterValue(families, "vekil_requests_total", labels); got != 0 {
					t.Fatalf("client-controlled model metric = %v, want 0", got)
				}
			}

			errorLabels := map[string]string{
				"provider":     "dynamic-provider",
				"public_model": tt.wantMetricModel,
				"code":         strconv.Itoa(tt.upstreamStatus),
			}
			if got := getCounterValue(families, "vekil_upstream_errors_total", errorLabels); got != tt.wantUpstreamError {
				t.Fatalf("upstream error metric = %v, want %v for labels %+v", got, tt.wantUpstreamError, errorLabels)
			}
		})
	}
}
