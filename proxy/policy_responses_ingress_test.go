package proxy

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func TestPolicyResponsesIngressStreamsTextThroughBaselineChatRoute(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
		WithAllowedModels("coding-economy"),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"coding-economy",
		"instructions":"You are a coding assistant.",
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"say ok"}]}],
		"store":false,
		"stream":true,
		"include":[]
	}`))
	ctx, summary := WithRequestSummary(t.Context())
	request = request.WithContext(ctx)
	h.HandleResponses(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "text/event-stream") {
		t.Fatalf("Content-Type=%q", got)
	}
	var sawCreated, sawText, sawCompleted bool
	if err := consumeResponsesSSEMessages(strings.NewReader(recorder.Body.String()), func(msg responsesSSEMessage) error {
		var event map[string]any
		if err := json.Unmarshal([]byte(msg.data), &event); err != nil {
			return err
		}
		switch event["type"] {
		case "response.created":
			sawCreated = true
		case "response.output_item.done":
			item, _ := event["item"].(map[string]any)
			if item["type"] == "message" && item["role"] == "assistant" {
				content, _ := item["content"].([]any)
				if len(content) == 1 {
					part, _ := content[0].(map[string]any)
					sawText = part["type"] == "output_text" && part["text"] == "ok"
				}
			}
		case "response.completed":
			sawCompleted = true
			response, _ := event["response"].(map[string]any)
			if response["model"] != "coding-economy" {
				t.Fatalf("completed response model=%v", response["model"])
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("decode response SSE: %v\n%s", err, recorder.Body.String())
	}
	if !sawCreated || !sawText || !sawCompleted {
		t.Fatalf("events created=%v text=%v completed=%v body=%s", sawCreated, sawText, sawCompleted, recorder.Body.String())
	}
	operationID := recorder.Header().Get("X-Vekil-Request-ID")
	if operationID == "" || operationID != summary.OperationID() {
		t.Fatalf("request ID/header = %q/%q", operationID, summary.OperationID())
	}
	lightRequests, lightModels := light.snapshot()
	if lightRequests != 1 || strings.Join(lightModels, ",") != "light-model" {
		t.Fatalf("light requests=%d models=%v", lightRequests, lightModels)
	}
	if powerfulRequests, _ := powerful.snapshot(); powerfulRequests != 0 {
		t.Fatalf("powerful requests=%d", powerfulRequests)
	}
	h.RecordRequest(summary, recorder.Code, "codex", 0)
	snapshot := h.stats.snapshot()
	if snapshot.Totals.Requests != 1 || len(snapshot.ByRoute) != 1 || snapshot.ByRoute[0].Route != "coding-economy" {
		t.Fatalf("policy Responses request was not recorded exactly once: %+v", snapshot)
	}
	if len(snapshot.Recent) != 1 || snapshot.Recent[0].OperationID != operationID {
		t.Fatalf("request stats operation ID = %+v, want %q", snapshot.Recent, operationID)
	}
	if len(snapshot.RecentAttempts) != 1 || snapshot.RecentAttempts[0].OperationID != operationID {
		t.Fatalf("route attempt operation ID = %+v, want %q", snapshot.RecentAttempts, operationID)
	}
}

func TestPolicyResponsesIngressAllowsResolvedPolicyAliasWithinLaunchScope(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)
	cfg.PolicyProfiles[0].PublicID = "coding-economy-20260717"
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")),
		WithProvidersConfig(cfg),
		WithAllowedModels("coding-economy"),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"coding-economy-20260717",
		"input":"say ok",
		"store":false,
		"stream":false
	}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Model != "coding-economy-20260717" {
		t.Fatalf("response model=%q, want canonical policy id", response.Model)
	}
}

func TestPolicyResponsesIngressRejectsSmallOutputLimitBeforePolicySelection(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{
		TurnType:  policyTurnTypePlanning,
		CodeScope: policyCodeScopeMultiFile,
		RiskLevel: policyRiskLevelHigh,
	})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	lightBefore, _ := light.snapshot()
	powerfulBefore, _ := powerful.snapshot()

	recorder := httptest.NewRecorder()
	h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"coding-economy",
		"input":"plan a risky multi-file change",
		"max_output_tokens":15,
		"store":false
	}`)))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "at least 16") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	lightAfter, _ := light.snapshot()
	powerfulAfter, _ := powerful.snapshot()
	if lightAfter != lightBefore || powerfulAfter != powerfulBefore {
		t.Fatalf("small output limit dispatched policy traffic: light %d->%d powerful %d->%d", lightBefore, lightAfter, powerfulBefore, powerfulAfter)
	}
}

