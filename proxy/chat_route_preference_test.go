package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/auth"
)

func copilotPreferencePublicConfig() ProvidersConfig {
	return ProvidersConfig{
		SchemaVersion: 2, StateBindings: &StateBindingsConfig{Mode: "memory"},
		Providers: []ProviderConfig{{ID: "copilot", Type: "copilot"}},
		ModelRoutes: []ModelRouteConfig{{
			ID: "mai-route", PublicID: "public-mai",
			Endpoints: []string{providerEndpointChatCompletions, providerEndpointResponses},
			Targets:   []ModelRouteTargetConfig{{ID: "primary", Provider: "copilot", UpstreamModel: "gpt-5.6-luna"}},
		}},
	}
}

func TestChatRouteCopilotCanonicalRefreshPreservesAdmittedSnapshot(t *testing.T) {
	upstream := newCopilotResponsesPolicyUpstreamWithVendor(t, policyClassifierSignals{}, "Experimental")
	var vendor atomic.Value
	vendor.Store("OpenAI")
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case providerEndpointModels:
			_, _ = fmt.Fprintf(w, `{"data":[{"id":"gpt-5.6-luna","vendor":%q,"supported_endpoints":["/chat/completions","/responses"]}]}`, vendor.Load().(string))
		case providerEndpointChatCompletions:
			_, _ = w.Write([]byte(`{"id":"old","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"old snapshot"},"finish_reason":"stop"}]}`))
		default:
			upstream.server.Config.Handler.ServeHTTP(w, r)
		}
	}))
	t.Cleanup(front.Close)
	h, err := NewProxyHandler(auth.NewTestAuthenticator("fixture-token"), nil,
		WithCopilotBaseURL(front.URL), WithProvidersConfig(copilotPreferencePublicConfig()), WithAllowedModels("public-mai", "public-mai-20251119"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	body := []byte(`{"model":"public-mai","messages":[{"role":"user","content":"ping"}]}`)
	ctx, admitted, before, err := h.withAdmittedExplicitRouteOperation(t.Context(), t.Context(), "public-mai", providerEndpointChatCompletions)
	if err != nil || admitted == nil || before.targets[0].preferResponses {
		t.Fatalf("initial admission: operation=%v err=%v", admitted, err)
	}
	const alias = "public-mai-20251119"
	aliasCtx, _, _, err := h.withAdmittedExplicitRouteOperation(t.Context(), t.Context(), alias, providerEndpointResponses)
	if err != nil {
		t.Fatal(err)
	}
	vendor.Store("Experimental")
	rec := httptest.NewRecorder()
	h.HandleModels(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if rec.Code != http.StatusOK || strings.Contains(rec.Body.String(), "gpt-5.6-luna") {
		t.Fatalf("catalog status=%d body=%s", rec.Code, rec.Body.String())
	}
	after, _ := h.providerSetup().lookupTerminalRoute("mai-route")
	if after == before || !after.targets[0].preferResponses || before.targets[0].preferResponses {
		t.Fatal("canonical refresh did not publish an independent preference snapshot")
	}
	aliasBody := []byte(fmt.Sprintf(`{"model":%q,"input":[{"role":"user","content":"ping"}],"store":false}`, alias))
	aliasResponse, err := h.postResponsesWithHeadersForModel(aliasCtx, aliasBody, nil, alias)
	if err != nil {
		t.Fatalf("admitted alias rejected after refresh: %v", err)
	}
	_ = aliasResponse.Body.Close()
	if aliasResponse.StatusCode != http.StatusOK {
		t.Fatalf("admitted alias status=%d", aliasResponse.StatusCode)
	}
	ctx, reused, captured, err := h.withChatExecutionRoute(ctx, ctx, "public-mai", body, false)
	if err != nil || reused != admitted || captured != before {
		t.Fatalf("admitted operation lost its snapshot: reused=%v captured=%v err=%v", reused == admitted, captured == before, err)
	}
	result, err := h.executeRoutedChatCompletions(ctx, body, chatCompletionsMode{}, chatExecutionOptions{}, "public-mai")
	if err != nil {
		t.Fatal(err)
	}
	if result.Response == nil || result.Response.StatusCode != http.StatusOK || result.Backend != chatBackendNativeChat {
		t.Fatalf("old operation result=%+v", result)
	}
	_ = result.Response.Body.Close()
	fresh := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(fresh, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body))))
	if fresh.Code != http.StatusOK || !strings.Contains(fresh.Body.String(), "answered by Luna") {
		t.Fatalf("fresh request status=%d body=%s", fresh.Code, fresh.Body.String())
	}
	secondCtx, second, _, err := h.withAdmittedExplicitRouteOperation(t.Context(), t.Context(), "public-mai", providerEndpointChatCompletions)
	if err != nil {
		t.Fatal(err)
	}
	vendor.Store("OpenAI")
	if err := h.ValidateDynamicProviderModels(t.Context()); err != nil {
		t.Fatal(err)
	}
	current, _ := h.providerSetup().lookupTerminalRoute("mai-route")
	if current.targets[0].preferResponses {
		t.Fatal("explicit validation did not clear the changed preference")
	}
	secondCtx, reused, captured, err = h.withChatExecutionRoute(secondCtx, secondCtx, "public-mai", body, false)
	if err != nil || reused != second || captured != after || !captured.targets[0].preferResponses {
		t.Fatalf("validation invalidated admitted Responses operation: err=%v", err)
	}
	result, err = h.executeRoutedChatCompletions(secondCtx, body, chatCompletionsMode{}, chatExecutionOptions{}, "public-mai")
	if err != nil {
		t.Fatal(err)
	}
	if result.Backend != chatBackendResponses || result.Completion == nil {
		t.Fatalf("admitted Responses operation result=%+v", result)
	}
}

