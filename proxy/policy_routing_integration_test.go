package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

type policyIntegrationUpstream struct {
	server *httptest.Server

	mu       sync.Mutex
	models   []string
	parallel []json.RawMessage
	requests int

	classifierSignals        policyClassifierSignals
	classifierBlock          <-chan struct{}
	classifierFailureStatus  atomic.Int64
	terminalFailureStatus    atomic.Int64
	terminalStreamFailure    atomic.Bool
	terminalRateLimitFailure atomic.Bool
}

func newPolicyIntegrationUpstream(t *testing.T, signals policyClassifierSignals) *policyIntegrationUpstream {
	t.Helper()
	u := &policyIntegrationUpstream{classifierSignals: signals}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var request struct {
			Model             string          `json:"model"`
			Stream            bool            `json:"stream"`
			ParallelToolCalls json.RawMessage `json:"parallel_tool_calls"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		u.mu.Lock()
		u.requests++
		u.models = append(u.models, request.Model)
		u.parallel = append(u.parallel, append(json.RawMessage(nil), request.ParallelToolCalls...))
		u.mu.Unlock()
		if request.Model == "classifier-model" {
			if status := int(u.classifierFailureStatus.Load()); status != 0 {
				http.Error(w, "classifier unavailable", status)
				return
			}
			if u.classifierBlock != nil {
				select {
				case <-u.classifierBlock:
				case <-r.Context().Done():
					return
				}
			}
			arguments, _ := json.Marshal(u.classifierSignals)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "classifier-response",
				"choices": []any{map[string]any{
					"index": 0,
					"message": map[string]any{
						"role":    "assistant",
						"content": nil,
						"tool_calls": []any{map[string]any{
							"id":   "call-policy",
							"type": "function",
							"function": map[string]any{
								"name":      policyClassifierToolName,
								"arguments": string(arguments),
							},
						}},
					},
				}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 4, "total_tokens": 14},
			})
			return
		}
		w.Header().Set("X-Request-ID", "terminal-provider-region-request")
		if status := int(u.terminalFailureStatus.Load()); status != 0 {
			w.Header().Set("Openai-Model", request.Model)
			w.Header().Set("X-Openai-Model", request.Model)
			w.Header().Set("X-Request-ID", "terminal-request")
			w.Header().Set("X-Azure-Request-ID", "azure-terminal-request")
			w.Header().Set("OpenAI-Processing-Ms", "17")
			w.Header().Set("X-RateLimit-Limit", "100")
			w.Header().Set("X-RateLimit-Remaining", "42")
			w.Header().Set("X-RateLimit-Reset", "123456")
			w.Header().Set("X-RateLimit-Model", "power-model")
			w.Header().Set("RateLimit-Policy", "power-provider")
			w.Header().Set("X-Vekil-Internal-Route", "power-route")
			http.Error(w, fmt.Sprintf("terminal unavailable for %s via power-route/power-provider", request.Model), status)
			return
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("Openai-Model", request.Model)
			w.Header().Set("X-Request-ID", "terminal-stream-request")
			w.Header().Set("X-Azure-Request-ID", "azure-terminal-stream-request")
			w.Header().Set("OpenAI-Processing-Ms", "19")
			if u.terminalRateLimitFailure.Load() {
				_, _ = fmt.Fprint(w, "event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\",\"message\":\"power-model quota failed via power-route/power-provider\"}}\n\n")
				return
			}
			if u.terminalStreamFailure.Load() {
				_, _ = fmt.Fprint(w, "event: error\ndata: {\"error\":{\"type\":\"server_error\",\"code\":\"power-route\",\"message\":\"power-model failed via power-provider\"}}\n\n")
				return
			}
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"terminal-stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n", request.Model)
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"terminal-stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", request.Model)
			_, _ = fmt.Fprint(w, "data: {\"id\":\"terminal-stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"ignored\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\ndata: [DONE]\n\n")
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Openai-Model", request.Model)
		w.Header().Set("X-Azure-Request-ID", "azure-terminal-success")
		w.Header().Set("OpenAI-Processing-Ms", "21")
		w.Header().Set("X-Vekil-Internal-Route", "terminal-route")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":      "terminal-response",
			"object":  "chat.completion",
			"created": 1,
			"model":   request.Model,
			"choices": []any{map[string]any{
				"index":         0,
				"message":       map[string]any{"role": "assistant", "content": "ok"},
				"finish_reason": "stop",
			}},
			"usage": map[string]any{"prompt_tokens": 2, "completion_tokens": 1, "total_tokens": 3},
		})
	}))
	t.Cleanup(u.server.Close)
	return u
}

func (u *policyIntegrationUpstream) snapshot() (int, []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.requests, append([]string(nil), u.models...)
}

func (u *policyIntegrationUpstream) parallelToolCallsSnapshot() []json.RawMessage {
	u.mu.Lock()
	defer u.mu.Unlock()
	parallel := make([]json.RawMessage, len(u.parallel))
	for index := range u.parallel {
		parallel[index] = append(json.RawMessage(nil), u.parallel[index]...)
	}
	return parallel
}

func policyIntegrationConfig(lightURL, powerfulURL, profileMode string) ProvidersConfig {
	trueValue := true
	parallel := true
	return ProvidersConfig{
		SchemaVersion: ProvidersConfigSchemaVersion2,
		Providers: []ProviderConfig{
			{ID: "light-provider", Type: string(providerTypeOpenAICompatible), BaseURL: lightURL, AuthType: string(providerAuthTypeNone), TrustDomain: "org-ai", ClassifierNoStoreSupported: &trueValue},
			{ID: "power-provider", Type: string(providerTypeOpenAICompatible), BaseURL: powerfulURL, AuthType: string(providerAuthTypeNone), TrustDomain: "org-ai"},
		},
		ModelRoutes: []ModelRouteConfig{
			{ID: "light-route", Exposure: modelRouteExposureInternal, Name: "Light", Endpoints: []string{providerEndpointChatCompletions}, ParallelToolCalls: &parallel, Targets: []ModelRouteTargetConfig{{ID: "light", Provider: "light-provider", UpstreamModel: "light-model"}}},
			{ID: "power-route", Exposure: modelRouteExposureInternal, Name: "Power", Endpoints: []string{providerEndpointChatCompletions}, ParallelToolCalls: &parallel, Targets: []ModelRouteTargetConfig{{ID: "power", Provider: "power-provider", UpstreamModel: "power-model"}}},
			{ID: "classifier-route", Exposure: modelRouteExposureInternal, InternalPurpose: modelRouteInternalPurposePolicyClassifier, Name: "Classifier", Endpoints: []string{providerEndpointChatCompletions}, Targets: []ModelRouteTargetConfig{{ID: "classifier", Provider: "light-provider", UpstreamModel: "classifier-model"}}},
		},
		PolicyProfiles: []PolicyProfileConfig{{
			ID: "coding-policy", PublicID: "coding-economy", Mode: profileMode,
			LightweightRoute: "light-route", PowerfulRoute: "power-route",
			BaselineTier: policyConfigTierLightweight, ClassifierUnavailableTier: policyConfigTierLightweight, ClassifierUncertainTier: policyConfigTierPowerful,
			Classifier: PolicyClassifierConfig{Route: "classifier-route", Profile: policyConfigClassifierProfileCodingAgentV1, TimeoutMS: 1000, MaxCompletionTokens: 64, MaxRequestBytes: 4096, RecentTurns: 2, MaxConcurrency: 2, ObserveSampleRate: 1},
			DataPolicy: PolicyDataPolicyConfig{ContentForwardingAcknowledged: true},
		}},
	}
}

func TestPolicyRoutingAllowedModelsScopesProfilesAndPreflightThroughAliases(t *testing.T) {
	selected := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
	unrelated := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelHigh})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})

	cfg := policyIntegrationConfig(selected.server.URL, powerful.server.URL, policyConfigModeEnforce)
	cfg.PolicyProfiles[0].PublicID = "coding-economy-20260717"
	trueValue := true
	cfg.Providers = append(cfg.Providers, ProviderConfig{
		ID:                         "unrelated-classifier-provider",
		Type:                       string(providerTypeOpenAICompatible),
		BaseURL:                    unrelated.server.URL,
		AuthType:                   string(providerAuthTypeNone),
		TrustDomain:                "org-ai",
		ClassifierNoStoreSupported: &trueValue,
	})
	cfg.ModelRoutes = append(cfg.ModelRoutes, ModelRouteConfig{
		ID:              "unrelated-classifier-route",
		Exposure:        modelRouteExposureInternal,
		InternalPurpose: modelRouteInternalPurposePolicyClassifier,
		Name:            "Unrelated Classifier",
		Endpoints:       []string{providerEndpointChatCompletions},
		Targets: []ModelRouteTargetConfig{{
			ID:            "unrelated-classifier",
			Provider:      "unrelated-classifier-provider",
			UpstreamModel: "classifier-model",
		}},
	})
	unrelatedProfile := cfg.PolicyProfiles[0]
	unrelatedProfile.ID = "unrelated-policy"
	unrelatedProfile.PublicID = "unrelated-economy"
	unrelatedProfile.Name = "Unrelated Economy"
	unrelatedProfile.Classifier.Route = "unrelated-classifier-route"
	cfg.PolicyProfiles = append(cfg.PolicyProfiles, unrelatedProfile)

	h, err := NewProxyHandler(nil, nil,
		WithProvidersConfig(cfg),
		WithAllowedModels("coding-economy"),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatal(err)
	}
	controller, ok := h.policyRoutingController.(*chatPolicyRoutingController)
	if !ok || controller == nil {
		t.Fatalf("policy controller = %T, want scoped controller", h.policyRoutingController)
	}
	if len(controller.ordered) != 1 || controller.ordered[0].config.ID != "coding-policy" {
		t.Fatalf("scoped profiles = %+v, want coding-policy only", controller.ordered)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatalf("InitializePolicyRouting() error = %v", err)
	}
	selectedRequests, _ := selected.snapshot()
	if selectedRequests != 1 {
		t.Fatalf("selected classifier requests = %d, want 1", selectedRequests)
	}
	unrelatedRequests, _ := unrelated.snapshot()
	if unrelatedRequests != 0 {
		t.Fatalf("unrelated classifier requests = %d, want 0", unrelatedRequests)
	}
}

func TestPolicyRoutingEnforceSelectsPowerfulAndPreservesPublicIdentity(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{
		TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile,
		RiskLevel: policyRiskLevelHigh,
	})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})

	h, err := NewProxyHandler(nil, nil,
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !h.PolicyRoutingPreflightPending() || !h.PolicyRoutingActive() {
		t.Fatal("active enforce profile did not gate startup preflight")
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatalf("InitializePolicyRouting() error = %v", err)
	}

	body := `{"model":"coding-economy","messages":[{"role":"user","content":"plan a multi-file refactor"}]}`
	requestCtx, _ := WithRequestSummary(t.Context())
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)).WithContext(requestCtx))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Model != "coding-economy" {
		t.Fatalf("response model = %q, want policy public id", response.Model)
	}
	if got := recorder.Header().Get("X-Vekil-Request-ID"); got == "" {
		t.Fatal("missing proxy operation id")
	}
	lightRequests, lightModels := light.snapshot()
	powerRequests, powerModels := powerful.snapshot()
	if lightRequests != 2 || strings.Join(lightModels, ",") != "classifier-model,classifier-model" {
		t.Fatalf("light/classifier requests = %d %v, want two classifier sends (preflight + request)", lightRequests, lightModels)
	}
	if powerRequests != 1 || strings.Join(powerModels, ",") != "power-model" {
		t.Fatalf("power requests = %d %v, want one terminal send", powerRequests, powerModels)
	}

	policyStats := h.policyRoutingController.(*chatPolicyRoutingController).PolicyStatsSnapshot()
	if len(policyStats.Profiles) != 1 || policyStats.Profiles[0].Profile != "coding-economy" {
		t.Fatalf("policy stats exposed internal profile id: %+v", policyStats.Profiles)
	}
	if policyStats.Profiles[0].Totals.ActualTiers.Powerful != 1 {
		t.Fatalf("policy stats = %+v", policyStats)
	}
	if got := policyStats.Profiles[0].Totals.PhysicalClassifierSends; got != 2 {
		t.Fatalf("classifier sends = %d, want preflight + request", got)
	}
	if got := policyStats.Profiles[0].Totals.ClassifierUsage.TotalTokens; got != 28 {
		t.Fatalf("classifier tokens = %d, want 28", got)
	}
}

func TestPolicyRoutingGlobalOffUsesBaselineWithoutClassifier(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelHigh})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, nil,
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
		WithPolicyRoutingMode(PolicyRoutingModeOff),
	)
	if err != nil {
		t.Fatal(err)
	}
	if h.PolicyRoutingActive() || h.PolicyRoutingPreflightPending() {
		t.Fatal("global off unexpectedly activated classifier preflight")
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	if stats := h.responsesChatReplayStore().Stats(); stats.Groups != 0 || stats.Calls != 0 || stats.TotalBytes != 0 {
		t.Fatalf("classifier preflight polluted client replay state: %+v", stats)
	}
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"hello"}]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	lightRequests, models := light.snapshot()
	if lightRequests != 1 || strings.Join(models, ",") != "light-model" {
		t.Fatalf("baseline requests = %d %v", lightRequests, models)
	}
	if powerRequests, _ := powerful.snapshot(); powerRequests != 0 {
		t.Fatalf("powerful requests = %d, want zero", powerRequests)
	}
}

func TestPolicyRoutingUsesCopilotResponsesTargetsInProcess(t *testing.T) {
	streamFixture := readResponsesChatStreamFixture(t, "stream_text.sse")
	var mu sync.Mutex
	var paths []string
	var models []string
	var authorizations []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var request map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var model string
		_ = json.Unmarshal(request["model"], &model)
		var stream bool
		_ = json.Unmarshal(request["stream"], &stream)
		tools := string(request["tools"])
		mu.Lock()
		paths = append(paths, r.URL.Path)
		models = append(models, model)
		authorizations = append(authorizations, r.Header.Get("Authorization"))
		mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Request-ID", "terminal-request-secret")
		w.Header().Set("X-Azure-Request-ID", "terminal-azure-secret")
		w.Header().Set("OpenAI-Request-ID", "terminal-openai-secret")
		w.Header().Set("Openai-Model", model)
		w.Header().Set("RateLimit-Remaining", "7")
		if strings.Contains(tools, policyClassifierToolName) {
			if _, exists := request["store"]; exists {
				http.Error(w, "classifier store field was not removed", http.StatusUnprocessableEntity)
				return
			}
			arguments, _ := json.Marshal(policyClassifierSignals{
				TurnType:  policyTurnTypePlanning,
				CodeScope: policyCodeScopeMultiFile,
				RiskLevel: policyRiskLevelHigh,
			})
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":     "resp-classifier",
				"object": "response",
				"status": "completed",
				"model":  model,
				"output": []any{map[string]any{
					"type":      "function_call",
					"id":        "fc-classifier",
					"call_id":   "call-classifier",
					"name":      policyClassifierToolName,
					"arguments": string(arguments),
					"status":    "completed",
				}},
				"usage": map[string]any{"input_tokens": 10, "output_tokens": 4, "total_tokens": 14},
			})
			return
		}
		if stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(streamFixture)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":     "resp-terminal",
			"object": "response",
			"status": "completed",
			"model":  model,
			"output": []any{map[string]any{
				"type":   "message",
				"id":     "msg-terminal",
				"status": "completed",
				"role":   "assistant",
				"content": []any{map[string]any{
					"type": "output_text",
					"text": "COPILOT_POLICY_OK",
				}},
			}},
			"usage": map[string]any{"input_tokens": 20, "output_tokens": 5, "total_tokens": 25},
		})
	}))
	t.Cleanup(upstream.Close)

	noStore := false
	cfg := policyIntegrationConfig("", "", policyConfigModeEnforce)
	cfg.Providers = []ProviderConfig{{
		ID:                         "copilot",
		Type:                       string(providerTypeCopilot),
		Default:                    true,
		TrustDomain:                "github-copilot",
		ClassifierNoStoreSupported: &noStore,
	}}
	for routeIndex := range cfg.ModelRoutes {
		cfg.ModelRoutes[routeIndex].Endpoints = []string{providerEndpointResponses}
		for targetIndex := range cfg.ModelRoutes[routeIndex].Targets {
			cfg.ModelRoutes[routeIndex].Targets[targetIndex].Provider = "copilot"
		}
	}
	cfg.PolicyProfiles[0].DataPolicy.AllowProviderRetention = true

	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("copilot-test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithCopilotBaseURL(upstream.URL),
		WithProvidersConfig(cfg),
		WithAllowedModels("coding-economy"),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !h.ModelUsesCopilot("coding-economy") {
		t.Fatal("Copilot-backed policy entry did not require authentication")
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"plan a risky multi-file change"}]}`),
	))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "COPILOT_POLICY_OK") {
		t.Fatalf("response body = %s", recorder.Body.String())
	}
	if stats := h.responsesChatReplayStore().Stats(); stats.Groups != 0 || stats.Calls != 0 || stats.TotalBytes != 0 {
		t.Fatalf("classifier request polluted client replay state: %+v", stats)
	}

	anthropicRecorder := httptest.NewRecorder()
	h.HandleAnthropicMessages(anthropicRecorder, httptest.NewRequest(
		http.MethodPost,
		providerEndpointMessages,
		strings.NewReader(`{"model":"coding-economy","max_tokens":64,"messages":[{"role":"user","content":"plan another risky multi-file change"}]}`),
	))
	if anthropicRecorder.Code != http.StatusOK {
		t.Fatalf("Anthropic status = %d body=%s", anthropicRecorder.Code, anthropicRecorder.Body.String())
	}
	for _, name := range []string{"X-Request-ID", "X-Azure-Request-ID", "OpenAI-Request-ID"} {
		if value := anthropicRecorder.Header().Get(name); value != "" {
			t.Fatalf("policy Anthropic response leaked %s=%q", name, value)
		}
	}
	if got := anthropicRecorder.Header().Get("RateLimit-Remaining"); got != "7" {
		t.Fatalf("RateLimit-Remaining = %q, want 7", got)
	}
	if got := anthropicRecorder.Header().Get("Openai-Model"); got != "coding-economy" {
		t.Fatalf("Openai-Model = %q, want policy public id", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if got := strings.Join(paths, ","); got != "/responses,/responses,/responses,/responses,/responses" {
		t.Fatalf("Copilot policy paths = %q, want five Responses sends", got)
	}
	if got := strings.Join(models, ","); got != "classifier-model,classifier-model,power-model,classifier-model,power-model" {
		t.Fatalf("Copilot policy models = %q", got)
	}
	for index, authorization := range authorizations {
		if authorization != "Bearer copilot-test-token" {
			t.Fatalf("Copilot Authorization[%d] = %q", index, authorization)
		}
	}
}

func TestPreparePolicyClassifierResponsesBodyHonorsNoStoreCapability(t *testing.T) {
	trueValue := true
	falseValue := false
	for _, tc := range []struct {
		name       string
		capability *bool
		wantStore  bool
	}{
		{name: "supported", capability: &trueValue, wantStore: true},
		{name: "unsupported", capability: &falseValue},
		{name: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := preparePolicyClassifierResponsesBody(
				[]byte(`{"model":"classifier","store":false,"input":"classify"}`),
				targetBinding{provider: &providerRuntime{classifierNoStoreSupported: tc.capability}},
			)
			if err != nil {
				t.Fatal(err)
			}
			var payload map[string]json.RawMessage
			if err := json.Unmarshal(body, &payload); err != nil {
				t.Fatal(err)
			}
			_, hasStore := payload["store"]
			if hasStore != tc.wantStore {
				t.Fatalf("store present = %v, want %v; body=%s", hasStore, tc.wantStore, body)
			}
		})
	}
}

