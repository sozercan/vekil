package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func newRouteAttemptStatsTestHandler(t *testing.T, kind providerType, endpoint, upstreamURL string, maxSends int) *ProxyHandler {
	t.Helper()
	if maxSends <= 0 {
		maxSends = 1
	}
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(ProvidersConfig{
			SchemaVersion: ProvidersConfigSchemaVersion2,
			Providers: []ProviderConfig{{
				ID:       "upstream",
				Type:     string(kind),
				Default:  true,
				BaseURL:  upstreamURL,
				AuthType: "none",
			}},
			ModelRoutes: []ModelRouteConfig{{
				ID:        "route-public",
				PublicID:  "public-model",
				Endpoints: []string{endpoint},
				Targets: []ModelRouteTargetConfig{{
					ID:            "target-upstream",
					Provider:      "upstream",
					UpstreamModel: "physical-model",
				}},
				Routing: ModelRouteRoutingConfig{
					Mode:              string(routeModePrimaryOnly),
					MaxTargetAttempts: 1,
					MaxUpstreamSends:  maxSends,
				},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)
	return h
}

func assertNoRouteAttemptStats(t *testing.T, h *ProxyHandler) {
	t.Helper()
	snap := h.stats.snapshot()
	if snap.UpstreamAttempts != 0 || snap.TargetSwitches != 0 || snap.RouteExhaustions != 0 || len(snap.ByTarget) != 0 {
		t.Fatalf("route-attempt stats = attempts:%d switches:%d exhaustions:%d by_target:%+v, want all suppressed",
			snap.UpstreamAttempts, snap.TargetSwitches, snap.RouteExhaustions, snap.ByTarget)
	}
}

func TestStandaloneNonInferenceExplicitRoutesSuppressRouteAttemptStats(t *testing.T) {
	tests := []struct {
		name             string
		providerKind     providerType
		routeEndpoint    string
		wantUpstreamPath string
		requestPath      string
		requestBody      string
		upstreamResponse string
		handle           func(*ProxyHandler, http.ResponseWriter, *http.Request)
	}{
		{
			name:             "translated anthropic count tokens",
			providerKind:     providerTypeOpenAICompatible,
			routeEndpoint:    providerEndpointChatCompletions,
			wantUpstreamPath: providerEndpointChatCompletions,
			requestPath:      "/v1/messages/count_tokens",
			requestBody:      `{"model":"public-model","messages":[{"role":"user","content":"count me"}]}`,
			upstreamResponse: `{"id":"chatcmpl-count","object":"chat.completion","created":1,"model":"physical-model","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":8}}`,
			handle:           (*ProxyHandler).HandleAnthropicMessagesCountTokens,
		},
		{
			name:             "gemini count tokens",
			providerKind:     providerTypeOpenAICompatible,
			routeEndpoint:    providerEndpointChatCompletions,
			wantUpstreamPath: providerEndpointChatCompletions,
			requestPath:      "/v1beta/models/public-model:countTokens",
			requestBody:      `{"contents":[{"role":"user","parts":[{"text":"count me"}]}]}`,
			upstreamResponse: `{"id":"chatcmpl-gemini-count","object":"chat.completion","created":1,"model":"physical-model","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":1,"total_tokens":8}}`,
			handle:           (*ProxyHandler).HandleGeminiModels,
		},
		{
			name:             "direct anthropic count tokens",
			providerKind:     providerTypeAnthropicCompatible,
			routeEndpoint:    providerEndpointMessages,
			wantUpstreamPath: providerEndpointMessagesCount,
			requestPath:      "/v1/messages/count_tokens",
			requestBody:      `{"model":"public-model","messages":[{"role":"user","content":"count me"}]}`,
			upstreamResponse: `{"input_tokens":7}`,
			handle:           (*ProxyHandler).HandleAnthropicMessagesCountTokens,
		},
		{
			name:             "responses compact",
			providerKind:     providerTypeOpenAICompatible,
			routeEndpoint:    providerEndpointResponses,
			wantUpstreamPath: providerEndpointResponses,
			requestPath:      "/v1/responses/compact",
			requestBody:      `{"model":"public-model","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"compact me"}]}]}`,
			upstreamResponse: `{"id":"resp-compact","object":"response","status":"completed","model":"physical-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint summary"}]}]}`,
			handle:           (*ProxyHandler).HandleCompact,
		},
		{
			name:             "memory trace summarize",
			providerKind:     providerTypeOpenAICompatible,
			routeEndpoint:    providerEndpointResponses,
			wantUpstreamPath: providerEndpointResponses,
			requestPath:      "/v1/memories/trace_summarize",
			requestBody:      `{"model":"public-model","traces":[{"id":"trace-1","items":[]}]}`,
			upstreamResponse: `{"id":"resp-memory","object":"response","status":"completed","model":"physical-model","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"[{\"trace_summary\":\"trace\",\"memory_summary\":\"memory\"}]"}]}]}`,
			handle:           (*ProxyHandler).HandleMemorySummarize,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				upstreamCalls.Add(1)
				if r.URL.Path != tt.wantUpstreamPath {
					t.Errorf("upstream path = %q, want %q", r.URL.Path, tt.wantUpstreamPath)
				}
				var body map[string]json.RawMessage
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Errorf("decode upstream request: %v", err)
				} else if got := rawJSONString(body["model"]); got != "physical-model" {
					t.Errorf("upstream model = %q, want physical-model", got)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, tt.upstreamResponse)
			}))
			defer upstream.Close()

			h := newRouteAttemptStatsTestHandler(t, tt.providerKind, tt.routeEndpoint, upstream.URL, 1)
			ctx, summary := WithRequestSummary(context.Background())
			req := httptest.NewRequest(http.MethodPost, tt.requestPath, strings.NewReader(tt.requestBody)).WithContext(ctx)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			tt.handle(h, w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if got := upstreamCalls.Load(); got != 1 {
				t.Fatalf("upstream calls = %d, want 1", got)
			}
			if got := w.Header().Get("X-Vekil-Request-ID"); got == "" || got != summary.OperationID() {
				t.Fatalf("X-Vekil-Request-ID = %q, summary operation = %q", got, summary.OperationID())
			}
			if summary.RouteID() != "route-public" || summary.UpstreamSendCount() != 0 || summary.TargetSwitchCount() != 0 || summary.RouteExhausted() {
				t.Fatalf("summary route=%q sends=%d switches=%d exhausted=%v, want suppressed physical attribution",
					summary.RouteID(), summary.UpstreamSendCount(), summary.TargetSwitchCount(), summary.RouteExhausted())
			}
			assertNoRouteAttemptStats(t, h)
		})
	}
}

