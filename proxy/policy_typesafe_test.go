package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"gopkg.in/yaml.v3"
)

func policyTypeSafeTestConfig(lightURL, powerfulURL, classifierURL, mode string) ProvidersConfig {
	cfg := policyIntegrationConfig(lightURL, powerfulURL, mode)
	cfg.Providers = append(cfg.Providers, ProviderConfig{
		ID: "evaluation", Type: "typesafe-compatible", BaseURL: classifierURL,
		AuthType: "none", TrustDomain: "org-ai",
	})
	cfg.ModelRoutes[2].Endpoints = []string{providerEndpointSystemOne}
	cfg.ModelRoutes[2].Targets[0].Provider = "evaluation"
	cfg.ModelRoutes[2].Targets[0].UpstreamModel = "test-evaluator"
	cfg.PolicyProfiles[0].DataPolicy.AllowProviderRetention = true
	return cfg
}

func policyTypeSafeTestResponse(t *testing.T, signals policyClassifierSignals) []byte {
	t.Helper()
	encoded, err := json.Marshal(signals)
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	answers := make(map[string]any, len(fields))
	for key, raw := range fields {
		choice := string(raw)
		if len(raw) > 0 && raw[0] == '"' {
			if err := json.Unmarshal(raw, &choice); err != nil {
				t.Fatal(err)
			}
		}
		answers[key] = map[string]any{
			"type": "choice", "choice": choice,
			"probabilities": map[string]float64{choice: 1}, "confidence": 0,
		}
	}
	body, err := json.Marshal(map[string]any{
		"model": "test-evaluator", "answers": answers,
		"usage": map[string]int{"input_tokens": 10, "output_tokens": 4},
	})
	if err != nil {
		t.Fatal(err)
	}
	return body
}

func TestPolicyTypeSafeConfig(t *testing.T) {
	base := func() ProvidersConfig {
		return policyTypeSafeTestConfig("https://light.example/v1", "https://power.example/v1", "https://evaluation.example/v1", "enforce")
	}
	for _, test := range []struct {
		name string
		edit func(*ProvidersConfig)
		want string
	}{
		{"valid", func(*ProvidersConfig) {}, ""},
		{"retention acknowledgement", func(c *ProvidersConfig) { c.PolicyProfiles[0].DataPolicy.AllowProviderRetention = false }, "allow_provider_retention"},
		{"no storage control", func(c *ProvidersConfig) { value := true; c.Providers[2].ClassifierNoStoreSupported = &value }, "classifier_no_store_supported"},
		{"trust domain required", func(c *ProvidersConfig) { c.Providers[2].TrustDomain = "" }, "trust_domain"},
		{"cross domain acknowledgement", func(c *ProvidersConfig) { c.Providers[2].TrustDomain = "external" }, "allow_cross_trust_domain"},
		{"no default", func(c *ProvidersConfig) { c.Providers[2].Default = true }, ".default"},
		{"no public models", func(c *ProvidersConfig) { c.Providers[2].Models = []ProviderModelConfig{{PublicID: "evaluator"}} }, ".models"},
		{"static only", func(c *ProvidersConfig) { c.Providers[2].ModelDiscovery = "openai" }, "model_discovery"},
		{"no public route", func(c *ProvidersConfig) {
			c.ModelRoutes[2].Exposure = "public"
			c.ModelRoutes[2].PublicID = "evaluator"
		}, "internal_purpose"},
		{"classifier purpose", func(c *ProvidersConfig) { c.ModelRoutes[2].InternalPurpose = "" }, "internal policy_classifier"},
		{"no chat endpoint", func(c *ProvidersConfig) { c.ModelRoutes[2].Endpoints = []string{"/chat/completions"} }, "not supported"},
		{"no mixed endpoints", func(c *ProvidersConfig) {
			c.ModelRoutes[2].Endpoints = append(c.ModelRoutes[2].Endpoints, "/chat/completions")
		}, "sole endpoint"},
		{"no terminal use", func(c *ProvidersConfig) { c.PolicyProfiles[0].Lightweight.Route = "classifier-route" }, "reserved for internal purpose"},
		{"one send", func(c *ProvidersConfig) { c.ModelRoutes[2].Routing.MaxUpstreamSends = 2 }, "max_upstream_sends"},
		{"no reasoning", func(c *ProvidersConfig) { c.PolicyProfiles[0].Classifier.ReasoningEffort = "low" }, "reasoning_effort"},
		{"no reasoning metadata", func(c *ProvidersConfig) { c.ModelRoutes[2].ReasoningEffort = []string{"low"} }, "reasoning_effort"},
		{"relative path", func(c *ProvidersConfig) { c.Providers[2].SystemOnePath = "evaluate" }, "systemone_path"},
		{"path query", func(c *ProvidersConfig) { c.Providers[2].SystemOnePath = "/evaluate?key=secret" }, "systemone_path"},
		{"protocol mismatch", func(c *ProvidersConfig) { c.Providers[2].Type = "openai-compatible" }, "not supported"},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := base()
			test.edit(&cfg)
			err := ValidateProvidersConfig(cfg)
			if test.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestPolicyTypeSafeConfigDecodesOffline(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			cfg := policyTypeSafeTestConfig(server.URL, server.URL, server.URL+"/prefix", "enforce")
			cfg.Providers[2].SystemOnePath = "/custom-evaluate"
			var body []byte
			var err error
			if format == "json" {
				body, err = json.Marshal(cfg)
			} else {
				body, err = yaml.Marshal(cfg)
			}
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(t.TempDir(), "providers."+format)
			if err := os.WriteFile(path, body, 0o600); err != nil {
				t.Fatal(err)
			}
			decoded, err := LoadProvidersConfigFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if decoded.Providers[2].SystemOnePath != "/custom-evaluate" {
				t.Fatal("custom endpoint was lost")
			}
			if err := ValidateProvidersConfigFile(path); err != nil {
				t.Fatal(err)
			}
		})
	}
	if calls.Load() != 0 {
		t.Fatal("offline validation contacted a provider")
	}
}

