package server

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
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

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/healthz?prompt=prompt-secret&user=user-secret&virtual-key=vk-secret", nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer key-secret")

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	_ = resp.Body.Close()

	metricsReq, err := http.NewRequest(http.MethodGet, ts.URL+"/metrics", nil)
	if err != nil {
		t.Fatalf("failed to create metrics request: %v", err)
	}
	metricsReq.Header.Set("Accept", "text/plain; version=0.0.4")

	metricsResp, err := ts.Client().Do(metricsReq)
	if err != nil {
		t.Fatalf("metrics request failed: %v", err)
	}
	defer func() { _ = metricsResp.Body.Close() }()

	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", metricsResp.StatusCode)
	}
	contentType := metricsResp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/plain") && !strings.Contains(contentType, "openmetrics-text") {
		t.Fatalf("/metrics content type = %q, want Prometheus text exposition", contentType)
	}

	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("failed to read metrics body: %v", err)
	}

	bodyText := string(body)
	for _, secret := range []string{"prompt-secret", "user-secret", "vk-secret", "key-secret"} {
		if strings.Contains(bodyText, secret) {
			t.Fatalf("metrics body unexpectedly exposed %q", secret)
		}
	}

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("failed to parse metrics exposition: %v", err)
	}

	if _, ok := families["go_goroutines"]; !ok {
		t.Fatalf("go_goroutines metric not found")
	}

	buildInfo := families["vekil_build_info"]
	if buildInfo == nil {
		t.Fatalf("vekil_build_info metric not found")
	}
	if got := len(buildInfo.Metric); got != 1 {
		t.Fatalf("vekil_build_info samples = %d, want 1", got)
	}
	buildLabels := metricLabels(buildInfo.Metric[0])
	if buildLabels["version"] == "" {
		t.Fatalf("vekil_build_info version label is empty")
	}
	if buildLabels["go_version"] == "" {
		t.Fatalf("vekil_build_info go_version label is empty")
	}
	if got := buildInfo.Metric[0].GetGauge().GetValue(); got != 1 {
		t.Fatalf("vekil_build_info gauge value = %v, want 1", got)
	}

	requests := families["vekil_http_requests_total"]
	if requests == nil {
		t.Fatalf("vekil_http_requests_total metric not found")
	}
	foundHealthz := false
	for _, metric := range requests.Metric {
		labels := metricLabels(metric)
		if labels["route"] == "GET /healthz" && labels["code"] == "200" {
			if metric.GetCounter().GetValue() < 1 {
				t.Fatalf("GET /healthz counter = %v, want >= 1", metric.GetCounter().GetValue())
			}
			foundHealthz = true
		}
	}
	if !foundHealthz {
		t.Fatalf("vekil_http_requests_total missing GET /healthz 200 sample")
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
		t.Fatalf("failed to initialize server: %v", err)
	}

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("metrics request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/metrics status = %d, want 404 when disabled", resp.StatusCode)
	}
}

func TestStartServesHealthzAndMetricsWithoutCachedAuth(t *testing.T) {
	authenticator, err := auth.NewAuthenticator(t.TempDir())
	if err != nil {
		t.Fatalf("failed to initialize authenticator: %v", err)
	}
	authenticator.DisableAutoDeviceFlow = true

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve port: %v", err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err := listener.Close(); err != nil {
		t.Fatalf("failed to release reserved port: %v", err)
	}

	srv, err := New(
		authenticator,
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		strconv.Itoa(port),
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Fatalf("failed to stop server: %v", err)
		}
	}()

	client := &http.Client{Timeout: 2 * time.Second}
	for _, path := range []string{"/healthz", "/metrics"} {
		resp, err := client.Get("http://127.0.0.1:" + strconv.Itoa(port) + path)
		if err != nil {
			t.Fatalf("GET %s failed: %v", path, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("failed to read %s response: %v", path, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s status = %d, want 200", path, resp.StatusCode)
		}
		if len(body) == 0 {
			t.Fatalf("%s returned an empty body", path)
		}
	}
}

func metricLabels(metric *dto.Metric) map[string]string {
	labels := make(map[string]string, len(metric.Label))
	for _, label := range metric.Label {
		labels[label.GetName()] = label.GetValue()
	}
	return labels
}