func TestPolicyRoutingEnforceCancellationAuthorizesNoTerminalSend(t *testing.T) {
	block := make(chan struct{})
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelHigh})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, nil,
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	light.classifierBlock = block
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	recorder := httptest.NewRecorder()
	go func() {
		defer close(done)
		h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewBufferString(`{"model":"coding-economy","messages":[{"role":"user","content":"plan"}]}`)).WithContext(ctx))
	}()
	deadline := time.Now().Add(time.Second)
	for {
		requests, _ := light.snapshot()
		if requests >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("classifier request did not start")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("handler did not return after cancellation")
	}
	if powerRequests, _ := powerful.snapshot(); powerRequests != 0 {
		t.Fatalf("terminal sends = %d, want zero", powerRequests)
	}
	requests, models := light.snapshot()
	if requests != 2 || strings.Join(models, ",") != "classifier-model,classifier-model" {
		t.Fatalf("light requests = %d %v", requests, models)
	}
	stats := h.policyRoutingController.(*chatPolicyRoutingController).PolicyStatsSnapshot()
	if len(stats.Profiles) != 1 || stats.Profiles[0].Totals.PhysicalClassifierSends != 2 {
		t.Fatalf("canceled classifier telemetry = %+v", stats)
	}
}

func TestChatOperationPlanDefensivelyCopiesCandidates(t *testing.T) {
	parallel := true
	route := &modelRoute{
		public: publicModelContract{id: "internal", routeID: "route-a", endpoints: []string{providerEndpointChatCompletions}, policy: providerRequestPolicy{parallelToolCalls: &parallel}},
		targets: []targetBinding{{
			id: "target-a", upstreamModel: "model-a",
			wirePolicy:  providerRequestPolicy{parallelToolCalls: &parallel},
			legacyOwner: providerModel{publicID: "legacy", supportedEndpoints: []string{providerEndpointChatCompletions}},
		}},
		policy: routePolicy{mode: routeModePrimaryOnly, maxTargetAttempts: 1, maxUpstreamSends: 1},
	}
	contract := publicModelContract{id: "policy", endpoints: []string{providerEndpointChatCompletions}}
	plan := newChatOperationPlan(chatOperationPlanOptions{OperationID: "operation", PublicID: "policy", RouteID: "route-a", Route: route, Contract: contract, SelectedTier: policyTierLightweight, EffectiveMode: policyModeOff})
	route.targets[0].id = "mutated"
	*route.public.policy.parallelToolCalls = false
	*route.targets[0].wirePolicy.parallelToolCalls = false
	route.targets[0].legacyOwner.supportedEndpoints[0] = "/responses"
	contract.endpoints[0] = "/responses"
	operation := newRouteOperationFromChatPlan(plan, t.Context())
	if operation == nil || operation.route.targets[0].id != "target-a" || operation.route.public.endpoints[0] != providerEndpointChatCompletions ||
		plan.terminalParallelToolCalls == nil || !*plan.terminalParallelToolCalls ||
		operation.route.targets[0].wirePolicy.parallelToolCalls == nil || !*operation.route.targets[0].wirePolicy.parallelToolCalls ||
		operation.route.targets[0].legacyOwner.supportedEndpoints[0] != providerEndpointChatCompletions {
		t.Fatalf("sealed operation mutated: %+v", operation)
	}
}

