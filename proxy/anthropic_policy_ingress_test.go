package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

type policyCloseTrackingReader struct {
	io.Reader
	closed bool
}

func (r *policyCloseTrackingReader) Close() error {
	r.closed = true
	return nil
}

func TestPolicySanitizedOpenAIStreamBoundsOneEvent(t *testing.T) {
	upstream := &policyCloseTrackingReader{Reader: strings.NewReader("data: one\ndata: two\ndata: three\ndata: four\ndata: five\n\n")}
	stream := newPolicySanitizedOpenAIStream(upstream).(*policySanitizedOpenAIStream)
	stream.maxEventBytes = 32
	recorder := httptest.NewRecorder()
	StreamOpenAIToAnthropic(recorder, stream, "coding-economy", "msg-policy-oversize")
	body := recorder.Body.String()
	if !strings.Contains(body, `"message":"upstream request failed"`) {
		t.Fatalf("oversized policy stream was not sanitized: %s", body)
	}
	for _, leaked := range []string{"exceeds maximum buffer", "upstream stream read failed", "data: one"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("oversized policy stream leaked %q: %s", leaked, body)
		}
	}
	if !upstream.closed {
		t.Fatal("oversized policy SSE event did not close upstream body")
	}
}

func TestHandleAnthropicMessagesPolicyIngressPreservesForcedStreamAndPublicContract(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)
	parallelUnsupported := false
	cfg.ModelRoutes[1].ParallelToolCalls = &parallelUnsupported

	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(cfg),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":64,
		"stream":false,
		"messages":[{"role":"user","content":"inspect the repository"}],
		"tools":[{"name":"read_file","description":"Read a file","input_schema":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"]}}],
		"tool_choice":{"type":"none"}
	}`)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type=%q, want non-streaming Anthropic JSON", got)
	}
	if got := recorder.Header().Get("X-Vekil-Request-ID"); got == "" {
		t.Fatal("missing X-Vekil-Request-ID")
	}
	var response models.AnthropicResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Anthropic response: %v; body=%s", err, recorder.Body.String())
	}
	if response.Model != "coding-economy" {
		t.Fatalf("response model=%q, want policy public id", response.Model)
	}
	if len(response.Content) != 1 || response.Content[0].Text == nil || *response.Content[0].Text != "ok" {
		t.Fatalf("response content=%+v, want text ok", response.Content)
	}

	lightRequests, lightModels := light.snapshot()
	if lightRequests != 1 || strings.Join(lightModels, ",") != "light-model" {
		t.Fatalf("light requests=%d models=%v, want one terminal request", lightRequests, lightModels)
	}
	if powerfulRequests, _ := powerful.snapshot(); powerfulRequests != 0 {
		t.Fatalf("powerful requests=%d, want zero", powerfulRequests)
	}
	parallel := light.parallelToolCallsSnapshot()
	if len(parallel) != 1 || string(parallel[0]) != "false" {
		t.Fatalf("parallel_tool_calls=%q, want policy contract false", parallel)
	}
}

func TestHandleAnthropicMessagesPolicyIngressDoesNotExposePublicTerminalID(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)
	cfg.ModelRoutes[0].Exposure = modelRouteExposurePublic
	cfg.ModelRoutes[0].PublicID = "light-public"
	cfg.ModelRoutes[0].Name = "Public Light Terminal"

	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(cfg),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":64,
		"messages":[{"role":"user","content":"hello"}]
	}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response models.AnthropicResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Model != "coding-economy" || strings.Contains(recorder.Body.String(), "light-public") {
		t.Fatalf("policy response exposed terminal identity: model=%q body=%s", response.Model, recorder.Body.String())
	}
}

func TestHandleAnthropicMessagesCountTokensPolicyIngressUsesPlannedProbe(t *testing.T) {
	var probe models.OpenAIRequest
	lightRequests := 0
	light := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		lightRequests++
		if err := json.NewDecoder(r.Body).Decode(&probe); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"count-probe","object":"chat.completion","model":"light-model","choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`))
	}))
	defer light.Close()
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	cfg := policyIntegrationConfig(light.URL, powerful.server.URL, policyConfigModeOff)
	parallelUnsupported := false
	cfg.ModelRoutes[0].ParallelToolCalls = &parallelUnsupported
	cfg.ModelRoutes[1].ParallelToolCalls = &parallelUnsupported
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(cfg),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessagesCountTokens(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessagesCount, strings.NewReader(`{
		"model":"coding-economy",
		"messages":[{"role":"user","content":"count this request"}],
		"tools":[{"name":"lookup","description":"Look up data","input_schema":{"type":"object","properties":{}}}],
		"tool_choice":{"type":"auto","disable_parallel_tool_use":true}
	}`)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Vekil-Request-ID"); got == "" {
		t.Fatal("missing X-Vekil-Request-ID")
	}
	var response models.AnthropicCountTokensResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode count_tokens response: %v", err)
	}
	if response.InputTokens != 2 {
		t.Fatalf("input_tokens=%d, want terminal prompt usage", response.InputTokens)
	}
	if lightRequests != 1 || probe.Model != "light-model" {
		t.Fatalf("light requests/model=%d/%q, want one light-model probe", lightRequests, probe.Model)
	}
	if probe.Stream == nil || *probe.Stream || probe.StreamOptions != nil {
		t.Fatalf("probe stream/stream_options=%v/%+v, want false/nil", probe.Stream, probe.StreamOptions)
	}
	if probe.Temperature == nil || *probe.Temperature != 0 || probe.MaxCompletionTokens == nil || *probe.MaxCompletionTokens != responsesChatMinimumOutputTokens || probe.MaxTokens != nil {
		t.Fatalf("probe temperature/max_completion/max=%v/%v/%v, want 0/%d/nil", probe.Temperature, probe.MaxCompletionTokens, probe.MaxTokens, responsesChatMinimumOutputTokens)
	}
	if probe.ParallelToolCalls != nil {
		t.Fatalf("probe parallel_tool_calls=%v, want omitted for unsupported terminal", probe.ParallelToolCalls)
	}
	if powerfulRequests, _ := powerful.snapshot(); powerfulRequests != 0 {
		t.Fatalf("powerful requests=%d, want zero", powerfulRequests)
	}
	assertNoRouteAttemptStats(t, h)
}

