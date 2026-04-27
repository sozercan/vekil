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

func TestMetricsEndpointReturnsPrometheusExposition(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
		WithBuildVersion("v1.2.3-test"),
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}

	testServer := httptest.NewServer(srv.httpServer.Handler)
	defer testServer.Close()

	const secret = "sk-test-super-secret"
	const prompt = "tell me a secret joke"

	req, err := http.NewRequest(http.MethodGet, testServer.URL+"/healthz?user=alice&prompt="+url.QueryEscape(prompt), nil)
	if err != nil {
		t.Fatalf("failed to create healthz request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+secret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	_ = resp.Body.Close()

	metricsResp, err := http.Get(testServer.URL + "/metrics")
	if err != nil {
		t.Fatalf("metrics request failed: %v", err)
	}
	defer metricsResp.Body.Close()

	if got := metricsResp.StatusCode; got != http.StatusOK {
		t.Fatalf("/metrics status = %d, want %d", got, http.StatusOK)
	}
	if got := metricsResp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain; version=0.0.4") {
		t.Fatalf("/metrics Content-Type = %q, want Prometheus text exposition", got)
	}

	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("failed to read /metrics response: %v", err)
	}
	text := string(body)
	for _, forbidden := range []string{secret, prompt, "alice"} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("/metrics unexpectedly contained %q", forbidden)
		}
	}

	parser := expfmt.TextParser{}
	families, err := parser.TextToMetricFamilies(strings.NewReader(text))
	if err != nil {
		t.Fatalf("failed to parse Prometheus exposition: %v", err)
	}

	if _, ok := families["go_goroutines"]; !ok {
		t.Fatalf("expected go runtime metrics in /metrics response")
	}

	buildInfo, ok := families[metricNameBuildInfo]
	if !ok {
		t.Fatalf("expected %s metric", metricNameBuildInfo)
	}
	if !familyUsesOnlyLabels(buildInfo, "version", "goversion") {
		t.Fatalf("%s exposed unexpected labels", metricNameBuildInfo)
	}
	if !hasMetricWithLabels(buildInfo, map[string]string{"version": "v1.2.3-test"}) {
		t.Fatalf("expected %s to include version label", metricNameBuildInfo)
	}

	requests, ok := families[metricNameHTTPRequests]
	if !ok {
		t.Fatalf("expected %s metric", metricNameHTTPRequests)
	}
	if !hasMetricWithLabels(requests, map[string]string{
		"route":        "/healthz",
		"method":       http.MethodGet,
		"status_class": "2xx",
	}) {
		t.Fatalf("expected %s to count GET /healthz", metricNameHTTPRequests)
	}
	if !familyUsesOnlyLabels(requests, "route", "method", "status_class") {
		t.Fatalf("%s exposed unexpected labels", metricNameHTTPRequests)
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

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, req)

	if got := rec.Code; got != http.StatusNotFound {
		t.Fatalf("/metrics status = %d, want %d when disabled", got, http.StatusNotFound)
	}
}

func hasMetricWithLabels(family *dto.MetricFamily, want map[string]string) bool {
	for _, metric := range family.Metric {
		labels := map[string]string{}
		for _, pair := range metric.Label {
			labels[pair.GetName()] = pair.GetValue()
		}
		if len(labels) < len(want) {
			continue
		}
		match := true
		for name, value := range want {
			if labels[name] != value {
				match = false
				break
			}
		}
		if match {
			return true
		}
	}
	return false
}

func familyUsesOnlyLabels(family *dto.MetricFamily, allowed ...string) bool {
	allowedSet := map[string]struct{}{}
	for _, name := range allowed {
		allowedSet[name] = struct{}{}
	}
	for _, metric := range family.Metric {
		for _, pair := range metric.Label {
			if _, ok := allowedSet[pair.GetName()]; !ok {
				return false
			}
		}
	}
	return true
}
