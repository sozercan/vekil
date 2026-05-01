package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestNew_ExposesPrometheusMetricsEndpoint(t *testing.T) {
	ts := newMetricsTestServer(t, WithBuildVersion("1.2.3-test"))

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	_ = resp.Body.Close()

	contentType, families, _ := scrapeMetrics(t, ts)

	for _, want := range []string{
		"text/plain",
		"version=0.0.4",
	} {
		if !strings.Contains(contentType, want) {
			t.Fatalf("Content-Type = %q, want substring %q", contentType, want)
		}
	}

	for _, name := range []string{
		"go_goroutines",
		"process_start_time_seconds",
		"vekil_build_info",
		"vekil_http_requests_total",
		"vekil_http_request_duration_seconds",
	} {
		if _, ok := families[name]; !ok {
			t.Fatalf("expected metric family %q in /metrics output", name)
		}
	}

	buildInfo := families["vekil_build_info"]
	if len(buildInfo.Metric) != 1 {
		t.Fatalf("vekil_build_info metric count = %d, want 1", len(buildInfo.Metric))
	}
	if got := metricLabels(buildInfo.Metric[0])["version"]; got != "1.2.3-test" {
		t.Fatalf("vekil_build_info version label = %q, want %q", got, "1.2.3-test")
	}
	if got := buildInfo.Metric[0].GetGauge().GetValue(); got != 1 {
		t.Fatalf("vekil_build_info value = %v, want 1", got)
	}

	if !hasMetricWithLabels(families["vekil_http_requests_total"], map[string]string{
		"handler": "healthz",
		"code":    "200",
	}) {
		t.Fatalf("vekil_http_requests_total missing healthz 200 series")
	}
}

func TestMetricsUseOnlyBoundedLabels(t *testing.T) {
	ts := newMetricsTestServer(t)

	const (
		userSentinel   = "metrics-user-sentinel-77"
		apiKeySentinel = "metrics-key-sentinel-77"
		promptSentinel = "metrics-prompt-sentinel-77"
	)

	req, err := http.NewRequest(
		http.MethodPost,
		ts.URL+"/v1/responses/compact?user="+url.QueryEscape(userSentinel),
		strings.NewReader(`{"prompt":"`+promptSentinel+`"`),
	)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+apiKeySentinel)
	req.Header.Set("X-API-Key", apiKeySentinel)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/responses/compact: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("POST /v1/responses/compact status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}
	_ = resp.Body.Close()

	_, families, body := scrapeMetrics(t, ts)

	for _, forbidden := range []string{userSentinel, apiKeySentinel, promptSentinel} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("/metrics output unexpectedly contained request data %q", forbidden)
		}
	}

	assertOnlyLabelNames(t, families["vekil_http_requests_total"], "handler", "code", "method")
	assertOnlyLabelNames(t, families["vekil_http_request_duration_seconds"], "handler", "method")

	if !hasMetricWithLabels(families["vekil_http_requests_total"], map[string]string{
		"handler": "responses_compact",
		"code":    "400",
	}) {
		t.Fatalf("vekil_http_requests_total missing responses_compact 400 series")
	}
}

func TestNew_DisablesMetricsEndpointWhenConfigured(t *testing.T) {
	ts := newMetricsTestServer(t, WithMetricsEnabled(false))

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /metrics status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func newMetricsTestServer(t *testing.T, opts ...Option) *httptest.Server {
	t.Helper()

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
		opts...,
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func scrapeMetrics(t *testing.T, ts *httptest.Server) (string, map[string]*dto.MetricFamily, string) {
	t.Helper()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /metrics status = %d, want %d: %s", resp.StatusCode, http.StatusOK, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}

	parser := expfmt.TextParser{}
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("parse /metrics exposition: %v\n%s", err, string(body))
	}

	return resp.Header.Get("Content-Type"), families, string(body)
}

func metricLabels(metric *dto.Metric) map[string]string {
	labels := make(map[string]string, len(metric.GetLabel()))
	for _, label := range metric.GetLabel() {
		labels[label.GetName()] = label.GetValue()
	}
	return labels
}

func hasMetricWithLabels(family *dto.MetricFamily, want map[string]string) bool {
	if family == nil {
		return false
	}

	for _, metric := range family.Metric {
		labels := metricLabels(metric)
		match := true
		for name, value := range want {
			if labels[name] != value {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}

	return false
}

func assertOnlyLabelNames(t *testing.T, family *dto.MetricFamily, allowed ...string) {
	t.Helper()

	if family == nil {
		t.Fatal("expected metric family to be present")
	}

	for _, metric := range family.Metric {
		for _, label := range metric.GetLabel() {
			if !slices.Contains(allowed, label.GetName()) {
				t.Fatalf("%s used unexpected label %q", family.GetName(), label.GetName())
			}
		}
	}
}
