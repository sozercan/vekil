package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func azureRetryTestHandler(t *testing.T, primaryURL, secondaryURL string, sends int) *ProxyHandler {
	t.Helper()
	config := ProvidersConfig{
		SchemaVersion: 2,
		StateBindings: &StateBindingsConfig{Mode: "memory"},
		Providers: []ProviderConfig{
			{ID: "east", Type: string(providerTypeAzureOpenAI), Default: true, BaseURL: primaryURL + "/openai/v1", APIKey: "east-key"},
			{ID: "west", Type: string(providerTypeAzureOpenAI), BaseURL: secondaryURL + "/openai/v1", APIKey: "west-key"},
		},
	}
	for _, publicID := range []string{"public-model", "public-model-low"} {
		config.ModelRoutes = append(config.ModelRoutes, ModelRouteConfig{
			ID: publicID, PublicID: publicID, Endpoints: []string{providerEndpointResponses},
			Targets: []ModelRouteTargetConfig{
				{ID: publicID + "-east", Provider: "east", UpstreamModel: "physical-model"},
				{ID: publicID + "-west", Provider: "west", UpstreamModel: "physical-model"},
			},
			Routing: ModelRouteRoutingConfig{Mode: "priority_failover", MaxTargetAttempts: 2, MaxUpstreamSends: sends},
		})
	}
	h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard), WithProvidersConfig(config))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.BeginShutdown)
	return h
}

const azureRetryCompletedSSE = "data: " + `{"type":"response.completed","response":{"id":"resp_done","model":"physical-model","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11}}}` + "\n\n"

func TestExplicitRouteAzureNegativeBalanceResetRecovers(t *testing.T) {
	for _, tc := range [][2]string{{"HTTP", "-1"}, {"HTTP", "0"}, {"JSON", "-1"}, {"JSON", "0"}, {"SSE", "-1"}, {"SSE", "0"}} {
		protocol := tc[0]
		t.Run(protocol+"/requests="+tc[1], func(t *testing.T) {
			var calls atomic.Int32
			h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if calls.Add(1) == 1 {
					headers := http.Header{"X-Ratelimit-Remaining-Tokens": {"-1"}, "X-Ratelimit-Reset-Tokens": {"2s"}, "X-Ratelimit-Remaining-Requests": {tc[1]}, "X-Ratelimit-Reset-Requests": {"1s"}}
					if protocol == "JSON" {
						return routeExecutorTestResponse(req, 200, headers, `{"status":"failed","output":[],"error":{"code":"rate_limit_exceeded"}}`), nil
					}
					if protocol == "SSE" {
						headers.Set("Content-Type", "text/event-stream")
						return routeExecutorTestResponse(req, 200, headers, "data: "+`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}`+"\n\n"), nil
					}
					return routeExecutorTestResponse(req, 429, headers, `{"error":{"code":"rate_limit_exceeded"}}`), nil
				}
				if protocol == "SSE" {
					return routeExecutorTestResponse(req, 200, http.Header{"Content-Type": {"text/event-stream"}}, azureRetryCompletedSSE), nil
				}
				return routeExecutorTestResponse(req, 200, nil, `{"status":"completed","output":[]}`), nil
			})}, routeModePrimaryOnly, 1, 2, explicitRouteTestProvider("primary", "http://primary.example", "key"))
			t.Cleanup(h.BeginShutdown)
			advance := azureTrafficTestClock(h)
			op := newRouteOperation(route, t.Context())
			done := make(chan azureTrafficTestResult, 1)
			go func() {
				resp, err := h.executeExplicitRouteRequest(withRouteOperation(t.Context(), op), route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", protocol == "SSE")
				done <- azureTrafficTestResult{blocked: resp, err: err}
			}()
			waitForAzureTrafficWaiters(t, h, 1)
			h.azureTraffic.mu.Lock()
			if len(h.azureTraffic.cooldowns) != 1 {
				t.Error("recovery lost its deployment cooldown")
			}
			for _, cooldown := range h.azureTraffic.cooldowns {
				if cooldown.until.Sub(h.azureTraffic.timeNow()) != 2*time.Second {
					t.Error("negative token balance lost its longer reset")
				}
			}
			h.azureTraffic.mu.Unlock()
			advance(2 * time.Second)
			result := receiveAzureTrafficResult(t, done)
			if result.err != nil || result.blocked == nil || result.blocked.StatusCode != 200 || calls.Load() != 2 {
				t.Fatalf("negative-balance recovery failed: calls=%d result=%+v", calls.Load(), result)
			}
			_, _ = io.Copy(io.Discard, result.blocked.Body)
			_ = result.blocked.Body.Close()
		})
	}
}

