package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

const (
	routeIsolationPublicModel = "release-matrix-public-model"
	routeIsolationPrimaryHost = "release-matrix-primary.example"
	routeIsolationBackupHost  = "release-matrix-backup.example"
)

type routeIsolationAzureTokenSource struct {
	token string
	calls atomic.Int32
}

func (s *routeIsolationAzureTokenSource) AccessToken(context.Context) (string, error) {
	s.calls.Add(1)
	return s.token, nil
}

type routeIsolationRequest struct {
	method string
	url    string
	host   string
	path   string
	query  string
	header http.Header
	body   []byte
}

type routeIsolationTransport struct {
	mu       sync.Mutex
	requests []routeIsolationRequest
}

func (t *routeIsolationTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	defer func() { _ = req.Body.Close() }()
	body, err := io.ReadAll(req.Body)
	if err != nil {
		return nil, fmt.Errorf("read route isolation request: %w", err)
	}

	captured := routeIsolationRequest{
		method: req.Method,
		url:    req.URL.String(),
		host:   req.URL.Hostname(),
		path:   req.URL.Path,
		query:  req.URL.RawQuery,
		header: req.Header.Clone(),
		body:   append([]byte(nil), body...),
	}
	t.mu.Lock()
	t.requests = append(t.requests, captured)
	t.mu.Unlock()

	switch captured.host {
	case routeIsolationPrimaryHost:
		return routeIsolationHTTPResponse(req, http.StatusTooManyRequests, `{"error":{"message":"primary quota","type":"rate_limit_error"}}`), nil
	case routeIsolationBackupHost:
		return routeIsolationHTTPResponse(req, http.StatusOK, `{"id":"resp-backup","object":"response","status":"completed","output":[]}`), nil
	default:
		return nil, fmt.Errorf("unexpected route isolation host %q", captured.host)
	}
}

func (t *routeIsolationTransport) snapshot() []routeIsolationRequest {
	t.mu.Lock()
	defer t.mu.Unlock()

	requests := make([]routeIsolationRequest, len(t.requests))
	for i, request := range t.requests {
		requests[i] = request
		requests[i].header = request.header.Clone()
		requests[i].body = append([]byte(nil), request.body...)
	}
	return requests
}

