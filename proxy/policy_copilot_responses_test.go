package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/sozercan/vekil/auth"
)

type copilotResponsesPolicyUpstream struct {
	server *httptest.Server

	mu                 sync.Mutex
	modelRequests      int
	classifierRequests int
	terminalModels     []string
	paths              []string
	classifierSignals  policyClassifierSignals
}

func newCopilotResponsesPolicyUpstream(t *testing.T, signals policyClassifierSignals) *copilotResponsesPolicyUpstream {
	t.Helper()
	upstream := &copilotResponsesPolicyUpstream{classifierSignals: signals}
	upstream.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstream.mu.Lock()
		upstream.paths = append(upstream.paths, r.URL.Path)
		upstream.mu.Unlock()
		if got := r.Header.Get("Authorization"); got != "Bearer fixture-token" {
			http.Error(w, "missing Copilot auth", http.StatusUnauthorized)
			return
		}
		switch r.URL.Path {
		case providerEndpointModels:
			upstream.mu.Lock()
			upstream.modelRequests++
			upstream.mu.Unlock()
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data": []any{
					map[string]any{"id": "gpt-5.6-luna", "object": "model", "owned_by": "github-copilot", "supported_endpoints": []string{providerEndpointChatCompletions, providerEndpointResponses}},
					map[string]any{"id": "gpt-5.6-sol", "object": "model", "owned_by": "github-copilot", "supported_endpoints": []string{providerEndpointResponses}},
				},
			})
		case providerEndpointResponses:
			defer func() { _ = r.Body.Close() }()
			var request struct {
				Model  string            `json:"model"`
				Input  []json.RawMessage `json:"input"`
				Stream bool              `json:"stream"`
				Store  json.RawMessage   `json:"store"`
				Tools  []struct {
					Name string `json:"name"`
				} `json:"tools"`
			}
			if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			classifier := false
			lookupTool := false
			for _, tool := range request.Tools {
				if tool.Name == policyClassifierToolName {
					classifier = true
				}
				if tool.Name == "lookup_symbol" {
					lookupTool = true
				}
			}
			if classifier {
				if len(request.Store) != 0 {
					http.Error(w, "classifier store must be omitted when no-store is not declared", http.StatusBadRequest)
					return
				}
				upstream.mu.Lock()
				upstream.classifierRequests++
				upstream.mu.Unlock()
				arguments, _ := json.Marshal(upstream.classifierSignals)
				_ = json.NewEncoder(w).Encode(map[string]any{
					"id":         "resp-classifier",
					"object":     "response",
					"created_at": int64(1),
					"status":     "completed",
					"output": []any{map[string]any{
						"type": "function_call", "id": "fc-classifier", "call_id": "call-classifier",
						"name": policyClassifierToolName, "arguments": string(arguments), "status": "completed",
					}},
					"usage": map[string]any{"input_tokens": 10, "output_tokens": 4, "total_tokens": 14},
				})
				return
			}

			upstream.mu.Lock()
			upstream.terminalModels = append(upstream.terminalModels, request.Model)
			upstream.mu.Unlock()
			if lookupTool {
				w.Header().Set("Content-Type", "text/event-stream")
				writeCopilotResponsesToolCallStream(w)
				return
			}
			hasToolOutput := false
			for _, raw := range request.Input {
				var item struct {
					Type string `json:"type"`
				}
				if json.Unmarshal(raw, &item) == nil && item.Type == "function_call_output" {
					hasToolOutput = true
					break
				}
			}
			text := "answered by Sol"
			if request.Model == "gpt-5.6-luna" {
				text = "answered by Luna"
			}
			if hasToolOutput {
				text = "tool result accepted"
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":         "resp-terminal",
				"object":     "response",
				"created_at": int64(2),
				"status":     "completed",
				"output": []any{map[string]any{
					"type": "message", "id": "msg-terminal", "status": "completed", "role": "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": text}},
				}},
				"usage": map[string]any{"input_tokens": 20, "output_tokens": 5, "total_tokens": 25},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(upstream.server.Close)
	return upstream
}

