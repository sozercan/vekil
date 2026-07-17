package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/net/http2"
)

type routeReplayDispatch struct {
	host       string
	reserved   int
	getBodyNil bool
}

type routeReplayDispatchLog struct {
	mu      sync.Mutex
	entries []routeReplayDispatch
}

func (l *routeReplayDispatchLog) record(req *http.Request) {
	reserved := -1
	if operation := routeOperationFromContext(req.Context()); operation != nil {
		reserved, _, _ = operation.snapshot()
	}

	l.mu.Lock()
	l.entries = append(l.entries, routeReplayDispatch{
		host:       req.URL.Host,
		reserved:   reserved,
		getBodyNil: req.GetBody == nil,
	})
	l.mu.Unlock()
}

func (l *routeReplayDispatchLog) snapshot() []routeReplayDispatch {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]routeReplayDispatch(nil), l.entries...)
}

func requireBudgetedRouteDispatches(t *testing.T, log *routeReplayDispatchLog, wantHosts ...string) {
	t.Helper()
	got := log.snapshot()
	if len(got) != len(wantHosts) {
		t.Fatalf("RoundTrip dispatches = %+v, want hosts %v", got, wantHosts)
	}
	for i, dispatch := range got {
		if dispatch.host != wantHosts[i] {
			t.Fatalf("dispatch %d host = %q, want %q (all=%+v)", i+1, dispatch.host, wantHosts[i], got)
		}
		if dispatch.reserved != i+1 {
			t.Fatalf("dispatch %d observed reserved sends = %d, want %d (all=%+v)", i+1, dispatch.reserved, i+1, got)
		}
		if !dispatch.getBodyNil {
			t.Fatalf("dispatch %d retained GetBody; inference sends must not be transparently replayable (all=%+v)", i+1, got)
		}
	}
}

