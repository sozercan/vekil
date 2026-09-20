package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
)

func TestConversationMigrationReappliesToolOutputOptimizer(t *testing.T) {
	for _, protocol := range []string{"http", "sse", "websocket"} {
		t.Run(protocol, func(t *testing.T) {
			originalOutput := strings.Repeat("original tool output ", 64)
			var east, west atomic.Int32
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
				}
				if strings.HasPrefix(req.URL.Host, "east.") && east.Add(1) > 1 {
					return nil, errors.New("prewrite outage")
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				req.Body = io.NopCloser(bytes.NewReader(body))
				if bytes.Contains(body, []byte(originalOutput)) || !bytes.Contains(body, []byte("reduced output")) {
					t.Errorf("%s did not receive the optimized tool result: %s", req.URL.Host, body)
				}
				id := "optimizer-east"
				if strings.HasPrefix(req.URL.Host, "west.") {
					west.Add(1)
					id = "optimizer-west"
				}
				return conversationResponse(t, req, id, conversationText("Saved answer.")), nil
			})
			h, _ := newConversationAPIHandler(t, transport, nil)
			optimizer := &recordingToolOptimizer{}
			configureRecordingToolOptimizer(h, optimizer)
			conversationCompleted(t, conversationPOST(t, h, map[string]any{
				"input": []any{
					map[string]any{"role": "user", "content": "Read the command output."},
					map[string]any{"type": "function_call", "call_id": "optimizer-call", "name": "shell_command", "arguments": `{"command":"cat big.log"}`},
					map[string]any{"type": "function_call_output", "call_id": "optimizer-call", "output": originalOutput},
				},
			}, http.Header{"X-Vekil-History-Complete": {"true"}}), false)
			fields := map[string]any{"previous_response_id": "optimizer-east", "input": "Continue after the outage."}
			if protocol == "websocket" {
				conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
				defer func() { _ = conn.Close() }()
				conversationWebSocketTurn(t, conn, fields)
			} else {
				fields["stream"] = protocol == "sse"
				conversationCompleted(t, conversationPOST(t, h, fields, nil), protocol == "sse")
			}
			if east.Load() != 2 || west.Load() != 1 || len(optimizer.snapshotReduceRequests()) != 2 {
				t.Fatalf("unexpected sends or optimizer calls: east=%d west=%d optimizer=%d", east.Load(), west.Load(), len(optimizer.snapshotReduceRequests()))
			}
			for _, id := range []string{"optimizer-east", "optimizer-west"} {
				snapshot, err := h.conversationHistory.lookupResponse("azure", id)
				if err != nil {
					t.Fatal(err)
				}
				history, err := json.Marshal(snapshot.Input)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Contains(history, []byte(originalOutput)) {
					t.Fatalf("optimization replaced the original saved history for %s", id)
				}
			}
		})
	}
}