func TestPolicyPublicIDRejectsStatefulResponsesWithoutUpstreamSend(t *testing.T) {
	var sends atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		http.Error(w, "unexpected", http.StatusBadGateway)
	}))
	defer upstream.Close()
	cfg := policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")), WithProvidersConfig(cfg), WithPolicyRoutingMode(PolicyRoutingModeOff))
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"coding-economy","input":"hello","previous_response_id":"resp-upstream"}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if sends.Load() != 0 {
		t.Fatalf("upstream sends = %d, want zero", sends.Load())
	}
}

func TestPolicyRoutingObserveDoesNotWaitForClassifier(t *testing.T) {
	block := make(chan struct{})
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelHigh})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, nil,
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeObserve)),
		WithPolicyRoutingMode(PolicyRoutingModeObserve),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	light.classifierBlock = block
	start := time.Now()
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"plan a refactor"}]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("observe response waited for classifier: %v", elapsed)
	}
	deadline := time.Now().Add(time.Second)
	var requests int
	var models []string
	for {
		requests, models = light.snapshot()
		if requests >= 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("observer did not start: light requests = %d %v", requests, models)
		}
		time.Sleep(time.Millisecond)
	}
	if requests != 3 {
		t.Fatalf("light requests = %d %v, want preflight + observer + baseline terminal", requests, models)
	}
	if powerRequests, _ := powerful.snapshot(); powerRequests != 0 {
		t.Fatalf("powerful terminal requests = %d, want zero in observe mode", powerRequests)
	}
	close(block)
	waitCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := h.WaitLifecycleWorkers(waitCtx); err != nil {
		t.Fatalf("WaitLifecycleWorkers() error = %v", err)
	}
	stats := h.policyRoutingController.(*chatPolicyRoutingController).PolicyStatsSnapshot()
	if len(stats.Profiles) != 1 || stats.Profiles[0].Totals.ShadowTiers.Powerful != 1 || stats.Profiles[0].Totals.ActualTiers.Lightweight != 1 {
		t.Fatalf("observe stats = %+v", stats)
	}
}

