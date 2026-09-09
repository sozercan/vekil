package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

type taskInferenceKind uint8

const (
	taskInference taskInferenceKind = iota
	taskTokenCount
	taskCompaction
	taskMemory
	taskClassifier
	taskInsight
	taskInferenceKinds
)

var taskInferenceKindNames = [taskInferenceKinds]string{
	"inference", "token_count", "compaction", "memory", "classifier", "insight",
}

type taskInferenceKindContextKey struct{}
type taskInferenceEndpointContextKey struct{}

func withTaskInferenceRequest(req *http.Request, endpoint string) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), taskInferenceEndpointContextKey{}, endpoint))
}

func withTaskInferenceKind(ctx context.Context, kind taskInferenceKind) context.Context {
	return context.WithValue(ctx, taskInferenceKindContextKey{}, kind)
}

func copyTaskInferenceContext(dst, src context.Context) context.Context {
	if src != nil {
		if kind, ok := src.Value(taskInferenceKindContextKey{}).(taskInferenceKind); ok {
			return withTaskInferenceKind(dst, kind)
		}
	}
	return dst
}

func taskInferenceKindFromRequest(req *http.Request) taskInferenceKind {
	if req == nil {
		return taskInference
	}
	if taskUsageEndpoint(req) == providerEndpointMessagesCount {
		return taskTokenCount
	}
	if kind, ok := req.Context().Value(taskInferenceKindContextKey{}).(taskInferenceKind); ok && kind < taskInferenceKinds {
		return kind
	}
	return taskInference
}

// Task usage is an independent physical-send ledger. It includes auxiliary
// requests and retries without changing client-request or explicit-route stats.
type taskUsageTotals struct {
	Sends              int64              `json:"sends"`
	Completed          int64              `json:"completed"`
	Errors             int64              `json:"errors"`
	Throttled          int64              `json:"throttled"`
	ReportedUsageSends int64              `json:"reported_usage_sends"`
	DurationMS         int64              `json:"duration_ms"`
	Usage              statsTokenUsage    `json:"usage"`
	CopilotUsage       copilotUsageTotals `json:"copilot_usage"`
}

type taskUsageRow struct {
	Kind string `json:"kind"`
	taskUsageTotals
}

type taskUsageSnapshot struct {
	Inflight int64           `json:"inflight"`
	Totals   taskUsageTotals `json:"totals"`
	ByKind   []taskUsageRow  `json:"by_kind"`
}

type taskUsageCollector struct {
	mu       sync.Mutex
	totals   taskUsageTotals
	kinds    [taskInferenceKinds]taskUsageTotals
	inflight int64
}

func (c *taskUsageCollector) snapshot() taskUsageSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	result := taskUsageSnapshot{Inflight: c.inflight, Totals: c.totals, ByKind: make([]taskUsageRow, 0, taskInferenceKinds)}
	for kind, row := range c.kinds {
		if row.Sends > 0 {
			result.ByKind = append(result.ByKind, taskUsageRow{Kind: taskInferenceKindNames[kind], taskUsageTotals: row})
		}
	}
	return result
}

func taskUsageEndpoint(req *http.Request) string {
	if req != nil {
		if endpoint, ok := req.Context().Value(taskInferenceEndpointContextKey{}).(string); ok {
			return endpoint
		}
	}
	if req == nil || req.URL == nil {
		return providerEndpointChatCompletions
	}
	switch {
	case strings.HasSuffix(req.URL.Path, providerEndpointResponses):
		return providerEndpointResponses
	case strings.HasSuffix(req.URL.Path, providerEndpointMessagesCount):
		return providerEndpointMessagesCount
	case strings.HasSuffix(req.URL.Path, providerEndpointMessages):
		return providerEndpointMessages
	default:
		return providerEndpointChatCompletions
	}
}

