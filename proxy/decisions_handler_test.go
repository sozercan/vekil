package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

const decisionsTestCatalog = `{"data":[
{"id":"example-decisions-model","supported_endpoints":["/v1/decisions"]},
{"id":"example-responses-model","supported_endpoints":["/responses"]},
{"id":"example-chat-model","supported_endpoints":["/chat/completions"]},
{"id":"example-legacy-model"}
]}`

type decisionsTestUpstream struct {
	mu         sync.Mutex
	paths      []string
	bodies     []string
	status     int
	retryAfter string
	response   string
	authValid  bool
	noCatalog  bool
}

func newDecisionsTestHandler(t *testing.T, upstream *decisionsTestUpstream, opts ...Option) *ProxyHandler {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == providerEndpointModels {
			if upstream.noCatalog {
				w.WriteHeader(http.StatusServiceUnavailable)
				return
			}
			_, _ = io.WriteString(w, decisionsTestCatalog)
			return
		}
		body, _ := io.ReadAll(r.Body)
		upstream.mu.Lock()
		upstream.paths = append(upstream.paths, r.URL.Path)
		upstream.bodies = append(upstream.bodies, string(body))
		upstream.authValid = r.Header.Get("Authorization") == "Bearer test-token"
		status, response, retryAfter := upstream.status, upstream.response, upstream.retryAfter
		upstream.mu.Unlock()
		if status == 0 {
			status = http.StatusOK
		}
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, response)
	}))
	t.Cleanup(server.Close)
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), append([]Option{WithCopilotBaseURL(server.URL)}, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	return h
}

func TestDecisionsPassesThroughAdvertisedModel(t *testing.T) {
	const (
		request  = `{"model":"example-decisions-model","input":"The sky is blue.","questions":[{"type":"predicate","name":"is_true","instructions":"Is the statement true?"}]}`
		response = `{"model":"example-decisions-model","answers":[{"type":"predicate","name":"is_true","probability":0.9}],"usage":{"input_tokens":12,"output_tokens":3,"total_tokens":15}}`
	)
	upstream := &decisionsTestUpstream{response: response}
	h := newDecisionsTestHandler(t, upstream)
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(request))
	ctx, summary := WithRequestSummary(req.Context())
	rec := httptest.NewRecorder()
	h.HandleDecisions(rec, req.WithContext(ctx))

	if rec.Code != http.StatusOK || rec.Body.String() != response {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.paths) != 1 || upstream.paths[0] != "/v1/decisions" || upstream.bodies[0] != request || !upstream.authValid {
		t.Fatalf("upstream paths=%v bodies=%v auth=%v", upstream.paths, upstream.bodies, upstream.authValid)
	}
	fields := map[string]any{}
	for _, field := range summary.LoggerFields() {
		fields[field.Key] = field.Value
	}
	if fields["endpoint"] != "decisions" || fields["provider_kind"] != "copilot" ||
		fields["prompt_tokens"] != 12 || fields["completion_tokens"] != 3 {
		t.Fatalf("summary fields = %#v", fields)
	}
	totals := h.stats.snapshot().TaskUsage.Totals
	if totals.Sends != 1 || totals.Completed != 1 || totals.Errors != 0 ||
		totals.Usage.PromptTokens != 12 || totals.Usage.CompletionTokens != 3 {
		t.Fatalf("task usage = %+v", totals)
	}
}

