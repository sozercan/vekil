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
	defer func() { _ = resp.Body.Close() }()
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

func TestExplicitRouteCapturedFinalResponseSanitizesProxyRequestID(t *testing.T) {
	t.Parallel()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Vekil-Request-ID", "upstream-spoof")
		w.Header().Set("X-Request-Id", "upstream-request-id")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"quota","type":"rate_limit_error"}}`)
	}))
	defer upstream.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePrimaryOnly, 1, 1,
		explicitRouteTestProvider("primary", upstream.URL, "primary-key"),
	)
	operation := newRouteOperation(route, context.Background())
	ctx := withRouteOperation(context.Background(), operation)
	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
	if err != nil {
		t.Fatalf("executeExplicitRouteRequest() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if got := headerGetCI(resp.Header, "X-Vekil-Request-ID"); got != "" {
		t.Fatalf("X-Vekil-Request-ID = %q, want empty", got)
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
	defer func() { _ = resp.Body.Close() }()
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

type routeExecutorRoundTripFunc func(*http.Request) (*http.Response, error)

func (f routeExecutorRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func routeExecutorTestResponse(req *http.Request, statusCode int, header http.Header, body string) *http.Response {
	if header == nil {
		header = make(http.Header)
	}
	return &http.Response{
		StatusCode:    statusCode,
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
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

func TestExplicitRouteFinalAttributionFollowsReturnedResult(t *testing.T) {
	tests := []struct {
		name              string
		secondaryResult   string
		wantStatus        int
		wantBodySubstring string
		wantErrSubstring  string
		wantTarget        string
		wantProvider      string
		wantKind          string
	}{
		{
			name:              "earlier primary response after later prewrite failure",
			secondaryResult:   "prewrite_error",
			wantStatus:        http.StatusTooManyRequests,
			wantBodySubstring: "primary quota",
			wantTarget:        "target-primary",
			wantProvider:      "primary",
			wantKind:          string(providerTypeAzureOpenAI),
		},
		{
			name:              "later successful response",
			secondaryResult:   "success",
			wantStatus:        http.StatusOK,
			wantBodySubstring: "resp-secondary",
			wantTarget:        "target-secondary",
			wantProvider:      "secondary",
			wantKind:          string(providerTypeOpenAICompatible),
		},
		{
			name:             "later ambiguous error",
			secondaryResult:  "ambiguous_error",
			wantErrSubstring: "secondary ambiguous failure",
			wantTarget:       "target-secondary",
			wantProvider:     "secondary",
			wantKind:         string(providerTypeOpenAICompatible),
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Hostname() {
				case "primary.example":
					return routeExecutorTestResponse(req, http.StatusTooManyRequests, nil, `{"error":{"message":"primary quota","type":"rate_limit_error"}}`), nil
				case "secondary.example":
					switch tc.secondaryResult {
					case "success":
						return routeExecutorTestResponse(req, http.StatusOK, nil, `{"id":"resp-secondary","status":"completed","output":[]}`), nil
					case "ambiguous_error":
						if trace := httptrace.ContextClientTrace(req.Context()); trace != nil && trace.WroteHeaders != nil {
							trace.WroteHeaders()
						}
						return nil, errors.New("secondary ambiguous failure")
					default:
						return nil, errors.New("secondary prewrite failure")
					}
				default:
					t.Fatalf("unexpected target host %q", req.URL.Hostname())
					return nil, errors.New("unexpected target")
				}
			})

			primary := explicitRouteTestProvider("primary", "https://primary.example", "one")
			secondary := explicitRouteTestProvider("secondary", "https://secondary.example", "two")
			secondary.kind = providerTypeOpenAICompatible
			secondary.paths = providerEndpointPolicyFor(providerTypeOpenAICompatible).defaultEndpointPaths()
			h, route := explicitRouteTestHandler(t, &http.Client{Transport: transport}, routeModePriorityFailover, 2, 2, primary, secondary)
			inbound, summary := WithRequestSummary(context.Background())
			operation := newRouteOperation(route, inbound)
			ctx := withRouteOperation(context.Background(), operation)

			resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
			if tc.wantErrSubstring != "" {
				if resp != nil {
					_ = resp.Body.Close()
					t.Fatalf("response = %#v, want error", resp)
				}
				if err == nil || !strings.Contains(err.Error(), tc.wantErrSubstring) {
					t.Fatalf("error = %v, want substring %q", err, tc.wantErrSubstring)
				}
			} else {
				if err != nil {
					t.Fatalf("error = %v", err)
				}
				if resp == nil || resp.StatusCode != tc.wantStatus {
					t.Fatalf("response = %#v, want status %d", resp, tc.wantStatus)
				}
				responseBody, readErr := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if readErr != nil {
					t.Fatalf("read response body: %v", readErr)
				}
				if tc.wantBodySubstring != "" && !strings.Contains(string(responseBody), tc.wantBodySubstring) {
					t.Fatalf("response body = %s, want substring %q", responseBody, tc.wantBodySubstring)
				}
			}

			stats := readSummaryForStats(summary)
			if stats.finalTarget != tc.wantTarget || stats.provider != tc.wantProvider || stats.kind != tc.wantKind {
				t.Fatalf("final attribution = %q/%q/%q, want %q/%q/%q", stats.finalTarget, stats.provider, stats.kind, tc.wantTarget, tc.wantProvider, tc.wantKind)
			}
			if stats.upstreamSends != 2 || stats.targetSwitches != 1 {
				t.Fatalf("route counts = sends:%d switches:%d, want 2/1", stats.upstreamSends, stats.targetSwitches)
			}
			lastTarget, lastProvider, lastKind := summary.lastUpstreamAttempt()
			if lastTarget != "target-secondary" || lastProvider != "secondary" || lastKind != string(providerTypeOpenAICompatible) {
				t.Fatalf("last attempt = %q/%q/%q, want secondary attribution", lastTarget, lastProvider, lastKind)
			}
		})
	}
}

func TestExplicitRouteCapturedResponsePreservesUpstreamRequestIDInTrace(t *testing.T) {
	tests := []struct {
		name                string
		requestIDHeader     string
		requestID           string
		primaryStatus       int
		primaryBody         string
		secondaryPrewrite   bool
		wantPrimaryDelivery requestDelivery
		wantPrimaryDecision routeRetryDecision
		wantTraceLen        int
	}{
		{
			name:                "certified rejection",
			requestIDHeader:     "X-Request-Id",
			requestID:           "request-primary",
			primaryStatus:       http.StatusTooManyRequests,
			primaryBody:         `{"error":{"message":"primary quota","type":"rate_limit_error"}}`,
			wantPrimaryDelivery: requestExplicitlyRejected,
			wantPrimaryDecision: routeRetrySuppressedMode,
			wantTraceLen:        1,
		},
		{
			name:                "ambiguous response",
			requestIDHeader:     "X-Azure-Request-Id",
			requestID:           "azure-primary",
			primaryStatus:       http.StatusServiceUnavailable,
			primaryBody:         `{"error":{"message":"gateway unavailable"}}`,
			wantPrimaryDelivery: requestDeliveredOrAmbiguous,
			wantPrimaryDecision: routeRetryAccepted,
			wantTraceLen:        1,
		},
		{
			name:                "final selected earlier response",
			requestIDHeader:     "Openai-Request-Id",
			requestID:           "openai-primary",
			primaryStatus:       http.StatusTooManyRequests,
			primaryBody:         `{"error":{"message":"primary quota","type":"rate_limit_error"}}`,
			secondaryPrewrite:   true,
			wantPrimaryDelivery: requestExplicitlyRejected,
			wantPrimaryDecision: routeRetrySwitchTarget,
			wantTraceLen:        2,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Hostname() {
				case "primary.example":
					headers := http.Header{
						"Authorization":       []string{"Bearer upstream-secret"},
						"Proxy-Authorization": []string{"Basic upstream-secret"},
						"Api-Key":             []string{"upstream-api-key"},
						"X-Api-Key":           []string{"upstream-x-api-key"},
						"X-Vekil-Request-ID":  []string{"spoofed-operation-id"},
						"X-Codex-Turn-State":  []string{"private-turn-state"},
						tc.requestIDHeader:    []string{tc.requestID},
					}
					return routeExecutorTestResponse(req, tc.primaryStatus, headers, tc.primaryBody), nil
				case "secondary.example":
					if !tc.secondaryPrewrite {
						t.Fatalf("unexpected secondary request")
					}
					return nil, errors.New("secondary prewrite failure")
				default:
					t.Fatalf("unexpected target host %q", req.URL.Hostname())
					return nil, errors.New("unexpected target")
				}
			})

			providers := []*providerRuntime{explicitRouteTestProvider("primary", "https://primary.example", "one")}
			mode, maxTargets, maxSends := routeModePrimaryOnly, 1, 1
			if tc.secondaryPrewrite {
				providers = append(providers, explicitRouteTestProvider("secondary", "https://secondary.example", "two"))
				mode, maxTargets, maxSends = routeModePriorityFailover, 2, 2
			}
			h, route := explicitRouteTestHandler(t, &http.Client{Transport: transport}, mode, maxTargets, maxSends, providers...)
			inbound, summary := WithRequestSummary(context.Background())
			operation := newRouteOperation(route, inbound)
			ctx := withRouteOperation(context.Background(), operation)

			resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model"}`), nil, "public-model", false)
			if err != nil {
				t.Fatalf("executeExplicitRouteRequest() error = %v", err)
			}
			if resp == nil {
				t.Fatal("response = nil")
			}
			defer func() { _ = resp.Body.Close() }()
			if resp.StatusCode != tc.primaryStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.primaryStatus)
			}
			body, readErr := io.ReadAll(resp.Body)
			if readErr != nil {
				t.Fatalf("read response body: %v", readErr)
			}
			if string(body) != tc.primaryBody {
				t.Fatalf("body = %s, want %s", body, tc.primaryBody)
			}

			for _, name := range append(append([]string{}, upstreamRequestIDHeaderNames...),
				"Authorization", "Proxy-Authorization", "Api-Key", "X-Api-Key", "X-Vekil-Request-ID", "X-Codex-Turn-State") {
				if got := headerGetCI(resp.Header, name); got != "" {
					t.Fatalf("sanitized response header %s = %q", name, got)
				}
			}

			_, _, trace := operation.snapshot()
			if len(trace) != tc.wantTraceLen {
				t.Fatalf("trace = %+v, want %d entries", trace, tc.wantTraceLen)
			}
			primaryTrace := trace[0]
			if primaryTrace.TargetID != "target-primary" || primaryTrace.StatusCode != tc.primaryStatus {
				t.Fatalf("primary trace = %+v", primaryTrace)
			}
			if primaryTrace.UpstreamID != tc.requestID {
				t.Fatalf("primary trace upstream ID = %q, want %q (trace=%+v)", primaryTrace.UpstreamID, tc.requestID, trace)
			}
			if primaryTrace.Delivery != tc.wantPrimaryDelivery {
				t.Fatalf("primary trace delivery = %q, want %q", primaryTrace.Delivery, tc.wantPrimaryDelivery)
			}
			if primaryTrace.Decision != tc.wantPrimaryDecision {
				t.Fatalf("primary trace decision = %q, want %q", primaryTrace.Decision, tc.wantPrimaryDecision)
			}
			if tc.secondaryPrewrite {
				secondaryTrace := trace[1]
				if secondaryTrace.TargetID != "target-secondary" || secondaryTrace.UpstreamID != "" || secondaryTrace.Decision != routeRetrySuppressedDelivery {
					t.Fatalf("secondary trace = %+v, want prewrite delivery failure", secondaryTrace)
				}
			}
			stats := readSummaryForStats(summary)
			if stats.finalTarget != "target-primary" {
				t.Fatalf("final target = %q, want target-primary", stats.finalTarget)
			}
			if stats.upstreamID != tc.requestID {
				t.Fatalf("final upstream ID = %q, want %q", stats.upstreamID, tc.requestID)
			}
		})
	}
}

