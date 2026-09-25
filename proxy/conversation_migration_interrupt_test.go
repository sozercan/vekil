package proxy

import (
	"bytes"
	"context"
	"encoding/json"
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

// conversationSignalWriter records a streamed response and signals once the
// client has received marker, so a test can interrupt after exposure.
type conversationSignalWriter struct {
	mu      sync.Mutex
	code    int
	header  http.Header
	body    bytes.Buffer
	marker  []byte
	matched chan struct{}
	once    sync.Once
}

func newConversationSignalWriter(marker string) *conversationSignalWriter {
	return &conversationSignalWriter{header: make(http.Header), marker: []byte(marker), matched: make(chan struct{})}
}

func (w *conversationSignalWriter) Header() http.Header { return w.header }

func (w *conversationSignalWriter) WriteHeader(code int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.code == 0 {
		w.code = code
	}
}

func (w *conversationSignalWriter) Flush() {}

func (w *conversationSignalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.body.Write(p)
	if bytes.Contains(w.body.Bytes(), w.marker) {
		w.once.Do(func() { close(w.matched) })
	}
	return len(p), nil
}

// conversationInterruptedPOST streams a turn and disconnects the client once
// marker has been delivered, or once ready closes when marker is empty, like a
// user interrupting an agent mid-turn.
func conversationInterruptedPOST(t *testing.T, h *ProxyHandler, fields map[string]any, headers http.Header, marker string, ready <-chan struct{}) int {
	t.Helper()
	fields["model"] = "coding"
	fields["stream"] = true
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	req := httptest.NewRequestWithContext(ctx, http.MethodPost, "/v1/responses", bytes.NewReader(body))
	req.Header = headers.Clone()
	req.Header.Set("Content-Type", "application/json")
	writer := newConversationSignalWriter(marker)
	done := make(chan struct{})
	go func() {
		defer close(done)
		h.HandleResponses(writer, req)
	}()
	if ready == nil {
		ready = writer.matched
	}
	select {
	case <-ready:
	case <-time.After(5 * time.Second):
		t.Fatalf("interrupted turn never delivered %q", marker)
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("interrupted turn did not stop")
	}
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return writer.code
}

// conversationLogBuffer collects handler logs written from request goroutines.
type conversationLogBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *conversationLogBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *conversationLogBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func conversationOutputItemDone(t *testing.T, index int, item map[string]any) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"type": "response.output_item.done", "output_index": index, "item": item})
	if err != nil {
		t.Fatal(err)
	}
	return "event: response.output_item.done\ndata: " + string(encoded) + "\n\n"
}