func TestDecisionsRejectsModelsWithoutAdvertisedEndpoint(t *testing.T) {
	tests := []struct {
		name    string
		request string
		message string
	}{
		{"chat model", `{"model":"example-chat-model","input":"x"}`, `does not support /v1/decisions`},
		{"other endpoint", `{"model":"example-responses-model"}`, `does not support /v1/decisions`},
		{"no advertised endpoints", `{"model":"example-legacy-model"}`, `does not support /v1/decisions`},
		{"unknown model", `{"model":"example-missing-model"}`, `does not support /v1/decisions`},
		{"missing model", `{"input":"x"}`, `model is required`},
		{"non-string model", `{"model":7}`, `model is required`},
		{"padded model", `{"model":" example-decisions-model"}`, `surrounding whitespace`},
		{"case-variant model key", `{"model":"example-chat-model","Model":"example-decisions-model"}`, `unsupported field \"Model\"`},
		{"only a case-variant model key", `{"MODEL":"example-decisions-model"}`, `unsupported field \"MODEL\"`},
		{"not an object", `[]`, `JSON object`},
		{"null body", `null`, `JSON object`},
		{"duplicate model", `{"model":"example-chat-model","model":"example-decisions-model"}`, `invalid JSON`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstream := &decisionsTestUpstream{response: `{}`}
			h := newDecisionsTestHandler(t, upstream)
			rec := httptest.NewRecorder()
			h.HandleDecisions(rec, httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(tt.request)))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), tt.message) {
				t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
			}
			upstream.mu.Lock()
			defer upstream.mu.Unlock()
			if len(upstream.paths) != 0 {
				t.Fatalf("rejected request reached upstream: %v", upstream.paths)
			}
		})
	}
}

// An openai-compatible catalog can advertise /v1/decisions, but that provider
// type routes only Chat and Responses paths under its base URL.
func TestDecisionsRejectsOpenAICompatibleProvider(t *testing.T) {
	var mu sync.Mutex
	var inference []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.URL.Path == providerEndpointModels {
			_, _ = io.WriteString(w, `{"object":"list","data":[{"id":"example-decisions-model","supported_endpoints":["/v1/decisions"]}]}`)
			return
		}
		mu.Lock()
		inference = append(inference, r.URL.Path)
		mu.Unlock()
	}))
	t.Cleanup(server.Close)
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID: "compatible", Type: "openai-compatible", Default: true, BaseURL: server.URL, AuthType: "none", ModelDiscovery: "openai",
		}}}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)

	rec := httptest.NewRecorder()
	h.HandleDecisions(rec, httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(`{"model":"example-decisions-model","input":"x"}`)))
	// The provider-level message shows the catalog gate passed and dispatch refused.
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `provider \"compatible\" does not support /v1/decisions`) {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(inference) != 0 {
		t.Fatalf("rejected request reached upstream: %v", inference)
	}
}

func TestDecisionsPassesThroughUpstreamErrors(t *testing.T) {
	upstream := &decisionsTestUpstream{status: http.StatusUnprocessableEntity, response: `{"error":{"message":"bad question"}}`}
	h := newDecisionsTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	h.HandleDecisions(rec, httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(`{"model":"example-decisions-model"}`)))
	if rec.Code != http.StatusUnprocessableEntity || rec.Body.String() != upstream.response {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
}

func TestDecisionsIsTrackedInferenceRoute(t *testing.T) {
	if !isInferenceRoute(http.MethodPost, "/v1/decisions") {
		t.Fatal("/v1/decisions is not tracked as an inference route")
	}
}