func writeCopilotResponsesToolCallStream(w http.ResponseWriter) {
	write := func(event string, payload any) {
		encoded, _ := json.Marshal(payload)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, encoded)
	}
	write("response.created", map[string]any{
		"type": "response.created", "sequence_number": 0,
		"response": map[string]any{"id": "resp-tool", "created_at": int64(3), "status": "in_progress", "output": []any{}},
	})
	write("response.output_item.added", map[string]any{
		"type": "response.output_item.added", "sequence_number": 1, "output_index": 0,
		"item": map[string]any{"type": "function_call", "id": "fc-lookup", "call_id": "call-lookup", "name": "lookup_symbol", "arguments": "", "status": "in_progress"},
	})
	write("response.function_call_arguments.delta", map[string]any{
		"type": "response.function_call_arguments.delta", "sequence_number": 2, "item_id": "fc-lookup", "output_index": 0, "delta": `{"symbol":"main"}`,
	})
	write("response.function_call_arguments.done", map[string]any{
		"type": "response.function_call_arguments.done", "sequence_number": 3, "item_id": "fc-lookup", "output_index": 0, "arguments": `{"symbol":"main"}`,
	})
	call := map[string]any{"type": "function_call", "id": "fc-lookup", "call_id": "call-lookup", "name": "lookup_symbol", "arguments": `{"symbol":"main"}`, "status": "completed"}
	write("response.output_item.done", map[string]any{
		"type": "response.output_item.done", "sequence_number": 4, "output_index": 0, "item": call,
	})
	write("response.completed", map[string]any{
		"type": "response.completed", "sequence_number": 5,
		"response": map[string]any{
			"id": "resp-tool", "created_at": int64(3), "status": "completed", "output": []any{call},
			"usage": map[string]any{"input_tokens": 20, "output_tokens": 8, "total_tokens": 28},
		},
	})
}

func (u *copilotResponsesPolicyUpstream) snapshot() (int, int, []string, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.modelRequests, u.classifierRequests, append([]string(nil), u.terminalModels...), append([]string(nil), u.paths...)
}

func directCopilotResponsesPolicyConfig(profileMode string) ProvidersConfig {
	parallel := true
	return ProvidersConfig{
		SchemaVersion: ProvidersConfigSchemaVersion2,
		Providers: []ProviderConfig{{
			ID:            "copilot",
			Type:          string(providerTypeCopilot),
			TrustDomain:   "github-copilot",
			IncludeModels: []string{"gpt-5.6-luna", "gpt-5.6-sol"},
		}},
		ModelRoutes: []ModelRouteConfig{
			{
				ID:                "luna-route",
				Exposure:          modelRouteExposureInternal,
				Endpoints:         []string{providerEndpointResponses},
				ParallelToolCalls: &parallel,
				Targets: []ModelRouteTargetConfig{{
					ID: "luna", Provider: "copilot", UpstreamModel: "gpt-5.6-luna",
				}},
			},
			{
				ID:                "sol-route",
				Exposure:          modelRouteExposureInternal,
				Endpoints:         []string{providerEndpointResponses},
				ParallelToolCalls: &parallel,
				Targets: []ModelRouteTargetConfig{{
					ID: "sol", Provider: "copilot", UpstreamModel: "gpt-5.6-sol",
				}},
			},
			{
				ID:                "classifier-route",
				Exposure:          modelRouteExposureInternal,
				InternalPurpose:   modelRouteInternalPurposePolicyClassifier,
				Endpoints:         []string{providerEndpointResponses},
				ParallelToolCalls: &parallel,
				Targets: []ModelRouteTargetConfig{{
					ID: "classifier", Provider: "copilot", UpstreamModel: "gpt-5.6-sol",
				}},
			},
		},
		PolicyProfiles: []PolicyProfileConfig{{
			ID:               "semantic-policy",
			PublicID:         "gpt-5.6-semantic",
			Mode:             profileMode,
			LightweightRoute: "luna-route",
			PowerfulRoute:    "sol-route",
			Classifier: PolicyClassifierConfig{
				Route: "classifier-route",
			},
			DataPolicy: PolicyDataPolicyConfig{
				ContentForwardingAcknowledged: true,
				AllowProviderRetention:        true,
			},
		}},
	}
}

func TestPolicyConfigAcceptsPinnedDynamicCopilotResponsesRoutes(t *testing.T) {
	if err := ValidateProvidersConfig(directCopilotResponsesPolicyConfig(policyConfigModeEnforce)); err != nil {
		t.Fatalf("ValidateProvidersConfig() error = %v", err)
	}
}

func TestPolicyConfigDirectCopilotRetainsTrustAndRetentionChecks(t *testing.T) {
	t.Run("retention acknowledgement", func(t *testing.T) {
		cfg := directCopilotResponsesPolicyConfig(policyConfigModeEnforce)
		cfg.PolicyProfiles[0].DataPolicy.AllowProviderRetention = false
		if err := ValidateProvidersConfig(cfg); err == nil || !strings.Contains(err.Error(), "data_policy.allow_provider_retention") {
			t.Fatalf("ValidateProvidersConfig() error = %v", err)
		}
	})

	t.Run("trust domain", func(t *testing.T) {
		cfg := directCopilotResponsesPolicyConfig(policyConfigModeEnforce)
		cfg.Providers[0].TrustDomain = ""
		if err := ValidateProvidersConfig(cfg); err == nil || !strings.Contains(err.Error(), "providers[0].trust_domain") {
			t.Fatalf("ValidateProvidersConfig() error = %v", err)
		}
	})
}

