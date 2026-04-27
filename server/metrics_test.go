package server

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestMetricsEndpointExposesPrometheusTextFormat(t *testing.T) {
	ts := newMetricsTestServer(t, WithBuildVersion("1.2.3"))

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()

	contentType, body, families := scrapeMetrics(t, ts.URL+"/metrics")
	if !strings.Contains(contentType, "text/plain") {
		t.Fatalf("/metrics Content-Type = %q, want Prometheus text", contentType)
	}

	for _, name := range []string{"go_goroutines", "process_cpu_seconds_total", "vekil_build_info", "vekil_http_requests_total"} {
		if families[name] == nil {
			t.Fatalf("missing metric family %q in /metrics body:\n%s", name, body)
		}
	}
	if !metricFamilyHasLabelValue(families["vekil_build_info"], "version", "1.2.3") {
		t.Fatalf("vekil_build_info missing version label in /metrics body:\n%s", body)
	}
	if !metricFamilyHasLabels(families["vekil_http_requests_total"], map[string]string{
		"route":  "/healthz",
		"code":   "200",
		"method": "get",
	}) {
		t.Fatalf("vekil_http_requests_total missing expected /healthz sample in /metrics body:\n%s", body)
	}
}

func TestMetricsEndpointUsesOnlyBoundedSafeRequestLabels(t *testing.T) {
	ts := newMetricsTestServer(t)

	secretUser := "alice@example.com"
	secretPrompt := "top-secret-prompt"
	secretAuth := "Bearer sk-live-super-secret"
	req, err := http.NewRequest(http.MethodGet, ts.URL+"/healthz?user="+secretUser+"&prompt="+secretPrompt, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", secretAuth)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	resp.Body.Close()

	_, body, families := scrapeMetrics(t, ts.URL+"/metrics")
	for _, forbidden := range []string{secretUser, secretPrompt, secretAuth} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("/metrics body unexpectedly contained sensitive value %q:\n%s", forbidden, body)
		}
	}

	family := families["vekil_http_requests_total"]
	if family == nil {
		t.Fatal("missing vekil_http_requests_total")
	}
	allowed := map[string]bool{"route": true, "code": true, "method": true}
	for _, metric := range family.Metric {
		for _, label := range metric.Label {
			if !allowed[label.GetName()] {
				t.Fatalf("unexpected dynamic label %q", label.GetName())
			}
			for _, forbidden := range []string{secretUser, secretPrompt, secretAuth} {
				if strings.Contains(label.GetValue(), forbidden) {
					t.Fatalf("label %q unexpectedly contained sensitive value %q", label.GetName(), forbidden)
				}
			}
		}
	}
}

func TestMetricsEndpointCanBeDisabled(t *testing.T) {
	ts := newMetricsTestServer(t, WithMetricsEnabled(false))

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("GET /metrics status = %d, want 404", resp.StatusCode)
	}
}

func newMetricsTestServer(t *testing.T, opts ...Option) *httptest.Server {
	t.Helper()

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
		opts...,
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ts := httptest.NewServer(srv.httpServer.Handler)
	t.Cleanup(ts.Close)
	return ts
}

func scrapeMetrics(t *testing.T, url string) (string, string, map[string]*dto.MetricFamily) {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", url, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("ReadAll %s: %v", url, err)
	}

	families, err := new(expfmt.TextParser).TextToMetricFamilies(bytes.NewReader(body))
	if err != nil {
		t.Fatalf("parse metrics from %s: %v\n%s", url, err, body)
	}

	return resp.Header.Get("Content-Type"), string(body), families
}

func metricFamilyHasLabelValue(family *dto.MetricFamily, name, value string) bool {
	if family == nil {
		return false
	}
	for _, metric := range family.Metric {
		for _, label := range metric.Label {
			if label.GetName() == name && label.GetValue() == value {
				return true
			}
		}
	}
	return false
}

func metricFamilyHasLabels(family *dto.MetricFamily, want map[string]string) bool {
	if family == nil {
		return false
	}
	for _, metric := range family.Metric {
		ok := true
		for name, value := range want {
			found := false
			for _, label := range metric.Label {
				if label.GetName() == name && label.GetValue() == value {
					found = true
					break
				}
			}
			if !found {
				ok = false
				break
			}
		}
		if ok {
			return true
		}
	}
	return false
}
