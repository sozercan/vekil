package server

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
)

func TestStart_ReturnsErrorWhenPortInUse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve port: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("failed to close listener: %v", err)
		}
	}()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("expected TCP address, got %T", listener.Addr())
	}

	srv, err := New(auth.NewTestAuthenticator("test-token"), logger.New(logger.ParseLevel("error")), "127.0.0.1", strconv.Itoa(addr.Port))
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}
	err = srv.Start()
	if err == nil {
		t.Fatal("expected Start to fail when port is already in use")
	}
	if srv.IsRunning() {
		t.Fatal("expected server to remain stopped after listen failure")
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("expected address-in-use error, got %v", err)
	}
}

func TestNew_ConfiguresExtendedWriteTimeout(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}

	if got, want := srv.httpServer.WriteTimeout, 65*time.Minute; got != want {
		t.Fatalf("WriteTimeout = %v, want %v", got, want)
	}
}

func TestNew_DerivesWriteTimeoutFromConfiguredProxyHandler(t *testing.T) {
	const customTimeout = 17 * time.Minute

	tests := []struct {
		name string
		opts []Option
	}{
		{
			name: "server wrapper",
			opts: []Option{WithStreamingUpstreamTimeout(customTimeout)},
		},
		{
			name: "proxy option",
			opts: []Option{WithProxyOptions(proxy.WithStreamingUpstreamTimeout(customTimeout))},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := New(
				auth.NewTestAuthenticator("test-token"),
				logger.New(logger.ParseLevel("error")),
				"127.0.0.1",
				"0",
				tc.opts...,
			)
			if err != nil {
				t.Fatalf("failed to initialize server: %v", err)
			}

			if got, want := srv.httpServer.WriteTimeout, customTimeout+5*time.Minute; got != want {
				t.Fatalf("WriteTimeout = %v, want %v", got, want)
			}
		})
	}
}

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

	healthzResp, err := ts.Client().Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = healthzResp.Body.Close()

	resp, err := ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/plain") && !strings.Contains(contentType, "openmetrics-text") {
		t.Fatalf("Content-Type = %q, want Prometheus exposition", contentType)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics response: %v", err)
	}
	families := parseMetricFamilies(t, string(body))

	if families["go_goroutines"] == nil {
		t.Fatal("expected go_goroutines metric to be exposed")
	}

	buildInfo := families["vekil_build_info"]
	if buildInfo == nil {
		t.Fatal("expected vekil_build_info metric to be exposed")
	}
	if got := len(buildInfo.GetMetric()); got != 1 {
		t.Fatalf("vekil_build_info metrics = %d, want 1", got)
	}
	if got := buildInfo.GetMetric()[0].GetGauge().GetValue(); got != 1 {
		t.Fatalf("vekil_build_info value = %v, want 1", got)
	}

	requests := families["vekil_http_requests_total"]
	if requests == nil {
		t.Fatal("expected vekil_http_requests_total metric to be exposed")
	}
	metric := findMetric(t, requests, map[string]string{
		"route":  "/healthz",
		"method": "get",
		"code":   "200",
	})
	if got := metric.GetCounter().GetValue(); got < 1 {
		t.Fatalf("vekil_http_requests_total{route=/healthz,method=get,code=200} = %v, want >= 1", got)
	}
}

func TestMetricsEndpointUsesOnlyBoundedLabels(t *testing.T) {
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

	const (
		secretUser   = "user@example.com"
		secretKey    = "sk-test-secret-value"
		secretPrompt = "prompt-should-not-appear-in-metrics"
	)

	req, err := http.NewRequest(http.MethodGet, ts.URL+"/healthz?user="+url.QueryEscape(secretUser), strings.NewReader(secretPrompt))
	if err != nil {
		t.Fatalf("new GET /healthz request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)

	healthzResp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = healthzResp.Body.Close()

	resp, err := ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /metrics response: %v", err)
	}
	bodyText := string(body)
	for _, secret := range []string{secretUser, secretKey, secretPrompt} {
		if strings.Contains(bodyText, secret) {
			t.Fatalf("/metrics unexpectedly included request data %q", secret)
		}
	}

	requests := parseMetricFamilies(t, bodyText)["vekil_http_requests_total"]
	if requests == nil {
		t.Fatal("expected vekil_http_requests_total metric to be exposed")
	}
	metric := findMetric(t, requests, map[string]string{
		"route":  "/healthz",
		"method": "get",
		"code":   "200",
	})

	labels := labelsFromMetric(metric)
	if got, want := len(labels), 3; got != want {
		t.Fatalf("vekil_http_requests_total labels = %d, want %d", got, want)
	}
	for _, unexpected := range []string{"user", "authorization", "prompt"} {
		if _, ok := labels[unexpected]; ok {
			t.Fatalf("unexpected label %q present in vekil_http_requests_total", unexpected)
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

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	resp, err := ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /metrics status = %d, want %d", resp.StatusCode, http.StatusNotFound)
	}
}

func parseMetricFamilies(t *testing.T, body string) map[string]*dto.MetricFamily {
	t.Helper()

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(body))
	if err != nil {
		t.Fatalf("parse Prometheus exposition: %v", err)
	}
	return families
}

func findMetric(t *testing.T, family *dto.MetricFamily, want map[string]string) *dto.Metric {
	t.Helper()

	for _, metric := range family.GetMetric() {
		labels := labelsFromMetric(metric)
		matched := true
		for key, value := range want {
			if labels[key] != value {
				matched = false
				break
			}
		}
		if matched {
			return metric
		}
	}

	t.Fatalf("metric family %q missing labels %#v", family.GetName(), want)
	return nil
}

func labelsFromMetric(metric *dto.Metric) map[string]string {
	labels := make(map[string]string, len(metric.GetLabel()))
	for _, label := range metric.GetLabel() {
		labels[label.GetName()] = label.GetValue()
	}
	return labels
}
