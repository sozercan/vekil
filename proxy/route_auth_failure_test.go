package proxy

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
)

func TestExplicitRouteCredentialFailureGuards(t *testing.T) {
	for _, tc := range []struct {
		name      string
		mode      routeMode
		kind      routeAttemptKind
		configure func(*routeOperation)
		lifecycle string
		noBackup  bool
		badConfig bool
		want      routeRetryDecision
	}{
		{name: "primary only", mode: routeModePrimaryOnly, want: routeRetrySuppressedMode},
		{name: "hard pin", configure: func(op *routeOperation) { _ = op.forcePinnedTarget("target-primary") }, want: routeRetrySuppressedState},
		{name: "soft pin", configure: func(op *routeOperation) { op.pinTarget("target-primary") }, want: routeRetrySuppressedState},
		{name: "target budget", configure: func(op *routeOperation) { op.remainingTargetAttempts = 1 }, want: routeRetrySuppressedBudget},
		{name: "send budget", configure: func(op *routeOperation) { op.remainingUpstreamSends = 0 }, want: routeRetrySuppressedBudget},
		{name: "protocol commitment", configure: func(op *routeOperation) { op.setCommitment(downstreamCommitmentProtocolFrame) }, want: routeRetrySuppressedCommitment},
		{name: "semantic commitment", configure: func(op *routeOperation) { op.setCommitment(downstreamCommitmentSemantic) }, want: routeRetrySuppressedCommitment},
		{name: "canceled request", lifecycle: "request", want: routeRetrySuppressedLifecycle},
		{name: "canceled client", lifecycle: "client", want: routeRetrySuppressedAdmission},
		{name: "shutdown", lifecycle: "shutdown", want: routeRetrySuppressedLifecycle},
		{name: "protocol recovery", kind: routeAttemptProtocolRecovery, want: routeRetrySuppressedState},
		{name: "compaction", kind: routeAttemptCompaction, want: routeRetrySuppressedState},
		{name: "compatibility recovery", kind: routeAttemptCompatibilityFallback, want: routeRetrySuppressedState},
		{name: "no eligible target", noBackup: true, want: routeRetrySuppressedNoTarget},
		{name: "missing credential configuration", badConfig: true, want: routeRetrySuppressedNonretryable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			inbound, disconnect := context.WithCancel(t.Context())
			defer disconnect()
			primary := explicitRouteTestProvider("primary", "http://primary.example", "")
			primary.authMode = providerAuthModeAzureIdentity
			secondary := explicitRouteTestProvider("secondary", "http://secondary.example", "key")
			mode := tc.mode
			if mode == "" {
				mode = routeModePriorityFailover
			}
			var calls atomic.Int32
			h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return routeExecutorTestResponse(req, 200, nil, `{"status":"completed","output":[]}`), nil
			})}, mode, 2, 2, primary, secondary)
			t.Cleanup(h.BeginShutdown)
			cause := errors.New("Azure CLI executable not found on path")
			if !tc.badConfig {
				primary.azureToken = azureRetryTokenSourceFunc(func(context.Context) (string, error) {
					switch tc.lifecycle {
					case "request":
						cancel()
					case "client":
						disconnect()
					case "shutdown":
						h.BeginShutdown()
					}
					return "", cause
				})
			}
			op := newRouteOperation(route, inbound)
			if tc.configure != nil {
				tc.configure(op)
			}
			if tc.noBackup {
				// This provider has no native Responses endpoint.
				secondary.kind = providerTypeAnthropicCompatible
			}
			ctx = withRouteOperation(ctx, op)
			if tc.kind != "" {
				ctx = withRouteAttemptKind(ctx, tc.kind)
			}
			resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
			if resp != nil {
				_ = resp.Body.Close()
				t.Fatal("unexpected upstream response")
			}
			if err == nil || !tc.badConfig && !errors.Is(err, cause) {
				t.Fatalf("credential failure lost: %v", err)
			}
			if tc.badConfig && providerRequestErrorCode(err) != "" {
				t.Fatalf("configuration error classified as unavailable credentials: %v", err)
			}
			sends, switches, trace := op.snapshot()
			if calls.Load() != 0 || sends != 0 || switches != 0 || len(trace) != 1 {
				t.Fatalf("unsafe dispatch: calls=%d sends=%d switches=%d trace=%+v", calls.Load(), sends, switches, trace)
			}
			if trace[0].Decision != tc.want || trace[0].Delivery != requestDefinitelyNotDelivered || !trace[0].CleanupDone {
				t.Fatalf("trace=%+v, want %s before delivery", trace[0], tc.want)
			}
		})
	}
}

