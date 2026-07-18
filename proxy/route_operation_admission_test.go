package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func newOperationAdmissionTestHandler(t testing.TB, providerKind providerType, endpoints []string, upstreamURL string) *ProxyHandler {
	t.Helper()
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(ProvidersConfig{
			SchemaVersion: ProvidersConfigSchemaVersion2,
			Providers: []ProviderConfig{{
				ID:       "upstream",
				Type:     string(providerKind),
				Default:  true,
				BaseURL:  upstreamURL,
				AuthType: "none",
			}},
			ModelRoutes: []ModelRouteConfig{{
				ID:        "admission-route",
				PublicID:  "public-model",
				Endpoints: endpoints,
				Targets: []ModelRouteTargetConfig{{
					ID:            "target-upstream",
					Provider:      "upstream",
					UpstreamModel: "physical-model",
				}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)
	return h
}

func assertExplicitAdmissionOperationID(t testing.TB, w *httptest.ResponseRecorder, summary *RequestSummary, wantRouteID string) string {
	t.Helper()
	operationID := w.Header().Get("X-Vekil-Request-ID")
	if operationID == "" {
		t.Fatal("missing X-Vekil-Request-ID")
	}
	if summary == nil || summary.OperationID() != operationID {
		got := ""
		if summary != nil {
			got = summary.OperationID()
		}
		t.Fatalf("X-Vekil-Request-ID = %q, summary operation ID = %q", operationID, got)
	}
	if summary.RouteID() != wantRouteID {
		t.Fatalf("summary route ID = %q, want %q", summary.RouteID(), wantRouteID)
	}
	return operationID
}

func latestAccountedRouteAttempt(t testing.TB, h *ProxyHandler, operation *routeOperation) recentRouteAttempt {
	t.Helper()
	if h == nil || h.stats == nil {
		t.Fatal("handler stats collector is unavailable")
	}
	if operation == nil {
		t.Fatal("admitted route operation is unavailable")
		return recentRouteAttempt{}
	}

	h.stats.mu.Lock()
	if h.stats.recentAttemptSize != 1 {
		h.stats.mu.Unlock()
		t.Fatalf("recent route attempts = %d, want exactly 1", h.stats.recentAttemptSize)
	}
	index := (h.stats.recentAttemptIdx - 1 + len(h.stats.recentAttempts)) % len(h.stats.recentAttempts)
	attempt := h.stats.recentAttempts[index]
	h.stats.mu.Unlock()
	if attempt.state == nil {
		t.Fatal("latest route attempt is missing accounting state")
	}

	operation.mu.bind(operation)
	operation.mu.Lock()
	record := operation.attemptRecords[attempt.Sequence]
	operation.mu.Unlock()
	if record == nil || record.state != attempt.state {
		t.Fatalf("admitted operation attempt record = %p/%p, stats record = %p", record, func() *routeAttemptRecordState {
			if record != nil {
				return record.state
			}
			return nil
		}(), attempt.state)
	}
	return attempt
}

func TestExplicitRouteOperationAdmissionCoversRouteLocalValidation(t *testing.T) {
	tests := []struct {
		name         string
		providerKind providerType
		endpoints    []string
		path         string
		body         string
		handle       func(*ProxyHandler, http.ResponseWriter, *http.Request)
		wantDetail   string
	}{
		{
			name:         "Responses duplicate key",
			providerKind: providerTypeOpenAICompatible,
			endpoints:    []string{providerEndpointResponses},
			path:         "/v1/responses",
			body:         `{"model":"public-model","input":"first","input":"second"}`,
			handle:       (*ProxyHandler).HandleResponses,
			wantDetail:   "duplicate",
		},
		{
			name:         "Responses compact duplicate key",
			providerKind: providerTypeOpenAICompatible,
			endpoints:    []string{providerEndpointResponses},
			path:         "/v1/responses/compact",
			body:         `{"model":"public-model","input":"first","input":"second"}`,
			handle:       (*ProxyHandler).HandleCompact,
			wantDetail:   "duplicate",
		},
		{
			name:         "Responses memory duplicate key",
			providerKind: providerTypeOpenAICompatible,
			endpoints:    []string{providerEndpointResponses},
			path:         "/v1/memories/trace_summarize",
			body:         `{"model":"public-model","traces":[],"traces":[]}`,
			handle:       (*ProxyHandler).HandleMemorySummarize,
			wantDetail:   "duplicate",
		},
		{
			name:         "OpenAI Chat duplicate key",
			providerKind: providerTypeOpenAICompatible,
			endpoints:    []string{providerEndpointChatCompletions},
			path:         "/v1/chat/completions",
			body:         `{"model":"public-model","messages":[{"role":"user","content":"first"}],"messages":[{"role":"user","content":"second"}]}`,
			handle:       (*ProxyHandler).HandleOpenAIChatCompletions,
			wantDetail:   "duplicate",
		},
		{
			name:         "OpenAI Chat strict validation",
			providerKind: providerTypeOpenAICompatible,
			endpoints:    []string{providerEndpointChatCompletions},
			path:         "/v1/chat/completions",
			body:         `{"model":"public-model","messages":[]}`,
			handle:       (*ProxyHandler).HandleOpenAIChatCompletions,
			wantDetail:   "messages must be a non-empty array",
		},
		{
			name:         "Anthropic duplicate key",
			providerKind: providerTypeAnthropicCompatible,
			endpoints:    []string{providerEndpointMessages},
			path:         "/v1/messages",
			body:         `{"model":"public-model","max_tokens":16,"messages":[{"role":"user","content":"first"}],"messages":[{"role":"user","content":"second"}]}`,
			handle:       (*ProxyHandler).HandleAnthropicMessages,
			wantDetail:   "duplicate",
		},
		{
			name:         "Gemini duplicate key",
			providerKind: providerTypeOpenAICompatible,
			endpoints:    []string{providerEndpointChatCompletions},
			path:         "/v1beta/models/public-model:generateContent",
			body:         `{"contents":[{"role":"user","parts":[{"text":"first"}]}],"contents":[{"role":"user","parts":[{"text":"second"}]}]}`,
			handle:       (*ProxyHandler).HandleGeminiModels,
			wantDetail:   "duplicate",
		},
		{
			name:         "Gemini strict validation",
			providerKind: providerTypeOpenAICompatible,
			endpoints:    []string{providerEndpointChatCompletions},
			path:         "/v1beta/models/public-model:generateContent",
			body:         `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"unexpected_field":true}`,
			handle:       (*ProxyHandler).HandleGeminiModels,
			wantDetail:   "not supported or unknown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				http.Error(w, "unexpected upstream request", http.StatusInternalServerError)
			}))
			defer upstream.Close()

			h := newOperationAdmissionTestHandler(t, tt.providerKind, tt.endpoints, upstream.URL)
			ctx, summary := WithRequestSummary(context.Background())
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body)).WithContext(ctx)
			w := httptest.NewRecorder()
			tt.handle(h, w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.wantDetail) {
				t.Fatalf("body = %s, want detail %q", w.Body.String(), tt.wantDetail)
			}
			if upstreamCalls.Load() != 0 {
				t.Fatalf("upstream calls = %d, want 0", upstreamCalls.Load())
			}
			assertExplicitAdmissionOperationID(t, w, summary, "admission-route")
		})
	}
}

