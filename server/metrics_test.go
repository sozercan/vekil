package server

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	prommodel "github.com/prometheus/common/model"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestMetricsEndpointExposesPrometheusMetrics(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Errorf("failed to stop server: %v", err)
		}
	})

	const (
		sensitiveUser   = "user_demo_123"
		sensitiveKey    = "sk-live-demo-123"
		sensitivePrompt = "never_log_this_prompt"
	)

	healthResp, err := http.Get("http://" + srv.Addr() + "/healthz?user=" + sensitiveUser + "&key=" + sensitiveKey + "&prompt=" + sensitivePrompt)
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, healthResp.Body)
	_ = healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", healthResp.StatusCode, http.StatusOK)
	}

	compactResp, err := http.Post(
		"http://"+srv.Addr()+"/v1/responses/compact",
		"application/json",
		strings.NewReader("user="+sensitiveUser+"\nkey="+sensitiveKey+"\nprompt="+sensitivePrompt),
	)
	if err != nil {
		t.Fatalf("POST /v1/responses/compact failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, compactResp.Body)
	_ = compactResp.Body.Close()
	if compactResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /v1/responses/compact status = %d, want %d", compactResp.StatusCode, http.StatusBadRequest)
	}

	metricsResp, err := http.Get("http://" + srv.Addr() + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer func() { _ = metricsResp.Body.Close() }()

	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", metricsResp.StatusCode, http.StatusOK)
	}
	if got := metricsResp.Header.Get("Content-Type"); !strings.Contains(got, "text/plain") || !strings.Contains(got, "version=0.0.4") {
		t.Fatalf("GET /metrics Content-Type = %q, want Prometheus text exposition", got)
	}

	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("failed to read /metrics response: %v", err)
	}

	parser := expfmt.NewTextParser(prommodel.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("failed to parse /metrics response: %v\n%s", err, string(body))
	}

	buildInfo, ok := families["vekil_build_info"]
	if !ok {
		t.Fatalf("/metrics missing vekil_build_info\n%s", string(body))
	}
	if len(buildInfo.Metric) != 1 {
		t.Fatalf("vekil_build_info sample count = %d, want 1", len(buildInfo.Metric))
	}
	if got := labelValue(buildInfo.Metric[0], "version"); got == "" {
		t.Fatalf("vekil_build_info version label is empty")
	}
	if got := labelValue(buildInfo.Metric[0], "goversion"); got == "" {
		t.Fatalf("vekil_build_info goversion label is empty")
	}

	if _, ok := families["go_goroutines"]; !ok {
		t.Fatalf("/metrics missing go_goroutines\n%s", string(body))
	}

	requests, ok := families["vekil_http_requests_total"]
	if !ok {
		t.Fatalf("/metrics missing vekil_http_requests_total\n%s", string(body))
	}
	if got := counterValueForHandler(requests, "healthz"); got != 1 {
		t.Fatalf("vekil_http_requests_total{handler=%q} = %v, want 1", "healthz", got)
	}
	if got := counterValueForHandler(requests, "responses_compact"); got != 1 {
		t.Fatalf("vekil_http_requests_total{handler=%q} = %v, want 1", "responses_compact", got)
	}

	for _, forbidden := range []string{sensitiveUser, sensitiveKey, sensitivePrompt} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("/metrics leaked request content %q", forbidden)
		}
	}
}

func TestNew_CanDisableMetricsEndpoint(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
		WithMetricsEnabled(false),
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("GET /metrics status = %d, want %d when metrics disabled", w.Code, http.StatusNotFound)
	}
}

func counterValueForHandler(family *dto.MetricFamily, handler string) float64 {
	for _, metric := range family.Metric {
		if labelValue(metric, "handler") == handler {
			return metric.GetCounter().GetValue()
		}
	}
	return 0
}

func labelValue(metric *dto.Metric, name string) string {
	for _, label := range metric.GetLabel() {
		if label.GetName() == name {
			return label.GetValue()
		}
	}
	return ""
}