func TestPolicyRoutingUsesCopilotResponsesForClassifierAndTerminalText(t *testing.T) {
	upstream := newCopilotResponsesPolicyUpstream(t, policyClassifierSignals{
		TurnType:                policyTurnTypePlanning,
		CodeScope:               policyCodeScopeMultiFile,
		RiskLevel:               policyRiskLevelHigh,
		RequiresCodebaseContext: true,
	})
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture-token"),
		nil,
		WithCopilotBaseURL(upstream.server.URL),
		WithProvidersConfig(directCopilotResponsesPolicyConfig(policyConfigModeEnforce)),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatalf("InitializePolicyRouting() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, providerEndpointChatCompletions, strings.NewReader(`{
		"model":"gpt-5.6-semantic",
		"messages":[{"role":"user","content":"Plan a coordinated multi-file migration."}],
		"max_completion_tokens":256
	}`))
	h.HandleOpenAIChatCompletions(recorder, request)
	if recorder.Code != http.StatusOK {
		_, _, _, paths := upstream.snapshot()
		t.Fatalf("status = %d, body = %s, upstream paths = %v", recorder.Code, recorder.Body.String(), paths)
	}
	var response struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("decode Chat response: %v", err)
	}
	if response.Model != "gpt-5.6-semantic" || len(response.Choices) != 1 || response.Choices[0].Message.Content != "answered by Sol" {
		t.Fatalf("response = %+v", response)
	}

	modelRequests, classifierRequests, terminalModels, paths := upstream.snapshot()
	if modelRequests != 1 || classifierRequests != 2 || strings.Join(terminalModels, ",") != "gpt-5.6-sol" {
		t.Fatalf("upstream requests: models=%d classifiers=%d terminals=%v paths=%v", modelRequests, classifierRequests, terminalModels, paths)
	}
	stats := h.policyRoutingController.(*chatPolicyRoutingController).PolicyStatsSnapshot()
	if len(stats.Profiles) != 1 || stats.Profiles[0].Totals.PhysicalClassifierSends != 2 || stats.Profiles[0].Totals.ActualTiers.Powerful != 1 || stats.Profiles[0].Totals.ClassifierUsage.TotalTokens != 28 {
		t.Fatalf("policy stats = %+v", stats)
	}
}

func TestPolicyRoutingSelectsCopilotResponsesLightweightRoute(t *testing.T) {
	upstream := newCopilotResponsesPolicyUpstream(t, policyClassifierSignals{
		TurnType:  policyTurnTypeLookup,
		CodeScope: policyCodeScopeNone,
		RiskLevel: policyRiskLevelLow,
	})
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture-token"),
		nil,
		WithCopilotBaseURL(upstream.server.URL),
		WithProvidersConfig(directCopilotResponsesPolicyConfig(policyConfigModeEnforce)),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatalf("InitializePolicyRouting() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, providerEndpointChatCompletions, strings.NewReader(`{
		"model":"gpt-5.6-semantic",
		"messages":[{"role":"user","content":"Explain one symbol."}],
		"max_completion_tokens":256
	}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Model != "gpt-5.6-semantic" || len(response.Choices) != 1 || response.Choices[0].Message.Content != "answered by Luna" {
		t.Fatalf("response = %+v", response)
	}
	modelRequests, classifierRequests, terminalModels, paths := upstream.snapshot()
	if modelRequests != 1 || classifierRequests != 2 || strings.Join(terminalModels, ",") != "gpt-5.6-luna" {
		t.Fatalf("upstream requests: models=%d classifiers=%d terminals=%v paths=%v", modelRequests, classifierRequests, terminalModels, paths)
	}
	stats := h.policyRoutingController.(*chatPolicyRoutingController).PolicyStatsSnapshot()
	if len(stats.Profiles) != 1 || stats.Profiles[0].Totals.ActualTiers.Lightweight != 1 {
		t.Fatalf("policy stats = %+v", stats)
	}
}