func TestExplicitRouteAzureStreamRateLimitAliases(t *testing.T) {
	for _, eventType := range []string{"response.failed", "error"} {
		for _, code := range []string{"quota_exceeded", "429", "rate_limit_error"} {
			t.Run(eventType+"/"+code, func(t *testing.T) {
				var calls atomic.Int32
				h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if calls.Add(1) == 1 {
						payload := `{"type":"error","error":{"code":"` + code + `"}}`
						if eventType == "response.failed" {
							payload = `{"type":"response.failed","response":{"error":{"code":"` + code + `"}}}`
						}
						return routeExecutorTestResponse(req, 200, http.Header{"Content-Type": {"text/event-stream"}, "Retry-After": {"1"}}, "data: "+payload+"\n\n"), nil
					}
					return routeExecutorTestResponse(req, 200, http.Header{"Content-Type": {"text/event-stream"}}, azureRetryCompletedSSE), nil
				})}, routeModePriorityFailover, 2, 2,
					explicitRouteTestProvider("primary", "http://primary.example", "key"),
					explicitRouteTestProvider("secondary", "http://secondary.example", "key"))
				t.Cleanup(h.BeginShutdown)
				op := newRouteOperation(route, t.Context())
				resp, err := h.executeExplicitRouteRequest(withRouteOperation(t.Context(), op), route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", true)
				if err != nil || resp == nil {
					t.Fatalf("stream alias did not recover: %v", err)
				}
				body, err := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				sends, switches, _ := op.snapshot()
				if err != nil || !strings.Contains(string(body), "resp_done") || sends != 2 || switches != 1 {
					t.Fatalf("stream alias recovery: sends=%d switches=%d body=%s err=%v", sends, switches, body, err)
				}
			})
		}
	}
}

func TestExplicitRouteAzureRefreshesIdentityAfterQueue(t *testing.T) {
	credential := &fakeAzureCredential{tokens: []azcore.AccessToken{{Token: "fresh-token", ExpiresOn: time.Now().Add(time.Hour)}}}
	source := newAzureSDKTokenSource(credential, "scope")
	source.cached = azcore.AccessToken{Token: "queued-token", ExpiresOn: time.Now().Add(time.Hour)}
	provider := explicitRouteTestProvider("primary", "http://primary.example", "")
	provider.authMode, provider.azureToken = providerAuthModeAzureIdentity, source
	var calls atomic.Int32
	h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.Header.Get("Authorization") != "Bearer fresh-token" {
			t.Error("queued request dispatched with its stale identity token")
		}
		return routeExecutorTestResponse(req, 200, nil, `{"status":"completed","output":[]}`), nil
	})}, routeModePrimaryOnly, 1, 1, provider)
	t.Cleanup(h.BeginShutdown)
	advance := azureTrafficTestClock(h)
	seed := azureTrafficTestRequest(t, h, t.Context(), "primary", "http://primary.example", "deployment-a")
	azureRouteTrafficFromRequest(seed).observe(429, http.Header{"Retry-After": {"2"}})
	done := make(chan azureTrafficTestResult, 1)
	go func() {
		resp, err := h.executeExplicitRouteRequest(t.Context(), route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
		done <- azureTrafficTestResult{blocked: resp, err: err}
	}()
	waitForAzureTrafficWaiters(t, h, 1)
	// Simulate token expiry while the request is held without sleeping through
	// a production-length cooldown or changing the credential cache policy.
	source.mu.Lock()
	source.cached.ExpiresOn = time.Now().Add(-time.Second)
	source.mu.Unlock()
	advance(2 * time.Second)
	result := receiveAzureTrafficResult(t, done)
	if result.err != nil || result.blocked == nil || calls.Load() != 1 {
		t.Fatalf("identity refresh recovery: calls=%d result=%+v", calls.Load(), result)
	}
	_, _ = io.Copy(io.Discard, result.blocked.Body)
	_ = result.blocked.Body.Close()
}

type azureRetryTokenSourceFunc func(context.Context) (string, error)

func (f azureRetryTokenSourceFunc) AccessToken(ctx context.Context) (string, error) { return f(ctx) }

func TestExplicitRouteAzureRetryAuthFailurePreservesRejection(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, afterQueue := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%t/after-queue=%t", stream, afterQueue), func(t *testing.T) {
				var unavailable atomic.Bool
				provider := explicitRouteTestProvider("primary", "http://primary.example", "")
				provider.authMode = providerAuthModeAzureIdentity
				provider.azureToken = azureRetryTokenSourceFunc(func(context.Context) (string, error) {
					if unavailable.Load() {
						return "", errors.New("identity refresh unavailable")
					}
					return "token", nil
				})
				var calls atomic.Int32
				h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					calls.Add(1)
					if !afterQueue {
						unavailable.Store(true)
					}
					headers := http.Header{"Retry-After": {"1"}, "X-Request-Id": {"original-rejection"}}
					if stream {
						headers.Set("Content-Type", "text/event-stream")
						return routeExecutorTestResponse(req, 200, headers, "data: "+`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}`+"\n\n"), nil
					}
					return routeExecutorTestResponse(req, 429, headers, `{"error":{"code":"rate_limit_exceeded"}}`), nil
				})}, routeModePrimaryOnly, 1, 2, provider)
				t.Cleanup(h.BeginShutdown)
				advance := azureTrafficTestClock(h)
				op := newRouteOperation(route, t.Context())
				done := make(chan azureTrafficTestResult, 1)
				go func() {
					resp, err := h.executeExplicitRouteRequest(withRouteOperation(t.Context(), op), route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", stream)
					done <- azureTrafficTestResult{blocked: resp, err: err}
				}()
				if afterQueue {
					waitForAzureTrafficWaiters(t, h, 1)
					unavailable.Store(true)
					advance(time.Second)
				}
				result := receiveAzureTrafficResult(t, done)
				if stream {
					var upstreamErr *upstreamError
					if !errors.As(result.err, &upstreamErr) || upstreamErr.statusCode != 429 {
						t.Fatalf("lost original stream rejection: %v", result.err)
					}
				} else {
					if result.err != nil || result.blocked == nil || result.blocked.StatusCode != 429 {
						t.Fatalf("lost original HTTP rejection: %+v", result)
					}
					_ = result.blocked.Body.Close()
				}
				sends, switches, trace := op.snapshot()
				if calls.Load() != 1 || sends != 1 || switches != 0 || len(trace) != 1 || trace[0].UpstreamID != "original-rejection" {
					t.Fatalf("auth failure changed dispatch or correlation: calls=%d sends=%d switches=%d trace=%+v", calls.Load(), sends, switches, trace)
				}
			})
		}
	}
}

