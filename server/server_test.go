package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/common/expfmt"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
)

func freeTCPPort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve port: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("failed to close reserved port: %v", err)
		}
	}()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("expected TCP address, got %T", listener.Addr())
	}
	return addr.Port
}

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

func TestNew_ExposesMetricsByDefault(t *testing.T) {
	port := freeTCPPort(t)
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		strconv.Itoa(port),
		WithBuildVersion("v1.2.3"),
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}
	t.Cleanup(func() {
		if err := srv.Stop(context.Background()); err != nil {
			t.Fatalf("failed to stop server: %v", err)
		}
	})

	baseURL := "http://127.0.0.1:" + strconv.Itoa(port)
	healthResp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	_ = healthResp.Body.Close()

	metricsResp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer func() { _ = metricsResp.Body.Close() }()
	if got := metricsResp.StatusCode; got != http.StatusOK {
		t.Fatalf("/metrics status = %d, want 200", got)
	}
	contentType := metricsResp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/plain") {
		t.Fatalf("/metrics Content-Type = %q, want Prometheus text exposition", contentType)
	}
	metricsBody, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("failed to read /metrics response: %v", err)
	}

	families, err := new(expfmt.TextParser).TextToMetricFamilies(strings.NewReader(string(metricsBody)))
	if err != nil {
		t.Fatalf("failed to parse metrics exposition: %v", err)
	}
	for _, want := range []string{"go_goroutines", "process_cpu_seconds_total", "vekil_build_info", "vekil_http_requests_total"} {
		if _, ok := families[want]; !ok {
			t.Fatalf("expected metric family %q in exposition", want)
		}
	}

	buildInfo := families["vekil_build_info"]
	if len(buildInfo.Metric) != 1 {
		t.Fatalf("vekil_build_info metrics = %d, want 1", len(buildInfo.Metric))
	}
	var versionLabel string
	for _, label := range buildInfo.Metric[0].GetLabel() {
		if label.GetName() == "version" {
			versionLabel = label.GetValue()
			break
		}
	}
	if versionLabel != "v1.2.3" {
		t.Fatalf("vekil_build_info version label = %q, want %q", versionLabel, "v1.2.3")
	}
}

func TestNew_DisablesMetricsEndpoint(t *testing.T) {
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
	resp := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(resp, req)

	if got := resp.Result().StatusCode; got != http.StatusNotFound {
		t.Fatalf("/metrics status = %d, want 404 when disabled", got)
	}
}

func TestMetrics_DoNotUseSensitiveRequestDataAsLabels(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}

	secrets := []string{
		"alice@example.com",
		"sk-live-123456",
		"prompt-secret-please-ignore",
	}

	req := httptest.NewRequest(
		http.MethodGet,
		fmt.Sprintf("/healthz?user=%s&key=%s&prompt=%s", secrets[0], secrets[1], secrets[2]),
		nil,
	)
	req.Header.Set("Authorization", "Bearer "+secrets[1])
	resp := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(resp, req)

	metricsReq := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsResp := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(metricsResp, metricsReq)

	body := metricsResp.Body.String()
	for _, secret := range secrets {
		if strings.Contains(body, secret) {
			t.Fatalf("metrics exposition leaked sensitive request data %q", secret)
		}
	}

	families, err := new(expfmt.TextParser).TextToMetricFamilies(strings.NewReader(body))
	if err != nil {
		t.Fatalf("failed to parse metrics exposition: %v", err)
	}
	httpRequests := families["vekil_http_requests_total"]
	if httpRequests == nil {
		t.Fatal("expected vekil_http_requests_total metric family")
	}

	for _, metric := range httpRequests.Metric {
		for _, label := range metric.GetLabel() {
			switch label.GetName() {
			case "handler":
				if label.GetValue() != "GET /healthz" && label.GetValue() != "GET /metrics" {
					t.Fatalf("unexpected handler label value %q", label.GetValue())
				}
			case "status_class":
				if label.GetValue() != "2xx" {
					t.Fatalf("unexpected status_class label value %q", label.GetValue())
				}
			default:
				t.Fatalf("unexpected label name %q", label.GetName())
			}
			for _, secret := range secrets {
				if label.GetValue() == secret {
					t.Fatalf("metrics label leaked sensitive request data %q", secret)
				}
			}
		}
	}
}
