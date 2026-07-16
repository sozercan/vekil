package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func explicitRouteTestProvider(id, baseURL, apiKey string) *providerRuntime {
	return &providerRuntime{
		id:            id,
		kind:          providerTypeAzureOpenAI,
		baseURL:       baseURL,
		apiKey:        apiKey,
		paths:         providerEndpointPolicyFor(providerTypeAzureOpenAI).defaultEndpointPaths(),
		includeModels: map[string]struct{}{},
		excludeModels: map[string]struct{}{},
		staticModels:  map[string]providerModel{},
	}
}

func explicitRouteTestHandler(t *testing.T, client *http.Client, mode routeMode, maxTargets, maxSends int, providers ...*providerRuntime) (*ProxyHandler, *modelRoute) {
	t.Helper()
	targets := make([]targetBinding, 0, len(providers))
	providerMap := make(map[string]*providerRuntime, len(providers))
	providerOrder := make([]string, 0, len(providers))
	for i, provider := range providers {
		providerMap[provider.id] = provider
		providerOrder = append(providerOrder, provider.id)
		targets = append(targets, targetBinding{id: "target-" + provider.id, provider: provider, upstreamModel: "deployment-" + string(rune('a'+i))})
	}
	route := &modelRoute{
		public:  publicModelContract{id: "public-model", routeID: "public-route", endpoints: []string{providerEndpointResponses, providerEndpointChatCompletions}},
		targets: targets,
		policy:  routePolicy{mode: mode, maxTargetAttempts: maxTargets, maxUpstreamSends: maxSends},
	}
	registry, err := newModelRouteRegistry([]*modelRoute{route})
	if err != nil {
		t.Fatalf("newModelRouteRegistry() error = %v", err)
	}
	h := &ProxyHandler{
		client: client,
		providersState: &providerSetup{
			providers:          providerMap,
			providerOrder:      providerOrder,
			defaultProviderID:  providers[0].id,
			routes:             registry,
			models:             map[string]providerModel{},
			hasConfiguredState: true,
		},
		streamingUpstreamTimeout: time.Second,
	}
	h.initializeLifecycle()
	return h, route
}

func TestExplicitRoutePriorityFailoverOnAuthoritative429(t *testing.T) {
	t.Parallel()
	var primaryCalls, secondaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		if got := r.Header.Get("api-key"); got != "primary-key" {
			t.Errorf("primary api-key = %q", got)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if got := body["model"]; got != "deployment-a" {
			t.Errorf("primary model = %#v", got)
		}
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"primary quota","type":"rate_limit_error"}}`)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryCalls.Add(1)
		if got := r.Header.Get("api-key"); got != "secondary-key" {
			t.Errorf("secondary api-key = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("secondary leaked Authorization = %q", got)
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if got := body["model"]; got != "deployment-b" {
			t.Errorf("secondary model = %#v", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"resp-secondary","model":"deployment-b","status":"completed","output":[]}`)
	}))
	defer secondary.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePriorityFailover, 2, 2,
		explicitRouteTestProvider("primary", primary.URL, "primary-key"),
		explicitRouteTestProvider("secondary", secondary.URL, "secondary-key"),
	)
	op := newRouteOperation(route, context.Background())
	ctx := withRouteOperation(context.Background(), op)
	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model","input":"hello"}`), http.Header{"Authorization": []string{"Bearer client-secret"}}, "public-model", false)
	if err != nil {
		t.Fatalf("executeExplicitRouteRequest() error = %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || !strings.Contains(string(body), "resp-secondary") {
		t.Fatalf("response = %d %s", resp.StatusCode, body)
	}
	if primaryCalls.Load() != 1 || secondaryCalls.Load() != 1 {
		t.Fatalf("calls primary=%d secondary=%d", primaryCalls.Load(), secondaryCalls.Load())
	}
	if sends, switches, _ := op.snapshot(); sends != 2 || switches != 1 {
		t.Fatalf("operation sends=%d switches=%d", sends, switches)
	}
}