func TestPolicyRoutingForcedStreamAggregationKeepsPolicyIdentity(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelHigh})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, nil,
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	body := `{"model":"coding-economy","messages":[{"role":"user","content":"plan and then call a tool"}],"tools":[{"type":"function","function":{"name":"read_file","parameters":{"type":"object"}}}]}`
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Model != "coding-economy" {
		t.Fatalf("aggregated response model = %q", response.Model)
	}
	requests, models := powerful.snapshot()
	if requests != 1 || strings.Join(models, ",") != "power-model" {
		t.Fatalf("powerful requests = %d %v", requests, models)
	}
}

func TestPolicyRoutingUsesSeparateUnavailableAndUncertainFallbacks(t *testing.T) {
	t.Run("uncertain uses powerful", func(t *testing.T) {
		light := newPolicyIntegrationUpstream(t, policyClassifierSignals{
			Abstain: true, TurnType: policyTurnTypeExecution, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow,
		})
		powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
		h, err := NewProxyHandler(nil, nil,
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
		h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"ambiguous"}]}`)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		if requests, models := powerful.snapshot(); requests != 1 || strings.Join(models, ",") != "power-model" {
			t.Fatalf("powerful requests = %d %v", requests, models)
		}
	})

	t.Run("unavailable uses lightweight", func(t *testing.T) {
		light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelHigh})
		powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
		h, err := NewProxyHandler(nil, nil,
			WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
			WithPolicyRoutingMode(PolicyRoutingModeEnforce),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.InitializePolicyRouting(t.Context()); err != nil {
			t.Fatal(err)
		}
		light.classifierFailureStatus.Store(http.StatusServiceUnavailable)
		recorder := httptest.NewRecorder()
		h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"hard request"}]}`)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		requests, models := light.snapshot()
		if requests != 3 || strings.Join(models, ",") != "classifier-model,classifier-model,light-model" {
			t.Fatalf("light requests = %d %v", requests, models)
		}
		if requests, _ := powerful.snapshot(); requests != 0 {
			t.Fatalf("powerful requests = %d, want zero", requests)
		}
	})
}

func TestPolicyRoutingPreflightFailureObserveDisablesProfileAndEnforceFails(t *testing.T) {
	t.Run("observe", func(t *testing.T) {
		light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
		powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
		light.classifierFailureStatus.Store(http.StatusServiceUnavailable)
		h, err := NewProxyHandler(nil, nil,
			WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeObserve)),
			WithPolicyRoutingMode(PolicyRoutingModeObserve),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.InitializePolicyRouting(t.Context()); err != nil {
			t.Fatalf("observe InitializePolicyRouting() error = %v", err)
		}
		if h.PolicyRoutingPreflightPending() || h.PolicyRoutingReadinessDiagnostic() == "" {
			t.Fatalf("pending=%v diagnostic=%q", h.PolicyRoutingPreflightPending(), h.PolicyRoutingReadinessDiagnostic())
		}
		ready := httptest.NewRecorder()
		h.HandleReadyz(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if ready.Code != http.StatusServiceUnavailable {
			t.Fatalf("ready status = %d body=%s", ready.Code, ready.Body.String())
		}
		diagnostic := h.PolicyRoutingReadinessDiagnostic()
		if err := h.InitializePolicyRouting(t.Context()); err != nil {
			t.Fatalf("observe retry InitializePolicyRouting() error = %v", err)
		}
		if got := h.PolicyRoutingReadinessDiagnostic(); got != diagnostic {
			t.Fatalf("observe retry diagnostic = %q, want preserved %q", got, diagnostic)
		}
		light.classifierFailureStatus.Store(0)
		recorder := httptest.NewRecorder()
		h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"hello"}]}`)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
		}
		requests, models := light.snapshot()
		if requests != 2 || strings.Join(models, ",") != "classifier-model,light-model" {
			t.Fatalf("requests after disabled observe = %d %v", requests, models)
		}
	})

	t.Run("enforce", func(t *testing.T) {
		light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
		powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
		light.classifierFailureStatus.Store(http.StatusServiceUnavailable)
		h, err := NewProxyHandler(nil, nil,
			WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
			WithPolicyRoutingMode(PolicyRoutingModeEnforce),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.InitializePolicyRouting(t.Context()); err == nil {
			t.Fatal("enforce preflight error = nil")
		}
		if !h.PolicyRoutingPreflightPending() {
			t.Fatal("preflight pending cleared after failure")
		}
		stats := h.policyRoutingController.(*chatPolicyRoutingController).PolicyStatsSnapshot()
		if len(stats.Profiles) != 1 || stats.Profiles[0].PreflightState != policyStatsPreflightFailed || stats.Profiles[0].Totals.PhysicalClassifierSends != 1 {
			t.Fatalf("enforce preflight failure stats = %+v", stats)
		}
		diagnostic := h.PolicyRoutingReadinessDiagnostic()
		canceledCtx, cancel := context.WithCancel(t.Context())
		cancel()
		if err := h.InitializePolicyRouting(canceledCtx); !errors.Is(err, context.Canceled) {
			t.Fatalf("canceled retry error = %v, want context canceled", err)
		}
		if got := h.PolicyRoutingReadinessDiagnostic(); got != diagnostic {
			t.Fatalf("canceled retry diagnostic = %q, want preserved %q", got, diagnostic)
		}
		light.classifierFailureStatus.Store(0)
		if err := h.InitializePolicyRouting(t.Context()); err != nil {
			t.Fatalf("retry InitializePolicyRouting() error = %v", err)
		}
		if h.PolicyRoutingPreflightPending() {
			t.Fatal("preflight pending remained set after successful retry")
		}
		if diagnostic := h.PolicyRoutingReadinessDiagnostic(); diagnostic != "" {
			t.Fatalf("readiness diagnostic remained after successful retry: %q", diagnostic)
		}
		ready := httptest.NewRecorder()
		h.HandleReadyz(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if ready.Code != http.StatusOK {
			t.Fatalf("ready status after retry=%d body=%s", ready.Code, ready.Body.String())
		}
	})
}

func TestPolicyRoutingCanceledPreflightKeepsStartupGatePending(t *testing.T) {
	block := make(chan struct{})
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
	light.classifierBlock = block
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, nil,
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- h.InitializePolicyRouting(ctx) }()
	deadline := time.Now().Add(time.Second)
	for {
		requests, _ := light.snapshot()
		if requests >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("classifier preflight did not start")
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("InitializePolicyRouting() error = %v, want context canceled", err)
	}
	if !h.PolicyRoutingPreflightPending() {
		t.Fatal("preflight pending cleared after cancellation")
	}
	ready := httptest.NewRecorder()
	h.HandleReadyz(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable || !strings.Contains(ready.Body.String(), "policy routing preflight pending") {
		t.Fatalf("ready status=%d body=%s", ready.Code, ready.Body.String())
	}

	close(block)
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatalf("retry InitializePolicyRouting() error = %v", err)
	}
	if h.PolicyRoutingPreflightPending() {
		t.Fatal("preflight pending remained set after successful retry")
	}
	if diagnostic := h.PolicyRoutingReadinessDiagnostic(); diagnostic != "" {
		t.Fatalf("readiness diagnostic remained after successful retry: %q", diagnostic)
	}
	ready = httptest.NewRecorder()
	h.HandleReadyz(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("ready status after retry=%d body=%s", ready.Code, ready.Body.String())
	}
}

func TestPolicyRoutingObserveFailureAfterSuccessDoesNotRestoreReadinessWithoutProbe(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, nil,
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeObserve)),
		WithPolicyRoutingMode(PolicyRoutingModeObserve),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatalf("initial InitializePolicyRouting() error = %v", err)
	}
	if h.PolicyRoutingPreflightPending() || h.PolicyRoutingReadinessDiagnostic() != "" {
		t.Fatalf("initial pending=%v diagnostic=%q", h.PolicyRoutingPreflightPending(), h.PolicyRoutingReadinessDiagnostic())
	}

	light.classifierFailureStatus.Store(http.StatusServiceUnavailable)
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatalf("failed observe InitializePolicyRouting() error = %v", err)
	}
	diagnostic := h.PolicyRoutingReadinessDiagnostic()
	if diagnostic == "" {
		t.Fatal("observe failure did not publish a readiness diagnostic")
	}
	profile := h.policyRoutingController.(*chatPolicyRoutingController).profiles["coding-policy"]
	if profile == nil || profile.preflightReady.Load() {
		t.Fatalf("failed observe profile remained ready: %+v", profile)
	}
	requestsBeforeRetry, _ := light.snapshot()

	light.classifierFailureStatus.Store(0)
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatalf("disabled observe retry InitializePolicyRouting() error = %v", err)
	}
	requestsAfterRetry, _ := light.snapshot()
	if requestsAfterRetry != requestsBeforeRetry {
		t.Fatalf("disabled observe retry sent classifier request: before=%d after=%d", requestsBeforeRetry, requestsAfterRetry)
	}
	if got := h.PolicyRoutingReadinessDiagnostic(); got != diagnostic {
		t.Fatalf("disabled observe retry diagnostic=%q, want preserved %q", got, diagnostic)
	}
	ready := httptest.NewRecorder()
	h.HandleReadyz(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
	if ready.Code != http.StatusServiceUnavailable {
		t.Fatalf("ready status=%d body=%s", ready.Code, ready.Body.String())
	}
}

func TestPolicyRoutingSelectedRouteFailureNeverCrossesTiers(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelHigh})
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
	powerful.terminalFailureStatus.Store(http.StatusServiceUnavailable)
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"plan"}]}`)))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
	}
	lightRequests, lightModels := light.snapshot()
	if lightRequests != 2 || strings.Join(lightModels, ",") != "classifier-model,classifier-model" {
		t.Fatalf("light requests = %d %v; selected powerful failure crossed tiers", lightRequests, lightModels)
	}
	if powerRequests, models := powerful.snapshot(); powerRequests != 1 || strings.Join(models, ",") != "power-model" {
		t.Fatalf("powerful requests = %d %v", powerRequests, models)
	}
}

