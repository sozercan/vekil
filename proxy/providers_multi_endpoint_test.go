package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/proxy/selector"
)

func TestMultiEndpointConfigLoading(t *testing.T) {
	cfg := `{
		"providers": [{
			"id": "azure-multi",
			"type": "azure-openai",
			"default": true,
			"api_version": "2024-12-01-preview",
			"selector": "weighted",
			"endpoints": [
				{
					"name": "east",
					"base_url": "https://east.openai.azure.com/openai",
					"api_key": "key-east",
					"weight": 2,
					"health": {
						"error_budget": "10/m",
						"cooldown": "30s"
					}
				},
				{
					"name": "west",
					"base_url": "https://west.openai.azure.com/openai",
					"api_key": "key-west",
					"weight": 1,
					"health": {
						"error_budget": "5/s",
						"cooldown": "1m"
					}
				}
			],
			"models": [
				{"public_id": "gpt-4o", "deployment": "gpt-4o", "endpoints": ["/chat/completions"]}
			]
		}]
	}`

	tmpFile := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(tmpFile, []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}

	providersCfg, err := LoadProvidersConfigFile(tmpFile)
	if err != nil {
		t.Fatalf("LoadProvidersConfigFile: %v", err)
	}

	if len(providersCfg.Providers) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(providersCfg.Providers))
	}
	p := providersCfg.Providers[0]
	if p.Selector != "weighted" {
		t.Errorf("selector = %q, want weighted", p.Selector)
	}
	if len(p.Endpoints) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(p.Endpoints))
	}
	if p.Endpoints[0].Name != "east" {
		t.Errorf("endpoint[0].Name = %q, want east", p.Endpoints[0].Name)
	}
	if p.Endpoints[0].Weight != 2 {
		t.Errorf("endpoint[0].Weight = %d, want 2", p.Endpoints[0].Weight)
	}
	if p.Endpoints[1].Health.ErrorBudget != "5/s" {
		t.Errorf("endpoint[1].Health.ErrorBudget = %q, want 5/s", p.Endpoints[1].Health.ErrorBudget)
	}
}

func TestMultiEndpointConfigLoadingYAML(t *testing.T) {
	cfg := `
providers:
  - id: azure-multi
    type: azure-openai
    default: true
    api_version: "2024-12-01-preview"
    selector: round_robin
    endpoints:
      - name: east
        base_url: https://east.openai.azure.com/openai
        api_key: key-east
        weight: 2
        health:
          error_budget: "10/m"
          cooldown: 30s
      - name: west
        base_url: https://west.openai.azure.com/openai
        api_key: key-west
        weight: 1
    models:
      - public_id: gpt-4o
        deployment: gpt-4o
        endpoints:
          - /chat/completions
`
	tmpFile := filepath.Join(t.TempDir(), "providers.yaml")
	if err := os.WriteFile(tmpFile, []byte(cfg), 0644); err != nil {
		t.Fatal(err)
	}

	providersCfg, err := LoadProvidersConfigFile(tmpFile)
	if err != nil {
		t.Fatalf("LoadProvidersConfigFile: %v", err)
	}

	if len(providersCfg.Providers) != 1 {
		t.Fatalf("expected 1 provider, got %d", len(providersCfg.Providers))
	}
	p := providersCfg.Providers[0]
	if p.Selector != "round_robin" {
		t.Errorf("selector = %q, want round_robin", p.Selector)
	}
	if len(p.Endpoints) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(p.Endpoints))
	}
}

