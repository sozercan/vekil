package proxy

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/logger"
)

func newResponsesRouteStreamingHandler(t *testing.T, primaryURL, secondaryURL string) *ProxyHandler {
	t.Helper()
	h, _ := explicitRouteTestHandler(t, http.DefaultClient, routeModePriorityFailover, 2, 2,
		explicitRouteTestProvider("primary", primaryURL, "primary-key"),
		explicitRouteTestProvider("secondary", secondaryURL, "secondary-key"),
	)
	h.log = logger.New(logger.LevelFatal)
	t.Cleanup(h.BeginShutdown)
	return h
}

func responsesRouteSSE(event, data string) string {
	return "event: " + event + "\ndata: " + data + "\n\n"
}

func TestHandleResponsesExplicitRouteStreamingSuccessNormalizesAndBindsState(t *testing.T) {
	var primaryCalls atomic.Int32
	var secondaryCalls atomic.Int32

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := primaryCalls.Add(1)
		if call != 1 {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = io.WriteString(w, `{"error":{"message":"pinned primary quota","type":"rate_limit_error","code":"rate_limit_exceeded"}}`)
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("OpenAI-Model", "deployment-a")
		w.Header().Set("X-OpenAI-Model", "deployment-a")
		w.Header().Set("X-Codex-Turn-State", "turn-state-primary")
		_, _ = io.WriteString(w,
			responsesRouteSSE("response.created", `{"type":"response.created","response":{"id":"resp-primary","model":"deployment-a","status":"in_progress","output":[]}}`)+
				responsesRouteSSE("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg-primary","output_index":0,"content_index":0,"delta":"hello"}`)+
				responsesRouteSSE("response.output_item.done", `{"type":"response.output_item.done","output_index":1,"item":{"id":"reasoning-primary","type":"reasoning","encrypted_content":"encrypted-primary","summary":[]}}`)+
				responsesRouteSSE("response.completed", `{"type":"response.completed","response":{"id":"resp-primary","model":"deployment-a","status":"completed","output":[{"id":"reasoning-primary","type":"reasoning","encrypted_content":"encrypted-primary","summary":[]}],"usage":{"input_tokens":3,"output_tokens":2,"total_tokens":5}}}`),
		)
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"unexpected-secondary","model":"deployment-b","status":"completed","output":[]}`)
	}))
	defer secondary.Close()

	h := newResponsesRouteStreamingHandler(t, primary.URL, secondary.URL)
	requestCtx, summary := WithRequestSummary(context.Background())
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-model","input":"hello","stream":true}`)).WithContext(requestCtx)
	w := httptest.NewRecorder()
	h.HandleResponses(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if strings.Contains(body, "deployment-a") || !strings.Contains(body, `"model":"public-model"`) {
		t.Fatalf("stream did not normalize the public model: %s", body)
	}
	for _, fragment := range []string{"resp-primary", "encrypted-primary", `"delta":"hello"`} {
		if !strings.Contains(body, fragment) {
			t.Fatalf("stream missing %q: %s", fragment, body)
		}
	}
	if got := w.Header().Get("OpenAI-Model"); got != "public-model" {
		t.Fatalf("OpenAI-Model = %q, want public-model", got)
	}
	if got := w.Header().Get("X-OpenAI-Model"); got != "public-model" {
		t.Fatalf("X-OpenAI-Model = %q, want public-model", got)
	}
	if got := w.Header().Get("X-Codex-Turn-State"); got != "turn-state-primary" {
		t.Fatalf("X-Codex-Turn-State = %q, want turn-state-primary", got)
	}
	if got := summary.FinalTarget(); got != "target-primary" {
		t.Fatalf("final target = %q, want target-primary", got)
	}
	if got := summary.UpstreamSendCount(); got != 1 {
		t.Fatalf("upstream sends = %d, want 1", got)
	}

	followups := []struct {
		name    string
		body    string
		headers http.Header
	}{
		{
			name: "response id",
			body: `{"model":"public-model","previous_response_id":"resp-primary","input":"continue"}`,
		},
		{
			name: "encrypted content",
			body: `{"model":"public-model","input":[{"type":"reasoning","encrypted_content":"encrypted-primary"},{"type":"message","role":"user","content":"continue"}]}`,
		},
		{
			name:    "turn state header",
			body:    `{"model":"public-model","input":"continue"}`,
			headers: http.Header{"X-Codex-Turn-State": []string{"turn-state-primary"}},
		},
	}
	for _, tt := range followups {
		t.Run(tt.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(tt.body))
			req.Header = tt.headers.Clone()
			w := httptest.NewRecorder()
			h.HandleResponses(w, req)
			if w.Code != http.StatusTooManyRequests {
				t.Fatalf("status = %d body=%s, want pinned primary 429", w.Code, w.Body.String())
			}
		})
	}

	if got := primaryCalls.Load(); got != int32(1+len(followups)) {
		t.Fatalf("primary calls = %d, want %d", got, 1+len(followups))
	}
	if got := secondaryCalls.Load(); got != 0 {
		t.Fatalf("secondary calls = %d, want 0 for bound state", got)
	}
}

