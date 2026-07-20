package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func newDashboardConfigTestHandler(t *testing.T) (*ProxyHandler, ResolvedProvidersConfig) {
	return newDashboardConfigTestHandlerWithManagedStartup(t, nil)
}

func newDashboardConfigTestHandlerWithManagedStartup(t *testing.T, mutate func(*ProvidersConfig)) (*ProxyHandler, ResolvedProvidersConfig) {
	t.Helper()
	root := t.TempDir()
	bootstrapPath := filepath.Join(root, "providers.json")
	userConfigDir := filepath.Join(root, "config")
	body := `{
  "providers": [{
    "id": "local",
    "type": "openai-compatible",
    "default": true,
    "base_url": "https://provider.example/v1",
    "api_key": "top-secret-key",
    "auth_type": "bearer",
    "extra_headers": {"X-Secret-Header": "header-secret"},
    "model_discovery": "static",
    "models": [{"public_id": "demo", "deployment": "demo-upstream", "endpoints": ["/chat/completions"], "name": "Demo"}]
  }]
}`
	if err := os.WriteFile(bootstrapPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	resolveOptions := ProvidersConfigResolveOptions{BootstrapPath: bootstrapPath, UserConfigDir: userConfigDir, Mode: ProvidersConfigUseManaged}
	resolved, err := ResolveProvidersConfig(resolveOptions)
	if err != nil {
		t.Fatal(err)
	}
	if mutate != nil {
		store, err := NewManagedProvidersConfigStore(resolved.Bootstrap)
		if err != nil {
			t.Fatal(err)
		}
		managed := cloneProvidersConfigForValidation(resolved.Config)
		mutate(&managed)
		if _, err := store.Commit(context.Background(), resolved.Revision, managed); err != nil {
			t.Fatal(err)
		}
		resolved, err = ResolveProvidersConfig(resolveOptions)
		if err != nil {
			t.Fatal(err)
		}
		if !resolved.Managed {
			t.Fatal("managed startup override was not resolved")
		}
	}
	store, err := NewManagedProvidersConfigStore(resolved.Bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(resolved.Config),
		WithDashboardConfigSource(resolved, store),
	)
	if err != nil {
		t.Fatal(err)
	}
	h.ConfigureDashboardConfigAccess(true, true, "", "test")
	return h, resolved
}

func TestDashboardConfigReadRedactsSecrets(t *testing.T) {
	h, _ := newDashboardConfigTestHandler(t)
	r := httptest.NewRequest(http.MethodGet, "/dashboard/api/v1/config", nil)
	r = h.PinRuntimeRequest(r)
	w := httptest.NewRecorder()
	h.HandleDashboardConfigRead(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	for _, secret := range []string{"top-secret-key", "header-secret"} {
		if strings.Contains(body, secret) {
			t.Fatalf("read response leaked %q: %s", secret, body)
		}
	}
	var response dashboardConfigReadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if !response.Capability.Available || !response.Capability.Writable {
		t.Fatalf("capability = %+v", response.Capability)
	}
	if response.Revision == "" || response.CSRFToken == "" || len(response.SecretStates) < 2 {
		t.Fatalf("incomplete config response: %+v", response)
	}
	if etag := w.Header().Get("ETag"); etag == "" {
		t.Fatal("missing ETag")
	}
}

func TestDashboardConfigImportYAMLReturnsRedactedDraftWithoutMutation(t *testing.T) {
	h, resolved := newDashboardConfigTestHandler(t)
	initialGeneration := h.runtimeGeneration()
	initialRevision := h.runtimeRevision()
	bootstrapBefore, err := os.ReadFile(resolved.Bootstrap.Source.BootstrapPath)
	if err != nil {
		t.Fatal(err)
	}
	yamlBody := `schema_version: 2
providers:
  - id: imported
    type: openai-compatible
    default: true
    base_url: https://provider.example/v1
    api_key: fixture-a
    auth_type: bearer
    extra_headers:
      X-Secret: fixture-b
    model_discovery: static
    models:
      - public_id: demo
        deployment: demo-upstream
        endpoints: [/chat/completions]
model_routes: []
policy_profiles: []
`
	r := httptest.NewRequest(http.MethodPost, "/dashboard/api/v1/config/import", strings.NewReader(yamlBody))
	r.Header.Set("Content-Type", "application/yaml; charset=utf-8")
	w := httptest.NewRecorder()
	h.HandleDashboardConfigImport(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	for _, secret := range []string{"fixture-a", "fixture-b"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatalf("import response leaked %q: %s", secret, w.Body.String())
		}
	}
	var response dashboardConfigImportResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.SchemaVersion != ProvidersConfigSchemaVersion2 {
		t.Fatalf("schema version = %d, want 2", response.SchemaVersion)
	}
	if got, want := response.StrippedSecretPaths, []string{"/providers/imported/api_key", "/providers/imported/extra_headers/X-Secret"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("stripped secret paths = %v, want %v", got, want)
	}
	var imported map[string]any
	if err := json.Unmarshal(response.Config, &imported); err != nil {
		t.Fatal(err)
	}
	providers, _ := imported["providers"].([]any)
	provider, _ := providers[0].(map[string]any)
	if _, ok := provider["api_key"]; ok {
		t.Fatalf("redacted import retained api_key: %v", provider)
	}
	headers, _ := provider["extra_headers"].(map[string]any)
	if headers["X-Secret"] != "" {
		t.Fatalf("redacted header = %#v, want empty", headers["X-Secret"])
	}
	if h.runtimeGeneration() != initialGeneration || h.runtimeRevision() != initialRevision {
		t.Fatalf("import changed runtime: generation=%d revision=%s", h.runtimeGeneration(), h.runtimeRevision())
	}
	bootstrapAfter, err := os.ReadFile(resolved.Bootstrap.Source.BootstrapPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(bootstrapBefore, bootstrapAfter) {
		t.Fatal("YAML import changed the bootstrap file")
	}
	if _, err := os.Stat(resolved.Bootstrap.Source.ManagedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("YAML import created a managed override: %v", err)
	}
}

func TestDashboardConfigImportYAMLRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
		wantCode    ConfigErrorCode
	}{
		{name: "wrong content type", contentType: "application/json", body: `{}`, wantStatus: http.StatusUnsupportedMediaType, wantCode: ConfigErrorInvalidSource},
		{name: "empty", contentType: "application/yaml", body: " \n", wantStatus: http.StatusBadRequest, wantCode: ConfigErrorEmpty},
		{name: "duplicate", contentType: "text/yaml", body: "providers:\n  - id: p\n    id: q\n    type: copilot\n", wantStatus: http.StatusBadRequest, wantCode: ConfigErrorDuplicateField},
		{name: "multiple documents", contentType: "application/x-yaml", body: "providers: []\n---\nproviders: []\n", wantStatus: http.StatusBadRequest, wantCode: ConfigErrorTrailingValue},
		{name: "oversized", contentType: "application/yaml", body: strings.Repeat("x", dashboardConfigBodyLimit+1), wantStatus: http.StatusRequestEntityTooLarge, wantCode: ConfigErrorInvalidYAML},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			h, _ := newDashboardConfigTestHandler(t)
			r := httptest.NewRequest(http.MethodPost, "/dashboard/api/v1/config/import", strings.NewReader(tc.body))
			r.Header.Set("Content-Type", tc.contentType)
			w := httptest.NewRecorder()
			h.HandleDashboardConfigImport(w, r)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d body=%s, want %d", w.Code, w.Body.String(), tc.wantStatus)
			}
			var response struct {
				Error DashboardConfigError `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if response.Error.Code != tc.wantCode {
				t.Fatalf("error code = %q, want %q (body=%s)", response.Error.Code, tc.wantCode, w.Body.String())
			}
		})
	}
}

func TestDashboardConfigImportYAMLRequiresWritableCapability(t *testing.T) {
	h, _ := newDashboardConfigTestHandler(t)
	h.ConfigureDashboardConfigAccess(true, false, "read only", "test")
	r := httptest.NewRequest(http.MethodPost, "/dashboard/api/v1/config/import", strings.NewReader("providers: []\n"))
	r.Header.Set("Content-Type", "application/yaml")
	w := httptest.NewRecorder()
	h.HandleDashboardConfigImport(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
}

func TestDashboardConfigPolicyEligibilityIncludesInternalTerminalRoutes(t *testing.T) {
	h, _ := newDashboardConfigTestHandlerWithManagedStartup(t, func(cfg *ProvidersConfig) {
		cfg.SchemaVersion = ProvidersConfigSchemaVersion2
		routing := ModelRouteRoutingConfig{Mode: string(routeModePrimaryOnly), MaxTargetAttempts: 1, MaxUpstreamSends: 1}
		target := func(id string) []ModelRouteTargetConfig {
			return []ModelRouteTargetConfig{{ID: id, Provider: "local", UpstreamModel: "demo-upstream"}}
		}
		cfg.ModelRoutes = []ModelRouteConfig{
			{ID: "public-chat", PublicID: "public-chat", Endpoints: []string{providerEndpointChatCompletions}, Targets: target("public"), Routing: routing},
			{ID: "internal-chat", Exposure: modelRouteExposureInternal, Endpoints: []string{providerEndpointChatCompletions}, Targets: target("internal"), Routing: routing},
			{ID: "classifier-route", Exposure: modelRouteExposureInternal, InternalPurpose: modelRouteInternalPurposePolicyClassifier, Endpoints: []string{providerEndpointChatCompletions}, Targets: target("classifier"), Routing: routing},
			{ID: "responses-only", PublicID: "responses-only", Endpoints: []string{providerEndpointResponses}, Targets: target("responses"), Routing: routing},
		}
	})

	eligibility := readDashboardConfig(t, h).PolicyEligibility
	if eligibility == nil {
		t.Fatal("policy eligibility metadata is missing")
	}
	terminalIDs := make([]string, 0, len(eligibility.TerminalRoutes))
	for _, route := range eligibility.TerminalRoutes {
		terminalIDs = append(terminalIDs, route.ID)
		if route.ID == "internal-chat" && route.Exposure != modelRouteExposureInternal {
			t.Fatalf("internal terminal exposure = %q, want %q", route.Exposure, modelRouteExposureInternal)
		}
	}
	if got, want := strings.Join(terminalIDs, ","), "public-chat,internal-chat"; got != want {
		t.Fatalf("terminal eligibility = %q, want %q", got, want)
	}
	if len(eligibility.ClassifierRoutes) != 1 || eligibility.ClassifierRoutes[0].ID != "classifier-route" {
		t.Fatalf("classifier eligibility = %+v, want classifier-route only", eligibility.ClassifierRoutes)
	}
}

func TestDashboardConfigApplyPersistsThenPublishes(t *testing.T) {
	h, resolved := newDashboardConfigTestHandler(t)
	current := h.currentRuntime()
	candidate := cloneProvidersConfigForValidation(current.config)
	candidate.Providers[0].APIKey = ""
	candidate.Providers[0].ExtraHeaders["X-Secret-Header"] = ""
	candidate.Providers[0].Models[0].Name = "Demo Updated"
	// The HTTP wire config is allowed to be temporarily secretless; encode it
	// as ordinary JSON and let the server strict decoder plus secret merge
	// reconstruct the candidate from the exact base revision.
	configBody, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	envelope := map[string]any{
		"base_revision": current.revision,
		"config":        json.RawMessage(configBody),
		"secret_operations": []SecretOperation{
			{Path: "/providers/local/api_key", Operation: "keep"},
			{Path: "/providers/local/extra_headers/X-Secret-Header", Operation: "keep"},
		},
	}
	requestBody, _ := json.Marshal(envelope)
	r := httptest.NewRequest(http.MethodPost, "/dashboard/api/v1/config/applies", bytes.NewReader(requestBody))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("If-Match", `"`+current.revision+`"`)
	w := httptest.NewRecorder()
	h.HandleDashboardConfigApply(w, r)
	if w.Code != http.StatusAccepted {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	var receipt ApplyReceipt
	if err := json.Unmarshal(w.Body.Bytes(), &receipt); err != nil {
		t.Fatal(err)
	}
	status := waitDashboardConfigApply(t, h.runtimeControl, receipt.ID)
	if status.State != ApplyStateSucceeded {
		t.Fatalf("apply status = %+v", status)
	}
	if h.runtimeGeneration() != current.generation+1 || h.runtimeRevision() == current.revision {
		t.Fatalf("runtime did not publish one new generation: generation=%d revision=%s", h.runtimeGeneration(), h.runtimeRevision())
	}
	active := h.currentProvidersConfig()
	if active.Providers[0].Models[0].Name != "Demo Updated" || active.Providers[0].APIKey != "top-secret-key" || active.Providers[0].ExtraHeaders["X-Secret-Header"] != "header-secret" {
		t.Fatalf("active config lost edits or secrets: %+v", active.Providers[0])
	}
	if _, err := os.Stat(resolved.Bootstrap.Source.ManagedPath); err != nil {
		t.Fatalf("managed file not committed: %v", err)
	}
	if response := readDashboardConfig(t, h); response.Source == nil || !response.Source.ManagedActive {
		t.Fatalf("managed_active after apply = %+v, want true", response.Source)
	}
}

func TestDashboardConfigRejectsStaleRevision(t *testing.T) {
	h, _ := newDashboardConfigTestHandler(t)
	cfg, err := json.Marshal(h.currentProvidersConfig())
	if err != nil {
		t.Fatal(err)
	}
	body, _ := json.Marshal(map[string]any{"base_revision": "cfg_stale", "config": json.RawMessage(cfg)})
	r := httptest.NewRequest(http.MethodPost, "/dashboard/api/v1/config/validate", bytes.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("If-Match", `"cfg_stale"`)
	w := httptest.NewRecorder()
	h.HandleDashboardConfigValidate(w, r)
	if w.Code != http.StatusPreconditionFailed {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if h.runtimeGeneration() != 1 {
		t.Fatal("stale validation changed the active runtime")
	}
}

func waitDashboardConfigApply(t *testing.T, control RuntimeControl, id string) ApplyStatus {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if status, ok := control.Status(id); ok && applyStateTerminal(status.State) {
			return status
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("apply %s did not finish", id)
	return ApplyStatus{}
}

func readDashboardConfig(t *testing.T, h *ProxyHandler) dashboardConfigReadResponse {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/dashboard/api/v1/config", nil)
	r = h.PinRuntimeRequest(r)
	w := httptest.NewRecorder()
	h.HandleDashboardConfigRead(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("config read status = %d body=%s", w.Code, w.Body.String())
	}
	var response dashboardConfigReadResponse
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

func TestDashboardConfigPendingApplyKeepsActiveRuntimeAndRejectsSecondApply(t *testing.T) {
	h, _ := newDashboardConfigTestHandler(t)
	current := h.currentRuntime()
	candidate := cloneProvidersConfigForValidation(current.config)
	candidate.Providers[0].APIKey = ""
	candidate.Providers[0].ExtraHeaders["X-Secret-Header"] = ""
	candidate.Providers[0].ModelDiscovery = "openai"
	operations := []SecretOperation{
		{Path: "/providers/local/api_key", Operation: "keep"},
		{Path: "/providers/local/extra_headers/X-Secret-Header", Operation: "keep"},
	}

	started := make(chan struct{})
	release := make(chan struct{})
	h.client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		select {
		case <-release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader(`{"object":"list","data":[]}`)),
			Request:    req,
		}, nil
	})}

	receipt, err := h.runtimeControl.Submit(ApplyRequest{BaseRevision: current.revision, Config: candidate, SecretOperations: operations})
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		status, _ := h.runtimeControl.Status(receipt.ID)
		t.Fatalf("candidate discovery did not start; status=%+v", status)
	}
	if got := h.currentRuntime(); got != current {
		t.Fatal("pending candidate replaced the active runtime")
	}
	if _, err := h.runtimeControl.Submit(ApplyRequest{BaseRevision: current.revision, Config: candidate, SecretOperations: operations}); !errors.Is(err, errConfigApplyInProgress) {
		t.Fatalf("second apply error = %v, want apply_in_progress", err)
	}
	close(release)
	status := waitDashboardConfigApply(t, h.runtimeControl, receipt.ID)
	if status.State != ApplyStateSucceeded {
		t.Fatalf("apply status = %+v", status)
	}
}

func TestDashboardConfigResetRestoresBootstrapGeneration(t *testing.T) {
	h, resolved := newDashboardConfigTestHandler(t)
	base := h.currentRuntime()
	candidate := cloneProvidersConfigForValidation(base.config)
	candidate.Providers[0].APIKey = ""
	candidate.Providers[0].ExtraHeaders["X-Secret-Header"] = ""
	candidate.Providers[0].Models[0].Name = "Managed Name"
	operations := []SecretOperation{
		{Path: "/providers/local/api_key", Operation: "keep"},
		{Path: "/providers/local/extra_headers/X-Secret-Header", Operation: "keep"},
	}
	receipt, err := h.runtimeControl.Submit(ApplyRequest{BaseRevision: base.revision, Config: candidate, SecretOperations: operations})
	if err != nil {
		t.Fatal(err)
	}
	if status := waitDashboardConfigApply(t, h.runtimeControl, receipt.ID); status.State != ApplyStateSucceeded {
		t.Fatalf("apply status = %+v", status)
	}
	managed := h.currentRuntime()
	if managed.config.Providers[0].Models[0].Name != "Managed Name" {
		t.Fatal("managed generation was not published")
	}

	resetReceipt, err := h.runtimeControl.Reset(ResetRequest{BaseRevision: managed.revision})
	if err != nil {
		t.Fatal(err)
	}
	if status := waitDashboardConfigApply(t, h.runtimeControl, resetReceipt.ID); status.State != ApplyStateSucceeded {
		t.Fatalf("reset status = %+v", status)
	}
	reset := h.currentRuntime()
	if reset.generation != managed.generation+1 || reset.revision != resolved.Bootstrap.Revision {
		t.Fatalf("reset runtime = generation %d revision %s", reset.generation, reset.revision)
	}
	if reset.config.Providers[0].Models[0].Name != "Demo" {
		t.Fatalf("reset config name = %q, want Demo", reset.config.Providers[0].Models[0].Name)
	}
	if _, err := os.Stat(resolved.Bootstrap.Source.ManagedPath); !os.IsNotExist(err) {
		t.Fatalf("managed override still exists after reset: %v", err)
	}
}

func TestDashboardConfigResetClearsManagedActiveFromStartupOverride(t *testing.T) {
	h, resolved := newDashboardConfigTestHandlerWithManagedStartup(t, func(cfg *ProvidersConfig) {
		cfg.Providers[0].Models[0].Name = "Managed Startup Name"
	})
	if response := readDashboardConfig(t, h); response.Source == nil || !response.Source.ManagedActive {
		t.Fatalf("managed_active at managed startup = %+v, want true", response.Source)
	}

	current := h.currentRuntime()
	resetReceipt, err := h.runtimeControl.Reset(ResetRequest{BaseRevision: current.revision})
	if err != nil {
		t.Fatal(err)
	}
	if status := waitDashboardConfigApply(t, h.runtimeControl, resetReceipt.ID); status.State != ApplyStateSucceeded {
		t.Fatalf("reset status = %+v", status)
	}
	if reset := h.currentRuntime(); reset.revision != resolved.Bootstrap.Revision {
		t.Fatalf("reset revision = %q, want %q", reset.revision, resolved.Bootstrap.Revision)
	}
	if response := readDashboardConfig(t, h); response.Source == nil || response.Source.ManagedActive {
		t.Fatalf("managed_active after reset = %+v, want false", response.Source)
	}
}

func TestDashboardConfigAssetsAreAllowlistedAndNoStore(t *testing.T) {
	h := &ProxyHandler{}
	page := httptest.NewRecorder()
	h.HandleDashboardConfig(page, httptest.NewRequest(http.MethodGet, "/dashboard/config", nil))
	if page.Code != http.StatusOK || !strings.Contains(page.Body.String(), "/dashboard/config.js") {
		t.Fatalf("config page status=%d body=%s", page.Code, page.Body.String())
	}
	if page.Header().Get("Cache-Control") != "no-store" || page.Header().Get("Content-Security-Policy") == "" {
		t.Fatalf("config page headers = %v", page.Header())
	}
	for _, path := range []string{"/dashboard/config.js", "/dashboard/config.css"} {
		w := httptest.NewRecorder()
		h.HandleDashboardAsset(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK || w.Header().Get("Cache-Control") != "no-store" || w.Body.Len() == 0 {
			t.Fatalf("asset %s status=%d headers=%v bytes=%d", path, w.Code, w.Header(), w.Body.Len())
		}
	}
	unknown := httptest.NewRecorder()
	h.HandleDashboardAsset(unknown, httptest.NewRequest(http.MethodGet, "/dashboard/secrets.json", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown asset status=%d", unknown.Code)
	}
}

func TestDashboardSecretOperationsRequireCompatibleExplicitIntent(t *testing.T) {
	base := ProvidersConfig{Providers: []ProviderConfig{{
		ID:           "p",
		Type:         string(providerTypeOpenAICompatible),
		BaseURL:      "https://provider.example/v1",
		AuthType:     string(providerAuthTypeBearer),
		APIKey:       "secret",
		ExtraHeaders: map[string]string{"X-Secret": "header"},
	}}}
	redacted := cloneProvidersConfigForValidation(base)
	redacted.Providers[0].APIKey = ""
	redacted.Providers[0].ExtraHeaders["X-Secret"] = ""

	tests := []struct {
		name       string
		mutate     func(*ProvidersConfig)
		operations []SecretOperation
		wantCode   ConfigErrorCode
	}{
		{name: "missing operations", wantCode: ConfigErrorCode("missing_secret_operation")},
		{name: "destination change cannot keep", mutate: func(cfg *ProvidersConfig) { cfg.Providers[0].BaseURL = "https://other.example/v1" }, operations: []SecretOperation{{Path: "/providers/p/api_key", Operation: "keep"}, {Path: "/providers/p/extra_headers/X-Secret", Operation: "keep"}}, wantCode: ConfigErrorCode("incompatible_secret_keep")},
		{name: "placeholder cannot be set", operations: []SecretOperation{{Path: "/providers/p/api_key", Operation: "set", Value: "***"}, {Path: "/providers/p/extra_headers/X-Secret", Operation: "clear"}}, wantCode: ConfigErrorCode("invalid_secret_operation")},
		{name: "raw secret rejected", mutate: func(cfg *ProvidersConfig) { cfg.Providers[0].APIKey = "new-secret" }, operations: []SecretOperation{{Path: "/providers/p/api_key", Operation: "clear"}, {Path: "/providers/p/extra_headers/X-Secret", Operation: "clear"}}, wantCode: ConfigErrorCode("secret_in_config")},
		{name: "url userinfo rejected", mutate: func(cfg *ProvidersConfig) { cfg.Providers[0].BaseURL = "https://user:pass@provider.example/v1" }, operations: []SecretOperation{{Path: "/providers/p/api_key", Operation: "set", Value: "replacement"}, {Path: "/providers/p/extra_headers/X-Secret", Operation: "clear"}}, wantCode: ConfigErrorInvalidConfig},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			candidate := cloneProvidersConfigForValidation(redacted)
			if tc.mutate != nil {
				tc.mutate(&candidate)
			}
			_, err := mergeDashboardConfigCandidate(base, candidate, tc.operations)
			var configErr *ConfigError
			if !errors.As(err, &configErr) || configErr.Code != tc.wantCode {
				t.Fatalf("error = %v, want code %q", err, tc.wantCode)
			}
		})
	}
}

func TestDashboardConfigShutdownCancelsBeforePersistencePublication(t *testing.T) {
	h, resolved := newDashboardConfigTestHandler(t)
	base := h.currentRuntime()
	candidate := cloneProvidersConfigForValidation(base.config)
	candidate.Providers[0].APIKey = ""
	candidate.Providers[0].ExtraHeaders["X-Secret-Header"] = ""
	candidate.Providers[0].Models[0].Name = "Must Not Publish"
	operations := []SecretOperation{
		{Path: "/providers/local/api_key", Operation: "keep"},
		{Path: "/providers/local/extra_headers/X-Secret-Header", Operation: "keep"},
	}
	if err := ensureManagedProvidersConfigDirectory(filepath.Dir(resolved.Bootstrap.Source.ManagedPath)); err != nil {
		t.Fatal(err)
	}
	lock, err := acquireManagedProvidersConfigFileLock(context.Background(), resolved.Bootstrap.Source.ManagedPath+".lock")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.release() }()

	receipt, err := h.runtimeControl.Submit(ApplyRequest{BaseRevision: base.revision, Config: candidate, SecretOperations: operations})
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		status, _ := h.runtimeControl.Status(receipt.ID)
		if status.State == ApplyStatePersisting {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	shutdownDone := make(chan struct{})
	go func() {
		h.BeginShutdown()
		close(shutdownDone)
	}()
	select {
	case <-shutdownDone:
	case <-time.After(time.Second):
		t.Fatal("shutdown did not wait for the canceled commit transaction")
	}
	status := waitDashboardConfigApply(t, h.runtimeControl, receipt.ID)
	if status.State != ApplyStateCanceledShutdown {
		t.Fatalf("apply status = %+v, want canceled_shutdown", status)
	}
	if h.currentRuntime() != base {
		t.Fatal("shutdown-raced candidate was published")
	}
	if _, err := os.Stat(resolved.Bootstrap.Source.ManagedPath); !os.IsNotExist(err) {
		t.Fatalf("shutdown-raced candidate was persisted: %v", err)
	}
}

func TestDashboardConfigRouteApplyUpdatesModelsImmediately(t *testing.T) {
	h, _ := newDashboardConfigTestHandler(t)
	base := h.currentRuntime()
	candidate := cloneProvidersConfigForValidation(base.config)
	candidate.SchemaVersion = ProvidersConfigSchemaVersion2
	candidate.Providers[0].APIKey = ""
	candidate.Providers[0].ExtraHeaders["X-Secret-Header"] = ""
	candidate.ModelRoutes = []ModelRouteConfig{{
		ID:        "route-demo",
		PublicID:  "route-demo",
		Name:      "Routed Demo",
		Endpoints: []string{providerEndpointChatCompletions},
		Targets: []ModelRouteTargetConfig{{
			ID:            "primary",
			Provider:      "local",
			UpstreamModel: "demo-upstream",
		}},
		Routing: ModelRouteRoutingConfig{Mode: string(routeModePrimaryOnly), MaxTargetAttempts: 1, MaxUpstreamSends: 1},
	}}
	operations := []SecretOperation{
		{Path: "/providers/local/api_key", Operation: "keep"},
		{Path: "/providers/local/extra_headers/X-Secret-Header", Operation: "keep"},
	}
	receipt, err := h.runtimeControl.Submit(ApplyRequest{BaseRevision: base.revision, Config: candidate, SecretOperations: operations})
	if err != nil {
		t.Fatal(err)
	}
	if status := waitDashboardConfigApply(t, h.runtimeControl, receipt.ID); status.State != ApplyStateSucceeded {
		t.Fatalf("apply status = %+v", status)
	}

	r := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	r = h.PinRuntimeRequest(r)
	w := httptest.NewRecorder()
	h.HandleModels(w, r)
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"id":"route-demo"`) {
		t.Fatalf("models status=%d body=%s", w.Code, w.Body.String())
	}
}