func TestMultiEndpointBuildProviderRuntime(t *testing.T) {
	cfg := ProviderConfig{
		ID:         "azure-multi",
		Type:       "azure-openai",
		Default:    true,
		APIVersion: "2024-12-01-preview",
		Selector:   "least_latency",
		Endpoints: []ProviderEndpointConfig{
			{
				Name:    "east",
				BaseURL: "https://east.openai.azure.com/openai",
				APIKey:  "key-east",
				Weight:  3,
				Health:  ProviderEndpointHealthConf{ErrorBudget: "10/m", Cooldown: "30s"},
			},
			{
				Name:    "west",
				BaseURL: "https://west.openai.azure.com/openai",
				APIKey:  "key-west",
				Weight:  1,
				Health:  ProviderEndpointHealthConf{ErrorBudget: "5/m", Cooldown: "1m"},
			},
		},
		Models: []ProviderModelConfig{
			{PublicID: "gpt-4o", Deployment: "gpt-4o", Endpoints: []string{"/chat/completions"}},
		},
	}

	runtime, err := buildProviderRuntime(cfg, "https://copilot.example.com", nil)
	if err != nil {
		t.Fatalf("buildProviderRuntime: %v", err)
	}

	if len(runtime.endpoints) != 2 {
		t.Fatalf("expected 2 endpoints, got %d", len(runtime.endpoints))
	}
	if runtime.endpoints[0].name != "east" {
		t.Errorf("endpoint[0].name = %q, want east", runtime.endpoints[0].name)
	}
	if runtime.endpoints[0].apiKey != "key-east" {
		t.Errorf("endpoint[0].apiKey = %q, want key-east", runtime.endpoints[0].apiKey)
	}
	if runtime.endpoints[1].weight != 1 {
		t.Errorf("endpoint[1].weight = %d, want 1", runtime.endpoints[1].weight)
	}
	if runtime.endpointSelector == nil {
		t.Error("endpointSelector should not be nil")
	}
}

func TestMultiEndpointPickEndpoint(t *testing.T) {
	runtime := &providerRuntime{
		id:               "test",
		endpointSelector: selector.NewRoundRobin(),
		endpoints: []*providerEndpoint{
			{
				name:    "a",
				baseURL: "https://a.example.com",
				apiKey:  "key-a",
				health:  NewEndpointHealthTracker(DefaultErrorBudget, DefaultCooldown),
				sel:     &selector.Endpoint{Name: "a", Healthy: true},
			},
			{
				name:    "b",
				baseURL: "https://b.example.com",
				apiKey:  "key-b",
				health:  NewEndpointHealthTracker(DefaultErrorBudget, DefaultCooldown),
				sel:     &selector.Endpoint{Name: "b", Healthy: true},
			},
		},
	}

	// Round-robin should alternate.
	ep1 := runtime.pickEndpoint()
	ep2 := runtime.pickEndpoint()
	if ep1.name == ep2.name {
		t.Errorf("round robin should alternate, got %q and %q", ep1.name, ep2.name)
	}
}

func TestMultiEndpointQuarantineSkips(t *testing.T) {
	now := time.Now()
	healthA := NewEndpointHealthTracker(ErrorBudget{Count: 2, Window: time.Minute}, 30*time.Second)
	healthA.now = func() time.Time { return now }
	healthB := NewEndpointHealthTracker(ErrorBudget{Count: 10, Window: time.Minute}, 30*time.Second)
	healthB.now = func() time.Time { return now }

	runtime := &providerRuntime{
		id:               "test",
		endpointSelector: selector.NewRoundRobin(),
		endpoints: []*providerEndpoint{
			{
				name:    "a",
				baseURL: "https://a.example.com",
				health:  healthA,
				sel:     &selector.Endpoint{Name: "a", Healthy: true},
			},
			{
				name:    "b",
				baseURL: "https://b.example.com",
				health:  healthB,
				sel:     &selector.Endpoint{Name: "b", Healthy: true},
			},
		},
	}

	// Quarantine endpoint A.
	healthA.RecordFailure()
	healthA.RecordFailure()

	// All picks should go to B since A is quarantined.
	for range 5 {
		ep := runtime.pickEndpoint()
		if ep.name != "b" {
			t.Errorf("should pick b (a is quarantined), got %q", ep.name)
		}
	}
}