func TestPolicyRoutingCopilotResponsesToolContinuationUsesSameProcessReplay(t *testing.T) {
	upstream := newCopilotResponsesPolicyUpstream(t, policyClassifierSignals{
		TurnType:                policyTurnTypePlanning,
		CodeScope:               policyCodeScopeMultiFile,
		RiskLevel:               policyRiskLevelHigh,
		RequiresCodebaseContext: true,
	})
	cfg := directCopilotResponsesPolicyConfig(policyConfigModeEnforce)
	// A replay produced by the powerful Responses tier must stay on that tier;
	// the unrelated lightweight tier may remain native Chat-only.
	cfg.ModelRoutes[0].Endpoints = []string{providerEndpointChatCompletions}
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture-token"),
		nil,
		WithCopilotBaseURL(upstream.server.URL),
		WithProvidersConfig(cfg),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatalf("InitializePolicyRouting() error = %v", err)
	}

	initialBody := `{
		"model":"gpt-5.6-semantic",
		"messages":[{"role":"user","content":"Call lookup_symbol for main."}],
		"max_completion_tokens":256,
		"parallel_tool_calls":false,
		"tools":[{"type":"function","function":{"name":"lookup_symbol","description":"Look up one symbol.","strict":true,"parameters":{"type":"object","additionalProperties":false,"properties":{"symbol":{"type":"string"}},"required":["symbol"]}}}],
		"tool_choice":{"type":"function","function":{"name":"lookup_symbol"}}
	}`
	initial := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(initial, httptest.NewRequest(http.MethodPost, providerEndpointChatCompletions, strings.NewReader(initialBody)))
	if initial.Code != http.StatusOK {
		t.Fatalf("initial status = %d, body = %s", initial.Code, initial.Body.String())
	}
	var initialResponse struct {
		Model   string `json:"model"`
		Choices []struct {
			Message json.RawMessage `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(initial.Body.Bytes(), &initialResponse); err != nil {
		t.Fatalf("decode initial response: %v", err)
	}
	if initialResponse.Model != "gpt-5.6-semantic" || len(initialResponse.Choices) != 1 {
		t.Fatalf("initial response = %+v", initialResponse)
	}
	var assistant struct {
		ToolCalls []struct {
			ID       string `json:"id"`
			Function struct {
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
			} `json:"function"`
		} `json:"tool_calls"`
	}
	if err := json.Unmarshal(initialResponse.Choices[0].Message, &assistant); err != nil {
		t.Fatalf("decode assistant tool call: %v", err)
	}
	if len(assistant.ToolCalls) != 1 || assistant.ToolCalls[0].Function.Name != "lookup_symbol" || !strings.HasPrefix(assistant.ToolCalls[0].ID, responsesChatReplayCallIDPrefix) {
		t.Fatalf("assistant tool calls = %+v", assistant.ToolCalls)
	}

	continuationBody, err := json.Marshal(map[string]any{
		"model": "gpt-5.6-semantic",
		"messages": []any{
			map[string]any{"role": "user", "content": "Call lookup_symbol for main."},
			json.RawMessage(initialResponse.Choices[0].Message),
			map[string]any{"role": "tool", "tool_call_id": assistant.ToolCalls[0].ID, "content": "main is a function"},
		},
		"max_completion_tokens": 256,
	})
	if err != nil {
		t.Fatal(err)
	}
	continuation := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(continuation, httptest.NewRequest(http.MethodPost, providerEndpointChatCompletions, strings.NewReader(string(continuationBody))))
	if continuation.Code != http.StatusOK {
		t.Fatalf("continuation status = %d, body = %s", continuation.Code, continuation.Body.String())
	}
	var continuationResponse struct {
		Model   string `json:"model"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(continuation.Body.Bytes(), &continuationResponse); err != nil {
		t.Fatalf("decode continuation response: %v", err)
	}
	if continuationResponse.Model != "gpt-5.6-semantic" || len(continuationResponse.Choices) != 1 || continuationResponse.Choices[0].Message.Content != "tool result accepted" {
		t.Fatalf("continuation response = %+v", continuationResponse)
	}

	modelRequests, classifierRequests, terminalModels, paths := upstream.snapshot()
	if modelRequests != 1 || classifierRequests != 2 || strings.Join(terminalModels, ",") != "gpt-5.6-sol,gpt-5.6-sol" {
		t.Fatalf("upstream requests: models=%d classifiers=%d terminals=%v paths=%v", modelRequests, classifierRequests, terminalModels, paths)
	}
	stats := h.policyRoutingController.(*chatPolicyRoutingController).PolicyStatsSnapshot()
	if len(stats.Profiles) != 1 || stats.Profiles[0].Totals.PhysicalClassifierSends != 2 || stats.Profiles[0].Totals.ActualTiers.Powerful != 2 {
		t.Fatalf("policy stats = %+v", stats)
	}
}
