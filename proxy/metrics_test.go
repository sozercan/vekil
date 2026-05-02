package proxy

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func scrapeMetrics(t *testing.T, handler *ProxyHandler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	w := httptest.NewRecorder()
	handler.MetricsHandler().ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("GET /metrics status = %d, want 200", w.Code)
	}
	return w.Body.String()
}

func requireMetricSample(t *testing.T, body, name string, labels map[string]string, wantValue float64) {
	t.Helper()
	if hasMetricSample(body, name, labels, wantValue) {
		return
	}
	t.Fatalf("metrics output missing %s%v %v:\n%s", name, labels, wantValue, body)
}

func hasMetricSample(body, name string, wantLabels map[string]string, wantValue float64) bool {
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		gotName, gotLabels, gotValue, ok := parseMetricSampleLine(line)
		if !ok || gotName != name || len(gotLabels) != len(wantLabels) {
			continue
		}

		match := true
		for key, want := range wantLabels {
			if gotLabels[key] != want {
				match = false
				break
			}
		}
		if !match {
			continue
		}

		value, err := strconv.ParseFloat(gotValue, 64)
		if err != nil {
			continue
		}
		if value == wantValue {
			return true
		}
	}

	return false
}

func parseMetricSampleLine(line string) (string, map[string]string, string, bool) {
	fields := strings.Fields(line)
	if len(fields) < 2 {
		return "", nil, "", false
	}

	nameAndLabels := fields[0]
	open := strings.IndexByte(nameAndLabels, '{')
	close := strings.LastIndexByte(nameAndLabels, '}')
	if open < 0 || close < open {
		return "", nil, "", false
	}

	labels := map[string]string{}
	if labelText := nameAndLabels[open+1 : close]; labelText != "" {
		for _, entry := range strings.Split(labelText, ",") {
			key, rawValue, ok := strings.Cut(entry, "=")
			if !ok {
				return "", nil, "", false
			}

			value, err := strconv.Unquote(rawValue)
			if err != nil {
				return "", nil, "", false
			}
			labels[key] = value
		}
	}

	return nameAndLabels[:open], labels, fields[1], true
}

func TestMetrics_HandleOpenAIChatCompletions(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/chat/completions" {
			t.Fatalf("upstream path = %q, want /chat/completions", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"gpt-4","choices":[{"index":0,"message":{"role":"assistant","content":"hello"}}],"usage":{"prompt_tokens":11,"completion_tokens":7,"total_tokens":18}}`))
	}))
	defer backend.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		withCopilotBaseURLForTest(backend.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.retryBaseDelay = time.Millisecond

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleOpenAIChatCompletions(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HandleOpenAIChatCompletions status = %d, want 200", w.Code)
	}

	body := scrapeMetrics(t, handler)

	requireMetricSample(t, body, "vekil_requests_total", map[string]string{
		"provider":     "copilot",
		"public_model": metricsPublicModelUnresolved,
		"endpoint":     "/v1/chat/completions",
		"status":       "200",
	}, 1)
	requireMetricSample(t, body, "vekil_tokens_total", map[string]string{
		"provider":     "copilot",
		"public_model": metricsPublicModelUnresolved,
		"direction":    "prompt",
	}, 11)
	requireMetricSample(t, body, "vekil_tokens_total", map[string]string{
		"provider":     "copilot",
		"public_model": metricsPublicModelUnresolved,
		"direction":    "completion",
	}, 7)
	requireMetricSample(t, body, "vekil_inflight_requests", map[string]string{
		"provider": "copilot",
	}, 0)
	if !strings.Contains(body, `vekil_build_info{`) {
		t.Fatalf("metrics output missing %q:\n%s", `vekil_build_info{`, body)
	}
}

func TestMetrics_HandleResponses(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/responses" {
			t.Fatalf("upstream path = %q, want /responses", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","model":"gpt-4","output":[],"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}}`))
	}))
	defer backend.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		withCopilotBaseURLForTest(backend.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-4","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HandleResponses status = %d, want 200", w.Code)
	}

	body := scrapeMetrics(t, handler)
	requireMetricSample(t, body, "vekil_requests_total", map[string]string{
		"provider":     "copilot",
		"public_model": metricsPublicModelUnresolved,
		"endpoint":     "/v1/responses",
		"status":       "200",
	}, 1)
	requireMetricSample(t, body, "vekil_tokens_total", map[string]string{
		"provider":     "copilot",
		"public_model": metricsPublicModelUnresolved,
		"direction":    "prompt",
	}, 9)
	requireMetricSample(t, body, "vekil_tokens_total", map[string]string{
		"provider":     "copilot",
		"public_model": metricsPublicModelUnresolved,
		"direction":    "completion",
	}, 4)
}

