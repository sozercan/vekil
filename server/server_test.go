package server

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

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
	srv := startTestServer(t, WithBuildVersion("1.2.3-test"))

	resp, err := http.Get("http://" + srv.httpServer.Addr + "/healthz")
	if err != nil {
		t.Fatalf("failed to call /healthz: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("failed to close /healthz response body: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	resp, err = http.Get("http://" + srv.httpServer.Addr + "/metrics")
	if err != nil {
		t.Fatalf("failed to call /metrics: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("failed to close /metrics response body: %v", err)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/plain; version=0.0.4") {
		t.Fatalf("/metrics Content-Type = %q, want Prometheus text exposition", got)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read /metrics response body: %v", err)
	}
	metrics := string(body)

	if !strings.Contains(metrics, "vekil_build_info") {
		t.Fatal("expected /metrics output to include vekil_build_info")
	}
	if !strings.Contains(metrics, `version="1.2.3-test"`) {
		t.Fatal("expected /metrics output to include the configured build version")
	}
	if !strings.Contains(metrics, "\ngo_goroutines ") {
		t.Fatal("expected /metrics output to include Go runtime metrics")
	}
	if !hasMetricLine(metrics, "vekil_http_requests_total", `method="GET"`, `route="/healthz"`, `code="200"`) {
		t.Fatal("expected /metrics output to include a bounded HTTP request counter for /healthz")
	}
}

func TestMetricsEndpoint_DoesNotExposeSensitiveRequestValues(t *testing.T) {
	srv := startTestServer(t)

	const (
		sensitiveToken  = "sk-test-secret"
		sensitivePrompt = "top-secret-prompt"
		sensitiveUser   = "alice@example.com"
	)

	req, err := http.NewRequest(
		http.MethodPost,
		"http://"+srv.httpServer.Addr+"/v1/memories/trace_summarize?user="+sensitiveUser,
		strings.NewReader(`{"prompt":"`+sensitivePrompt+`"`),
	)
	if err != nil {
		t.Fatalf("failed to build request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+sensitiveToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("failed to call trace summarize endpoint: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("failed to close trace summarize response body: %v", err)
	}
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("trace summarize status = %d, want %d", resp.StatusCode, http.StatusBadRequest)
	}

	resp, err = http.Get("http://" + srv.httpServer.Addr + "/metrics")
	if err != nil {
		t.Fatalf("failed to call /metrics: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Fatalf("failed to close /metrics response body: %v", err)
		}
	}()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read /metrics response body: %v", err)
	}
	metrics := string(body)

	for _, sensitiveValue := range []string{sensitiveToken, sensitivePrompt, sensitiveUser} {
		if strings.Contains(metrics, sensitiveValue) {
			t.Fatalf("expected /metrics output to omit sensitive value %q", sensitiveValue)
		}
	}
	if !hasMetricLine(metrics, "vekil_http_requests_total", `method="POST"`, `route="/v1/memories/trace_summarize"`, `code="400"`) {
		t.Fatal("expected /metrics output to include a bounded HTTP request counter for trace summarize")
	}
}

func TestNew_DisablesMetricsEndpointWhenConfigured(t *testing.T) {
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
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("/metrics status = %d, want %d when metrics are disabled", rec.Code, http.StatusNotFound)
	}
}

func startTestServer(t *testing.T, opts ...Option) *Server {
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

	if err := srv.Start(); err != nil {
		t.Fatalf("failed to start server: %v", err)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		if err := srv.Stop(ctx); err != nil {
			t.Fatalf("failed to stop server: %v", err)
		}
	})

	return srv
}

func hasMetricLine(metrics, name string, wantLabels ...string) bool {
	for _, line := range strings.Split(metrics, "\n") {
		if !strings.HasPrefix(line, name+"{") {
			continue
		}

		matches := true
		for _, wantLabel := range wantLabels {
			if !strings.Contains(line, wantLabel) {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}

	return false
}