func TestExplicitRouteExhaustionAccountingRequiresBudgetOrNoTarget(t *testing.T) {
	tests := []struct {
		name               string
		endpoint           string
		stream             bool
		body               string
		maxTargets         int
		maxSends           int
		configureProviders func(primary, secondary *providerRuntime)
		prepareOperation   func(t *testing.T, operation *routeOperation)
		roundTrip          func(t *testing.T, secondaryCalls *atomic.Int32) routeExecutorRoundTripFunc
		wantErr            bool
		wantStatus         int
		wantSends          int
		wantExhaustions    int64
		wantTrace          bool
		wantTraceDecision  routeRetryDecision
	}{
		{
			name:       "no eligible target",
			endpoint:   providerEndpointMessages,
			body:       `{"model":"public-model"}`,
			maxTargets: 2,
			maxSends:   2,
			configureProviders: func(_ *providerRuntime, secondary *providerRuntime) {
				secondary.kind = providerTypeOpenAICompatible
			},
			roundTrip: func(_ *testing.T, secondaryCalls *atomic.Int32) routeExecutorRoundTripFunc {
				return func(req *http.Request) (*http.Response, error) {
					if req.URL.Hostname() == "secondary.example" {
						secondaryCalls.Add(1)
					}
					return routeExecutorTestResponse(req, http.StatusTooManyRequests, nil, `{"error":{"code":"rate_limit_exceeded"}}`), nil
				}
			},
			wantStatus:        http.StatusTooManyRequests,
			wantSends:         1,
			wantExhaustions:   1,
			wantTrace:         true,
			wantTraceDecision: routeRetrySwitchTarget,
		},
		{
			name:       "target budget exhausted",
			endpoint:   providerEndpointResponses,
			body:       `{"model":"public-model"}`,
			maxTargets: 1,
			maxSends:   2,
			roundTrip: func(_ *testing.T, secondaryCalls *atomic.Int32) routeExecutorRoundTripFunc {
				return func(req *http.Request) (*http.Response, error) {
					if req.URL.Hostname() == "secondary.example" {
						secondaryCalls.Add(1)
					}
					return routeExecutorTestResponse(req, http.StatusTooManyRequests, nil, `{"error":{"code":"rate_limit_exceeded"}}`), nil
				}
			},
			wantStatus:        http.StatusTooManyRequests,
			wantSends:         1,
			wantExhaustions:   1,
			wantTrace:         true,
			wantTraceDecision: routeRetrySuppressedMode,
		},
		{
			name:       "send budget exhausted",
			endpoint:   providerEndpointResponses,
			body:       `{"model":"public-model"}`,
			maxTargets: 2,
			maxSends:   2,
			prepareOperation: func(t *testing.T, operation *routeOperation) {
				for range 2 {
					if reserved, decision := operation.reserveSendAtDispatch(context.Background(), false); !reserved || decision != routeRetryAccepted {
						t.Fatalf("preconsume send = reserved:%v decision:%q", reserved, decision)
					}
				}
			},
			roundTrip: func(t *testing.T, _ *atomic.Int32) routeExecutorRoundTripFunc {
				return func(*http.Request) (*http.Response, error) {
					t.Fatal("unexpected upstream dispatch")
					return nil, nil
				}
			},
			wantErr:           true,
			wantSends:         2,
			wantExhaustions:   1,
			wantTrace:         true,
			wantTraceDecision: routeRetrySuppressedBudget,
		},
		{
			name:       "ambiguous delivery leaves target",
			endpoint:   providerEndpointResponses,
			body:       `{"model":"public-model"}`,
			maxTargets: 2,
			maxSends:   2,
			roundTrip: func(_ *testing.T, secondaryCalls *atomic.Int32) routeExecutorRoundTripFunc {
				return func(req *http.Request) (*http.Response, error) {
					if req.URL.Hostname() == "secondary.example" {
						secondaryCalls.Add(1)
						return routeExecutorTestResponse(req, http.StatusOK, nil, `{}`), nil
					}
					if trace := httptrace.ContextClientTrace(req.Context()); trace != nil && trace.WroteHeaders != nil {
						trace.WroteHeaders()
					}
					return nil, errors.New("ambiguous transport failure")
				}
			},
			wantErr:           true,
			wantSends:         1,
			wantTrace:         true,
			wantTraceDecision: routeRetrySuppressedDelivery,
		},
		{
			name:       "semantic progress leaves target",
			endpoint:   providerEndpointResponses,
			stream:     true,
			body:       `{"model":"public-model","stream":true}`,
			maxTargets: 2,
			maxSends:   2,
			roundTrip: func(_ *testing.T, secondaryCalls *atomic.Int32) routeExecutorRoundTripFunc {
				return func(req *http.Request) (*http.Response, error) {
					if req.URL.Hostname() == "secondary.example" {
						secondaryCalls.Add(1)
						return routeExecutorTestResponse(req, http.StatusOK, nil, `{}`), nil
					}
					header := make(http.Header)
					header.Set("Content-Type", "text/event-stream")
					body := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-progress\"}}\n\n" +
						"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-progress\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"quota\"},\"usage\":{\"input_tokens\":3,\"output_tokens\":1,\"total_tokens\":4}}}\n\n"
					return routeExecutorTestResponse(req, http.StatusOK, header, body), nil
				}
			},
			wantErr:           true,
			wantSends:         1,
			wantTrace:         true,
			wantTraceDecision: routeRetrySuppressedProgress,
		},
		{
			name:       "nonretryable preparation failure leaves target",
			endpoint:   providerEndpointResponses,
			body:       `{"model":`,
			maxTargets: 2,
			maxSends:   2,
			roundTrip: func(t *testing.T, _ *atomic.Int32) routeExecutorRoundTripFunc {
				return func(*http.Request) (*http.Response, error) {
					t.Fatal("unexpected upstream dispatch")
					return nil, nil
				}
			},
			wantErr:           true,
			wantTrace:         true,
			wantTraceDecision: routeRetrySuppressedNonretryable,
		},
		{
			name:       "state binding failure leaves target",
			endpoint:   providerEndpointChatCompletions,
			body:       `{"model":"public-model"}`,
			maxTargets: 2,
			maxSends:   2,
			roundTrip: func(_ *testing.T, secondaryCalls *atomic.Int32) routeExecutorRoundTripFunc {
				return func(req *http.Request) (*http.Response, error) {
					if req.URL.Hostname() == "secondary.example" {
						secondaryCalls.Add(1)
						return routeExecutorTestResponse(req, http.StatusOK, nil, `{}`), nil
					}
					header := make(http.Header)
					header.Add("X-Codex-Turn-State", "state-one")
					header.Add("X-Codex-Turn-State", "state-two")
					return routeExecutorTestResponse(req, http.StatusOK, header, `{}`), nil
				}
			},
			wantErr:           true,
			wantSends:         1,
			wantTrace:         true,
			wantTraceDecision: routeRetrySuppressedState,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var secondaryCalls atomic.Int32
			primary := explicitRouteTestProvider("primary", "https://primary.example", "one")
			secondary := explicitRouteTestProvider("secondary", "https://secondary.example", "two")
			if tc.configureProviders != nil {
				tc.configureProviders(primary, secondary)
			}
			client := &http.Client{Transport: tc.roundTrip(t, &secondaryCalls)}
			h, route := explicitRouteTestHandler(t, client, routeModePriorityFailover, tc.maxTargets, tc.maxSends, primary, secondary)
			h.stats = newStatsCollector()
			operation := newRouteOperation(route, context.Background())
			if tc.prepareOperation != nil {
				tc.prepareOperation(t, operation)
			}
			ctx := withRouteOperation(context.Background(), operation)

			resp, err := h.executeExplicitRouteRequest(ctx, route, tc.endpoint, []byte(tc.body), nil, "public-model", tc.stream)
			if resp != nil {
				defer func() { _ = resp.Body.Close() }()
			}
			if tc.wantErr && err == nil {
				t.Fatal("error = nil")
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("error = %v", err)
			}
			if tc.wantStatus != 0 {
				if resp == nil || resp.StatusCode != tc.wantStatus {
					t.Fatalf("response = %#v, want status %d", resp, tc.wantStatus)
				}
			}
			if got := secondaryCalls.Load(); got != 0 {
				t.Fatalf("secondary calls = %d, want 0", got)
			}

			sends, _, trace := operation.snapshot()
			if sends != tc.wantSends {
				t.Fatalf("upstream sends = %d, want %d", sends, tc.wantSends)
			}
			if tc.wantTrace {
				if len(trace) == 0 {
					t.Fatal("attempt trace is empty")
				}
				if got := trace[len(trace)-1].Decision; got != tc.wantTraceDecision {
					t.Fatalf("last trace decision = %q, want %q (trace=%+v)", got, tc.wantTraceDecision, trace)
				}
			} else if len(trace) != 0 {
				t.Fatalf("attempt trace = %+v, want empty", trace)
			}

			if got := h.stats.snapshot().RouteExhaustions; got != tc.wantExhaustions {
				t.Fatalf("route exhaustions after execute = %d, want %d", got, tc.wantExhaustions)
			}
			// Re-evaluating the terminal operation must neither invent an exhaustion
			// nor double-count a real one.
			h.recordExplicitRouteExhaustion(operation, tc.endpoint)
			h.recordExplicitRouteExhaustion(operation, tc.endpoint)
			if got := h.stats.snapshot().RouteExhaustions; got != tc.wantExhaustions {
				t.Fatalf("route exhaustions after repeated accounting = %d, want %d", got, tc.wantExhaustions)
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
	defer func() { _ = resp.Body.Close() }()
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
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if strings.Contains(string(body), "resp-hidden") || !strings.Contains(string(body), "resp-visible") {
		t.Fatalf("stream body = %s", body)
	}
}

func TestExplicitRouteStreamingPreambleByteBoundCommitsWithoutSwitch(t *testing.T) {
	var secondaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		created := "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-large-primary\",\"metadata\":{\"padding\":\"" + strings.Repeat("x", 96*1024) + "\"}}}\n\n"
		failed := "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-large-primary\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"late quota\"}}}\n\n"
		_, _ = io.WriteString(w, created)
		_, _ = io.WriteString(w, failed)
	}))
	defer primary.Close()
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-secondary\"}}\n\n")
	}))
	defer secondary.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePriorityFailover, 2, 2,
		explicitRouteTestProvider("primary", primary.URL, "one"),
		explicitRouteTestProvider("secondary", secondary.URL, "two"),
	)
	requestCtx, summary := WithRequestSummary(context.Background())
	operation := newRouteOperation(route, requestCtx)
	ctx := withRouteOperation(requestCtx, operation)
	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model","stream":true}`), nil, "public-model", true)
	if err != nil {
		t.Fatalf("executeExplicitRouteRequest() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "resp-large-primary") || !strings.Contains(string(body), "late quota") {
		t.Fatalf("body did not preserve committed primary stream: %s", body)
	}
	if secondaryCalls.Load() != 0 {
		t.Fatalf("secondary calls = %d, want 0", secondaryCalls.Load())
	}
	if got := summary.TargetSwitchCount(); got != 0 {
		t.Fatalf("target switches = %d, want 0", got)
	}
}