func TestPolicyResponsesIngressUsesSuccessfulFailoverHeaders(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("RateLimit-Remaining", "1")
		w.Header().Set("X-Request-ID", "primary-request")
		_, _ = fmt.Fprint(w, "data: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"quota\"}}\n\n")
	}))
	t.Cleanup(primary.Close)
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("RateLimit-Remaining", "9")
		w.Header().Set("X-Request-ID", "secondary-request")
		_, _ = fmt.Fprint(w, strings.Join([]string{
			`data: {"id":"chat-secondary","object":"chat.completion.chunk","created":1,"model":"secondary-model","choices":[{"index":0,"delta":{"role":"assistant","content":"ok"}}]}`,
			`data: {"id":"chat-secondary","object":"chat.completion.chunk","created":1,"model":"secondary-model","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`data: [DONE]`,
			"",
		}, "\n\n"))
	}))
	t.Cleanup(secondary.Close)

	cfg := policyIntegrationConfig(primary.URL, secondary.URL, policyConfigModeOff)
	cfg.ModelRoutes[0].Targets = []ModelRouteTargetConfig{
		{ID: "primary", Provider: "light-provider", UpstreamModel: "primary-model"},
		{ID: "secondary", Provider: "power-provider", UpstreamModel: "secondary-model"},
	}
	cfg.ModelRoutes[0].Routing = ModelRouteRoutingConfig{
		Mode:              string(routeModePriorityFailover),
		MaxTargetAttempts: 2,
		MaxUpstreamSends:  2,
	}
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")),
		WithProvidersConfig(cfg),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"coding-economy",
		"input":"say ok",
		"store":false,
		"stream":false
	}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("RateLimit-Remaining"); got != "9" {
		t.Fatalf("RateLimit-Remaining=%q, want successful secondary value 9", got)
	}
	if got := recorder.Header().Get("X-Request-ID"); got != "" {
		t.Fatalf("X-Request-ID=%q, want policy-filtered", got)
	}
}

func TestPolicyResponsesIngressOverridesCodexEffortAndDropsReasoningSummary(t *testing.T) {
	var captured map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chat-summary","object":"chat.completion","created":1,"model":"light-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	}))
	defer upstream.Close()
	cfg := policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)
	cfg.ModelRoutes[0].ReasoningEffort = []string{"low"}
	cfg.ModelRoutes[1].ReasoningEffort = []string{"max"}
	cfg.PolicyProfiles[0].Lightweight.ReasoningEffort = "low"
	cfg.PolicyProfiles[0].Powerful.ReasoningEffort = "max"
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")),
		WithProvidersConfig(cfg),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"coding-economy",
		"input":"say ok",
		"reasoning":{"effort":"high","summary":"auto"},
		"store":false,
		"stream":false
	}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var effort string
	if err := json.Unmarshal(captured["reasoning_effort"], &effort); err != nil || effort != "low" {
		t.Fatalf("reasoning_effort=%q raw=%s", effort, captured["reasoning_effort"])
	}
	if _, exists := captured["reasoning"]; exists {
		t.Fatalf("reasoning summary leaked upstream: %+v", captured)
	}
	if _, exists := captured["reasoning_summary"]; exists {
		t.Fatalf("reasoning_summary leaked upstream: %+v", captured)
	}
}

func TestPolicyResponsesIngressAppliesProfileTierEffortWhenReasoningUnset(t *testing.T) {
	var captured map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprint(w, `{"id":"chat-default","object":"chat.completion","created":1,"model":"light-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`)
	}))
	defer upstream.Close()
	cfg := policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)
	cfg.ModelRoutes[0].ReasoningEffort = []string{"low"}
	cfg.ModelRoutes[1].ReasoningEffort = []string{"max"}
	cfg.PolicyProfiles[0].Lightweight.ReasoningEffort = "low"
	cfg.PolicyProfiles[0].Powerful.ReasoningEffort = "max"
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")),
		WithProvidersConfig(cfg),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"coding-economy",
		"input":"say ok",
		"reasoning":{},
		"store":false,
		"stream":false
	}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var effort string
	if err := json.Unmarshal(captured["reasoning_effort"], &effort); err != nil || effort != "low" {
		t.Fatalf("reasoning_effort=%q raw=%s", effort, captured["reasoning_effort"])
	}
}

