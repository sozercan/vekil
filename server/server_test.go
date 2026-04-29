package server

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
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
	families := parseMetricFamilies(t, body)

	buildInfo := findMetric(t, families["vekil_build_info"], map[string]string{"version": "1.2.3-test"})
	if got := buildInfo.GetGauge().GetValue(); got != 1 {
		t.Fatalf("vekil_build_info value = %v, want 1", got)
	}

	if _, ok := families["go_goroutines"]; !ok {
		t.Fatal("expected Go runtime metrics to include go_goroutines")
	}

	requestMetric := findMetric(t, families["vekil_http_requests_total"], map[string]string{
		"method": "get",
		"route":  "/healthz",
		"code":   "200",
	})
	if got := requestMetric.GetCounter().GetValue(); got != 1 {
		t.Fatalf("vekil_http_requests_total{/healthz} = %v, want 1", got)
	}
}

func TestMetricsEndpoint_DoesNotExposeSensitiveRequestValuesAsLabels(t *testing.T) {
	srv := startTestServer(t)

	const (
		sensitiveToken  = "sk-test-secret"
		sensitivePrompt = "top-secret-prompt"
		sensitiveUser   = "alice@example.com"
	)

	req, err := http.NewRequest(
		http.MethodPost,
		"http://"+srv.httpServer.Addr+"/v1/memories/trace_summarize?user="+sensitiveUser,
		strings.NewReader(`{"prompt":"`+sensitivePrompt+`"}`),
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
	families := parseMetricFamilies(t, body)

	requestMetric := findMetric(t, families["vekil_http_requests_total"], map[string]string{
		"method": "post",
		"route":  "/v1/memories/trace_summarize",
		"code":   "400",
	})
	if got := requestMetric.GetCounter().GetValue(); got != 1 {
		t.Fatalf("vekil_http_requests_total{trace_summarize} = %v, want 1", got)
	}

	for _, metric := range families["vekil_http_requests_total"].GetMetric() {
		labels := labelMap(metric)
		if len(labels) != 3 {
			t.Fatalf("vekil_http_requests_total labels = %v, want exactly method/route/code", labels)
		}
		for _, name := range []string{"method", "route", "code"} {
			if _, ok := labels[name]; !ok {
				t.Fatalf("vekil_http_requests_total missing %q label in %v", name, labels)
			}
		}
	}

	for familyName, family := range families {
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				for _, sensitiveValue := range []string{sensitiveToken, sensitivePrompt, sensitiveUser} {
					if strings.Contains(label.GetValue(), sensitiveValue) {
						t.Fatalf("unexpected sensitive value %q in %s label on %s", sensitiveValue, label.GetName(), familyName)
					}
				}
			}
		}
	}
}

func TestMetricsInstrumentation_PreservesOptionalResponseWriterInterfaces(t *testing.T) {
	metrics := newServerMetrics("1.2.3-test")
	wrapped := metrics.wrap("/v1/responses", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, ok := w.(http.Flusher); !ok {
			t.Fatal("wrapped response writer does not implement http.Flusher")
		}
		if _, ok := w.(http.Hijacker); !ok {
			t.Fatal("wrapped response writer does not implement http.Hijacker")
		}
		w.(http.Flusher).Flush()
		w.WriteHeader(http.StatusSwitchingProtocols)
	}))

	connA, connB := net.Pipe()
	defer func() { _ = connA.Close() }()
	defer func() { _ = connB.Close() }()

	rec := &interfacePreservingResponseWriter{
		header: make(http.Header),
		conn:   connA,
		rw:     bufio.NewReadWriter(bufio.NewReader(connA), bufio.NewWriter(connA)),
	}

	wrapped.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))

	if !rec.flushed {
		t.Fatal("expected wrapped handler to preserve Flush behavior")
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

func parseMetricFamilies(t *testing.T, body []byte) map[string]*dto.MetricFamily {
	t.Helper()

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("failed to parse Prometheus metrics: %v\n%s", err, string(body))
	}
	return families
}

func findMetric(t *testing.T, family *dto.MetricFamily, want map[string]string) *dto.Metric {
	t.Helper()

	if family == nil {
		t.Fatalf("missing metric family for labels %v", want)
	}

	for _, metric := range family.GetMetric() {
		labels := labelMap(metric)
		if len(labels) < len(want) {
			continue
		}

		matches := true
		for name, value := range want {
			if labels[name] != value {
				matches = false
				break
			}
		}
		if matches {
			return metric
		}
	}

	t.Fatalf("failed to find metric %q with labels %v", family.GetName(), want)
	return nil
}

func labelMap(metric *dto.Metric) map[string]string {
	labels := make(map[string]string, len(metric.GetLabel()))
	for _, label := range metric.GetLabel() {
		labels[label.GetName()] = label.GetValue()
	}
	return labels
}

type interfacePreservingResponseWriter struct {
	header  http.Header
	status  int
	flushed bool
	conn    net.Conn
	rw      *bufio.ReadWriter
}

func (w *interfacePreservingResponseWriter) Header() http.Header {
	return w.header
}

func (w *interfacePreservingResponseWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return len(p), nil
}

func (w *interfacePreservingResponseWriter) WriteHeader(status int) {
	w.status = status
}

func (w *interfacePreservingResponseWriter) Flush() {
	w.flushed = true
}

func (w *interfacePreservingResponseWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	return w.conn, w.rw, nil
}