func TestPolicyRoutingClientStreamKeepsPolicyIdentity(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelHigh})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, nil,
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
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"plan"}],"stream":true}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	stream := recorder.Body.String()
	if !strings.Contains(stream, `"model":"coding-economy"`) || strings.Contains(stream, `"model":"power-model"`) {
		t.Fatalf("stream leaked terminal identity: %s", stream)
	}
}

func TestPolicyRoutingRejectsNonTextAndHostedToolsBeforeAnySend(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{
			name: "image content",
			body: `{"model":"coding-economy","messages":[{"role":"user","content":[{"type":"text","text":"look"},{"type":"image_url","image_url":{"url":"https://example.test/image.png"}}]}]}`,
		},
		{
			name: "hosted tool",
			body: `{"model":"coding-economy","messages":[{"role":"user","content":"search"}],"tools":[{"type":"web_search"}]}`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			h, err := NewProxyHandler(nil, nil,
				WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
				WithPolicyRoutingMode(PolicyRoutingModeOff),
			)
			if err != nil {
				t.Fatal(err)
			}
			recorder := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tc.body)))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if sends, _ := light.snapshot(); sends != 0 {
				t.Fatalf("light/classifier sends=%d, want zero", sends)
			}
			if sends, _ := powerful.snapshot(); sends != 0 {
				t.Fatalf("powerful sends=%d, want zero", sends)
			}
		})
	}
}

func TestPolicyRoutingSharedClassifierPreflightCountsOnePhysicalSend(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeObserve)
	second := clonePolicyProfileConfig(cfg.PolicyProfiles[0])
	second.ID = "coding-policy-two"
	second.PublicID = "coding-economy-two"
	cfg.PolicyProfiles = append(cfg.PolicyProfiles, second)
	h, err := NewProxyHandler(nil, nil, WithProvidersConfig(cfg), WithPolicyRoutingMode(PolicyRoutingModeObserve))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	requests, models := light.snapshot()
	if requests != 1 || strings.Join(models, ",") != "classifier-model" {
		t.Fatalf("preflight requests = %d %v, want one shared send", requests, models)
	}
	stats := h.policyRoutingController.(*chatPolicyRoutingController).PolicyStatsSnapshot()
	if len(stats.Profiles) != 2 || stats.Profiles[0].Profile != "coding-economy" || stats.Profiles[1].Profile != "coding-economy-two" {
		t.Fatalf("shared preflight stats exposed internal profile ids: %+v", stats.Profiles)
	}
	if stats.Totals.PhysicalClassifierSends != 1 {
		t.Fatalf("global preflight physical sends = %d", stats.Totals.PhysicalClassifierSends)
	}
}

func TestPolicyRoutingRejectsRequestsUntilPreflightSucceeds(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, nil,
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"must wait"}]}`)))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if sends, _ := light.snapshot(); sends != 0 {
		t.Fatalf("classifier/baseline sends=%d before preflight", sends)
	}
	if sends, _ := powerful.snapshot(); sends != 0 {
		t.Fatalf("powerful sends=%d before preflight", sends)
	}
}

func TestPolicyRoutingClassifierRejectionUsesUnavailableTier(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, nil,
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)),
		WithPolicyRoutingMode(PolicyRoutingModeEnforce),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	light.classifierFailureStatus.Store(http.StatusBadRequest)
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"request rejected by classifier"}]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	requests, models := light.snapshot()
	if requests != 3 || strings.Join(models, ",") != "classifier-model,classifier-model,light-model" {
		t.Fatalf("light requests=%d models=%v, want unavailable fallback", requests, models)
	}
	if sends, _ := powerful.snapshot(); sends != 0 {
		t.Fatalf("powerful sends=%d, want zero", sends)
	}
}

func TestPolicyRoutingPreDispatchClassifierFailureDoesNotCountPhysicalSend(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)
	cfg.PolicyProfiles[0].ClassifierUnavailableTier = policyConfigTierPowerful
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")), WithProvidersConfig(cfg), WithPolicyRoutingMode(PolicyRoutingModeEnforce))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	classifierRoute, _ := h.providerSetup().lookupTerminalRoute("classifier-route")
	classifierRoute.targets[0].provider.baseURL = "://invalid"
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"hello"}]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	stats := h.policyRoutingController.(*chatPolicyRoutingController).PolicyStatsSnapshot()
	if got := stats.Profiles[0].Totals.PhysicalClassifierSends; got != 1 {
		t.Fatalf("physical classifier sends = %d, want preflight only", got)
	}
	if sends, _ := powerful.snapshot(); sends != 1 {
		t.Fatalf("powerful fallback sends = %d", sends)
	}
}

func TestPolicyRoutingSharedBreakerStateAppliesToEveryProfile(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)
	second := clonePolicyProfileConfig(cfg.PolicyProfiles[0])
	second.ID = "coding-policy-two"
	second.PublicID = "coding-economy-two"
	cfg.PolicyProfiles = append(cfg.PolicyProfiles, second)
	h, err := NewProxyHandler(nil, nil, WithProvidersConfig(cfg), WithPolicyRoutingMode(PolicyRoutingModeEnforce))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	light.classifierFailureStatus.Store(http.StatusServiceUnavailable)
	for index := 0; index < policyBreakerFailureThreshold; index++ {
		recorder := httptest.NewRecorder()
		h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"hello"}]}`)))
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d status=%d body=%s", index, recorder.Code, recorder.Body.String())
		}
	}
	before, _ := light.snapshot()
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy-two","messages":[{"role":"user","content":"hello"}]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("second profile status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	after, _ := light.snapshot()
	if after != before+1 { // baseline terminal only; shared breaker suppresses classifier send.
		t.Fatalf("light requests before/after = %d/%d", before, after)
	}
	stats := h.policyRoutingController.(*chatPolicyRoutingController).PolicyStatsSnapshot()
	if len(stats.Profiles) != 2 || stats.Profiles[0].BreakerState != policyStatsBreakerOpen || stats.Profiles[1].BreakerState != policyStatsBreakerOpen {
		t.Fatalf("shared breaker stats = %+v", stats.Profiles)
	}
}