func TestPolicyResponsesIngressRoundTripsNamespacedToolContinuation(t *testing.T) {
	const replayID = "call_vekil_AAAAAAAAAAAAAAAAAAAAAA"
	type capturedRequest struct {
		Model    string `json:"model"`
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"messages"`
		Tools []struct {
			Type     string `json:"type"`
			Function struct {
				Name string `json:"name"`
			} `json:"function"`
		} `json:"tools"`
	}
	var mu sync.Mutex
	var requests []capturedRequest
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var request capturedRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		requests = append(requests, request)
		requestNumber := len(requests)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		if requestNumber == 1 {
			if len(request.Tools) != 1 || request.Tools[0].Type != "function" || request.Tools[0].Function.Name == "" {
				http.Error(w, "namespace tool was not flattened", http.StatusBadRequest)
				return
			}
			name := request.Tools[0].Function.Name
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"terminal-stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":%q,\"type\":\"function\",\"function\":{\"name\":%q,\"arguments\":\"{\\\"path\\\":\\\"README.md\\\"}\"}}]}}]}\n\n", request.Model, replayID, name)
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"terminal-stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n", request.Model)
		} else {
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"terminal-stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"done\"}}]}\n\n", request.Model)
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"terminal-stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", request.Model)
		}
		_, _ = fmt.Fprint(w, "data: {\"id\":\"terminal-stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"ignored\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	cfg := policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")), WithProvidersConfig(cfg), WithPolicyRoutingMode(PolicyRoutingModeOff))
	if err != nil {
		t.Fatal(err)
	}

	firstBody := `{
		"model":"coding-economy",
		"input":"inspect the readme",
			"tools":[{"type":"namespace","name":"mcp__files","description":"File tools","tools":[{"type":"function","name":"read_file","description":"Read a file","strict":false,"parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}}]}],
		"tool_choice":"auto",
		"parallel_tool_calls":true,
		"store":false,
		"stream":true,
		"include":[]
	}`
	first := httptest.NewRecorder()
	h.HandleResponses(first, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(firstBody)))
	if first.Code != http.StatusOK {
		t.Fatalf("first status=%d body=%s", first.Code, first.Body.String())
	}
	var sawNamespacedCall bool
	if err := consumeResponsesSSEMessages(strings.NewReader(first.Body.String()), func(msg responsesSSEMessage) error {
		var event map[string]any
		if err := json.Unmarshal([]byte(msg.data), &event); err != nil {
			return err
		}
		if event["type"] != "response.output_item.done" {
			return nil
		}
		item, _ := event["item"].(map[string]any)
		sawNamespacedCall = item["type"] == "function_call" && item["call_id"] == replayID && item["namespace"] == "mcp__files" && item["name"] == "read_file"
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if !sawNamespacedCall {
		t.Fatalf("first response missing namespaced function call: %s", first.Body.String())
	}

	secondBody := `{
		"model":"coding-economy",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"inspect the readme"}]},
			{"type":"function_call","namespace":"mcp__files","name":"read_file","arguments":"{\"path\":\"README.md\"}","call_id":"` + replayID + `"},
			{"type":"function_call_output","call_id":"` + replayID + `","output":"contents"}
		],
			"tools":[
				{"type":"namespace","name":"mcp__files","description":"File tools","tools":[{"type":"function","name":"read_file","description":"Read a file","strict":false,"parameters":{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}}]},
				{"type":"function","name":"mcp__files__read_file","description":"Colliding top-level tool added on the continuation","strict":false,"parameters":{"type":"object","properties":{}}}
			],
		"tool_choice":"auto",
		"parallel_tool_calls":true,
		"store":false,
		"stream":true,
		"include":[]
	}`
	second := httptest.NewRecorder()
	h.HandleResponses(second, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(secondBody)))
	if second.Code != http.StatusOK || !strings.Contains(second.Body.String(), `"text":"done"`) {
		t.Fatalf("second status=%d body=%s", second.Code, second.Body.String())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(requests) != 2 {
		t.Fatalf("upstream requests=%d", len(requests))
	}
	firstToolName := requests[0].Tools[0].Function.Name
	if firstToolName == "read_file" || strings.Contains(firstToolName, ".") || len(firstToolName) > 64 {
		t.Fatalf("flattened tool name=%q", firstToolName)
	}
	if len(requests[1].Messages) < 3 {
		t.Fatalf("continuation messages=%+v", requests[1].Messages)
	}
	var sawAssistantCall, sawToolOutput bool
	for _, message := range requests[1].Messages {
		if message.Role == "assistant" && len(message.ToolCalls) == 1 {
			sawAssistantCall = message.ToolCalls[0].ID == replayID && message.ToolCalls[0].Function.Name == firstToolName
		}
		if message.Role == "tool" {
			sawToolOutput = message.ToolCallID == replayID
		}
	}
	if !sawAssistantCall || !sawToolOutput {
		t.Fatalf("continuation assistant=%v tool=%v messages=%+v", sawAssistantCall, sawToolOutput, requests[1].Messages)
	}
}

func TestPolicyResponsesIngressPreservesEmptyStreamedText(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chat-empty\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chat-empty\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chat-empty\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":0,\"total_tokens\":1}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")), WithProvidersConfig(policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)), WithPolicyRoutingMode(PolicyRoutingModeOff))
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"coding-economy","input":"hello","store":false,"stream":true}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), `"text":""`) || !strings.Contains(recorder.Body.String(), "event: response.completed") {
		t.Fatalf("empty streamed text was not preserved: %s", recorder.Body.String())
	}
}

func TestPolicyResponsesIngressRejectsStoredRequestsBeforeUpstreamSend(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	ctx, summary := WithRequestSummary(t.Context())
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"coding-economy","input":"hello","store":true}`)).WithContext(ctx)
	h.HandleResponses(recorder, request)
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "store") {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("X-Vekil-Request-ID"); got == "" || got != summary.OperationID() {
		t.Fatalf("request ID/header = %q/%q", got, summary.OperationID())
	}
	for _, name := range []string{"Openai-Model", "X-Openai-Model"} {
		if got := recorder.Header().Get(name); got != "coding-economy" {
			t.Fatalf("%s = %q, want policy public ID", name, got)
		}
	}
	if sends, _ := light.snapshot(); sends != 0 {
		t.Fatalf("light sends=%d", sends)
	}
	if sends, _ := powerful.snapshot(); sends != 0 {
		t.Fatalf("powerful sends=%d", sends)
	}
}

