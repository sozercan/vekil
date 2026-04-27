package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

type flushTrackingResponseWriter struct {
	header  http.Header
	body    strings.Builder
	status  int
	flushes int
}

func (w *flushTrackingResponseWriter) Header() http.Header {
	if w.header == nil {
		w.header = make(http.Header)
	}
	return w.header
}

func (w *flushTrackingResponseWriter) WriteHeader(statusCode int) {
	w.status = statusCode
}

func (w *flushTrackingResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return w.body.Write(p)
}

func (w *flushTrackingResponseWriter) Flush() {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.flushes++
}

func TestStatusCapturingResponseWriter_ForwardsFlusherForStreaming(t *testing.T) {
	base := &flushTrackingResponseWriter{}
	wrapped := newStatusCapturingResponseWriter(base)

	if _, ok := interface{}(wrapped).(http.Flusher); !ok {
		t.Fatal("wrapped writer does not implement http.Flusher")
	}

	StreamOpenAIPassthrough(wrapped, io.NopCloser(strings.NewReader("data: hello\n\n")))

	if got := base.Header().Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("Content-Type = %q, want %q", got, "text/event-stream")
	}
	if got := base.body.String(); got != "data: hello\n\n" {
		t.Fatalf("body = %q, want %q", got, "data: hello\n\n")
	}
	if base.flushes == 0 {
		t.Fatal("expected streaming writer to flush")
	}
	if got := wrapped.StatusCode(); got != http.StatusOK {
		t.Fatalf("StatusCode() = %d, want %d", got, http.StatusOK)
	}
}

func TestBoundedPublicModelLabel(t *testing.T) {
	handler := newTestProxyHandler(t, func(http.ResponseWriter, *http.Request) {})
	registerKnownMetricsTestModels(t, handler, "gpt-4.1")

	if got := handler.boundedPublicModelLabel("gpt-4.1", "/chat/completions"); got != "gpt-4.1" {
		t.Fatalf("known model label = %q, want %q", got, "gpt-4.1")
	}
	if got := handler.boundedPublicModelLabel("sk-live-secret-value", "/chat/completions"); got != metricsUnknownLabel {
		t.Fatalf("unknown model label = %q, want %q", got, metricsUnknownLabel)
	}
	if got := handler.boundedPublicModelLabel("   ", "/chat/completions"); got != metricsUnknownLabel {
		t.Fatalf("blank model label = %q, want %q", got, metricsUnknownLabel)
	}
}

func TestHandleResponses_BoundsUnknownPublicModelMetricLabel(t *testing.T) {
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
			"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}
		}`)
	})
	handler.metrics = metrics

	req, err := http.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"sk-live-secret-value",
		"input":"hello"
	}`))
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	w := new(flushTrackingResponseWriter)

	handler.HandleResponses(w, req)

	if got := testutil.ToFloat64(metrics.requestsTotal.WithLabelValues("responses", "copilot", metricsUnknownLabel, "200")); got != 1 {
		t.Fatalf("unknown-model vekil_requests_total = %v, want 1", got)
	}
	if got := testutil.ToFloat64(metrics.tokensTotal.WithLabelValues("responses", "copilot", metricsUnknownLabel, tokenDirectionPrompt)); got != 3 {
		t.Fatalf("unknown-model prompt tokens = %v, want 3", got)
	}
	if got := testutil.ToFloat64(metrics.tokensTotal.WithLabelValues("responses", "copilot", metricsUnknownLabel, tokenDirectionCompletion)); got != 2 {
		t.Fatalf("unknown-model completion tokens = %v, want 2", got)
	}
}

func TestWriteBufferedUpstreamResponse_StreamsLargeBodyWithoutFullBuffering(t *testing.T) {
	largeBody := strings.Repeat("x", maxBufferedUpstreamBody+1024)
	resp := &http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(largeBody)),
	}
	rec := httptest.NewRecorder()

	body, err := writeBufferedUpstreamResponse(rec, resp)
	if err != nil {
		t.Fatalf("writeBufferedUpstreamResponse() error = %v", err)
	}
	if body != nil {
		t.Fatal("expected no buffered body when upstream response exceeds capture limit")
	}
	if got := rec.Body.Len(); got != len(largeBody) {
		t.Fatalf("streamed body length = %d, want %d", got, len(largeBody))
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want %q", got, "application/json")
	}
}
