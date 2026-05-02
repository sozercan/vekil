package proxy

import (
	"net/http"
	"net/http/httptest"
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

	for _, want := range []string{
		`vekil_requests_total{provider="copilot",public_model="gpt-4",endpoint="/v1/chat/completions",status="200"} 1`,
		`vekil_tokens_total{provider="copilot",public_model="gpt-4",direction="prompt"} 11`,
		`vekil_tokens_total{provider="copilot",public_model="gpt-4",direction="completion"} 7`,
		`vekil_inflight_requests{provider="copilot"} 0`,
		`vekil_build_info{`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q:\n%s", want, body)
		}
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
	for _, want := range []string{
		`vekil_requests_total{provider="copilot",public_model="gpt-4",endpoint="/v1/responses",status="200"} 1`,
		`vekil_tokens_total{provider="copilot",public_model="gpt-4",direction="prompt"} 9`,
		`vekil_tokens_total{provider="copilot",public_model="gpt-4",direction="completion"} 4`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q:\n%s", want, body)
		}
	}
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
	for _, want := range []string{
		`vekil_requests_total{provider="copilot",public_model="claude-sonnet-4.5",endpoint="/v1/messages",status="200"} 1`,
		`vekil_tokens_total{provider="copilot",public_model="claude-sonnet-4.5",direction="prompt"} 4`,
		`vekil_tokens_total{provider="copilot",public_model="claude-sonnet-4.5",direction="completion"} 2`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("metrics output missing %q:\n%s", want, body)
		}
	}
}