func TestExplicitRoutePrimaryOnlyNeverSwitches(t *testing.T) {
	t.Parallel()
	var secondaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"quota"}}`)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer secondary.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePrimaryOnly, 1, 1,
		explicitRouteTestProvider("primary", primary.URL, "one"),
		explicitRouteTestProvider("secondary", secondary.URL, "two"),
	)
	ctx := withRouteOperation(context.Background(), newRouteOperation(route, context.Background()))
	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
	if err != nil {
		t.Fatalf("execute error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if secondaryCalls.Load() != 0 {
		t.Fatalf("secondary calls = %d", secondaryCalls.Load())
	}
}

type routeTraceTransport struct {
	ambiguous bool
	fallback  http.RoundTripper
	calls     atomic.Int32
}

func (t *routeTraceTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	call := t.calls.Add(1)
	if call == 1 {
		if t.ambiguous {
			if trace := httptrace.ContextClientTrace(req.Context()); trace != nil && trace.WroteHeaders != nil {
				trace.WroteHeaders()
			}
		}
		return nil, errors.New("injected transport failure")
	}
	return t.fallback.RoundTrip(req)
}

func TestExplicitRoutePrewriteFailureCanSwitchButAmbiguousCannot(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name          string
		ambiguous     bool
		wantSecondary int32
		wantErr       bool
	}{
		{name: "prewrite", wantSecondary: 1},
		{name: "ambiguous", ambiguous: true, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var secondaryCalls atomic.Int32
			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				secondaryCalls.Add(1)
				_, _ = io.WriteString(w, `{"status":"completed"}`)
			}))
			defer secondary.Close()
			transport := &routeTraceTransport{ambiguous: tc.ambiguous, fallback: http.DefaultTransport}
			client := &http.Client{Transport: transport}
			h, route := explicitRouteTestHandler(t, client, routeModePriorityFailover, 2, 2,
				explicitRouteTestProvider("primary", "http://primary.invalid", "one"),
				explicitRouteTestProvider("secondary", secondary.URL, "two"),
			)
			ctx := withRouteOperation(context.Background(), newRouteOperation(route, context.Background()))
			resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
			if tc.wantErr {
				if err == nil {
					if resp != nil && resp.Body != nil {
						_ = resp.Body.Close()
					}
					t.Fatal("error = nil")
				}
			} else {
				if err != nil {
					t.Fatalf("error = %v", err)
				}
				_ = resp.Body.Close()
			}
			if got := secondaryCalls.Load(); got != tc.wantSecondary {
				t.Fatalf("secondary calls = %d, want %d", got, tc.wantSecondary)
			}
		})
	}
}

func TestExplicitRouteDoesNotFollowRedirect(t *testing.T) {
	t.Parallel()
	var redirectedCalls atomic.Int32
	redirected := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirectedCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer redirected.Close()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, redirected.URL, http.StatusTemporaryRedirect)
	}))
	defer primary.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePriorityFailover, 1, 1,
		explicitRouteTestProvider("primary", primary.URL, "one"),
	)
	ctx := withRouteOperation(context.Background(), newRouteOperation(route, context.Background()))
	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if redirectedCalls.Load() != 0 {
		t.Fatalf("redirect target calls = %d", redirectedCalls.Load())
	}
}

func TestExplicitRouteStreamingPreambleFailureSwitchesBeforeCommit(t *testing.T) {
	t.Parallel()
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-hidden\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"quota\"}}}\n\n")
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-visible\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-visible\",\"status\":\"completed\"}}\n\n")
	}))
	defer secondary.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePriorityFailover, 2, 2,
		explicitRouteTestProvider("primary", primary.URL, "one"),
		explicitRouteTestProvider("secondary", secondary.URL, "two"),
	)
	ctx := withRouteOperation(context.Background(), newRouteOperation(route, context.Background()))
	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model","stream":true}`), nil, "public-model", true)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), "resp-hidden") || !strings.Contains(string(body), "resp-visible") {
		t.Fatalf("stream body = %s", body)
	}
}

