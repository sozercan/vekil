package server

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	io_prometheus_client "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestMetricsEndpointExposesPrometheusText(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
		WithMetricsEnabled(true),
		WithBuildInfoVersion("1.2.3"),
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer stopTestServer(t, srv)

	resp, err := http.Get(fmt.Sprintf("http://%s/healthz", srv.Addr()))
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = http.Get(fmt.Sprintf("http://%s/metrics", srv.Addr()))
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/plain") {
		t.Fatalf("Content-Type = %q, want Prometheus text exposition", got)
	}

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		t.Fatalf("failed to parse Prometheus exposition: %v", err)
	}

	if _, ok := families["go_goroutines"]; !ok {
		t.Fatalf("metrics output missing go_goroutines")
	}

	buildInfo := families["vekil_build_info"]
	if buildInfo == nil || len(buildInfo.Metric) != 1 {
		t.Fatalf("metrics output missing vekil_build_info")
	}
	labels := metricLabels(buildInfo.Metric[0])
	if got := labels["version"]; got != "1.2.3" {
		t.Fatalf("vekil_build_info version = %q, want %q", got, "1.2.3")
	}

	requests := families["vekil_http_requests_total"]
	if requests == nil {
		t.Fatalf("metrics output missing vekil_http_requests_total")
	}
	var foundHealthz bool
	for _, metric := range requests.Metric {
		labels := metricLabels(metric)
		if labels["method"] == http.MethodGet && labels["route"] == "/healthz" && labels["code"] == "200" {
			foundHealthz = true
			break
		}
	}
	if !foundHealthz {
		t.Fatalf("metrics output missing GET /healthz counter")
	}
}

func TestMetricsEndpointDoesNotExposeSensitiveLabelValues(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
		WithMetricsEnabled(true),
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer stopTestServer(t, srv)

	const (
		userValue   = "alice@example.com"
		keyValue    = "sk-test-123"
		promptValue = "tell me the launch code"
	)

	query := url.Values{}
	query.Set("user", userValue)
	query.Set("api_key", keyValue)
	query.Set("prompt", promptValue)
	req, err := http.NewRequest(http.MethodGet, fmt.Sprintf("http://%s/healthz?%s", srv.Addr(), query.Encode()), nil)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+keyValue)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	_ = resp.Body.Close()

	metricsResp, err := http.Get(fmt.Sprintf("http://%s/metrics", srv.Addr()))
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer metricsResp.Body.Close()

	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("failed to read metrics response: %v", err)
	}
	metricsText := string(body)
	for _, disallowed := range []string{userValue, keyValue, promptValue, "Authorization"} {
		if strings.Contains(metricsText, disallowed) {
			t.Fatalf("metrics output unexpectedly contained %q", disallowed)
		}
	}

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(metricsText))
	if err != nil {
		t.Fatalf("failed to parse Prometheus exposition: %v", err)
	}

	requests := families["vekil_http_requests_total"]
	if requests == nil {
		t.Fatalf("metrics output missing vekil_http_requests_total")
	}
	for _, metric := range requests.Metric {
		for _, label := range metric.GetLabel() {
			switch label.GetName() {
			case "method", "route", "code":
			default:
				t.Fatalf("unexpected label %q on vekil_http_requests_total", label.GetName())
			}
		}
	}
}

func stopTestServer(t *testing.T, srv *Server) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("failed to stop server: %v", err)
	}
}

func metricLabels(metric interface {
	GetLabel() []*io_prometheus_client.LabelPair
}) map[string]string {
	labels := make(map[string]string)
	for _, label := range metric.GetLabel() {
		labels[label.GetName()] = label.GetValue()
	}
	return labels
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
		t.Fatalf("failed to initialize server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer stopTestServer(t, srv)

	resp, err := http.Get(fmt.Sprintf("http://%s/metrics", srv.Addr()))
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /metrics status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func TestMetricsEndpointIsDisabledByDefault(t *testing.T) {
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
	defer stopTestServer(t, srv)

	resp, err := http.Get(fmt.Sprintf("http://%s/metrics", srv.Addr()))
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /metrics status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}