func TestExplicitRouteAzurePinnedResponsesRetryPreservesState(t *testing.T) {
	for _, failure := range []string{"HTTP", "preamble failure", "embedded reset", "JSON failure", "JSON embedded reset"} {
		t.Run(failure, func(t *testing.T) {
			var primaryCalls, secondaryCalls atomic.Int32
			var mu sync.Mutex
			var retryBodies []string
			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					_, _ = io.WriteString(w, `{"data":[]}`)
					return
				}
				call := primaryCalls.Add(1)
				body, _ := io.ReadAll(r.Body)
				w.Header().Set("X-Request-ID", fmt.Sprintf("east-attempt-%d", call))
				if call == 1 {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("X-Codex-Turn-State", "turn_seed")
					_, _ = io.WriteString(w, `{"id":"resp_seed","model":"physical-model","status":"completed","output":[{"type":"reasoning","id":"rs_seed","encrypted_content":"opaque_seed"}]}`)
					return
				}
				mu.Lock()
				retryBodies = append(retryBodies, string(body))
				mu.Unlock()
				if r.Header.Get("X-Codex-Turn-State") != "turn_seed" || r.Header.Get("api-key") != "east-key" {
					t.Error("retry changed provider credentials or turn state")
				}
				if call == 2 {
					if failure != "embedded reset" && failure != "JSON embedded reset" {
						w.Header().Set("Retry-After", "1")
					}
					if strings.HasPrefix(failure, "JSON") {
						w.Header().Set("Content-Type", "application/json")
						if failure == "JSON embedded reset" {
							_, _ = io.WriteString(w, `{"id":"resp_rejected","status":"failed","output":[],"error":{"code":"rate_limit_exceeded","headers":{"retry-after-ms":1}}}`)
						} else {
							_, _ = io.WriteString(w, `{"id":"resp_rejected","status":"failed","output":[],"error":{"code":"rate_limit_exceeded"}}`)
						}
					} else if failure == "HTTP" {
						w.WriteHeader(429)
						_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded","message":"wait"}}`)
					} else {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = io.WriteString(w, "data: "+`{"type":"response.created","response":{"id":"resp_rejected","output":[]}}`+"\n\n")
						if failure == "embedded reset" {
							_, _ = io.WriteString(w, "data: "+`{"type":"error","code":"rate_limit_exceeded","message":"wait","headers":{"retry-after-ms":1}}`+"\n\n")
						} else {
							_, _ = io.WriteString(w, "data: "+`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"wait"}}}`+"\n\n")
						}
					}
					return
				}
				if strings.HasPrefix(failure, "JSON") {
					w.Header().Set("Content-Type", "application/json")
					_, _ = io.WriteString(w, `{"id":"resp_done","model":"physical-model","status":"completed","output":[]}`)
				} else {
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, azureRetryCompletedSSE)
				}
			}))
			defer primary.Close()
			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				secondaryCalls.Add(1)
				t.Error("provider-bound state migrated to the secondary")
				w.WriteHeader(500)
			}))
			defer secondary.Close()
			h := azureRetryTestHandler(t, primary.URL, secondary.URL, 3)
			advance := azureTrafficTestClock(h)
			first := httptest.NewRecorder()
			h.HandleResponses(first, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-model","input":"first"}`)))
			if first.Code != 200 {
				t.Fatalf("bootstrap: %d %s", first.Code, first.Body.String())
			}
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":"public-model","stream":%t,"previous_response_id":"resp_seed","input":[{"type":"reasoning","encrypted_content":"opaque_seed"},{"role":"user","content":"next"}]}`, !strings.HasPrefix(failure, "JSON"))))
			request.Header.Set("X-Codex-Turn-State", "turn_seed")
			response := httptest.NewRecorder()
			done := make(chan struct{})
			go func() { defer close(done); h.HandleResponses(response, request) }()
			waitForAzureTrafficWaiters(t, h, 1)
			if primaryCalls.Load() != 2 || secondaryCalls.Load() != 0 {
				t.Fatal("request retried before the reset")
			}
			advance(time.Second)
			select {
			case <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("retry did not complete")
			}
			if response.Code != 200 || !strings.Contains(response.Body.String(), "resp_done") || strings.Contains(response.Body.String(), "resp_rejected") {
				t.Fatalf("recovered response: %d %s", response.Code, response.Body.String())
			}
			mu.Lock()
			if len(retryBodies) != 2 || retryBodies[0] != retryBodies[1] {
				t.Errorf("retry changed the request: %v", retryBodies)
			}
			mu.Unlock()
			h.stats.mu.Lock()
			attempts := h.stats.recentAttemptsSnapshot()
			h.stats.mu.Unlock()
			operationID := response.Header().Get("X-Vekil-Request-ID")
			var matching []recentRouteAttempt
			for _, attempt := range attempts {
				if attempt.OperationID == operationID {
					matching = append(matching, attempt)
				}
			}
			if len(matching) != 2 || matching[0].AttemptKind != routeAttemptRateLimitRetry || matching[1].RetryDecision != routeRetrySameTarget || matching[0].TargetID != matching[1].TargetID {
				t.Fatalf("retry accounting = %+v", matching)
			}
		})
	}
}

