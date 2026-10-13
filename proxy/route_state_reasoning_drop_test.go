package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

func reasoningDropItem(token string) map[string]any {
	return map[string]any{"type": "reasoning", "encrypted_content": token, "summary": []any{}}
}

func reasoningDropUserMessage() map[string]any {
	return map[string]any{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "continue"}}}
}

// forwardedReasoning returns the reasoning content an upstream received, and
// whether the user message survived.
func forwardedReasoning(t *testing.T, body []byte) ([]string, bool) {
	t.Helper()
	var request struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		t.Fatalf("forwarded body: %v: %s", err, body)
	}
	var reasoning []string
	user := false
	for _, item := range request.Input {
		switch {
		case item["type"] == "reasoning":
			value, _ := item["encrypted_content"].(string)
			reasoning = append(reasoning, value)
		case item["role"] == "user":
			user = true
		}
	}
	return reasoning, user
}

func TestExplicitResponsesUnboundReasoningIsDropped(t *testing.T) {
	cases := []struct {
		name   string
		tokens []string
		owner  string
		kept   []string
	}{
		{"all missing", []string{"opaque-missing", "opaque-missing-b"}, "primary", nil},
		{"known owner then missing", []string{"opaque-known-b", "opaque-missing"}, "secondary", []string{"opaque-known-b"}},
		{"missing then known owner", []string{"opaque-missing", "opaque-known-b"}, "secondary", []string{"opaque-known-b"}},
	}
	for _, surface := range []string{"json", "stream", "compact"} {
		for _, tc := range cases {
			t.Run(surface+"/"+tc.name, func(t *testing.T) {
				h, primary, secondary := newResponsesStateDiagnosticHandler(t)
				input := make([]any, 0, len(tc.tokens)+1)
				for _, token := range tc.tokens {
					input = append(input, reasoningDropItem(token))
				}
				input = append(input, reasoningDropUserMessage())
				body, err := json.Marshal(map[string]any{"model": "public-ws-model", "input": input, "stream": surface == "stream"})
				if err != nil {
					t.Fatal(err)
				}
				path, handle := "/v1/responses", h.HandleResponses
				if surface == "compact" {
					path, handle = "/v1/responses/compact", h.HandleCompact
				}
				w := httptest.NewRecorder()
				handle(w, httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body)))
				if w.Code != http.StatusOK {
					t.Fatalf("response = %d %s", w.Code, w.Body.String())
				}
				owner, other := primary, secondary
				if tc.owner == "secondary" {
					owner, other = secondary, primary
				}
				if other.calls.Load() != 0 {
					t.Fatalf("request left the owner of its remaining state: primary=%d secondary=%d", primary.calls.Load(), secondary.calls.Load())
				}
				forwarded, _ := owner.onlyRequest(t)
				if strings.Contains(string(forwarded), "opaque-missing") {
					t.Fatalf("unbound reasoning was forwarded: %s", forwarded)
				}
				if surface != "compact" {
					reasoning, user := forwardedReasoning(t, forwarded)
					if !reflect.DeepEqual(reasoning, tc.kept) || !user {
						t.Fatalf("forwarded reasoning = %v user=%t, want %v and the user message", reasoning, user, tc.kept)
					}
				}
				if got := h.stats.snapshot().StateBindingReasoningDrops; got != 1 {
					t.Fatalf("reasoning drops = %d, want 1", got)
				}
			})
		}
	}
}

func TestExplicitResponsesUnboundReasoningWithOtherStateFailsClosed(t *testing.T) {
	cases := []struct {
		name      string
		input     []any
		turnState string
		wantCode  string
	}{
		{"unknown compaction", []any{reasoningDropItem("opaque-missing"), map[string]any{"type": "compaction", "encrypted_content": "opaque-missing-b"}}, "", "provider_state_unavailable"},
		{"unknown turn state", []any{reasoningDropItem("opaque-missing")}, "opaque-missing-turn", "provider_state_unavailable"},
		{"tombstone", []any{reasoningDropItem("opaque-missing"), reasoningDropItem("opaque-tombstone")}, "", ""},
		{"different owners", []any{reasoningDropItem("opaque-missing"), reasoningDropItem("opaque-known-a"), reasoningDropItem("opaque-known-b")}, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, primary, secondary := newResponsesStateDiagnosticHandler(t)
			body, err := json.Marshal(map[string]any{"model": "public-ws-model", "input": append(tc.input, reasoningDropUserMessage())})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			if tc.turnState != "" {
				req.Header.Set("X-Codex-Turn-State", tc.turnState)
			}
			w := httptest.NewRecorder()
			h.HandleResponses(w, req)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("response = %d %s", w.Code, w.Body.String())
			}
			var response struct {
				Error struct {
					Code *string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			if code := response.Error.Code; (tc.wantCode == "") != (code == nil) || (code != nil && *code != tc.wantCode) {
				t.Fatalf("error = %s, want code %q", w.Body.String(), tc.wantCode)
			}
			if primary.calls.Load() != 0 || secondary.calls.Load() != 0 {
				t.Fatalf("rejection sent upstream: primary=%d secondary=%d", primary.calls.Load(), secondary.calls.Load())
			}
			if got := h.stats.snapshot().StateBindingReasoningDrops; got != 0 {
				t.Fatalf("reasoning drops = %d, want 0", got)
			}
		})
	}
}

func TestDurableResponsesUnboundReasoningIsDropped(t *testing.T) {
	h, primary, secondary := newDurableDiagnosticHandler(t)
	body, err := json.Marshal(map[string]any{"model": "public-ws-model", "input": []any{reasoningDropItem("opaque-missing"), reasoningDropUserMessage()}})
	if err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.HandleResponses(w, httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body)))
	if w.Code != http.StatusOK || secondary.calls.Load() != 0 {
		t.Fatalf("response = %d %s, secondary calls = %d", w.Code, w.Body.String(), secondary.calls.Load())
	}
	forwarded, _ := primary.onlyRequest(t)
	if reasoning, user := forwardedReasoning(t, forwarded); len(reasoning) != 0 || !user {
		t.Fatalf("forwarded reasoning = %v user=%t", reasoning, user)
	}
}

func TestExplicitResponsesWebSocketUnboundReasoningIsDropped(t *testing.T) {
	var mu sync.Mutex
	var bodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, body)
		mu.Unlock()
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_reasoning_drop\",\"model\":\"physical-primary\"}}\n\n")
		_, _ = fmt.Fprint(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_reasoning_drop\",\"model\":\"physical-primary\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
	}))
	defer upstream.Close()

	h := newExplicitRouteResponsesWebSocketHandler(t, upstream.URL, upstream.URL)
	server := startResponsesWebSocketProxyServer(t, h)
	conn := mustDialResponsesWebSocket(t, server, nil)
	defer func() { _ = conn.Close() }()
	request := newResponsesWebSocketCreateRequest([]any{reasoningDropItem("opaque-missing"), reasoningDropUserMessage()})
	request["model"] = "public-ws-model"
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	for {
		frame := mustReadWebSocketJSONSkipMetadata(t, conn)
		if frame["type"] == "error" {
			t.Fatalf("error frame = %+v", frame)
		}
		if frame["type"] == "response.completed" {
			break
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 1 {
		t.Fatalf("upstream requests = %d, want 1", len(bodies))
	}
	if reasoning, user := forwardedReasoning(t, bodies[0]); len(reasoning) != 0 || !user {
		t.Fatalf("forwarded reasoning = %v user=%t", reasoning, user)
	}
}