func TestExplicitRoutesRespectAllowedModelScopeAcrossHTTPHandlers(t *testing.T) {
	tests := []struct {
		name         string
		providerKind providerType
		endpoint     string
		path         string
		body         string
		handle       func(*ProxyHandler, http.ResponseWriter, *http.Request)
	}{
		{
			name:         "Responses",
			providerKind: providerTypeOpenAICompatible,
			endpoint:     providerEndpointResponses,
			path:         "/v1/responses",
			body:         `{"model":"other-model","input":"hi"}`,
			handle:       (*ProxyHandler).HandleResponses,
		},
		{
			name:         "OpenAI Chat",
			providerKind: providerTypeOpenAICompatible,
			endpoint:     providerEndpointChatCompletions,
			path:         "/v1/chat/completions",
			body:         `{"model":"other-model","messages":[{"role":"user","content":"hi"}]}`,
			handle:       (*ProxyHandler).HandleOpenAIChatCompletions,
		},
		{
			name:         "Anthropic Messages",
			providerKind: providerTypeAnthropicCompatible,
			endpoint:     providerEndpointMessages,
			path:         "/v1/messages",
			body:         `{"model":"other-model","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`,
			handle:       (*ProxyHandler).HandleAnthropicMessages,
		},
		{
			name:         "Anthropic Count Tokens",
			providerKind: providerTypeAnthropicCompatible,
			endpoint:     providerEndpointMessages,
			path:         "/v1/messages/count_tokens",
			body:         `{"model":"other-model","messages":[{"role":"user","content":"hi"}]}`,
			handle:       (*ProxyHandler).HandleAnthropicMessagesCountTokens,
		},
		{
			name:         "Gemini",
			providerKind: providerTypeOpenAICompatible,
			endpoint:     providerEndpointChatCompletions,
			path:         "/v1beta/models/other-model:generateContent",
			body:         `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			handle:       (*ProxyHandler).HandleGeminiModels,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				http.Error(w, "unexpected upstream request", http.StatusInternalServerError)
			}))
			defer upstream.Close()

			h, err := NewProxyHandler(
				auth.NewTestAuthenticator("test-token"),
				logger.NewWithWriter(logger.LevelError, io.Discard),
				WithAllowedModels("selected-model"),
				WithProvidersConfig(ProvidersConfig{
					SchemaVersion: ProvidersConfigSchemaVersion2,
					Providers: []ProviderConfig{{
						ID: "upstream", Type: string(tt.providerKind), Default: true,
						BaseURL: upstream.URL, AuthType: "none",
					}},
					ModelRoutes: []ModelRouteConfig{
						{
							ID: "selected-route", PublicID: "selected-model", Endpoints: []string{tt.endpoint},
							Targets: []ModelRouteTargetConfig{{ID: "selected", Provider: "upstream", UpstreamModel: "physical-selected"}},
						},
						{
							ID: "other-route", PublicID: "other-model", Endpoints: []string{tt.endpoint},
							Targets: []ModelRouteTargetConfig{{ID: "other", Provider: "upstream", UpstreamModel: "physical-other"}},
						},
					},
				}),
			)
			if err != nil {
				t.Fatalf("NewProxyHandler() error = %v", err)
			}
			defer h.BeginShutdown()

			w := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body))
			tt.handle(h, w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "not allowed") {
				t.Fatalf("body = %s, want model-not-allowed detail", w.Body.String())
			}
			if upstreamCalls.Load() != 0 {
				t.Fatalf("upstream calls = %d, want 0", upstreamCalls.Load())
			}
		})
	}
}

func TestWithExplicitRouteOperationRejectsDisallowedRouteWithoutPriorAdmission(t *testing.T) {
	h := newOperationAdmissionTestHandler(t, providerTypeOpenAICompatible, []string{providerEndpointResponses}, "https://upstream.invalid")
	h.allowedModels = map[string]struct{}{"selected-model": {}}

	ctx, operation, route, err := h.withExplicitRouteOperation(
		context.Background(),
		context.Background(),
		"public-model",
		providerEndpointResponses,
	)
	if err == nil || !strings.Contains(err.Error(), "not allowed") {
		t.Fatalf("withExplicitRouteOperation() error = %v, want model-not-allowed", err)
	}
	if operation != nil || route != nil {
		t.Fatalf("disallowed route operation = %p, route = %p; want nil", operation, route)
	}
	if routeOperationFromContext(ctx) != nil {
		t.Fatal("disallowed route was attached to context")
	}
}

func TestExplicitRouteOperationAdmissionCoversUnsupportedEndpoints(t *testing.T) {
	tests := []struct {
		name         string
		providerKind providerType
		endpoints    []string
		path         string
		body         string
		handle       func(*ProxyHandler, http.ResponseWriter, *http.Request)
		wantEndpoint string
	}{
		{
			name:         "Responses",
			providerKind: providerTypeOpenAICompatible,
			endpoints:    []string{providerEndpointChatCompletions},
			path:         "/v1/responses",
			body:         `{"model":"public-model","input":"hi"}`,
			handle:       (*ProxyHandler).HandleResponses,
			wantEndpoint: providerEndpointResponses,
		},
		{
			name:         "OpenAI Chat",
			providerKind: providerTypeAnthropicCompatible,
			endpoints:    []string{providerEndpointMessages},
			path:         "/v1/chat/completions",
			body:         `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`,
			handle:       (*ProxyHandler).HandleOpenAIChatCompletions,
			wantEndpoint: providerEndpointChatCompletions,
		},
		{
			name:         "Gemini",
			providerKind: providerTypeOpenAICompatible,
			endpoints:    []string{providerEndpointResponses},
			path:         "/v1beta/models/public-model:countTokens",
			body:         `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			handle:       (*ProxyHandler).HandleGeminiModels,
			wantEndpoint: providerEndpointChatCompletions,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				upstreamCalls.Add(1)
				http.Error(w, "unexpected upstream request", http.StatusInternalServerError)
			}))
			defer upstream.Close()

			h := newOperationAdmissionTestHandler(t, tt.providerKind, tt.endpoints, upstream.URL)
			ctx, summary := WithRequestSummary(context.Background())
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body)).WithContext(ctx)
			w := httptest.NewRecorder()
			tt.handle(h, w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tt.wantEndpoint) {
				t.Fatalf("body = %s, want endpoint %q", w.Body.String(), tt.wantEndpoint)
			}
			if upstreamCalls.Load() != 0 {
				t.Fatalf("upstream calls = %d, want 0", upstreamCalls.Load())
			}
			assertExplicitAdmissionOperationID(t, w, summary, "admission-route")
		})
	}
}

