package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	dto "github.com/prometheus/client_model/go"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestHandleOpenAIChatCompletionsRecordsMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("path = %q, want /chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	}))
	defer upstream.Close()

	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		withCopilotBaseURLForTest(upstream.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.HandleOpenAIChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := testutil.ToFloat64(h.metrics.requestsTotal.WithLabelValues("copilot", unknownMetricsLabel, "/v1/chat/completions", "200")); got != 1 {
		t.Fatalf("requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.metrics.tokensTotal.WithLabelValues("copilot", unknownMetricsLabel, "prompt")); got != 5 {
		t.Fatalf("prompt tokens_total = %v, want 5", got)
	}
	if got := testutil.ToFloat64(h.metrics.tokensTotal.WithLabelValues("copilot", unknownMetricsLabel, "completion")); got != 3 {
		t.Fatalf("completion tokens_total = %v, want 3", got)
	}
	if got := testutil.ToFloat64(h.metrics.inflightRequests.WithLabelValues("copilot")); got != 0 {
		t.Fatalf("inflight_requests = %v, want 0", got)
	}
}

func TestHandleResponsesStreamingRecordsMetricsOnClose(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("path = %q, want /responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":11,\"output_tokens\":7,\"total_tokens\":18}}}\n\n")
	}))
	defer upstream.Close()

	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		withCopilotBaseURLForTest(upstream.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4","stream":true,"input":[]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.HandleResponses(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := testutil.ToFloat64(h.metrics.requestsTotal.WithLabelValues("copilot", unknownMetricsLabel, "/v1/responses", "200")); got != 1 {
		t.Fatalf("requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.metrics.tokensTotal.WithLabelValues("copilot", unknownMetricsLabel, "prompt")); got != 11 {
		t.Fatalf("prompt tokens_total = %v, want 11", got)
	}
	if got := testutil.ToFloat64(h.metrics.tokensTotal.WithLabelValues("copilot", unknownMetricsLabel, "completion")); got != 7 {
		t.Fatalf("completion tokens_total = %v, want 7", got)
	}
	if got := histogramSampleCount(t, h.metrics.registry, "vekil_stream_first_byte_latency_seconds", map[string]string{
		"provider":     "copilot",
		"public_model": unknownMetricsLabel,
		"endpoint":     "/v1/responses",
	}); got != 1 {
		t.Fatalf("stream first-byte sample_count = %d, want 1", got)
	}
}

func TestHandleOpenAIChatCompletionsRecordsRetryMetrics(t *testing.T) {
	attempts := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = fmt.Fprint(w, `{"error":{"message":"slow down"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	}))
	defer upstream.Close()

	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		withCopilotBaseURLForTest(upstream.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	h.retryBaseDelay = time.Millisecond

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()

	h.HandleOpenAIChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if got := testutil.ToFloat64(h.metrics.retriesTotal.WithLabelValues("copilot", unknownMetricsLabel, "429")); got != 1 {
		t.Fatalf("retries_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.metrics.upstreamErrorsTotal.WithLabelValues("copilot", unknownMetricsLabel, "429")); got != 1 {
		t.Fatalf("upstream_errors_total = %v, want 1", got)
	}
}

func TestHandleResponsesWebSocketRecordsMetrics(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/openai/v1/responses" {
			t.Fatalf("path = %q, want /openai/v1/responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-ws-1\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-ws-1\",\"usage\":{\"input_tokens\":13,\"output_tokens\":8,\"total_tokens\":21}}}\n\n")
	}))
	defer upstream.Close()

	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:      "azure",
				Type:    "azure-openai",
				Default: true,
				BaseURL: upstream.URL + "/openai/v1",
				APIKey:  "azure-test-key",
				Models: []ProviderModelConfig{{
					PublicID:   "gpt-5-public",
					Deployment: "gpt-5-4-prod",
					Endpoints:  []string{"/responses"},
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	server := startResponsesWebSocketProxyServer(t, h)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()

	request := newResponsesWebSocketCreateRequest([]interface{}{
		map[string]interface{}{
			"type": "message",
			"role": "user",
			"content": []map[string]string{
				{"type": "input_text", "text": "hello"},
			},
		},
	})
	request["model"] = "gpt-5-public"

	if err := conn.WriteJSON(request); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}

	if got := mustReadWebSocketJSONSkipMetadata(t, conn)["type"]; got != "response.created" {
		t.Fatalf("first websocket event type = %v, want response.created", got)
	}
	if got := mustReadWebSocketJSONSkipMetadata(t, conn)["type"]; got != "response.completed" {
		t.Fatalf("second websocket event type = %v, want response.completed", got)
	}

	if got := testutil.ToFloat64(h.metrics.requestsTotal.WithLabelValues("azure", "gpt-5-public", "/v1/responses", "200")); got != 1 {
		t.Fatalf("requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(h.metrics.tokensTotal.WithLabelValues("azure", "gpt-5-public", "prompt")); got != 13 {
		t.Fatalf("prompt tokens_total = %v, want 13", got)
	}
	if got := testutil.ToFloat64(h.metrics.tokensTotal.WithLabelValues("azure", "gpt-5-public", "completion")); got != 8 {
		t.Fatalf("completion tokens_total = %v, want 8", got)
	}
	if got := testutil.ToFloat64(h.metrics.inflightRequests.WithLabelValues("azure")); got != 0 {
		t.Fatalf("inflight_requests = %v, want 0", got)
	}
	if got := histogramSampleCount(t, h.metrics.registry, "vekil_stream_first_byte_latency_seconds", map[string]string{
		"provider":     "azure",
		"public_model": "gpt-5-public",
		"endpoint":     "/v1/responses",
	}); got != 1 {
		t.Fatalf("stream first-byte sample_count = %d, want 1", got)
	}
}

func TestNormalizeProvidedBuildVersionTreatsDevAsUnset(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{name: "empty stays empty", in: "", want: ""},
		{name: "dev is treated as unset", in: "dev", want: ""},
		{name: "mixed case dev is treated as unset", in: " Dev ", want: ""},
		{name: "real version is preserved", in: "1.2.3", want: "1.2.3"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := normalizeProvidedBuildVersion(tt.in); got != tt.want {
				t.Fatalf("normalizeProvidedBuildVersion(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func histogramSampleCount(t *testing.T, registry gatherer, name string, labels map[string]string) uint64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("Gather() error = %v", err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if labelPairsMatch(metric.GetLabel(), labels) {
				if metric.GetHistogram() == nil {
					t.Fatalf("metric %s is not a histogram", name)
				}
				return metric.GetHistogram().GetSampleCount()
			}
		}
	}
	t.Fatalf("metric %s with labels %v not found", name, labels)
	return 0
}

type gatherer interface {
	Gather() ([]*dto.MetricFamily, error)
}

func labelPairsMatch(pairs []*dto.LabelPair, want map[string]string) bool {
	if len(pairs) != len(want) {
		return false
	}
	for _, pair := range pairs {
		value, ok := want[pair.GetName()]
		if !ok || value != pair.GetValue() {
			return false
		}
	}
	return true
}
