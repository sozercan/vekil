package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

const (
	benchmarkPreparedCreated = "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-benchmark\",\"status\":\"in_progress\"}}\n\n"
	benchmarkPreparedDelta   = "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"response_id\":\"resp-benchmark\",\"output_index\":0,\"content_index\":0,\"delta\":\"x\"}\n\n"
)

var benchmarkRouteExecutorByteSink byte

// BenchmarkExplicitRoutePreparedStreamTTFT measures the explicit-route
// precommit path through the point where the committed prepared body hands its
// first byte to the downstream reader. The standard ns/op includes cleanup;
// ttft-ns/op stops at the first downstream byte.
func BenchmarkExplicitRoutePreparedStreamTTFT(b *testing.B) {
	first, _, err := benchmarkPreparedStreamFirstByte()
	if err != nil {
		b.Fatalf("validate prepared stream handoff: %v", err)
	}
	if first != benchmarkPreparedCreated[0] {
		b.Fatalf("prepared stream first byte = %q, want %q", first, benchmarkPreparedCreated[0])
	}

	b.ReportAllocs()
	b.SetBytes(int64(len(benchmarkPreparedCreated) + len(benchmarkPreparedDelta)))
	var totalTTFT time.Duration
	var ttft time.Duration
	for b.Loop() {
		first, ttft, err = benchmarkPreparedStreamFirstByte()
		if err != nil {
			b.Fatal(err)
		}
		benchmarkRouteExecutorByteSink = first
		totalTTFT += ttft
	}
	b.ReportMetric(float64(totalTTFT.Nanoseconds())/float64(b.N), "ttft-ns/op")
}

func benchmarkPreparedStreamFirstByte() (byte, time.Duration, error) {
	started := time.Now()
	prepared := newResponsesPreparedStreamWithPolicy(&http.Response{
		StatusCode: http.StatusOK,
		Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
		Body:       newSplitChunkEOFReadCloser(benchmarkPreparedCreated, benchmarkPreparedDelta),
	}, responsesPrecommitMaxPeekBytes, true, true)

	result, hasResult, _, err := prepared.await(context.Background(), context.Background(), time.Second)
	if err != nil {
		prepared.abort()
		<-prepared.doneCh
		return 0, 0, fmt.Errorf("await precommit decision: %w", err)
	}
	if !hasResult || result.decision != responsesPeekDecisionPassthrough {
		prepared.abort()
		<-prepared.doneCh
		return 0, 0, fmt.Errorf("precommit decision = %+v, present=%t", result, hasResult)
	}

	resp := prepared.commitResponse()
	var first [1]byte
	if _, err := io.ReadFull(resp.Body, first[:]); err != nil {
		_ = resp.Body.Close()
		<-prepared.doneCh
		return 0, 0, fmt.Errorf("read first committed byte: %w", err)
	}
	ttft := time.Since(started)
	if err := resp.Body.Close(); err != nil {
		<-prepared.doneCh
		return 0, 0, fmt.Errorf("close prepared body: %w", err)
	}
	<-prepared.doneCh
	return first[0], ttft, nil
}

// BenchmarkRouteAttemptStatsConcurrentContention models concurrent successful
// failovers against the one process-wide stats collector. Each benchmark op is
// exactly two physical attempts and one target switch; assertions stay outside
// the timed parallel loop.
func BenchmarkRouteAttemptStatsConcurrentContention(b *testing.B) {
	h := &ProxyHandler{stats: newStatsCollector()}
	ctx := context.Background()
	primary := RouteAttemptObservation{
		OperationID:  "op-benchmark",
		RouteID:      "route-benchmark",
		TargetID:     "target-primary",
		ProviderID:   "provider-primary",
		ProviderKind: "azure-openai",
	}
	secondary := RouteAttemptObservation{
		OperationID:  "op-benchmark",
		RouteID:      "route-benchmark",
		TargetID:     "target-secondary",
		ProviderID:   "provider-secondary",
		ProviderKind: "openai-compatible",
	}

	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			h.RecordUpstreamAttempt(ctx, primary)
			h.RecordTargetSwitch(ctx)
			h.RecordUpstreamAttempt(ctx, secondary)
		}
	})
	b.StopTimer()

	snap := h.stats.snapshot()
	wantAttempts := int64(b.N) * 2
	wantSwitches := int64(b.N)
	if snap.UpstreamAttempts != wantAttempts || snap.TargetSwitches != wantSwitches {
		b.Fatalf("route stats attempts/switches = %d/%d, want %d/%d", snap.UpstreamAttempts, snap.TargetSwitches, wantAttempts, wantSwitches)
	}
	attemptsByTarget := make(map[string]int64, len(snap.ByTarget))
	for _, target := range snap.ByTarget {
		attemptsByTarget[target.Target] = target.Attempts
	}
	for _, targetID := range []string{primary.TargetID, secondary.TargetID} {
		if got := attemptsByTarget[targetID]; got != int64(b.N) {
			b.Fatalf("attempts for %s = %d, want %d", targetID, got, b.N)
		}
	}
	b.ReportMetric(2, "attempts/op")
	b.ReportMetric(1, "switches/op")
}