func TestExplicitRouteAzureCooldownSkipsSharedDeploymentForFreshAlias(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Retry-After", "60")
		w.WriteHeader(429)
		_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryCalls.Add(1)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body["model"] != "physical-model" {
			t.Errorf("physical model = %v", body["model"])
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, azureRetryCompletedSSE)
	}))
	defer secondary.Close()
	h := azureRetryTestHandler(t, primary.URL, secondary.URL, 2)
	for _, model := range []string{"public-model", "public-model-low"} {
		response := httptest.NewRecorder()
		h.HandleResponses(response, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"stream":true,"input":"fresh"}`, model))))
		if response.Code != 200 {
			t.Fatalf("fresh alias %s: %d %s", model, response.Code, response.Body.String())
		}
	}
	if primaryCalls.Load() != 1 || secondaryCalls.Load() != 2 {
		t.Fatalf("cooldown did not share the physical deployment: east=%d west=%d", primaryCalls.Load(), secondaryCalls.Load())
	}
}

func TestExplicitRouteAzureRetryUsesSendBudgetAndLatestError(t *testing.T) {
	var calls atomic.Int32
	h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		call := calls.Add(1)
		headers := http.Header{"Retry-After": {"1"}, "X-Request-Id": {fmt.Sprintf("attempt-%d", call)}}
		if call == 3 {
			headers.Set("Retry-After", "60")
		}
		return routeExecutorTestResponse(req, 429, headers, fmt.Sprintf(`{"error":{"code":"rate_limit_exceeded","message":"attempt %d"}}`, call)), nil
	})}, routeModePrimaryOnly, 1, 3, explicitRouteTestProvider("primary", "http://primary.example", "key"))
	t.Cleanup(h.BeginShutdown)
	advance := azureTrafficTestClock(h)
	op := newRouteOperation(route, t.Context())
	ctx := withRouteOperation(t.Context(), op)
	done := make(chan azureTrafficTestResult, 1)
	go func() {
		resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
		done <- azureTrafficTestResult{blocked: resp, err: err}
	}()
	for attempt := int32(1); attempt < 3; attempt++ {
		waitForAzureTrafficWaiters(t, h, 1)
		advance(time.Second)
		deadline := time.Now().Add(3 * time.Second)
		for calls.Load() <= attempt && time.Now().Before(deadline) {
			time.Sleep(time.Millisecond)
		}
		if calls.Load() <= attempt {
			t.Fatal("retry was not dispatched after reset")
		}
	}
	result := receiveAzureTrafficResult(t, done)
	if result.err != nil || result.blocked == nil {
		t.Fatalf("exhausted request = %+v", result)
	}
	defer func() { _ = result.blocked.Body.Close() }()
	body, err := io.ReadAll(result.blocked.Body)
	if err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 3 || result.blocked.StatusCode != 429 || result.blocked.Header.Get("Retry-After") != "60" || !strings.Contains(string(body), "attempt 3") {
		t.Fatalf("budget or final error: sends=%d status=%d headers=%v", calls.Load(), result.blocked.StatusCode, result.blocked.Header)
	}
	sends, switches, trace := op.snapshot()
	if sends != 3 || switches != 0 || len(trace) != 3 || trace[2].Decision != routeRetrySuppressedBudget || trace[2].UpstreamID != "attempt-3" {
		t.Fatalf("operation: sends=%d switches=%d trace=%+v", sends, switches, trace)
	}
}

func TestExplicitRouteAzureRetryCancellationPreservesRejection(t *testing.T) {
	for _, shutdown := range []bool{false, true} {
		t.Run(map[bool]string{false: "disconnect", true: "shutdown"}[shutdown], func(t *testing.T) {
			var calls atomic.Int32
			h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return routeExecutorTestResponse(req, 429, http.Header{"Retry-After": {"30"}}, `{"error":{"code":"rate_limit_exceeded"}}`), nil
			})}, routeModePrimaryOnly, 1, 3, explicitRouteTestProvider("primary", "http://primary.example", "key"))
			t.Cleanup(h.BeginShutdown)
			inbound, cancel := context.WithCancel(t.Context())
			defer cancel()
			op := newRouteOperation(route, inbound)
			ctx := withRouteOperation(context.Background(), op)
			done := make(chan azureTrafficTestResult, 1)
			go func() {
				resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
				done <- azureTrafficTestResult{blocked: resp, err: err}
			}()
			waitForAzureTrafficWaiters(t, h, 1)
			if shutdown {
				h.BeginShutdown()
			} else {
				cancel()
			}
			result := receiveAzureTrafficResult(t, done)
			if result.err != nil || result.blocked == nil || result.blocked.StatusCode != 429 || calls.Load() != 1 {
				t.Fatalf("canceled recovery: sends=%d result=%+v", calls.Load(), result)
			}
			_ = result.blocked.Body.Close()
			waitForAzureTrafficWaiters(t, h, 0)
			want := routeRetrySuppressedAdmission
			if shutdown {
				want = routeRetrySuppressedLifecycle
			}
			_, _, trace := op.snapshot()
			if len(trace) != 1 || trace[0].Decision != want {
				t.Fatalf("canceled retry trace = %+v, want %s", trace, want)
			}
		})
	}
}

func TestExplicitRouteAzureRetrySafetyBoundaries(t *testing.T) {
	failed := "data: " + `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"wait"}}}` + "\n\n"
	for _, tc := range []struct {
		name       string
		reset      string
		stream     string
		kind       routeAttemptKind
		commitment downstreamCommitment
		transport  bool
	}{
		{name: "missing reset"},
		{name: "invalid reset", reset: "later"},
		{name: "long reset", reset: "86400"},
		{name: "deadline", reset: "5"},
		{name: "protocol recovery", reset: "1", kind: routeAttemptProtocolRecovery},
		{name: "compaction", reset: "1", kind: routeAttemptCompaction},
		{name: "compatibility fallback", reset: "1", kind: routeAttemptCompatibilityFallback},
		{name: "semantic commitment", reset: "1", commitment: downstreamCommitmentSemantic},
		{name: "ambiguous delivery", reset: "1", transport: true},
		{name: "partial text", reset: "1", stream: "data: " + `{"type":"response.output_text.delta","delta":"partial"}` + "\n\n" + failed},
		{name: "tool activity", reset: "1", stream: "data: " + `{"type":"response.output_item.added","item":{"type":"function_call","name":"run","call_id":"call_1"}}` + "\n\n" + failed},
		{name: "reported usage", reset: "1", stream: "data: " + `{"type":"response.failed","response":{"usage":{"input_tokens":10,"output_tokens":1,"total_tokens":11},"error":{"code":"rate_limit_exceeded"}}}` + "\n\n"},
		{name: "malformed stream", reset: "1", stream: "data: {invalid}\n\n" + failed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var calls atomic.Int32
			h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if tc.transport {
					if trace := httptrace.ContextClientTrace(req.Context()); trace != nil && trace.WroteRequest != nil {
						trace.WroteRequest(httptrace.WroteRequestInfo{})
					}
					return nil, io.ErrUnexpectedEOF
				}
				headers := http.Header{"Retry-After": {tc.reset}}
				if tc.stream != "" {
					headers.Set("Content-Type", "text/event-stream")
					return routeExecutorTestResponse(req, 200, headers, tc.stream), nil
				}
				return routeExecutorTestResponse(req, 429, headers, `{"error":{"code":"rate_limit_exceeded"}}`), nil
			})}, routeModePrimaryOnly, 1, 3, explicitRouteTestProvider("primary", "http://primary.example", "key"))
			t.Cleanup(h.BeginShutdown)
			ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
			defer cancel()
			op := newRouteOperation(route, ctx)
			if tc.commitment != "" {
				if err := op.forcePinnedTarget(route.targets[0].id); err != nil {
					t.Fatal(err)
				}
				op.setCommitment(tc.commitment)
			}
			ctx = withRouteAttemptKind(withRouteOperation(ctx, op), tc.kind)
			resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model","stream":true}`), nil, "public-model", tc.stream != "")
			if resp != nil {
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
			} else if err == nil {
				t.Fatal("request lost both response and error")
			}
			sends, switches, trace := op.snapshot()
			if calls.Load() != 1 || sends != 1 || switches != 0 || len(trace) != 1 || trace[0].Decision == routeRetrySameTarget {
				t.Fatalf("unsafe retry: calls=%d sends=%d switches=%d trace=%+v", calls.Load(), sends, switches, trace)
			}
			if ctx.Err() != nil {
				t.Fatal("unsafe attempt waited for recovery")
			}
		})
	}
}