func TestPolicyTypeSafeSystemOnePathRejectsOtherProviders(t *testing.T) {
	for _, kind := range []providerType{providerTypeCopilot, providerTypeAzureOpenAI, providerTypeOpenAICodex, providerTypeOpenAICompatible, providerTypeAnthropicCompatible} {
		t.Run(string(kind), func(t *testing.T) {
			provider := ProviderConfig{
				ID: "other", Type: string(kind), BaseURL: "https://provider.example/openai/v1", APIKey: "test-key",
				SystemOnePath: "/evaluate", Models: []ProviderModelConfig{{PublicID: "test-model"}},
			}
			cfg := ProvidersConfig{SchemaVersion: ProvidersConfigSchemaVersion2, Providers: []ProviderConfig{provider}}
			if err := ValidateProvidersConfig(cfg); err == nil || !strings.Contains(err.Error(), "providers[0].systemone_path") {
				t.Fatalf("config validation = %v, want systemone_path rejection", err)
			}
			if _, err := buildProviderRuntime(provider, "https://copilot.example", nil); err == nil || !strings.Contains(err.Error(), "systemone_path") {
				t.Fatalf("runtime validation = %v, want systemone_path rejection", err)
			}
		})
	}
}

func TestPolicyTypeSafeReadiness(t *testing.T) {
	response := policyTypeSafeTestResponse(t, policyClassifierSignals{
		TurnType: policyTurnTypeEdit, CodeScope: policyCodeScopeFile, RiskLevel: policyRiskLevelLow,
	})
	for _, test := range []struct {
		name, mode string
		failure    bool
		wantStatus int
		wantCalls  int32
	}{
		{name: "enforce", mode: "enforce", wantStatus: http.StatusOK, wantCalls: 1},
		{name: "observe", mode: "observe", wantStatus: http.StatusOK, wantCalls: 1},
		{name: "off", mode: "off", failure: true, wantStatus: http.StatusOK},
		{name: "failed preflight", mode: "observe", failure: true, wantStatus: http.StatusServiceUnavailable, wantCalls: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			evaluator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != "/systemone" {
					t.Errorf("unexpected readiness request: %s %s", r.Method, r.URL.Path)
				}
				if test.failure {
					w.WriteHeader(http.StatusServiceUnavailable)
					return
				}
				_, _ = w.Write(response)
			}))
			t.Cleanup(evaluator.Close)
			cfg := policyTypeSafeTestConfig(evaluator.URL, evaluator.URL, evaluator.URL, test.mode)
			h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), nil, WithProvidersConfig(cfg))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(h.BeginShutdown)
			if err := h.InitializePolicyRouting(t.Context()); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				ready := httptest.NewRecorder()
				h.HandleReadyz(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
				if ready.Code != test.wantStatus {
					t.Fatalf("readiness = %d %s", ready.Code, ready.Body.String())
				}
			}
			if calls.Load() != test.wantCalls {
				t.Fatalf("readiness made extra classifier requests: calls=%d, want %d", calls.Load(), test.wantCalls)
			}
		})
	}
}