func TestStandaloneNonInferenceSuppressionIncludesSwitchAndExhaustion(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int32
	quotaResponse := func(calls *atomic.Int32) http.HandlerFunc {
		return func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"quota","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
		}
	}
	primary := httptest.NewServer(quotaResponse(&primaryCalls))
	defer primary.Close()
	secondary := httptest.NewServer(quotaResponse(&secondaryCalls))
	defer secondary.Close()

	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(ProvidersConfig{
			SchemaVersion: ProvidersConfigSchemaVersion2,
			Providers: []ProviderConfig{
				{ID: "primary", Type: string(providerTypeOpenAICompatible), Default: true, BaseURL: primary.URL, AuthType: "none"},
				{ID: "secondary", Type: string(providerTypeOpenAICompatible), BaseURL: secondary.URL, AuthType: "none"},
			},
			ModelRoutes: []ModelRouteConfig{{
				ID:        "route-public",
				PublicID:  "public-model",
				Endpoints: []string{providerEndpointResponses},
				Targets: []ModelRouteTargetConfig{
					{ID: "target-primary", Provider: "primary", UpstreamModel: "physical-primary"},
					{ID: "target-secondary", Provider: "secondary", UpstreamModel: "physical-secondary"},
				},
				Routing: ModelRouteRoutingConfig{Mode: string(routeModePriorityFailover), MaxTargetAttempts: 2, MaxUpstreamSends: 2},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	defer h.BeginShutdown()

	ctx, summary := WithRequestSummary(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/memories/trace_summarize", strings.NewReader(`{"model":"public-model","traces":[{"id":"trace-1","items":[]}]}`)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.HandleMemorySummarize(w, req)

	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429; body=%s", w.Code, w.Body.String())
	}
	if primaryCalls.Load() != 1 || secondaryCalls.Load() != 1 {
		t.Fatalf("upstream calls primary=%d secondary=%d, want 1/1", primaryCalls.Load(), secondaryCalls.Load())
	}
	if summary.UpstreamSendCount() != 0 || summary.TargetSwitchCount() != 0 || summary.RouteExhausted() {
		t.Fatalf("summary sends=%d switches=%d exhausted=%v, want failover topology suppressed",
			summary.UpstreamSendCount(), summary.TargetSwitchCount(), summary.RouteExhausted())
	}
	assertNoRouteAttemptStats(t, h)
}

func TestCountedResponsesReplayChildrenRetainRouteAttemptStats(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := upstreamCalls.Add(1)
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("read upstream request: %v", err)
		}
		var body map[string]any
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Fatalf("decode upstream request: %v", err)
		}
		if got, _ := body["model"].(string); got != "physical-model" {
			t.Errorf("upstream model = %q, want physical-model", got)
		}

		w.Header().Set("Content-Type", "application/json")
		if instructions, _ := body["instructions"].(string); strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
			_, _ = io.WriteString(w, `{"id":"resp-compact-child","object":"response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checkpoint summary"}]}]}`)
			return
		}
		switch call {
		case 1:
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			_, _ = io.WriteString(w, `{"error":{"message":"request too large","code":"payload_too_large"}}`)
		case 3:
			_, _ = io.WriteString(w, `{"id":"resp-replayed","object":"response","status":"completed","output":[]}`)
		default:
			t.Fatalf("unexpected normal upstream call %d", call)
		}
	}))
	defer upstream.Close()

	h := newRouteAttemptStatsTestHandler(t, providerTypeOpenAICompatible, providerEndpointResponses, upstream.URL, 3)
	h.responsesWS = ResponsesWebSocketConfig{DisableAutoCompact: true, AutoCompactKeepTail: 2}

	reqBody, err := json.Marshal(map[string]any{
		"model": "public-model",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "first turn"}}},
			map[string]any{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "input_text", "text": "first answer"}}},
			map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "second turn"}}},
			map[string]any{"type": "message", "role": "assistant", "content": []map[string]string{{"type": "input_text", "text": "second answer"}}},
			map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "latest turn"}}},
		},
	})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}

	ctx, summary := WithRequestSummary(context.Background())
	ctx = h.MarkRetryStatsTrackedIfInference(ctx, http.MethodPost, "/v1/responses")
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(reqBody)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	h.HandleResponses(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
	}
	if got := upstreamCalls.Load(); got != 3 {
		t.Fatalf("upstream calls = %d, want 3 (initial + compact child + replay child)", got)
	}
	if got := summary.UpstreamSendCount(); got != 3 {
		t.Fatalf("request summary upstream sends = %d, want 3", got)
	}
	if summary.OperationID() == "" || summary.RouteID() != "route-public" || summary.FinalTarget() != "target-upstream" {
		t.Fatalf("summary operation=%q route=%q target=%q", summary.OperationID(), summary.RouteID(), summary.FinalTarget())
	}

	h.RecordRequest(summary, w.Code, "test-agent", 0)
	snap := h.stats.snapshot()
	if snap.Totals.Requests != 1 {
		t.Fatalf("client requests = %d, want 1", snap.Totals.Requests)
	}
	if snap.UpstreamAttempts != 3 || len(snap.ByTarget) != 1 || snap.ByTarget[0].Attempts != 3 {
		t.Fatalf("physical stats = attempts:%d by_target:%+v, want 3 attempts on one target", snap.UpstreamAttempts, snap.ByTarget)
	}
}
