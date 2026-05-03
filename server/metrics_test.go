package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestMetricsEndpointExposesPrometheusMetrics(t *testing.T) {
	ts := newMetricsTestServer(t)

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	metricsResp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	body, families := readMetricsResponse(t, metricsResp)

	contentType := metricsResp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/plain") || !strings.Contains(contentType, "version=0.0.4") {
		t.Fatalf("Content-Type = %q, want Prometheus text exposition", contentType)
	}

	if len(body) == 0 {
		t.Fatal("/metrics returned an empty body")
	}

	if _, ok := families["go_goroutines"]; !ok {
		t.Fatal("metrics missing go_goroutines")
	}

	buildInfo := families["vekil_build_info"]
	if buildInfo == nil {
		t.Fatal("metrics missing vekil_build_info")
	}
	if len(buildInfo.Metric) != 1 {
		t.Fatalf("vekil_build_info metric count = %d, want 1", len(buildInfo.Metric))
	}
	if got := buildInfo.Metric[0].GetGauge().GetValue(); got != 1 {
		t.Fatalf("vekil_build_info value = %v, want 1", got)
	}
	buildLabels := labelMap(buildInfo.Metric[0])
	for _, label := range []string{"version", "revision", "go_version"} {
		if strings.TrimSpace(buildLabels[label]) == "" {
			t.Fatalf("vekil_build_info missing %q label value", label)
		}
	}

	requests := families["vekil_http_requests_total"]
	if requests == nil {
		t.Fatal("metrics missing vekil_http_requests_total")
	}

	foundHealthz := false
	for _, metric := range requests.Metric {
		labels := labelMap(metric)
		if labels["handler"] == "/healthz" && labels["method"] == "get" {
			foundHealthz = true
			if got := metric.GetCounter().GetValue(); got < 1 {
				t.Fatalf("/healthz counter = %v, want at least 1", got)
			}
		}
	}
	if !foundHealthz {
		t.Fatal("metrics missing /healthz request counter")
	}
}

func TestMetricsEndpointDoesNotUseRequestContentAsLabels(t *testing.T) {
	ts := newMetricsTestServer(t)

	const (
		userValue   = "metrics-user-should-not-appear@example.invalid"
		apiKeyValue = "sk-test-metrics-should-not-appear"
		promptValue = "prompt-should-not-appear-in-metrics"
	)

	req, err := http.NewRequest(
		http.MethodGet,
		ts.URL+"/healthz?user="+url.QueryEscape(userValue)+"&prompt="+url.QueryEscape(promptValue),
		strings.NewReader(promptValue),
	)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+apiKeyValue)
	req.Header.Set("X-API-Key", apiKeyValue)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()

	body, families := scrapeMetrics(t, ts.URL)
	for _, forbidden := range []string{userValue, apiKeyValue, promptValue} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("/metrics unexpectedly contains request content %q", forbidden)
		}
	}

	requests := families["vekil_http_requests_total"]
	if requests == nil {
		t.Fatal("metrics missing vekil_http_requests_total")
	}
	for _, metric := range requests.Metric {
		labels := labelMap(metric)
		for name := range labels {
			switch name {
			case "handler", "method":
			default:
				t.Fatalf("unexpected request metric label %q", name)
			}
		}
		if strings.Contains(labels["handler"], "?") {
			t.Fatalf("handler label contains raw request path %q", labels["handler"])
		}
	}
}

func TestMetricsEndpointCanBeDisabled(t *testing.T) {
	ts := newMetricsTestServer(t, WithMetricsEnabled(false))

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
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
		t.Fatalf("failed to initialize server: %v", err)
	}

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func scrapeMetrics(t *testing.T, baseURL string) (string, map[string]*dto.MetricFamily) {
	t.Helper()

	resp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	return readMetricsResponse(t, resp)
}

func readMetricsResponse(t *testing.T, resp *http.Response) (string, map[string]*dto.MetricFamily) {
	t.Helper()
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("GET /metrics status = %d, want %d; body=%q", resp.StatusCode, http.StatusOK, string(body))
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read /metrics response: %v", err)
	}

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("failed to parse Prometheus metrics: %v", err)
	}

	return string(body), families
}

func labelMap(metric *dto.Metric) map[string]string {
	labels := make(map[string]string, len(metric.GetLabel()))
	for _, label := range metric.GetLabel() {
		labels[label.GetName()] = label.GetValue()
	}
	return labels
}