func TestExplicitRouteStreamingTopLevelErrorSwitchesBeforeCommit(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"primary quota\"}\n\n")
	}))
	defer primary.Close()
	var secondaryCalls atomic.Int32
	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-secondary\"}}\n\n")
	}))
	defer secondary.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePriorityFailover, 2, 2,
		explicitRouteTestProvider("primary", primary.URL, "one"),
		explicitRouteTestProvider("secondary", secondary.URL, "two"),
	)
	ctx := withRouteOperation(context.Background(), newRouteOperation(route, context.Background()))
	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model","stream":true}`), nil, "public-model", true)
	if err != nil {
		t.Fatalf("executeExplicitRouteRequest() error = %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	if !strings.Contains(string(body), "resp-secondary") || strings.Contains(string(body), "primary quota") {
		t.Fatalf("stream body = %s", body)
	}
	if secondaryCalls.Load() != 1 {
		t.Fatalf("secondary calls = %d, want 1", secondaryCalls.Load())
	}
}

func TestExplicitRouteStreamingTopLevelErrorPreservesDiagnostics(t *testing.T) {
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"primary quota\",\"headers\":{\"Retry-After\":\"600\",\"X-Request-Id\":\"event-request-id\"}}\n\n")
	}))
	defer primary.Close()

	h, route := explicitRouteTestHandler(t, http.DefaultClient, routeModePrimaryOnly, 1, 1,
		explicitRouteTestProvider("primary", primary.URL, "one"),
	)
	ctx := withRouteOperation(context.Background(), newRouteOperation(route, context.Background()))
	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model","stream":true}`), nil, "public-model", true)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("response = %#v, want translated error", resp)
	}
	if got := upstreamStatusCode(err, 0); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d error=%v, want 429", got, err)
	}
	if !strings.Contains(formatUpstreamRequestFailure(err, "fallback"), "primary quota") {
		t.Fatalf("formatted error = %q, want provider message", formatUpstreamRequestFailure(err, "fallback"))
	}
	retryAfter, headers := upstreamErrorRetryMetadata(err)
	if retryAfter != "600" {
		t.Fatalf("Retry-After metadata = %q, want 600", retryAfter)
	}
	if got := headerGetCI(headers, "X-Request-Id"); got != "event-request-id" {
		t.Fatalf("X-Request-Id = %q, want event-request-id", got)
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
		w.Header().Set("X-Vekil-Request-ID", "upstream-spoof")
		w.Header().Set("X-Request-Id", "secondary-request-id")
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
	if got := w.Header().Get("X-Vekil-Request-ID"); got == "" || got == "upstream-spoof" || got != summary.OperationID() {
		t.Fatalf("X-Vekil-Request-ID=%q summary=%q", got, summary.OperationID())
	}
	if got := w.Header().Get("X-Request-Id"); got != "secondary-request-id" {
		t.Fatalf("X-Request-Id = %q, want secondary-request-id", got)
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

func TestConfiguredExplicitResponsesRoutePreparesEachTargetFromLogicalRequest(t *testing.T) {
	requestBody := `{
		"model":"public-model",
		"input":"hello",
		"tools":[
			{"type":"image_generation"},
			{"type":"function","name":"lookup","parameters":{"type":"object"}}
		],
		"tool_choice":{"type":"image_generation"}
	}`

	for _, tc := range []struct {
		name  string
		order []providerType
	}{
		{name: "azure_then_openai_compatible", order: []providerType{providerTypeAzureOpenAI, providerTypeOpenAICompatible}},
		{name: "openai_compatible_then_azure", order: []providerType{providerTypeOpenAICompatible, providerTypeAzureOpenAI}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			captures := map[providerType]chan []byte{
				providerTypeAzureOpenAI:      make(chan []byte, 1),
				providerTypeOpenAICompatible: make(chan []byte, 1),
			}
			servers := make(map[providerType]*httptest.Server, len(captures))
			for kind, capture := range captures {
				kind := kind
				capture := capture
				servers[kind] = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodGet {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, `{"data":[]}`)
						return
					}
					body, err := io.ReadAll(r.Body)
					if err != nil {
						t.Errorf("%s read body: %v", kind, err)
					}
					capture <- body
					w.Header().Set("Content-Type", "application/json")
					if kind == tc.order[0] {
						w.WriteHeader(http.StatusTooManyRequests)
						_, _ = io.WriteString(w, `{"error":{"message":"quota","type":"rate_limit_error"}}`)
						return
					}
					_, _ = io.WriteString(w, `{"id":"resp-success","model":"physical-success","status":"completed","output":[]}`)
				}))
			}
			defer func() {
				for _, server := range servers {
					server.Close()
				}
			}()

			providerID := func(kind providerType) string {
				if kind == providerTypeAzureOpenAI {
					return "azure"
				}
				return "openai"
			}
			providerConfig := func(kind providerType, isDefault bool) ProviderConfig {
				config := ProviderConfig{
					ID:      providerID(kind),
					Type:    string(kind),
					Default: isDefault,
				}
				if kind == providerTypeAzureOpenAI {
					config.BaseURL = servers[kind].URL + "/openai/v1"
					config.APIKey = "azure-key"
				} else {
					config.BaseURL = servers[kind].URL
					config.AuthType = "none"
				}
				return config
			}

			providers := make([]ProviderConfig, 0, len(tc.order))
			targets := make([]ModelRouteTargetConfig, 0, len(tc.order))
			for i, kind := range tc.order {
				id := providerID(kind)
				providers = append(providers, providerConfig(kind, i == 0))
				targets = append(targets, ModelRouteTargetConfig{
					ID:            id + "-target",
					Provider:      id,
					UpstreamModel: id + "-model",
				})
			}

			h, err := NewProxyHandler(auth.NewTestAuthenticator("test-token"), logger.NewWithWriter(logger.LevelError, io.Discard),
				WithProvidersConfig(ProvidersConfig{
					SchemaVersion: 2,
					Providers:     providers,
					ModelRoutes: []ModelRouteConfig{{
						ID:        "route-public",
						PublicID:  "public-model",
						Endpoints: []string{providerEndpointResponses},
						Targets:   targets,
						Routing: ModelRouteRoutingConfig{
							Mode:              string(routeModePriorityFailover),
							MaxTargetAttempts: 2,
							MaxUpstreamSends:  2,
						},
					}},
				}),
			)
			if err != nil {
				t.Fatalf("NewProxyHandler() error = %v", err)
			}
			defer h.BeginShutdown()

			w := httptest.NewRecorder()
			h.HandleResponses(w, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(requestBody)))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}

			for kind, capture := range captures {
				var body []byte
				select {
				case body = <-capture:
				default:
					t.Fatalf("%s target was not called", kind)
				}
				var payload struct {
					Model      string            `json:"model"`
					Tools      []json.RawMessage `json:"tools"`
					ToolChoice json.RawMessage   `json:"tool_choice"`
				}
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatalf("decode %s body %s: %v", kind, body, err)
				}
				if want := providerID(kind) + "-model"; payload.Model != want {
					t.Errorf("%s model = %q, want %q", kind, payload.Model, want)
				}
				toolTypes := make(map[string]bool, len(payload.Tools))
				for _, rawTool := range payload.Tools {
					toolTypes[responsesToolType(rawTool)] = true
				}
				if !toolTypes["function"] {
					t.Errorf("%s tools = %s, missing function tool", kind, body)
				}
				if kind == providerTypeAzureOpenAI {
					if toolTypes["image_generation"] {
						t.Errorf("azure tools retained image_generation: %s", body)
					}
					if len(payload.ToolChoice) != 0 {
						t.Errorf("azure tool_choice retained unsupported selection: %s", payload.ToolChoice)
					}
					continue
				}
				if !toolTypes["image_generation"] {
					t.Errorf("openai-compatible tools prematurely stripped image_generation: %s", body)
				}
				var toolChoice struct {
					Type string `json:"type"`
				}
				if err := json.Unmarshal(payload.ToolChoice, &toolChoice); err != nil || toolChoice.Type != "image_generation" {
					t.Errorf("openai-compatible tool_choice = %s, want image_generation (err=%v)", payload.ToolChoice, err)
				}
			}
		})
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
	defer func() { _ = resp.Body.Close() }()
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

