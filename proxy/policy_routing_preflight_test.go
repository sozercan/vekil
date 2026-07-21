package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPolicyRoutingPreflightDiagnostics(t *testing.T) {
	t.Run("observe identifies every failed route with sanitized errors", func(t *testing.T) {
		const (
			firstSecret  = "classifier-one-secret=sk-observe-one"
			secondSecret = "classifier-two-secret=sk-observe-two"
		)
		first := newPolicyPreflightFailureServer(t, http.StatusServiceUnavailable, firstSecret)
		second := newPolicyPreflightFailureServer(t, http.StatusTooManyRequests, secondSecret)

		cfg := policyIntegrationConfig(first.URL, second.URL, policyConfigModeObserve)
		trueValue := true
		cfg.Providers = append(cfg.Providers, ProviderConfig{
			ID:                         "classifier-two-provider",
			Type:                       string(providerTypeOpenAICompatible),
			BaseURL:                    second.URL,
			AuthType:                   string(providerAuthTypeNone),
			TrustDomain:                "org-ai",
			ClassifierNoStoreSupported: &trueValue,
		})
		cfg.ModelRoutes = append(cfg.ModelRoutes, ModelRouteConfig{
			ID:              "classifier-route-two",
			Exposure:        modelRouteExposureInternal,
			InternalPurpose: modelRouteInternalPurposePolicyClassifier,
			Name:            "Classifier Two",
			Endpoints:       []string{providerEndpointChatCompletions},
			Targets: []ModelRouteTargetConfig{{
				ID:            "classifier-two",
				Provider:      "classifier-two-provider",
				UpstreamModel: "classifier-model",
			}},
		})
		secondProfile := clonePolicyProfileConfig(cfg.PolicyProfiles[0])
		secondProfile.ID = "coding-policy-two"
		secondProfile.PublicID = "coding-economy-two"
		secondProfile.Classifier.Route = "classifier-route-two"
		cfg.PolicyProfiles = append(cfg.PolicyProfiles, secondProfile)

		h, err := NewProxyHandler(nil, nil,
			WithProvidersConfig(cfg),
			WithPolicyRoutingMode(PolicyRoutingModeObserve),
		)
		if err != nil {
			t.Fatal(err)
		}
		if err := h.InitializePolicyRouting(t.Context()); err != nil {
			t.Fatalf("InitializePolicyRouting() error = %v", err)
		}

		wantDiagnostic := `policy classifier preflight failed for route "classifier-route": policy classifier upstream server failure; policy classifier preflight failed for route "classifier-route-two": policy classifier rate limited`
		if got := h.PolicyRoutingReadinessDiagnostic(); got != wantDiagnostic {
			t.Fatalf("readiness diagnostic = %q, want %q", got, wantDiagnostic)
		}
		for _, secret := range []string{firstSecret, secondSecret} {
			if strings.Contains(h.PolicyRoutingReadinessDiagnostic(), secret) {
				t.Fatalf("readiness diagnostic leaked classifier response: %q", h.PolicyRoutingReadinessDiagnostic())
			}
		}

		ready := httptest.NewRecorder()
		h.HandleReadyz(ready, httptest.NewRequest(http.MethodGet, "/readyz", nil))
		if ready.Code != http.StatusServiceUnavailable {
			t.Fatalf("ready status = %d body=%s", ready.Code, ready.Body.String())
		}
		var payload struct {
			Status string `json:"status"`
			Error  string `json:"error"`
		}
		if err := json.Unmarshal(ready.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode ready response: %v", err)
		}
		if payload.Status != "not_ready" || payload.Error != wantDiagnostic {
			t.Fatalf("ready payload = %+v, want error %q", payload, wantDiagnostic)
		}
	})

	t.Run("enforce behavior remains unchanged", func(t *testing.T) {
		const secret = "classifier-enforce-secret=sk-enforce"
		upstream := newPolicyPreflightFailureServer(t, http.StatusServiceUnavailable, secret)
		h, err := NewProxyHandler(nil, nil,
			WithProvidersConfig(policyIntegrationConfig(upstream.URL, upstream.URL, policyConfigModeEnforce)),
			WithPolicyRoutingMode(PolicyRoutingModeEnforce),
		)
		if err != nil {
			t.Fatal(err)
		}

		err = h.InitializePolicyRouting(t.Context())
		wantError := `policy classifier preflight failed for route "classifier-route": policy classifier upstream server failure`
		if err == nil || err.Error() != wantError {
			t.Fatalf("InitializePolicyRouting() error = %v, want %q", err, wantError)
		}
		if got, want := h.PolicyRoutingReadinessDiagnostic(), "policy classifier preflight failed"; got != want {
			t.Fatalf("readiness diagnostic = %q, want %q", got, want)
		}
		if strings.Contains(err.Error(), secret) || strings.Contains(h.PolicyRoutingReadinessDiagnostic(), secret) {
			t.Fatalf("enforce preflight leaked classifier response: error=%q diagnostic=%q", err, h.PolicyRoutingReadinessDiagnostic())
		}
	})
}

func newPolicyPreflightFailureServer(t *testing.T, status int, responseBody string) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, responseBody, status)
	}))
	t.Cleanup(server.Close)
	return server
}