func TestHandleAnthropicMessagesPolicyIngressKeepsPrewarmNonStreaming(t *testing.T) {
	var probe models.OpenAIRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		if err := json.NewDecoder(r.Body).Decode(&probe); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"prewarm","object":"chat.completion","model":"light-model","choices":[],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}`)
	}))
	defer upstream.Close()
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":0,
		"messages":[{"role":"user","content":"warm cache"}],
		"tools":[{"name":"lookup","input_schema":{"type":"object"}}]
	}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if probe.Stream != nil || probe.StreamOptions != nil {
		t.Fatalf("prewarm stream/stream_options = %v/%+v, want omitted", probe.Stream, probe.StreamOptions)
	}
}

func TestHandleAnthropicMessagesPolicyIngressAllowsDeterministicReplayContinuation(t *testing.T) {
	const replayID = "call_vekil_AAAAAAAAAAAAAAAAAAAAAA"
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":64,
		"messages":[
			{"role":"user","content":"look up the result"},
			{"role":"assistant","content":[{"type":"tool_use","id":"`+replayID+`","name":"lookup","input":{"query":"status"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"`+replayID+`","content":"ready"}]}
		],
		"tools":[{"name":"lookup","input_schema":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}]
	}`)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response models.AnthropicResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Anthropic response: %v; body=%s", err, recorder.Body.String())
	}
	if response.Model != "coding-economy" {
		t.Fatalf("response model=%q, want policy public id", response.Model)
	}
	lightRequests, lightModels := light.snapshot()
	if lightRequests != 1 || strings.Join(lightModels, ",") != "light-model" {
		t.Fatalf("light requests=%d models=%v, want deterministic baseline continuation", lightRequests, lightModels)
	}
	if powerfulRequests, _ := powerful.snapshot(); powerfulRequests != 0 {
		t.Fatalf("powerful requests=%d, want zero", powerfulRequests)
	}
}

