package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"hash"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/logger"
)

const (
	testToolOptimizerMaxProviderCalls  = 8
	testToolOptimizerExternalCallLimit = 4
	testOversizedOptimizerBodyBytes    = 16 << 20
)

type reliabilityToolOptimizer struct {
	delay   time.Duration
	release <-chan struct{}

	calls  atomic.Int32
	active atomic.Int32
	peak   atomic.Int32
}

func (o *reliabilityToolOptimizer) ID() string { return "reliability" }

func (o *reliabilityToolOptimizer) RewriteCommand(ctx context.Context, _ ToolCommandRewriteRequest) (ToolCommandRewriteResult, error) {
	o.enter()
	defer o.leave()
	if err := o.wait(ctx); err != nil {
		return ToolCommandRewriteResult{}, err
	}
	return ToolCommandRewriteResult{}, nil
}

func (o *reliabilityToolOptimizer) ReduceOutput(ctx context.Context, _ ToolOutputReduceRequest) (ToolOutputReduceResult, error) {
	o.enter()
	defer o.leave()
	if err := o.wait(ctx); err != nil {
		return ToolOutputReduceResult{}, err
	}
	return ToolOutputReduceResult{}, nil
}

func (*reliabilityToolOptimizer) optimizerUsesExternalProcess() {}

func (o *reliabilityToolOptimizer) enter() {
	o.calls.Add(1)
	active := o.active.Add(1)
	for {
		peak := o.peak.Load()
		if active <= peak || o.peak.CompareAndSwap(peak, active) {
			return
		}
	}
}

func (o *reliabilityToolOptimizer) leave() {
	o.active.Add(-1)
}