func TestExplicitRouteStreamingFailureWithUsageTranslatesWithoutSwitch(t *testing.T) {
	var secondaryCalls atomic.Int32
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-partial-usage\"}}\n\n"+
			"event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp-partial-usage\",\"error\":{\"code\":\"rate_limit_exceeded\",\"message\":\"quota\"},\"usage\":{\"input_tokens\":11,\"output_tokens\":4,\"total_tokens\":15}}}\n\n")
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
	requestCtx, summary := WithRequestSummary(context.Background())
	operation := newRouteOperation(route, requestCtx)
	ctx := withRouteOperation(requestCtx, operation)
	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, []byte(`{"model":"public-model","stream":true}`), nil, "public-model", true)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("response = %#v, want translated route failure", resp)
	}
	if got := upstreamStatusCode(err, 0); got != http.StatusTooManyRequests {
		t.Fatalf("status = %d error=%v, want 429", got, err)
	}
	if secondaryCalls.Load() != 0 {
		t.Fatalf("secondary calls = %d, want 0", secondaryCalls.Load())
	}
	if got := summary.TargetSwitchCount(); got != 0 {
		t.Fatalf("target switches = %d, want 0", got)
	}
	usage := readSummaryForStats(summary)
	if usage.prompt != 11 || usage.completion != 4 || usage.total != 15 {
		t.Fatalf("usage = prompt:%d completion:%d total:%d, want 11/4/15", usage.prompt, usage.completion, usage.total)
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
	defer func() { _ = resp.Body.Close() }()
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
	defer func() { _ = resp.Body.Close() }()
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
