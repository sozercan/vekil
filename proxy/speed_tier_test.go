package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func speedTierIntPtr(v int) *int { return &v }

func newSpeedTierRoutingHandler(t *testing.T, enabled bool, models []ProviderModelConfig) (*ProxyHandler, *bytes.Buffer) {
	t.Helper()
	logs := &bytes.Buffer{}
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelInfo, logs),
		WithProvidersConfig(ProvidersConfig{
			SpeedTierEnabled: enabled,
			Providers: []ProviderConfig{{
				ID:       "test-provider",
				Type:     "openai-compatible",
				Default:  true,
				BaseURL:  "https://upstream.example.com",
				AuthType: "none",
				Models:   models,
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	return handler, logs
}

func speedTierModels() []ProviderModelConfig {
	return []ProviderModelConfig{
		{
			PublicID:   "sonnet-public",
			Deployment: "sonnet-upstream",
			Endpoints:  []string{providerEndpointChatCompletions, providerEndpointResponses, providerEndpointMessages},
			SpeedTier: &SpeedTierConfig{
				DowngradeTo: "haiku-public",
				Semantics:   speedTierSemanticsAll,
				When: SpeedTierWhenConfig{
					MaxTokensLTE:      speedTierIntPtr(512),
					ToolsCountLTE:     speedTierIntPtr(0),
					InputCharsLTE:     speedTierIntPtr(4096),
					RequireEndpointIn: []string{providerEndpointChatCompletions, providerEndpointResponses, providerEndpointMessages},
				},
				NeverWhen: SpeedTierNeverWhenConfig{
					ThinkingEnabled:   true,
					ReasoningEffortIn: []string{"medium", "high"},
					HasHeader:         "X-Vekil-No-Downgrade",
				},
			},
		},
		{
			PublicID:   "haiku-public",
			Deployment: "haiku-upstream",
			Endpoints:  []string{providerEndpointChatCompletions, providerEndpointResponses, providerEndpointMessages},
		},
		{
			PublicID:   "other-public",
			Deployment: "other-upstream",
			Endpoints:  []string{providerEndpointChatCompletions, providerEndpointResponses, providerEndpointMessages},
		},
	}
}

func TestEvaluateSpeedTierAllAnyAndDenylist(t *testing.T) {
	owner := providerModel{publicID: "sonnet-public", speedTier: &speedTierRule{
		downgradeTo: "haiku-public",
		semantics:   speedTierSemanticsAll,
		when: SpeedTierWhenConfig{
			MaxTokensLTE:  speedTierIntPtr(512),
			ToolsCountLTE: speedTierIntPtr(0),
		},
		neverWhen: SpeedTierNeverWhenConfig{ReasoningEffortIn: []string{"high"}},
	}}

	body := []byte(`{"model":"sonnet-public","max_tokens":256,"tools":[],"messages":[{"role":"user","content":"hi"}]}`)
	decision := evaluateSpeedTier(body, providerEndpointChatCompletions, nil, owner, false)
	if decision.decision != speedTierDecisionDowngraded || decision.triggeringSignal != speedTierSemanticsAll {
		t.Fatalf("all decision = %+v, want downgraded/all", decision)
	}

	body = []byte(`{"model":"sonnet-public","max_tokens":256,"tools":[{"type":"function"}]}`)
	decision = evaluateSpeedTier(body, providerEndpointChatCompletions, nil, owner, false)
	if decision.decision != speedTierDecisionConsideredReject {
		t.Fatalf("all partial decision = %+v, want considered_rejected", decision)
	}

	owner.speedTier.semantics = speedTierSemanticsAny
	decision = evaluateSpeedTier(body, providerEndpointChatCompletions, nil, owner, false)
	if decision.decision != speedTierDecisionDowngraded || decision.triggeringSignal != "max_tokens_lte" {
		t.Fatalf("any decision = %+v, want max_tokens_lte downgrade", decision)
	}

	body = []byte(`{"model":"sonnet-public","max_tokens":256,"tools":[],"reasoning_effort":"high"}`)
	decision = evaluateSpeedTier(body, providerEndpointChatCompletions, nil, owner, false)
	if decision.decision != speedTierDecisionOptedOut || decision.reason != "reasoning_effort" {
		t.Fatalf("denylist decision = %+v, want opted_out reasoning_effort", decision)
	}
}

func TestResolveProviderRequestSpeedTierDowngradesAndLogs(t *testing.T) {
	handler, logs := newSpeedTierRoutingHandler(t, true, speedTierModels())
	_, owner, rewrittenBody, err := handler.resolveProviderRequest([]byte(`{"model":"sonnet-public","max_tokens":256,"tools":[],"messages":[{"role":"user","content":"hi"}]}`), providerEndpointChatCompletions)
	if err != nil {
		t.Fatalf("resolveProviderRequest() error = %v", err)
	}
	if owner.publicID != "haiku-public" {
		t.Fatalf("owner.publicID = %q, want haiku-public", owner.publicID)
	}
	if got := extractRequestModel(rewrittenBody); got != "haiku-upstream" {
		t.Fatalf("rewritten model = %q, want haiku-upstream", got)
	}
	entry := decodeSingleSpeedTierLog(t, logs.String())
	if entry["msg"] != "speed tier routing decision" || entry["decision"] != speedTierDecisionDowngraded || entry["routed_to"] != "haiku-public" {
		t.Fatalf("log entry = %#v, want downgraded routed_to haiku-public", entry)
	}
	if _, ok := entry["equivalent_to"]; ok {
		t.Fatalf("log entry must not claim equivalence: %#v", entry)
	}
}

func TestResolveProviderRequestSpeedTierFailOpenWhenDisabled(t *testing.T) {
	handler, _ := newSpeedTierRoutingHandler(t, false, speedTierModels())
	_, owner, rewrittenBody, err := handler.resolveProviderRequest([]byte(`{"model":"sonnet-public","max_tokens":256,"tools":[]}`), providerEndpointChatCompletions)
	if err != nil {
		t.Fatalf("resolveProviderRequest() error = %v", err)
	}
	if owner.publicID != "sonnet-public" {
		t.Fatalf("owner.publicID = %q, want sonnet-public", owner.publicID)
	}
	if got := extractRequestModel(rewrittenBody); got != "sonnet-upstream" {
		t.Fatalf("rewritten model = %q, want sonnet-upstream", got)
	}
}

func TestResolveProviderRequestSpeedTierHeaderOptOutAndAlias(t *testing.T) {
	handler, _ := newSpeedTierRoutingHandler(t, true, speedTierModels())
	headers := http.Header{"X-Vekil-Routing": []string{"no-downgrade"}}
	_, owner, rewrittenBody, err := handler.resolveProviderRequest([]byte(`{"model":"sonnet-public","max_tokens":256,"tools":[]}`), providerEndpointChatCompletions, headers)
	if err != nil {
		t.Fatalf("resolveProviderRequest(optout) error = %v", err)
	}
	if owner.publicID != "sonnet-public" || extractRequestModel(rewrittenBody) != "sonnet-upstream" {
		t.Fatalf("optout owner/model = %q/%q, want sonnet-public/sonnet-upstream", owner.publicID, extractRequestModel(rewrittenBody))
	}

	_, owner, rewrittenBody, err = handler.resolveProviderRequest([]byte(`{"model":"fast/sonnet-public","max_tokens":4096,"tools":[{"type":"function"}],"reasoning_effort":"low"}`), providerEndpointChatCompletions)
	if err != nil {
		t.Fatalf("resolveProviderRequest(alias) error = %v", err)
	}
	if owner.publicID != "haiku-public" || extractRequestModel(rewrittenBody) != "haiku-upstream" {
		t.Fatalf("alias owner/model = %q/%q, want haiku-public/haiku-upstream", owner.publicID, extractRequestModel(rewrittenBody))
	}
}

func TestBuildProvidersIgnoresInvalidSpeedTierWhenDisabled(t *testing.T) {
	handler := &ProxyHandler{copilotURL: "https://copilot.example.com"}
	_, err := handler.buildConfiguredProviderSetup(t.Context(), ProvidersConfig{Providers: []ProviderConfig{{
		ID:       "p",
		Type:     "openai-compatible",
		Default:  true,
		BaseURL:  "https://upstream.example.com",
		AuthType: "none",
		Models: []ProviderModelConfig{{
			PublicID: "source",
			SpeedTier: &SpeedTierConfig{
				DowngradeTo: "missing-while-disabled",
				Semantics:   "invalid-while-disabled",
			},
		}},
	}}})
	if err != nil {
		t.Fatalf("buildConfiguredProviderSetup() with disabled speed tier error = %v, want nil", err)
	}
}

func TestBuildProvidersRejectsInvalidSpeedTierConfig(t *testing.T) {
	base := func(models []ProviderModelConfig) ProvidersConfig {
		return ProvidersConfig{SpeedTierEnabled: true, Providers: []ProviderConfig{{
			ID:       "p",
			Type:     "openai-compatible",
			Default:  true,
			BaseURL:  "https://upstream.example.com",
			AuthType: "none",
			Models:   models,
		}}}
	}
	tests := []struct {
		name string
		cfg  ProvidersConfig
		want string
	}{
		{
			name: "unknown target",
			cfg:  base([]ProviderModelConfig{{PublicID: "source", SpeedTier: &SpeedTierConfig{DowngradeTo: "missing"}}}),
			want: "not a known public_id",
		},
		{
			name: "chained target",
			cfg: base([]ProviderModelConfig{
				{PublicID: "source", SpeedTier: &SpeedTierConfig{DowngradeTo: "mid"}},
				{PublicID: "mid", SpeedTier: &SpeedTierConfig{DowngradeTo: "leaf"}},
				{PublicID: "leaf"},
			}),
			want: "chained downgrades",
		},
		{
			name: "endpoint mismatch",
			cfg: base([]ProviderModelConfig{
				{PublicID: "source", Endpoints: []string{providerEndpointResponses}, SpeedTier: &SpeedTierConfig{DowngradeTo: "target"}},
				{PublicID: "target", Endpoints: []string{providerEndpointChatCompletions}},
			}),
			want: "does not support endpoint",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler := &ProxyHandler{copilotURL: "https://copilot.example.com"}
			_, err := handler.buildConfiguredProviderSetup(t.Context(), tt.cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("buildConfiguredProviderSetup() error = %v, want containing %q", err, tt.want)
			}
		})
	}
}

func decodeSingleSpeedTierLog(t *testing.T, logs string) map[string]interface{} {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		if !strings.Contains(line, "speed tier routing decision") {
			continue
		}
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("unmarshal log entry %q: %v", line, err)
		}
		return entry
	}
	t.Fatalf("no speed tier decision log in %q", logs)
	return nil
}

