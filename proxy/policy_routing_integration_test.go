package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/logger"
)

type policyIntegrationUpstream struct {
	server *httptest.Server

	mu       sync.Mutex
	models   []string
	requests int

	classifierSignals       policyClassifierSignals
	classifierBlock         <-chan struct{}
	classifierFailureStatus atomic.Int64
	terminalFailureStatus   atomic.Int64
}

func newPolicyIntegrationUpstream(t *testing.T, signals policyClassifierSignals) *policyIntegrationUpstream {
	t.Helper()
	u := &policyIntegrationUpstream{classifierSignals: signals}
	u.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { _ = r.Body.Close() }()
		var request struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		u.mu.Lock()
		u.requests++
		u.models = append(u.models, request.Model)
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
		if status := int(u.terminalFailureStatus.Load()); status != 0 {
			http.Error(w, "terminal unavailable", status)
			return
		}
		if request.Stream {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"terminal-stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"ok\"}}]}\n\n", request.Model)
			_, _ = fmt.Fprintf(w, "data: {\"id\":\"terminal-stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n", request.Model)
			_, _ = fmt.Fprint(w, "data: {\"id\":\"terminal-stream\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"ignored\",\"choices\":[],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\ndata: [DONE]\n\n")
			return
		}
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

func policyIntegrationConfig(lightURL, powerfulURL, profileMode string) ProvidersConfig {
	trueValue := true
	parallel := true
	return ProvidersConfig{
		SchemaVersion: ProvidersConfigSchemaVersion3,
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
	if len(policyStats.Profiles) != 1 || policyStats.Profiles[0].Totals.ActualTiers.Powerful != 1 {
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
		public: publicModelContract{id: "internal", routeID: "route-a", endpoints: []string{providerEndpointChatCompletions}},
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
	*route.targets[0].wirePolicy.parallelToolCalls = false
	route.targets[0].legacyOwner.supportedEndpoints[0] = "/responses"
	contract.endpoints[0] = "/responses"
	operation := newRouteOperationFromChatPlan(plan, t.Context())
	if operation == nil || operation.route.targets[0].id != "target-a" || operation.route.public.endpoints[0] != providerEndpointChatCompletions ||
		operation.route.targets[0].wirePolicy.parallelToolCalls == nil || !*operation.route.targets[0].wirePolicy.parallelToolCalls ||
		operation.route.targets[0].legacyOwner.supportedEndpoints[0] != providerEndpointChatCompletions {
		t.Fatalf("sealed operation mutated: %+v", operation)
	}
}

func TestPolicyPublicIDRejectedOnResponsesWithoutUpstreamSend(t *testing.T) {
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
	h.HandleResponses(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"coding-economy","input":"hello"}`)))
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
		light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
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
		if h.PolicyRoutingPreflightPending() {
			t.Fatal("preflight pending remained set after failure")
		}
		stats := h.policyRoutingController.(*chatPolicyRoutingController).PolicyStatsSnapshot()
		if len(stats.Profiles) != 1 || stats.Profiles[0].PreflightState != policyStatsPreflightFailed || stats.Profiles[0].Totals.PhysicalClassifierSends != 1 {
			t.Fatalf("enforce preflight failure stats = %+v", stats)
		}
	})
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
	if snapshot.ByRoute[0].Route == "light-route" || snapshot.ByRoute[0].Route == "power-route" ||
		snapshot.Recent[0].RouteID == "light-route" || snapshot.Recent[0].RouteID == "power-route" {
		t.Fatalf("client request stats leaked terminal route: by_route=%+v recent=%+v", snapshot.ByRoute, snapshot.Recent)
	}
}