func (o *reliabilityToolOptimizer) wait(ctx context.Context) error {
	if o.release != nil {
		select {
		case <-o.release:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if o.delay <= 0 {
		return nil
	}
	timer := time.NewTimer(o.delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func TestToolOptimizerTurnUsesOneStageDeadlineAcrossSixItems(t *testing.T) {
	optimizer := &reliabilityToolOptimizer{delay: 40 * time.Millisecond}
	handler := &ProxyHandler{toolOptimizers: reliabilityOutputManager(optimizer, 90)}
	body := reliabilityResponsesTurnBody(t, 6)

	started := time.Now()
	rewritten, changed := handler.maybeReduceResponsesToolOutputsInRequestBody(context.Background(), body, nil, "")
	elapsed := time.Since(started)

	t.Logf("six 40ms stalls: elapsed=%v provider_calls=%d", elapsed, optimizer.calls.Load())
	if changed != 0 || string(rewritten) != string(body) {
		t.Fatalf("timed-out optimizer changed payload: count=%d body=%s", changed, rewritten)
	}
	if elapsed < 65*time.Millisecond || elapsed > 190*time.Millisecond {
		t.Fatalf("six 40ms stalls completed in %v, want one roughly 90ms stage budget", elapsed)
	}
	if calls := optimizer.calls.Load(); calls < 2 || calls > 3 {
		t.Fatalf("provider calls = %d, want only calls fitting in one stage budget", calls)
	}
}

func TestToolOptimizerTurnStopsAtProviderCallLimit(t *testing.T) {
	optimizer := &reliabilityToolOptimizer{}
	handler := &ProxyHandler{toolOptimizers: reliabilityOutputManager(optimizer, 500)}
	body := reliabilityResponsesTurnBody(t, testToolOptimizerMaxProviderCalls+4)

	rewritten, changed := handler.maybeReduceResponsesToolOutputsInRequestBody(context.Background(), body, nil, "")
	if changed != 0 || string(rewritten) != string(body) {
		t.Fatalf("call-limited optimizer changed payload: count=%d body=%s", changed, rewritten)
	}
	if calls := int(optimizer.calls.Load()); calls != testToolOptimizerMaxProviderCalls {
		t.Fatalf("provider calls = %d, want cutoff at %d", calls, testToolOptimizerMaxProviderCalls)
	}
}

func TestToolOptimizerManagerBoundsConcurrentExternalCalls(t *testing.T) {
	release := make(chan struct{})
	optimizer := &reliabilityToolOptimizer{release: release}
	manager := reliabilityOutputManager(optimizer, 2000)

	const callers = 64
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(callers)
	for i := 0; i < callers; i++ {
		go func(i int) {
			defer wg.Done()
			<-start
			manager.ReduceOutput(context.Background(), ToolOutputReduceRequest{
				CallID:  fmt.Sprintf("call-%d", i),
				Command: "cat big.log",
				Output:  "large output",
			})
		}(i)
	}
	close(start)
	waitForAtomicAtLeast(t, &optimizer.active, testToolOptimizerExternalCallLimit, time.Second)
	time.Sleep(50 * time.Millisecond)
	peakBeforeRelease := optimizer.peak.Load()
	close(release)
	wg.Wait()

	t.Logf("64 concurrent requests: peak external calls=%d", peakBeforeRelease)
	if peakBeforeRelease > testToolOptimizerExternalCallLimit {
		t.Fatalf("concurrent provider calls peaked at %d, want at most %d", peakBeforeRelease, testToolOptimizerExternalCallLimit)
	}
	if peak := optimizer.peak.Load(); peak > testToolOptimizerExternalCallLimit {
		t.Fatalf("overall concurrent provider calls peaked at %d, want at most %d", peak, testToolOptimizerExternalCallLimit)
	}
	if calls := optimizer.calls.Load(); calls != callers {
		t.Fatalf("provider calls = %d, want %d", calls, callers)
	}
}

func TestToolOptimizerSemaphoreWithoutDeadlineFailsOpenWhenSaturated(t *testing.T) {
	release := make(chan struct{})
	optimizer := &reliabilityToolOptimizer{release: release}
	manager := NewToolOptimizerManager(ToolOptimizersConfig{
		Enabled: true,
		OutputReduce: ToolOptimizerOutputConfig{
			Enabled:       true,
			TimeoutMS:     0,
			MinInputBytes: 1,
			timeoutMSSet:  true,
		},
	}, []stagedToolOptimizer{{optimizer: optimizer, outputReduce: true}})

	var holders sync.WaitGroup
	holders.Add(testToolOptimizerExternalCallLimit)
	for i := 0; i < testToolOptimizerExternalCallLimit; i++ {
		go func(i int) {
			defer holders.Done()
			manager.ReduceOutput(context.Background(), ToolOutputReduceRequest{
				CallID:  fmt.Sprintf("no-deadline-holder-%d", i),
				Command: "cat big.log",
				Output:  "large output",
			})
		}(i)
	}
	waitForAtomicAtLeast(t, &optimizer.active, testToolOptimizerExternalCallLimit, time.Second)

	started := time.Now()
	result := manager.ReduceOutput(context.Background(), ToolOutputReduceRequest{
		CallID:  "saturated-no-deadline",
		Command: "cat big.log",
		Output:  "large output",
	})
	elapsed := time.Since(started)
	callsWhileSaturated := optimizer.calls.Load()

	close(release)
	holders.Wait()

	if result.Changed {
		t.Fatalf("saturated no-deadline call changed output: %+v", result)
	}
	if elapsed > 50*time.Millisecond {
		t.Fatalf("saturated no-deadline call returned in %v, want immediate fail-open", elapsed)
	}
	if callsWhileSaturated != testToolOptimizerExternalCallLimit {
		t.Fatalf("provider calls while saturated = %d, want %d holders only", callsWhileSaturated, testToolOptimizerExternalCallLimit)
	}
}

func TestToolOptimizerSemaphoreWaitHonorsCancellation(t *testing.T) {
	release := make(chan struct{})
	optimizer := &reliabilityToolOptimizer{release: release}
	manager := reliabilityOutputManager(optimizer, 2000)

	var holders sync.WaitGroup
	holders.Add(testToolOptimizerExternalCallLimit)
	for i := 0; i < testToolOptimizerExternalCallLimit; i++ {
		go func(i int) {
			defer holders.Done()
			manager.ReduceOutput(context.Background(), ToolOutputReduceRequest{
				CallID:  fmt.Sprintf("holder-%d", i),
				Command: "cat big.log",
				Output:  "large output",
			})
		}(i)
	}
	waitForAtomicAtLeast(t, &optimizer.active, testToolOptimizerExternalCallLimit, time.Second)

	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	started := time.Now()
	result := manager.ReduceOutput(ctx, ToolOutputReduceRequest{
		CallID:  "canceled-waiter",
		Command: "cat big.log",
		Output:  "large output",
	})
	elapsed := time.Since(started)
	callsWhileSaturated := optimizer.calls.Load()

	close(release)
	holders.Wait()

	if result.Changed {
		t.Fatalf("canceled semaphore waiter changed output: %+v", result)
	}
	if elapsed < 25*time.Millisecond || elapsed > 200*time.Millisecond {
		t.Fatalf("canceled semaphore wait returned in %v, want context-bounded wait", elapsed)
	}
	if callsWhileSaturated != testToolOptimizerExternalCallLimit {
		t.Fatalf("provider calls while saturated = %d, want %d holders only", callsWhileSaturated, testToolOptimizerExternalCallLimit)
	}
}

type immediateTimeoutToolOptimizer struct {
	calls atomic.Int32
}

func (*immediateTimeoutToolOptimizer) ID() string { return "immediate-timeout" }
func (o *immediateTimeoutToolOptimizer) RewriteCommand(context.Context, ToolCommandRewriteRequest) (ToolCommandRewriteResult, error) {
	o.calls.Add(1)
	return ToolCommandRewriteResult{}, context.DeadlineExceeded
}
func (o *immediateTimeoutToolOptimizer) ReduceOutput(context.Context, ToolOutputReduceRequest) (ToolOutputReduceResult, error) {
	o.calls.Add(1)
	return ToolOutputReduceResult{}, context.DeadlineExceeded
}

func TestToolOptimizerTurnStopsTimedOutProviderForRemainingItems(t *testing.T) {
	timedOut := &immediateTimeoutToolOptimizer{}
	fallback := &reliabilityToolOptimizer{}
	manager := NewToolOptimizerManager(ToolOptimizersConfig{
		Enabled: true,
		OutputReduce: ToolOptimizerOutputConfig{
			Enabled:       true,
			TimeoutMS:     500,
			MinInputBytes: 1,
		},
	}, []stagedToolOptimizer{
		{optimizer: timedOut, outputReduce: true},
		{optimizer: fallback, outputReduce: true},
	})
	handler := &ProxyHandler{toolOptimizers: manager}
	body := reliabilityResponsesTurnBody(t, 3)

	rewritten, changed := handler.maybeReduceResponsesToolOutputsInRequestBody(context.Background(), body, nil, "")
	if changed != 0 || string(rewritten) != string(body) {
		t.Fatalf("timed-out provider changed payload: count=%d body=%s", changed, rewritten)
	}
	if calls := timedOut.calls.Load(); calls != 1 {
		t.Fatalf("timed-out provider calls = %d, want 1 for the turn", calls)
	}
	if calls := fallback.calls.Load(); calls != 3 {
		t.Fatalf("fallback provider calls = %d, want 3", calls)
	}
}

func TestShouldInspectNonStreamingResponsesRequiresEligibleProvider(t *testing.T) {
	cfg := ToolOptimizersConfig{
		Enabled:        true,
		CommandRewrite: ToolOptimizerRewriteConfig{Enabled: true},
		OutputReduce:   ToolOptimizerOutputConfig{Enabled: true},
	}
	optimizer := &reliabilityToolOptimizer{}
	tests := []struct {
		name      string
		providers []stagedToolOptimizer
		want      bool
	}{
		{name: "no providers", want: false},
		{name: "provider supports neither stage", providers: []stagedToolOptimizer{{optimizer: optimizer}}, want: false},
		{name: "command provider", providers: []stagedToolOptimizer{{optimizer: optimizer, commandRewrite: true}}, want: true},
		{name: "output provider", providers: []stagedToolOptimizer{{optimizer: optimizer, outputReduce: true}}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			manager := NewToolOptimizerManager(cfg, tt.providers)
			if got := manager.ShouldInspectNonStreamingResponses(); got != tt.want {
				t.Fatalf("ShouldInspectNonStreamingResponses() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestToolOptimizerOversizedResponsesBodyStreamsUnchangedWithinSniffCap(t *testing.T) {
	fake := &recordingToolOptimizer{}
	handler := &ProxyHandler{log: logger.New(logger.LevelInfo)}
	configureRecordingToolOptimizer(handler, fake)
	payload := newOptimizerLargePayload(t, "responses", testOversizedOptimizerBodyBytes)
	body := payload.newBody()
	writer := newOptimizerTrackingResponseWriter(body)
	resp := optimizerLargeHTTPResponse(body, payload.total)

	if err := handler.writeResponsesUpstreamResponse(context.Background(), writer, resp, handler.toolContexts, "session:oversized-responses"); err != nil {
		t.Fatalf("write oversized Responses response: %v", err)
	}
	assertOptimizerLargePassthrough(t, writer, payload)
	if calls := len(fake.snapshotRewriteRequests()); calls != 0 {
		t.Fatalf("oversized Responses body launched %d optimizer calls, want 0", calls)
	}
	if _, ok := handler.toolContexts.Get("session:oversized-responses", "call-large"); ok {
		t.Fatalf("oversized Responses body was inspected instead of streamed")
	}
}

func TestToolOptimizerOversizedChatBodyStreamsUnchangedWithinSniffCap(t *testing.T) {
	fake := &recordingToolOptimizer{}
	handler := &ProxyHandler{log: logger.New(logger.LevelInfo)}
	configureRecordingToolOptimizer(handler, fake)
	payload := newOptimizerLargePayload(t, "chat", testOversizedOptimizerBodyBytes)
	body := payload.newBody()
	writer := newOptimizerTrackingResponseWriter(body)
	resp := optimizerLargeHTTPResponse(body, payload.total)

	if err := handler.maybeWriteOptimizedOpenAIChatPassthrough(context.Background(), writer, resp, "gpt-test", handler.toolContexts, "session:oversized-chat"); err != nil {
		t.Fatalf("write oversized chat response: %v", err)
	}
	assertOptimizerLargePassthrough(t, writer, payload)
	if calls := len(fake.snapshotRewriteRequests()); calls != 0 {
		t.Fatalf("oversized Chat body launched %d optimizer calls, want 0", calls)
	}
	if _, ok := handler.toolContexts.Get("session:oversized-chat", "call-large"); ok {
		t.Fatalf("oversized Chat body was inspected instead of streamed")
	}
}

func BenchmarkToolOptimizerOversizedResponsesPassthrough(b *testing.B) {
	benchmarkOptimizerOversizedPassthrough(b, "responses")
}

func BenchmarkToolOptimizerOversizedChatPassthrough(b *testing.B) {
	benchmarkOptimizerOversizedPassthrough(b, "chat")
}

func benchmarkOptimizerOversizedPassthrough(b *testing.B, surface string) {
	cfg := ToolOptimizersConfig{
		Enabled:        true,
		CommandRewrite: ToolOptimizerRewriteConfig{Enabled: true},
	}
	handler := &ProxyHandler{
		log: logger.New(logger.LevelInfo),
		toolOptimizers: NewToolOptimizerManager(cfg, []stagedToolOptimizer{{
			optimizer:      noopToolOptimizer{id: "benchmark"},
			commandRewrite: true,
		}}),
		toolContexts: NewToolExecutionContextStore(),
	}
	payload := newOptimizerLargePayload(b, surface, testOversizedOptimizerBodyBytes)
	b.ReportAllocs()
	b.SetBytes(payload.total)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		body := payload.newBody()
		writer := &optimizerDiscardResponseWriter{header: make(http.Header)}
		resp := optimizerLargeHTTPResponse(body, payload.total)
		if surface == "responses" {
			if err := handler.writeResponsesUpstreamResponse(context.Background(), writer, resp, nil, ""); err != nil {
				b.Fatalf("write oversized Responses response: %v", err)
			}
		} else if err := handler.maybeWriteOptimizedOpenAIChatPassthrough(context.Background(), writer, resp, "gpt-test", nil, ""); err != nil {
			b.Fatalf("write oversized chat response: %v", err)
		}
	}
}

func reliabilityOutputManager(optimizer ToolOptimizer, timeoutMS int) *ToolOptimizerManager {
	return NewToolOptimizerManager(ToolOptimizersConfig{
		Enabled: true,
		OutputReduce: ToolOptimizerOutputConfig{
			Enabled:       true,
			TimeoutMS:     timeoutMS,
			MinInputBytes: 1,
		},
	}, []stagedToolOptimizer{{optimizer: optimizer, outputReduce: true}})
}

func reliabilityResponsesTurnBody(t *testing.T, outputCount int) []byte {
	t.Helper()
	items := make([]any, 0, outputCount*2)
	for i := 0; i < outputCount; i++ {
		callID := fmt.Sprintf("call-%d", i)
		arguments, err := json.Marshal(map[string]string{"command": "go test ./..."})
		if err != nil {
			t.Fatalf("marshal command arguments: %v", err)
		}
		items = append(items,
			map[string]any{
				"type":      "function_call",
				"name":      "shell_command",
				"call_id":   callID,
				"arguments": string(arguments),
			},
			map[string]any{
				"type":    "function_call_output",
				"call_id": callID,
				"output":  "large output",
			},
		)
	}
	body, err := json.Marshal(map[string]any{"model": "gpt-test", "input": items})
	if err != nil {
		t.Fatalf("marshal Responses turn: %v", err)
	}
	return body
}

func waitForAtomicAtLeast(t *testing.T, value *atomic.Int32, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for int(value.Load()) < want {
		if time.Now().After(deadline) {
			t.Fatalf("value = %d, did not reach %d within %v", value.Load(), want, timeout)
		}
		time.Sleep(time.Millisecond)
	}
}

type optimizerLargePayload struct {
	prefix   string
	padding  int64
	suffix   string
	total    int64
	expected [sha256.Size]byte
}

func newOptimizerLargePayload(tb testing.TB, surface string, total int64) optimizerLargePayload {
	tb.Helper()
	arguments, err := json.Marshal(`{"command":"go test ./..."}`)
	if err != nil {
		tb.Fatalf("marshal large payload arguments: %v", err)
	}
	var prefix string
	switch surface {
	case "responses":
		prefix = `{"id":"resp-large","object":"response","status":"completed","output":[{"type":"function_call","name":"shell_command","call_id":"call-large","arguments":` + string(arguments) + `}],"padding":"`
	case "chat":
		prefix = `{"id":"chat-large","object":"chat.completion","created":1,"model":"gpt-test","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call-large","type":"function","function":{"name":"shell_command","arguments":` + string(arguments) + `}}]},"finish_reason":"tool_calls"}],"padding":"`
	default:
		tb.Fatalf("unknown optimizer large payload surface %q", surface)
	}
	suffix := `"}`
	padding := total - int64(len(prefix)+len(suffix))
	if padding <= 0 {
		tb.Fatalf("large payload total %d is too small", total)
	}
	payload := optimizerLargePayload{prefix: prefix, padding: padding, suffix: suffix, total: total}
	h := sha256.New()
	_, _ = io.Copy(h, payload.newReader())
	copy(payload.expected[:], h.Sum(nil))
	return payload
}

func (p optimizerLargePayload) newReader() io.Reader {
	return io.MultiReader(
		strings.NewReader(p.prefix),
		&optimizerRepeatedByteReader{remaining: p.padding, value: 'x'},
		strings.NewReader(p.suffix),
	)
}

func (p optimizerLargePayload) newBody() *optimizerTrackingReadCloser {
	return &optimizerTrackingReadCloser{reader: p.newReader()}
}

type optimizerRepeatedByteReader struct {
	remaining int64
	value     byte
}

func (r *optimizerRepeatedByteReader) Read(p []byte) (int, error) {
	if r.remaining <= 0 {
		return 0, io.EOF
	}
	if int64(len(p)) > r.remaining {
		p = p[:r.remaining]
	}
	for i := range p {
		p[i] = r.value
	}
	r.remaining -= int64(len(p))
	return len(p), nil
}

type optimizerTrackingReadCloser struct {
	reader    io.Reader
	bytesRead int64
}

func (r *optimizerTrackingReadCloser) Read(p []byte) (int, error) {
	n, err := r.reader.Read(p)
	r.bytesRead += int64(n)
	return n, err
}

func (*optimizerTrackingReadCloser) Close() error { return nil }

type optimizerTrackingResponseWriter struct {
	header         http.Header
	status         int
	source         *optimizerTrackingReadCloser
	firstWriteRead int64
	wrote          bool
	bytesWritten   int64
	hash           hash.Hash
}

func newOptimizerTrackingResponseWriter(source *optimizerTrackingReadCloser) *optimizerTrackingResponseWriter {
	return &optimizerTrackingResponseWriter{
		header: make(http.Header),
		source: source,
		hash:   sha256.New(),
	}
}

func (w *optimizerTrackingResponseWriter) Header() http.Header { return w.header }

func (w *optimizerTrackingResponseWriter) WriteHeader(status int) { w.status = status }

func (w *optimizerTrackingResponseWriter) Write(p []byte) (int, error) {
	if !w.wrote {
		w.wrote = true
		w.firstWriteRead = w.source.bytesRead
	}
	w.bytesWritten += int64(len(p))
	_, _ = w.hash.Write(p)
	return len(p), nil
}

type optimizerDiscardResponseWriter struct {
	header http.Header
	status int
}

func (w *optimizerDiscardResponseWriter) Header() http.Header { return w.header }
func (w *optimizerDiscardResponseWriter) WriteHeader(status int) {
	w.status = status
}
func (*optimizerDiscardResponseWriter) Write(p []byte) (int, error) { return len(p), nil }

func optimizerLargeHTTPResponse(body io.ReadCloser, total int64) *http.Response {
	header := make(http.Header)
	header.Set("Content-Type", "application/json")
	header.Set("Content-Length", strconv.FormatInt(total, 10))
	return &http.Response{
		StatusCode:    http.StatusOK,
		Header:        header,
		Body:          body,
		ContentLength: total,
	}
}

func assertOptimizerLargePassthrough(t *testing.T, writer *optimizerTrackingResponseWriter, payload optimizerLargePayload) {
	t.Helper()
	if writer.status != http.StatusOK {
		t.Fatalf("status = %d, want 200", writer.status)
	}
	if writer.bytesWritten != payload.total {
		t.Fatalf("bytes written = %d, want %d", writer.bytesWritten, payload.total)
	}
	if got := writer.header.Get("Content-Length"); got != strconv.FormatInt(payload.total, 10) {
		t.Fatalf("Content-Length = %q, want %d", got, payload.total)
	}
	t.Logf("oversized body: total=%d first_write_after_read=%d", payload.total, writer.firstWriteRead)
	if writer.firstWriteRead > usageSniffMaxBuffer+1 {
		t.Fatalf("first downstream write happened after reading %d bytes, want at most %d", writer.firstWriteRead, usageSniffMaxBuffer+1)
	}
	if got := writer.hash.Sum(nil); !strings.EqualFold(fmt.Sprintf("%x", got), fmt.Sprintf("%x", payload.expected)) {
		t.Fatalf("oversized body hash = %x, want %x", got, payload.expected)
	}
}