// BenchmarkExplicitRouteTwoTargetFailover64MiB exercises the worst-case request
// body size through an authoritative primary rejection and one secondary send.
// Run with -benchmem to capture allocation pressure; sends/op and the exact
// per-target/body-byte totals are checked after the timed loop.
func BenchmarkExplicitRouteTwoTargetFailover64MiB(b *testing.B) {
	const requestSize = 64 << 20
	requestBody := makeBenchmarkRouteRequestBody(requestSize)

	var primarySends atomic.Int64
	var secondarySends atomic.Int64
	var sentBytes atomic.Int64
	transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		n, err := io.Copy(io.Discard, req.Body)
		closeErr := req.Body.Close()
		if err != nil {
			return nil, fmt.Errorf("read benchmark request body: %w", err)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close benchmark request body: %w", closeErr)
		}
		sentBytes.Add(n)

		switch req.URL.Hostname() {
		case "primary.example":
			primarySends.Add(1)
			return routeExecutorTestResponse(req, http.StatusTooManyRequests, nil, `{"error":{"code":"rate_limit_exceeded","message":"benchmark failover"}}`), nil
		case "secondary.example":
			secondarySends.Add(1)
			return routeExecutorTestResponse(req, http.StatusOK, nil, `{"id":"resp-benchmark","status":"completed","output":[]}`), nil
		default:
			return nil, fmt.Errorf("unexpected benchmark target %q", req.URL.Hostname())
		}
	})
	h, route := newTwoTargetRouteBenchmarkHandler(b, transport)

	b.ReportAllocs()
	b.SetBytes(int64(len(requestBody)))
	b.ResetTimer()
	for b.Loop() {
		operation := newRouteOperation(route, context.Background())
		ctx := withRouteOperation(context.Background(), operation)
		resp, err := h.executeExplicitRouteRequest(ctx, route, providerEndpointResponses, requestBody, nil, "public-model", false)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := io.Copy(io.Discard, resp.Body); err != nil {
			_ = resp.Body.Close()
			b.Fatal(err)
		}
		if err := resp.Body.Close(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()

	wantPerTarget := int64(b.N)
	if got := primarySends.Load(); got != wantPerTarget {
		b.Fatalf("primary sends = %d, want %d", got, wantPerTarget)
	}
	if got := secondarySends.Load(); got != wantPerTarget {
		b.Fatalf("secondary sends = %d, want %d", got, wantPerTarget)
	}
	wantBytes := int64(len(requestBody)) * 2 * int64(b.N)
	if got := sentBytes.Load(); got != wantBytes {
		b.Fatalf("upstream request bytes = %d, want %d", got, wantBytes)
	}
	b.ReportMetric(2, "sends/op")
	b.ReportMetric(float64(len(requestBody)*2), "wire-B/op")
}

func makeBenchmarkRouteRequestBody(size int) []byte {
	prefix := []byte(`{"model":"public-model","input":"`)
	suffix := []byte(`"}`)
	if size < len(prefix)+len(suffix) {
		panic("benchmark request size is too small")
	}
	body := make([]byte, size)
	n := copy(body, prefix)
	payloadEnd := len(body) - len(suffix)
	for i := n; i < payloadEnd; i++ {
		body[i] = 'x'
	}
	copy(body[payloadEnd:], suffix)
	return body
}

func newTwoTargetRouteBenchmarkHandler(tb testing.TB, transport http.RoundTripper) (*ProxyHandler, *modelRoute) {
	tb.Helper()
	primary := explicitRouteTestProvider("primary", "https://primary.example", "primary-key")
	secondary := explicitRouteTestProvider("secondary", "https://secondary.example", "secondary-key")
	targets := []targetBinding{
		{id: "target-primary", provider: primary, upstreamModel: "public-model"},
		{id: "target-secondary", provider: secondary, upstreamModel: "public-model"},
	}
	route := &modelRoute{
		public: publicModelContract{
			id:        "public-model",
			routeID:   "route-benchmark",
			endpoints: []string{providerEndpointResponses},
		},
		targets: targets,
		policy: routePolicy{
			mode:              routeModePriorityFailover,
			maxTargetAttempts: 2,
			maxUpstreamSends:  2,
		},
	}
	registry, err := newModelRouteRegistry([]*modelRoute{route})
	if err != nil {
		tb.Fatalf("create benchmark route registry: %v", err)
	}
	h := &ProxyHandler{
		client: &http.Client{Transport: transport},
		providersState: &providerSetup{
			providers: map[string]*providerRuntime{
				primary.id:   primary,
				secondary.id: secondary,
			},
			providerOrder:      []string{primary.id, secondary.id},
			defaultProviderID:  primary.id,
			routes:             registry,
			models:             map[string]providerModel{},
			hasConfiguredState: true,
		},
		streamingUpstreamTimeout: time.Second,
	}
	h.initializeLifecycle()
	tb.Cleanup(h.BeginShutdown)
	return h, route
}