func TestPolicyResponsesIngressEnforceSelectsPowerfulAndReturnsJSON(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{
		TurnType:  policyTurnTypePlanning,
		CodeScope: policyCodeScopeMultiFile,
		RiskLevel: policyRiskLevelHigh,
	})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"coding-economy",
		"input":"plan a multi-file refactor",
		"store":false,
		"stream":false
	}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Fatalf("Content-Type=%q", got)
	}
	var response policyResponsesResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Model != "coding-economy" || response.Status != "completed" || len(response.Output) != 1 {
		t.Fatalf("response=%+v", response)
	}
	content, _ := response.Output[0]["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "ok" {
		t.Fatalf("output=%+v", response.Output)
	}
	lightRequests, lightModels := light.snapshot()
	if lightRequests != 2 || strings.Join(lightModels, ",") != "classifier-model,classifier-model" {
		t.Fatalf("light requests=%d models=%v", lightRequests, lightModels)
	}
	powerfulRequests, powerfulModels := powerful.snapshot()
	if powerfulRequests != 1 || strings.Join(powerfulModels, ",") != "power-model" {
		t.Fatalf("powerful requests=%d models=%v", powerfulRequests, powerfulModels)
	}
}

func TestCollectPolicyResponsesRejectsPreAggregatedNativeCompletion(t *testing.T) {
	finishReason := "tool_calls"
	completion := &models.OpenAIResponse{Choices: []models.OpenAIChoice{{
		Index: 0,
		Message: models.OpenAIMessage{Role: "assistant", ToolCalls: []models.OpenAIToolCall{{
			ID: "call-repaired", Type: "function", Function: models.OpenAIFunctionCall{Name: "lookup", Arguments: `{}`},
		}}},
		FinishReason: &finishReason,
	}}}
	h := &ProxyHandler{}
	if _, err := h.collectPolicyResponsesChatCompletion(t.Context(), &chatExecutionResult{Completion: completion}, nil, chatCompletionsMode{}); err == nil || !strings.Contains(err.Error(), "unsupported pre-aggregated native completion") {
		t.Fatalf("error = %v", err)
	}
	got, err := h.collectPolicyResponsesChatCompletion(t.Context(), &chatExecutionResult{Completion: completion, Backend: chatBackendResponses}, nil, chatCompletionsMode{})
	if err != nil || got != completion {
		t.Fatalf("Responses-backed completion = %#v, error = %v", got, err)
	}
}