func TestExplicitRouteAzurePinnedWebSocketTurnRetries(t *testing.T) {
	var primaryCalls, secondaryCalls atomic.Int32
	bodies := make(chan string, 2)
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			_, _ = io.WriteString(w, `{"data":[]}`)
			return
		}
		call := primaryCalls.Add(1)
		if call > 1 {
			body, _ := io.ReadAll(r.Body)
			bodies <- string(body)
			if r.Header.Get("api-key") != "east-key" {
				t.Error("websocket retry changed provider credentials")
			}
		}
		if call == 2 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(429)
			_, _ = io.WriteString(w, `{"error":{"code":"rate_limit_exceeded"}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, strings.ReplaceAll(azureRetryCompletedSSE, "resp_done", fmt.Sprintf("resp_%d", call)))
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryCalls.Add(1)
		w.WriteHeader(500)
	}))
	defer secondary.Close()
	h := azureRetryTestHandler(t, primary.URL, secondary.URL, 3)
	advance := azureTrafficTestClock(h)
	server := startResponsesWebSocketProxyServer(t, h)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()
	request := newResponsesWebSocketCreateRequest([]interface{}{})
	request["model"] = "public-model"
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	first := mustReadWebSocketJSONSkipMetadata(t, conn)
	if first["type"] != "response.completed" || websocketResponseID(t, first) != "resp_1" {
		t.Fatalf("first turn = %+v", first)
	}
	request["previous_response_id"] = "resp_1"
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	waitForAzureTrafficWaiters(t, h, 1)
	if primaryCalls.Load() != 2 || secondaryCalls.Load() != 0 {
		t.Fatal("pinned websocket turn retried before reset or migrated")
	}
	advance(time.Second)
	second := mustReadWebSocketJSONSkipMetadata(t, conn)
	if second["type"] != "response.completed" || websocketResponseID(t, second) != "resp_3" {
		t.Fatalf("recovered turn = %+v", second)
	}
	if primaryCalls.Load() != 3 || secondaryCalls.Load() != 0 {
		t.Fatal("websocket retry changed the target or send count")
	}
	firstBody, secondBody := <-bodies, <-bodies
	if firstBody != secondBody {
		t.Fatal("websocket retry changed the request")
	}
}

func TestExplicitRouteAzureProbeLateThrottleDelaysQueuedRequest(t *testing.T) {
	reader, writer := io.Pipe()
	defer func() { _ = reader.Close() }()
	defer func() { _ = writer.Close() }()
	go func() {
		defer func() { _ = writer.Close() }()
		_, _ = io.WriteString(writer, "data: "+`{"type":"response.output_text.delta","delta":"partial"}`+"\n\n")
		_, _ = io.WriteString(writer, "data: "+`{"type":"error","code":"rate_limit_exceeded","headers":{"retry-after-ms":5000}}`+"\n\n")
	}()
	var calls atomic.Int32
	h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		resp := routeExecutorTestResponse(req, 200, http.Header{"Content-Type": {"text/event-stream"}}, azureRetryCompletedSSE)
		if calls.Add(1) == 1 {
			resp.Body = reader
		}
		return resp, nil
	})}, routeModePrimaryOnly, 1, 1, explicitRouteTestProvider("primary", "http://primary.example", "key"))
	t.Cleanup(h.BeginShutdown)
	advance := azureTrafficTestClock(h)
	seed := azureTrafficTestRequest(t, h, t.Context(), "primary", "http://primary.example", "deployment-a")
	azureRouteTrafficFromRequest(seed).observe(429, http.Header{"Retry-After": {"1"}})
	advance(time.Second)
	execute := func() azureTrafficTestResult {
		ctx := withRouteOperation(t.Context(), newRouteOperation(route, t.Context()))
		resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model","stream":true}`), nil, "public-model", true)
		return azureTrafficTestResult{blocked: resp, err: err}
	}
	first := execute()
	if first.err != nil || first.blocked == nil {
		t.Fatalf("probe = %+v", first)
	}
	defer func() { _ = first.blocked.Body.Close() }()
	second := make(chan azureTrafficTestResult, 1)
	go func() { second <- execute() }()
	waitForAzureTrafficWaiters(t, h, 1)
	if _, err := io.Copy(io.Discard, first.blocked.Body); err != nil {
		t.Fatal(err)
	}
	advance(4 * time.Second)
	if calls.Load() != 1 {
		t.Fatal("late throttle released the queued request before reset")
	}
	advance(time.Second)
	result := receiveAzureTrafficResult(t, second)
	if result.err != nil || result.blocked == nil || calls.Load() != 2 {
		t.Fatalf("queued request = %+v, calls=%d", result, calls.Load())
	}
	defer func() { _ = result.blocked.Body.Close() }()
	if _, err := io.Copy(io.Discard, result.blocked.Body); err != nil {
		t.Fatal(err)
	}
	h.azureTraffic.mu.Lock()
	defer h.azureTraffic.mu.Unlock()
	if len(h.azureTraffic.cooldowns) != 0 || h.azureTraffic.waiters != 0 {
		t.Fatal("recovery retained the probe or queue after completion")
	}
}