func TestExplicitRouteCredentialFailurePreservesErrorPrecedence(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			primary := explicitRouteTestProvider("primary", "http://primary.example", "")
			primary.authMode = providerAuthModeAzureIdentity
			primary.azureToken = azureRetryTokenSourceFunc(func(context.Context) (string, error) {
				return "", errors.New("run az login")
			})
			var calls atomic.Int32
			h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				if req.URL.Host != "secondary.example" {
					t.Errorf("unexpected inference target: %s", req.URL.Host)
				}
				return routeExecutorTestResponse(req, status, nil, `{"error":{"message":"upstream rejection"}}`), nil
			})}, routeModePriorityFailover, 3, 3, primary,
				explicitRouteTestProvider("secondary", "http://secondary.example", "key"))
			t.Cleanup(h.BeginShutdown)
			op := newRouteOperation(route, t.Context())
			resp, err := h.executeExplicitRouteRequest(withRouteOperation(t.Context(), op), route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
			if resp != nil {
				_ = resp.Body.Close()
			}
			if calls.Load() != 1 {
				t.Fatalf("inference calls=%d, want 1", calls.Load())
			}
			if status == http.StatusTooManyRequests {
				if upstreamStatusCode(err, 0) != http.StatusServiceUnavailable || providerRequestErrorCode(err) != upstreamAuthUnavailableCode || !strings.Contains(err.Error(), "run az login") {
					t.Fatalf("lost canonical local auth error: resp=%v err=%v", resp, err)
				}
			} else if err != nil || resp == nil || resp.StatusCode != status {
				t.Fatalf("ambiguous delivery must take precedence: resp=%v err=%v", resp, err)
			}
			sends, switches, _ := op.snapshot()
			if sends != 1 || switches != 1 {
				t.Fatalf("sends=%d switches=%d", sends, switches)
			}
		})
	}
}

func TestExplicitRouteUpstreamAuthRejectionDoesNotFailOver(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			var calls atomic.Int32
			h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				calls.Add(1)
				return routeExecutorTestResponse(req, status, nil, `{"error":{"message":"upstream authentication rejected"}}`), nil
			})}, routeModePriorityFailover, 2, 2,
				explicitRouteTestProvider("primary", "http://primary.example", "key"),
				explicitRouteTestProvider("secondary", "http://secondary.example", "key"))
			t.Cleanup(h.BeginShutdown)
			resp, err := h.executeExplicitRouteRequest(t.Context(), route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
			if resp == nil || err != nil {
				t.Fatalf("lost upstream rejection: resp=%v err=%v", resp, err)
			}
			_ = resp.Body.Close()
			if calls.Load() != 1 || resp.StatusCode != status {
				t.Fatalf("calls=%d status=%d", calls.Load(), resp.StatusCode)
			}
		})
	}
}

func TestExplicitRouteCredentialFailureWithCopilotSource(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	primary := explicitRouteTestProvider("copilot", "http://copilot.example", "")
	primary.kind = providerTypeCopilot
	var calls atomic.Int32
	h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		if req.URL.Host != "secondary.example" || req.Header.Get("api-key") != "key" || req.Header.Get("Authorization") != "" {
			t.Errorf("bad failover request: url=%s headers=%v", req.URL, req.Header)
		}
		return routeExecutorTestResponse(req, 200, nil, `{"status":"completed","output":[]}`), nil
	})}, routeModePriorityFailover, 2, 1, primary,
		explicitRouteTestProvider("secondary", "http://secondary.example", "key"))
	h.auth = auth.NewTestAuthenticator("")
	h.auth.DisableAutoDeviceFlow = true
	t.Cleanup(h.BeginShutdown)
	op := newRouteOperation(route, t.Context())
	resp, err := h.executeExplicitRouteRequest(withRouteOperation(t.Context(), op), route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
	if err != nil || resp == nil {
		t.Fatalf("credential failure did not fail over: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	sends, switches, trace := op.snapshot()
	if calls.Load() != 1 || sends != 1 || switches != 1 || len(trace) != 2 || trace[0].Decision != routeRetrySwitchTarget {
		t.Fatalf("calls=%d sends=%d switches=%d trace=%+v", calls.Load(), sends, switches, trace)
	}
}

func TestExplicitRouteCredentialFailureAfterAdmission(t *testing.T) {
	primary := explicitRouteTestProvider("primary", "http://primary.example", "")
	primary.authMode = providerAuthModeAzureIdentity
	var authCalls, sends atomic.Int32
	primary.azureToken = azureRetryTokenSourceFunc(func(context.Context) (string, error) {
		if authCalls.Add(1) == 1 {
			return "initial-token", nil
		}
		return "", errors.New("Azure identity refresh unavailable")
	})
	h, route := explicitRouteTestHandler(t, &http.Client{Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		sends.Add(1)
		if req.URL.Host != "secondary.example" {
			t.Errorf("inference sent without refreshed credentials: %s", req.URL.Host)
		}
		return routeExecutorTestResponse(req, 200, nil, `{"status":"completed","output":[]}`), nil
	})}, routeModePriorityFailover, 2, 1, primary,
		explicitRouteTestProvider("secondary", "http://secondary.example", "key"))
	t.Cleanup(h.BeginShutdown)
	advance := azureTrafficTestClock(h)
	seed := azureTrafficTestRequest(t, h, t.Context(), "primary", "http://primary.example", "deployment-a")
	azureRouteTrafficFromRequest(seed).observe(429, http.Header{"Retry-After": {"1"}})
	advance(time.Second)
	op := newRouteOperation(route, t.Context())
	resp, err := h.executeExplicitRouteRequest(withRouteOperation(t.Context(), op), route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
	if err != nil || resp == nil {
		t.Fatalf("post-admission auth failure did not fail over: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	networkSends, switches, trace := op.snapshot()
	if authCalls.Load() != 2 || sends.Load() != 1 || networkSends != 1 || switches != 1 || len(trace) != 2 || trace[0].Decision != routeRetrySwitchTarget || !trace[0].CleanupDone {
		t.Fatalf("auth calls=%d sends=%d network sends=%d switches=%d trace=%+v", authCalls.Load(), sends.Load(), networkSends, switches, trace)
	}
}