func TestChatRouteFailedCopilotRefreshLeavesPreferenceUnchanged(t *testing.T) {
	upstream := newCopilotResponsesPolicyUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(auth.NewTestAuthenticator("fixture-token"), nil,
		WithCopilotBaseURL(upstream.server.URL), WithProvidersConfig(copilotPreferencePublicConfig()))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	setup := h.providerSetup()
	before, _ := setup.lookupTerminalRoute("mai-route")
	catalog, err := decodeProviderModelsFromBody(setup.providerByID("copilot"), []byte(`{"data":[
		{"id":"gpt-5.6-luna","vendor":"Experimental","supported_endpoints":["/chat/completions","/responses"]},
		{"id":"public-mai","supported_endpoints":["/chat/completions"]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.replaceInstalledProviderModels(setup, map[string][]providerModel{"copilot": catalog}); err == nil {
		t.Fatal("expected the public model collision to reject the catalog")
	}
	after, _ := setup.lookupTerminalRoute("mai-route")
	if after != before || after.targets[0].preferResponses {
		t.Fatal("failed catalog replacement changed the installed preference")
	}
}

func TestChatRouteCopilotExplicitGeminiUsesResponses(t *testing.T) {
	for _, responsesOnly := range []bool{false, true} {
		for _, action := range []string{"generateContent", "streamGenerateContent", "countTokens"} {
			t.Run(fmt.Sprintf("%s/responsesOnly=%t", action, responsesOnly), func(t *testing.T) {
				upstream := newCopilotResponsesPolicyUpstreamWithVendor(t, policyClassifierSignals{}, "Experimental")
				cfg := copilotPreferencePublicConfig()
				if responsesOnly {
					cfg.ModelRoutes[0].Endpoints = []string{providerEndpointResponses}
				}
				h, err := NewProxyHandler(auth.NewTestAuthenticator("fixture-token"), nil,
					WithCopilotBaseURL(upstream.server.URL), WithProvidersConfig(cfg))
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(h.BeginShutdown)
				rec := httptest.NewRecorder()
				h.HandleGeminiModels(rec, httptest.NewRequest(http.MethodPost, "/v1beta/models/public-mai:"+action,
					strings.NewReader(`{"contents":[{"role":"user","parts":[{"text":"ping"}]}]}`)))
				if rec.Code != http.StatusOK {
					t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
				}
				if action != "countTokens" && !strings.Contains(rec.Body.String(), "answered by Luna") {
					t.Fatalf("unexpected generation: %s", rec.Body.String())
				}
				if action == "countTokens" && !strings.Contains(rec.Body.String(), `"totalTokens":20`) {
					t.Fatalf("unexpected count: %s", rec.Body.String())
				}
				_, _, terminals, paths := upstream.snapshot()
				if len(terminals) != 1 || paths[len(paths)-1] != providerEndpointResponses {
					t.Fatalf("terminals=%v paths=%v", terminals, paths)
				}
			})
		}
	}
}

func TestChatRouteCopilotDropStopBeforeResponsesTranslation(t *testing.T) {
	for _, surface := range []string{"Chat", "Anthropic", "policy"} {
		t.Run(surface, func(t *testing.T) {
			upstream := newCopilotResponsesPolicyUpstreamWithVendor(t, policyClassifierSignals{
				TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow,
			}, "Microsoft")
			cfg := copilotPreferencePublicConfig()
			if surface == "policy" {
				cfg = directCopilotResponsesPolicyConfig(policyConfigModeEnforce)
			}
			drop := true
			for i := range cfg.ModelRoutes {
				cfg.ModelRoutes[i].DropStopSequences = &drop
				cfg.ModelRoutes[i].Endpoints = []string{providerEndpointChatCompletions, providerEndpointResponses}
			}
			h, err := NewProxyHandler(auth.NewTestAuthenticator("fixture-token"), nil,
				WithCopilotBaseURL(upstream.server.URL), WithProvidersConfig(cfg))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(h.BeginShutdown)
			if err := h.InitializePolicyRouting(t.Context()); err != nil {
				t.Fatal(err)
			}
			rec := httptest.NewRecorder()
			if surface == "Anthropic" {
				h.HandleAnthropicMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages",
					strings.NewReader(`{"model":"public-mai","max_tokens":32,"messages":[{"role":"user","content":"ping"}],"stop_sequences":["done"]}`)))
			} else {
				model := "public-mai"
				if surface == "policy" {
					model = "gpt-5.6-semantic"
				}
				h.HandleOpenAIChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
					strings.NewReader(fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"ping"}],"stop":["done"]}`, model))))
			}
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			for _, body := range upstream.responsesBodies() {
				var request map[string]json.RawMessage
				if err := json.Unmarshal([]byte(body), &request); err != nil {
					t.Fatal(err)
				}
				if _, ok := request["stop"]; ok {
					t.Fatalf("stop leaked to Responses: %s", body)
				}
			}
		})
	}
}