func TestPolicyTypeSafeExample(t *testing.T) {
	t.Setenv("TYPESAFE_API_KEY", "test-key")
	if err := ValidateProvidersConfigFile(filepath.Join("..", "examples", "policy-routing-typesafe.yaml")); err != nil {
		t.Fatal(err)
	}
}

func TestPolicyTypeSafeHasNoImplicitDefault(t *testing.T) {
	cfg := policyTypeSafeTestConfig("https://light.example", "https://power.example", "https://evaluate.example", "off")
	cfg.Providers = cfg.Providers[2:]
	cfg.ModelRoutes = cfg.ModelRoutes[2:]
	cfg.PolicyProfiles = nil
	h := &ProxyHandler{}
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(t.Context(), cfg, false)
	if err != nil {
		t.Fatal(err)
	}
	if setup.defaultProviderID != "" {
		t.Fatalf("evaluation provider became the default: %q", setup.defaultProviderID)
	}
	cfg.SchemaVersion = ProvidersConfigSchemaVersion1
	cfg.ModelRoutes = nil
	if err := ValidateProvidersConfig(cfg); err == nil || !strings.Contains(err.Error(), "schema_version: 2") {
		t.Fatalf("schema v1 accepted an evaluation provider: %v", err)
	}
}

func TestPolicyTypeSafeRouting(t *testing.T) {
	lightSignals := policyClassifierSignals{TurnType: policyTurnTypeEdit, CodeScope: policyCodeScopeFile, ToolCallCountEstimate: 3, ModifyingToolCallCountEstimate: 1, RiskLevel: policyRiskLevelLow}
	powerSignals := policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelMedium}
	abstainSignals := lightSignals
	abstainSignals.Abstain = true
	for _, test := range []struct {
		name       string
		mode       string
		signals    policyClassifierSignals
		status     int
		malformed  bool
		timeout    bool
		customPath bool
		wantPower  bool
	}{
		{name: "bounded edit", mode: "enforce", signals: lightSignals},
		{name: "planning", mode: "enforce", signals: powerSignals, wantPower: true},
		{name: "custom endpoint and auth", mode: "enforce", signals: powerSignals, customPath: true, wantPower: true},
		{name: "abstention", mode: "enforce", signals: abstainSignals, wantPower: true},
		{name: "invalid answers", mode: "enforce", malformed: true, wantPower: true},
		{name: "rejected", mode: "enforce", status: 422},
		{name: "redirect not followed", mode: "enforce", status: 307},
		{name: "rate limited", mode: "enforce", status: 429},
		{name: "overloaded", mode: "enforce", status: 529},
		{name: "timeout", mode: "enforce", timeout: true},
		{name: "off", mode: "off", signals: powerSignals},
	} {
		t.Run(test.name, func(t *testing.T) {
			light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			power := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
			validResponse := policyTypeSafeTestResponse(t, test.signals)
			preflightResponse := policyTypeSafeTestResponse(t, lightSignals)
			var calls atomic.Int32
			release := make(chan struct{})
			defer close(release)
			wantPath := "/v1/systemone"
			if test.customPath {
				wantPath = "/v1/custom-evaluate"
			}
			evaluator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				if r.Method != http.MethodPost || r.URL.Path != wantPath {
					t.Errorf("request = %s %s", r.Method, r.URL.Path)
				}
				if r.Header.Get("X-Evaluation-Key") != "Token evaluator-secret" || r.Header.Get("X-Evaluation-Tenant") != "local-test" {
					t.Error("provider auth or extra headers were not applied")
				}
				if r.Header.Get("Authorization") != "" {
					t.Error("client authorization leaked to classifier")
				}
				raw, _ := io.ReadAll(r.Body)
				var body struct {
					Model     string                            `json:"model"`
					State     policyClassifierFacts             `json:"state"`
					Questions map[string]policyTypeSafeQuestion `json:"questions"`
				}
				if err := json.Unmarshal(raw, &body); err != nil {
					t.Error(err)
				}
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(raw, &fields)
				if len(fields) != 3 || body.Model != "test-evaluator" || body.State.SchemaVersion == "" || len(body.Questions) != 7 {
					t.Errorf("unexpected TypeSafe request: fields=%v model=%q questions=%d", fields, body.Model, len(body.Questions))
				}
				for name, question := range body.Questions {
					if question.Type != "choice" || len(question.Criteria) < 2 || !strings.Contains(question.Instructions, "current_user_task") {
						t.Errorf("question %q lacks its choice contract or task instructions", name)
					}
				}
				if strings.Contains(string(raw), "schema-secret") || strings.Contains(string(raw), "evaluator-secret") {
					t.Error("classifier facts leaked tool schema or credentials")
				}
				w.Header().Set("Content-Type", "application/json")
				if call == 1 {
					_, _ = w.Write(preflightResponse)
					return
				}
				if test.timeout {
					select {
					case <-r.Context().Done():
					case <-release:
					}
					return
				}
				if test.status != 0 {
					w.Header().Set("Location", "http://"+r.Host+"/redirected")
					w.WriteHeader(test.status)
					return
				}
				if test.malformed {
					_, _ = io.WriteString(w, `{"answers":{}}`)
					return
				}
				_, _ = w.Write(validResponse)
			}))
			t.Cleanup(evaluator.Close)
			cfg := policyTypeSafeTestConfig(light.server.URL, power.server.URL, evaluator.URL+"/v1", test.mode)
			cfg.Providers[2].AuthType = "api-key-header"
			cfg.Providers[2].AuthHeader = "X-Evaluation-Key"
			cfg.Providers[2].AuthPrefix = "Token"
			cfg.Providers[2].APIKeyEnv = "TEST_EVALUATION_KEY"
			cfg.Providers[2].ExtraHeaders = map[string]string{"X-Evaluation-Tenant": "local-test"}
			t.Setenv("TEST_EVALUATION_KEY", "evaluator-secret")
			if test.customPath {
				cfg.Providers[2].SystemOnePath = "/custom-evaluate"
			}
			if test.timeout {
				cfg.PolicyProfiles[0].Classifier.TimeoutMS = 100
			}
			h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), nil, WithProvidersConfig(cfg))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(h.BeginShutdown)
			if err := h.InitializePolicyRouting(t.Context()); err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"change one file"}],"tools":[{"type":"function","function":{"name":"edit","parameters":{"type":"object","description":"schema-secret"}}}]}`))
			request.Header.Set("Authorization", "Bearer client-secret")
			recorder := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(recorder, request)
			if recorder.Code != http.StatusOK || !strings.Contains(recorder.Body.String(), `"model":"coding-economy"`) {
				t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
			}
			lightCalls, _ := light.snapshot()
			powerCalls, _ := power.snapshot()
			if lightCalls+powerCalls != 1 || (powerCalls == 1) != test.wantPower {
				t.Fatalf("terminal calls = light:%d powerful:%d", lightCalls, powerCalls)
			}
			wantCalls := int32(2)
			if test.mode == "off" {
				wantCalls = 0
			}
			if calls.Load() != wantCalls {
				t.Fatalf("classifier sends = %d, want %d", calls.Load(), wantCalls)
			}
			stats := h.policyRoutingController.(*chatPolicyRoutingController).PolicyStatsSnapshot().Profiles[0].Totals
			if stats.PhysicalClassifierSends != int64(wantCalls) {
				t.Fatalf("reported classifier sends = %d", stats.PhysicalClassifierSends)
			}
			if test.status == 0 && !test.timeout && !test.malformed && test.mode != "off" {
				if stats.ClassifierUsage.TotalTokens != 28 {
					t.Fatalf("classifier usage = %+v", stats.ClassifierUsage)
				}
				var classifierUsage taskUsageTotals
				for _, row := range h.stats.snapshot().TaskUsage.ByKind {
					if row.Kind == "classifier" {
						classifierUsage = row.taskUsageTotals
					}
				}
				if classifierUsage.Sends != 2 || classifierUsage.Completed != 2 || classifierUsage.Errors != 0 || classifierUsage.ReportedUsageSends != 2 ||
					classifierUsage.Usage != (statsTokenUsage{PromptTokens: 20, CompletionTokens: 8, TotalTokens: 28}) {
					t.Fatalf("classifier task usage = %+v", classifierUsage)
				}
			}
			catalog := httptest.NewRecorder()
			h.HandleModels(catalog, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
			if catalog.Code != http.StatusOK || strings.Contains(catalog.Body.String(), "test-evaluator") {
				t.Fatalf("classifier leaked into catalog: %d %s", catalog.Code, catalog.Body.String())
			}
			direct := httptest.NewRecorder()
			h.HandleOpenAIChatCompletions(direct, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"test-evaluator","messages":[{"role":"user","content":"hello"}]}`)))
			if direct.Code != http.StatusBadRequest || calls.Load() != wantCalls {
				t.Fatalf("classifier was publicly callable: status=%d calls=%d", direct.Code, calls.Load())
			}
		})
	}
}