func TestExplicitRouteOperationAdmissionReusesNormalRequestOperation(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		body         string
		endpoint     string
		handle       func(*ProxyHandler, http.ResponseWriter, *http.Request)
		responseBody string
	}{
		{
			name:         "Responses",
			path:         "/v1/responses",
			body:         `{"model":"public-model","input":"hi"}`,
			endpoint:     providerEndpointResponses,
			handle:       (*ProxyHandler).HandleResponses,
			responseBody: `{"id":"resp-normal","object":"response","status":"completed","model":"physical-model","output":[]}`,
		},
		{
			name:         "OpenAI Chat",
			path:         "/v1/chat/completions",
			body:         `{"model":"public-model","messages":[{"role":"user","content":"hi"}]}`,
			endpoint:     providerEndpointChatCompletions,
			handle:       (*ProxyHandler).HandleOpenAIChatCompletions,
			responseBody: `{"id":"chat-normal","object":"chat.completion","model":"physical-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
		},
		{
			name:         "Gemini",
			path:         "/v1beta/models/public-model:generateContent",
			body:         `{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`,
			endpoint:     providerEndpointChatCompletions,
			handle:       (*ProxyHandler).HandleGeminiModels,
			responseBody: `{"id":"chat-gemini-normal","object":"chat.completion","model":"physical-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var upstreamCalls atomic.Int32
			dispatchedOperations := make(chan *routeOperation, 2)
			h := newOperationAdmissionTestHandler(t, providerTypeOpenAICompatible, []string{tt.endpoint}, "https://admission-upstream.example")
			h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				upstreamCalls.Add(1)
				dispatchedOperations <- routeOperationFromContext(req.Context())
				return &http.Response{
					StatusCode:    http.StatusOK,
					Status:        "200 OK",
					Header:        http.Header{"Content-Type": []string{"application/json"}},
					Body:          io.NopCloser(strings.NewReader(tt.responseBody)),
					ContentLength: int64(len(tt.responseBody)),
					Request:       req,
				}, nil
			})}

			ctx, summary := WithRequestSummary(context.Background())
			admissionCtx, admittedOperation, admittedRoute, err := h.withAdmittedExplicitRouteOperation(ctx, ctx, "public-model", tt.endpoint)
			if err != nil {
				t.Fatalf("withAdmittedExplicitRouteOperation() error = %v", err)
			}
			if admittedOperation == nil || admittedRoute == nil || admittedRoute.legacy {
				t.Fatal("explicit route operation was not admitted before execution")
			}
			admissionOperationID := admittedOperation.operationID()
			if admissionOperationID == "" {
				t.Fatal("admission operation ID is empty")
			}

			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body)).WithContext(admissionCtx)
			w := httptest.NewRecorder()
			tt.handle(h, w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200; body=%s", w.Code, w.Body.String())
			}
			if upstreamCalls.Load() != 1 {
				t.Fatalf("upstream calls = %d, want 1", upstreamCalls.Load())
			}
			if got := assertExplicitAdmissionOperationID(t, w, summary, "admission-route"); got != admissionOperationID {
				t.Fatalf("response operation ID = %q, want admission ID %q", got, admissionOperationID)
			}
			if got := summary.UpstreamSendCount(); got != 1 {
				t.Fatalf("summary upstream sends = %d, want 1", got)
			}

			var dispatchedOperation *routeOperation
			select {
			case dispatchedOperation = <-dispatchedOperations:
			default:
				t.Fatal("upstream dispatch did not capture a route operation")
			}
			if dispatchedOperation != admittedOperation {
				t.Fatalf("dispatch operation = %p, want admission operation %p", dispatchedOperation, admittedOperation)
			}
			if got := dispatchedOperation.operationID(); got != admissionOperationID {
				t.Fatalf("dispatch operation ID = %q, want admission ID %q", got, admissionOperationID)
			}
			select {
			case extra := <-dispatchedOperations:
				t.Fatalf("unexpected extra dispatch operation %p", extra)
			default:
			}

			attempt := latestAccountedRouteAttempt(t, h, admittedOperation)
			if attempt.OperationID != admissionOperationID {
				t.Fatalf("attempt accounting operation ID = %q, want admission ID %q", attempt.OperationID, admissionOperationID)
			}
			if attempt.RouteID != "admission-route" || attempt.TargetID != "target-upstream" || attempt.Sequence != 1 {
				t.Fatalf("attempt accounting route=%q target=%q sequence=%d, want admission-route/target-upstream/1", attempt.RouteID, attempt.TargetID, attempt.Sequence)
			}
		})
	}
}