func TestPolicyCopilotDualEndpointLocalReplayStaysOnResponses(t *testing.T) {
	for _, mode := range []string{policyConfigModeOff, policyConfigModeObserve} {
		t.Run(mode, func(t *testing.T) {
			upstream := newCopilotResponsesPolicyUpstreamWithVendor(t, policyClassifierSignals{
				TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow,
			}, "Experimental")
			cfg := directCopilotResponsesPolicyConfig(mode)
			for i := range cfg.ModelRoutes {
				cfg.ModelRoutes[i].Endpoints = []string{providerEndpointChatCompletions, providerEndpointResponses}
			}
			h, err := NewProxyHandler(auth.NewTestAuthenticator("fixture-token"), nil,
				WithCopilotBaseURL(upstream.server.URL), WithProvidersConfig(cfg))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(h.BeginShutdown)
			if err := h.InitializePolicyRouting(t.Context()); err != nil {
				t.Fatal(err)
			}
			initial := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(initial, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{
				"model":"gpt-5.6-semantic","messages":[{"role":"user","content":"Look up main."}],
				"max_completion_tokens":256,"parallel_tool_calls":false,
				"tools":[{"type":"function","function":{"name":"lookup_symbol","parameters":{"type":"object","properties":{"symbol":{"type":"string"}}}}}],
				"tool_choice":{"type":"function","function":{"name":"lookup_symbol"}}
			}`)))
			if initial.Code != http.StatusOK {
				t.Fatalf("initial status=%d body=%s", initial.Code, initial.Body.String())
			}
			var response struct {
				Choices []struct {
					Message json.RawMessage `json:"message"`
				} `json:"choices"`
			}
			if err := json.Unmarshal(initial.Body.Bytes(), &response); err != nil || len(response.Choices) != 1 {
				t.Fatalf("initial response=%s err=%v", initial.Body.String(), err)
			}
			var assistant struct {
				ToolCalls []struct {
					ID string `json:"id"`
				} `json:"tool_calls"`
			}
			if err := json.Unmarshal(response.Choices[0].Message, &assistant); err != nil || len(assistant.ToolCalls) != 1 {
				t.Fatalf("assistant=%s err=%v", response.Choices[0].Message, err)
			}
			body, err := json.Marshal(map[string]any{
				"model": "gpt-5.6-semantic", "max_completion_tokens": 256,
				"messages": []any{
					map[string]any{"role": "user", "content": "Look up main."},
					response.Choices[0].Message,
					map[string]any{"role": "tool", "tool_call_id": assistant.ToolCalls[0].ID, "content": "main is a function"},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := h.WaitLifecycleWorkers(t.Context()); err != nil {
				t.Fatal(err)
			}
			// A refreshed catalog can prefer native Chat for new work while this
			// in-flight tool turn still requires the locally saved Responses items.
			setup := h.providerSetup()
			catalog, err := decodeProviderModelsFromBody(setup.providerByID("copilot"), []byte(`{"data":[
				{"id":"gpt-5.6-luna","vendor":"OpenAI","supported_endpoints":["/chat/completions","/responses"]},
				{"id":"gpt-5.6-sol","vendor":"OpenAI","supported_endpoints":["/chat/completions","/responses"]}
			]}`))
			if err != nil {
				t.Fatal(err)
			}
			if err := h.replaceInstalledProviderModels(setup, map[string][]providerModel{"copilot": catalog}); err != nil {
				t.Fatal(err)
			}
			for _, routeID := range []string{"luna-route", "sol-route"} {
				route, _ := setup.lookupTerminalRoute(routeID)
				if endpoint, _, err := explicitChatExecutionEndpoint(route, nil, false); err != nil || endpoint != providerEndpointChatCompletions {
					t.Fatalf("new work on %s still prefers Responses: endpoint=%s err=%v", routeID, endpoint, err)
				}
			}
			continuation := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(continuation, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body))))
			if continuation.Code != http.StatusOK || !strings.Contains(continuation.Body.String(), "tool result accepted") {
				t.Fatalf("continuation status=%d body=%s", continuation.Code, continuation.Body.String())
			}
			bodies := upstream.responsesBodies()
			if len(bodies) < 2 {
				t.Fatalf("missing Responses continuation: %v", bodies)
			}
			type replayRequest struct {
				Input []struct {
					Type   string `json:"type"`
					CallID string `json:"call_id"`
				} `json:"input"`
			}
			var restored replayRequest
			var replayBody string
			// Observe-mode classifier traffic can arrive asynchronously.
			for _, body := range bodies {
				var request replayRequest
				if err := json.Unmarshal([]byte(body), &request); err != nil {
					t.Fatal(err)
				}
				for _, item := range request.Input {
					if item.Type == "function_call_output" {
						restored, replayBody = request, body
						break
					}
				}
			}
			if strings.Contains(replayBody, assistant.ToolCalls[0].ID) {
				t.Fatalf("proxy replay ID reached upstream: %s", replayBody)
			}
			var restoredCall, restoredResult bool
			for _, item := range restored.Input {
				switch item.Type {
				case "function_call", "function_call_output":
					if item.CallID != "call-lookup" {
						t.Fatalf("upstream call ID was not restored: %s", replayBody)
					}
					restoredCall = restoredCall || item.Type == "function_call"
					restoredResult = restoredResult || item.Type == "function_call_output"
				}
			}
			if !restoredCall || !restoredResult {
				t.Fatalf("saved tool items were not replayed: %s", replayBody)
			}
			_, _, terminals, paths := upstream.snapshot()
			if len(terminals) != 2 {
				t.Fatalf("terminals=%v paths=%v", terminals, paths)
			}
			for _, path := range paths {
				if path == providerEndpointChatCompletions {
					t.Fatalf("local replay dispatched to native Chat: %v", paths)
				}
			}
		})
	}
}

func TestChatRouteGeminiReplayErrorsPreserveClientStatus(t *testing.T) {
	for _, action := range []string{"generateContent", "streamGenerateContent", "countTokens"} {
		t.Run(action, func(t *testing.T) {
			upstream := newCopilotResponsesPolicyUpstreamWithVendor(t, policyClassifierSignals{}, "Experimental")
			cfg := copilotPreferencePublicConfig()
			cfg.ModelRoutes[0].Endpoints = []string{providerEndpointChatCompletions}
			h, err := NewProxyHandler(auth.NewTestAuthenticator("fixture-token"), nil,
				WithCopilotBaseURL(upstream.server.URL), WithProvidersConfig(cfg))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(h.BeginShutdown)
			callID := responsesChatReplayCallIDPrefix + strings.Repeat("A", 22)
			body := fmt.Sprintf(`{"contents":[
				{"role":"model","parts":[{"functionCall":{"id":%q,"name":"lookup","args":{}}}]},
				{"role":"user","parts":[{"functionResponse":{"id":%q,"name":"lookup","response":{"ok":true}}}]}
			]}`, callID, callID)
			rec := httptest.NewRecorder()
			h.HandleGeminiModels(rec, httptest.NewRequest(http.MethodPost, "/v1beta/models/public-mai:"+action, strings.NewReader(body)))
			var response struct {
				Error struct {
					Code    int    `json:"code"`
					Status  string `json:"status"`
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if rec.Code != http.StatusBadRequest || response.Error.Code != http.StatusBadRequest ||
				response.Error.Status != "INVALID_ARGUMENT" || response.Error.Message != missingResponsesChatReplayError().Message {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
			}
			if rec.Header().Get("X-Vekil-Request-ID") == "" {
				t.Fatal("client rejection lost its admitted operation ID")
			}
			_, _, terminals, paths := upstream.snapshot()
			if len(terminals) != 0 {
				t.Fatalf("client rejection dispatched upstream: %v", paths)
			}
			for _, path := range paths {
				if path != providerEndpointModels {
					t.Fatalf("client rejection dispatched upstream: %v", paths)
				}
			}
		})
	}
}

func TestPolicyCopilotRefreshDuringClassificationKeepsValidatedRoutes(t *testing.T) {
	upstream := newCopilotResponsesPolicyUpstreamWithVendor(t, policyClassifierSignals{
		TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow,
	}, "Experimental")
	var vendor atomic.Value
	vendor.Store("OpenAI")
	var refreshOnClassifier atomic.Bool
	var nativeCalls atomic.Int32
	var h *ProxyHandler
	front := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case providerEndpointModels:
			_, _ = fmt.Fprintf(w, `{"data":[
				{"id":"gpt-5.6-luna","vendor":%q,"supported_endpoints":["/chat/completions","/responses"]},
				{"id":"gpt-5.6-sol","vendor":%q,"supported_endpoints":["/chat/completions","/responses"]}
			]}`, vendor.Load().(string), vendor.Load().(string))
		case providerEndpointChatCompletions:
			nativeCalls.Add(1)
			var body map[string]json.RawMessage
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
				t.Error(err)
			}
			if _, present := body["stop"]; !present {
				t.Error("admitted native Chat request lost its stop field")
			}
			_, _ = w.Write([]byte(`{"id":"old","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"old snapshot"},"finish_reason":"stop"}]}`))
		case providerEndpointResponses:
			if refreshOnClassifier.CompareAndSwap(true, false) {
				vendor.Store("Experimental")
				if err := h.ValidateDynamicProviderModels(r.Context()); err != nil {
					t.Error(err)
					http.Error(w, "catalog refresh failed", http.StatusInternalServerError)
					return
				}
			}
			upstream.server.Config.Handler.ServeHTTP(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(front.Close)
	cfg := directCopilotResponsesPolicyConfig(policyConfigModeEnforce)
	for i := 0; i < 2; i++ {
		cfg.ModelRoutes[i].Endpoints = []string{providerEndpointChatCompletions, providerEndpointResponses}
	}
	cfg.PolicyProfiles[0].Classifier.TimeoutMS = 5000
	var err error
	h, err = NewProxyHandler(auth.NewTestAuthenticator("fixture-token"), nil,
		WithCopilotBaseURL(front.URL), WithProvidersConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"gpt-5.6-semantic","messages":[{"role":"user","content":"Inspect one function."}],"stop":["done"]}`
	refreshOnClassifier.Store(true)
	rec := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "old snapshot") || nativeCalls.Load() != 1 {
		t.Fatalf("admitted policy request changed backend: status=%d native=%d body=%s", rec.Code, nativeCalls.Load(), rec.Body.String())
	}
	_, classified, _, _ := upstream.snapshot()
	if classified != 2 {
		t.Fatalf("classifier sends=%d, want preflight plus one classification", classified)
	}
	for _, routeID := range []string{"luna-route", "sol-route"} {
		route, _ := h.providerSetup().lookupTerminalRoute(routeID)
		if route == nil || !route.targets[0].preferResponses {
			t.Fatalf("refresh did not update %s", routeID)
		}
	}
	fresh := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(fresh, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	_, classifiedAfter, _, _ := upstream.snapshot()
	if fresh.Code != http.StatusBadRequest || classifiedAfter != classified || nativeCalls.Load() != 1 {
		t.Fatalf("new Responses request bypassed preflight: status=%d classifiers=%d body=%s", fresh.Code, classifiedAfter, fresh.Body.String())
	}
}
