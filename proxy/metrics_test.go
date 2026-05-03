package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
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
	seedMetricsTestModels(t, handler, "gpt-4", "claude-3-sonnet", "gemini-2.5-pro")
	return handler
}

func seedMetricsTestModels(t testing.TB, handler *ProxyHandler, publicModels ...string) {
	t.Helper()

	if handler == nil {
		return
	}
	if handler.providersState == nil {
		handler.providersState = defaultProviderSetup(handler)
	}
	models := make([]providerModel, 0, len(publicModels))
	for _, publicModel := range publicModels {
		publicModel = strings.TrimSpace(publicModel)
		if publicModel == "" {
			continue
		}
		models = append(models, providerModel{
			publicID:           publicModel,
			upstreamModel:      publicModel,
			providerID:         "copilot",
			supportedEndpoints: append([]string(nil), defaultStaticProviderEndpoints...),
		})
	}
	if err := handler.providersState.replaceProviderModels("copilot", models); err != nil {
		t.Fatalf("replaceProviderModels() error = %v", err)
	}
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

func TestMetricsHandleOpenAIChatCompletionsDiscoversRuntimeModelLabels(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/models":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"object":"list","data":[{"id":"gpt-4","object":"model","supported_endpoints":["/chat/completions","/responses"]}]}`))
		case "/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":0,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
		default:
			t.Fatalf("unexpected upstream path %q", r.URL.Path)
		}
	}))
	defer server.Close()

	handler, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.New(logger.LevelInfo), withCopilotBaseURLForTest(server.URL))
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.client = server.Client()
	handler.retryBaseDelay = time.Millisecond

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

func TestMetricsClampUnknownPublicModelLabels(t *testing.T) {
	handler := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":0,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":3,"total_tokens":8}}`))
	})

	const rawModel = "client-controlled-unknown-model-1234567890"
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+rawModel+`","messages":[{"role":"user","content":"hi"}]}`))
	handler.HandleOpenAIChatCompletions(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	metricsText := renderMetricsText(t, handler)
	assertMetricLine(t, metricsText, "vekil_requests_total", map[string]string{
		"endpoint":     "/v1/chat/completions",
		"provider":     "copilot",
		"public_model": "unknown",
		"status":       "200",
	}, "1")
	if strings.Contains(metricsText, rawModel) {
		t.Fatalf("metrics unexpectedly exposed raw request model %q:\n%s", rawModel, metricsText)
	}
}

func TestMetricsEndpointHealthUsesSanitizedIdentifier(t *testing.T) {
	handler := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			t.Fatalf("unexpected upstream path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"object":"list","data":[]}`))
	})

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	handler.HandleReadyz(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("readyz status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}

	provider := handler.providerSetup().defaultProvider()
	metricsText := renderMetricsText(t, handler)
	assertMetricLine(t, metricsText, "vekil_endpoint_healthy", map[string]string{
		"provider": "copilot",
		"endpoint": metricsEndpointIdentifier(provider),
	}, "1")
	if strings.Contains(metricsText, provider.baseURL) {
		t.Fatalf("metrics unexpectedly exposed raw provider baseURL %q:\n%s", provider.baseURL, metricsText)
	}
}

func TestWriteUpstreamResponseObservedStreamsWithoutFullBuffering(t *testing.T) {
	collector, err := newMetricsCollector()
	if err != nil {
		t.Fatalf("newMetricsCollector() error = %v", err)
	}
	observer := collector.startRequest("copilot", "gpt-4", "/v1/chat/completions", false)

	body := &passthroughBlockingReadCloser{
		first:   []byte(`{"usage":{"prompt_tokens":5,`),
		second:  []byte(`"completion_tokens":3}}`),
		release: make(chan struct{}),
	}
	writer := newPassthroughWriteRecorder()
	done := make(chan error, 1)

	go func() {
		done <- writeUpstreamResponseObserved(writer, &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       body,
		}, observer)
	}()

	select {
	case <-writer.firstBodyWrite:
	case <-time.After(time.Second):
		t.Fatal("expected passthrough write before upstream body was fully read")
	}

	select {
	case err := <-done:
		t.Fatalf("writeUpstreamResponseObserved() returned before releasing second chunk: %v", err)
	default:
	}

	close(body.release)
	if err := <-done; err != nil {
		t.Fatalf("writeUpstreamResponseObserved() error = %v", err)
	}

	if got := writer.statusCode; got != http.StatusOK {
		t.Fatalf("statusCode = %d, want 200", got)
	}
	if got := writer.body.String(); got != `{"usage":{"prompt_tokens":5,"completion_tokens":3}}` {
		t.Fatalf("body = %q, want full passthrough body", got)
	}
}

func TestMetricsHandleResponsesCanceledBeforeCommitUses499(t *testing.T) {
	handler := newMetricsTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		<-r.Context().Done()
	})

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4","input":"Hello","stream":true}`)).WithContext(ctx)
	handler.HandleResponses(rec, req)

	metricsText := renderMetricsText(t, handler)
	assertMetricLine(t, metricsText, "vekil_requests_total", map[string]string{
		"endpoint":     "/v1/responses",
		"provider":     "copilot",
		"public_model": "gpt-4",
		"status":       "499",
	}, "1")
}

type passthroughBlockingReadCloser struct {
	first   []byte
	second  []byte
	release chan struct{}
	read    int
}

func (r *passthroughBlockingReadCloser) Read(p []byte) (int, error) {
	switch r.read {
	case 0:
		r.read++
		copy(p, r.first)
		return len(r.first), nil
	case 1:
		<-r.release
		r.read++
		copy(p, r.second)
		return len(r.second), io.EOF
	default:
		return 0, io.EOF
	}
}

func (r *passthroughBlockingReadCloser) Close() error { return nil }

type passthroughWriteRecorder struct {
	header         http.Header
	statusCode     int
	body           bytes.Buffer
	firstBodyWrite chan struct{}
	once           sync.Once
}

func newPassthroughWriteRecorder() *passthroughWriteRecorder {
	return &passthroughWriteRecorder{
		header:         make(http.Header),
		firstBodyWrite: make(chan struct{}),
	}
}

func (w *passthroughWriteRecorder) Header() http.Header {
	return w.header
}

func (w *passthroughWriteRecorder) WriteHeader(statusCode int) {
	w.statusCode = statusCode
}

func (w *passthroughWriteRecorder) Write(p []byte) (int, error) {
	if len(p) > 0 {
		w.once.Do(func() {
			close(w.firstBodyWrite)
		})
	}
	return w.body.Write(p)
}
