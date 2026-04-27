package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
)

func histogramSampleCount(t *testing.T, observer prometheus.Observer) uint64 {
	t.Helper()
	collector, ok := observer.(interface{ Write(*dto.Metric) error })
	if !ok {
		t.Fatal("observer does not support metric collection")
	}
	metric := &dto.Metric{}
	if err := collector.Write(metric); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if metric.Histogram == nil {
		t.Fatal("expected histogram metric")
	}
	return metric.Histogram.GetSampleCount()
}

func registerKnownMetricsTestModels(t *testing.T, handler *ProxyHandler, models ...string) {
	t.Helper()

	setup := defaultProviderSetup(handler)
	knownModels := make([]providerModel, 0, len(models))
	for _, model := range models {
		knownModels = append(knownModels, providerModel{
			publicID:      model,
			upstreamModel: model,
			providerID:    "copilot",
		})
	}
	if err := setup.replaceProviderModels("copilot", knownModels); err != nil {
		t.Fatalf("replaceProviderModels() error = %v", err)
	}
	handler.providersState = setup
}

func TestHandleOpenAIChatCompletions_RecordsRequestAndTokenMetrics(t *testing.T) {
	metrics, err := NewMetrics("test-version")
	if err != nil {
		t.Fatalf("NewMetrics() error = %v", err)
	}

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"model":"gpt-4.1",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}
		}`)
	})
	handler.metrics = metrics
	registerKnownMetricsTestModels(t, handler, "gpt-4.1")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
		"model":"gpt-4.1",
		"messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(rec, req)

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
	if got := testutil.ToFloat64(metrics.requestsTotal.WithLabelValues("chat_completions", "copilot", "gpt-4.1", "200")); got != 1 {
		t.Fatalf("vekil_requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.tokensTotal.WithLabelValues("chat_completions", "copilot", "gpt-4.1", tokenDirectionPrompt)); got != 11 {
		t.Fatalf("prompt tokens = %v, want 11", got)
	}
	if got := testutil.ToFloat64(metrics.tokensTotal.WithLabelValues("chat_completions", "copilot", "gpt-4.1", tokenDirectionCompletion)); got != 7 {
		t.Fatalf("completion tokens = %v, want 7", got)
	}
}

func TestHandleResponses_RecordsRequestAndTokenMetrics(t *testing.T) {
	metrics, err := NewMetrics("test-version")
	if err != nil {
		t.Fatalf("NewMetrics() error = %v", err)
	}

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"resp-test",
			"object":"response",
			"status":"completed",
			"model":"gpt-5",
			"output":[],
			"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}
		}`)
	})
	handler.metrics = metrics
	registerKnownMetricsTestModels(t, handler, "gpt-5")

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"gpt-5",
		"input":"hello"
	}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.HandleResponses(rec, req)

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
	if got := testutil.ToFloat64(metrics.requestsTotal.WithLabelValues("responses", "copilot", "gpt-5", "200")); got != 1 {
		t.Fatalf("vekil_requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.tokensTotal.WithLabelValues("responses", "copilot", "gpt-5", tokenDirectionPrompt)); got != 9 {
		t.Fatalf("prompt tokens = %v, want 9", got)
	}
	if got := testutil.ToFloat64(metrics.tokensTotal.WithLabelValues("responses", "copilot", "gpt-5", tokenDirectionCompletion)); got != 4 {
		t.Fatalf("completion tokens = %v, want 4", got)
	}
}

func TestHandleAnthropicMessages_InvalidJSON_RecordsRequestMetrics(t *testing.T) {
	metrics, err := NewMetrics("test-version")
	if err != nil {
		t.Fatalf("NewMetrics() error = %v", err)
	}

	handler := newTestProxyHandler(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("unexpected upstream request")
	})
	handler.metrics = metrics
	registerKnownMetricsTestModels(t, handler, "claude-3.7-sonnet")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-3.7-sonnet",`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.HandleAnthropicMessages(rec, req)

	if got := rec.Code; got != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
	}
	if got := testutil.ToFloat64(metrics.requestsTotal.WithLabelValues("messages", "copilot", "claude-3.7-sonnet", "400")); got != 1 {
		t.Fatalf("vekil_requests_total = %v, want 1", got)
	}
	if got := histogramSampleCount(t, metrics.requestDuration.WithLabelValues("messages", "copilot", "claude-3.7-sonnet", "400")); got != 1 {
		t.Fatalf("vekil_request_duration_seconds sample_count = %d, want 1", got)
	}
}

func TestHandleMemorySummarize_InvalidJSON_RecordsRequestMetrics(t *testing.T) {
	metrics, err := NewMetrics("test-version")
	if err != nil {
		t.Fatalf("NewMetrics() error = %v", err)
	}

	handler := newTestProxyHandler(t, func(http.ResponseWriter, *http.Request) {
		t.Fatal("unexpected upstream request")
	})
	handler.metrics = metrics
	registerKnownMetricsTestModels(t, handler, "gpt-5")

	req := httptest.NewRequest(http.MethodPost, "/v1/memories/trace_summarize", strings.NewReader(`{"model":"gpt-5",`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.HandleMemorySummarize(rec, req)

	if got := rec.Code; got != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", got, http.StatusBadRequest)
	}
	if got := testutil.ToFloat64(metrics.requestsTotal.WithLabelValues("memory_trace_summarize", "copilot", "gpt-5", "400")); got != 1 {
		t.Fatalf("vekil_requests_total = %v, want 1", got)
	}
	if got := histogramSampleCount(t, metrics.requestDuration.WithLabelValues("memory_trace_summarize", "copilot", "gpt-5", "400")); got != 1 {
		t.Fatalf("vekil_request_duration_seconds sample_count = %d, want 1", got)
	}
}

func TestHandleAnthropicMessages_RetryMetricsUsePublicEndpointLabel(t *testing.T) {
	metrics, err := NewMetrics("test-version")
	if err != nil {
		t.Fatalf("NewMetrics() error = %v", err)
	}

	calls := 0
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, `{"error":{"message":"retry me"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"id":"chatcmpl-test",
			"object":"chat.completion",
			"model":"claude-3.7-sonnet",
			"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],
			"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}
		}`)
	})
	handler.metrics = metrics
	registerKnownMetricsTestModels(t, handler, "claude-3.7-sonnet")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{
		"model":"claude-3.7-sonnet",
		"max_tokens":64,
		"messages":[{"role":"user","content":"hello"}]
	}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	handler.HandleAnthropicMessages(rec, req)

	if got := rec.Code; got != http.StatusOK {
		t.Fatalf("status = %d, want %d", got, http.StatusOK)
	}
	if calls != 2 {
		t.Fatalf("upstream calls = %d, want 2", calls)
	}
	if got := testutil.ToFloat64(metrics.requestsTotal.WithLabelValues("messages", "copilot", "claude-3.7-sonnet", "200")); got != 1 {
		t.Fatalf("vekil_requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.retriesTotal.WithLabelValues("messages", "copilot", "claude-3.7-sonnet", "status", "503")); got != 1 {
		t.Fatalf("vekil_retries_total(messages) = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.upstreamErrors.WithLabelValues("messages", "copilot", "claude-3.7-sonnet", "status", "503")); got == 0 {
		t.Fatal("expected vekil_upstream_errors_total(messages) to be recorded")
	}
	if got := testutil.ToFloat64(metrics.retriesTotal.WithLabelValues("chat_completions", "copilot", "claude-3.7-sonnet", "status", "503")); got != 0 {
		t.Fatalf("vekil_retries_total(chat_completions) = %v, want 0", got)
	}
}