func TestRouteInferenceTransportHTTP1StaleReusedConnectionResetIsNotReplayedOrFailedOver(t *testing.T) {
	var primaryRequests atomic.Int32
	var secondaryRequests atomic.Int32
	serverErrors := make(chan error, 4)
	recordServerError := func(err error) {
		select {
		case serverErrors <- err:
		default:
		}
	}

	// Warm one keep-alive connection, then reset that reused connection after
	// accepting the inference body but before returning response bytes. With the
	// Idempotency-Key below and NewRequest's normal GetBody, net/http replays
	// this failure on a fresh connection; the explicit-route send must not.
	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/warmup" {
			_, _ = io.WriteString(w, "warm")
			return
		}

		requestNumber := primaryRequests.Add(1)
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			recordServerError(fmt.Errorf("read primary request body: %w", err))
			return
		}
		if requestNumber > 1 {
			// A transparent same-target replay would land here and turn the
			// ambiguous first dispatch into an apparently successful request.
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"unexpected-replay","status":"completed","output":[]}`)
			return
		}

		hijacker, ok := w.(http.Hijacker)
		if !ok {
			recordServerError(errors.New("HTTP/1 response writer does not support hijacking"))
			return
		}
		conn, _, err := hijacker.Hijack()
		if err != nil {
			recordServerError(fmt.Errorf("hijack primary connection: %w", err))
			return
		}
		tcpConn, ok := conn.(*net.TCPConn)
		if !ok {
			recordServerError(fmt.Errorf("primary connection type = %T, want *net.TCPConn", conn))
			_ = conn.Close()
			return
		}
		if err := tcpConn.SetLinger(0); err != nil {
			recordServerError(fmt.Errorf("set primary reset linger: %w", err))
		}
		if err := tcpConn.Close(); err != nil {
			recordServerError(fmt.Errorf("reset primary connection: %w", err))
		}
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondaryRequests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"secondary","status":"completed","output":[]}`)
	}))
	defer secondary.Close()

	baseTransport := http.DefaultTransport.(*http.Transport).Clone()
	baseTransport.ForceAttemptHTTP2 = false
	baseTransport.MaxIdleConnsPerHost = 1
	baseTransport.MaxConnsPerHost = 1
	defer baseTransport.CloseIdleConnections()

	var dispatches routeReplayDispatchLog
	client := &http.Client{
		Transport: routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if routeOperationFromContext(req.Context()) != nil {
				dispatches.record(req)
			}
			return baseTransport.RoundTrip(req)
		}),
		Timeout: 5 * time.Second,
	}

	warmupResp, err := client.Get(primary.URL + "/warmup")
	if err != nil {
		t.Fatalf("warm reused HTTP/1 connection: %v", err)
	}
	_, _ = io.Copy(io.Discard, warmupResp.Body)
	if err := warmupResp.Body.Close(); err != nil {
		t.Fatalf("close warmup response: %v", err)
	}

	var gotConnections atomic.Int32
	var reusedConnection atomic.Bool
	inbound := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			gotConnections.Add(1)
			if info.Reused {
				reusedConnection.Store(true)
			}
		},
	})
	h, route := explicitRouteTestHandler(t, client, routeModePriorityFailover, 2, 2,
		explicitRouteTestProvider("primary", primary.URL, "one"),
		explicitRouteTestProvider("secondary", secondary.URL, "two"),
	)
	operation := newRouteOperation(route, inbound)
	ctx := withRouteOperation(inbound, operation)

	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses,
		[]byte(`{"model":"public-model","input":"hello"}`),
		http.Header{"Idempotency-Key": []string{"route-replay-test"}}, "public-model", false)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("response = %#v, want ambiguous transport error", resp)
	}
	if err == nil {
		t.Fatal("error = nil, want reused-connection reset")
	}

	select {
	case serverErr := <-serverErrors:
		t.Fatal(serverErr)
	default:
	}
	if !reusedConnection.Load() {
		t.Fatal("route dispatch did not reuse the warmed HTTP/1 connection")
	}
	if got := gotConnections.Load(); got != 1 {
		t.Fatalf("GotConn callbacks = %d, want 1; an extra callback indicates an implicit transport replay", got)
	}
	if got := primaryRequests.Load(); got != 1 {
		t.Fatalf("primary requests = %d, want 1; request was transparently replayed on the same target", got)
	}
	if got := secondaryRequests.Load(); got != 0 {
		t.Fatalf("secondary requests = %d, want 0 after ambiguous primary delivery", got)
	}
	requireBudgetedRouteDispatches(t, &dispatches, primary.Listener.Addr().String())

	sends, switches, trace := operation.snapshot()
	if sends != 1 || switches != 0 {
		t.Fatalf("route budget = sends:%d switches:%d, want 1/0", sends, switches)
	}
	if len(trace) != 1 || trace[0].TargetID != "target-primary" ||
		trace[0].Delivery != requestDeliveredOrAmbiguous || trace[0].Decision != routeRetrySuppressedDelivery {
		t.Fatalf("route trace = %+v, want one ambiguous terminal primary attempt", trace)
	}
}

