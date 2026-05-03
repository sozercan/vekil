package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func newMetricsTestProxyHandler(t testing.TB, backend http.HandlerFunc, opts ...Option) *ProxyHandler {
	t.Helper()
	server := httptest.NewServer(backend)
	t.Cleanup(server.Close)

	options := append([]Option{withCopilotBaseURLForTest(server.URL)}, opts...)
	handler, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.New(logger.LevelInfo), options...)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.client = server.Client()
	handler.retryBaseDelay = time.Millisecond
	return handler
}

func renderMetricsText(t testing.TB, handler *ProxyHandler) string {
	t.Helper()
	metricsHandler := handler.MetricsHandler()
	if metricsHandler == nil {
		t.Fatal("metrics handler is nil")
	}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsHandler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status = %d, want 200", rec.Code)
	}
	return rec.Body.String()
}

func assertMetricLine(t testing.TB, metricsText, metricName string, labels map[string]string, wantValue string) {
	t.Helper()

	for _, line := range strings.Split(metricsText, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if !strings.HasPrefix(line, metricName+"{") && !(len(labels) == 0 && strings.HasPrefix(line, metricName+" ")) {
			continue
		}

		matched := true
		for key, value := range labels {
			if !strings.Contains(line, key+"=\""+value+"\"") {
				matched = false
				break
			}
		}
		if !matched {
			continue
		}
		if wantValue != "" && !strings.HasSuffix(line, " "+wantValue) {
			continue
		}
		return
	}

	t.Fatalf("metric %s with labels %v and value %q not found in:\n%s", metricName, labels, wantValue, metricsText)
}

func TestMetricsHandleOpenAIChatCompletions(t *testing.T) {
	handler := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("unexpected upstream path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":0,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`))
	handler.HandleOpenAIChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	metricsText := renderMetricsText(t, handler)
	assertMetricLine(t, metricsText, "vekil_requests_total", map[string]string{
		"endpoint":     "/v1/chat/completions",
		"provider":     "copilot",
		"public_model": "gpt-4",
		"status":       "200",
	}, "1")
	assertMetricLine(t, metricsText, "vekil_tokens_total", map[string]string{
		"direction":    "prompt",
		"endpoint":     "/v1/chat/completions",
		"provider":     "copilot",
		"public_model": "gpt-4",
	}, "5")
	assertMetricLine(t, metricsText, "vekil_tokens_total", map[string]string{
		"direction":    "completion",
		"endpoint":     "/v1/chat/completions",
		"provider":     "copilot",
		"public_model": "gpt-4",
	}, "3")
	if !strings.Contains(metricsText, "vekil_build_info{") {
		t.Fatalf("metrics output missing vekil_build_info:\n%s", metricsText)
	}
}

func TestMetricsHandleAnthropicMessages(t *testing.T) {
	handler := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"chatcmpl-1\",\"model\":\"claude-3-sonnet\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"hello\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10,\"completion_tokens\":4,\"total_tokens\":14}}\n\n"))
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-3-sonnet","messages":[{"role":"user","content":"hi"}],"max_tokens":32}`))
	handler.HandleAnthropicMessages(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	metricsText := renderMetricsText(t, handler)
	assertMetricLine(t, metricsText, "vekil_requests_total", map[string]string{
		"endpoint":     "/v1/messages",
		"provider":     "copilot",
		"public_model": "claude-3-sonnet",
		"status":       "200",
	}, "1")
	assertMetricLine(t, metricsText, "vekil_tokens_total", map[string]string{
		"direction":    "prompt",
		"endpoint":     "/v1/messages",
		"provider":     "copilot",
		"public_model": "claude-3-sonnet",
	}, "10")
	assertMetricLine(t, metricsText, "vekil_tokens_total", map[string]string{
		"direction":    "completion",
		"endpoint":     "/v1/messages",
		"provider":     "copilot",
		"public_model": "claude-3-sonnet",
	}, "4")
}

func TestMetricsHandleResponsesStreaming(t *testing.T) {
	handler := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-1\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n\n"))
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-1\",\"usage\":{\"input_tokens\":9,\"output_tokens\":4,\"total_tokens\":13}}}\n\n"))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4","input":"Hello","stream":true}`))
	handler.HandleResponses(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	metricsText := renderMetricsText(t, handler)
	assertMetricLine(t, metricsText, "vekil_requests_total", map[string]string{
		"endpoint":     "/v1/responses",
		"provider":     "copilot",
		"public_model": "gpt-4",
		"status":       "200",
	}, "1")
	assertMetricLine(t, metricsText, "vekil_tokens_total", map[string]string{
		"direction":    "prompt",
		"endpoint":     "/v1/responses",
		"provider":     "copilot",
		"public_model": "gpt-4",
	}, "9")
	assertMetricLine(t, metricsText, "vekil_tokens_total", map[string]string{
		"direction":    "completion",
		"endpoint":     "/v1/responses",
		"provider":     "copilot",
		"public_model": "gpt-4",
	}, "4")
	assertMetricLine(t, metricsText, "vekil_stream_first_byte_latency_seconds_count", map[string]string{
		"endpoint":     "/v1/responses",
		"provider":     "copilot",
		"public_model": "gpt-4",
	}, "1")
}

func TestMetricsHandleGeminiGenerateContent(t *testing.T) {
	handler := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-gemini","object":"chat.completion","created":0,"model":"gemini-2.5-pro","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":6,"completion_tokens":2,"total_tokens":8}}`))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/models/gemini-2.5-pro:generateContent", strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`))
	handler.HandleGeminiModels(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	metricsText := renderMetricsText(t, handler)
	assertMetricLine(t, metricsText, "vekil_requests_total", map[string]string{
		"endpoint":     "/v1/models/*:generateContent",
		"provider":     "copilot",
		"public_model": "gemini-2.5-pro",
		"status":       "200",
	}, "1")
	assertMetricLine(t, metricsText, "vekil_tokens_total", map[string]string{
		"direction":    "prompt",
		"endpoint":     "/v1/models/*:generateContent",
		"provider":     "copilot",
		"public_model": "gemini-2.5-pro",
	}, "6")
	assertMetricLine(t, metricsText, "vekil_tokens_total", map[string]string{
		"direction":    "completion",
		"endpoint":     "/v1/models/*:generateContent",
		"provider":     "copilot",
		"public_model": "gemini-2.5-pro",
	}, "2")
}

func TestMetricsRecordRetriesAndUpstreamErrors(t *testing.T) {
	attempts := 0
	handler := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		attempts++
		if attempts == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"message":"slow down"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","model":"gpt-4","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Hello!"}]}],"usage":{"input_tokens":5,"output_tokens":3,"total_tokens":8}}`))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4","input":"Hello"}`))
	handler.HandleResponses(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}

	metricsText := renderMetricsText(t, handler)
	assertMetricLine(t, metricsText, "vekil_retries_total", map[string]string{
		"endpoint":     "/v1/responses",
		"provider":     "copilot",
		"public_model": "gpt-4",
		"reason":       "429",
	}, "1")
	assertMetricLine(t, metricsText, "vekil_upstream_errors_total", map[string]string{
		"code":         "429",
		"endpoint":     "/v1/responses",
		"provider":     "copilot",
		"public_model": "gpt-4",
	}, "1")
}