// Begin immediately before the actual transport call, after local admission.
// finish attaches bounded response observation after header decompression.
func (h *ProxyHandler) beginTaskInferenceSend(req *http.Request) *taskInferenceSend {
	if h == nil || h.stats == nil {
		return nil
	}
	kind := taskInferenceKindFromRequest(req)
	collector := &h.stats.taskUsage
	collector.mu.Lock()
	collector.totals.Sends++
	collector.kinds[kind].Sends++
	collector.inflight++
	collector.mu.Unlock()
	return &taskInferenceSend{req: req, state: &taskUsageBody{collector: collector, kind: kind, start: time.Now()}}
}

type taskInferenceSend struct {
	req   *http.Request
	state *taskUsageBody
	once  sync.Once
}

func (s *taskInferenceSend) finish(resp *http.Response, sendErr error) {
	if s != nil {
		s.once.Do(func() { s.finishResponse(resp, sendErr) })
	}
}

func (s *taskInferenceSend) finishResponse(resp *http.Response, sendErr error) {
	state, req := s.state, s.req
	if sendErr != nil || resp == nil {
		state.publish(statsTokenUsage{}, false, copilotUsageTotals{}, true, false)
		return
	}
	state.failed = resp.StatusCode >= http.StatusBadRequest
	state.throttled = resp.StatusCode == http.StatusTooManyRequests
	endpoint := taskUsageEndpoint(req)
	state.nativeCount = endpoint == providerEndpointMessagesCount
	if state.nativeCount {
		endpoint = providerEndpointMessages
	}
	state.observer = newRouteAttemptResponseObserver(nil, nil, routeAttemptTrace{StatusCode: resp.StatusCode}, nil,
		endpoint, strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "text/event-stream"), nil)
	state.observer.captureCopilotUsage = true
	// A finished generation may stop at its output limit without failing the
	// physical send. Route attempts retain their stricter completion contract.
	state.observer.acceptIncompleteResponses = true
	// Chat's [DONE] sentinel does not finish a Responses send.
	state.observer.requireResponsesTerminal = true
	state.observer.requireMessageStop = true
	if !state.observer.streaming && state.observer.envelope != nil {
		state.observer.envelope.fields = append(state.observer.envelope.fields, routeAttemptJSONField{name: "copilot_usage", maxBytes: 64 << 10})
		if state.nativeCount {
			state.observer.envelope.fields = append(state.observer.envelope.fields, routeAttemptJSONField{name: "input_tokens", maxBytes: 32})
		}
	}
	if resp.Body == nil || resp.Body == http.NoBody {
		state.publish(statsTokenUsage{}, false, copilotUsageTotals{}, true, state.throttled)
		return
	}
	state.inner = resp.Body
	resp.Body = state
}

type taskUsageBody struct {
	inner            io.ReadCloser
	observer         *routeAttemptResponseObserver
	collector        *taskUsageCollector
	kind             taskInferenceKind
	start            time.Time
	failed           bool
	throttled        bool
	nativeCount      bool
	closeOnce        sync.Once
	closeErr         error
	mu               sync.Mutex
	activeReads      int
	closing          bool
	completed        bool
	reported         bool
	errorCounted     bool
	throttleCounted  bool
	accounted        statsTokenUsage
	copilotAccounted copilotUsageTotals
}

func (b *taskUsageBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	b.activeReads++
	b.mu.Unlock()
	// Preserve the underlying Read/Close contract. A synthetic early read
	// error could signal cleanup completion while the transport is still open.
	n, err := b.inner.Read(p)
	if n > 0 {
		b.observer.observe(p[:n])
	}
	b.mu.Lock()
	b.activeReads--
	finish := b.activeReads == 0 && (err != nil || b.closing)
	b.mu.Unlock()
	if finish {
		finishErr := err
		if finishErr == nil {
			finishErr = errRouteAttemptBodyClosedEarly
		}
		b.observer.finish(errors.Is(finishErr, io.EOF), finishErr)
		b.publishObserver()
	}
	return n, err
}