func TestAnthropicSplitMixedAssistantReplayMatchesInnerStore(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()
	route := responsesChatReplayRoute{ProviderID: "bridge", PublicModel: "light-model", UpstreamModel: "gpt-5.6-luna"}
	assistantText := "Baseline diagnosis is reproducible; I will add the regression first."
	arguments := `{"file_path":"/private/tmp/calc_test.go","old_string":"old","new_string":"new"}`
	clientArguments := map[string]any{
		"replace_all": false,
		"file_path":   "/private/tmp/calc_test.go",
		"old_string":  "old",
		"new_string":  "new",
	}
	outputItem, _ := json.Marshal(map[string]any{
		"type": "function_call", "id": "fc-edit", "call_id": "upstream-edit", "name": "Edit", "arguments": arguments,
	})
	published, err := store.Publish(responsesChatReplayPublishRequest{
		Route: route, AssistantContent: json.RawMessage(`"` + assistantText + `"`),
		OutputItems: []json.RawMessage{outputItem},
		Calls: []responsesChatReplayPublishCall{{
			UpstreamCallID: "upstream-edit", Name: "Edit", VisibleArguments: arguments, OutputItemIndex: 0,
			OptionalDefaults: responsesChatReplayOptionalDefaults{"replace_all": json.RawMessage("false")},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	replayID := published.Projection.Calls[0].ID
	var req models.AnthropicRequest
	requestBody, _ := json.Marshal(map[string]any{
		"model": "light-model",
		"tools": []any{map[string]any{
			"name": "Edit",
			"input_schema": map[string]any{
				"type": "object",
				"properties": map[string]any{
					"file_path":  map[string]any{"type": "string"},
					"old_string": map[string]any{"type": "string"},
					"new_string": map[string]any{"type": "string"},
					"replace_all": map[string]any{
						"type": "boolean", "default": false,
					},
				},
				"required": []string{"file_path", "old_string", "new_string"},
			},
		}},
		"messages": []any{
			map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": assistantText}}},
			map[string]any{"role": "assistant", "content": []any{map[string]any{
				"type": "tool_use", "id": replayID, "name": "Edit", "input": clientArguments,
			}}},
			map[string]any{"role": "user", "content": []any{map[string]any{
				"type": "tool_result", "tool_use_id": replayID, "content": "String to replace not found", "is_error": true,
			}}},
		},
	})
	if err := json.Unmarshal(requestBody, &req); err != nil {
		t.Fatal(err)
	}
	chat, err := TranslateAnthropicToOpenAI(&req)
	if err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 || string(chat.Messages[0].Content) != `"`+assistantText+`"` || len(chat.Messages[0].ToolCalls) != 1 {
		t.Fatalf("split Anthropic assistant turn was not re-merged: %+v", chat.Messages)
	}
	chatBody, _ := json.Marshal(chat)
	if _, err := translateChatRequestToResponses(chatBody, responsesChatRequestOptions{
		ReplayStore: store,
		ReplayRoute: route,
	}); err != nil {
		t.Fatalf("split Anthropic mixed turn did not match inner replay store: %v; chat=%s", err, chatBody)
	}
}

func TestHandleAnthropicMessagesPolicyIngressSanitizesTerminalHTTPError(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	light.terminalFailureStatus.Store(http.StatusBadGateway)

	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":64,
		"messages":[{"role":"user","content":"inspect the repository"}]
	}`)))

	assertSanitizedAnthropicPolicyError(t, recorder, http.StatusBadGateway)
}

func TestHandleAnthropicMessagesPolicyIngressSanitizesTerminalStreamError(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	light.terminalStreamFailure.Store(true)

	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":64,
		"stream":true,
		"messages":[{"role":"user","content":"inspect the repository"}]
	}`)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"model":"coding-economy"`) || !strings.Contains(body, `"message":"upstream request failed"`) {
		t.Fatalf("policy stream did not preserve public identity and sanitized error: %s", body)
	}
	for _, leaked := range []string{"power-model", "power-route", "power-provider"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("policy stream leaked %q: %s", leaked, body)
		}
	}
}

func TestHandleAnthropicMessagesCountTokensPolicyIngressSanitizesTerminalError(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	light.terminalFailureStatus.Store(http.StatusBadGateway)

	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessagesCountTokens(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessagesCount, strings.NewReader(`{
		"model":"coding-economy",
		"messages":[{"role":"user","content":"count this request"}]
	}`)))

	assertSanitizedAnthropicPolicyError(t, recorder, http.StatusBadGateway)
	assertNoRouteAttemptStats(t, h)
}

func assertSanitizedAnthropicPolicyError(t *testing.T, recorder *httptest.ResponseRecorder, wantStatus int) {
	t.Helper()
	if recorder.Code != wantStatus {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response models.AnthropicError
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Anthropic error: %v; body=%s", err, recorder.Body.String())
	}
	if response.Error.Message != "upstream request failed" {
		t.Fatalf("error message=%q, want sanitized upstream failure", response.Error.Message)
	}
	for _, leaked := range []string{"terminal unavailable", "light-model", "power-model", "power-route", "power-provider"} {
		if strings.Contains(recorder.Body.String(), leaked) {
			t.Fatalf("policy error leaked %q: %s", leaked, recorder.Body.String())
		}
	}
	for _, header := range []string{"X-Request-ID", "X-Azure-Request-ID", "OpenAI-Processing-Ms", "X-Vekil-Internal-Route", "X-RateLimit-Model", "RateLimit-Policy"} {
		if got := recorder.Header().Get(header); got != "" {
			t.Fatalf("%s=%q, want omitted", header, got)
		}
	}
	for _, header := range []string{"Openai-Model", "X-Openai-Model"} {
		if got := recorder.Header().Get(header); got != "coding-economy" {
			t.Fatalf("%s=%q, want policy public id", header, got)
		}
	}
}

func TestHandleAnthropicMessagesPolicyIngressPreservesClientStream(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":64,
		"stream":true,
		"messages":[{"role":"user","content":"inspect the repository"}]
	}`)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type=%q, want Anthropic SSE", got)
	}
	body := recorder.Body.String()
	for _, want := range []string{`event: message_start`, `"model":"coding-economy"`, `"text":"ok"`, `event: message_stop`} {
		if !strings.Contains(body, want) {
			t.Fatalf("stream missing %q: %s", want, body)
		}
	}
	if strings.Contains(body, `"model":"light-model"`) {
		t.Fatalf("stream leaked terminal model: %s", body)
	}
	lightRequests, lightModels := light.snapshot()
	if lightRequests != 1 || strings.Join(lightModels, ",") != "light-model" {
		t.Fatalf("light requests=%d models=%v, want one streamed terminal request", lightRequests, lightModels)
	}
	if powerfulRequests, _ := powerful.snapshot(); powerfulRequests != 0 {
		t.Fatalf("powerful requests=%d, want zero", powerfulRequests)
	}
}