func TestLegacyValidationFailuresDoNotExposeOperationID(t *testing.T) {
	tests := []struct {
		name   string
		path   string
		body   string
		handle func(*ProxyHandler, http.ResponseWriter, *http.Request)
	}{
		{
			name:   "OpenAI Chat",
			path:   "/v1/chat/completions",
			body:   `{"model":"gpt-4o","messages":[]}`,
			handle: (*ProxyHandler).HandleOpenAIChatCompletions,
		},
		{
			name:   "Gemini",
			path:   "/v1beta/models/gemini-2.5-pro:generateContent",
			body:   `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"unexpected_field":true}`,
			handle: (*ProxyHandler).HandleGeminiModels,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestProxyHandler(t, func(w http.ResponseWriter, _ *http.Request) {
				t.Fatal("validation failure reached upstream")
			})
			ctx, summary := WithRequestSummary(context.Background())
			req := httptest.NewRequest(http.MethodPost, tt.path, strings.NewReader(tt.body)).WithContext(ctx)
			w := httptest.NewRecorder()
			tt.handle(h, w, req)

			if w.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%s", w.Code, w.Body.String())
			}
			if got := w.Header().Get("X-Vekil-Request-ID"); got != "" {
				t.Fatalf("legacy X-Vekil-Request-ID = %q, want empty", got)
			}
			if summary.OperationID() != "" || summary.RouteID() != "" {
				t.Fatalf("legacy summary operation=%q route=%q, want empty", summary.OperationID(), summary.RouteID())
			}
		})
	}
}
