package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil/promlint"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func startMetricsTestServer(t *testing.T, opts ...Option) string {
	t.Helper()

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
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
			t.Errorf("failed to stop server: %v", err)
		}
	})

	return "http://" + srv.Addr()
}

func TestStart_ExposesPrometheusMetrics(t *testing.T) {
	baseURL := startMetricsTestServer(t, WithBuildVersion("test-build-version"))
	client := &http.Client{Timeout: 5 * time.Second}

	secretUser := "issue-77-user@example.com"
	secretKey := "sk-issue-77-secret-key"
	secretPrompt := "issue-77-prompt-should-not-appear"

	healthReq, err := http.NewRequest(http.MethodGet, baseURL+"/healthz", strings.NewReader(secretPrompt))
	if err != nil {
		t.Fatalf("failed to create health request: %v", err)
	}
	healthReq.Header.Set("Authorization", "Bearer "+secretKey)
	healthReq.Header.Set("X-Test-User", secretUser)

	healthResp, err := client.Do(healthReq)
	if err != nil {
		t.Fatalf("failed to call /healthz: %v", err)
	}
	defer func() { _ = healthResp.Body.Close() }()

	if healthResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(healthResp.Body)
		t.Fatalf("/healthz status = %d, want 200; body=%s", healthResp.StatusCode, body)
	}

	metricsResp, err := client.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("failed to call /metrics: %v", err)
	}
	defer func() { _ = metricsResp.Body.Close() }()

	if contentType := metricsResp.Header.Get("Content-Type"); !strings.HasPrefix(contentType, "text/plain") {
		t.Fatalf("/metrics Content-Type = %q, want text/plain", contentType)
	}

	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("failed to read /metrics body: %v", err)
	}
	text := string(body)

	problems, err := promlint.New(strings.NewReader(text)).Lint()
	if err != nil {
		t.Fatalf("failed to parse Prometheus metrics: %v\n%s", err, text)
	}
	if len(problems) != 0 {
		t.Fatalf("Prometheus metrics lint problems: %v\n%s", problems, text)
	}

	for _, want := range []string{
		"go_goroutines",
		`vekil_build_info{version="test-build-version"} 1`,
		`vekil_http_requests_total`,
		`route="/healthz"`,
		`method="GET"`,
		`code="200"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("/metrics output missing %q\n%s", want, text)
		}
	}

	for _, forbidden := range []string{secretUser, secretKey, secretPrompt} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("/metrics output unexpectedly contained request data %q\n%s", forbidden, text)
		}
	}
}

func TestStart_CanDisablePrometheusMetrics(t *testing.T) {
	baseURL := startMetricsTestServer(t, WithMetricsEnabled(false))
	client := &http.Client{Timeout: 5 * time.Second}

	resp, err := client.Get(baseURL + "/metrics")
	if err != nil {
		t.Fatalf("failed to call /metrics: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusNotFound {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("/metrics status = %d, want 404; body=%s", resp.StatusCode, body)
	}
}