func TestPolicyRoutingValidatesSharedPublicContractBeforeClassifierSend(t *testing.T) {
	falseValue := false
	tests := []struct {
		name   string
		mutate func(*ProvidersConfig)
		field  string
	}{
		{
			name: "reasoning effort outside intersection",
			mutate: func(cfg *ProvidersConfig) {
				cfg.ModelRoutes[0].ReasoningEffort = []string{"low", "medium"}
				cfg.ModelRoutes[1].ReasoningEffort = []string{"medium", "high"}
			},
			field: `"reasoning_effort":"high"`,
		},
		{
			name: "parallel tools outside intersection",
			mutate: func(cfg *ProvidersConfig) {
				cfg.ModelRoutes[1].ParallelToolCalls = &falseValue
			},
			field: `"parallel_tool_calls":true`,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)
			tc.mutate(&cfg)
			h, err := NewProxyHandler(nil, nil, WithProvidersConfig(cfg), WithPolicyRoutingMode(PolicyRoutingModeOff))
			if err != nil {
				t.Fatal(err)
			}
			body := `{"model":"coding-economy","messages":[{"role":"user","content":"hello"}],` + tc.field + `}`
			recorder := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			if sends, _ := light.snapshot(); sends != 0 {
				t.Fatalf("light/classifier sends=%d, want zero", sends)
			}
			if sends, _ := powerful.snapshot(); sends != 0 {
				t.Fatalf("powerful sends=%d, want zero", sends)
			}
		})
	}
}

func TestPolicyRoutingNativeChatHonorsFalseParallelToolCallsContract(t *testing.T) {
	tests := []struct {
		name                     string
		requestField             string
		selectedTerminalSupports bool
		wantParallelToolCalls    string
	}{
		{name: "omitted/capable terminal", selectedTerminalSupports: true, wantParallelToolCalls: "false"},
		{name: "explicit false/capable terminal", requestField: `,"parallel_tool_calls":false`, selectedTerminalSupports: true, wantParallelToolCalls: "false"},
		{name: "omitted/unsupported terminal", selectedTerminalSupports: false},
		{name: "explicit false/unsupported terminal", requestField: `,"parallel_tool_calls":false`, selectedTerminalSupports: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)
			cfg.ModelRoutes[0].ParallelToolCalls = boolPointer(tc.selectedTerminalSupports)
			cfg.ModelRoutes[1].ParallelToolCalls = boolPointer(!tc.selectedTerminalSupports)

			h, err := NewProxyHandler(nil, nil, WithProvidersConfig(cfg), WithPolicyRoutingMode(PolicyRoutingModeOff))
			if err != nil {
				t.Fatal(err)
			}
			if err := h.InitializePolicyRouting(t.Context()); err != nil {
				t.Fatal(err)
			}

			body := `{"model":"coding-economy","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object"}}}]` + tc.requestField + `}`
			recorder := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
			if recorder.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}

			requests, models := light.snapshot()
			if requests != 1 || strings.Join(models, ",") != "light-model" {
				t.Fatalf("selected terminal requests=%d models=%v, want one light-model request", requests, models)
			}
			parallel := light.parallelToolCallsSnapshot()
			if len(parallel) != 1 {
				t.Fatalf("parallel_tool_calls snapshots=%d, want one", len(parallel))
			}
			if got := string(parallel[0]); got != tc.wantParallelToolCalls {
				t.Fatalf("parallel_tool_calls=%q, want %q", got, tc.wantParallelToolCalls)
			}
		})
	}
}

func TestPolicyAliasIsRewrittenWhilePublicIdentityStaysCanonical(t *testing.T) {
	upstream := newPolicyIntegrationUpstream(t, policyClassifierSignals{
		TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow,
	})
	cfg := policyIntegrationConfig(upstream.server.URL, upstream.server.URL, policyConfigModeEnforce)
	cfg.PolicyProfiles[0].PublicID = "claude-sonnet-4.5"
	cfg.ModelRoutes[0].Targets[0].UpstreamModel = "claude-sonnet-4.5"
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")), WithProvidersConfig(cfg), WithPolicyRoutingMode(PolicyRoutingModeEnforce))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"claude-sonnet-4-5-20250514","messages":[{"role":"user","content":"hello"}]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Model != "claude-sonnet-4.5" {
		t.Fatalf("response model=%q", response.Model)
	}
	_, models := upstream.snapshot()
	if got := strings.Join(models, ","); got != "classifier-model,classifier-model,claude-sonnet-4.5" {
		t.Fatalf("upstream models=%q", got)
	}
}

func TestRouteOnlyPolicyConfigRejectsUnknownChatModelLocally(t *testing.T) {
	var sends atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { sends.Add(1) }))
	defer upstream.Close()
	cfg := policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeOff)
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")), WithProvidersConfig(cfg), WithPolicyRoutingMode(PolicyRoutingModeOff))
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"typo-model","messages":[{"role":"user","content":"hello"}]}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if sends.Load() != 0 {
		t.Fatalf("upstream sends=%d", sends.Load())
	}
}

