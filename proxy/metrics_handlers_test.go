package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

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
