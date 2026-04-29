package server

import (
	"bytes"
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

func TestNew_MetricsEndpointEnabledByDefault(t *testing.T) {
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

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", resp.StatusCode)
	}
}

func TestNew_WithMetricsDisabledOmitsMetricsEndpoint(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
		WithMetrics(false, "1.2.3-test"),
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /metrics status = %d, want 404", resp.StatusCode)
	}
}

func TestMetricsEndpointExposesPrometheusTextFormat(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
		WithMetrics(true, "1.2.3-test"),
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	resp.Body.Close()

	metricsResp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer metricsResp.Body.Close()

	if metricsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", metricsResp.StatusCode)
	}
	if got := metricsResp.Header.Get("Content-Type"); !strings.HasPrefix(got, "text/plain") {
		t.Fatalf("Content-Type = %q, want Prometheus text format", got)
	}

	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}

	families, err := parseMetricsText(body)
	if err != nil {
		t.Fatalf("parse /metrics output: %v\n%s", err, body)
	}

	if _, ok := families["go_goroutines"]; !ok {
		t.Fatal("missing go_goroutines metric")
	}

	buildInfo := families["vekil_build_info"]
	if len(buildInfo) == 0 {
		t.Fatal("missing vekil_build_info metric")
	}
	if got := buildInfo[0].labels["version"]; got != "1.2.3-test" {
		t.Fatalf("version label = %q, want %q", got, "1.2.3-test")
	}

	requests := families["vekil_http_requests_total"]
	if len(requests) == 0 {
		t.Fatal("missing vekil_http_requests_total metric")
	}
	if !hasCounterSample(requests, map[string]string{
		"handler": "/healthz",
		"method":  http.MethodGet,
		"code":    strconv.Itoa(http.StatusOK),
	}) {
		t.Fatal("missing /healthz request counter sample")
	}
}

func TestMetricsEndpointDoesNotExposeUserDerivedLabels(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
		WithMetrics(true, "1.2.3-test"),
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}

	ts := httptest.NewServer(srv.httpServer.Handler)
	defer ts.Close()

	secret := "user-secret@example.com"
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/healthz?user="+secret, strings.NewReader("prompt-secret"))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer sk-secret")
	req.Header.Set("X-User", secret)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz failed: %v", err)
	}
	resp.Body.Close()

	metricsResp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics failed: %v", err)
	}
	defer metricsResp.Body.Close()

	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("read /metrics body: %v", err)
	}
	if bytes.Contains(body, []byte(secret)) || bytes.Contains(body, []byte("sk-secret")) || bytes.Contains(body, []byte("prompt-secret")) {
		t.Fatal("metrics output leaked request-specific secret content")
	}

	families, err := parseMetricsText(body)
	if err != nil {
		t.Fatalf("parse /metrics output: %v", err)
	}

	requests := families["vekil_http_requests_total"]
	if len(requests) == 0 {
		t.Fatal("missing vekil_http_requests_total metric")
	}
	for _, metric := range requests {
		for name := range metric.labels {
			if name != "handler" && name != "code" && name != "method" {
				t.Fatalf("unexpected label %q", name)
			}
		}
	}
}

type metricSample struct {
	labels map[string]string
	value  string
}

func hasCounterSample(metrics []metricSample, want map[string]string) bool {
	for _, metric := range metrics {
		matched := true
		for key, wantValue := range want {
			if got := metric.labels[key]; got != wantValue {
				matched = false
				break
			}
		}
		if matched {
			return true
		}
	}
	return false
}

func parseMetricsText(body []byte) (map[string][]metricSample, error) {
	families := make(map[string][]metricSample)
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		nameAndLabels, value, ok := strings.Cut(line, " ")
		if !ok {
			return nil, strconv.ErrSyntax
		}

		name := nameAndLabels
		labels := map[string]string{}
		if open := strings.IndexByte(nameAndLabels, '{'); open >= 0 {
			close := strings.LastIndexByte(nameAndLabels, '}')
			if close < open {
				return nil, strconv.ErrSyntax
			}
			name = nameAndLabels[:open]
			parsed, err := parseMetricLabels(nameAndLabels[open+1 : close])
			if err != nil {
				return nil, err
			}
			labels = parsed
		}

		families[name] = append(families[name], metricSample{
			labels: labels,
			value:  value,
		})
	}
	return families, nil
}

func parseMetricLabels(input string) (map[string]string, error) {
	labels := make(map[string]string)
	for len(strings.TrimSpace(input)) > 0 {
		input = strings.TrimSpace(input)

		eq := strings.IndexByte(input, '=')
		if eq <= 0 || eq+1 >= len(input) || input[eq+1] != '"' {
			return nil, strconv.ErrSyntax
		}
		key := strings.TrimSpace(input[:eq])

		value, rest, err := parseQuotedMetricValue(input[eq+1:])
		if err != nil {
			return nil, err
		}
		labels[key] = value

		rest = strings.TrimSpace(rest)
		if rest == "" {
			break
		}
		if rest[0] != ',' {
			return nil, strconv.ErrSyntax
		}
		input = rest[1:]
	}
	return labels, nil
}

func parseQuotedMetricValue(input string) (string, string, error) {
	if input == "" || input[0] != '"' {
		return "", "", strconv.ErrSyntax
	}

	var b strings.Builder
	escaped := false
	for i := 1; i < len(input); i++ {
		ch := input[i]
		if escaped {
			switch ch {
			case '\\', '"':
				b.WriteByte(ch)
			case 'n':
				b.WriteByte('\n')
			default:
				return "", "", strconv.ErrSyntax
			}
			escaped = false
			continue
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			return b.String(), input[i+1:], nil
		}
		b.WriteByte(ch)
	}

	return "", "", strconv.ErrSyntax
}
