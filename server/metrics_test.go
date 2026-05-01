package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

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

	client := &http.Client{Timeout: 5 * time.Second}

	req, err := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+"/healthz?user=alice@example.com&key=sk-test-value&prompt=top-secret-prompt", nil)
	if err != nil {
		t.Fatalf("failed to create healthz request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer sk-live-secret")

	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	if got, want := resp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("GET /healthz status = %d, want %d", got, want)
	}
	_ = resp.Body.Close()

	metricsResp, err := client.Get("http://" + srv.Addr() + "/metrics")
	if err != nil {
		t.Fatalf("metrics request failed: %v", err)
	}
	defer func() { _ = metricsResp.Body.Close() }()

	if got, want := metricsResp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("GET /metrics status = %d, want %d", got, want)
	}
	if contentType := metricsResp.Header.Get("Content-Type"); !strings.Contains(contentType, "version=0.0.4") {
		t.Fatalf("GET /metrics Content-Type = %q, want Prometheus text exposition", contentType)
	}

	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("failed to read metrics response: %v", err)
	}
	bodyText := string(body)

	families, err := new(expfmt.TextParser).TextToMetricFamilies(strings.NewReader(bodyText))
	if err != nil {
		t.Fatalf("failed to parse Prometheus exposition: %v", err)
	}

	if _, ok := families["go_goroutines"]; !ok {
		t.Fatalf("metrics output missing go_goroutines")
	}

	buildInfo, ok := families["vekil_build_info"]
	if !ok {
		t.Fatalf("metrics output missing vekil_build_info")
	}
	if len(buildInfo.GetMetric()) != 1 || buildInfo.GetMetric()[0].GetGauge().GetValue() != 1 {
		t.Fatalf("vekil_build_info gauge = %+v, want one sample with value 1", buildInfo.GetMetric())
	}

	requests, ok := families["vekil_http_requests_total"]
	if !ok {
		t.Fatalf("metrics output missing vekil_http_requests_total")
	}

	foundHealthzSample := false
	for _, metric := range requests.GetMetric() {
		labels := map[string]string{}
		for _, label := range metric.GetLabel() {
			labels[label.GetName()] = label.GetValue()
		}
		if labels["handler"] == "healthz" && labels["method"] == http.MethodGet && labels["code"] == "200" && metric.GetCounter().GetValue() >= 1 {
			foundHealthzSample = true
			break
		}
	}
	if !foundHealthzSample {
		t.Fatalf("metrics output missing healthz request counter sample: %+v", requests.GetMetric())
	}

	for _, forbidden := range []string{
		"alice@example.com",
		"sk-test-value",
		"top-secret-prompt",
		"sk-live-secret",
	} {
		if strings.Contains(bodyText, forbidden) {
			t.Fatalf("metrics output leaked request content %q", forbidden)
		}
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

	client := &http.Client{Timeout: 5 * time.Second}
	resp, err := client.Get("http://" + srv.Addr() + "/metrics")
	if err != nil {
		t.Fatalf("metrics request failed: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got, want := resp.StatusCode, http.StatusNotFound; got != want {
		t.Fatalf("GET /metrics status = %d, want %d when metrics are disabled", got, want)
	}
}
