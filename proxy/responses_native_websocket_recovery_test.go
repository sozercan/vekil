package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

// recoveryTestUpstream is a fake Copilot upstream whose native connections can
// be closed after a given number of turns, and which serves HTTP /responses
// posts, including compaction summaries.
type recoveryTestUpstream struct {
	upgrades, posts atomic.Int32
	closeAfterTurns int32
	frames          chan recoveryFrame
	postBodies      chan []json.RawMessage
	closed          chan struct{}
}

type recoveryFrame struct {
	connection int32
	input      []json.RawMessage
	previousID string
}

func newRecoveryTestUpstream() *recoveryTestUpstream {
	return &recoveryTestUpstream{frames: make(chan recoveryFrame, 8), postBodies: make(chan []json.RawMessage, 8), closed: make(chan struct{}, 8)}
}

func (u *recoveryTestUpstream) handler(t *testing.T) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/responses" {
			turn := u.posts.Add(1)
			var body struct {
				Input  []json.RawMessage `json:"input"`
				Stream bool              `json:"stream"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			u.postBodies <- body.Input
			response := fmt.Sprintf(`{"id":"resp-http-%d","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"summary of earlier work"}]}]}`, turn)
			if !body.Stream {
				// Compaction summaries are non-streaming requests.
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, response)
				return
			}
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.completed\",\"response\":%s}\n\n", response)
			return
		}
		conn, err := responsesWebSocketUpgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		connection := u.upgrades.Add(1)
		defer func() { _ = conn.Close(); u.closed <- struct{}{} }()
		for turn := int32(1); ; turn++ {
			var create struct {
				Input              []json.RawMessage `json:"input"`
				PreviousResponseID string            `json:"previous_response_id"`
			}
			if conn.ReadJSON(&create) != nil {
				return
			}
			u.frames <- recoveryFrame{connection, create.Input, create.PreviousResponseID}
			if err := conn.WriteJSON(map[string]any{"type": "response.completed", "response": map[string]any{
				"id":     fmt.Sprintf("resp-native-%d-%d", connection, turn),
				"output": []any{map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]string{"type": "output_text", "text": fmt.Sprintf("answer %d", turn)}}}},
			}}); err != nil {
				return
			}
			if u.closeAfterTurns > 0 && turn >= u.closeAfterTurns {
				return
			}
		}
	}
}

func recoveryTurn(t *testing.T, conn interface {
	WriteJSON(any) error
}, read func() map[string]any, request map[string]any, input ...any) string {
	t.Helper()
	request["input"] = input
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	frame := read()
	if frame["type"] != "response.completed" {
		t.Fatalf("turn = %#v", frame)
	}
	id := websocketResponseID(t, frame)
	request["previous_response_id"] = id
	return id
}

// readRecoveryTerminal reads frames until a terminal one, skipping the
// lifecycle events that synthetic compaction responses stream first.
func readRecoveryTerminal(t *testing.T, conn *websocket.Conn) map[string]any {
	t.Helper()
	for {
		frame := mustReadWebSocketJSONSkipMetadata(t, conn)
		if typ, _ := frame["type"].(string); typ != "response.created" && typ != "response.output_item.done" && typ != "response.output_item.added" {
			return frame
		}
	}
}

func userItem(text string) map[string]string {
	return map[string]string{"role": "user", "content": text}
}

func TestResponsesNativeWebSocketCompactionTriggerUsesProxyHistory(t *testing.T) {
	upstream := newRecoveryTestUpstream()
	h := newTestProxyHandler(t, upstream.handler(t))
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true, DisableAutoCompact: true}
	advertiseNativeWebSocketForTest(h)
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	read := func() map[string]any { return readRecoveryTerminal(t, conn) }
	request := newResponsesWebSocketCreateRequest(nil)

	recoveryTurn(t, conn, read, request, userItem("first"))
	recoveryTurn(t, conn, read, request, userItem("second"))
	// A client compaction (Codex remote compaction) summarizes the proxy's full
	// history over HTTP instead of being rejected on the native connection.
	recoveryTurn(t, conn, read, request, map[string]string{"type": "compaction_trigger"})
	recoveryTurn(t, conn, read, request, userItem("after compaction"))

	if first, second := <-upstream.frames, <-upstream.frames; len(first.input) != 1 || first.previousID != "" || len(second.input) != 1 || second.previousID != "resp-native-1-1" {
		t.Fatalf("native turns before compaction = %+v then %+v", first, second)
	}
	summarized := <-upstream.postBodies
	encoded, _ := json.Marshal(summarized)
	if !strings.Contains(string(encoded), "first") || !strings.Contains(string(encoded), "answer 2") {
		t.Fatalf("compaction did not summarize the full history: %s", encoded)
	}
	// The turn after compaction replays the compacted history on the same
	// connection, starting a new upstream conversation.
	resumed := <-upstream.frames
	encoded, _ = json.Marshal(resumed.input)
	if resumed.connection != 1 || resumed.previousID != "" || !strings.Contains(string(encoded), "summary of earlier work") || !strings.Contains(string(encoded), "after compaction") || strings.Contains(string(encoded), `"first"`) {
		t.Fatalf("post-compaction native turn = %+v input=%s", resumed, encoded)
	}
	if upstream.upgrades.Load() != 1 || upstream.posts.Load() != 1 {
		t.Fatalf("transport: upgrades=%d posts=%d", upstream.upgrades.Load(), upstream.posts.Load())
	}
}