func (b *taskUsageBody) Close() error {
	b.closeOnce.Do(func() {
		b.mu.Lock()
		b.closing = true
		b.mu.Unlock()
		b.closeErr = b.inner.Close()
		b.mu.Lock()
		finish := !b.completed && b.activeReads == 0
		b.mu.Unlock()
		if finish {
			b.observer.finish(false, errRouteAttemptBodyClosedEarly)
			b.publishObserver()
		}
	})
	return b.closeErr
}

func (b *taskUsageBody) publishObserver() {
	o := b.observer
	o.mu.Lock()
	usage, haveUsage := o.usage, o.haveUsage
	// A provider cancellation terminal completes a successful HTTP exchange.
	// Cancellation before a terminal still indicates an interrupted send.
	terminalCancellation := o.terminal && o.outcome == routeAttemptOutcomeCanceled
	failed := b.failed || (!terminalCancellation && o.outcome != routeAttemptOutcomeSucceeded && o.outcome != routeAttemptOutcomeInFlight)
	throttled := b.throttled || o.statusCode == http.StatusTooManyRequests
	copilot := o.copilotUsage
	if !o.streaming {
		if raw, ok := o.envelope.field("copilot_usage"); ok {
			copilot, _ = parseCopilotUsage(raw)
		}
	}
	if b.nativeCount {
		// Sizing is not inference usage. A valid count has no Messages
		// terminal envelope and must not be classified as a failed inference.
		usage, haveUsage = statsTokenUsage{}, false
		var count *int64
		raw, present := o.envelope.field("input_tokens")
		valid := !o.streaming && o.envelope.complete() && present && json.Unmarshal(raw, &count) == nil && count != nil && *count >= 0
		failed = b.failed || !valid
	}
	o.mu.Unlock()
	b.publish(usage, haveUsage, copilot, failed, throttled)
}

func (b *taskUsageBody) canceledAtFailure() bool {
	if observed, ok := b.inner.(interface{ canceledAtFailure() bool }); ok {
		return observed.canceledAtFailure()
	}
	return false
}

func (b *taskUsageBody) cancelRouteAttempt() {
	cancelRouteAttemptBody(b.inner)
}

func (b *taskUsageBody) routeAttemptTransportOwnership() *routeAttemptTransportOwner {
	return routeAttemptTransportOwnership(b.inner)
}

func (b *taskUsageBody) publish(usage statsTokenUsage, haveUsage bool, copilot copilotUsageTotals, failed, throttled bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	delta := statsTokenUsageDelta(usage, b.accounted)
	b.accounted.add(delta)
	copilotDelta := copilotUsageTotals{
		TotalNanoAIU: max(copilot.TotalNanoAIU-b.copilotAccounted.TotalNanoAIU, 0),
		ComputeUnits: max(copilot.ComputeUnits-b.copilotAccounted.ComputeUnits, 0),
	}
	b.copilotAccounted.add(copilotDelta)
	c := b.collector
	c.mu.Lock()
	defer c.mu.Unlock()
	var durationMS int64
	if !b.completed {
		durationMS = max(time.Since(b.start).Milliseconds(), 0)
	}
	for _, row := range []*taskUsageTotals{&c.totals, &c.kinds[b.kind]} {
		row.Usage.add(delta)
		row.CopilotUsage.add(copilotDelta)
		if haveUsage && !b.reported {
			row.ReportedUsageSends++
		}
		if failed && !b.errorCounted {
			row.Errors++
		}
		if throttled && !b.throttleCounted {
			row.Throttled++
		}
		if !b.completed {
			row.Completed++
			row.DurationMS = policyStatsSaturatingAdd(row.DurationMS, durationMS)
		}
	}
	if !b.completed {
		c.inflight--
	}
	b.reported = b.reported || haveUsage
	b.errorCounted = b.errorCounted || failed
	b.throttleCounted = b.throttleCounted || throttled
	b.completed = true
}
