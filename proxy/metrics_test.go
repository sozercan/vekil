package proxy

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	promtest "github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func newMetricsTestProxyHandler(t testing.TB, backend http.HandlerFunc, opts ...Option) *ProxyHandler {
	t.Helper()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)

	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		append([]Option{withCopilotBaseURLForTest(server.URL)}, opts...)...,
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	h.client = server.Client()
	h.retryBaseDelay = time.Millisecond
	return h
}

func seedMetricsPublicModels(h *ProxyHandler, publicModels ...string) {
	setup := defaultProviderSetup(h)
	setup.models = make(map[string]providerModel, len(publicModels))
	for _, publicModel := range publicModels {
		publicModel = strings.TrimSpace(publicModel)
		if publicModel == "" {
			continue
		}
		setup.models[publicModel] = providerModel{
			publicID:      publicModel,
			upstreamModel: publicModel,
			providerID:    "copilot",
		}
	}
	h.providersState = setup
}

func histogramSampleCount(t testing.TB, reg *prometheus.Registry, name string, labels map[string]string) uint64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if labelsMatch(metric, labels) {
				if metric.Histogram == nil {
					t.Fatalf("metric %q is not a histogram", name)
				}
				return metric.Histogram.GetSampleCount()
			}
		}
	}
	t.Fatalf("histogram %q with labels %v not found", name, labels)
	return 0
}

func gaugeValue(t testing.TB, reg *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.Metric {
			if labelsMatch(metric, labels) {
				if metric.Gauge == nil {
					t.Fatalf("metric %q is not a gauge", name)
				}
				return metric.Gauge.GetValue()
			}
		}
	}
	t.Fatalf("gauge %q with labels %v not found", name, labels)
	return 0
}

func labelsMatch(metric *dto.Metric, labels map[string]string) bool {
	if len(metric.GetLabel()) != len(labels) {
		return false
	}
	for _, pair := range metric.GetLabel() {
		if labels[pair.GetName()] != pair.GetValue() {
			return false
		}
	}
	return true
}

func metricsBody(t testing.TB, h *ProxyHandler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	metricsHandler := h.MetricsHandler()
	if metricsHandler == nil {
		t.Fatal("MetricsHandler() = nil, want non-nil")
	}
	metricsHandler.ServeHTTP(w, req)
	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll(metrics) error = %v", err)
	}
	return string(body)
}

func TestHandleOpenAIChatCompletions_RecordsMetrics(t *testing.T) {
	h := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	})
	seedMetricsPublicModels(h, "gpt-4")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`))
	w := httptest.NewRecorder()

	h.HandleOpenAIChatCompletions(w, req)

	if got := promtest.ToFloat64(h.metrics.requests.WithLabelValues("copilot", "gpt-4", "/v1/chat/completions", "200")); got != 1 {
		t.Fatalf("requests_total = %v, want 1", got)
	}
	if got := promtest.ToFloat64(h.metrics.tokens.WithLabelValues("copilot", "gpt-4", "/v1/chat/completions", "prompt")); got != 5 {
		t.Fatalf("prompt tokens = %v, want 5", got)
	}
	if got := promtest.ToFloat64(h.metrics.tokens.WithLabelValues("copilot", "gpt-4", "/v1/chat/completions", "completion")); got != 3 {
		t.Fatalf("completion tokens = %v, want 3", got)
	}
	if got := histogramSampleCount(t, h.metrics.registry, "vekil_request_duration_seconds", map[string]string{"provider": "copilot", "public_model": "gpt-4", "endpoint": "/v1/chat/completions", "status": "200"}); got != 1 {
		t.Fatalf("duration sample count = %d, want 1", got)
	}
	if got := gaugeValue(t, h.metrics.registry, "vekil_inflight_requests", map[string]string{"provider": "copilot"}); got != 0 {
		t.Fatalf("inflight_requests = %v, want 0", got)
	}
}

func TestHandleAnthropicMessages_RecordsMetrics(t *testing.T) {
	h := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w,
			"data: {\"id\":\"c1\",\"created\":1,\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n"+
				"data: {\"id\":\"c1\",\"created\":1,\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"Hello\"}}]}\n\n"+
				"data: {\"id\":\"c1\",\"created\":1,\"model\":\"gpt-4\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":5,\"total_tokens\":15}}\n\n"+
				"data: [DONE]\n",
		)
	})
	seedMetricsPublicModels(h, "claude-sonnet-4-5")

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"Hi"}]}`))
	w := httptest.NewRecorder()

	h.HandleAnthropicMessages(w, req)

	if got := promtest.ToFloat64(h.metrics.requests.WithLabelValues("copilot", "claude-sonnet-4-5", "/v1/messages", "200")); got != 1 {
		t.Fatalf("requests_total = %v, want 1", got)
	}
	if got := promtest.ToFloat64(h.metrics.tokens.WithLabelValues("copilot", "claude-sonnet-4-5", "/v1/messages", "prompt")); got != 10 {
		t.Fatalf("prompt tokens = %v, want 10", got)
	}
	if got := promtest.ToFloat64(h.metrics.tokens.WithLabelValues("copilot", "claude-sonnet-4-5", "/v1/messages", "completion")); got != 5 {
		t.Fatalf("completion tokens = %v, want 5", got)
	}
}

