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
		WithCopilotBaseURL(front.URL), WithProvidersConfig(copilotPreferencePublicConfig()), WithAllowedModels("public-mai"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	body := []byte(`{"model":"public-mai","messages":[{"role":"user","content":"ping"}]}`)
	ctx, admitted, before, err := h.withAdmittedExplicitRouteOperation(t.Context(), t.Context(), "public-mai", providerEndpointChatCompletions)
	if err != nil || admitted == nil || before.targets[0].preferResponses {
		t.Fatalf("initial admission: operation=%v err=%v", admitted, err)
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
			continuation := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(continuation, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(string(body))))
			if continuation.Code != http.StatusOK || !strings.Contains(continuation.Body.String(), "tool result accepted") {
				t.Fatalf("continuation status=%d body=%s", continuation.Code, continuation.Body.String())
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
