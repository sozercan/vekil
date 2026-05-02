package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func assertMetricLine(t *testing.T, metrics, metricName, value string, labels map[string]string) {
	t.Helper()
	for _, line := range strings.Split(metrics, "\n") {
		if !strings.HasPrefix(line, metricName) {
			continue
		}
		if value != "" && !strings.HasSuffix(line, " "+value) {
			continue
		}
		matched := true
		for key, want := range labels {
			if !strings.Contains(line, key+`="`+want+`"`) {
				matched = false
				break
			}
		}
		if matched {
			return
		}
	}
	t.Fatalf("metric %s with labels %v and value %q not found\n%s", metricName, labels, value, metrics)
}

func readMetricsSnapshot(t *testing.T, handler *ProxyHandler) string {
	t.Helper()
	metricsHandler := handler.MetricsHandler()
	if metricsHandler == nil {
		t.Fatal("metrics handler is nil")
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	metricsHandler.ServeHTTP(w, req)

	res := w.Result()
	defer func() { _ = res.Body.Close() }()
	body, err := io.ReadAll(res.Body)
	if err != nil {
		t.Fatalf("read metrics body: %v", err)
	}
	return string(body)
}

func TestHandleOpenAIChatCompletions_RecordsPrometheusMetrics(t *testing.T) {
	t.Parallel()

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`)
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hello"}]}`))
	w := httptest.NewRecorder()
	handler.HandleOpenAIChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	metrics := readMetricsSnapshot(t, handler)
	assertMetricLine(t, metrics, "vekil_requests_total", "1", map[string]string{
		"provider":     "copilot",
		"public_model": "gpt-4",
		"endpoint":     "chat_completions",
		"status":       "200",
	})
	assertMetricLine(t, metrics, "vekil_request_duration_seconds_count", "1", map[string]string{
		"provider":     "copilot",
		"public_model": "gpt-4",
		"endpoint":     "chat_completions",
		"status":       "200",
	})
	assertMetricLine(t, metrics, "vekil_tokens_total", "5", map[string]string{
		"provider":     "copilot",
		"public_model": "gpt-4",
		"direction":    "prompt",
	})
	assertMetricLine(t, metrics, "vekil_tokens_total", "3", map[string]string{
		"provider":     "copilot",
		"public_model": "gpt-4",
		"direction":    "completion",
	})
	assertMetricLine(t, metrics, "vekil_inflight_requests", "0", map[string]string{
		"provider": "copilot",
	})
	assertMetricLine(t, metrics, "vekil_build_info", "1", map[string]string{
		"version": "test",
		"commit":  "test",
	})
}

func TestHandleResponses_StreamingMetricsRecordedOnStreamClose(t *testing.T) {
	t.Parallel()

	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":11,\"input_tokens_details\":null,\"output_tokens\":7,\"output_tokens_details\":null,\"total_tokens\":18}}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","stream":true,"input":[]}`))
	w := httptest.NewRecorder()
	handler.HandleResponses(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	metrics := readMetricsSnapshot(t, handler)
	assertMetricLine(t, metrics, "vekil_requests_total", "1", map[string]string{
		"provider":     "copilot",
		"public_model": "gpt-5.4",
		"endpoint":     "responses",
		"status":       "200",
	})
	assertMetricLine(t, metrics, "vekil_request_duration_seconds_count", "1", map[string]string{
		"provider":     "copilot",
		"public_model": "gpt-5.4",
		"endpoint":     "responses",
		"status":       "200",
	})
	assertMetricLine(t, metrics, "vekil_request_first_byte_duration_seconds_count", "1", map[string]string{
		"provider":     "copilot",
		"public_model": "gpt-5.4",
		"endpoint":     "responses",
		"status":       "200",
	})
	assertMetricLine(t, metrics, "vekil_tokens_total", "11", map[string]string{
		"provider":     "copilot",
		"public_model": "gpt-5.4",
		"direction":    "prompt",
	})
	assertMetricLine(t, metrics, "vekil_tokens_total", "7", map[string]string{
		"provider":     "copilot",
		"public_model": "gpt-5.4",
		"direction":    "completion",
	})
}