func TestConfiguredExplicitRouteHandleResponsesAndCatalog(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		primaryCalls.Add(1)
		if got := r.Header.Get("api-key"); got != "primary-key" {
			t.Errorf("primary api-key = %q", got)
		}
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"quota","type":"rate_limit_error"}}`)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		secondaryCalls.Add(1)
		if got := r.Header.Get("api-key"); got != "secondary-key" {
			t.Errorf("secondary api-key = %q", got)
		}
		rawBody, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(rawBody, &body)
		if got := body["model"]; got != "physical-secondary" {
			t.Errorf("secondary model = %#v", got)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Openai-Model", "physical-secondary")
		w.Header().Set("X-Openai-Model", "physical-secondary")
		_, _ = io.WriteString(w, `{"id":"resp_configured","model":"physical-secondary","status":"completed","output":[]}`)
	}))
	defer secondary.Close()

	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			SchemaVersion: 2,
			Providers: []ProviderConfig{
				{ID: "primary", Type: string(providerTypeAzureOpenAI), Default: true, BaseURL: primary.URL + "/openai/v1", APIKey: "primary-key"},
				{ID: "secondary", Type: string(providerTypeAzureOpenAI), BaseURL: secondary.URL + "/openai/v1", APIKey: "secondary-key"},
			},
			ModelRoutes: []ModelRouteConfig{{
				ID: "route-public", PublicID: "public-model", Name: "Public Model", Endpoints: []string{providerEndpointResponses},
				Targets: []ModelRouteTargetConfig{
					{ID: "primary-target", Provider: "primary", UpstreamModel: "physical-primary"},
					{ID: "secondary-target", Provider: "secondary", UpstreamModel: "physical-secondary"},
				},
				Routing: ModelRouteRoutingConfig{Mode: string(routeModePriorityFailover), MaxTargetAttempts: 2, MaxUpstreamSends: 2},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	defer h.BeginShutdown()

	requestCtx, summary := WithRequestSummary(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-model","input":"hello"}`)).WithContext(requestCtx)
	w := httptest.NewRecorder()
	h.HandleResponses(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if strings.Contains(w.Body.String(), "physical-secondary") || !strings.Contains(w.Body.String(), `"model":"public-model"`) {
		t.Fatalf("client body did not normalize public model: %s", w.Body.String())
	}
	if got := w.Header().Get("Openai-Model"); got != "public-model" {
		t.Fatalf("Openai-Model = %q", got)
	}
	if got := w.Header().Get("X-Openai-Model"); got != "public-model" {
		t.Fatalf("X-Openai-Model = %q", got)
	}
	if got := w.Header().Get("X-Vekil-Request-ID"); got == "" || got != summary.OperationID() {
		t.Fatalf("X-Vekil-Request-ID=%q summary=%q", got, summary.OperationID())
	}
	if primaryCalls.Load() != 1 || secondaryCalls.Load() != 1 {
		t.Fatalf("calls primary=%d secondary=%d", primaryCalls.Load(), secondaryCalls.Load())
	}
	if summary.RouteID() != "route-public" || summary.FinalTarget() != "secondary-target" || summary.UpstreamSendCount() != 2 || summary.TargetSwitchCount() != 1 {
		t.Fatalf("summary route=%q target=%q sends=%d switches=%d", summary.RouteID(), summary.FinalTarget(), summary.UpstreamSendCount(), summary.TargetSwitchCount())
	}

	entry, _, err := h.buildMergedModelsEntry(context.Background(), "", "")
	if err != nil {
		t.Fatalf("buildMergedModelsEntry() error = %v", err)
	}
	var catalog struct {
		Data []struct {
			ID      string `json:"id"`
			OwnedBy string `json:"owned_by"`
		} `json:"data"`
		Models []struct {
			Slug string `json:"slug"`
		} `json:"models"`
	}
	if err := json.Unmarshal(entry.body, &catalog); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	if len(catalog.Data) != 1 || catalog.Data[0].ID != "public-model" || catalog.Data[0].OwnedBy != "route-public" {
		t.Fatalf("catalog data = %+v", catalog.Data)
	}
	if len(catalog.Models) != 1 || catalog.Models[0].Slug != "public-model" {
		t.Fatalf("catalog models = %+v", catalog.Models)
	}
}

