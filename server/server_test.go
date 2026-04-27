package server

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestMetricsEndpointExposesPrometheusMetricsWithoutSensitiveLabels(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
		WithBuildVersion("1.2.3"),
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	secretUser := "alice@example.com"
	secretKey := "sk-test-secret"
	secretPrompt := "super secret prompt"

	query := url.Values{
		"user":   []string{secretUser},
		"prompt": []string{secretPrompt},
	}
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/healthz?"+query.Encode(), nil)
	if err != nil {
		t.Fatalf("failed to build healthz request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+secretKey)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	_ = resp.Body.Close()

	resp, err = ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("metrics request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	if contentType := resp.Header.Get("Content-Type"); !strings.Contains(contentType, "text/plain") {
		t.Fatalf("/metrics Content-Type = %q, want Prometheus text exposition", contentType)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read metrics response: %v", err)
	}

	families, err := new(expfmt.TextParser).TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("/metrics response was not valid Prometheus exposition: %v\n%s", err, body)
	}
	if _, ok := families["go_goroutines"]; !ok {
		t.Fatal("expected go runtime metrics in /metrics output")
	}
	if _, ok := families["process_cpu_seconds_total"]; !ok {
		t.Fatal("expected process metrics in /metrics output")
	}

	buildInfo := families["vekil_build_info"]
	if buildInfo == nil || len(buildInfo.Metric) != 1 {
		t.Fatalf("expected one vekil_build_info metric, got %#v", buildInfo)
	}
	labels := map[string]string{}
	for _, label := range buildInfo.Metric[0].GetLabel() {
		labels[label.GetName()] = label.GetValue()
	}
	if got := labels["version"]; got != "1.2.3" {
		t.Fatalf("vekil_build_info version label = %q, want %q", got, "1.2.3")
	}

	requests := families["vekil_http_requests_total"]
	if requests == nil {
		t.Fatal("expected vekil_http_requests_total metric")
	}
	foundHealthz := false
	for _, metric := range requests.Metric {
		metricLabels := map[string]string{}
		for _, label := range metric.GetLabel() {
			metricLabels[label.GetName()] = label.GetValue()
		}
		for name := range metricLabels {
			switch name {
			case "handler", "method", "code":
			default:
				t.Fatalf("unexpected vekil_http_requests_total label %q", name)
			}
		}
		if metricLabels["handler"] == "healthz" && metricLabels["method"] == http.MethodGet && metricLabels["code"] == "200" {
			foundHealthz = true
		}
	}
	if !foundHealthz {
		t.Fatal("expected bounded healthz request counter in /metrics output")
	}

	for _, secret := range []string{secretUser, secretKey, secretPrompt} {
		if strings.Contains(string(body), secret) {
			t.Fatalf("/metrics unexpectedly exposed sensitive request content %q", secret)
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
		t.Fatalf("metrics request failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("/metrics status = %d, want %d when disabled", resp.StatusCode, http.StatusNotFound)
	}
}