func TestHandleResponsesStreaming_RecordsMetrics(t *testing.T) {
	h := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w,
			"event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n"+
				"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":7,\"output_tokens\":4,\"total_tokens\":11}}}\n\n",
		)
	})
	seedMetricsPublicModels(h, "gpt-4")

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4","input":"Hi","stream":true}`))
	w := httptest.NewRecorder()

	h.HandleResponses(w, req)

	if got := promtest.ToFloat64(h.metrics.requests.WithLabelValues("copilot", "gpt-4", "/v1/responses", "200")); got != 1 {
		t.Fatalf("requests_total = %v, want 1", got)
	}
	if got := promtest.ToFloat64(h.metrics.tokens.WithLabelValues("copilot", "gpt-4", "/v1/responses", "prompt")); got != 7 {
		t.Fatalf("prompt tokens = %v, want 7", got)
	}
	if got := promtest.ToFloat64(h.metrics.tokens.WithLabelValues("copilot", "gpt-4", "/v1/responses", "completion")); got != 4 {
		t.Fatalf("completion tokens = %v, want 4", got)
	}
	if got := histogramSampleCount(t, h.metrics.registry, "vekil_request_first_byte_latency_seconds", map[string]string{"provider": "copilot", "public_model": "gpt-4", "endpoint": "/v1/responses"}); got != 1 {
		t.Fatalf("first byte sample count = %d, want 1", got)
	}
}

func TestHandleGeminiGenerateContent_RecordsMetrics(t *testing.T) {
	h := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":6,"total_tokens":10}}`)
	})
	seedMetricsPublicModels(h, "gemini-2.5-pro")

	reqBody := `{"contents":[{"role":"user","parts":[{"text":"Hi"}]}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/gemini-2.5-pro:generateContent", strings.NewReader(reqBody))
	w := httptest.NewRecorder()

	h.HandleGeminiModels(w, req)

	if got := promtest.ToFloat64(h.metrics.requests.WithLabelValues("copilot", "gemini-2.5-pro", "/gemini:generateContent", "200")); got != 1 {
		t.Fatalf("requests_total = %v, want 1", got)
	}
	if got := promtest.ToFloat64(h.metrics.tokens.WithLabelValues("copilot", "gemini-2.5-pro", "/gemini:generateContent", "prompt")); got != 4 {
		t.Fatalf("prompt tokens = %v, want 4", got)
	}
	if got := promtest.ToFloat64(h.metrics.tokens.WithLabelValues("copilot", "gemini-2.5-pro", "/gemini:generateContent", "completion")); got != 6 {
		t.Fatalf("completion tokens = %v, want 6", got)
	}
}

func TestHandleOpenAIChatCompletions_RetryMetrics(t *testing.T) {
	var attempts atomic.Int32
	h := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	})
	seedMetricsPublicModels(h, "gpt-4")

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`))
	w := httptest.NewRecorder()

	h.HandleOpenAIChatCompletions(w, req)

	if got := promtest.ToFloat64(h.metrics.retries.WithLabelValues("copilot", "gpt-4", "/v1/chat/completions", "429")); got != 1 {
		t.Fatalf("retries_total = %v, want 1", got)
	}
	if got := promtest.ToFloat64(h.metrics.requests.WithLabelValues("copilot", "gpt-4", "/v1/chat/completions", "200")); got != 1 {
		t.Fatalf("requests_total = %v, want 1", got)
	}
}

