package server

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"sort"
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

	healthReq, err := http.NewRequest(http.MethodGet, ts.URL+"/healthz?user=user-secret&prompt=prompt-secret", nil)
	if err != nil {
		t.Fatalf("failed to create health request: %v", err)
	}
	healthReq.Header.Set("Authorization", "Bearer key-secret")
	healthResp, err := http.DefaultClient.Do(healthReq)
	if err != nil {
		t.Fatalf("failed to call /healthz: %v", err)
	}
	defer healthResp.Body.Close()
	if healthResp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want %d", healthResp.StatusCode, http.StatusOK)
	}

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("failed to call /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("failed to read /metrics body: %v", err)
	}
	if !strings.Contains(resp.Header.Get("Content-Type"), "text/plain") {
		t.Fatalf("/metrics content-type = %q, want Prometheus text exposition", resp.Header.Get("Content-Type"))
	}
	for _, secret := range []string{"user-secret", "prompt-secret", "key-secret"} {
		if bytes.Contains(body, []byte(secret)) {
			t.Fatalf("/metrics body unexpectedly contained %q", secret)
		}
	}

	parser := expfmt.TextParser{}
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("failed to parse Prometheus exposition: %v", err)
	}
	if _, ok := families["go_goroutines"]; ok {
		t.Fatalf("parsed metrics unexpectedly exposed go_goroutines")
	}
	if _, ok := families["process_cpu_seconds_total"]; ok {
		t.Fatalf("parsed metrics unexpectedly exposed process_cpu_seconds_total")
	}
	if _, ok := families["vekil_build_info"]; ok {
		t.Fatalf("parsed metrics unexpectedly exposed vekil_build_info")
	}

	requests := families["vekil_http_requests_total"]
	if requests == nil {
		t.Fatalf("parsed metrics missing vekil_http_requests_total")
	}
	metric := findMetricByLabel(requests.GetMetric(), "route", "/healthz")
	if metric == nil {
		t.Fatalf("vekil_http_requests_total missing /healthz sample")
	}
	if got := metric.GetCounter().GetValue(); got < 1 {
		t.Fatalf("vekil_http_requests_total /healthz sample = %v, want >= 1", got)
	}
	if got := labelNames(metric); strings.Join(got, ",") != "code,method,route" {
		t.Fatalf("vekil_http_requests_total labels = %v, want [code method route]", got)
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

	req := httptestRequest(http.MethodGet, "/metrics")
	resp := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(resp, req)

	if resp.Code != http.StatusNotFound {
		t.Fatalf("/metrics status = %d, want %d when metrics are disabled", resp.Code, http.StatusNotFound)
	}
}

func findMetricByLabel(metrics []*dto.Metric, labelName, labelValue string) *dto.Metric {
	for _, metric := range metrics {
		for _, label := range metric.GetLabel() {
			if label.GetName() == labelName && label.GetValue() == labelValue {
				return metric
			}
		}
	}
	return nil
}

func labelNames(metric *dto.Metric) []string {
	names := make([]string, 0, len(metric.GetLabel()))
	for _, label := range metric.GetLabel() {
		names = append(names, label.GetName())
	}
	sort.Strings(names)
	return names
}

func httptestRequest(method, target string) *http.Request {
	req, err := http.NewRequest(method, target, nil)
	if err != nil {
		panic(err)
	}
	return req
}