func TestHandleResponsesExplicitRouteStreamingPrecommitHandoff(t *testing.T) {
	tests := []struct {
		name               string
		primaryStream      string
		wantPrimaryBody    string
		wantSecondaryBody  string
		wantSecondaryCalls int32
		wantFinalTarget    string
		wantSwitches       int64
	}{
		{
			name: "certified precommit failure switches targets",
			primaryStream: responsesRouteSSE("response.created", `{"type":"response.created","response":{"id":"resp-primary-hidden","model":"deployment-a","status":"in_progress","output":[]}}`) +
				responsesRouteSSE("response.failed", `{"type":"response.failed","response":{"id":"resp-primary-hidden","status":"failed","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"primary quota"}}}`),
			wantSecondaryBody:  "resp-secondary-visible",
			wantSecondaryCalls: 1,
			wantFinalTarget:    "target-secondary",
			wantSwitches:       1,
		},
		{
			name: "malformed terminal event fails open on primary",
			primaryStream: responsesRouteSSE("response.created", `{"type":"response.created","response":{"id":"resp-primary-malformed","model":"deployment-a","status":"in_progress","output":[]}}`) +
				responsesRouteSSE("response.failed", `{not-json}`),
			wantPrimaryBody:    `{not-json}`,
			wantSecondaryCalls: 0,
			wantFinalTarget:    "target-primary",
			wantSwitches:       0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var primaryCalls atomic.Int32
			var secondaryCalls atomic.Int32

			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("OpenAI-Model", "deployment-a")
				_, _ = io.WriteString(w, tt.primaryStream)
			}))
			defer primary.Close()

			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondaryCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				w.Header().Set("OpenAI-Model", "deployment-b")
				_, _ = io.WriteString(w,
					responsesRouteSSE("response.created", `{"type":"response.created","response":{"id":"resp-secondary-visible","model":"deployment-b","status":"in_progress","output":[]}}`)+
						responsesRouteSSE("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg-secondary","output_index":0,"content_index":0,"delta":"secondary"}`)+
						responsesRouteSSE("response.completed", `{"type":"response.completed","response":{"id":"resp-secondary-visible","model":"deployment-b","status":"completed","output":[]}}`),
				)
			}))
			defer secondary.Close()

			h := newResponsesRouteStreamingHandler(t, primary.URL, secondary.URL)
			requestCtx, summary := WithRequestSummary(context.Background())
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-model","input":"hello","stream":true}`)).WithContext(requestCtx)
			w := httptest.NewRecorder()
			h.HandleResponses(w, req)

			if w.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s, want 200", w.Code, w.Body.String())
			}
			body := w.Body.String()
			if tt.wantPrimaryBody != "" && !strings.Contains(body, tt.wantPrimaryBody) {
				t.Fatalf("body = %s, want primary fragment %q", body, tt.wantPrimaryBody)
			}
			if tt.wantSecondaryBody != "" && !strings.Contains(body, tt.wantSecondaryBody) {
				t.Fatalf("body = %s, want secondary fragment %q", body, tt.wantSecondaryBody)
			}
			if tt.wantSecondaryCalls > 0 && strings.Contains(body, "resp-primary-hidden") {
				t.Fatalf("failed primary preamble leaked after failover: %s", body)
			}
			if strings.Contains(body, "deployment-a") || strings.Contains(body, "deployment-b") {
				t.Fatalf("physical model leaked to client: %s", body)
			}
			if got := primaryCalls.Load(); got != 1 {
				t.Fatalf("primary calls = %d, want 1", got)
			}
			if got := secondaryCalls.Load(); got != tt.wantSecondaryCalls {
				t.Fatalf("secondary calls = %d, want %d", got, tt.wantSecondaryCalls)
			}
			if got := summary.FinalTarget(); got != tt.wantFinalTarget {
				t.Fatalf("final target = %q, want %q", got, tt.wantFinalTarget)
			}
			if got := summary.TargetSwitchCount(); got != tt.wantSwitches {
				t.Fatalf("target switches = %d, want %d", got, tt.wantSwitches)
			}
		})
	}
}