func TestConfiguredExplicitRouteBindsResponseStateToExactTarget(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		primaryCalls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"quota"}}`)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		call := secondaryCalls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if call == 2 && body["previous_response_id"] != "resp_bound" {
			t.Errorf("follow-up previous_response_id = %#v", body["previous_response_id"])
		}
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Codex-Turn-State", "turn-bound")
		_, _ = io.WriteString(w, `{"id":"resp_bound","model":"physical-secondary","status":"completed","output":[]}`)
	}))
	defer secondary.Close()

	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.New(logger.LevelError),
		WithProvidersConfig(ProvidersConfig{
			SchemaVersion: 2,
			Providers: []ProviderConfig{
				{ID: "primary", Type: string(providerTypeAzureOpenAI), Default: true, BaseURL: primary.URL + "/openai/v1", APIKey: "primary-key"},
				{ID: "secondary", Type: string(providerTypeAzureOpenAI), BaseURL: secondary.URL + "/openai/v1", APIKey: "secondary-key"},
			},
			ModelRoutes: []ModelRouteConfig{{
				ID: "route-public", PublicID: "public-model", Endpoints: []string{providerEndpointResponses},
				Targets: []ModelRouteTargetConfig{
					{ID: "primary-target", Provider: "primary", UpstreamModel: "physical-primary"},
					{ID: "secondary-target", Provider: "secondary", UpstreamModel: "physical-secondary"},
				},
				Routing: ModelRouteRoutingConfig{Mode: string(routeModePriorityFailover), MaxTargetAttempts: 2, MaxUpstreamSends: 2},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	defer h.BeginShutdown()

	first := httptest.NewRecorder()
	h.HandleResponses(first, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-model","input":"first"}`)))
	if first.Code != http.StatusOK || first.Header().Get("X-Codex-Turn-State") != "turn-bound" {
		t.Fatalf("first response = %d headers=%v body=%s", first.Code, first.Header(), first.Body.String())
	}

	secondReq := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-model","previous_response_id":"resp_bound","input":"next"}`))
	secondReq.Header.Set("X-Codex-Turn-State", "turn-bound")
	second := httptest.NewRecorder()
	h.HandleResponses(second, secondReq)
	if second.Code != http.StatusOK {
		t.Fatalf("second response = %d body=%s", second.Code, second.Body.String())
	}
	if primaryCalls.Load() != 1 || secondaryCalls.Load() != 2 {
		t.Fatalf("state-pinned calls primary=%d secondary=%d", primaryCalls.Load(), secondaryCalls.Load())
	}

	beforePrimary, beforeSecondary := primaryCalls.Load(), secondaryCalls.Load()
	unknown := httptest.NewRecorder()
	h.HandleResponses(unknown, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-model","previous_response_id":"resp_unknown","input":"next"}`)))
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown state status = %d body=%s", unknown.Code, unknown.Body.String())
	}
	if primaryCalls.Load() != beforePrimary || secondaryCalls.Load() != beforeSecondary {
		t.Fatalf("unknown state reached upstream: primary=%d secondary=%d", primaryCalls.Load(), secondaryCalls.Load())
	}
}

func TestExplicitRouteUncertified503DoesNotSwitch(t *testing.T) {
	var secondaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = io.WriteString(w, `{"error":{"message":"gateway unavailable"}}`)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer secondary.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePriorityFailover, 2, 2,
		explicitRouteTestProvider("primary", primary.URL, "one"),
		explicitRouteTestProvider("secondary", secondary.URL, "two"),
	)
	ctx := withRouteOperation(context.Background(), newRouteOperation(route, context.Background()))
	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if secondaryCalls.Load() != 0 {
		t.Fatalf("secondary calls = %d", secondaryCalls.Load())
	}
}