func TestExplicitRouteAzureProbeWaitsForResponseCleanup(t *testing.T) {
	for _, scenario := range []struct {
		name          string
		stream        bool
		headerFailure bool
	}{
		{name: "HTTP rejection"},
		{name: "stream rejection", stream: true},
		{name: "Chat header rejection", headerFailure: true},
		{name: "stream header rejection", stream: true, headerFailure: true},
	} {
		for _, blockClose := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/block-close=%t", scenario.name, blockClose), func(t *testing.T) {
				readRelease := make(chan struct{})
				closeRelease := make(chan struct{})
				var readOnce, closeOnce sync.Once
				releaseRead := func() { readOnce.Do(func() { close(readRelease) }) }
				releaseClose := func() { closeOnce.Do(func() { close(closeRelease) }) }
				t.Cleanup(func() { releaseRead(); releaseClose() })
				if blockClose {
					releaseRead()
				} else {
					releaseClose()
				}
				payload := `{"error":{"code":"rate_limit_exceeded"}}`
				status := http.StatusTooManyRequests
				endpoint := providerEndpointResponses
				headers := http.Header{"Retry-After": {"1"}, "Content-Type": {"application/json"}}
				if scenario.stream {
					payload = "data: " + `{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded"}}}` + "\n\n"
					status = http.StatusOK
					headers.Set("Content-Type", "text/event-stream")
				}
				if scenario.headerFailure {
					status = http.StatusOK
					headers.Add("X-Codex-Turn-State", "state-one")
					headers.Add("X-Codex-Turn-State", "state-two")
					if scenario.stream {
						payload = azureRetryCompletedSSE
					} else {
						endpoint = providerEndpointChatCompletions
						payload = `{"choices":[],"usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}}`
					}
				}
				body := &routeAttemptUsageThenBlockingCleanupBody{
					data: []byte(payload), readBlocked: make(chan struct{}), readRelease: readRelease,
					closeStarted: make(chan struct{}), closeRelease: closeRelease,
				}
				var calls atomic.Int32
				h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
					if calls.Add(1) == 1 {
						resp := routeExecutorTestResponse(req, status, headers, payload)
						resp.Body = body
						return resp, nil
					}
					if scenario.stream {
						return routeExecutorTestResponse(req, 200, http.Header{"Content-Type": {"text/event-stream"}}, azureRetryCompletedSSE), nil
					}
					return routeExecutorTestResponse(req, 200, nil, `{"status":"completed","output":[]}`), nil
				})}, routeModePrimaryOnly, 1, 1, explicitRouteTestProvider("primary", "http://primary.example", "key"))
				t.Cleanup(h.BeginShutdown)
				advance := azureTrafficTestClock(h)
				seed := azureTrafficTestRequest(t, h, t.Context(), "primary", "http://primary.example", "deployment-a")
				azureRouteTrafficFromRequest(seed).observe(429, http.Header{"Retry-After": {"1"}})
				advance(time.Second)
				execute := func() <-chan azureTrafficTestResult {
					done := make(chan azureTrafficTestResult, 1)
					go func() {
						ctx := withRouteOperation(t.Context(), newRouteOperation(route, t.Context()))
						resp, err := h.executeExplicitRouteRequest(ctx, route, endpoint, []byte(`{"model":"public-model"}`), nil, "public-model", scenario.stream)
						done <- azureTrafficTestResult{blocked: resp, err: err}
					}()
					return done
				}
				first := execute()
				waiting := body.readBlocked
				if blockClose {
					waiting = body.closeStarted
				}
				select {
				case <-waiting:
				case <-time.After(3 * time.Second):
					t.Fatal("probe never entered blocked cleanup")
				}
				result := receiveAzureTrafficResult(t, first)
				if scenario.headerFailure && (result.err == nil || !strings.Contains(result.err.Error(), "conflicting X-Codex-Turn-State")) {
					t.Fatalf("expected header binding failure: %+v", result)
				}
				if result.blocked != nil {
					_ = result.blocked.Body.Close()
				}
				advance(2 * time.Second)
				second := execute()
				waitForAzureTrafficWaiters(t, h, 1)
				if calls.Load() != 1 {
					t.Fatalf("queued request overlapped cleanup: calls=%d", calls.Load())
				}
				releaseRead()
				releaseClose()
				result = receiveAzureTrafficResult(t, second)
				if result.err != nil || result.blocked == nil || result.blocked.StatusCode != 200 || calls.Load() != 2 {
					t.Fatalf("queue did not recover after cleanup: calls=%d result=%+v", calls.Load(), result)
				}
				_, _ = io.Copy(io.Discard, result.blocked.Body)
				_ = result.blocked.Body.Close()
				waitForAzureTrafficWaiters(t, h, 0)
			})
		}
	}
}

