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

func TestMetricsEndpointExposesPrometheusMetrics(t *testing.T) {
	baseURL := startTestHTTPServer(t, WithBuildVersion("1.2.3"))

	resp, err := http.Get(baseURL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if got, want := resp.StatusCode, http.StatusOK; got != want {
		t.Fatalf("GET /metrics status = %d, want %d", got, want)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "text/plain") {
		t.Fatalf("GET /metrics content-type = %q, want Prometheus text exposition", contentType)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading /metrics body: %v", err)
	}

	parser := expfmt.TextParser{}
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("parsing Prometheus metrics: %v", err)
	}

	for _, name := range []string{"go_goroutines", "vekil_build_info", "vekil_http_requests_total", "vekil_http_request_duration_seconds"} {
		if _, ok := families[name]; !ok {
			t.Fatalf("missing metric family %q", name)
		}
	}

	buildInfo := families["vekil_build_info"].Metric
	if len(buildInfo) == 0 {
		t.Fatal("vekil_build_info has no samples")
	}
	foundVersion := false
	for _, metric := range buildInfo {
		for _, label := range metric.GetLabel() {
			if label.GetName() == "version" && label.GetValue() == "1.2.3" && metric.GetGauge().GetValue() == 1 {
				foundVersion = true
			}
		}
	}
	if !foundVersion {
		t.Fatalf("vekil_build_info missing version label for %q", "1.2.3")
	}

	foundHealthz := false
	for _, metric := range families["vekil_http_requests_total"].Metric {
		var handler, method, codeClass string
		for _, label := range metric.GetLabel() {
			switch label.GetName() {
			case "handler":
				handler = label.GetValue()
			case "method":
				method = label.GetValue()
			case "code_class":
				codeClass = label.GetValue()
			}
		}
		if handler == "/healthz" && method == http.MethodGet && codeClass == "2xx" {
			foundHealthz = true
			break
		}
	}
	if !foundHealthz {
		t.Fatal("missing GET /healthz request metric sample")
	}
}

func TestMetricsEndpointOmitsSensitiveContentFromLabels(t *testing.T) {
	baseURL := startTestHTTPServer(t)

	req, err := http.NewRequest(http.MethodGet, baseURL+"/healthz?user=alice@example.com&prompt=Write+a+poem&api_key=sk-test-query", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer sk-live-header")
	req.Header.Set("X-User", "alice@example.com")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("reading /metrics body: %v", err)
	}
	bodyText := string(body)

	for _, secret := range []string{"alice@example.com", "Write a poem", "Write+a+poem", "sk-test-query", "sk-live-header"} {
		if strings.Contains(bodyText, secret) {
			t.Fatalf("metrics output leaked request content %q", secret)
		}
	}

	parser := expfmt.TextParser{}
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("parsing Prometheus metrics: %v", err)
	}

	requestMetrics, ok := families["vekil_http_requests_total"]
	if !ok {
		t.Fatal("missing metric family vekil_http_requests_total")
	}
	for _, metric := range requestMetrics.Metric {
		for _, label := range metric.GetLabel() {
			switch label.GetName() {
			case "handler":
				if strings.Contains(label.GetValue(), "?") {
					t.Fatalf("handler label leaked query string: %q", label.GetValue())
				}
			case "method", "code_class":
			default:
				t.Fatalf("unexpected request metric label %q", label.GetName())
			}
		}
	}
}

func TestMetricsEndpointCanBeDisabled(t *testing.T) {
	baseURL := startTestHTTPServer(t, WithMetricsEnabled(false))

	resp, err := http.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if got, want := resp.StatusCode, http.StatusNotFound; got != want {
		t.Fatalf("GET /metrics status = %d, want %d", got, want)
	}
}

func startTestHTTPServer(t *testing.T, opts ...Option) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

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

	go func() {
		if err := srv.httpServer.Serve(listener); err != nil && err != http.ErrServerClosed {
			t.Errorf("Serve() error = %v", err)
		}
	}()

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.httpServer.Shutdown(ctx); err != nil {
			t.Errorf("Shutdown() error = %v", err)
		}
	})

	return "http://" + listener.Addr().String()
}