func TestHandleOpenAIChatCompletions_TransportRetryMetrics(t *testing.T) {
	var attempts atomic.Int32
	h := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("unexpected backend request")
	})
	seedMetricsPublicModels(h, "gpt-4")
	h.client = &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			if attempts.Add(1) == 1 {
				return nil, errors.New("dial tcp: connection refused")
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(strings.NewReader(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)),
			}, nil
		}),
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`))
	w := httptest.NewRecorder()

	h.HandleOpenAIChatCompletions(w, req)

	if got := promtest.ToFloat64(h.metrics.retries.WithLabelValues("copilot", "gpt-4", "/v1/chat/completions", "transport")); got != 1 {
		t.Fatalf("transport retries_total = %v, want 1", got)
	}
	if got := promtest.ToFloat64(h.metrics.retries.WithLabelValues("copilot", "gpt-4", "/v1/chat/completions", "timeout")); got != 0 {
		t.Fatalf("timeout retries_total = %v, want 0", got)
	}
}

func TestHandleResponses_UpstreamErrorMetrics(t *testing.T) {
	h := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	})
	seedMetricsPublicModels(h, "gpt-4")
	h.maxRetries = 1

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4","input":"Hi"}`))
	w := httptest.NewRecorder()

	h.HandleResponses(w, req)

	if got := promtest.ToFloat64(h.metrics.upstreamErrors.WithLabelValues("copilot", "gpt-4", "/v1/responses", "503")); got != 1 {
		t.Fatalf("upstream_errors_total = %v, want 1", got)
	}
	if got := promtest.ToFloat64(h.metrics.requests.WithLabelValues("copilot", "gpt-4", "/v1/responses", "503")); got != 1 {
		t.Fatalf("requests_total = %v, want 1", got)
	}
}

func TestBeginRequestMetrics_CollapsesUnknownPublicModelLabel(t *testing.T) {
	h := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {})

	tracker := h.beginRequestMetrics("/gemini:generateContent", "/chat/completions", "tenant-secret-model")
	if tracker == nil {
		t.Fatal("beginRequestMetrics() = nil, want non-nil")
	}
	tracker.Finish(http.StatusBadRequest)

	if got := promtest.ToFloat64(h.metrics.requests.WithLabelValues("copilot", "unknown", "/gemini:generateContent", "400")); got != 1 {
		t.Fatalf("requests_total = %v, want 1", got)
	}
	if body := metricsBody(t, h); strings.Contains(body, `public_model="tenant-secret-model"`) {
		t.Fatalf("metrics output leaked raw public model label:\n%s", body)
	}
}

func TestHandleOpenAIChatCompletions_PopulatesMetricsPublicModelFromTrustedUpstreamResponseWithoutCatalog(t *testing.T) {
	var chatCalls atomic.Int32

	h := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			chatCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
		default:
			t.Fatalf("unexpected backend path %q", r.URL.Path)
		}
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`))
	w := httptest.NewRecorder()

	h.HandleOpenAIChatCompletions(w, req)

	if got := chatCalls.Load(); got != 1 {
		t.Fatalf("/chat/completions calls = %d, want 1", got)
	}
	if got := promtest.ToFloat64(h.metrics.requests.WithLabelValues("copilot", "gpt-4", "/v1/chat/completions", "200")); got != 1 {
		t.Fatalf("gpt-4 requests_total = %v, want 1", got)
	}
	if got := promtest.ToFloat64(h.metrics.requests.WithLabelValues("copilot", "unknown", "/v1/chat/completions", "200")); got != 0 {
		t.Fatalf("unknown-model requests_total = %v, want 0", got)
	}
}

func TestHandleOpenAIChatCompletions_PopulatesRetryMetricsPublicModelFromTrustedUpstreamResponseWithoutCatalog(t *testing.T) {
	var attempts atomic.Int32

	h := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/chat/completions" {
			t.Fatalf("unexpected backend path %q", got)
		}
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`))
	w := httptest.NewRecorder()

	h.HandleOpenAIChatCompletions(w, req)

	if got := promtest.ToFloat64(h.metrics.retries.WithLabelValues("copilot", "gpt-4", "/v1/chat/completions", "429")); got != 1 {
		t.Fatalf("gpt-4 retries_total = %v, want 1", got)
	}
	if got := promtest.ToFloat64(h.metrics.retries.WithLabelValues("copilot", "unknown", "/v1/chat/completions", "429")); got != 0 {
		t.Fatalf("unknown-model retries_total = %v, want 0", got)
	}
}