func TestHandleOpenAIChatCompletionsSpeedTierDowngradesFullFlow(t *testing.T) {
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != providerEndpointChatCompletions {
			t.Fatalf("upstream path = %q, want %q", got, providerEndpointChatCompletions)
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		upstreamModel = speedTierRawJSONString(payload["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	handler := newSpeedTierHTTPHandler(t, upstream.URL, true)
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"sonnet-public","max_tokens":256,"tools":[],"messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	handler.HandleOpenAIChatCompletions(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if upstreamModel != "haiku-upstream" {
		t.Fatalf("upstream model = %q, want haiku-upstream", upstreamModel)
	}
}

func TestResponsesWebSocketSpeedTierEvaluatesPerTurnAndPinsAfterUpgrade(t *testing.T) {
	var upstreamModels []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		upstreamModels = append(upstreamModels, speedTierRawJSONString(payload["model"]))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed"}`))
	}))
	defer upstream.Close()

	handler := newSpeedTierHTTPHandler(t, upstream.URL, true)
	session := &responsesWebSocketSession{
		baseHeaders:  http.Header{},
		toolContexts: NewToolExecutionContextStore(),
		toolScope:    "test-ws",
	}

	first := parseSpeedTierWSRequest(t, `{"type":"response.create","model":"sonnet-public","max_tokens":256,"tools":[],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	resp, err := session.postCreateRequestSegments(handler, t.Context(), first, [][]json.RawMessage{first.Input}, false)
	if err != nil {
		t.Fatalf("first postCreateRequestSegments() error = %v", err)
	}
	_ = resp.Body.Close()

	second := parseSpeedTierWSRequest(t, `{"type":"response.create","model":"sonnet-public","max_tokens":2048,"tools":[{"type":"function","name":"search"}],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"use tool"}]}]}`)
	resp, err = session.postCreateRequestSegments(handler, t.Context(), second, [][]json.RawMessage{second.Input}, false)
	if err != nil {
		t.Fatalf("second postCreateRequestSegments() error = %v", err)
	}
	_ = resp.Body.Close()

	third := parseSpeedTierWSRequest(t, `{"type":"response.create","model":"sonnet-public","max_tokens":256,"tools":[],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi again"}]}]}`)
	resp, err = session.postCreateRequestSegments(handler, t.Context(), third, [][]json.RawMessage{third.Input}, false)
	if err != nil {
		t.Fatalf("third postCreateRequestSegments() error = %v", err)
	}
	_ = resp.Body.Close()

	resetOther := parseSpeedTierWSRequest(t, `{"type":"response.create","model":"other-public","max_tokens":256,"tools":[],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"new chain"}]}]}`)
	resp, err = session.postCreateRequestSegments(handler, t.Context(), resetOther, [][]json.RawMessage{resetOther.Input}, false)
	if err != nil {
		t.Fatalf("resetOther postCreateRequestSegments() error = %v", err)
	}
	_ = resp.Body.Close()

	want := []string{"haiku-upstream", "sonnet-upstream", "sonnet-upstream", "other-upstream"}
	if len(upstreamModels) != len(want) {
		t.Fatalf("upstream models = %v, want %v", upstreamModels, want)
	}
	for i := range want {
		if upstreamModels[i] != want[i] {
			t.Fatalf("upstream models = %v, want %v", upstreamModels, want)
		}
	}
}

