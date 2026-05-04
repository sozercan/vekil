package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sozercan/vekil/models"
)

func TestHandleMetricsExposesPrometheusMetrics(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID:      "chatcmpl-metrics-expose",
			Object:  "chat.completion",
			Created: 123,
			Model:   "gpt-4",
			Choices: []models.OpenAIChoice{{
				Index:        0,
				Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)},
				FinishReason: strPtr("stop"),
			}},
			Usage: &models.OpenAIUsage{PromptTokens: 5, CompletionTokens: 2, TotalTokens: 7},
		})
	})

	probeReq := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`))
	probeReq.Header.Set("Content-Type", "application/json")
	probeW := httptest.NewRecorder()
	handler.HandleOpenAIChatCompletions(probeW, probeReq)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()

	handler.HandleMetrics(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}
	if got := resp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain exposition", got)
	}

	body, _ := io.ReadAll(resp.Body)
	text := string(body)
	for _, want := range []string{
		"# HELP vekil_requests_total",
		"# HELP vekil_request_duration_seconds",
		"# HELP vekil_tokens_total",
		"vekil_build_info",
		"go_goroutines",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("metrics output missing %q", want)
		}
	}
}

func TestHandleOpenAIChatCompletionsObservesMetrics(t *testing.T) {
	handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(models.OpenAIResponse{
			ID:      "chatcmpl-metrics",
			Object:  "chat.completion",
			Created: 123,
			Model:   "gpt-4",
			Choices: []models.OpenAIChoice{{
				Index:        0,
				Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)},
				FinishReason: strPtr("stop"),
			}},
			Usage: &models.OpenAIUsage{PromptTokens: 11, CompletionTokens: 3, TotalTokens: 14},
		})
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}

	if got := testutil.ToFloat64(handler.metrics.requestsTotal.WithLabelValues("copilot", "gpt-4", metricEndpointChatCompletions, "200")); got != 1 {
		t.Fatalf("requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(handler.metrics.tokensTotal.WithLabelValues("copilot", "gpt-4", metricEndpointChatCompletions, "prompt")); got != 11 {
		t.Fatalf("prompt tokens_total = %v, want 11", got)
	}
	if got := testutil.ToFloat64(handler.metrics.tokensTotal.WithLabelValues("copilot", "gpt-4", metricEndpointChatCompletions, "completion")); got != 3 {
		t.Fatalf("completion tokens_total = %v, want 3", got)
	}
}