func TestHandleAnthropicMessagesPolicyIngressSanitizesMalformedStreamError(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Openai-Model", "light-model")
		_, _ = io.WriteString(w, "data: not-json\n\n")
	}))
	defer upstream.Close()
	h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(`{
		"model":"coding-economy",
		"max_tokens":64,
		"messages":[{"role":"user","content":"hello"}]
	}`)))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response models.AnthropicError
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Error.Message != "upstream request failed" || strings.Contains(recorder.Body.String(), "invalid character") || strings.Contains(recorder.Body.String(), "not-json") {
		t.Fatalf("malformed upstream detail leaked: %s", recorder.Body.String())
	}
}

func TestAnthropicPolicyPlannerUnavailableUsesSanitizedServerError(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   string
		body   string
		handle func(*ProxyHandler, http.ResponseWriter, *http.Request)
	}{
		{
			name:   "messages",
			path:   providerEndpointMessages,
			body:   `{"model":"coding-economy","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`,
			handle: (*ProxyHandler).HandleAnthropicMessages,
		},
		{
			name:   "count_tokens",
			path:   providerEndpointMessagesCount,
			body:   `{"model":"coding-economy","messages":[{"role":"user","content":"hello"}]}`,
			handle: (*ProxyHandler).HandleAnthropicMessagesCountTokens,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			h, err := NewProxyHandler(nil, logger.NewWithWriter(logger.LevelError, io.Discard),
				WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
				WithPolicyRoutingMode(PolicyRoutingModeEnforce),
			)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			tc.handle(h, recorder, httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.body)))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			var response models.AnthropicError
			if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Error.Message != "upstream request failed" || strings.Contains(recorder.Body.String(), "preflight") {
				t.Fatalf("planner detail leaked: %s", recorder.Body.String())
			}
		})
	}
}