func TestVersion2ConfigRejectsAmbiguousDuplicateRequestKeysBeforeDispatch(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.New(logger.LevelError), WithProvidersConfig(ProvidersConfig{
		SchemaVersion: 2,
		Providers:     []ProviderConfig{{ID: "azure", Type: string(providerTypeAzureOpenAI), Default: true, BaseURL: upstream.URL + "/openai/v1", APIKey: "key"}},
		ModelRoutes: []ModelRouteConfig{{
			ID: "route", PublicID: "public-model", Endpoints: []string{providerEndpointResponses},
			Targets: []ModelRouteTargetConfig{{ID: "target", Provider: "azure", UpstreamModel: "physical"}},
		}},
	}))
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	defer h.BeginShutdown()

	w := httptest.NewRecorder()
	h.HandleResponses(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-model","model":"unknown","input":"hello"}`)))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}

func TestExplicitRouteStreamingFailureWithEmbeddedOutputDoesNotSwitch(t *testing.T) {
	var secondaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"output\":[{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"partial\"}]}],\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"late quota\"}}}\n\n")
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer secondary.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePriorityFailover, 2, 2,
		explicitRouteTestProvider("primary", primary.URL, "one"),
		explicitRouteTestProvider("secondary", secondary.URL, "two"),
	)
	ctx := withRouteOperation(context.Background(), newRouteOperation(route, context.Background()))
	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model","stream":true}`), nil, "public-model", true)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), "partial") {
		t.Fatalf("body = %s", body)
	}
	if secondaryCalls.Load() != 0 {
		t.Fatalf("secondary calls = %d", secondaryCalls.Load())
	}
}

func TestExplicitRouteHardPinnedTargetNeverRetriesOrSwitches(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer secondary.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePriorityFailover, 2, 4,
		explicitRouteTestProvider("primary", primary.URL, "one"),
		explicitRouteTestProvider("secondary", secondary.URL, "two"),
	)
	op := newRouteOperation(route, context.Background())
	if err := op.forcePinnedTarget("target-primary"); err != nil {
		t.Fatal(err)
	}
	ctx := withRouteOperation(context.Background(), op)
	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
	if err != nil {
		t.Fatalf("error = %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if primaryCalls.Load() != 1 || secondaryCalls.Load() != 0 {
		t.Fatalf("calls primary=%d secondary=%d", primaryCalls.Load(), secondaryCalls.Load())
	}
}

func TestVersion2RouteRegistryRejectsDynamicLegacyAliasCollisionAtomically(t *testing.T) {
	providerA := explicitRouteTestProvider("dynamic-a", "http://a.invalid", "a")
	providerB := explicitRouteTestProvider("dynamic-b", "http://b.invalid", "b")
	explicitProvider := explicitRouteTestProvider("explicit", "http://e.invalid", "e")
	explicit := &modelRoute{
		public:  publicModelContract{id: "unrelated-public", routeID: "explicit-route", endpoints: []string{providerEndpointResponses}},
		targets: []targetBinding{{id: "explicit-target", provider: explicitProvider, upstreamModel: "physical"}},
		policy:  routePolicy{mode: routeModePrimaryOnly, maxTargetAttempts: 1, maxUpstreamSends: 1},
	}
	registry, err := newModelRouteRegistry([]*modelRoute{explicit})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.addLegacyProvider(providerA, []providerModel{{publicID: "claude-sonnet-4-5", upstreamModel: "a", providerID: providerA.id}}); err != nil {
		t.Fatalf("initial add: %v", err)
	}
	before := registry.load()
	err = registry.addLegacyProvider(providerB, []providerModel{{publicID: "claude-sonnet-4.5", upstreamModel: "b", providerID: providerB.id}})
	if err == nil {
		t.Fatal("collision error = nil")
	}
	if registry.load() != before {
		t.Fatal("registry snapshot changed after rejected collision")
	}
	if _, ok := registry.lookup("claude-sonnet-4.5"); ok {
		t.Fatal("colliding model was published")
	}
}

func TestExplicitRouteDispatchGateClosesOnInboundCancellation(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePriorityFailover, 1, 1,
		explicitRouteTestProvider("primary", server.URL, "one"),
	)
	inbound, cancel := context.WithCancel(context.Background())
	cancel()
	op := newRouteOperation(route, inbound)
	ctx := withRouteOperation(context.Background(), op)
	_, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
	if err == nil || !strings.Contains(err.Error(), "client disconnected") {
		t.Fatalf("error = %v", err)
	}
	if calls.Load() != 0 {
		t.Fatalf("upstream calls = %d", calls.Load())
	}
}