func TestHandleOpenAIChatCompletions_PopulatesMetricsPublicModelFromCachedCatalog(t *testing.T) {
	var modelsCalls atomic.Int32
	var chatCalls atomic.Int32

	h := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			modelsCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-4","object":"model","owned_by":"copilot"}]}`)
		case "/chat/completions":
			chatCalls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
		default:
			t.Fatalf("unexpected backend path %q", r.URL.Path)
		}
	})

	modelsReq := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	modelsW := httptest.NewRecorder()
	h.HandleModels(modelsW, modelsReq)
	if got := modelsW.Result().StatusCode; got != http.StatusOK {
		t.Fatalf("/v1/models status = %d, want 200", got)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"Hi"}]}`))
	w := httptest.NewRecorder()

	h.HandleOpenAIChatCompletions(w, req)

	if got := modelsCalls.Load(); got != 1 {
		t.Fatalf("/models calls = %d, want 1", got)
	}
	if got := chatCalls.Load(); got != 1 {
		t.Fatalf("/chat/completions calls = %d, want 1", got)
	}
	if got := promtest.ToFloat64(h.metrics.requests.WithLabelValues("copilot", "gpt-4", "/v1/chat/completions", "200")); got != 1 {
		t.Fatalf("requests_total = %v, want 1", got)
	}
	if got := promtest.ToFloat64(h.metrics.requests.WithLabelValues("copilot", "unknown", "/v1/chat/completions", "200")); got != 0 {
		t.Fatalf("unknown-model requests_total = %v, want 0", got)
	}
}

func TestHandleResponses_PopulatesMetricsPublicModelFromTrustedUpstreamResponseWithoutCatalog(t *testing.T) {
	var responsesCalls atomic.Int32

	h := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/responses" {
			t.Fatalf("unexpected backend path %q", got)
		}
		responsesCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"resp-1","object":"response","status":"completed","model":"gpt-4","output":[],"usage":{"input_tokens":7,"output_tokens":4,"total_tokens":11}}`)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4","input":"Hi"}`))
	w := httptest.NewRecorder()

	h.HandleResponses(w, req)

	if got := responsesCalls.Load(); got != 1 {
		t.Fatalf("/responses calls = %d, want 1", got)
	}
	if got := promtest.ToFloat64(h.metrics.requests.WithLabelValues("copilot", "gpt-4", "/v1/responses", "200")); got != 1 {
		t.Fatalf("gpt-4 requests_total = %v, want 1", got)
	}
	if got := promtest.ToFloat64(h.metrics.requests.WithLabelValues("copilot", "unknown", "/v1/responses", "200")); got != 0 {
		t.Fatalf("unknown-model requests_total = %v, want 0", got)
	}
}

func TestWriteUpstreamResponseWithObserver_ForwardsBodyOnObserverParseError(t *testing.T) {
	resp := &http.Response{
		StatusCode: http.StatusBadGateway,
		Header:     http.Header{"Content-Type": []string{"text/plain"}},
		Body:       io.NopCloser(strings.NewReader("not-json-at-all")),
	}
	w := httptest.NewRecorder()

	writeUpstreamResponseWithObserver(w, resp, func(body io.Reader) {
		if usage := extractOpenAIUsageFromReader(body); usage != nil {
			t.Fatalf("extractOpenAIUsageFromReader() = %+v, want nil", usage)
		}
	})

	if w.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadGateway)
	}
	if got := w.Header().Get("Content-Type"); got != "text/plain" {
		t.Fatalf("Content-Type = %q, want text/plain", got)
	}
	if got := w.Body.String(); got != "not-json-at-all" {
		t.Fatalf("body = %q, want %q", got, "not-json-at-all")
	}
}

func TestMetricsHandler_ExportsBuildInfo(t *testing.T) {
	h := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"Hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}, WithBuildInfo(BuildInfo{Version: "1.2.3", Commit: "abc123"}))

	body := metricsBody(t, h)
	if !strings.Contains(body, `vekil_build_info{commit="abc123",go_version="`) || !strings.Contains(body, `version="1.2.3"} 1`) {
		t.Fatalf("metrics output missing build info:\n%s", body)
	}
}