func routeIsolationHTTPResponse(req *http.Request, status int, body string) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	return &http.Response{
		StatusCode:    status,
		Status:        fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

type routeIsolationExpectation struct {
	path            string
	model           string
	headers         map[string]string
	absentHeaders   []string
	forbiddenValues []string
}

func TestExplicitRouteFailoverIsolatesTargetCredentialsAndHeaders(t *testing.T) {
	inboundSecrets := []string{
		"release-matrix-inbound-bearer-one",
		"release-matrix-inbound-bearer-two",
		"release-matrix-inbound-api-key-one",
		"release-matrix-inbound-api-key-two",
		"release-matrix-inbound-x-api-key-one",
		"release-matrix-inbound-x-api-key-two",
	}

	tests := []struct {
		name         string
		primary      ProviderConfig
		backup       ProviderConfig
		tokenSources map[string]*routeIsolationAzureTokenSource
		primaryWant  routeIsolationExpectation
		backupWant   routeIsolationExpectation
	}{
		{
			name: "azure api key to azure entra",
			primary: ProviderConfig{
				ID:       "primary",
				Type:     string(providerTypeAzureOpenAI),
				BaseURL:  "https://" + routeIsolationPrimaryHost + "/api-key/openai/v1",
				AuthMode: string(providerAuthModeAPIKey),
				APIKey:   "release-matrix-primary-azure-key",
			},
			backup: ProviderConfig{
				ID:       "backup",
				Type:     string(providerTypeAzureOpenAI),
				BaseURL:  "https://" + routeIsolationBackupHost + "/entra/openai/v1",
				AuthMode: string(providerAuthModeAzureIdentity),
			},
			tokenSources: map[string]*routeIsolationAzureTokenSource{
				"backup": {token: "release-matrix-backup-entra-token"},
			},
			primaryWant: routeIsolationExpectation{
				path:  "/api-key/openai/v1/responses",
				model: "release-matrix-primary-azure-model",
				headers: map[string]string{
					"api-key": "release-matrix-primary-azure-key",
				},
				absentHeaders:   []string{"Authorization"},
				forbiddenValues: []string{"release-matrix-backup-entra-token"},
			},
			backupWant: routeIsolationExpectation{
				path:  "/entra/openai/v1/responses",
				model: "release-matrix-backup-entra-model",
				headers: map[string]string{
					"Authorization": "Bearer release-matrix-backup-entra-token",
				},
				absentHeaders:   []string{"api-key"},
				forbiddenValues: []string{"release-matrix-primary-azure-key"},
			},
		},
		{
			name: "azure entra to azure api key",
			primary: ProviderConfig{
				ID:       "primary",
				Type:     string(providerTypeAzureOpenAI),
				BaseURL:  "https://" + routeIsolationPrimaryHost + "/entra/openai/v1",
				AuthMode: string(providerAuthModeAzureIdentity),
			},
			backup: ProviderConfig{
				ID:       "backup",
				Type:     string(providerTypeAzureOpenAI),
				BaseURL:  "https://" + routeIsolationBackupHost + "/api-key/openai/v1",
				AuthMode: string(providerAuthModeAPIKey),
				APIKey:   "release-matrix-backup-azure-key",
			},
			tokenSources: map[string]*routeIsolationAzureTokenSource{
				"primary": {token: "release-matrix-primary-entra-token"},
			},
			primaryWant: routeIsolationExpectation{
				path:  "/entra/openai/v1/responses",
				model: "release-matrix-primary-entra-model",
				headers: map[string]string{
					"Authorization": "Bearer release-matrix-primary-entra-token",
				},
				absentHeaders:   []string{"api-key"},
				forbiddenValues: []string{"release-matrix-backup-azure-key"},
			},
			backupWant: routeIsolationExpectation{
				path:  "/api-key/openai/v1/responses",
				model: "release-matrix-backup-api-key-model",
				headers: map[string]string{
					"api-key": "release-matrix-backup-azure-key",
				},
				absentHeaders:   []string{"Authorization"},
				forbiddenValues: []string{"release-matrix-primary-entra-token"},
			},
		},
		{
			name: "generic bearer to custom api key header",
			primary: ProviderConfig{
				ID:            "primary",
				Type:          string(providerTypeOpenAICompatible),
				BaseURL:       "https://" + routeIsolationPrimaryHost,
				AuthType:      string(providerAuthTypeBearer),
				APIKey:        "release-matrix-primary-bearer-key",
				ResponsesPath: "/bearer/responses",
			},
			backup: ProviderConfig{
				ID:            "backup",
				Type:          string(providerTypeOpenAICompatible),
				BaseURL:       "https://" + routeIsolationBackupHost,
				AuthType:      string(providerAuthTypeAPIKeyHeader),
				AuthHeader:    "X-Release-Matrix-Key",
				AuthPrefix:    "Token",
				APIKey:        "release-matrix-backup-custom-key",
				ResponsesPath: "/custom-key/responses",
			},
			primaryWant: routeIsolationExpectation{
				path:  "/bearer/responses",
				model: "release-matrix-primary-bearer-model",
				headers: map[string]string{
					"Authorization": "Bearer release-matrix-primary-bearer-key",
				},
				absentHeaders:   []string{"X-Release-Matrix-Key", "api-key"},
				forbiddenValues: []string{"release-matrix-backup-custom-key"},
			},
			backupWant: routeIsolationExpectation{
				path:  "/custom-key/responses",
				model: "release-matrix-backup-custom-model",
				headers: map[string]string{
					"X-Release-Matrix-Key": "Token release-matrix-backup-custom-key",
				},
				absentHeaders:   []string{"Authorization", "api-key"},
				forbiddenValues: []string{"release-matrix-primary-bearer-key"},
			},
		},
		{
			name: "distinct provider extra headers",
			primary: ProviderConfig{
				ID:            "primary",
				Type:          string(providerTypeOpenAICompatible),
				BaseURL:       "https://" + routeIsolationPrimaryHost,
				AuthType:      string(providerAuthTypeNone),
				ResponsesPath: "/primary-extra/responses",
				ExtraHeaders: map[string]string{
					"X-Release-Matrix-Scope":        "release-matrix-primary-scope",
					"X-Release-Matrix-Primary-Only": "release-matrix-primary-only",
				},
			},
			backup: ProviderConfig{
				ID:            "backup",
				Type:          string(providerTypeOpenAICompatible),
				BaseURL:       "https://" + routeIsolationBackupHost,
				AuthType:      string(providerAuthTypeNone),
				ResponsesPath: "/backup-extra/responses",
				ExtraHeaders: map[string]string{
					"X-Release-Matrix-Scope":       "release-matrix-backup-scope",
					"X-Release-Matrix-Backup-Only": "release-matrix-backup-only",
				},
			},
			primaryWant: routeIsolationExpectation{
				path:  "/primary-extra/responses",
				model: "release-matrix-primary-extra-model",
				headers: map[string]string{
					"X-Release-Matrix-Scope":        "release-matrix-primary-scope",
					"X-Release-Matrix-Primary-Only": "release-matrix-primary-only",
				},
				absentHeaders: []string{"Authorization", "api-key", "X-Release-Matrix-Backup-Only"},
				forbiddenValues: []string{
					"release-matrix-backup-scope",
					"release-matrix-backup-only",
				},
			},
			backupWant: routeIsolationExpectation{
				path:  "/backup-extra/responses",
				model: "release-matrix-backup-extra-model",
				headers: map[string]string{
					"X-Release-Matrix-Scope":       "release-matrix-backup-scope",
					"X-Release-Matrix-Backup-Only": "release-matrix-backup-only",
				},
				absentHeaders: []string{"Authorization", "api-key", "X-Release-Matrix-Primary-Only"},
				forbiddenValues: []string{
					"release-matrix-primary-scope",
					"release-matrix-primary-only",
				},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tc.primary.Default = true
			transport := &routeIsolationTransport{}
			handler, route := newRouteIsolationHandler(t, transport, tc.primary, tc.backup, tc.primaryWant.model, tc.backupWant.model, tc.tokenSources)

			logicalBody := []byte(`{"model":"` + routeIsolationPublicModel + `","input":"release matrix","metadata":{"logical_request":"same"}}`)
			inboundHeaders := make(http.Header)
			inboundHeaders.Set("Authorization", "Bearer "+inboundSecrets[0])
			inboundHeaders.Add("Authorization", "Bearer "+inboundSecrets[1])
			inboundHeaders.Set("api-key", inboundSecrets[2])
			inboundHeaders.Add("api-key", inboundSecrets[3])
			inboundHeaders.Set("X-Api-Key", inboundSecrets[4])
			inboundHeaders.Add("X-Api-Key", inboundSecrets[5])
			inboundHeaders.Set("X-Release-Matrix-Shared", "release-matrix-shared-request-one")
			inboundHeaders.Add("X-Release-Matrix-Shared", "release-matrix-shared-request-two")

			inbound := context.Background()
			operation := newRouteOperation(route, inbound)
			ctx := withRouteOperation(inbound, operation)
			resp, err := handler.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, logicalBody, inboundHeaders, routeIsolationPublicModel, false)
			if err != nil {
				t.Fatalf("executeExplicitRouteRequest() error = %v", err)
			}
			defer func() { _ = resp.Body.Close() }()
			responseBody, err := io.ReadAll(resp.Body)
			if err != nil {
				t.Fatalf("read explicit route response: %v", err)
			}
			if resp.StatusCode != http.StatusOK || !strings.Contains(string(responseBody), "resp-backup") {
				t.Fatalf("explicit route response = %d %s, want backup success", resp.StatusCode, responseBody)
			}

			requests := transport.snapshot()
			if len(requests) != 2 {
				t.Fatalf("upstream send count = %d, want exactly 2", len(requests))
			}
			if got := countRouteIsolationRequests(requests, routeIsolationPrimaryHost); got != 1 {
				t.Fatalf("primary send count = %d, want 1", got)
			}
			if got := countRouteIsolationRequests(requests, routeIsolationBackupHost); got != 1 {
				t.Fatalf("backup send count = %d, want 1", got)
			}

			primaryRequest := routeIsolationRequestForHost(t, requests, routeIsolationPrimaryHost)
			backupRequest := routeIsolationRequestForHost(t, requests, routeIsolationBackupHost)
			assertRouteIsolationRequest(t, primaryRequest, tc.primaryWant, append(append([]string(nil), inboundSecrets...), tc.backupWant.model), true)
			assertRouteIsolationRequest(t, backupRequest, tc.backupWant, append(append([]string(nil), inboundSecrets...), tc.primaryWant.model), true)

			sends, switches, trace := operation.snapshot()
			if sends != 2 || switches != 1 || len(trace) != 2 {
				t.Fatalf("operation sends=%d switches=%d trace=%d, want 2/1/2", sends, switches, len(trace))
			}
			if trace[0].Decision != routeRetrySwitchTarget || trace[1].Decision != routeRetryAccepted {
				t.Fatalf("operation trace decisions = [%s %s], want [%s %s]", trace[0].Decision, trace[1].Decision, routeRetrySwitchTarget, routeRetryAccepted)
			}

			for providerID, source := range tc.tokenSources {
				if got := source.calls.Load(); got != 1 {
					t.Fatalf("Azure token source %q calls = %d, want 1", providerID, got)
				}
			}
		})
	}
}