func TestRouteInferenceTransportHTTP2RetrySignalIsNotReplayedOrFailedOver(t *testing.T) {
	// net/http does not expose a supported server API for emitting a peer
	// REFUSED_STREAM, and its bundled HTTP/2 retry errors are internal details.
	// Inject the equivalent exported x/net/http2 error at the RoundTripper
	// boundary and drive httptrace's write signal instead. This proves the
	// contract visible to Vekil without coupling the test to Go's private HTTP/2
	// implementation: the request is non-rewindable, its sole dispatch already
	// owns one send reservation, and ambiguous delivery cannot retry or fail over.
	var dispatches routeReplayDispatchLog
	var primaryDispatches atomic.Int32
	var secondaryDispatches atomic.Int32
	transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		dispatches.record(req)
		switch req.URL.Hostname() {
		case "primary.example":
			primaryDispatches.Add(1)
			if trace := httptrace.ContextClientTrace(req.Context()); trace != nil {
				if trace.WroteHeaders != nil {
					trace.WroteHeaders()
				}
				if trace.WroteRequest != nil {
					trace.WroteRequest(httptrace.WroteRequestInfo{})
				}
			}
			return nil, http2.StreamError{
				StreamID: 1,
				Code:     http2.ErrCodeRefusedStream,
				Cause:    errors.New("received from peer"),
			}
		case "secondary.example":
			secondaryDispatches.Add(1)
			return routeExecutorTestResponse(req, http.StatusOK, nil, `{"id":"secondary","status":"completed","output":[]}`), nil
		default:
			return nil, fmt.Errorf("unexpected route host %q", req.URL.Host)
		}
	})

	h, route := explicitRouteTestHandler(t, &http.Client{Transport: transport}, routeModePriorityFailover, 2, 2,
		explicitRouteTestProvider("primary", "https://primary.example", "one"),
		explicitRouteTestProvider("secondary", "https://secondary.example", "two"),
	)
	operation := newRouteOperation(route, context.Background())
	ctx := withRouteOperation(context.Background(), operation)
	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses,
		[]byte(`{"model":"public-model","input":"hello"}`),
		http.Header{"Idempotency-Key": []string{"route-h2-replay-test"}}, "public-model", false)
	if resp != nil {
		_ = resp.Body.Close()
		t.Fatalf("response = %#v, want HTTP/2 stream error", resp)
	}
	if err == nil {
		t.Fatal("error = nil, want HTTP/2 REFUSED_STREAM failure")
	}
	if got := primaryDispatches.Load(); got != 1 {
		t.Fatalf("primary RoundTrip dispatches = %d, want 1", got)
	}
	if got := secondaryDispatches.Load(); got != 0 {
		t.Fatalf("secondary RoundTrip dispatches = %d, want 0 after ambiguous HTTP/2 delivery", got)
	}
	requireBudgetedRouteDispatches(t, &dispatches, "primary.example")

	sends, switches, trace := operation.snapshot()
	if sends != 1 || switches != 0 {
		t.Fatalf("route budget = sends:%d switches:%d, want 1/0", sends, switches)
	}
	if len(trace) != 1 || trace[0].TargetID != "target-primary" ||
		trace[0].Delivery != requestDeliveredOrAmbiguous || trace[0].Decision != routeRetrySuppressedDelivery {
		t.Fatalf("route trace = %+v, want one ambiguous terminal primary attempt", trace)
	}
}

func TestRouteInferenceTransportPrewriteFailureBudgetsSecondaryDispatch(t *testing.T) {
	var dispatches routeReplayDispatchLog
	transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		dispatches.record(req)
		switch req.URL.Hostname() {
		case "primary.example":
			// No write-related httptrace callback: the failure is definitely
			// prewrite and may safely admit the next configured target.
			return nil, errors.New("dial tcp: injected prewrite failure")
		case "secondary.example":
			return routeExecutorTestResponse(req, http.StatusOK, nil, `{"id":"secondary","status":"completed","output":[]}`), nil
		default:
			return nil, fmt.Errorf("unexpected route host %q", req.URL.Host)
		}
	})

	h, route := explicitRouteTestHandler(t, &http.Client{Transport: transport}, routeModePriorityFailover, 2, 2,
		explicitRouteTestProvider("primary", "https://primary.example", "one"),
		explicitRouteTestProvider("secondary", "https://secondary.example", "two"),
	)
	operation := newRouteOperation(route, context.Background())
	ctx := withRouteOperation(context.Background(), operation)
	resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses,
		[]byte(`{"model":"public-model","input":"hello"}`), nil, "public-model", false)
	if err != nil {
		t.Fatalf("executeExplicitRouteRequest() error = %v", err)
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("response = %#v, want secondary 200", resp)
	}
	_ = resp.Body.Close()

	requireBudgetedRouteDispatches(t, &dispatches, "primary.example", "secondary.example")
	sends, switches, trace := operation.snapshot()
	if sends != 2 || switches != 1 {
		t.Fatalf("route budget = sends:%d switches:%d, want 2/1", sends, switches)
	}
	if len(trace) != 2 {
		t.Fatalf("route trace = %+v, want primary failure plus secondary success", trace)
	}
	if trace[0].TargetID != "target-primary" || trace[0].Delivery != requestDefinitelyNotDelivered || trace[0].Decision != routeRetrySwitchTarget {
		t.Fatalf("primary trace = %+v, want definitely-prewrite target switch", trace[0])
	}
	if trace[1].TargetID != "target-secondary" || trace[1].StatusCode != http.StatusOK || trace[1].Decision != routeRetryAccepted {
		t.Fatalf("secondary trace = %+v, want accepted success", trace[1])
	}
}
