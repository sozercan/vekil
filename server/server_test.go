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

func TestMetricsEndpointExposesPrometheusTextFormat(t *testing.T) {
	port := freePort(t)
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		port,
		WithMetricsBuildVersion("1.2.3-test"),
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

	const secretQuery = "prompt=top-secret-prompt&user=alice@example.com&key=sk-live-123"
	mustHTTPGet(t, "http://127.0.0.1:"+port+"/healthz?"+secretQuery)

	resp := mustHTTPGet(t, "http://127.0.0.1:"+port+"/metrics")
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read metrics response: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("failed to close metrics response body: %v", err)
	}

	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/plain") {
		t.Fatalf("Content-Type = %q, want Prometheus text exposition", contentType)
	}

	bodyText := string(body)
	if !strings.Contains(bodyText, "\ngo_goroutines ") && !strings.HasPrefix(bodyText, "go_goroutines ") {
		t.Fatalf("metrics output missing go_goroutines")
	}
	if !strings.Contains(bodyText, "\nprocess_cpu_seconds_total ") && !strings.HasPrefix(bodyText, "process_cpu_seconds_total ") {
		t.Fatalf("metrics output missing process_cpu_seconds_total")
	}
	if !strings.Contains(bodyText, "vekil_build_info{version=\"1.2.3-test\"} 1") {
		t.Fatalf("metrics output missing vekil_build_info")
	}
	if !containsMetricsLine(bodyText, "vekil_http_requests_total{", "handler=\"healthz\"", "code=\"200\"") {
		t.Fatalf("vekil_http_requests_total missing bounded healthz sample")
	}

	for _, forbidden := range []string{"top-secret-prompt", "alice@example.com", "sk-live-123"} {
		if strings.Contains(bodyText, forbidden) {
			t.Fatalf("metrics output leaked request data %q", forbidden)
		}
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
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if got, want := rec.Code, http.StatusNotFound; got != want {
		t.Fatalf("GET /metrics status = %d, want %d", got, want)
	}
}

func freePort(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to allocate free port: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("failed to release free port: %v", err)
		}
	}()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("expected TCP address, got %T", listener.Addr())
	}
	return strconv.Itoa(addr.Port)
}

func mustHTTPGet(t *testing.T, rawURL string) *http.Response {
	t.Helper()

	client := &http.Client{Timeout: 5 * time.Second}
	var lastErr error
	for range 50 {
		resp, err := client.Get(rawURL)
		if err == nil {
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				t.Fatalf("GET %s status = %d, want 200; body=%s", rawURL, resp.StatusCode, body)
			}
			return resp
		}
		lastErr = err
		time.Sleep(20 * time.Millisecond)
	}

	t.Fatalf("GET %s failed: %v", rawURL, lastErr)
	return nil
}

func containsMetricsLine(body string, substrings ...string) bool {
	for _, line := range strings.Split(body, "\n") {
		matches := true
		for _, substring := range substrings {
			if !strings.Contains(line, substring) {
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