func TestMetrics_UnresolvedRequestedModelUsesBoundedLabel(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/responses" {
			t.Fatalf("upstream path = %q, want /responses", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"message":"unsupported model"}}`))
	}))
	defer backend.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		withCopilotBaseURLForTest(backend.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"tenant-a/custom-model","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("HandleResponses status = %d, want 400", w.Code)
	}

	body := scrapeMetrics(t, handler)
	requireMetricSample(t, body, "vekil_requests_total", map[string]string{
		"provider":     "copilot",
		"public_model": metricsPublicModelUnresolved,
		"endpoint":     "/v1/responses",
		"status":       "400",
	}, 1)
	if strings.Contains(body, "tenant-a/custom-model") {
		t.Fatalf("metrics output leaked raw requested model label:\n%s", body)
	}
}

func TestMetrics_KnownConfiguredModelUsesPublicModelLabel(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/openai/v1/responses" {
			t.Fatalf("upstream path = %q, want /openai/v1/responses", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed","model":"gpt-5-4-prod","output":[],"usage":{"input_tokens":9,"output_tokens":4,"total_tokens":13}}`))
	}))
	defer backend.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		WithProvidersConfig(ProvidersConfig{
			Providers: []ProviderConfig{{
				ID:      "azure",
				Type:    "azure-openai",
				Default: true,
				BaseURL: backend.URL + "/openai/v1",
				APIKey:  "azure-test-key",
				Models: []ProviderModelConfig{{
					PublicID:   "gpt-5-public",
					Deployment: "gpt-5-4-prod",
					Endpoints:  []string{"/responses"},
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5-public","input":"hello"}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleResponses(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HandleResponses status = %d, want 200", w.Code)
	}

	body := scrapeMetrics(t, handler)
	requireMetricSample(t, body, "vekil_requests_total", map[string]string{
		"provider":     "azure",
		"public_model": "gpt-5-public",
		"endpoint":     "/v1/responses",
		"status":       "200",
	}, 1)
	requireMetricSample(t, body, "vekil_tokens_total", map[string]string{
		"provider":     "azure",
		"public_model": "gpt-5-public",
		"direction":    "prompt",
	}, 9)
	requireMetricSample(t, body, "vekil_tokens_total", map[string]string{
		"provider":     "azure",
		"public_model": "gpt-5-public",
		"direction":    "completion",
	}, 4)
}

func TestMetrics_HandleAnthropicMessages(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/chat/completions" {
			t.Fatalf("upstream path = %q, want /chat/completions", got)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"id":"chunk-1","object":"chat.completion.chunk","created":1,"model":"claude-sonnet-4.5","choices":[{"index":0,"delta":{"content":"Hello"}}]}`,
			``,
			`data: {"id":"chunk-1","object":"chat.completion.chunk","created":1,"model":"claude-sonnet-4.5","choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":4,"completion_tokens":2,"total_tokens":6}}`,
			``,
			`data: [DONE]`,
			``,
		}, "\n")))
	}))
	defer backend.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelInfo),
		withCopilotBaseURLForTest(backend.URL),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"claude-sonnet-4.5","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("HandleAnthropicMessages status = %d, want 200", w.Code)
	}

	body := scrapeMetrics(t, handler)
	requireMetricSample(t, body, "vekil_requests_total", map[string]string{
		"provider":     "copilot",
		"public_model": metricsPublicModelUnresolved,
		"endpoint":     "/v1/messages",
		"status":       "200",
	}, 1)
	requireMetricSample(t, body, "vekil_tokens_total", map[string]string{
		"provider":     "copilot",
		"public_model": metricsPublicModelUnresolved,
		"direction":    "prompt",
	}, 4)
	requireMetricSample(t, body, "vekil_tokens_total", map[string]string{
		"provider":     "copilot",
		"public_model": metricsPublicModelUnresolved,
		"direction":    "completion",
	}, 2)
}

func TestProviderHealthEndpoint_RedactsBaseURL(t *testing.T) {
	provider := &providerRuntime{
		id:      "azure",
		kind:    providerTypeAzureOpenAI,
		baseURL: "https://user:secret@example.openai.azure.com:8443/openai/v1",
	}

	if got, want := providerHealthEndpoint(provider), "example.openai.azure.com"; got != want {
		t.Fatalf("providerHealthEndpoint() = %q, want %q", got, want)
	}
}
