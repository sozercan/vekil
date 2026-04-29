package server

import (
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
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

func TestMetricsEndpoint_ExposesPrometheusMetrics(t *testing.T) {
	originalBuildVersion := buildVersion
	buildVersion = "1.2.3-test"
	defer func() {
		buildVersion = originalBuildVersion
	}()

	port := reserveTCPPort(t)
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
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
	defer stopServer(t, srv)

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)

	req, err := http.NewRequest(http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		t.Fatalf("failed to create healthz request: %v", err)
	}
	resp, err := doRequestWithRetry(req)
	if err != nil {
		t.Fatalf("failed to call /healthz: %v", err)
	}
	_ = resp.Body.Close()

	metricsReq, err := http.NewRequest(http.MethodGet, baseURL+"/metrics", nil)
	if err != nil {
		t.Fatalf("failed to create /metrics request: %v", err)
	}
	metricsResp, err := doRequestWithRetry(metricsReq)
	if err != nil {
		t.Fatalf("failed to call /metrics: %v", err)
	}
	defer func() { _ = metricsResp.Body.Close() }()

	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want %d", metricsResp.StatusCode, http.StatusOK)
	}
	if contentType := metricsResp.Header.Get("Content-Type"); !strings.Contains(contentType, "text/plain") {
		t.Fatalf("/metrics content-type = %q, want Prometheus text exposition", contentType)
	}

	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("failed to read /metrics response: %v", err)
	}

	parser := expfmt.TextParser{}
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("/metrics was not valid Prometheus exposition: %v", err)
	}

	if _, ok := families["go_goroutines"]; !ok {
		t.Fatalf("expected go_goroutines metric family in /metrics output")
	}

	buildInfo, ok := families["vekil_build_info"]
	if !ok {
		t.Fatalf("expected vekil_build_info metric family in /metrics output")
	}
	if len(buildInfo.Metric) != 1 {
		t.Fatalf("vekil_build_info metric count = %d, want 1", len(buildInfo.Metric))
	}
	if got := metricLabels(buildInfo.Metric[0])["version"]; got != "1.2.3-test" {
		t.Fatalf("vekil_build_info version label = %q, want %q", got, "1.2.3-test")
	}

	requests, ok := families["vekil_http_requests_total"]
	if !ok {
		t.Fatalf("expected vekil_http_requests_total metric family in /metrics output")
	}

	var foundHealthz bool
	for _, metric := range requests.Metric {
		labels := metricLabels(metric)
		if labels["route"] != "/healthz" {
			continue
		}
		if !strings.EqualFold(labels["method"], http.MethodGet) {
			t.Fatalf("/healthz method label = %q, want %q", labels["method"], http.MethodGet)
		}
		if labels["code"] != "200" {
			t.Fatalf("/healthz code label = %q, want 200", labels["code"])
		}
		foundHealthz = true
	}
	if !foundHealthz {
		t.Fatalf("expected /healthz request to be counted in vekil_http_requests_total")
	}
}

func TestMetricsEndpoint_DoesNotExposeRequestSecretsAsLabels(t *testing.T) {
	const (
		secretUser   = "user-top-secret"
		secretKey    = "key-top-secret"
		secretPrompt = "prompt-top-secret"
	)

	port := reserveTCPPort(t)
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
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
	defer stopServer(t, srv)

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)

	req, err := http.NewRequest(http.MethodGet, baseURL+"/healthz?user="+secretUser+"&prompt="+secretPrompt, nil)
	if err != nil {
		t.Fatalf("failed to create healthz request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	req.Header.Set("X-API-Key", secretKey)
	req.Header.Set("X-User", secretUser)

	resp, err := doRequestWithRetry(req)
	if err != nil {
		t.Fatalf("failed to call /healthz: %v", err)
	}
	_ = resp.Body.Close()

	metricsReq, err := http.NewRequest(http.MethodGet, baseURL+"/metrics", nil)
	if err != nil {
		t.Fatalf("failed to create /metrics request: %v", err)
	}
	metricsResp, err := doRequestWithRetry(metricsReq)
	if err != nil {
		t.Fatalf("failed to call /metrics: %v", err)
	}
	defer func() { _ = metricsResp.Body.Close() }()

	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("failed to read /metrics response: %v", err)
	}
	text := string(body)
	for _, secret := range []string{secretUser, secretKey, secretPrompt} {
		if strings.Contains(text, secret) {
			t.Fatalf("/metrics unexpectedly contained request secret %q", secret)
		}
	}

	parser := expfmt.TextParser{}
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("/metrics was not valid Prometheus exposition: %v", err)
	}

	requests, ok := families["vekil_http_requests_total"]
	if !ok {
		t.Fatalf("expected vekil_http_requests_total metric family in /metrics output")
	}
	for _, metric := range requests.Metric {
		for name := range metricLabels(metric) {
			switch name {
			case "route", "method", "code":
			default:
				t.Fatalf("unexpected label %q on vekil_http_requests_total", name)
			}
		}
	}
}

func TestMetricsEndpoint_CanBeDisabled(t *testing.T) {
	port := reserveTCPPort(t)
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		strconv.Itoa(port),
		WithMetricsEnabled(false),
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}

	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	defer stopServer(t, srv)

	req, err := http.NewRequest(http.MethodGet, "http://127.0.0.1:"+strconv.Itoa(port)+"/metrics", nil)
	if err != nil {
		t.Fatalf("failed to create /metrics request: %v", err)
	}
	resp, err := doRequestWithRetry(req)
	if err != nil {
		t.Fatalf("failed to call /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/metrics status = %d, want %d when disabled", resp.StatusCode, http.StatusNotFound)
	}
}

func reserveTCPPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve port: %v", err)
	}
	defer func() { _ = listener.Close() }()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("expected TCP address, got %T", listener.Addr())
	}
	return addr.Port
}

func stopServer(t *testing.T, srv *Server) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("failed to stop server: %v", err)
	}
}

func doRequestWithRetry(req *http.Request) (*http.Response, error) {
	client := &http.Client{Timeout: 2 * time.Second}

	var lastErr error
	for i := 0; i < 20; i++ {
		resp, err := client.Do(req.Clone(req.Context()))
		if err == nil {
			return resp, nil
		}
		lastErr = err
		time.Sleep(25 * time.Millisecond)
	}

	return nil, lastErr
}

func metricLabels(metric *dto.Metric) map[string]string {
	labels := make(map[string]string)
	for _, label := range metric.GetLabel() {
		labels[label.GetName()] = label.GetValue()
	}
	return labels
}