func TestResponsesNativeWebSocketReconnectsAfterUpstreamClose(t *testing.T) {
	upstream := newRecoveryTestUpstream()
	upstream.closeAfterTurns = 2
	h := newTestProxyHandler(t, upstream.handler(t))
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true, DisableAutoCompact: true}
	advertiseNativeWebSocketForTest(h)
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	read := func() map[string]any { return readRecoveryTerminal(t, conn) }
	request := newResponsesWebSocketCreateRequest(nil)

	recoveryTurn(t, conn, read, request, userItem("first"))
	recoveryTurn(t, conn, read, request, userItem("second"))
	// The upstream closes the idle connection between turns.
	select {
	case <-upstream.closed:
	case <-time.After(time.Second):
		t.Fatal("upstream did not close")
	}
	time.Sleep(50 * time.Millisecond) // Let the proxy observe the close.
	recoveryTurn(t, conn, read, request, userItem("third"))
	recoveryTurn(t, conn, read, request, userItem("fourth"))

	frames := []recoveryFrame{<-upstream.frames, <-upstream.frames, <-upstream.frames, <-upstream.frames}
	if frames[2].connection != 2 || frames[2].previousID != "" || len(frames[2].input) != 5 {
		t.Fatalf("first turn on the new connection = connection %d previous %q items %d, want the full history", frames[2].connection, frames[2].previousID, len(frames[2].input))
	}
	if frames[3].connection != 2 || frames[3].previousID != "resp-native-2-1" || len(frames[3].input) != 1 {
		t.Fatalf("next turn = connection %d previous %q items %d, want a one-item continuation", frames[3].connection, frames[3].previousID, len(frames[3].input))
	}
}

func TestResponsesNativeWebSocketOversizedHistoryStaysOnHTTP(t *testing.T) {
	upstream := newRecoveryTestUpstream()
	h := newTestProxyHandler(t, upstream.handler(t))
	h.responsesWS = ResponsesWebSocketConfig{Enabled: true, NativeUpstream: true, DisableAutoCompact: true}
	advertiseNativeWebSocketForTest(h)
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	read := func() map[string]any { return readRecoveryTerminal(t, conn) }
	request := newResponsesWebSocketCreateRequest(nil)

	recoveryTurn(t, conn, read, request, userItem("first"))
	// An oversized turn uses HTTP with the full history.
	big := make([]any, responsesNativeMaxPendingItems+1)
	for i := range big {
		big[i] = userItem("x")
	}
	recoveryTurn(t, conn, read, request, big...)
	// A small next turn would have to send the native connection everything it
	// missed, which is still too large, so it also uses HTTP.
	recoveryTurn(t, conn, read, request, userItem("small"))

	if first := <-upstream.frames; len(first.input) != 1 {
		t.Fatalf("first native turn = %+v", first)
	}
	if upstream.upgrades.Load() != 1 || upstream.posts.Load() != 2 || len(upstream.frames) != 0 {
		t.Fatalf("transport: upgrades=%d posts=%d native=%d, want oversized history to stay on HTTP", upstream.upgrades.Load(), upstream.posts.Load(), len(upstream.frames))
	}
	if second, third := <-upstream.postBodies, <-upstream.postBodies; len(second) != 2+len(big) || len(third) != len(second)+2 {
		t.Fatalf("HTTP turns carried %d then %d items, want the full history", len(second), len(third))
	}
}