func TestMultiEndpointPickExcluding(t *testing.T) {
	runtime := &providerRuntime{
		id:               "test",
		endpointSelector: selector.NewRoundRobin(),
		endpoints: []*providerEndpoint{
			{
				name:    "a",
				baseURL: "https://a.example.com",
				health:  NewEndpointHealthTracker(DefaultErrorBudget, DefaultCooldown),
				sel:     &selector.Endpoint{Name: "a", Healthy: true},
			},
			{
				name:    "b",
				baseURL: "https://b.example.com",
				health:  NewEndpointHealthTracker(DefaultErrorBudget, DefaultCooldown),
				sel:     &selector.Endpoint{Name: "b", Healthy: true},
			},
		},
	}

	epA := runtime.endpoints[0]
	got := runtime.pickEndpointExcluding(epA)
	if got.name == "a" {
		// May happen if round-robin lands on A, but since we try multiple times
		// it should eventually get B.
		got = runtime.pickEndpointExcluding(epA)
	}
	if got.name != "b" {
		t.Errorf("pickEndpointExcluding(a) should prefer b, got %q", got.name)
	}
}

func TestMultiEndpointSingleEndpointNoSelector(t *testing.T) {
	runtime := &providerRuntime{
		id: "test",
	}

	ep := runtime.pickEndpoint()
	if ep != nil {
		t.Error("pickEndpoint should return nil for provider without endpoints")
	}
}

func TestMultiEndpointRetryPicksDifferentEndpoint(t *testing.T) {
	// Create two test servers.
	callCountA := 0
	serverA := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCountA++
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error": "rate limited"}`))
	}))
	defer serverA.Close()

	callCountB := 0
	serverB := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		callCountB++
		w.WriteHeader(http.StatusOK)
		_ = json.NewEncoder(w).Encode(map[string]string{"status": "ok"})
	}))
	defer serverB.Close()

	provider := &providerRuntime{
		id:               "test-multi",
		kind:             providerTypeOpenAICompatible,
		baseURL:          serverA.URL,
		authType:         providerAuthTypeNone,
		endpointSelector: selector.NewRoundRobin(),
		endpoints: []*providerEndpoint{
			{
				name:    "a",
				baseURL: serverA.URL,
				health:  NewEndpointHealthTracker(ErrorBudget{Count: 10, Window: time.Minute}, 30*time.Second),
				sel:     &selector.Endpoint{Name: "a", BaseURL: serverA.URL, Healthy: true},
			},
			{
				name:    "b",
				baseURL: serverB.URL,
				health:  NewEndpointHealthTracker(ErrorBudget{Count: 10, Window: time.Minute}, 30*time.Second),
				sel:     &selector.Endpoint{Name: "b", BaseURL: serverB.URL, Healthy: true},
			},
		},
	}

	h := &ProxyHandler{
		client:         http.DefaultClient,
		maxRetries:     2,
		retryBaseDelay: 1 * time.Millisecond,
	}

	resp, err := h.doWithRetryMultiEndpoint(provider, func(ep *providerEndpoint) func() (*http.Request, error) {
		return func() (*http.Request, error) {
			req, err := http.NewRequest(http.MethodPost, ep.baseURL+"/chat/completions", strings.NewReader(`{"model":"test"}`))
			if err != nil {
				return nil, err
			}
			req.Header.Set("Content-Type", "application/json")
			return req, nil
		}
	})

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	// Both servers should have been hit (A failed, then B succeeded).
	if callCountA == 0 {
		t.Error("server A should have been called at least once")
	}
	if callCountB == 0 {
		t.Error("server B should have been called")
	}
}