func TestPolicyTypeSafeObserveDoesNotWaitForClassifier(t *testing.T) {
	light := newPolicyIntegrationUpstream(t, policyClassifierSignals{})
	signals := policyClassifierSignals{TurnType: policyTurnTypePlanning, CodeScope: policyCodeScopeMultiFile, RiskLevel: policyRiskLevelHigh}
	response := policyTypeSafeTestResponse(t, signals)
	var calls atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	defer close(release)
	evaluator := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		_ = r.Body.Close()
		if calls.Add(1) > 1 {
			close(started)
			select {
			case <-r.Context().Done():
			case <-release:
			}
		}
		_, _ = w.Write(response)
	}))
	t.Cleanup(evaluator.Close)
	cfg := policyTypeSafeTestConfig(light.server.URL, light.server.URL, evaluator.URL, "observe")
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), nil, WithProvidersConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	defer h.BeginShutdown()
	if err := h.InitializePolicyRouting(t.Context()); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	recorder := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"coding-economy","messages":[{"role":"user","content":"plan a refactor"}]}`)).WithContext(ctx))
	if recorder.Code != http.StatusOK {
		t.Fatalf("response = %d %s", recorder.Code, recorder.Body.String())
	}
	if _, models := light.snapshot(); fmt.Sprint(models) != "[light-model]" {
		t.Fatalf("observe selected %v", models)
	}
	select {
	case <-started:
	case <-ctx.Done():
		t.Fatal("observe classifier did not start")
	}
}
