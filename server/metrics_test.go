package server

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/prometheus/common/expfmt"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestMetricsEndpointExposesPrometheusTextFormat(t *testing.T) {
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

	sensitiveUser := "user@example.com"
	sensitiveKey := "sk-test-secret"
	sensitivePrompt := "keep-this-prompt-private"

	req, err := http.NewRequest(
		http.MethodGet,
		ts.URL+"/healthz?user="+url.QueryEscape(sensitiveUser)+"&api_key="+url.QueryEscape(sensitiveKey)+"&prompt="+url.QueryEscape(sensitivePrompt),
		nil,
	)
	if err != nil {
		t.Fatalf("failed to create request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+sensitiveKey)

	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatalf("healthz request failed: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("healthz status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	_ = resp.Body.Close()

	metricsResp, err := ts.Client().Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("metrics request failed: %v", err)
	}
	defer metricsResp.Body.Close()

	if got := metricsResp.StatusCode; got != http.StatusOK {
		t.Fatalf("/metrics status = %d, want %d", got, http.StatusOK)
	}
	if contentType := metricsResp.Header.Get("Content-Type"); !strings.Contains(contentType, "text/plain") {
		t.Fatalf("/metrics Content-Type = %q, want Prometheus text exposition", contentType)
	}

	bodyBytes, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("failed to read /metrics response: %v", err)
	}
	body := string(bodyBytes)

	parser := expfmt.TextParser{}
	families, err := parser.TextToMetricFamilies(strings.NewReader(body))
	if err != nil {
		t.Fatalf("/metrics returned invalid Prometheus exposition: %v\n%s", err, body)
	}

	for _, name := range []string{"go_goroutines", "vekil_build_info", "vekil_http_requests_total"} {
		if _, ok := families[name]; !ok {
			t.Fatalf("/metrics missing %q\n%s", name, body)
		}
	}

	httpRequests := families["vekil_http_requests_total"]
	foundHealthz := false
	for _, metric := range httpRequests.GetMetric() {
		labels := map[string]string{}
		for _, label := range metric.GetLabel() {
			labels[label.GetName()] = label.GetValue()
		}
		if labels["handler"] == "healthz" && labels["code"] == "200" {
			foundHealthz = true
			break
		}
	}
	if !foundHealthz {
		t.Fatalf("vekil_http_requests_total missing healthz request sample\n%s", body)
	}

	secrets := []string{
		sensitiveUser,
		sensitiveKey,
		sensitivePrompt,
		"Bearer " + sensitiveKey,
	}
	allowedRequestLabels := map[string]struct{}{
		"handler": {},
		"code":    {},
		"method":  {},
	}

	for _, metric := range httpRequests.GetMetric() {
		for _, label := range metric.GetLabel() {
			if _, ok := allowedRequestLabels[label.GetName()]; !ok {
				t.Fatalf("unexpected request metric label %q", label.GetName())
			}
			for _, secret := range secrets {
				if strings.Contains(label.GetValue(), secret) {
					t.Fatalf("request metric label %q leaked sensitive value %q", label.GetName(), secret)
				}
			}
		}
	}

	for _, secret := range secrets {
		if strings.Contains(body, secret) {
			t.Fatalf("/metrics body leaked sensitive value %q", secret)
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

	if got := resp.StatusCode; got != http.StatusNotFound {
		t.Fatalf("/metrics status = %d, want %d when disabled", got, http.StatusNotFound)
	}
}
