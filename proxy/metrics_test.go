package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func metricsBodyHasLine(body, metric string, parts ...string) bool {
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, metric+"{") {
			continue
		}
		matches := true
		for _, part := range parts {
			if !strings.Contains(line, part) {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func TestInstrumentHandler_RecordsRequestAndBuildMetrics(t *testing.T) {
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithMetricsEnabled(true),
		WithBuildVersion("1.2.3"),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	wrapped := handler.InstrumentHandler("/v1/test", func(w http.ResponseWriter, r *http.Request) {
		scope := requestMetricsFromContext(r.Context())
		scope.SetProvider("copilot")
		scope.SetPublicModel("gpt-4.1")
		scope.observeTokens("input", 11)
		scope.observeTokens("output", 7)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{"ok":true}`))
	})

	req := httptest.NewRequest(http.MethodPost, "/v1/test", strings.NewReader(`{"model":"gpt-4.1"}`))
	rec := httptest.NewRecorder()
	wrapped(rec, req)

	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRec := httptest.NewRecorder()
	handler.MetricsHandler().ServeHTTP(metricsRec, metricsReq)

	body := metricsRec.Body.String()
	if !metricsBodyHasLine(body, "vekil_requests_total", `endpoint="/v1/test"`, `provider="copilot"`, `public_model="gpt-4.1"`, `status="success"`, `code="201"`) {
		t.Fatalf("metrics output missing request counter:\n%s", body)
	}
	if !metricsBodyHasLine(body, "vekil_tokens_total", `endpoint="/v1/test"`, `provider="copilot"`, `public_model="gpt-4.1"`, `direction="input"`) {
		t.Fatalf("metrics output missing input token counter:\n%s", body)
	}
	if !metricsBodyHasLine(body, "vekil_tokens_total", `endpoint="/v1/test"`, `provider="copilot"`, `public_model="gpt-4.1"`, `direction="output"`) {
		t.Fatalf("metrics output missing output token counter:\n%s", body)
	}
	if !strings.Contains(body, `vekil_build_info{version="1.2.3"} 1`) {
		t.Fatalf("metrics output missing build info:\n%s", body)
	}
}

func TestDoWithRetry_RecordsRetryMetrics(t *testing.T) {
	attempts := 0
	handler := newRoundTripTestProxyHandler(t, func(req *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return &http.Response{
				StatusCode: http.StatusTooManyRequests,
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"error":"retry"}`)),
			}, nil
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`ok`)),
		}, nil
	})
	handler.metrics = newProxyMetrics("dev")

	scope := handler.metrics.beginRequest("/v1/test")
	scope.SetProvider("copilot")
	scope.SetPublicModel("gpt-4.1")
	ctx := withRequestMetricsScope(context.Background(), scope)

	resp, err := handler.doWithRetry(ctx, func() (*http.Request, error) {
		return http.NewRequestWithContext(ctx, http.MethodGet, "http://upstream.test/test", nil)
	})
	if err != nil {
		t.Fatalf("doWithRetry() error = %v", err)
	}
	_ = resp.Body.Close()
	scope.finish(http.StatusOK)

	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRec := httptest.NewRecorder()
	handler.MetricsHandler().ServeHTTP(metricsRec, metricsReq)

	body := metricsRec.Body.String()
	if !metricsBodyHasLine(body, "vekil_retries_total", `endpoint="/v1/test"`, `provider="copilot"`, `public_model="gpt-4.1"`, `reason="status_code"`, `code="429"`) {
		t.Fatalf("metrics output missing retry counter:\n%s", body)
	}
}