func TestMultiEndpointSingleEndpointConfigUnchanged(t *testing.T) {
	// Verify that a single-endpoint config (the current schema) still works
	// byte-identically without any endpoints[] field.
	cfg := ProviderConfig{
		ID:         "azure-single",
		Type:       "azure-openai",
		Default:    true,
		BaseURL:    "https://myresource.openai.azure.com/openai",
		APIKey:     "my-key",
		APIVersion: "2024-12-01-preview",
		Models: []ProviderModelConfig{
			{PublicID: "gpt-4o", Deployment: "gpt-4o", Endpoints: []string{"/chat/completions"}},
		},
	}

	runtime, err := buildProviderRuntime(cfg, "https://copilot.example.com", nil)
	if err != nil {
		t.Fatalf("buildProviderRuntime: %v", err)
	}

	// No multi-endpoint state should be set.
	if len(runtime.endpoints) != 0 {
		t.Errorf("expected 0 endpoints, got %d", len(runtime.endpoints))
	}
	if runtime.endpointSelector != nil {
		t.Error("endpointSelector should be nil for single-endpoint config")
	}
	if runtime.baseURL != "https://myresource.openai.azure.com/openai" {
		t.Errorf("baseURL = %q, want original value", runtime.baseURL)
	}
	if runtime.apiKey != "my-key" {
		t.Errorf("apiKey = %q, want my-key", runtime.apiKey)
	}
}

func TestMultiEndpointValidationErrors(t *testing.T) {
	tests := []struct {
		name      string
		endpoints []ProviderEndpointConfig
		wantErr   string
	}{
		{
			name: "empty name",
			endpoints: []ProviderEndpointConfig{
				{Name: "", BaseURL: "https://a.example.com"},
			},
			wantErr: "endpoint name is required",
		},
		{
			name: "duplicate name",
			endpoints: []ProviderEndpointConfig{
				{Name: "east", BaseURL: "https://a.example.com", APIKey: "k1"},
				{Name: "east", BaseURL: "https://b.example.com", APIKey: "k2"},
			},
			wantErr: "duplicate endpoint name",
		},
		{
			name: "empty base_url",
			endpoints: []ProviderEndpointConfig{
				{Name: "east", BaseURL: ""},
			},
			wantErr: "base_url is required",
		},
		{
			name: "invalid error budget",
			endpoints: []ProviderEndpointConfig{
				{Name: "east", BaseURL: "https://a.example.com", Health: ProviderEndpointHealthConf{ErrorBudget: "bad"}},
			},
			wantErr: "invalid error budget",
		},
		{
			name: "invalid cooldown",
			endpoints: []ProviderEndpointConfig{
				{Name: "east", BaseURL: "https://a.example.com", Health: ProviderEndpointHealthConf{Cooldown: "not-a-duration"}},
			},
			wantErr: "invalid cooldown",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := buildProviderEndpoints("test-provider", tt.endpoints)
			if err == nil {
				t.Fatal("expected error, got nil")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error %q does not contain %q", err.Error(), tt.wantErr)
			}
		})
	}
}

func TestMultiEndpointLatencyEWMAUpdate(t *testing.T) {
	ep := &providerEndpoint{
		name: "test",
		sel:  &selector.Endpoint{Name: "test", Healthy: true},
	}

	// First measurement initializes.
	updateEndpointLatencyEWMA(ep, 100*time.Millisecond)
	if ep.sel.LoadLatencyEWMA() != 100*time.Millisecond {
		t.Errorf("first EWMA = %v, want 100ms", ep.sel.LoadLatencyEWMA())
	}

	// Second measurement applies smoothing.
	updateEndpointLatencyEWMA(ep, 200*time.Millisecond)
	// Expected: 0.3*200 + 0.7*100 = 60 + 70 = 130ms
	expected := time.Duration(0.3*float64(200*time.Millisecond) + 0.7*float64(100*time.Millisecond))
	if ep.sel.LoadLatencyEWMA() != expected {
		t.Errorf("second EWMA = %v, want %v", ep.sel.LoadLatencyEWMA(), expected)
	}
}