func TestExplicitRouteAzureRenewedResetPreservesUpstreamRejection(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			var calls atomic.Int32
			h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				headers := http.Header{"Retry-After": {"1"}, "X-Request-Id": {"original-rejection"}}
				if stream {
					headers.Set("Content-Type", "text/event-stream")
					return routeExecutorTestResponse(req, 200, headers, "data: "+`{"type":"response.failed","response":{"error":{"code":"rate_limit_exceeded","message":"original error"}}}`+"\n\n"), nil
				}
				return routeExecutorTestResponse(req, 429, headers, `{"error":{"code":"rate_limit_exceeded","message":"original error"}}`), nil
			})}, routeModePrimaryOnly, 1, 3, explicitRouteTestProvider("primary", "http://primary.example", "key"))
			t.Cleanup(h.BeginShutdown)
			azureTrafficTestClock(h)
			ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
			defer cancel()
			op := newRouteOperation(route, ctx)
			ctx = withRouteOperation(ctx, op)
			done := make(chan azureTrafficTestResult, 1)
			go func() {
				resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model","stream":true}`), nil, "public-model", stream)
				done <- azureTrafficTestResult{blocked: resp, err: err}
			}()
			waitForAzureTrafficWaiters(t, h, 1)
			req := azureTrafficTestRequest(t, h, t.Context(), "primary", "http://primary.example", "deployment-a")
			azureRouteTrafficFromRequest(req).observe(429, http.Header{"Retry-After": {"60"}})
			result := receiveAzureTrafficResult(t, done)
			if stream {
				var upstreamErr *upstreamError
				if !errors.As(result.err, &upstreamErr) || upstreamErr.statusCode != 429 || upstreamErr.retryAfter != "60" || !strings.Contains(string(upstreamErr.body), "original error") {
					t.Fatalf("original streamed rejection lost: %v", result.err)
				}
			} else {
				if result.err != nil || result.blocked == nil {
					t.Fatalf("original HTTP rejection lost: %+v", result)
				}
				defer func() { _ = result.blocked.Body.Close() }()
				body, _ := io.ReadAll(result.blocked.Body)
				if result.blocked.StatusCode != 429 || result.blocked.Header.Get("Retry-After") != "60" || !strings.Contains(string(body), "original error") {
					t.Fatalf("original HTTP rejection changed: %d %v %s", result.blocked.StatusCode, result.blocked.Header, body)
				}
			}
			sends, _, trace := op.snapshot()
			if calls.Load() != 1 || sends != 1 || len(trace) != 1 || trace[0].UpstreamID != "original-rejection" || trace[0].Decision != routeRetrySuppressedAdmission {
				t.Fatalf("renewed cooldown retry: calls=%d sends=%d trace=%+v", calls.Load(), sends, trace)
			}
		})
	}
}

