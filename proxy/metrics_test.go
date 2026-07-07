package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
)

func newMetricsHandler(t *testing.T, metricsEnabled bool) *ProxyHandler {
	t.Helper()
	opts := []Option{
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:             "test-provider",
				Type:           "openai-compatible",
				Default:        true,
				BaseURL:        "http://127.0.0.1:59999/v1",
				AuthType:       "bearer",
				APIKey:         "mock",
				ModelDiscovery: "static",
				Models: []ProviderModelConfig{
					{PublicID: "test-model", Endpoints: []string{"/chat/completions", "/responses"}},
				},
			}},
		}),
	}
	if metricsEnabled {
		opts = append(opts, WithMetrics("1.0.0-test", "abc1234", "go1.25.0"))
	}
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), nil, opts...)
	if err != nil {
		t.Fatalf("NewProxyHandler: %v", err)
	}
	return h
}

func TestMetricsExposition(t *testing.T) {
	h := newMetricsHandler(t, true)

	// Simulate a completed request to populate metrics.
	summary := &RequestSummary{}
	summary.setRoute("chat_completions", "test-model", false)
	summary.setProvider("test-provider", "openai-compatible")
	prompt := 100
	completion := 50
	total := 150
	summary.mu.Lock()
	summary.promptTokens = &prompt
	summary.completionTokens = &completion
	summary.totalTokens = &total
	summary.mu.Unlock()
	h.RecordRequest(summary, http.StatusOK, "test-agent/1.0", 500*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	h.MetricsHTTPHandler().ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d", rr.Code)
	}

	body := rr.Body.String()

	// Check expected metric families are present.
	// Note: CounterVec/GaugeVec only appear in exposition after first use.
	// We check metrics that our simulated request populated.
	expected := []string{
		"vekil_requests_total",
		"vekil_request_duration_seconds",
		"vekil_tokens_total",
		"vekil_build_info",
		"go_goroutines",
		"process_cpu_seconds_total",
	}
	for _, metric := range expected {
		if !strings.Contains(body, metric) {
			t.Errorf("expected %q in exposition, not found", metric)
		}
	}

	// Check build info labels.
	if !strings.Contains(body, `version="1.0.0-test"`) {
		t.Error("expected build info version label")
	}
	if !strings.Contains(body, `commit="abc1234"`) {
		t.Error("expected build info commit label")
	}
	if !strings.Contains(body, `go_version="go1.25.0"`) {
		t.Error("expected build info go_version label")
	}

	// Check that our simulated request shows up.
	if !strings.Contains(body, `provider="test-provider"`) {
		t.Error("expected provider label from simulated request")
	}
	if !strings.Contains(body, `public_model="test-model"`) {
		t.Error("expected public_model label from simulated request")
	}
}

func TestMetricsDisabled(t *testing.T) {
	h := newMetricsHandler(t, false)

	if h.MetricsEnabled() {
		t.Fatal("expected metrics to be disabled")
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	h.MetricsHTTPHandler().ServeHTTP(rr, req)

	if rr.Code != http.StatusNotFound {
		t.Fatalf("expected 404 when metrics disabled, got %d", rr.Code)
	}
}

func TestMetricsCardinalityBounding(t *testing.T) {
	h := newMetricsHandler(t, true)

	// Model labels longer than statLabelMaxLen should be truncated.
	longModel := strings.Repeat("x", 100)
	summary := &RequestSummary{}
	summary.setRoute("chat_completions", longModel, false)
	summary.setProvider("test-provider", "openai-compatible")
	h.RecordRequest(summary, http.StatusOK, "test/1.0", 100*time.Millisecond)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	h.MetricsHTTPHandler().ServeHTTP(rr, req)

	body := rr.Body.String()
	// The full 100-char model should NOT appear verbatim.
	if strings.Contains(body, longModel) {
		t.Error("expected long model name to be truncated in Prometheus labels")
	}
}

func TestMetricsRetries(t *testing.T) {
	h := newMetricsHandler(t, true)

	// Simulate retries with context carrying the retry-stats marker and a summary.
	ctx, summary := WithRequestSummary(context.TODO())
	summary.setProvider("test-provider", "openai-compatible")
	summary.setRoute("responses", "test-model", true)
	ctx = markRetryStatsTracked(ctx)

	h.logRetryAttempt(ctx, 0, http.StatusTooManyRequests, "", 1*time.Second, nil)
	h.logRetryAttempt(ctx, 1, http.StatusServiceUnavailable, "", 2*time.Second, nil)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	h.MetricsHTTPHandler().ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, `reason="429"`) {
		t.Error("expected retry with reason=429")
	}
	if !strings.Contains(body, `reason="5xx"`) {
		t.Error("expected retry with reason=5xx")
	}
}

func TestMetricsUpstreamErrors(t *testing.T) {
	h := newMetricsHandler(t, true)

	summary := &RequestSummary{}
	summary.setRoute("chat_completions", "test-model", false)
	summary.setProvider("test-provider", "openai-compatible")
	h.RecordRequest(summary, http.StatusBadGateway, "test/1.0", 2*time.Second)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	h.MetricsHTTPHandler().ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, `code="502"`) {
		t.Error("expected upstream error with code=502")
	}
}

func TestMetricsInflight(t *testing.T) {
	h := newMetricsHandler(t, true)

	h.metrics.incInflight("test-provider")
	h.metrics.incInflight("test-provider")
	h.metrics.decInflight("test-provider")

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	h.MetricsHTTPHandler().ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, "vekil_inflight_requests") {
		t.Error("expected inflight metric")
	}
	// Should have value 1 (2 inc - 1 dec).
	if !strings.Contains(body, `vekil_inflight_requests{provider="test-provider"} 1`) {
		t.Errorf("expected inflight=1, body:\n%s", body)
	}
}

func TestMetricsTokens(t *testing.T) {
	h := newMetricsHandler(t, true)

	summary := &RequestSummary{}
	summary.setRoute("chat_completions", "test-model", false)
	summary.setProvider("test-provider", "openai-compatible")
	prompt := 200
	completion := 100
	total := 300
	summary.mu.Lock()
	summary.promptTokens = &prompt
	summary.completionTokens = &completion
	summary.totalTokens = &total
	summary.mu.Unlock()
	h.RecordRequest(summary, http.StatusOK, "test/1.0", 1*time.Second)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rr := httptest.NewRecorder()
	h.MetricsHTTPHandler().ServeHTTP(rr, req)

	body := rr.Body.String()
	if !strings.Contains(body, `direction="prompt"`) {
		t.Error("expected tokens with direction=prompt")
	}
	if !strings.Contains(body, `direction="completion"`) {
		t.Error("expected tokens with direction=completion")
	}
}
