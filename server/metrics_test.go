package server

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestMetricsEndpoint_ExposesPrometheusText(t *testing.T) {
	baseURL := startTestHTTPServer(t)

	resp, _ := httpGet(t, baseURL+"/healthz")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	resp, body := httpGet(t, baseURL+"/metrics")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want %d; body=%q", resp.StatusCode, http.StatusOK, body)
	}
	contentType := resp.Header.Get("Content-Type")
	if !strings.Contains(contentType, "text/plain") || !strings.Contains(contentType, "version=0.0.4") {
		t.Fatalf("Content-Type = %q, want Prometheus text exposition", contentType)
	}
	if !strings.Contains(body, "# HELP go_goroutines ") {
		t.Fatalf("metrics output missing go runtime metric, got %q", body)
	}

	info := currentBuildInfo()
	expected := fmt.Sprintf(`# HELP vekil_build_info A metric with a constant '1' value labeled by Vekil build information.
# TYPE vekil_build_info gauge
vekil_build_info{go_version=%q,revision=%q,version=%q} 1
# HELP vekil_http_requests_total Total number of completed HTTP requests handled by Vekil.
# TYPE vekil_http_requests_total counter
vekil_http_requests_total{code="200",handler="healthz",method="get"} 1
`, info.goVersion, info.revision, info.version)

	if err := testutil.ScrapeAndCompare(baseURL+"/metrics", strings.NewReader(expected), "vekil_build_info", "vekil_http_requests_total"); err != nil {
		t.Fatalf("metrics scrape mismatch: %v", err)
	}
}

func TestMetricsEndpoint_DoesNotUseRequestDataAsLabels(t *testing.T) {
	baseURL := startTestHTTPServer(t)

	user := "user-example-invalid-12345"
	prompt := "prompt-secret-67890"
	key := "sk-test-secret-98765"

	req, err := http.NewRequest(http.MethodGet, baseURL+"/healthz?user="+user+"&prompt="+prompt, nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-API-Key", key)

	resp, _ := doRequest(t, req)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want %d", resp.StatusCode, http.StatusOK)
	}

	expected := `# HELP vekil_http_requests_total Total number of completed HTTP requests handled by Vekil.
# TYPE vekil_http_requests_total counter
vekil_http_requests_total{code="200",handler="healthz",method="get"} 1
`
	if err := testutil.ScrapeAndCompare(baseURL+"/metrics", strings.NewReader(expected), "vekil_http_requests_total"); err != nil {
		t.Fatalf("metrics scrape mismatch: %v", err)
	}

	_, body := httpGet(t, baseURL+"/metrics")
	for _, secret := range []string{user, prompt, key} {
		if strings.Contains(body, secret) {
			t.Fatalf("metrics output leaked request data %q: %q", secret, body)
		}
	}
}

func TestMetricsEndpoint_CanBeDisabled(t *testing.T) {
	baseURL := startTestHTTPServer(t, WithMetricsEnabled(false))

	resp, body := httpGet(t, baseURL+"/metrics")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /metrics status = %d, want %d; body=%q", resp.StatusCode, http.StatusNotFound, body)
	}
}

func startTestHTTPServer(t *testing.T, opts ...Option) string {
	t.Helper()

	for i := 0; i < 10; i++ {
		port := reservePort(t)
		srv, err := New(
			auth.NewTestAuthenticator("test-token"),
			logger.New(logger.ParseLevel("error")),
			"127.0.0.1",
			strconv.Itoa(port),
			opts...,
		)
		if err != nil {
			t.Fatalf("failed to initialize server: %v", err)
		}
		if err := srv.Start(); err != nil {
			if strings.Contains(err.Error(), "address already in use") {
				continue
			}
			t.Fatalf("failed to start server: %v", err)
		}

		t.Cleanup(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := srv.Stop(ctx); err != nil {
				t.Errorf("failed to stop server: %v", err)
			}
		})
		return fmt.Sprintf("http://127.0.0.1:%d", port)
	}

	t.Fatal("failed to start test server after retrying free ports")
	return ""
}

func reservePort(t *testing.T) int {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve port: %v", err)
	}
	defer func() { _ = listener.Close() }()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("expected TCP address, got %T", listener.Addr())
	}
	return addr.Port
}

func httpGet(t *testing.T, url string) (*http.Response, string) {
	t.Helper()

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	return doRequest(t, req)
}

func doRequest(t *testing.T, req *http.Request) (*http.Response, string) {
	t.Helper()

	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("request failed: %v", err)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = resp.Body.Close()
		t.Fatalf("failed to read response body: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("failed to close response body: %v", err)
	}

	return resp, string(body)
}