func TestConversationMigrationClientInterruptKeepsConversationUsable(t *testing.T) {
	seedReasoning := map[string]any{"type": "reasoning", "encrypted_content": "seed-encrypted", "summary": []any{}}
	interruptedReasoning := map[string]any{"type": "reasoning", "encrypted_content": "interrupted-encrypted", "summary": []any{}}
	interruptedCall := map[string]any{"type": "function_call", "call_id": "call-interrupted", "name": "edit", "arguments": "{}"}
	abortedResult := map[string]any{"type": "function_call_output", "call_id": "call-interrupted", "output": "aborted"}
	history := []any{
		map[string]any{"role": "user", "content": "Seed."}, seedReasoning, conversationText("Known earlier answer."),
		map[string]any{"role": "user", "content": "Next."},
	}
	for _, scenario := range []string{"during precommit hold", "before output", "full history after items", "response ID after items", "altered items"} {
		t.Run(scenario, func(t *testing.T) {
			var sends, west atomic.Int32
			var resumedBody atomic.Value
			resumedBody.Store("")
			held := make(chan struct{})
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
				}
				if strings.HasPrefix(req.URL.Host, "west.") {
					west.Add(1)
				}
				switch sends.Add(1) {
				case 1:
					return conversationResponse(t, req, "seed", seedReasoning, conversationText("Known earlier answer.")), nil
				case 2:
					if scenario == "during precommit hold" {
						// Headers arrived, but nothing has been committed downstream.
						close(held)
						return conversationStreamResponse(req, nil, time.Minute), nil
					}
					parts := []any{conversationLifecycleEvent(t, "response.created", "in_progress", 0)}
					if scenario != "before output" {
						parts = append(parts,
							conversationOutputItemDone(t, 0, map[string]any{"id": "rs-interrupted", "type": "reasoning", "encrypted_content": "interrupted-encrypted", "summary": []any{}}),
							conversationOutputItemDone(t, 1, map[string]any{"id": "msg-interrupted", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Partial answer."}}}),
							conversationOutputItemDone(t, 2, map[string]any{"id": "fc-interrupted", "type": "function_call", "status": "completed", "call_id": "call-interrupted", "name": "edit", "arguments": "{}"}),
							// An unfinished message is visible as deltas only; clients
							// do not keep it, so it is not part of saved history.
							"data: "+`{"type":"response.output_text.delta","item_id":"msg-unfinished","output_index":3,"content_index":0,"delta":"Unfinished"}`+"\n\n")
					}
					return conversationStreamResponse(req, nil, append(parts, time.Minute)...), nil
				}
				body, _ := io.ReadAll(req.Body)
				resumedBody.Store(string(body))
				req.Body = io.NopCloser(bytes.NewReader(body))
				return conversationResponse(t, req, "resumed", conversationText("Resumed.")), nil
			})
			logs := &conversationLogBuffer{}
			h, _ := newConversationAPIHandler(t, transport, nil, func(h *ProxyHandler) {
				h.log = logger.NewWithWriter(logger.LevelInfo, logs)
			})
			session := http.Header{"Session_id": {"client-a"}}
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, session), false)

			marker, ready := "Unfinished", (<-chan struct{})(nil)
			switch scenario {
			case "during precommit hold":
				marker, ready = "", held
			case "before output":
				marker = "response.created"
			}
			code := conversationInterruptedPOST(t, h, map[string]any{"previous_response_id": "seed", "input": "Next."}, session, marker, ready)
			if sends.Load() != 2 {
				t.Fatalf("interrupted turn sends = %d", sends.Load())
			}
			// The client's interrupt is neither an uncertain execution nor an
			// upstream failure.
			if recovery := logs.String(); !strings.Contains(recovery, `"outcome":"interrupted"`) || strings.Contains(recovery, `"outcome":"blocked"`) || strings.Contains(recovery, "upstream request failed") {
				t.Fatalf("interrupt recovery logs: %s", recovery)
			}
			if scenario == "during precommit hold" && code != 499 {
				t.Fatalf("interrupt before commitment recorded status %d, want 499", code)
			}

			var resumed *httptest.ResponseRecorder
			switch scenario {
			case "during precommit hold", "before output":
				resumed = conversationPOST(t, h, map[string]any{"input": append(append([]any{}, history...), map[string]any{"role": "user", "content": "After interrupt."})}, session)
			case "response ID after items":
				resumed = conversationPOST(t, h, map[string]any{"previous_response_id": conversationStreamedResponseID, "input": []any{abortedResult, map[string]any{"role": "user", "content": "After interrupt."}}}, session)
			case "full history after items", "altered items":
				text := "Partial answer."
				if scenario == "altered items" {
					text = "A different answer."
				}
				input := append(append([]any{}, history...), interruptedReasoning, conversationText(text), interruptedCall, abortedResult, map[string]any{"role": "user", "content": "After interrupt."})
				resumed = conversationPOST(t, h, map[string]any{"input": input}, session)
			}
			if scenario == "altered items" {
				// Only items Vekil delivered from the owner extend the history.
				if resumed.Code != http.StatusBadRequest || sends.Load() != 2 || !strings.Contains(resumed.Body.String(), "conversation_history_incomplete") {
					t.Fatalf("altered interrupted history accepted: %d %s", resumed.Code, resumed.Body.String())
				}
				return
			}
			conversationCompleted(t, resumed, false)
			if sends.Load() != 3 || west.Load() != 0 {
				t.Fatalf("resume was not one owner send: sends=%d west=%d", sends.Load(), west.Load())
			}
			body := resumedBody.Load().(string)
			if scenario != "during precommit hold" && scenario != "before output" && (!strings.Contains(body, "Partial answer.") || !strings.Contains(body, "aborted") || strings.Contains(body, "Unfinished")) {
				t.Fatalf("resumed history does not match the delivered items: %s", body)
			}
		})
	}
}