func TestDecisionsHonorsAllowedModels(t *testing.T) {
	upstream := &decisionsTestUpstream{response: `{}`}
	h := newDecisionsTestHandler(t, upstream, WithAllowedModels("example-chat-model"))
	rec := httptest.NewRecorder()
	h.HandleDecisions(rec, httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(`{"model":"example-decisions-model"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "not allowed by this proxy") {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.paths) != 0 {
		t.Fatalf("disallowed model reached upstream: %v", upstream.paths)
	}
}

func TestDecisionsFailsClosedWithoutCatalog(t *testing.T) {
	upstream := &decisionsTestUpstream{response: `{}`, noCatalog: true}
	h := newDecisionsTestHandler(t, upstream)
	rec := httptest.NewRecorder()
	h.HandleDecisions(rec, httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(`{"model":"example-decisions-model"}`)))
	if rec.Code < http.StatusInternalServerError {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.paths) != 0 {
		t.Fatalf("request without a catalog reached upstream: %v", upstream.paths)
	}
}

// decisionsTestChatRequest builds a Chat request the way the proxy sends one,
// so it carries the same Copilot account and integration identity as a
// Decisions request from the same handler.
func decisionsTestChatRequest(t *testing.T, h *ProxyHandler) *http.Request {
	t.Helper()
	body := []byte(`{"model":"example-chat-model","messages":[{"role":"user","content":"hi"}]}`)
	provider, owner, body, err := h.resolveProviderRequest(body, providerEndpointChatCompletions)
	if err != nil {
		t.Fatal(err)
	}
	req, err := h.newProviderJSONInferenceRequest(context.Background(), provider, http.MethodPost, providerEndpointChatCompletions, body, nil, "", owner)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = req.Body.Close() })
	return req
}

func TestDecisionsHonorsCopilotAccountCooldown(t *testing.T) {
	upstream := &decisionsTestUpstream{response: `{}`}
	h := newDecisionsTestHandler(t, upstream)
	chat := decisionsTestChatRequest(t, h)
	h.copilotTraffic.observeThrottle(copilotTrafficTestMetadata(t, chat), http.StatusTooManyRequests, "604800", []byte(`{"error":{"code":"user_global_rate_limited"}}`))

	rec := httptest.NewRecorder()
	h.HandleDecisions(rec, httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(`{"model":"example-decisions-model"}`)))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	upstream.mu.Lock()
	defer upstream.mu.Unlock()
	if len(upstream.paths) != 0 {
		t.Fatalf("request during an account cooldown reached upstream: %v", upstream.paths)
	}
}

func TestDecisionsThrottleSetsSharedCooldown(t *testing.T) {
	upstream := &decisionsTestUpstream{
		status:     http.StatusTooManyRequests,
		retryAfter: "604800",
		response:   `{"error":{"code":"user_global_rate_limited","message":"rate limited"}}`,
	}
	h := newDecisionsTestHandler(t, upstream)
	send := func() int {
		rec := httptest.NewRecorder()
		h.HandleDecisions(rec, httptest.NewRequest(http.MethodPost, "/v1/decisions", strings.NewReader(`{"model":"example-decisions-model"}`)))
		return rec.Code
	}
	if code := send(); code != http.StatusTooManyRequests {
		t.Fatalf("first status = %d", code)
	}
	if code := send(); code != http.StatusTooManyRequests {
		t.Fatalf("second status = %d", code)
	}
	upstream.mu.Lock()
	sent := len(upstream.paths)
	upstream.mu.Unlock()
	if sent != 1 {
		t.Fatalf("upstream sends = %d, want 1: the throttle did not set a cooldown", sent)
	}

	permit, blocked, err := h.acquireCopilotInference(decisionsTestChatRequest(t, h))
	permit.release()
	if err != nil || blocked == nil {
		t.Fatalf("chat admission after a Decisions throttle: blocked=%v err=%v", blocked != nil, err)
	}
	_ = blocked.Body.Close()
}

func TestObserveResponseProviderUsesServingProvider(t *testing.T) {
	ctx, summary := WithRequestSummary(context.Background())
	summary.setProvider("copilot", "copilot")
	req := httptest.NewRequest(http.MethodPost, "/v1/decisions", nil)
	req = req.WithContext(context.WithValue(req.Context(), providerRouteContextKey{}, providerRouteInfo{id: "discovered", kind: "openai"}))

	observeResponseProvider(ctx, &http.Response{Request: req})

	fields := map[string]any{}
	for _, field := range summary.LoggerFields() {
		fields[field.Key] = field.Value
	}
	if fields["provider"] != "discovered" || fields["provider_kind"] != "openai" {
		t.Fatalf("summary fields = %#v", fields)
	}
}
