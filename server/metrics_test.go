package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
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
		t.Fatalf("New() error = %v", err)
	}

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	healthzResp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz error = %v", err)
	}
	defer func() { _ = healthzResp.Body.Close() }()

	if healthzResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", healthzResp.StatusCode)
	}

	families, body, resp := fetchMetricFamilies(t, ts.URL+"/metrics")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("GET /metrics Content-Type = %q, want Prometheus text exposition", contentType)
	}

	if _, ok := families["go_goroutines"]; !ok {
		t.Fatalf("expected go_goroutines metric family in /metrics output")
	}

	buildInfo := families["vekil_build_info"]
	if buildInfo == nil || len(buildInfo.Metric) == 0 {
		t.Fatalf("expected vekil_build_info metric family in /metrics output")
	}
	buildLabels := labelMap(buildInfo.Metric[0])
	for _, key := range []string{"version", "revision", "go_version"} {
		if got := strings.TrimSpace(buildLabels[key]); got == "" {
			t.Fatalf("vekil_build_info missing %q label value: %#v", key, buildLabels)
		}
	}

	requests := families["vekil_http_requests_total"]
	if requests == nil {
		t.Fatalf("expected vekil_http_requests_total metric family in /metrics output\n%s", body)
	}
	assertBoundedRequestLabels(t, requests)
	if !hasRequestMetric(requests, "/healthz", "200") {
		t.Fatalf("expected vekil_http_requests_total sample for GET /healthz, got:\n%s", body)
	}
}

func TestMetricsEndpointCanBeDisabled(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
		WithMetricsEnabled(false),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /metrics status = %d, want 404 when metrics are disabled", resp.StatusCode)
	}
}

func TestServerMetricsRequestLabelsStayBounded(t *testing.T) {
	metrics := newServerMetrics()

	mux := http.NewServeMux()
	mux.Handle(
		"POST /test",
		metrics.instrument("/test", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			w.WriteHeader(http.StatusAccepted)
		})),
	)
	mux.Handle("GET /metrics", metrics.handler)

	ts := httptest.NewServer(mux)
	defer ts.Close()

	req, err := http.NewRequest(
		http.MethodPost,
		ts.URL+"/test?user=alice@example.com&virtual-key=vk_live_123&prompt=super-secret",
		strings.NewReader(`{"user":"alice@example.com","api_key":"sk-secret","prompt":"super-secret"}`),
	)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer sk-header-secret")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /test error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("POST /test status = %d, want 202", resp.StatusCode)
	}

	families, body, metricsResp := fetchMetricFamilies(t, ts.URL+"/metrics")
	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", metricsResp.StatusCode)
	}

	requests := families["vekil_http_requests_total"]
	if requests == nil {
		t.Fatalf("expected vekil_http_requests_total metric family in /metrics output\n%s", body)
	}
	assertBoundedRequestLabels(t, requests)
	if !hasRequestMetric(requests, "/test", "202") {
		t.Fatalf("expected vekil_http_requests_total sample for POST /test, got:\n%s", body)
	}

	for _, secret := range []string{
		"alice@example.com",
		"vk_live_123",
		"super-secret",
		"sk-secret",
		"sk-header-secret",
	} {
		if strings.Contains(body, secret) {
			t.Fatalf("metrics output leaked request content %q:\n%s", secret, body)
		}
	}
}

func fetchMetricFamilies(t *testing.T, metricsURL string) (map[string]*dto.MetricFamily, string, *http.Response) {
	t.Helper()

	resp, err := http.Get(metricsURL)
	if err != nil {
		t.Fatalf("GET %s error = %v", metricsURL, err)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = resp.Body.Close()
		t.Fatalf("read %s body error = %v", metricsURL, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close %s body error = %v", metricsURL, err)
	}

	parser := expfmt.TextParser{}
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("parse Prometheus exposition error = %v\n%s", err, body)
	}

	resp.Body = io.NopCloser(bytes.NewReader(body))
	return families, string(body), resp
}

func assertBoundedRequestLabels(t *testing.T, family *dto.MetricFamily) {
	t.Helper()

	for _, metric := range family.Metric {
		labels := labelMap(metric)
		if len(labels) != 3 {
			t.Fatalf("request metric labels = %#v, want exactly route/method/code", labels)
		}
		for _, key := range []string{"route", "method", "code"} {
			if _, ok := labels[key]; !ok {
				t.Fatalf("request metric labels = %#v, missing %q", labels, key)
			}
		}
	}
}

func hasRequestMetric(family *dto.MetricFamily, route, code string) bool {
	for _, metric := range family.Metric {
		labels := labelMap(metric)
		if labels["route"] == route && labels["code"] == code {
			return true
		}
	}
	return false
}

func labelMap(metric *dto.Metric) map[string]string {
	labels := make(map[string]string, len(metric.Label))
	for _, label := range metric.Label {
		labels[label.GetName()] = label.GetValue()
	}
	return labels
}
