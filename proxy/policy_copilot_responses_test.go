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
			w.Header().Set("Openai-Model", request.Model)
			w.Header().Set("X-Github-Request-ID", "github-upstream-request")
			w.Header().Set("X-Request-ID", "upstream-request")
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

func TestProxyHandlerPolicyRoutingDefaultsToConfigMode(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
		want policyMode
	}{
		{name: "default follows profile", want: policyModeEnforce},
		{name: "explicit off remains off", opts: []Option{WithPolicyRoutingMode(PolicyRoutingModeOff)}, want: policyModeOff},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := []Option{
				WithProvidersConfig(directCopilotResponsesPolicyConfig(policyConfigModeEnforce)),
				WithDeferredDynamicProviderModelValidation(true),
			}
			options = append(options, tc.opts...)
			h, err := NewProxyHandler(auth.NewTestAuthenticator("fixture-token"), nil, options...)
			if err != nil {
				t.Fatalf("NewProxyHandler() error = %v", err)
			}
			t.Cleanup(h.BeginShutdown)
			controller := h.policyRoutingController.(*chatPolicyRoutingController)
			profile := controller.profiles["semantic-policy"]
			if profile == nil || profile.effectiveMode() != tc.want {
				t.Fatalf("effective mode = %v, want %v", profile.effectiveMode(), tc.want)
			}
			wantActive := tc.want != policyModeOff
			if h.PolicyRoutingActive() != wantActive || h.PolicyRoutingPreflightPending() != wantActive {
				t.Fatalf("active = %v, preflight pending = %v, want both %v", h.PolicyRoutingActive(), h.PolicyRoutingPreflightPending(), wantActive)
			}
		})
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
	if got := recorder.Header().Get("Openai-Model"); got != "gpt-5.6-semantic" {
		t.Fatalf("Openai-Model = %q, want public policy model", got)
	}
	for _, name := range []string{"X-Github-Request-ID", "X-Request-ID"} {
		if got := recorder.Header().Get(name); got != "" {
			t.Fatalf("%s = %q, want omitted", name, got)
		}
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

func TestPolicyPublicIDMayMatchHiddenCopilotTargetModel(t *testing.T) {
	upstream := newCopilotResponsesPolicyUpstream(t, policyClassifierSignals{
		TurnType:  policyTurnTypeLookup,
		CodeScope: policyCodeScopeNone,
		RiskLevel: policyRiskLevelLow,
	})
	cfg := directCopilotResponsesPolicyConfig(policyConfigModeEnforce)
	cfg.PolicyProfiles[0].PublicID = "gpt-5.6-luna"
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

	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, providerEndpointChatCompletions, strings.NewReader(`{
		"model":"gpt-5.6-luna",
		"messages":[{"role":"user","content":"Explain one symbol."}],
		"max_completion_tokens":256
	}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Model != "gpt-5.6-luna" {
		t.Fatalf("response model = %q, want policy public ID", response.Model)
	}
	_, classifierRequests, terminalModels, _ := upstream.snapshot()
	if classifierRequests != 2 || strings.Join(terminalModels, ",") != "gpt-5.6-luna" {
		t.Fatalf("classifier requests = %d, terminal models = %v", classifierRequests, terminalModels)
	}
}

func TestPublicExplicitRouteMayMatchHiddenCopilotTargetModel(t *testing.T) {
	cfg := directCopilotResponsesPolicyConfig(policyConfigModeEnforce)
	cfg.Providers = append(cfg.Providers, ProviderConfig{
		ID:             "local",
		Type:           "openai-compatible",
		BaseURL:        "https://local.example.test/v1",
		AuthType:       "none",
		ModelDiscovery: "static",
		TrustDomain:    "org",
	})
	cfg.ModelRoutes = append(cfg.ModelRoutes, ModelRouteConfig{
		ID:        "public-luna-route",
		PublicID:  "gpt-5.6-luna",
		Endpoints: []string{providerEndpointChatCompletions},
		Targets: []ModelRouteTargetConfig{{
			ID:            "local-luna",
			Provider:      "local",
			UpstreamModel: "local-luna",
		}},
	})
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture-token"),
		nil,
		WithProvidersConfig(cfg),
		WithDeferredDynamicProviderModelValidation(true),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)

	if !h.modelAllowedForRequest("gpt-5.6-luna", providerEndpointChatCompletions) {
		t.Fatal("public explicit route was blocked by a provider-local hidden model")
	}
	route, ok := h.resolveModelRouteForRequest("gpt-5.6-luna", providerEndpointChatCompletions)
	if !ok || route == nil || route.public.id != "gpt-5.6-luna" || route.public.routeID != "public-luna-route" {
		t.Fatalf("resolved route = %+v, known = %v", route, ok)
	}
}

func TestHiddenCopilotTargetRejectsNormalizedAliases(t *testing.T) {
	cfg := directCopilotResponsesPolicyConfig(policyConfigModeEnforce)
	cfg.Providers[0].IncludeModels = []string{"claude-sonnet-4.6", "gpt-5.6-sol"}
	cfg.ModelRoutes[0].Targets[0].UpstreamModel = "claude-sonnet-4.6"
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture-token"),
		nil,
		WithProvidersConfig(cfg),
		WithDeferredDynamicProviderModelValidation(true),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)

	for _, model := range []string{"claude-sonnet-4.6", "claude-sonnet-4-6", "claude-sonnet-4-6-20260101"} {
		t.Run(model, func(t *testing.T) {
			if h.modelAllowedForRequest(model, providerEndpointChatCompletions) {
				t.Fatalf("hidden Copilot target alias %q was allowed for direct routing", model)
			}
		})
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

func TestPolicyResponsesReplayPreservesPowerfulTierWhenRoutesShareTerminal(t *testing.T) {
	cfg := directCopilotResponsesPolicyConfig(policyConfigModeEnforce)
	cfg.PolicyProfiles[0].LightweightRoute = "sol-route"
	cfg.ModelRoutes = cfg.ModelRoutes[1:]
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture-token"),
		nil,
		WithProvidersConfig(cfg),
		WithDeferredDynamicProviderModelValidation(true),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)

	controller := h.policyRoutingController.(*chatPolicyRoutingController)
	profile := controller.profiles["semantic-policy"]
	if profile == nil || profile.lightweight != profile.powerful {
		t.Fatal("test profile did not compile both tiers to the same route")
	}

	publish := newResponsesChatReplayTestRequest("shared-route", replayTestCallSpec{
		upstreamID: "upstream-shared",
		name:       "lookup_symbol",
		visible:    `{"symbol":"main"}`,
	})
	publish.Route = responsesChatReplayRoute{
		ProviderID:    "copilot",
		PublicModel:   "gpt-5.6-semantic",
		UpstreamModel: "gpt-5.6-sol",
		RouteID:       "sol-route",
		PolicyTier:    policyTierPowerful.String(),
	}
	published, err := h.responsesChatReplayStore().Publish(publish)
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	call := published.Projection.Calls[0]
	body, err := json.Marshal(map[string]any{
		"model": "gpt-5.6-semantic",
		"messages": []any{
			map[string]any{
				"role":    "assistant",
				"content": published.Projection.Content,
				"tool_calls": []any{map[string]any{
					"id": call.ID, "type": "function",
					"function": map[string]any{"name": call.Name, "arguments": call.Arguments},
				}},
			},
			map[string]any{"role": "tool", "tool_call_id": call.ID, "content": "main is a function"},
		},
		"max_completion_tokens": 256,
	})
	if err != nil {
		t.Fatal(err)
	}

	route, tier, err := controller.resolvePolicyResponsesReplayRoute(profile, body)
	if err != nil {
		t.Fatalf("resolvePolicyResponsesReplayRoute() error = %v", err)
	}
	if route == nil || route.public.routeID != "sol-route" || tier != policyTierPowerful {
		t.Fatalf("resolved route/tier = (%v, %v), want sol-route/powerful", route, tier)
	}
}

func TestPolicyConfigRejectsCopilotTargetsOutsideProviderFilters(t *testing.T) {
	t.Run("include models", func(t *testing.T) {
		cfg := directCopilotResponsesPolicyConfig(policyConfigModeEnforce)
		cfg.Providers[0].IncludeModels = []string{"gpt-5.6-sol"}
		err := ValidateProvidersConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "gpt-5.6-luna") || !strings.Contains(err.Error(), "include_models/exclude_models") {
			t.Fatalf("ValidateProvidersConfig() error = %v", err)
		}
	})
	t.Run("exclude models", func(t *testing.T) {
		cfg := directCopilotResponsesPolicyConfig(policyConfigModeEnforce)
		cfg.Providers[0].ExcludeModels = []string{"gpt-5.6-sol"}
		err := ValidateProvidersConfig(cfg)
		if err == nil || !strings.Contains(err.Error(), "gpt-5.6-sol") || !strings.Contains(err.Error(), "include_models/exclude_models") {
			t.Fatalf("ValidateProvidersConfig() error = %v", err)
		}
	})
}

func TestPolicyCopilotPinnedTargetsMustSurviveDynamicDiscovery(t *testing.T) {
	tests := []struct {
		name   string
		models []map[string]any
		want   string
	}{
		{
			name: "missing pinned model",
			models: []map[string]any{
				{"id": "gpt-5.6-sol", "object": "model", "supported_endpoints": []string{providerEndpointResponses}},
			},
			want: `pinned model "gpt-5.6-luna"`,
		},
		{
			name: "missing pinned endpoint",
			models: []map[string]any{
				{"id": "gpt-5.6-luna", "object": "model", "supported_endpoints": []string{providerEndpointChatCompletions}},
				{"id": "gpt-5.6-sol", "object": "model", "supported_endpoints": []string{providerEndpointResponses}},
			},
			want: `does not advertise endpoint "/responses"`,
		},
		{
			name: "omitted endpoint advertisement",
			models: []map[string]any{
				{"id": "gpt-5.6-luna", "object": "model"},
				{"id": "gpt-5.6-sol", "object": "model", "supported_endpoints": []string{providerEndpointResponses}},
			},
			want: "did not advertise supported_endpoints",
		},
		{
			name: "invalid endpoint advertisement",
			models: []map[string]any{
				{"id": "gpt-5.6-luna", "object": "model", "supported_endpoints": []string{"", "   "}},
				{"id": "gpt-5.6-sol", "object": "model", "supported_endpoints": []string{providerEndpointResponses}},
			},
			want: "did not advertise supported_endpoints",
		},
		{
			name: "disabled pinned model",
			models: []map[string]any{
				{"id": "gpt-5.6-luna", "object": "model", "supported_endpoints": []string{providerEndpointResponses}, "policy": map[string]any{"state": "disabled"}},
				{"id": "gpt-5.6-sol", "object": "model", "supported_endpoints": []string{providerEndpointResponses}},
			},
			want: `pinned model "gpt-5.6-luna"`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != providerEndpointModels {
					http.NotFound(w, r)
					return
				}
				if got := r.Header.Get("Authorization"); got != "Bearer fixture-token" {
					http.Error(w, "missing auth", http.StatusUnauthorized)
					return
				}
				_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": tc.models})
			}))
			defer upstream.Close()

			h, err := NewProxyHandler(
				auth.NewTestAuthenticator("fixture-token"),
				nil,
				WithCopilotBaseURL(upstream.URL),
				WithProvidersConfig(directCopilotResponsesPolicyConfig(policyConfigModeEnforce)),
				WithAllowedModels("gpt-5.6-semantic"),
				WithDeferredDynamicProviderModelValidation(true),
				WithPolicyRoutingMode(PolicyRoutingModeEnforce),
			)
			if err != nil {
				t.Fatalf("NewProxyHandler() error = %v", err)
			}
			defer h.BeginShutdown()
			err = h.ValidateDynamicProviderModels(t.Context())
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ValidateDynamicProviderModels() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestPolicyCopilotDynamicValidationSkipsInactiveTierRoutes(t *testing.T) {
	classifierNoStore := true
	cfg := ProvidersConfig{
		SchemaVersion: 2,
		Providers: []ProviderConfig{
			{ID: "copilot", Type: "copilot", Default: true, TrustDomain: "org", IncludeModels: []string{"classifier", "power"}},
			{ID: "local", Type: "openai-compatible", BaseURL: "https://local.example.test/v1", AuthType: "none", ModelDiscovery: "static", TrustDomain: "org", ClassifierNoStoreSupported: &classifierNoStore},
		},
		ModelRoutes: []ModelRouteConfig{
			{ID: "light", Exposure: "internal", Endpoints: []string{providerEndpointResponses}, Targets: []ModelRouteTargetConfig{{ID: "light", Provider: "local", UpstreamModel: "light"}}},
			{ID: "power", Exposure: "internal", Endpoints: []string{providerEndpointResponses}, Targets: []ModelRouteTargetConfig{{ID: "power", Provider: "copilot", UpstreamModel: "power"}}},
			{ID: "classifier", Exposure: "internal", InternalPurpose: modelRouteInternalPurposePolicyClassifier, Endpoints: []string{providerEndpointResponses}, Targets: []ModelRouteTargetConfig{{ID: "classifier", Provider: "copilot", UpstreamModel: "classifier"}}},
		},
		PolicyProfiles: []PolicyProfileConfig{{
			ID: "policy", PublicID: "semantic", Mode: policyConfigModeEnforce, LightweightRoute: "light", PowerfulRoute: "power",
			Classifier: PolicyClassifierConfig{Route: "classifier"},
			DataPolicy: PolicyDataPolicyConfig{ContentForwardingAcknowledged: true, AllowProviderRetention: true},
		}},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != providerEndpointModels {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{
			map[string]any{"id": "classifier", "object": "model", "supported_endpoints": []string{providerEndpointResponses}},
		}})
	}))
	defer upstream.Close()

	for _, tc := range []struct {
		name          string
		allowedModels []string
	}{
		{name: "allowed model scope", allowedModels: []string{"semantic"}},
		{name: "unscoped serve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := []Option{
				WithCopilotBaseURL(upstream.URL),
				WithProvidersConfig(cfg),
				WithDeferredDynamicProviderModelValidation(true),
				WithPolicyRoutingMode(PolicyRoutingModeObserve),
			}
			if len(tc.allowedModels) > 0 {
				options = append(options, WithAllowedModels(tc.allowedModels...))
			}
			h, err := NewProxyHandler(
				auth.NewTestAuthenticator("fixture-token"),
				nil,
				options...,
			)
			if err != nil {
				t.Fatalf("NewProxyHandler() error = %v", err)
			}
			t.Cleanup(h.BeginShutdown)
			if err := h.ValidateDynamicProviderModels(t.Context()); err != nil {
				t.Fatalf("ValidateDynamicProviderModels() rejected inactive powerful route: %v", err)
			}
		})
	}
}

func TestPolicyCopilotExplicitTargetsForceDynamicValidation(t *testing.T) {
	upstream := newCopilotResponsesPolicyUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
	cfg := directCopilotResponsesPolicyConfig(policyConfigModeEnforce)
	cfg.Providers[0].IncludeModels = nil
	cfg.Providers[0].ExcludeModels = nil
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture-token"),
		nil,
		WithCopilotBaseURL(upstream.server.URL),
		WithProvidersConfig(cfg),
		WithAllowedModels("gpt-5.6-semantic"),
		WithDeferredDynamicProviderModelValidation(true),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	defer h.BeginShutdown()
	if !h.DynamicProviderValidationPending() {
		t.Fatal("direct Copilot policy targets did not require dynamic validation")
	}
	if err := h.ValidateDynamicProviderModels(t.Context()); err != nil {
		t.Fatalf("ValidateDynamicProviderModels() error = %v", err)
	}
	modelRequests, _, _, _ := upstream.snapshot()
	if modelRequests != 1 {
		t.Fatalf("model discovery requests = %d, want 1", modelRequests)
	}
}

func TestPolicyCopilotSynchronousAllowedModelValidationUsesLocalSetup(t *testing.T) {
	upstream := newCopilotResponsesPolicyUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture-token"),
		nil,
		WithCopilotBaseURL(upstream.server.URL),
		WithProvidersConfig(directCopilotResponsesPolicyConfig(policyConfigModeEnforce)),
		WithAllowedModels("gpt-5.6-semantic"),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	defer h.BeginShutdown()
	modelRequests, _, _, _ := upstream.snapshot()
	if modelRequests != 1 {
		t.Fatalf("model discovery requests = %d, want 1", modelRequests)
	}
}

func TestPolicyResponsesContractRejectsBeforeClassifierDispatch(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value any
	}{
		{name: "stop", field: "stop", value: []string{"END"}},
		{name: "multiple choices", field: "n", value: 2},
		{name: "invalid stream", field: "stream", value: "yes"},
		{name: "unknown field", field: "unsupported_field", value: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			upstream := newCopilotResponsesPolicyUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
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
			defer h.BeginShutdown()
			if err := h.InitializePolicyRouting(t.Context()); err != nil {
				t.Fatalf("InitializePolicyRouting() error = %v", err)
			}
			_, before, _, _ := upstream.snapshot()
			payload := map[string]any{
				"model":                 "gpt-5.6-semantic",
				"messages":              []any{map[string]any{"role": "user", "content": "hello"}},
				"max_completion_tokens": 256,
				tc.field:                tc.value,
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, providerEndpointChatCompletions, strings.NewReader(string(body))))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
			}
			_, after, terminalModels, _ := upstream.snapshot()
			if after != before || len(terminalModels) != 0 {
				t.Fatalf("upstream requests changed: classifiers %d->%d terminals=%v", before, after, terminalModels)
			}
		})
	}
}

func TestPolicyCopilotSynchronousValidationSkipsInactiveTierRoutes(t *testing.T) {
	classifierNoStore := true
	cfg := ProvidersConfig{
		SchemaVersion: 2,
		Providers: []ProviderConfig{
			{ID: "copilot", Type: "copilot", Default: true, TrustDomain: "org", IncludeModels: []string{"classifier", "power"}},
			{ID: "local", Type: "openai-compatible", BaseURL: "https://local.example.test/v1", AuthType: "none", ModelDiscovery: "static", TrustDomain: "org", ClassifierNoStoreSupported: &classifierNoStore},
		},
		ModelRoutes: []ModelRouteConfig{
			{ID: "light", Exposure: "internal", Endpoints: []string{providerEndpointResponses}, Targets: []ModelRouteTargetConfig{{ID: "light", Provider: "local", UpstreamModel: "light"}}},
			{ID: "power", Exposure: "internal", Endpoints: []string{providerEndpointResponses}, Targets: []ModelRouteTargetConfig{{ID: "power", Provider: "copilot", UpstreamModel: "power"}}},
			{ID: "classifier", Exposure: "internal", InternalPurpose: modelRouteInternalPurposePolicyClassifier, Endpoints: []string{providerEndpointResponses}, Targets: []ModelRouteTargetConfig{{ID: "classifier", Provider: "copilot", UpstreamModel: "classifier"}}},
		},
		PolicyProfiles: []PolicyProfileConfig{{
			ID: "policy", PublicID: "semantic", Mode: policyConfigModeEnforce, LightweightRoute: "light", PowerfulRoute: "power",
			Classifier: PolicyClassifierConfig{Route: "classifier"},
			DataPolicy: PolicyDataPolicyConfig{ContentForwardingAcknowledged: true, AllowProviderRetention: true},
		}},
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": []any{
			map[string]any{"id": "classifier", "object": "model", "supported_endpoints": []string{providerEndpointResponses}},
		}})
	}))
	defer upstream.Close()

	for _, tc := range []struct {
		name          string
		allowedModels []string
	}{
		{name: "allowed model scope", allowedModels: []string{"semantic"}},
		{name: "unscoped serve"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			options := []Option{
				WithCopilotBaseURL(upstream.URL),
				WithProvidersConfig(cfg),
				WithPolicyRoutingMode(PolicyRoutingModeObserve),
			}
			if len(tc.allowedModels) > 0 {
				options = append(options, WithAllowedModels(tc.allowedModels...))
			}
			h, err := NewProxyHandler(
				auth.NewTestAuthenticator("fixture-token"),
				nil,
				options...,
			)
			if err != nil {
				t.Fatalf("NewProxyHandler() rejected inactive powerful route: %v", err)
			}
			t.Cleanup(h.BeginShutdown)
		})
	}
}

func TestReadPolicyClassifierUsageForResponsesShape(t *testing.T) {
	body := []byte(`{"status":"completed","output":[{"type":"unknown"}],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18,"input_tokens_details":{"cached_tokens":3},"output_tokens_details":{"reasoning_tokens":5}}}`)
	usage := readPolicyClassifierUsageForEndpoint(body, providerEndpointResponses)
	want := policyStatsTokenUsage{InputTokens: 11, OutputTokens: 7, TotalTokens: 18, CachedInputTokens: 3, ReasoningTokens: 5}
	if usage != want {
		t.Fatalf("usage = %+v, want %+v", usage, want)
	}
}

func TestPolicyCopilotInternalTargetsAreNotPublicModels(t *testing.T) {
	upstream := newCopilotResponsesPolicyUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
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
	defer h.BeginShutdown()

	modelsRecorder := httptest.NewRecorder()
	h.HandleModels(modelsRecorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if modelsRecorder.Code != http.StatusOK {
		t.Fatalf("models status = %d, body = %s", modelsRecorder.Code, modelsRecorder.Body.String())
	}
	var catalog struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.Unmarshal(modelsRecorder.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	ids := make(map[string]bool, len(catalog.Data))
	for _, model := range catalog.Data {
		ids[model.ID] = true
	}
	if !ids["gpt-5.6-semantic"] || ids["gpt-5.6-luna"] || ids["gpt-5.6-sol"] {
		t.Fatalf("public model ids = %v", ids)
	}

	for _, model := range []string{"gpt-5.6-luna", "gpt-5.6-sol"} {
		recorder := httptest.NewRecorder()
		body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"hello"}]}`, model)
		h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, providerEndpointChatCompletions, strings.NewReader(body)))
		if recorder.Code != http.StatusBadRequest {
			t.Fatalf("direct %s status = %d, body = %s", model, recorder.Code, recorder.Body.String())
		}
	}
}
