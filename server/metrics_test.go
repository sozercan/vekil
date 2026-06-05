package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMetricsHandlerReportsRequestsAndInflight(t *testing.T) {
	metrics := newMetrics()
	started := make(chan struct{})
	release := make(chan struct{})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /slow", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("GET /metrics", metrics.handle)

	handler := metrics.instrument(mux)

	done := make(chan struct{})
	go func() {
		defer close(done)
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/slow", nil))
	}()

	<-started

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	body := rr.Body.String()
	if got, want := rr.Code, http.StatusOK; got != want {
		t.Fatalf("status = %d, want %d", got, want)
	}
	if got, want := rr.Header().Get("Content-Type"), "text/plain; version=0.0.4; charset=utf-8"; got != want {
		t.Fatalf("Content-Type = %q, want %q", got, want)
	}
	if !strings.Contains(body, requestsMetricName+" 1") {
		t.Fatalf("expected request counter in metrics output, got %q", body)
	}
	if !strings.Contains(body, inflightMetricName+" 1") {
		t.Fatalf("expected in-flight gauge while request is active, got %q", body)
	}

	close(release)
	<-done

	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if body := rr.Body.String(); !strings.Contains(body, inflightMetricName+" 0") {
		t.Fatalf("expected in-flight gauge to return to zero, got %q", body)
	}
}