func newSpeedTierHTTPHandler(t *testing.T, upstreamURL string, enabled bool) *ProxyHandler {
	t.Helper()
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			SpeedTierEnabled: enabled,
			Providers: []ProviderConfig{{
				ID:                  "test-provider",
				Type:                "openai-compatible",
				Default:             true,
				BaseURL:             upstreamURL,
				AuthType:            "none",
				ChatCompletionsPath: providerEndpointChatCompletions,
				ResponsesPath:       providerEndpointResponses,
				Models:              speedTierModels(),
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	return handler
}

func newSpeedTierAnthropicHTTPHandler(t *testing.T, upstreamURL string, enabled bool) *ProxyHandler {
	t.Helper()
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			SpeedTierEnabled: enabled,
			Providers: []ProviderConfig{{
				ID:           "test-provider",
				Type:         "anthropic-compatible",
				Default:      true,
				BaseURL:      upstreamURL,
				AuthType:     "none",
				MessagesPath: providerEndpointMessages,
				Models:       speedTierModels(),
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	return handler
}

func parseSpeedTierWSRequest(t *testing.T, body string) *responsesWebSocketCreateRequest {
	t.Helper()
	req, err := parseResponsesWebSocketCreateRequest([]byte(body))
	if err != nil {
		t.Fatalf("parseResponsesWebSocketCreateRequest() error = %v", err)
	}
	return req
}

func TestSpeedTierChatRetryPreservesOptOutHeaders(t *testing.T) {
	var retryModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		retryModel = speedTierRawJSONString(payload["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-retry","object":"chat.completion","choices":[]}`))
	}))
	defer upstream.Close()

	handler := newSpeedTierHTTPHandler(t, upstream.URL, true)
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad stream_options"}}`)),
	}
	body := []byte(`{"model":"sonnet-public","max_tokens":256,"tools":[],"messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true}}`)
	headers := http.Header{"X-Vekil-Routing": []string{"no-downgrade"}}
	retryResp, _, _ := handler.retryChatCompletionsWithoutInjectedStreamOptions(t.Context(), resp, body, chatCompletionsMode{injectedStreamUsage: true}, headers)
	if retryResp == nil {
		t.Fatal("retry response is nil")
	}
	_ = retryResp.Body.Close()
	if retryModel != "sonnet-upstream" {
		t.Fatalf("retry upstream model = %q, want sonnet-upstream", retryModel)
	}
}

func TestSpeedTierChatRetryPreservesFirstSelectedModel(t *testing.T) {
	var retryModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		retryModel = speedTierRawJSONString(payload["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-retry","object":"chat.completion","choices":[]}`))
	}))
	defer upstream.Close()

	handler := newSpeedTierHTTPHandler(t, upstream.URL, true)
	resp := &http.Response{
		StatusCode: http.StatusBadRequest,
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(strings.NewReader(`{"error":{"message":"bad stream_options"}}`)),
	}
	body := []byte(`{"model":"sonnet-public","max_tokens":256,"tools":[],"messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true}}`)
	selectedOwner := providerModel{publicID: "sonnet-public"}
	retryResp, _, _ := handler.retryChatCompletionsWithoutInjectedStreamOptions(t.Context(), resp, body, chatCompletionsMode{injectedStreamUsage: true}, nil, selectedOwner)
	if retryResp == nil {
		t.Fatal("retry response is nil")
	}
	_ = retryResp.Body.Close()
	if retryModel != "sonnet-upstream" {
		t.Fatalf("retry upstream model = %q, want first selected model sonnet-upstream", retryModel)
	}
}

func TestGeminiRetryPreservesFirstSelectedModel(t *testing.T) {
	var retryModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		retryModel = speedTierRawJSONString(payload["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-retry","object":"chat.completion","choices":[]}`))
	}))
	defer upstream.Close()

	handler := newSpeedTierHTTPHandler(t, upstream.URL, true)
	resp := &http.Response{StatusCode: http.StatusBadRequest, Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"bad stream_options"}}`))}
	body := []byte(`{"model":"sonnet-public","max_tokens":256,"tools":[],"messages":[{"role":"user","content":"hi"}],"stream_options":{"include_usage":true}}`)
	selectedOwner := providerModel{publicID: "sonnet-public"}
	retryResp, _, _ := handler.retryChatCompletionsWithoutInjectedStreamOptions(t.Context(), resp, body, chatCompletionsMode{injectedStreamUsage: true}, nil, selectedOwner)
	if retryResp == nil {
		t.Fatal("retry response is nil")
	}
	_ = retryResp.Body.Close()
	if retryModel != "sonnet-upstream" {
		t.Fatalf("Gemini retry upstream model = %q, want first selected model sonnet-upstream", retryModel)
	}
}

func TestAnthropicTranslatedSpeedTierHonorsOptOutHeaders(t *testing.T) {
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		upstreamModel = speedTierRawJSONString(payload["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-anthropic","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	handler := newSpeedTierHTTPHandler(t, upstream.URL, true)
	var anthropicReq models.AnthropicRequest
	body := []byte(`{"model":"sonnet-public","max_tokens":256,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		t.Fatalf("unmarshal anthropic request: %v", err)
	}
	oaiBody, _, err := prepareAnthropicChatCompletionsRequest(&anthropicReq)
	if err != nil {
		t.Fatalf("prepareAnthropicChatCompletionsRequest() error = %v", err)
	}
	headers := http.Header{"X-Vekil-Routing": []string{"no-downgrade"}}
	resp, err := handler.postChatCompletionsWithHeaders(t.Context(), oaiBody, headers)
	if err != nil {
		t.Fatalf("postChatCompletionsWithHeaders() error = %v", err)
	}
	_ = resp.Body.Close()
	if upstreamModel != "sonnet-upstream" {
		t.Fatalf("translated upstream model = %q, want sonnet-upstream", upstreamModel)
	}
}

func TestAnthropicCountTokensDoesNotApplySpeedTier(t *testing.T) {
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/messages/count_tokens" {
			t.Fatalf("upstream path = %q, want /v1/messages/count_tokens", got)
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		upstreamModel = speedTierRawJSONString(payload["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"input_tokens":7}`))
	}))
	defer upstream.Close()

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			SpeedTierEnabled: true,
			Providers: []ProviderConfig{{
				ID:           "anthropic",
				Type:         "anthropic-compatible",
				Default:      true,
				BaseURL:      upstream.URL,
				AuthType:     "none",
				MessagesPath: "/v1/messages",
				Models: []ProviderModelConfig{
					{PublicID: "sonnet-public", Deployment: "sonnet-upstream", Endpoints: []string{providerEndpointMessages}, SpeedTier: &SpeedTierConfig{DowngradeTo: "haiku-public", When: SpeedTierWhenConfig{MaxTokensLTE: speedTierIntPtr(512)}}},
					{PublicID: "haiku-public", Deployment: "haiku-upstream", Endpoints: []string{providerEndpointMessages}},
				},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}

	resp, err := handler.postAnthropicMessagesCountTokens(t.Context(), []byte(`{"model":"sonnet-public","max_tokens":256,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`), nil)
	if err != nil {
		t.Fatalf("postAnthropicMessagesCountTokens() error = %v", err)
	}
	_ = resp.Body.Close()
	if upstreamModel != "sonnet-upstream" {
		t.Fatalf("count_tokens upstream model = %q, want sonnet-upstream", upstreamModel)
	}
}

func TestSpeedTierSystemCharsCountsDeveloperAndResponsesInputMessages(t *testing.T) {
	features := collectSpeedTierFeatures([]byte(`{"model":"m","messages":[{"role":"developer","content":"dev rules"}],"input":[{"type":"message","role":"system","content":[{"type":"input_text","text":"system rules"}]}]}`))
	if features.systemChars != len("dev rules")+len("system rules") {
		t.Fatalf("systemChars = %d, want %d", features.systemChars, len("dev rules")+len("system rules"))
	}
}

func TestResponsesWebSocketSpeedTierHonorsUpgradeOptOutHeader(t *testing.T) {
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		upstreamModel = speedTierRawJSONString(payload["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed"}`))
	}))
	defer upstream.Close()

	handler := newSpeedTierHTTPHandler(t, upstream.URL, true)
	session := &responsesWebSocketSession{
		routingHeaders: http.Header{"X-Vekil-Routing": []string{"no-downgrade"}},
		baseHeaders:    http.Header{},
		toolContexts:   NewToolExecutionContextStore(),
		toolScope:      "test-ws",
	}
	req := parseSpeedTierWSRequest(t, `{"type":"response.create","model":"sonnet-public","max_tokens":256,"tools":[],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	resp, err := session.postCreateRequestSegments(handler, t.Context(), req, [][]json.RawMessage{req.Input}, false)
	if err != nil {
		t.Fatalf("postCreateRequestSegments() error = %v", err)
	}
	_ = resp.Body.Close()
	if upstreamModel != "sonnet-upstream" {
		t.Fatalf("upstream model = %q, want sonnet-upstream", upstreamModel)
	}
}

func TestSpeedTierEndpointFilterDoesNotTriggerAnyModeByItself(t *testing.T) {
	owner := providerModel{publicID: "sonnet-public", speedTier: &speedTierRule{
		downgradeTo: "haiku-public",
		semantics:   speedTierSemanticsAny,
		when: SpeedTierWhenConfig{
			MaxTokensLTE:      speedTierIntPtr(10),
			RequireEndpointIn: []string{providerEndpointResponses},
		},
	}}
	decision := evaluateSpeedTier([]byte(`{"model":"sonnet-public","max_tokens":2048}`), providerEndpointResponses, nil, owner, false)
	if decision.decision != speedTierDecisionConsideredReject {
		t.Fatalf("decision = %+v, want endpoint filter not to trigger any-mode downgrade", decision)
	}
}

func TestResolveProviderRequestPreservesLiteralFastPublicID(t *testing.T) {
	handler, _ := newSpeedTierRoutingHandler(t, true, []ProviderModelConfig{
		{PublicID: "fast/sonnet-public", Deployment: "literal-fast-upstream", Endpoints: []string{providerEndpointChatCompletions}},
		{PublicID: "sonnet-public", Deployment: "sonnet-upstream", Endpoints: []string{providerEndpointChatCompletions}, SpeedTier: &SpeedTierConfig{DowngradeTo: "haiku-public", When: SpeedTierWhenConfig{MaxTokensLTE: speedTierIntPtr(512)}}},
		{PublicID: "haiku-public", Deployment: "haiku-upstream", Endpoints: []string{providerEndpointChatCompletions}},
	})
	_, owner, rewrittenBody, err := handler.resolveProviderRequest([]byte(`{"model":"fast/sonnet-public","max_tokens":256}`), providerEndpointChatCompletions)
	if err != nil {
		t.Fatalf("resolveProviderRequest() error = %v", err)
	}
	if owner.publicID != "fast/sonnet-public" || extractRequestModel(rewrittenBody) != "literal-fast-upstream" {
		t.Fatalf("owner/model = %q/%q, want literal fast model", owner.publicID, extractRequestModel(rewrittenBody))
	}
}

func TestCompactInflightKeyIncludesOnlySpeedTierRoutingHeaders(t *testing.T) {
	fields := map[string]json.RawMessage{"model": json.RawMessage(`"sonnet-public"`), "input": json.RawMessage(`[]`)}
	keyDefault, okDefault := compactInflightKey(fields, nil, http.Header{"X-Vekil-Routing": []string{"default"}, "Authorization": []string{"Bearer a"}, "User-Agent": []string{"one"}})
	keySpeed, okSpeed := compactInflightKey(fields, nil, http.Header{"X-Vekil-Routing": []string{"speed"}, "Authorization": []string{"Bearer a"}, "User-Agent": []string{"one"}})
	keyNoisy, okNoisy := compactInflightKey(fields, nil, http.Header{"X-Vekil-Routing": []string{"default"}, "Authorization": []string{"Bearer b"}, "User-Agent": []string{"two"}, "Cookie": []string{"session=abc"}})
	if !okDefault || !okSpeed || !okNoisy {
		t.Fatalf("compactInflightKey ok = %v/%v/%v, want all true", okDefault, okSpeed, okNoisy)
	}
	if keyDefault == keySpeed {
		t.Fatalf("compact inflight key ignored speed-tier routing header: %q", keyDefault)
	}
	if keyDefault != keyNoisy {
		t.Fatalf("compact inflight key included unrelated volatile headers: default=%q noisy=%q", keyDefault, keyNoisy)
	}
}

func TestShouldForwardAnthropicMessagesDirectHonorsFastAlias(t *testing.T) {
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:           "anthropic",
			Type:         "anthropic-compatible",
			Default:      true,
			BaseURL:      "https://anthropic.example.com",
			AuthType:     "none",
			MessagesPath: "/v1/messages",
			Models: []ProviderModelConfig{
				{PublicID: "claude-sonnet", Endpoints: []string{providerEndpointMessages}, SpeedTier: &SpeedTierConfig{DowngradeTo: "claude-haiku", When: SpeedTierWhenConfig{MaxTokensLTE: speedTierIntPtr(512)}}},
				{PublicID: "claude-haiku", Endpoints: []string{providerEndpointMessages}},
			},
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	if !handler.shouldForwardAnthropicMessagesDirect("fast/claude-sonnet") {
		t.Fatal("fast/ alias should preserve direct Anthropic routing for the base model")
	}
}

func TestResponsesWebSocketSpeedTierHonorsCustomUpgradeDenyHeader(t *testing.T) {
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		upstreamModel = speedTierRawJSONString(payload["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed"}`))
	}))
	defer upstream.Close()

	models := speedTierModels()
	models[0].SpeedTier.NeverWhen.HasHeader = "X-Do-Not-Downgrade"
	handler, _ := newSpeedTierRoutingHandler(t, true, models)
	handler.providersState.providers["test-provider"].baseURL = upstream.URL
	handler.providersState.providers["test-provider"].paths.responses = providerEndpointResponses
	session := &responsesWebSocketSession{
		routingHeaders: http.Header{"X-Do-Not-Downgrade": []string{"1"}},
		baseHeaders:    http.Header{},
		toolContexts:   NewToolExecutionContextStore(),
		toolScope:      "test-ws",
	}
	req := parseSpeedTierWSRequest(t, `{"type":"response.create","model":"sonnet-public","max_tokens":256,"tools":[],"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]}`)
	resp, err := session.postCreateRequestSegments(handler, t.Context(), req, [][]json.RawMessage{req.Input}, false)
	if err != nil {
		t.Fatalf("postCreateRequestSegments() error = %v", err)
	}
	_ = resp.Body.Close()
	if upstreamModel != "sonnet-upstream" {
		t.Fatalf("upstream model = %q, want sonnet-upstream", upstreamModel)
	}
}

func TestAnthropicTranslatedThinkingForcesNoDowngrade(t *testing.T) {
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		upstreamModel = speedTierRawJSONString(payload["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-anthropic","object":"chat.completion","choices":[]}`))
	}))
	defer upstream.Close()

	handler := newSpeedTierHTTPHandler(t, upstream.URL, true)
	var anthropicReq models.AnthropicRequest
	body := []byte(`{"model":"sonnet-public","max_tokens":256,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`)
	if err := json.Unmarshal(body, &anthropicReq); err != nil {
		t.Fatalf("unmarshal anthropic request: %v", err)
	}
	oaiBody, _, err := prepareAnthropicChatCompletionsRequest(&anthropicReq)
	if err != nil {
		t.Fatalf("prepareAnthropicChatCompletionsRequest() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	resp, err := handler.postChatCompletionsWithHeaders(t.Context(), oaiBody, anthropicRoutingHeaders(req, &anthropicReq))
	if err != nil {
		t.Fatalf("postChatCompletionsWithHeaders() error = %v", err)
	}
	_ = resp.Body.Close()
	if upstreamModel != "sonnet-upstream" {
		t.Fatalf("thinking upstream model = %q, want sonnet-upstream", upstreamModel)
	}
}

func TestDirectAnthropicThinkingForcesNoDowngrade(t *testing.T) {
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Path; got != "/v1/messages" {
			t.Fatalf("upstream path = %q, want /v1/messages", got)
		}
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		upstreamModel = speedTierRawJSONString(payload["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg-1","type":"message","role":"assistant","model":"sonnet-upstream","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	handler := newSpeedTierAnthropicHTTPHandler(t, upstream.URL, true)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"sonnet-public","max_tokens":256,"thinking":{"type":"enabled","budget_tokens":1024},"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if upstreamModel != "sonnet-upstream" {
		t.Fatalf("direct thinking upstream model = %q, want sonnet-upstream", upstreamModel)
	}
}

func TestDirectAnthropicSpeedTierPreservesRequestedResponseModel(t *testing.T) {
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		upstreamModel = speedTierRawJSONString(payload["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg-1","type":"message","role":"assistant","model":"haiku-upstream","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	handler := newSpeedTierAnthropicHTTPHandler(t, upstream.URL, true)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"sonnet-public","max_tokens":256,"tools":[],"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	handler.HandleAnthropicMessages(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if upstreamModel != "haiku-upstream" {
		t.Fatalf("direct speed-tier upstream model = %q, want haiku-upstream", upstreamModel)
	}
	var resp models.AnthropicResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if resp.Model != "sonnet-public" {
		t.Fatalf("response model = %q, want requested public model sonnet-public", resp.Model)
	}
}

func TestResponsesWebSocketPinnedModelForcesDefaultRoutingHeaders(t *testing.T) {
	session := &responsesWebSocketSession{
		routingHeaders:        http.Header{"X-Vekil-Routing": []string{"speed"}},
		baseHeaders:           http.Header{},
		speedTierPinnedModel:  "sonnet-public",
		speedTierPinnedSource: "sonnet-public",
	}
	req := parseSpeedTierWSRequest(t, `{"type":"response.create","model":"sonnet-public","client_metadata":{"ws_request_header_X-Vekil-Routing":"speed"},"input":[]}`)
	headers := session.speedTierRoutingHeadersForRequest(req, false)
	if got := headers.Get("X-Vekil-Routing"); got != "default" {
		t.Fatalf("X-Vekil-Routing = %q, want default", got)
	}
}

func TestResponsesWebSocketOptOutDoesNotCreateStickyPin(t *testing.T) {
	var upstreamModels []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		upstreamModels = append(upstreamModels, speedTierRawJSONString(payload["model"]))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp-1","object":"response","status":"completed"}`))
	}))
	defer upstream.Close()

	handler := newSpeedTierHTTPHandler(t, upstream.URL, true)
	session := &responsesWebSocketSession{baseHeaders: http.Header{}, toolContexts: NewToolExecutionContextStore(), toolScope: "test-ws"}

	first := parseSpeedTierWSRequest(t, `{"type":"response.create","model":"sonnet-public","max_tokens":256,"tools":[],"input":[]}`)
	resp, err := session.postCreateRequestSegments(handler, t.Context(), first, [][]json.RawMessage{first.Input}, false)
	if err != nil {
		t.Fatalf("first postCreateRequestSegments() error = %v", err)
	}
	_ = resp.Body.Close()

	optOut := parseSpeedTierWSRequest(t, `{"type":"response.create","model":"sonnet-public","client_metadata":{"ws_request_header_X-Vekil-Routing":"no-downgrade"},"max_tokens":256,"tools":[],"input":[]}`)
	resp, err = session.postCreateRequestSegments(handler, t.Context(), optOut, [][]json.RawMessage{optOut.Input}, false)
	if err != nil {
		t.Fatalf("optOut postCreateRequestSegments() error = %v", err)
	}
	_ = resp.Body.Close()

	third := parseSpeedTierWSRequest(t, `{"type":"response.create","model":"sonnet-public","max_tokens":256,"tools":[],"input":[]}`)
	resp, err = session.postCreateRequestSegments(handler, t.Context(), third, [][]json.RawMessage{third.Input}, false)
	if err != nil {
		t.Fatalf("third postCreateRequestSegments() error = %v", err)
	}
	_ = resp.Body.Close()

	want := []string{"haiku-upstream", "sonnet-upstream", "haiku-upstream"}
	if len(upstreamModels) != len(want) {
		t.Fatalf("upstream models = %v, want %v", upstreamModels, want)
	}
	for i := range want {
		if upstreamModels[i] != want[i] {
			t.Fatalf("upstream models = %v, want %v", upstreamModels, want)
		}
	}
	if session.speedTierPinnedModel != "" {
		t.Fatalf("speedTierPinnedModel = %q, want empty after opt-out rejection", session.speedTierPinnedModel)
	}
}

func TestSpeedTierSystemCharsMissingDoesNotMatch(t *testing.T) {
	owner := providerModel{publicID: "sonnet-public", speedTier: &speedTierRule{
		downgradeTo: "haiku-public",
		semantics:   speedTierSemanticsAny,
		when:        SpeedTierWhenConfig{SystemCharsLTE: speedTierIntPtr(2048)},
	}}
	decision := evaluateSpeedTier([]byte(`{"model":"sonnet-public","messages":[{"role":"user","content":"hi"}]}`), providerEndpointChatCompletions, nil, owner, false)
	if decision.decision != speedTierDecisionConsideredReject {
		t.Fatalf("decision = %+v, want missing system field not to match", decision)
	}
}

func TestFastAliasUnknownDynamicModelFailsOpenToBaseModel(t *testing.T) {
	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		WithDeferredDynamicProviderModelValidation(true),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:             "dynamic",
			Type:           "openai-compatible",
			Default:        true,
			BaseURL:        "https://upstream.example.com",
			AuthType:       "none",
			ModelDiscovery: "openai",
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	_, owner, rewrittenBody, err := handler.resolveProviderRequest([]byte(`{"model":"fast/new-model","messages":[{"role":"user","content":"hi"}]}`), providerEndpointChatCompletions)
	if err != nil {
		t.Fatalf("resolveProviderRequest() error = %v", err)
	}
	if owner.publicID != "new-model" || extractRequestModel(rewrittenBody) != "new-model" {
		t.Fatalf("owner/model = %q/%q, want new-model/new-model", owner.publicID, extractRequestModel(rewrittenBody))
	}
}

func TestSpeedTierMaxTokensSignalIncludesMaxCompletionTokens(t *testing.T) {
	owner := providerModel{publicID: "sonnet-public", speedTier: &speedTierRule{
		downgradeTo: "haiku-public",
		semantics:   speedTierSemanticsAll,
		when:        SpeedTierWhenConfig{MaxTokensLTE: speedTierIntPtr(512)},
	}}
	decision := evaluateSpeedTier([]byte(`{"model":"sonnet-public","max_completion_tokens":128}`), providerEndpointChatCompletions, nil, owner, false)
	if decision.decision != speedTierDecisionDowngraded {
		t.Fatalf("decision = %+v, want max_completion_tokens to satisfy max_tokens_lte", decision)
	}
}

func TestSpeedTierMaxTokensNullDoesNotMatch(t *testing.T) {
	owner := providerModel{publicID: "sonnet-public", speedTier: &speedTierRule{
		downgradeTo: "haiku-public",
		semantics:   speedTierSemanticsAny,
		when:        SpeedTierWhenConfig{MaxTokensLTE: speedTierIntPtr(512)},
	}}
	decision := evaluateSpeedTier([]byte(`{"model":"sonnet-public","max_tokens":null}`), providerEndpointChatCompletions, nil, owner, false)
	if decision.decision != speedTierDecisionConsideredReject {
		t.Fatalf("decision = %+v, want max_tokens:null not to match", decision)
	}
}

func TestCompactInflightKeyIncludesConfiguredDenyHeaders(t *testing.T) {
	fields := map[string]json.RawMessage{"model": json.RawMessage(`"sonnet-public"`), "input": json.RawMessage(`[]`)}
	headersA := http.Header{"X-Do-Not-Downgrade": []string{"1"}, "Authorization": []string{"Bearer a"}}
	headersB := http.Header{"Authorization": []string{"Bearer b"}}
	keyA, okA := compactInflightKeyWithRoutingHeaderNames(fields, nil, []string{"X-Do-Not-Downgrade"}, headersA)
	keyB, okB := compactInflightKeyWithRoutingHeaderNames(fields, nil, []string{"X-Do-Not-Downgrade"}, headersB)
	keyNoisy, okNoisy := compactInflightKeyWithRoutingHeaderNames(fields, nil, []string{"X-Do-Not-Downgrade"}, http.Header{"X-Do-Not-Downgrade": []string{"1"}, "Authorization": []string{"Bearer c"}})
	if !okA || !okB || !okNoisy {
		t.Fatalf("compactInflightKey ok = %v/%v/%v, want true", okA, okB, okNoisy)
	}
	if keyA == keyB {
		t.Fatalf("custom deny header did not affect compact key")
	}
	if keyA != keyNoisy {
		t.Fatalf("unrelated auth header affected compact key")
	}
}

func TestValidateProviderSpeedTierRejectsDisabledTarget(t *testing.T) {
	models := []providerModel{
		{publicID: "sonnet-public", supportedEndpoints: []string{providerEndpointChatCompletions}, speedTier: &speedTierRule{downgradeTo: "haiku-public", semantics: speedTierSemanticsAll}},
		{publicID: "haiku-public", supportedEndpoints: []string{providerEndpointChatCompletions}, disabled: true},
	}
	if err := validateProviderSpeedTierModels("test-provider", models); err == nil || !strings.Contains(err.Error(), "is disabled") {
		t.Fatalf("validateProviderSpeedTierModels() error = %v, want disabled target error", err)
	}
}

func TestGeminiThinkingConfigForcesNoDowngrade(t *testing.T) {
	var upstreamModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatalf("decode upstream body: %v", err)
		}
		upstreamModel = speedTierRawJSONString(payload["model"])
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-gemini","object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer upstream.Close()

	handler := newSpeedTierHTTPHandler(t, upstream.URL, true)
	reqBody := `{"contents":[{"role":"user","parts":[{"text":"hi"}]}],"generationConfig":{"thinkingConfig":{"includeThoughts":true},"maxOutputTokens":256}}`
	req := httptest.NewRequest(http.MethodPost, "/v1beta/models/sonnet-public:generateContent", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	handler.HandleGeminiModels(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	if upstreamModel != "sonnet-upstream" {
		t.Fatalf("Gemini thinking upstream model = %q, want sonnet-upstream", upstreamModel)
	}
}
