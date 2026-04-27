package server

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestMetricsEndpointExposesPrometheusFormat(t *testing.T) {
	srv := startTestServer(t, WithBuildVersion("v1.2.3"))

	mustGET(t, "http://"+srv.Addr()+"/healthz")

	resp := mustGET(t, "http://"+srv.Addr()+"/metrics")
	if got := resp.Header.Get("Content-Type"); !strings.Contains(got, "text/plain") {
		t.Fatalf("Content-Type = %q, want Prometheus text format", got)
	}

	families := parseMetricFamilies(t, resp)

	if families["go_goroutines"] == nil {
		t.Fatal("expected go_goroutines metric family")
	}
	if families["vekil_build_info"] == nil {
		t.Fatal("expected vekil_build_info metric family")
	}
	if families["vekil_http_requests_total"] == nil {
		t.Fatal("expected vekil_http_requests_total metric family")
	}
	if families["vekil_http_request_duration_seconds"] == nil {
		t.Fatal("expected vekil_http_request_duration_seconds metric family")
	}

	assertMetricWithLabels(t, families["vekil_build_info"], map[string]string{"version": "v1.2.3"})
	assertMetricWithLabels(t, families["vekil_http_requests_total"], map[string]string{
		"route":  "/healthz",
		"method": http.MethodGet,
		"code":   "200",
	})
}

func TestMetricsEndpointDoesNotUseRequestDataAsLabels(t *testing.T) {
	srv := startTestServer(t)

	const (
		userSentinel   = "user-sentinel-123"
		keySentinel    = "sk-sentinel-456"
		promptSentinel = "prompt-sentinel-789"
	)

	req, err := http.NewRequest(http.MethodGet, "http://"+srv.Addr()+"/healthz?user="+userSentinel+"&prompt="+promptSentinel, nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+keySentinel)
	req.Header.Set("X-API-Key", keySentinel)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("Do() error = %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusOK)
	}
	_ = resp.Body.Close()

	metricsResp := mustGET(t, "http://"+srv.Addr()+"/metrics")
	body, err := io.ReadAll(metricsResp.Body)
	if err != nil {
		t.Fatalf("ReadAll() error = %v", err)
	}
	_ = metricsResp.Body.Close()

	for _, forbidden := range []string{userSentinel, keySentinel, promptSentinel} {
		if strings.Contains(string(body), forbidden) {
			t.Fatalf("metrics output unexpectedly contained %q", forbidden)
		}
	}

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("TextToMetricFamilies() error = %v", err)
	}
	for name, family := range families {
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				value := label.GetValue()
				for _, forbidden := range []string{userSentinel, keySentinel, promptSentinel} {
					if strings.Contains(value, forbidden) {
						t.Fatalf("metric %s label %s unexpectedly contained %q", name, label.GetName(), forbidden)
					}
				}
			}
		}
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
		t.Fatalf("New() error = %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Stop(ctx); err != nil {
			t.Fatalf("Stop() error = %v", err)
		}
	})
	return srv
}

func mustGET(t *testing.T, url string) *http.Response {
	t.Helper()

	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s error = %v", url, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d, want %d", url, resp.StatusCode, http.StatusOK)
	}
	return resp
}

func parseMetricFamilies(t *testing.T, resp *http.Response) map[string]*dto.MetricFamily {
	t.Helper()
	defer resp.Body.Close()

	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(resp.Body)
	if err != nil {
		t.Fatalf("TextToMetricFamilies() error = %v", err)
	}
	return families
}

func assertMetricWithLabels(t *testing.T, family *dto.MetricFamily, want map[string]string) {
	t.Helper()

	for _, metric := range family.GetMetric() {
		matches := true
		for key, value := range want {
			if metricLabelValue(metric, key) != value {
				matches = false
				break
			}
		}
		if matches {
			return
		}
	}

	t.Fatalf("metric family %s missing labels %v", family.GetName(), want)
}

func metricLabelValue(metric interface{ GetLabel() []*dto.LabelPair }, name string) string {
	for _, label := range metric.GetLabel() {
		if label.GetName() == name {
			return label.GetValue()
		}
	}
	return ""
}