type responsesRouteCommitWriter struct {
	header http.Header

	mu        sync.Mutex
	status    int
	body      bytes.Buffer
	commit    sync.Once
	committed chan struct{}
}

func newResponsesRouteCommitWriter() *responsesRouteCommitWriter {
	return &responsesRouteCommitWriter{
		header:    make(http.Header),
		committed: make(chan struct{}),
	}
}

func (w *responsesRouteCommitWriter) Header() http.Header { return w.header }

func (w *responsesRouteCommitWriter) WriteHeader(status int) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = status
	}
	w.mu.Unlock()
	w.commit.Do(func() { close(w.committed) })
}

func (w *responsesRouteCommitWriter) Write(p []byte) (int, error) {
	if w.statusCode() == 0 {
		w.WriteHeader(http.StatusOK)
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.Write(p)
}

func (w *responsesRouteCommitWriter) Flush() {}

func (w *responsesRouteCommitWriter) statusCode() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.status
}

func (w *responsesRouteCommitWriter) bodyString() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.body.String()
}

func TestHandleResponsesExplicitRouteStreamingDoesNotSwitchAfterDownstreamCommit(t *testing.T) {
	tests := []struct {
		name             string
		lateStream       string
		wantLateFragment string
	}{
		{
			name:             "classified failure after output",
			lateStream:       responsesRouteSSE("response.failed", `{"type":"response.failed","response":{"id":"resp-primary-committed","model":"deployment-a","status":"failed","error":{"type":"rate_limit_error","code":"rate_limit_exceeded","message":"late quota"}}}`),
			wantLateFragment: "late quota",
		},
		{
			name:             "malformed failure after output",
			lateStream:       responsesRouteSSE("response.failed", `{late-not-json}`),
			wantLateFragment: `{late-not-json}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var primaryCalls atomic.Int32
			var secondaryCalls atomic.Int32
			releaseLate := make(chan struct{})
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(releaseLate) }) }

			primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				primaryCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w,
					responsesRouteSSE("response.created", `{"type":"response.created","response":{"id":"resp-primary-committed","model":"deployment-a","status":"in_progress","output":[]}}`)+
						responsesRouteSSE("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg-primary","output_index":0,"content_index":0,"delta":"visible output"}`),
				)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				<-releaseLate
				_, _ = io.WriteString(w, tt.lateStream)
			}))
			defer primary.Close()

			secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				secondaryCalls.Add(1)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, responsesRouteSSE("response.completed", `{"type":"response.completed","response":{"id":"unexpected-secondary","model":"deployment-b","status":"completed","output":[]}}`))
			}))
			defer secondary.Close()
			defer release()

			h := newResponsesRouteStreamingHandler(t, primary.URL, secondary.URL)
			requestCtx, summary := WithRequestSummary(context.Background())
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"public-model","input":"hello","stream":true}`)).WithContext(requestCtx)
			writer := newResponsesRouteCommitWriter()
			done := make(chan struct{})
			go func() {
				defer close(done)
				h.HandleResponses(writer, req)
			}()

			select {
			case <-writer.committed:
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for downstream commitment")
			}
			if got := secondaryCalls.Load(); got != 0 {
				t.Fatalf("secondary calls before late primary event = %d, want 0", got)
			}

			release()
			select {
			case <-done:
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for committed stream handoff")
			}

			if got := writer.statusCode(); got != http.StatusOK {
				t.Fatalf("status = %d, want 200", got)
			}
			body := writer.bodyString()
			for _, fragment := range []string{`"delta":"visible output"`, tt.wantLateFragment} {
				if !strings.Contains(body, fragment) {
					t.Fatalf("body missing %q: %s", fragment, body)
				}
			}
			if strings.Contains(body, "deployment-a") || strings.Contains(body, "unexpected-secondary") {
				t.Fatalf("committed stream leaked a physical or secondary model: %s", body)
			}
			if got := primaryCalls.Load(); got != 1 {
				t.Fatalf("primary calls = %d, want 1", got)
			}
			if got := secondaryCalls.Load(); got != 0 {
				t.Fatalf("secondary calls after downstream commit = %d, want 0", got)
			}
			if got := summary.FinalTarget(); got != "target-primary" {
				t.Fatalf("final target = %q, want target-primary", got)
			}
			if got := summary.TargetSwitchCount(); got != 0 {
				t.Fatalf("target switches = %d, want 0", got)
			}
		})
	}
}

type responsesRouteTrackingBody struct {
	reader *strings.Reader
	closed atomic.Bool
}

func newResponsesRouteTrackingBody(body string) *responsesRouteTrackingBody {
	return &responsesRouteTrackingBody{reader: strings.NewReader(body)}
}

func (b *responsesRouteTrackingBody) Read(p []byte) (int, error) { return b.reader.Read(p) }

func (b *responsesRouteTrackingBody) Close() error {
	b.closed.Store(true)
	return nil
}

type responsesRouteFailWriter struct {
	*responsesRouteCommitWriter
	failWrites   atomic.Bool
	failedWrites atomic.Int32
	bodyOnce     sync.Once
	bodyWritten  chan struct{}
}

func newResponsesRouteFailWriter() *responsesRouteFailWriter {
	return &responsesRouteFailWriter{
		responsesRouteCommitWriter: newResponsesRouteCommitWriter(),
		bodyWritten:                make(chan struct{}),
	}
}

func (w *responsesRouteFailWriter) Write(p []byte) (int, error) {
	if w.failWrites.Load() {
		w.failedWrites.Add(1)
		return 0, io.ErrClosedPipe
	}
	n, err := w.responsesRouteCommitWriter.Write(p)
	if n > 0 {
		w.bodyOnce.Do(func() { close(w.bodyWritten) })
	}
	return n, err
}

func TestHandleResponsesExplicitRouteStreamingClientCancellationClosesBodies(t *testing.T) {
	var primaryCalls atomic.Int32
	var secondaryCalls atomic.Int32
	releaseAfterCancel := make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(releaseAfterCancel) }) }
	upstreamCanceled := make(chan struct{})
	var upstreamCanceledOnce sync.Once
	forceUpstreamReturn := make(chan struct{})
	var forceUpstreamReturnOnce sync.Once
	forceReturn := func() { forceUpstreamReturnOnce.Do(func() { close(forceUpstreamReturn) }) }

	primary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		primaryCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w,
			responsesRouteSSE("response.created", `{"type":"response.created","response":{"id":"resp-cancel-primary","model":"deployment-a","status":"in_progress","output":[]}}`)+
				responsesRouteSSE("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg-cancel-primary","output_index":0,"content_index":0,"delta":"before cancel"}`),
		)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}

		<-releaseAfterCancel
		_, _ = io.WriteString(w, responsesRouteSSE("response.output_text.delta", `{"type":"response.output_text.delta","item_id":"msg-cancel-primary","output_index":0,"content_index":0,"delta":"after cancel"}`))
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			upstreamCanceledOnce.Do(func() { close(upstreamCanceled) })
		case <-forceUpstreamReturn:
		}
	}))
	defer primary.Close()

	secondary := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secondaryCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, responsesRouteSSE("response.completed", `{"type":"response.completed","response":{"id":"unexpected-secondary","model":"deployment-b","status":"completed","output":[]}}`))
	}))
	defer secondary.Close()

	h := newResponsesRouteStreamingHandler(t, primary.URL, secondary.URL)
	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	defer forceReturn()
	defer release()
	requestBody := newResponsesRouteTrackingBody(`{"model":"public-model","input":"hello","stream":true}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(clientCtx)
	req.Body = requestBody
	req.ContentLength = int64(requestBody.reader.Len())
	writer := newResponsesRouteFailWriter()
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleResponses(writer, req)
	}()

	select {
	case <-writer.bodyWritten:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for committed response body")
	}
	writer.failWrites.Store(true)
	cancelClient()
	release()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for handler cleanup after client cancellation")
	}
	select {
	case <-upstreamCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream response body cancellation")
	}

	if !requestBody.closed.Load() {
		t.Fatal("inbound request body was not closed")
	}
	if got := writer.failedWrites.Load(); got == 0 {
		t.Fatal("simulated disconnected client did not reject a stream write")
	}
	if got := primaryCalls.Load(); got != 1 {
		t.Fatalf("primary calls = %d, want 1", got)
	}
	if got := secondaryCalls.Load(); got != 0 {
		t.Fatalf("secondary calls after client cancellation = %d, want 0", got)
	}
}

type responsesRouteCloseBody struct {
	started chan struct{}
	release <-chan struct{}
	done    chan struct{}
	calls   atomic.Int32
	start   sync.Once
	finish  sync.Once
}

func newResponsesRouteCloseBody(release <-chan struct{}) *responsesRouteCloseBody {
	return &responsesRouteCloseBody{
		started: make(chan struct{}),
		release: release,
		done:    make(chan struct{}),
	}
}

func (b *responsesRouteCloseBody) Read([]byte) (int, error) { return 0, io.EOF }

func (b *responsesRouteCloseBody) Close() error {
	b.calls.Add(1)
	b.start.Do(func() { close(b.started) })
	if b.release != nil {
		<-b.release
	}
	b.finish.Do(func() { close(b.done) })
	return nil
}

func TestCloseResponseBodyWithTimeout(t *testing.T) {
	tests := []struct {
		name    string
		nilBody bool
		block   bool
		timeout time.Duration
		want    bool
	}{
		{name: "nil body", nilBody: true, timeout: time.Second, want: true},
		{name: "immediate close", timeout: time.Second, want: true},
		{name: "immediate close without timeout", timeout: 0, want: true},
		{name: "blocked close times out", block: true, timeout: 50 * time.Millisecond, want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var body io.ReadCloser
			var tracked *responsesRouteCloseBody
			var release chan struct{}
			if !tt.nilBody {
				if tt.block {
					release = make(chan struct{})
					defer func() {
						if release != nil {
							close(release)
						}
					}()
				}
				tracked = newResponsesRouteCloseBody(release)
				body = tracked
			}

			got := closeResponseBodyWithTimeout(body, tt.timeout)
			if got != tt.want {
				t.Fatalf("closeResponseBodyWithTimeout() = %v, want %v", got, tt.want)
			}
			if tracked == nil {
				return
			}

			select {
			case <-tracked.started:
			case <-time.After(time.Second):
				t.Fatal("body Close was not called")
			}
			if release != nil {
				close(release)
				release = nil
			}
			select {
			case <-tracked.done:
			case <-time.After(time.Second):
				t.Fatal("body Close did not finish after release")
			}
			if got := tracked.calls.Load(); got != 1 {
				t.Fatalf("Close calls = %d, want 1", got)
			}
		})
	}
}