func newRouteIsolationHandler(
	t *testing.T,
	transport http.RoundTripper,
	primary ProviderConfig,
	backup ProviderConfig,
	primaryModel string,
	backupModel string,
	tokenSources map[string]*routeIsolationAzureTokenSource,
) (*ProxyHandler, *modelRoute) {
	t.Helper()

	options := []Option{WithProvidersConfig(ProvidersConfig{
		SchemaVersion: ProvidersConfigSchemaVersion2,
		Providers:     []ProviderConfig{primary, backup},
		ModelRoutes: []ModelRouteConfig{{
			ID:        "release-matrix-route",
			PublicID:  routeIsolationPublicModel,
			Endpoints: []string{providerEndpointResponses},
			Targets: []ModelRouteTargetConfig{
				{ID: "primary-target", Provider: primary.ID, UpstreamModel: primaryModel},
				{ID: "backup-target", Provider: backup.ID, UpstreamModel: backupModel},
			},
			Routing: ModelRouteRoutingConfig{
				Mode:              string(routeModePriorityFailover),
				MaxTargetAttempts: 2,
				MaxUpstreamSends:  2,
			},
		}},
	})}
	if len(tokenSources) > 0 {
		options = append(options, func(handler *ProxyHandler) {
			handler.azureIdentityTokenSourceFactory = func(providerID, _ string) (azureTokenSource, error) {
				source := tokenSources[providerID]
				if source == nil {
					return nil, fmt.Errorf("missing route isolation Azure token source for provider %q", providerID)
				}
				return source, nil
			}
		})
	}

	handler, err := NewProxyHandler(
		auth.NewTestAuthenticator("release-matrix-copilot-token"),
		logger.New(logger.LevelError),
		options...,
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	handler.client = &http.Client{Transport: transport}
	t.Cleanup(handler.BeginShutdown)

	route, known := handler.resolveModelRouteForRequest(routeIsolationPublicModel, providerEndpointResponses)
	if !known || route == nil || route.legacy {
		t.Fatal("explicit release-matrix route was not resolved")
	}
	return handler, route
}

func countRouteIsolationRequests(requests []routeIsolationRequest, host string) int {
	count := 0
	for _, request := range requests {
		if request.host == host {
			count++
		}
	}
	return count
}

func routeIsolationRequestForHost(t *testing.T, requests []routeIsolationRequest, host string) routeIsolationRequest {
	t.Helper()
	for _, request := range requests {
		if request.host == host {
			return request
		}
	}
	t.Fatalf("no request captured for host %q", host)
	return routeIsolationRequest{}
}

func assertRouteIsolationRequest(t *testing.T, request routeIsolationRequest, want routeIsolationExpectation, forbiddenValues []string, wantSharedHeader bool) {
	t.Helper()

	if request.method != http.MethodPost {
		t.Errorf("%s method = %q, want POST", request.host, request.method)
	}
	if request.path != want.path {
		t.Errorf("%s path = %q, want %q (URL %s)", request.host, request.path, want.path, request.url)
	}
	wantURL := "https://" + request.host + want.path
	if request.url != wantURL {
		t.Errorf("%s URL = %q, want exactly %q", request.host, request.url, wantURL)
	}
	if request.query != "" {
		t.Errorf("%s query = %q, want empty", request.host, request.query)
	}
	if wantSharedHeader {
		assertRouteIsolationHeaderValues(t, request, "X-Release-Matrix-Shared", []string{
			"release-matrix-shared-request-one",
			"release-matrix-shared-request-two",
		})
	}
	for name, value := range want.headers {
		assertRouteIsolationHeaderValues(t, request, name, []string{value})
	}
	for _, name := range want.absentHeaders {
		if got := request.header.Values(name); len(got) != 0 {
			t.Errorf("%s leaked header %s values = %#v", request.host, name, got)
		}
	}
	for _, value := range append(append([]string(nil), want.forbiddenValues...), forbiddenValues...) {
		if location, got, found := routeIsolationRequestValueContaining(request, value); found {
			t.Errorf("%s leaked forbidden value %q through %s=%q", request.host, value, location, got)
		}
	}

	var payload struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(request.body, &payload); err != nil {
		t.Fatalf("decode %s request body: %v; body=%s", request.host, err, request.body)
	}
	if payload.Model != want.model {
		t.Errorf("%s model = %q, want %q; body=%s", request.host, payload.Model, want.model, request.body)
	}
}

func assertRouteIsolationHeaderValues(t *testing.T, request routeIsolationRequest, name string, want []string) {
	t.Helper()
	if got := request.header.Values(name); !reflect.DeepEqual(got, want) {
		t.Errorf("%s header %s values = %#v, want exactly %#v", request.host, name, got, want)
	}
}

func routeIsolationRequestValueContaining(request routeIsolationRequest, needle string) (string, string, bool) {
	if needle == "" {
		return "", "", false
	}
	if strings.Contains(request.query, needle) {
		return "query", request.query, true
	}
	if strings.Contains(request.url, needle) {
		return "URL", request.url, true
	}
	if strings.Contains(string(request.body), needle) {
		return "body", string(request.body), true
	}
	for name, values := range request.header {
		for index, value := range values {
			if strings.Contains(value, needle) {
				return fmt.Sprintf("header %s[%d]", name, index), value, true
			}
		}
	}
	return "", "", false
}

type routeIsolationRequestOptimizer struct {
	rewriteCalls atomic.Int32
	reduceCalls  atomic.Int32

	mu             sync.Mutex
	reduceRequests []ToolOutputReduceRequest
}

func (*routeIsolationRequestOptimizer) ID() string { return "route-isolation-request-optimizer" }

func (o *routeIsolationRequestOptimizer) RewriteCommand(context.Context, ToolCommandRewriteRequest) (ToolCommandRewriteResult, error) {
	o.rewriteCalls.Add(1)
	return ToolCommandRewriteResult{}, nil
}

func (o *routeIsolationRequestOptimizer) ReduceOutput(_ context.Context, request ToolOutputReduceRequest) (ToolOutputReduceResult, error) {
	o.reduceCalls.Add(1)
	o.mu.Lock()
	o.reduceRequests = append(o.reduceRequests, request)
	o.mu.Unlock()
	return ToolOutputReduceResult{Changed: true, Output: "release-matrix-optimized-output"}, nil
}

func (o *routeIsolationRequestOptimizer) snapshotReduceRequests() []ToolOutputReduceRequest {
	o.mu.Lock()
	defer o.mu.Unlock()
	return append([]ToolOutputReduceRequest(nil), o.reduceRequests...)
}

func TestHandleResponsesExplicitRouteFailoverRunsRequestOptimizerOnce(t *testing.T) {
	primary := ProviderConfig{
		ID:            "primary",
		Type:          string(providerTypeOpenAICompatible),
		Default:       true,
		BaseURL:       "https://" + routeIsolationPrimaryHost,
		AuthType:      string(providerAuthTypeBearer),
		APIKey:        "release-matrix-optimizer-primary-key",
		ResponsesPath: "/optimizer-primary/responses",
	}
	backup := ProviderConfig{
		ID:            "backup",
		Type:          string(providerTypeOpenAICompatible),
		BaseURL:       "https://" + routeIsolationBackupHost,
		AuthType:      string(providerAuthTypeBearer),
		APIKey:        "release-matrix-optimizer-backup-key",
		ResponsesPath: "/optimizer-backup/responses",
	}
	transport := &routeIsolationTransport{}
	handler, _ := newRouteIsolationHandler(
		t,
		transport,
		primary,
		backup,
		"release-matrix-optimizer-primary-model",
		"release-matrix-optimizer-backup-model",
		nil,
	)

	enabled := true
	optimizer := &routeIsolationRequestOptimizer{}
	handler.toolOptimizers = NewToolOptimizerManager(ToolOptimizersConfig{
		Enabled: true,
		Tools: ToolOptimizerToolsConfig{
			ShellFunctionCalls: ToolOptimizerShellFunctionCallsConfig{
				Enabled:        &enabled,
				Names:          []string{"shell_command"},
				CommandArgPath: "/command",
			},
		},
		OutputReduce: ToolOptimizerOutputConfig{
			Enabled:       true,
			MinInputBytes: 1,
		},
	}, []stagedToolOptimizer{{optimizer: optimizer, outputReduce: true}})
	handler.toolContexts = NewToolExecutionContextStore()

	body := `{
		"model":"` + routeIsolationPublicModel + `",
		"input":[
			{"type":"function_call","name":"shell_command","call_id":"call-release-matrix","arguments":"{\"command\":\"cat release.log\"}"},
			{"type":"function_call_output","call_id":"call-release-matrix","output":"release-matrix-unoptimized-output"}
		],
		"metadata":{"logical_request":"optimizer-failover"},
		"stream":false
	}`
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	responseRecorder := httptest.NewRecorder()

	handler.HandleResponses(responseRecorder, request)

	response := responseRecorder.Result()
	defer func() { _ = response.Body.Close() }()
	responseBody, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatalf("read handler response: %v", err)
	}
	if response.StatusCode != http.StatusOK {
		t.Fatalf("handler response = %d %s, want 200", response.StatusCode, responseBody)
	}

	if got := optimizer.reduceCalls.Load(); got != 1 {
		t.Fatalf("request optimizer reduce calls = %d, want exactly 1", got)
	}
	if got := optimizer.rewriteCalls.Load(); got != 0 {
		t.Fatalf("request optimizer rewrite calls = %d, want 0", got)
	}
	reduceRequests := optimizer.snapshotReduceRequests()
	if len(reduceRequests) != 1 {
		t.Fatalf("captured optimizer requests = %d, want 1", len(reduceRequests))
	}
	if got := reduceRequests[0]; got.CallID != "call-release-matrix" || got.Command != "cat release.log" || got.Output != "release-matrix-unoptimized-output" {
		t.Fatalf("optimizer request = %+v, want release-matrix call/command/output", got)
	}

	requests := transport.snapshot()
	if len(requests) != 2 {
		t.Fatalf("upstream send count = %d, want exactly 2", len(requests))
	}
	if got := countRouteIsolationRequests(requests, routeIsolationPrimaryHost); got != 1 {
		t.Fatalf("optimizer primary send count = %d, want 1", got)
	}
	if got := countRouteIsolationRequests(requests, routeIsolationBackupHost); got != 1 {
		t.Fatalf("optimizer backup send count = %d, want 1", got)
	}

	primaryRequest := routeIsolationRequestForHost(t, requests, routeIsolationPrimaryHost)
	backupRequest := routeIsolationRequestForHost(t, requests, routeIsolationBackupHost)
	assertRouteIsolationRequest(t, primaryRequest, routeIsolationExpectation{
		path:  "/optimizer-primary/responses",
		model: "release-matrix-optimizer-primary-model",
		headers: map[string]string{
			"Authorization": "Bearer release-matrix-optimizer-primary-key",
		},
		forbiddenValues: []string{"release-matrix-optimizer-backup-key"},
	}, []string{"release-matrix-optimizer-backup-model"}, false)
	assertRouteIsolationRequest(t, backupRequest, routeIsolationExpectation{
		path:  "/optimizer-backup/responses",
		model: "release-matrix-optimizer-backup-model",
		headers: map[string]string{
			"Authorization": "Bearer release-matrix-optimizer-backup-key",
		},
		forbiddenValues: []string{"release-matrix-optimizer-primary-key"},
	}, []string{"release-matrix-optimizer-primary-model"}, false)

	primaryLogical := decodeRouteIsolationLogicalPayload(t, primaryRequest.body)
	backupLogical := decodeRouteIsolationLogicalPayload(t, backupRequest.body)
	if !reflect.DeepEqual(primaryLogical, backupLogical) {
		t.Fatalf("prepared target logical payloads differ\nprimary: %#v\nbackup:  %#v", primaryLogical, backupLogical)
	}
	assertRouteIsolationOptimizedOutput(t, primaryLogical)
}

func decodeRouteIsolationLogicalPayload(t *testing.T, body []byte) map[string]interface{} {
	t.Helper()
	var payload map[string]interface{}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatalf("decode prepared target body: %v; body=%s", err, body)
	}
	delete(payload, "model")
	return payload
}

func assertRouteIsolationOptimizedOutput(t *testing.T, payload map[string]interface{}) {
	t.Helper()
	input, ok := payload["input"].([]interface{})
	if !ok || len(input) != 2 {
		t.Fatalf("optimized input = %#v, want two items", payload["input"])
	}
	output, ok := input[1].(map[string]interface{})
	if !ok {
		t.Fatalf("optimized output item = %#v, want object", input[1])
	}
	if got := output["output"]; got != "release-matrix-optimized-output" {
		t.Fatalf("optimized output = %#v, want release-matrix-optimized-output", got)
	}
}