func TestPolicyResponsesChatUpstreamErrorDrainsAndClosesBody(t *testing.T) {
	body := newTrackingReadCloser(strings.Repeat("x", upstreamErrorDetailMaxBodyBytes*2))
	response := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"1"}},
		Body:       body,
	}

	got := policyResponsesChatUpstreamError(response)
	if got.StatusCode != http.StatusTooManyRequests || got.Type != "rate_limit_error" || got.Message != "upstream request failed" {
		t.Fatalf("error = %+v", got)
	}
	if got.Headers.Get("Retry-After") != "1" {
		t.Fatalf("Retry-After = %q, want 1", got.Headers.Get("Retry-After"))
	}
	if body.bytesRead != upstreamErrorDetailMaxBodyBytes {
		t.Fatalf("body bytes read = %d, want bounded drain of %d", body.bytesRead, upstreamErrorDetailMaxBodyBytes)
	}
	if !body.closed {
		t.Fatal("upstream error body was not closed")
	}
}

func TestPolicyResponsesIngressTranslatesStructuredOutput(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var request models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var format struct {
			Type       string `json:"type"`
			JSONSchema struct {
				Name   string          `json:"name"`
				Schema json.RawMessage `json:"schema"`
				Strict *bool           `json:"strict"`
			} `json:"json_schema"`
		}
		if err := json.Unmarshal(request.ResponseFormat, &format); err != nil {
			t.Errorf("decode response_format: %v", err)
		}
		if format.Type != "json_schema" || format.JSONSchema.Name != "codex_output_schema" || format.JSONSchema.Strict == nil || !*format.JSONSchema.Strict {
			t.Errorf("response_format = %+v", format)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chat-schema\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"{\\\"ok\\\":true}\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chat-schema\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chat-schema\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")),
		WithProvidersConfig(policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"coding-economy",
		"input":"return JSON",
		"store":false,
		"stream":true,
		"text":{"format":{"type":"json_schema","name":"codex_output_schema","schema":{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false},"strict":true}}
	}`)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, "event: response.completed") || !strings.Contains(body, `"text":{"format":{"type":"json_schema"`) || !strings.Contains(body, `{\"ok\":true}`) {
		t.Fatalf("structured policy response is incomplete: %s", body)
	}
}

func TestPolicyResponsesIngressAllowsStreamWithoutUsage(t *testing.T) {
	var request map[string]json.RawMessage
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if _, ok := request["stream_options"]; !ok {
			t.Errorf("request is missing injected stream_options: %v", request)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chat-no-usage\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n")
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chat-no-usage\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")),
		WithProvidersConfig(policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"coding-economy",
		"input":"hello",
		"store":false,
		"stream":true
	}`)))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var completed *policyResponsesResponse
	if err := consumeResponsesSSEMessages(strings.NewReader(recorder.Body.String()), func(msg responsesSSEMessage) error {
		var event struct {
			Type     string                  `json:"type"`
			Response policyResponsesResponse `json:"response"`
		}
		if err := json.Unmarshal([]byte(msg.data), &event); err != nil {
			return err
		}
		if event.Type == "response.completed" {
			value := event.Response
			completed = &value
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if completed == nil {
		t.Fatalf("missing response.completed event: %s", recorder.Body.String())
	}
	if completed.Usage.InputTokens != 0 || completed.Usage.OutputTokens != 0 || completed.Usage.TotalTokens != 0 {
		t.Fatalf("usage = %+v, want zero-valued fallback", completed.Usage)
	}
}

func TestPolicyResponsesIngressRejectsMalformedTerminalCompletion(t *testing.T) {
	for _, tc := range []struct {
		name              string
		requestBody       string
		upstreamStream    bool
		wantParallelFalse bool
		contentType       string
		responseBody      string
	}{
		{
			name:         "non-streaming missing finish reason",
			requestBody:  `{"model":"coding-economy","input":"hello","store":false}`,
			contentType:  "application/json",
			responseBody: `{"id":"chat-missing-finish","object":"chat.completion","created":1,"model":"light-model","choices":[{"index":0,"message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`,
		},
		{
			name:         "non-streaming non-assistant role",
			requestBody:  `{"model":"coding-economy","input":"hello","store":false}`,
			contentType:  "application/json",
			responseBody: `{"id":"chat-wrong-role","object":"chat.completion","created":1,"model":"light-model","choices":[{"index":0,"message":{"role":"user","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`,
		},
		{
			name:           "streaming missing finish reason",
			requestBody:    `{"model":"coding-economy","input":"hello","store":false,"stream":true}`,
			upstreamStream: true,
			contentType:    "text/event-stream",
			responseBody: "data: {\"id\":\"chat-missing-finish\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n" +
				"data: {\"id\":\"chat-missing-finish\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name:           "streaming non-function tool call",
			requestBody:    `{"model":"coding-economy","input":"use lookup","store":false,"stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`,
			upstreamStream: true,
			contentType:    "text/event-stream",
			responseBody: "data: {\"id\":\"chat-custom-call\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-custom\",\"type\":\"custom\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]}}]}\n\n" +
				"data: {\"id\":\"chat-custom-call\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
				"data: {\"id\":\"chat-custom-call\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name:           "streaming refusal with tool call",
			requestBody:    `{"model":"coding-economy","input":"use lookup","store":false,"stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`,
			upstreamStream: true,
			contentType:    "text/event-stream",
			responseBody: "data: {\"id\":\"chat-refusal-call\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"refusal\":\"cannot comply\",\"tool_calls\":[{\"index\":0,\"id\":\"call-refusal\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}}]}}]}\n\n" +
				"data: {\"id\":\"chat-refusal-call\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
				"data: {\"id\":\"chat-refusal-call\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name:           "streaming invalid function arguments",
			requestBody:    `{"model":"coding-economy","input":"use lookup","store":false,"stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`,
			upstreamStream: true,
			contentType:    "text/event-stream",
			responseBody: "data: {\"id\":\"chat-invalid-args\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-invalid-args\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{not-json\"}}]}}]}\n\n" +
				"data: {\"id\":\"chat-invalid-args\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
				"data: {\"id\":\"chat-invalid-args\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name:           "required tool choice ignored",
			requestBody:    `{"model":"coding-economy","input":"use lookup","store":false,"stream":true,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":"required"}`,
			upstreamStream: true,
			contentType:    "text/event-stream",
			responseBody: "data: {\"id\":\"chat-required-stop\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"no tool\"}}]}\n\n" +
				"data: {\"id\":\"chat-required-stop\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n" +
				"data: {\"id\":\"chat-required-stop\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n" +
				"data: [DONE]\n\n",
		},
		{
			name:              "parallel tool calls while disabled",
			requestBody:       `{"model":"coding-economy","input":"use both","store":false,"stream":true,"parallel_tool_calls":false,"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"function","name":"search","parameters":{"type":"object"}}]}`,
			upstreamStream:    true,
			wantParallelFalse: true,
			contentType:       "text/event-stream",
			responseBody: "data: {\"id\":\"chat-parallel-calls\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-a\",\"type\":\"function\",\"function\":{\"name\":\"lookup\",\"arguments\":\"{}\"}},{\"index\":1,\"id\":\"call-b\",\"type\":\"function\",\"function\":{\"name\":\"search\",\"arguments\":\"{}\"}}]}}]}\n\n" +
				"data: {\"id\":\"chat-parallel-calls\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n" +
				"data: {\"id\":\"chat-parallel-calls\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"light-model\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\n" +
				"data: [DONE]\n\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				defer func() { _ = r.Body.Close() }()
				var request models.OpenAIRequest
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				gotStream := request.Stream != nil && *request.Stream
				if gotStream != tc.upstreamStream {
					t.Errorf("upstream stream = %v, want %v", gotStream, tc.upstreamStream)
				}
				if tc.wantParallelFalse && (request.ParallelToolCalls == nil || *request.ParallelToolCalls) {
					t.Errorf("upstream parallel_tool_calls = %v, want false", request.ParallelToolCalls)
				}
				w.Header().Set("Content-Type", tc.contentType)
				_, _ = fmt.Fprint(w, tc.responseBody)
			}))
			defer upstream.Close()

			h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")),
				WithProvidersConfig(policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)),
				WithPolicyRoutingMode(PolicyRoutingModeOff),
			)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(tc.requestBody)))

			if recorder.Code != http.StatusBadGateway {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			body := recorder.Body.String()
			if !strings.Contains(body, "failed to translate policy response") || strings.Contains(body, `"type":"function_call"`) || strings.Contains(body, "call-custom") {
				t.Fatalf("unsafe policy translation error: %s", body)
			}
		})
	}
}