func TestPolicyClientStatsUsePublicIDNotTerminalRouteID(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, nil, WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)), WithPolicyRoutingMode(PolicyRoutingModeEnforce))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, summary := WithRequestSummary(t.Context())
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"hello"}]}`)).WithContext(ctx))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := summary.RouteID(); got != "coding-economy" {
		t.Fatalf("summary route=%q", got)
	}
	h.stats.record(summary, recorder.Code, "test", time.Millisecond)
	snapshot := h.stats.snapshot()
	if len(snapshot.ByRoute) != 1 || snapshot.ByRoute[0].Route != "coding-economy" {
		t.Fatalf("by_route=%+v", snapshot.ByRoute)
	}
	if len(snapshot.Recent) != 1 || snapshot.Recent[0].RouteID != "coding-economy" {
		t.Fatalf("recent=%+v", snapshot.Recent)
	}
	if snapshot.Recent[0].FinalTarget != "coding-economy" || snapshot.Recent[0].Provider != "" {
		t.Fatalf("recent policy attribution leaked terminal topology: %+v", snapshot.Recent[0])
	}
	if len(snapshot.ByProvider) != 0 {
		t.Fatalf("by_provider leaked policy terminal provider: %+v", snapshot.ByProvider)
	}
	if len(snapshot.ByTarget) != 1 || snapshot.ByTarget[0].Route != "coding-economy" || snapshot.ByTarget[0].Target != "coding-economy" ||
		snapshot.ByTarget[0].Provider != "" || snapshot.ByTarget[0].Kind != "" {
		t.Fatalf("by_target leaked policy terminal topology: %+v", snapshot.ByTarget)
	}
	if len(snapshot.RecentAttempts) != 1 || snapshot.RecentAttempts[0].RouteID != "coding-economy" || snapshot.RecentAttempts[0].TargetID != "coding-economy" ||
		snapshot.RecentAttempts[0].ProviderID != "" || snapshot.RecentAttempts[0].ProviderKind != "" || snapshot.RecentAttempts[0].UpstreamRequestID != "" {
		t.Fatalf("recent_attempts leaked policy terminal topology: %+v", snapshot.RecentAttempts)
	}
}

func TestPolicyResponsesReplayIDRejectedInEnforceBeforeClassifierSend(t *testing.T) {
	bodies := []struct {
		name string
		body string
	}{
		{name: "well_typed", body: `{"model":"coding-economy","messages":[{"role":"assistant","tool_calls":[{"id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","content":"ok"}]}`},
		{name: "unrelated_type_error", body: `{"model":"coding-economy","messages":[{"role":"user","content":"hello","tool_call_id":123},{"role":"assistant","tool_calls":[{"id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","content":"ok"}]}`},
		{name: "case_folded_siblings", body: `{"model":"coding-economy","messages":[{"role":"assistant","tool_calls":[{"id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","ID":123,"type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","content":"ok"}],"MESSAGES":123}`},
		{name: "case_insensitive_keys", body: `{"model":"coding-economy","messages":[{"Role":"assistant","Tool_Calls":[{"ID":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"Role":"tool","Tool_Call_ID":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","content":"ok"}]}`},
	}
	for _, request := range bodies {
		t.Run(request.name, func(t *testing.T) {
			light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
			powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			h, err := NewProxyHandler(nil, nil, WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)), WithPolicyRoutingMode(PolicyRoutingModeEnforce))
			if err != nil {
				t.Fatal(err)
			}
			if err := h.InitializePolicyRouting(t.Context()); err != nil {
				t.Fatal(err)
			}
			before, _ := light.snapshot()
			recorder := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(request.body)))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			after, _ := light.snapshot()
			if after != before {
				t.Fatalf("classifier/terminal sends before=%d after=%d", before, after)
			}
			if sends, _ := powerful.snapshot(); sends != 0 {
				t.Fatalf("powerful sends=%d", sends)
			}
		})
	}
}

func TestPolicyAmbiguousCustomReplayRejectedBeforeObserveClassifierSend(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, nil,
		WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeObserve)),
		WithPolicyRoutingMode(PolicyRoutingModeObserve),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	before, _ := light.snapshot()
	body := `{
		"model":"coding-economy",
		"messages":[
			{"role":"assistant","tool_calls":[{
				"id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA",
				"type":"custom",
				"custom":{"name":"apply_patch","input":"patch"},
				"function":{"name":"apply_patch","arguments":"{}"}
			}]},
			{"role":"tool","tool_call_id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","content":"done"}
		]
	}`
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	waitCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := h.WaitLifecycleWorkers(waitCtx); err != nil {
		t.Fatal(err)
	}
	after, _ := light.snapshot()
	if after != before {
		t.Fatalf("classifier/terminal sends before=%d after=%d", before, after)
	}
	if sends, _ := powerful.snapshot(); sends != 0 {
		t.Fatalf("powerful sends=%d", sends)
	}
}

func TestPolicyResponsesReplayContinuationPlansDeterministicBaseline(t *testing.T) {
	modes := []struct {
		name        string
		profileMode string
		runtimeMode PolicyRoutingMode
	}{
		{name: "off", profileMode: policyConfigModeEnforce, runtimeMode: PolicyRoutingModeOff},
		{name: "observe", profileMode: policyConfigModeObserve, runtimeMode: PolicyRoutingModeObserve},
	}
	body := []byte(`{"model":"coding-economy","messages":[{"role":"assistant","tool_calls":[{"id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","content":"ok"}]}`)
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
			powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			h, err := NewProxyHandler(nil, nil, WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, mode.profileMode)), WithPolicyRoutingMode(mode.runtimeMode))
			if err != nil {
				t.Fatal(err)
			}
			if err := h.InitializePolicyRouting(t.Context()); err != nil {
				t.Fatal(err)
			}

			plan, err := h.planOpenAIChatPolicy(t.Context(), "coding-economy", body)
			if err != nil {
				t.Fatalf("planOpenAIChatPolicy() error = %v", err)
			}
			if !plan.valid() {
				t.Fatalf("plan = %+v, want valid baseline plan", plan)
			}
			if plan.selectedTier != policyTierLightweight || len(plan.candidateSnapshot()) != 1 {
				t.Fatalf("plan tier/candidates = %s/%d, want lightweight/1", plan.selectedTier, len(plan.candidateSnapshot()))
			}
			if !plan.allowsResponsesReplayPassthrough() {
				t.Fatalf("plan = %+v, want Responses replay passthrough", plan)
			}
			if mode.runtimeMode == PolicyRoutingModeObserve {
				waitCtx, cancel := context.WithTimeout(t.Context(), time.Second)
				defer cancel()
				if err := h.WaitLifecycleWorkers(waitCtx); err != nil {
					t.Fatalf("WaitLifecycleWorkers() error = %v", err)
				}
			}
		})
	}
}

func TestPolicyResponsesReplayContinuationRejectsNondeterministicBaseline(t *testing.T) {
	modes := []struct {
		name        string
		profileMode string
		runtimeMode PolicyRoutingMode
	}{
		{name: "off", profileMode: policyConfigModeEnforce, runtimeMode: PolicyRoutingModeOff},
		{name: "observe", profileMode: policyConfigModeObserve, runtimeMode: PolicyRoutingModeObserve},
	}
	body := []byte(`{"model":"coding-economy","messages":[{"role":"assistant","tool_calls":[{"id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","content":"ok"}]}`)
	for _, mode := range modes {
		t.Run(mode.name, func(t *testing.T) {
			light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
			powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			cfg := policyIntegrationConfig(light.server.URL, powerful.server.URL, mode.profileMode)
			cfg.ModelRoutes[0].Targets = append(cfg.ModelRoutes[0].Targets, ModelRouteTargetConfig{
				ID: "light-secondary", Provider: "light-provider", UpstreamModel: "light-model-secondary",
			})
			h, err := NewProxyHandler(nil, nil, WithProvidersConfig(cfg), WithPolicyRoutingMode(mode.runtimeMode))
			if err != nil {
				t.Fatal(err)
			}
			if err := h.InitializePolicyRouting(t.Context()); err != nil {
				t.Fatal(err)
			}
			before, _ := light.snapshot()

			plan, err := h.planOpenAIChatPolicy(t.Context(), "coding-economy", body)
			if err == nil {
				t.Fatalf("planOpenAIChatPolicy() plan = %+v, want replay rejection", plan)
			}
			var requestErr *providerRequestError
			if !errors.As(err, &requestErr) || requestErr.statusCode != http.StatusBadRequest {
				t.Fatalf("planOpenAIChatPolicy() error = %T %v, want 400 providerRequestError", err, err)
			}
			after, _ := light.snapshot()
			if after != before {
				t.Fatalf("classifier/terminal sends before=%d after=%d", before, after)
			}
			if sends, _ := powerful.snapshot(); sends != 0 {
				t.Fatalf("powerful sends=%d", sends)
			}
		})
	}
}

func TestPolicyInvalidNativeToolHistoryRejectedBeforeClassifierSend(t *testing.T) {
	bodies := []struct {
		name string
		body string
	}{
		{
			name: "unknown tool result",
			body: `{"model":"coding-economy","messages":[{"role":"user","content":"task"},{"role":"assistant","tool_calls":[{"id":"call-known","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call-unknown","content":"result"}]}`,
		},
		{
			name: "duplicate tool result",
			body: `{"model":"coding-economy","messages":[{"role":"user","content":"task"},{"role":"assistant","tool_calls":[{"id":"call-dup","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call-dup","content":"first"},{"role":"tool","tool_call_id":"call-dup","content":"second"}]}`,
		},
		{
			name: "duplicate assistant tool call ID",
			body: `{"model":"coding-economy","messages":[{"role":"user","content":"task"},{"role":"assistant","tool_calls":[{"id":"call-same","type":"function","function":{"name":"one","arguments":"{}"}},{"id":"call-same","type":"function","function":{"name":"two","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call-same","content":"result"}]}`,
		},
		{
			name: "missing tool result",
			body: `{"model":"coding-economy","messages":[{"role":"user","content":"task"},{"role":"assistant","tool_calls":[{"id":"call-pending","type":"function","function":{"name":"lookup","arguments":"{}"}}]},{"role":"user","content":"continue"}]}`,
		},
	}

	for _, test := range bodies {
		t.Run(test.name, func(t *testing.T) {
			light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
			powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			h, err := NewProxyHandler(nil, nil, WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)), WithPolicyRoutingMode(PolicyRoutingModeEnforce))
			if err != nil {
				t.Fatal(err)
			}
			if err := h.InitializePolicyRouting(t.Context()); err != nil {
				t.Fatal(err)
			}
			before, _ := light.snapshot()
			recorder := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(test.body)))
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			after, _ := light.snapshot()
			if after != before {
				t.Fatalf("classifier/terminal sends before=%d after=%d", before, after)
			}
			if sends, _ := powerful.snapshot(); sends != 0 {
				t.Fatalf("powerful sends=%d", sends)
			}
		})
	}
}

