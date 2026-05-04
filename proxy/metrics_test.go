package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

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

	if got := testutil.ToFloat64(handler.metrics.requestsTotal.WithLabelValues("copilot", metricLabelUnknown, metricEndpointChatCompletions, "200")); got != 1 {
		t.Fatalf("requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(handler.metrics.tokensTotal.WithLabelValues("copilot", metricLabelUnknown, metricEndpointChatCompletions, "prompt")); got != 11 {
		t.Fatalf("prompt tokens_total = %v, want 11", got)
	}
	if got := testutil.ToFloat64(handler.metrics.tokensTotal.WithLabelValues("copilot", metricLabelUnknown, metricEndpointChatCompletions, "completion")); got != 3 {
		t.Fatalf("completion tokens_total = %v, want 3", got)
	}
}

func TestBeginRequestMetricsBoundsPublicModelLabels(t *testing.T) {
	t.Run("unresolved models collapse to unknown", func(t *testing.T) {
		handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("backend should not be called")
		})

		tracker := handler.beginRequestMetrics(metricEndpointChatCompletions, "/chat/completions", "attacker-controlled-model")
		if tracker == nil {
			t.Fatal("beginRequestMetrics returned nil tracker")
		}
		if got := tracker.labels.provider; got != "copilot" {
			t.Fatalf("provider = %q, want copilot", got)
		}
		if got := tracker.labels.publicModel; got != metricLabelUnknown {
			t.Fatalf("publicModel = %q, want %q", got, metricLabelUnknown)
		}
	})

	t.Run("resolved models keep bounded public ids and unsupported endpoints collapse", func(t *testing.T) {
		handler := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
			t.Fatal("backend should not be called")
		})
		setup, err := handler.buildConfiguredProviderSetup(context.Background(), ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:         "azure",
				Type:       "azure-openai",
				Default:    true,
				BaseURL:    "https://example.invalid/openai/v1",
				APIKey:     "test-key",
				APIVersion: "preview",
				Models: []ProviderModelConfig{{
					PublicID:   "gpt-5-public",
					Deployment: "gpt-5-4-prod",
					Endpoints:  []string{"/responses"},
				}},
			}},
		})
		if err != nil {
			t.Fatalf("buildConfiguredProviderSetup: %v", err)
		}
		handler.providersState = setup

		supported := handler.beginRequestMetrics(metricEndpointResponses, "/responses", "gpt-5-public")
		if supported == nil {
			t.Fatal("beginRequestMetrics returned nil tracker")
		}
		if got := supported.labels.provider; got != "azure" {
			t.Fatalf("provider = %q, want azure", got)
		}
		if got := supported.labels.publicModel; got != "gpt-5-public" {
			t.Fatalf("publicModel = %q, want gpt-5-public", got)
		}

		unsupported := handler.beginRequestMetrics(metricEndpointChatCompletions, "/chat/completions", "gpt-5-public")
		if unsupported == nil {
			t.Fatal("beginRequestMetrics returned nil tracker")
		}
		if got := unsupported.labels.provider; got != "azure" {
			t.Fatalf("provider = %q, want azure", got)
		}
		if got := unsupported.labels.publicModel; got != metricLabelUnknown {
			t.Fatalf("publicModel = %q, want %q", got, metricLabelUnknown)
		}
	})
}

func TestWriteOpenAIUpstreamResponseStreamsBeforeEOF(t *testing.T) {
	pr, pw := io.Pipe()
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       pr,
	}

	recorder := httptest.NewRecorder()
	signalW := newStreamingSignalResponseWriter(recorder)
	resultCh := make(chan struct {
		usage *models.OpenAIUsage
		err   error
	}, 1)

	go func() {
		usage, err := writeOpenAIUpstreamResponse(signalW, resp)
		resultCh <- struct {
			usage *models.OpenAIUsage
			err   error
		}{usage: usage, err: err}
	}()

	firstChunk := `{"id":"chatcmpl-stream","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"`
	if _, err := pw.Write([]byte(firstChunk)); err != nil {
		t.Fatalf("write first chunk: %v", err)
	}

	select {
	case <-signalW.bodyCh:
	case <-time.After(200 * time.Millisecond):
		t.Fatal("response body was not streamed before upstream EOF")
	}

	secondChunk := `ok"}}],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`
	if _, err := pw.Write([]byte(secondChunk)); err != nil {
		t.Fatalf("write second chunk: %v", err)
	}
	if err := pw.Close(); err != nil {
		t.Fatalf("close pipe writer: %v", err)
	}

	result := <-resultCh
	if result.err != nil {
		t.Fatalf("writeOpenAIUpstreamResponse returned error: %v", result.err)
	}
	if result.usage == nil {
		t.Fatal("usage = nil, want parsed usage")
	}
	if result.usage.PromptTokens != 11 || result.usage.CompletionTokens != 3 {
		t.Fatalf("usage = %+v, want prompt=11 completion=3", result.usage)
	}

	if got := recorder.Body.String(); got != firstChunk+secondChunk {
		t.Fatalf("body = %q, want %q", got, firstChunk+secondChunk)
	}
}

func TestWriteResponsesUpstreamResponseExtractsUsage(t *testing.T) {
	body := `{"id":"resp-1","object":"response","output":[],"usage":{"input_tokens":5,"output_tokens":7}}`
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}

	recorder := httptest.NewRecorder()
	promptTokens, completionTokens, ok, err := writeResponsesUpstreamResponse(recorder, resp)
	if err != nil {
		t.Fatalf("writeResponsesUpstreamResponse returned error: %v", err)
	}
	if !ok {
		t.Fatal("ok = false, want true")
	}
	if promptTokens != 5 || completionTokens != 7 {
		t.Fatalf("usage = (%d, %d), want (5, 7)", promptTokens, completionTokens)
	}
	if got := recorder.Body.String(); got != body {
		t.Fatalf("body = %q, want %q", got, body)
	}
}

type streamingSignalResponseWriter struct {
	http.ResponseWriter
	bodyOnce sync.Once
	bodyCh   chan struct{}
}

func newStreamingSignalResponseWriter(w http.ResponseWriter) *streamingSignalResponseWriter {
	return &streamingSignalResponseWriter{
		ResponseWriter: w,
		bodyCh:         make(chan struct{}),
	}
}

func (w *streamingSignalResponseWriter) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.bodyOnce.Do(func() {
			close(w.bodyCh)
		})
	}
	return w.ResponseWriter.Write(p)
}