func TestPolicyResponsesIngressSanitizesTerminalFailure(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	light.terminalFailureStatus.Store(http.StatusBadGateway)

	recorder := httptest.NewRecorder()
	h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"coding-economy",
		"input":"hello",
		"store":false
	}`)))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"message":"upstream request failed"`) || !strings.Contains(body, `"code":"upstream_error"`) {
		t.Fatalf("unsanitized policy error: %s", body)
	}
	for _, leaked := range []string{"terminal unavailable", "light-model", "light-route", "light-provider"} {
		if strings.Contains(body, leaked) {
			t.Fatalf("policy error leaked %q: %s", leaked, body)
		}
	}
	for _, header := range []string{"X-Request-ID", "X-Azure-Request-ID", "OpenAI-Processing-Ms", "X-Vekil-Internal-Route"} {
		if got := recorder.Header().Get(header); got != "" {
			t.Fatalf("%s=%q, want omitted", header, got)
		}
	}
}

func TestPolicyResponsesIngressCapturesToolOptimizerContext(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var request models.OpenAIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"chat-tool\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"tool_calls\":[{\"index\":0,\"id\":\"call-shell\",\"type\":\"function\",\"function\":{\"name\":\"shell_command\",\"arguments\":\"{\\\"command\\\":\\\"grep foo big.log\\\"}\"}}]}}]}\n\n", request.Model)
		_, _ = fmt.Fprintf(w, "data: {\"id\":\"chat-tool\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"tool_calls\"}]}\n\n", request.Model)
		_, _ = fmt.Fprint(w, "data: {\"id\":\"chat-tool\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"ignored\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()
	cfg := policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")), WithProvidersConfig(cfg), WithPolicyRoutingMode(PolicyRoutingModeOff))
	if err != nil {
		t.Fatal(err)
	}
	fake := &recordingToolOptimizer{}
	configureRecordingToolOptimizer(h, fake)

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{
		"model":"coding-economy",
		"input":"run a command",
		"tools":[{"type":"function","name":"shell_command","description":"Run shell","parameters":{"type":"object","properties":{"command":{"type":"string"}},"required":["command"]}}],
		"store":false,
		"stream":false
	}`))
	request.Header.Set("session_id", "policy-optimizer")
	h.HandleResponses(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	toolContext, ok := h.toolContexts.Get("session:policy-optimizer", "call-shell")
	if !ok {
		t.Fatal("policy Responses completion did not capture tool context")
	}
	if toolContext.ToolName != "shell_command" || toolContext.OriginalCommand != "grep foo big.log" {
		t.Fatalf("captured tool context=%+v", toolContext)
	}
}

func TestPolicyResponsesIngressPlannerUnavailableUsesServerError(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")),
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatal(err)
	}
	// Deliberately do not initialize policy routing: enforce requests must fail as
	// transient server unavailability, not as a permanent invalid client request.
	recorder := httptest.NewRecorder()
	h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"coding-economy","input":"hello","store":false}`)))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if strings.Contains(body, `"type":"invalid_request_error"`) || !strings.Contains(body, `"message":"upstream request failed"`) {
		t.Fatalf("planner outage used client-error semantics: %s", body)
	}
}

func TestObservePolicyResponsesExecutionErrorRecordsUsage(t *testing.T) {
	ctx, summary := WithRequestSummary(t.Context())
	observePolicyResponsesExecutionError(ctx, &chatExecutionError{
		StatusCode: http.StatusTooManyRequests,
		Usage: &models.OpenAIUsage{
			PromptTokens:     17,
			CompletionTokens: 3,
			TotalTokens:      20,
		},
	})
	stats := readSummaryForStats(summary)
	if stats.prompt != 17 || stats.completion != 3 || stats.total != 20 {
		t.Fatalf("failed execution usage=%+v", stats)
	}
}