func TestPolicyTerminalSuccessHeadersAreSanitized(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypeLookup, CodeScope: policyCodeScopeNone, RiskLevel: policyRiskLevelLow})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, nil, WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeOff)), WithPolicyRoutingMode(PolicyRoutingModeOff))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"hello"}]}`)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type=%q", got)
	}
	if got := recorder.Header().Get("Openai-Model"); got != "coding-economy" {
		t.Fatalf("Openai-Model=%q", got)
	}
	if got := recorder.Header().Get("X-Request-ID"); got != "" {
		t.Fatalf("X-Request-ID=%q, want omitted", got)
	}
	if got := recorder.Header().Get("X-Vekil-Request-ID"); got == "" {
		t.Fatal("X-Vekil-Request-ID is missing")
	}
	for _, name := range []string{"X-Azure-Request-ID", "OpenAI-Processing-Ms", "X-Vekil-Internal-Route"} {
		if got := recorder.Header().Get(name); got != "" {
			t.Fatalf("%s=%q", name, got)
		}
	}
}

func TestPolicyTerminalHTTPErrorIsSanitized(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelHigh})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")), WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)), WithPolicyRoutingMode(PolicyRoutingModeEnforce))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	powerful.terminalFailureStatus.Store(http.StatusBadGateway)
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"plan"}]}`)))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `"message":"upstream request failed"`) || !strings.Contains(body, `"code":"upstream_error"`) ||
		strings.Contains(body, "terminal unavailable") || strings.Contains(body, "power-model") || strings.Contains(body, "power-route") || strings.Contains(body, "power-provider") {
		t.Fatalf("unsanitized policy error body: %s", body)
	}
	if got := recorder.Header().Get("Openai-Model"); got != "coding-economy" {
		t.Fatalf("Openai-Model=%q", got)
	}
	if got := recorder.Header().Get("X-Openai-Model"); got != "coding-economy" {
		t.Fatalf("X-Openai-Model=%q", got)
	}
	if got := recorder.Header().Get("X-Request-ID"); got != "" {
		t.Fatalf("X-Request-ID=%q, want omitted", got)
	}
	if got := recorder.Header().Get("X-Vekil-Request-ID"); got == "" {
		t.Fatal("X-Vekil-Request-ID is missing")
	}
	if got := recorder.Header().Get("X-Vekil-Internal-Route"); got != "" {
		t.Fatalf("X-Vekil-Internal-Route=%q", got)
	}
	if got := recorder.Header().Get("X-Azure-Request-ID"); got != "" {
		t.Fatalf("X-Azure-Request-ID=%q", got)
	}
	if got := recorder.Header().Get("OpenAI-Processing-Ms"); got != "" {
		t.Fatalf("OpenAI-Processing-Ms=%q", got)
	}
	for name, want := range map[string]string{
		"X-RateLimit-Limit":     "100",
		"X-RateLimit-Remaining": "42",
		"X-RateLimit-Reset":     "123456",
	} {
		if got := recorder.Header().Get(name); got != want {
			t.Fatalf("%s=%q, want %q", name, got, want)
		}
	}
	if got := recorder.Header().Get("X-RateLimit-Model"); got != "" {
		t.Fatalf("X-RateLimit-Model=%q", got)
	}
	if got := recorder.Header().Get("RateLimit-Policy"); got != "" {
		t.Fatalf("RateLimit-Policy=%q", got)
	}
}

func TestPolicyTerminalStreamErrorIsSanitized(t *testing.T) {
	tests := []struct {
		name            string
		body            string
		wantStatus      int
		wantStreamHeads bool
	}{
		{
			name:            "client_stream",
			body:            `{"model":"coding-economy","stream":true,"messages":[{"role":"user","content":"plan"}]}`,
			wantStatus:      http.StatusOK,
			wantStreamHeads: true,
		},
		{
			name:       "forced_stream",
			body:       `{"model":"coding-economy","messages":[{"role":"user","content":"plan"}],"tools":[{"type":"function","function":{"name":"lookup","description":"look up data","parameters":{"type":"object","properties":{}}}}]}`,
			wantStatus: http.StatusBadGateway,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelHigh})
			powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			h, err := NewProxyHandler(nil, logger.New(logger.ParseLevel("error")), WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)), WithPolicyRoutingMode(PolicyRoutingModeEnforce))
			if err != nil {
				t.Fatal(err)
			}
			if err := h.InitializePolicyRouting(t.Context()); err != nil {
				t.Fatal(err)
			}
			powerful.terminalStreamFailure.Store(true)
			recorder := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(tt.body)))
			if recorder.Code != tt.wantStatus {
				t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
			}
			body := recorder.Body.String()
			if !strings.Contains(body, `"message":"upstream request failed"`) || !strings.Contains(body, `"code":"upstream_error"`) ||
				strings.Contains(body, "power-model") || strings.Contains(body, "power-route") || strings.Contains(body, "power-provider") {
				t.Fatalf("unsanitized policy stream error: %s", body)
			}
			if got := recorder.Header().Get("X-Azure-Request-ID"); got != "" {
				t.Fatalf("X-Azure-Request-ID=%q", got)
			}
			if got := recorder.Header().Get("OpenAI-Processing-Ms"); got != "" {
				t.Fatalf("OpenAI-Processing-Ms=%q", got)
			}
			if tt.wantStreamHeads {
				if got := recorder.Header().Get("Openai-Model"); got != "coding-economy" {
					t.Fatalf("Openai-Model=%q", got)
				}
				if got := recorder.Header().Get("X-Request-ID"); got != "" {
					t.Fatalf("X-Request-ID=%q, want omitted", got)
				}
				if got := recorder.Header().Get("X-Vekil-Request-ID"); got == "" {
					t.Fatal("X-Vekil-Request-ID is missing")
				}
			}
		})
	}
}

func TestPolicyPrecommitStreamErrorIsSanitized(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelHigh})
	powerful := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	h, err := NewProxyHandler(nil, nil, WithProvidersConfig(policyIntegrationConfig(light.server.URL, powerful.server.URL, policyConfigModeEnforce)), WithPolicyRoutingMode(PolicyRoutingModeEnforce))
	if err != nil {
		t.Fatal(err)
	}
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	powerful.terminalRateLimitFailure.Store(true)
	recorder := httptest.NewRecorder()
	body := `{"model":"coding-economy","stream":true,"messages":[{"role":"user","content":"plan"}]}`
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	responseBody := recorder.Body.String()
	if !strings.Contains(responseBody, `"message":"upstream request failed"`) || !strings.Contains(responseBody, `"code":"upstream_error"`) ||
		strings.Contains(responseBody, "power-model") || strings.Contains(responseBody, "power-route") || strings.Contains(responseBody, "power-provider") {
		t.Fatalf("unsanitized precommit policy stream error: %s", responseBody)
	}
	if got := recorder.Header().Get("Openai-Model"); got != "coding-economy" {
		t.Fatalf("Openai-Model=%q", got)
	}
	if got := recorder.Header().Get("X-Request-ID"); got != "" {
		t.Fatalf("X-Request-ID=%q, want omitted", got)
	}
	if got := recorder.Header().Get("X-Vekil-Request-ID"); got == "" {
		t.Fatal("X-Vekil-Request-ID is missing")
	}
	if got := recorder.Header().Get("X-Azure-Request-ID"); got != "" {
		t.Fatalf("X-Azure-Request-ID=%q", got)
	}
	if got := recorder.Header().Get("OpenAI-Processing-Ms"); got != "" {
		t.Fatalf("OpenAI-Processing-Ms=%q", got)
	}
}