func TestExplicitRouteAzureConcurrentFailoverExhaustionAndClientRetries(t *testing.T) {
	// A has already reached West when B consumes its remaining capacity.
	// East and the final fallback are unavailable throughout the scenario.
	var eastCalls, westCalls, fallbackCalls atomic.Int32
	westSelected := make(chan struct{})
	westConsumed := make(chan struct{})
	var releaseOnce sync.Once
	releaseWest := func() { releaseOnce.Do(func() { close(westConsumed) }) }
	t.Cleanup(releaseWest)
	transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		switch req.URL.Hostname() {
		case "east.example":
			eastCalls.Add(1)
			return routeExecutorTestResponse(req, 429, http.Header{"Retry-After": {"60"}, "X-Request-Id": {"east-rejection"}}, `{"error":{"code":"rate_limit_exceeded","message":"east quota"}}`), nil
		case "west.example":
			westCalls.Add(1)
			body, _ := io.ReadAll(req.Body)
			if strings.Contains(string(body), `"input":"A"`) {
				close(westSelected)
				select {
				case <-westConsumed:
				case <-req.Context().Done():
					return nil, req.Context().Err()
				}
				return routeExecutorTestResponse(req, 429, http.Header{"Retry-After": {"30"}}, `{"error":{"code":"rate_limit_exceeded","message":"west quota"}}`), nil
			}
			return routeExecutorTestResponse(req, 200, http.Header{
				"X-Ratelimit-Remaining-Tokens": {"-100"}, "X-Ratelimit-Reset-Tokens": {"30"},
			}, `{"id":"resp_b","status":"completed","output":[],"usage":{"input_tokens":10,"output_tokens":1}}`), nil
		case "fallback.example":
			fallbackCalls.Add(1)
			return routeExecutorTestResponse(req, 503, nil, `{"error":{"code":"model_overloaded","message":"fallback unavailable"}}`), nil
		default:
			return nil, fmt.Errorf("unexpected upstream %s", req.URL.Hostname())
		}
	})
	fallback := explicitRouteTestProvider("fallback", "http://fallback.example", "key")
	fallback.kind = providerTypeOpenAICompatible
	h, route := explicitRouteTestHandler(t, &http.Client{Transport: transport}, routeModePriorityFailover, 3, 3,
		explicitRouteTestProvider("east", "http://east.example", "key"),
		explicitRouteTestProvider("west", "http://west.example", "key"),
		fallback)
	t.Cleanup(h.BeginShutdown)
	azureTrafficTestClock(h)
	type result struct {
		response  *http.Response
		operation *routeOperation
		err       error
	}
	execute := func(ctx context.Context, input string, pinned bool) result {
		op := newRouteOperation(route, ctx)
		if pinned {
			if err := op.forcePinnedTarget("target-west"); err != nil {
				return result{operation: op, err: err}
			}
		}
		response, err := h.executeExplicitRouteRequest(withRouteOperation(ctx, op), route, providerEndpointResponses,
			[]byte(fmt.Sprintf(`{"model":"public-model","input":%q}`, input)), nil, "public-model", false)
		return result{response, op, err}
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	pending := make(chan result, 1)
	go func() { pending <- execute(ctx, "A", false) }()
	select {
	case <-westSelected:
	case <-ctx.Done():
		t.Fatal("A did not reach West")
	}
	b := execute(ctx, "B", true)
	if b.err != nil || b.response == nil || b.response.StatusCode != 200 {
		t.Fatalf("B failed: %+v", b)
	}
	_, _ = io.Copy(io.Discard, b.response.Body)
	_ = b.response.Body.Close()
	if sends, switches, _ := b.operation.snapshot(); sends != 1 || switches != 0 {
		t.Fatalf("B was replayed or migrated: sends=%d switches=%d", sends, switches)
	}
	releaseWest()
	var a result
	select {
	case a = <-pending:
	case <-ctx.Done():
		t.Fatal("A did not finish after West rejected it")
	}
	if a.err != nil || a.response == nil || a.response.StatusCode != 429 {
		t.Fatalf("A lost its upstream rejection: %+v", a)
	}
	_, _ = io.Copy(io.Discard, a.response.Body)
	_ = a.response.Body.Close()
	if sends, switches, trace := a.operation.snapshot(); sends != 3 || switches != 2 || len(trace) != 3 ||
		trace[0].StatusCode != 429 || trace[1].StatusCode != 429 || trace[2].StatusCode != 503 {
		t.Fatalf("unbounded or incorrect failover: sends=%d switches=%d trace=%+v", sends, switches, trace)
	}
	// Client retries are distinct operations. They still share deployment
	// cooldowns, so fresh budgets cannot produce more Azure sends.
	for range 8 {
		retried := execute(ctx, "client retry", false)
		if retried.err != nil || retried.response == nil || retried.response.StatusCode != 503 {
			t.Fatalf("client retry lost the actual fallback failure: %+v", retried)
		}
		_ = retried.response.Body.Close()
		if sends, _, _ := retried.operation.snapshot(); sends != 1 {
			t.Fatalf("client retry sent to a cooled-down deployment: sends=%d", sends)
		}
	}
	if eastCalls.Load() != 1 || westCalls.Load() != 2 || fallbackCalls.Load() != 9 {
		t.Fatalf("unexpected sends: east=%d west=%d fallback=%d", eastCalls.Load(), westCalls.Load(), fallbackCalls.Load())
	}
}
