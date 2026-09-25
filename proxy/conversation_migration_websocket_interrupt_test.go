package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/logger"
)

// conversationWebSocketResume sends a continuation on a fresh connection,
// retrying while the interrupted turn still holds the conversation admission.
func conversationWebSocketResume(t *testing.T, server *httptest.Server, headers http.Header, fields map[string]any) map[string]any {
	t.Helper()
	fields["type"], fields["model"], fields["store"] = "response.create", "coding", false
	deadline := time.Now().Add(5 * time.Second)
	for {
		conn := mustDialResponsesWebSocket(t, server, headers)
		if err := conn.WriteJSON(fields); err != nil {
			t.Fatal(err)
		}
		frame := mustReadWebSocketJSONSkipMetadata(t, conn)
		for frame["type"] == "response.created" {
			frame = mustReadWebSocketJSONSkipMetadata(t, conn)
		}
		_ = conn.Close()
		encoded, _ := json.Marshal(frame)
		if frame["type"] == "error" && bytes.Contains(encoded, []byte("conversation_turn_in_progress")) && time.Now().Before(deadline) {
			time.Sleep(20 * time.Millisecond)
			continue
		}
		if frame["type"] != "response.completed" {
			t.Fatalf("resumed websocket turn failed: %s", encoded)
		}
		return frame["response"].(map[string]any)
	}
}

func TestConversationMigrationWebSocketClientInterruptKeepsConversationUsable(t *testing.T) {
	seedReasoning := map[string]any{"type": "reasoning", "encrypted_content": "seed-encrypted", "summary": []any{}}
	history := []any{
		map[string]any{"role": "user", "content": "Seed."}, seedReasoning, conversationText("Known earlier answer."),
		map[string]any{"role": "user", "content": "Next."},
	}
	afterInterrupt := map[string]any{"role": "user", "content": "After interrupt."}
	for _, scenario := range []string{"during precommit hold", "before output", "after items"} {
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
					var parts []any
					switch scenario {
					case "during precommit hold":
						close(held)
					case "before output":
						parts = []any{conversationLifecycleEvent(t, "response.created", "in_progress", 0)}
					default:
						parts = []any{
							conversationLifecycleEvent(t, "response.created", "in_progress", 0),
							conversationOutputItemDone(t, 0, map[string]any{"id": "rs-interrupted", "type": "reasoning", "encrypted_content": "interrupted-encrypted", "summary": []any{}}),
							conversationOutputItemDone(t, 1, map[string]any{"id": "msg-interrupted", "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": "Partial answer."}}}),
							conversationOutputItemDone(t, 2, map[string]any{"id": "fc-interrupted", "type": "function_call", "status": "completed", "call_id": "call-interrupted", "name": "edit", "arguments": "{}"}),
							conversationInterruptEvent(t, "response.output_text.delta", map[string]any{"item_id": "msg-unfinished", "output_index": 3, "content_index": 0, "delta": "Unfinished"}),
							conversationInterruptEvent(t, "response.output_text.delta", map[string]any{"item_id": "msg-unfinished", "output_index": 3, "content_index": 0, "delta": " message"}),
						}
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
			server := startResponsesWebSocketProxyServer(t, h)
			session := http.Header{"Session_id": {"ws-client"}}
			conn := mustDialResponsesWebSocket(t, server, session)
			conversationWebSocketTurn(t, conn, map[string]any{"input": []any{map[string]any{"role": "user", "content": "Seed."}}})

			// Interrupt the next turn by dropping the socket once the scenario's
			// point has been reached.
			if err := conn.WriteJSON(map[string]any{"type": "response.create", "model": "coding", "store": false, "previous_response_id": "seed", "input": []any{map[string]any{"role": "user", "content": "Next."}}}); err != nil {
				t.Fatal(err)
			}
			switch scenario {
			case "during precommit hold":
				select {
				case <-held:
				case <-time.After(5 * time.Second):
					t.Fatal("interrupted turn never dispatched")
				}
			default:
				marker := " message"
				if scenario == "before output" {
					marker = "response.created"
				}
				for {
					frame := mustReadWebSocketJSONSkipMetadata(t, conn, 5*time.Second)
					encoded, _ := json.Marshal(frame)
					if frame["type"] == "error" || frame["type"] == "response.completed" {
						t.Fatalf("interrupted turn ended before the interrupt: %s", encoded)
					}
					if bytes.Contains(encoded, []byte(marker)) {
						break
					}
				}
			}
			_ = conn.Close()

			var resumed map[string]any
			if scenario == "after items" {
				// A full-history client keeps the completed items it received and
				// returns an aborted result for the interrupted tool call.
				input := append(append([]any{}, history...),
					map[string]any{"type": "reasoning", "encrypted_content": "interrupted-encrypted", "summary": []any{}},
					conversationText("Partial answer."),
					map[string]any{"type": "function_call", "call_id": "call-interrupted", "name": "edit", "arguments": "{}"},
					map[string]any{"type": "function_call_output", "call_id": "call-interrupted", "output": "aborted"},
					afterInterrupt)
				resumed = conversationWebSocketResume(t, server, session, map[string]any{"input": input})
			} else {
				resumed = conversationWebSocketResume(t, server, session, map[string]any{"previous_response_id": "seed", "input": []any{afterInterrupt}})
			}
			if resumed["id"] != "resumed" || sends.Load() != 3 || west.Load() != 0 {
				t.Fatalf("resume was not one owner send: id=%v sends=%d west=%d", resumed["id"], sends.Load(), west.Load())
			}
			if recovery := logs.String(); !strings.Contains(recovery, `"outcome":"interrupted"`) || strings.Contains(recovery, `"outcome":"blocked"`) {
				t.Fatalf("interrupt recovery logs: %s", recovery)
			}
			body := resumedBody.Load().(string)
			if scenario == "after items" && (!strings.Contains(body, "Partial answer.") || !strings.Contains(body, "aborted") || strings.Contains(body, "Unfinished")) {
				t.Fatalf("resumed history does not match the delivered items: %s", body)
			}
		})
	}
}
