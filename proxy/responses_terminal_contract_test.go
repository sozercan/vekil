package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
)

func TestResponsesWebSocketTerminalOutputControlsReplay(t *testing.T) {
	for _, test := range []struct {
		name     string
		done     []string
		terminal string
		output   string
		want     []string
	}{
		{name: "completed reordered", done: []string{"b", "a"}, terminal: "response.completed", output: `[{"type":"function_call","call_id":"a","name":"lookup","arguments":"{}"},{"type":"function_call","call_id":"b","name":"lookup","arguments":"{}"}]`, want: []string{"a", "b"}},
		{name: "completed missing done", done: []string{"a"}, terminal: "response.completed", output: `[{"type":"function_call","call_id":"a","name":"lookup","arguments":"{}"},{"type":"function_call","call_id":"b","name":"lookup","arguments":"{}"}]`, want: []string{"a", "b"}},
		{name: "completed empty", done: []string{"a"}, terminal: "response.completed", output: `[]`},
		{name: "absent output preserves done", done: []string{"a"}, terminal: "response.completed", want: []string{"a"}},
		{name: "incomplete snapshot", done: []string{"b"}, terminal: "response.incomplete", output: `[{"type":"function_call","call_id":"a","name":"lookup","arguments":"{}"}]`, want: []string{"a"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			replayed := make(chan []string, 1)
			h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				if calls.Add(1) == 1 {
					for _, id := range test.done {
						_, _ = fmt.Fprintf(w, "data: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"function_call\",\"call_id\":%q,\"name\":\"lookup\",\"arguments\":\"{}\"}}\n\n", id)
					}
					output := ""
					if test.output != "" {
						output = `,"output":` + test.output
					}
					_, _ = fmt.Fprintf(w, "data: {\"type\":%q,\"response\":{\"id\":\"resp-first\"%s}}\n\n", test.terminal, output)
					return
				}
				var request struct {
					Input []struct {
						CallID string `json:"call_id"`
					} `json:"input"`
				}
				if json.NewDecoder(r.Body).Decode(&request) != nil {
					t.Error("invalid replay body")
				}
				var ids []string
				for _, item := range request.Input {
					if item.CallID != "" {
						ids = append(ids, item.CallID)
					}
				}
				replayed <- ids
				_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-next\"}}\n\n")
			})
			h.responsesWS.DisableAutoCompact = true
			conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
			defer func() { _ = conn.Close() }()
			create := newResponsesWebSocketCreateRequest(nil)
			if err := conn.WriteJSON(create); err != nil {
				t.Fatal(err)
			}
			for {
				if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] == test.terminal {
					break
				}
			}
			create["previous_response_id"] = "resp-first"
			if err := conn.WriteJSON(create); err != nil {
				t.Fatal(err)
			}
			if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.completed" {
				t.Fatalf("continuation = %#v", frame)
			}
			if got := <-replayed; !reflect.DeepEqual(got, test.want) {
				t.Fatalf("replayed call order = %v, want %v", got, test.want)
			}
		})
	}
}

func TestResponsesCancelledHTTPPreservesUsage(t *testing.T) {
	for _, eventType := range []string{"response.completed", "response.cancelled"} {
		t.Run(eventType, func(t *testing.T) {
			stream := fmt.Sprintf("data: {\"type\":%q,\"response\":{\"id\":\"resp-terminal\",\"usage\":{\"input_tokens\":7,\"output_tokens\":2,\"total_tokens\":9}}}\n\n", eventType)
			h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, stream)
			})
			ctx, summary := WithRequestSummary(context.Background())
			req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.4","input":"hello","stream":true}`)).WithContext(ctx)
			w := httptest.NewRecorder()
			h.HandleResponses(w, req)
			if w.Code != http.StatusOK || w.Body.String() != stream {
				t.Fatalf("response = %d %s", w.Code, w.Body.String())
			}
			if summary.failureStatus != 0 || summary.promptTokens == nil || *summary.promptTokens != 7 || summary.completionTokens == nil || *summary.completionTokens != 2 || summary.totalTokens == nil || *summary.totalTokens != 9 {
				t.Fatalf("terminal accounting = failure:%d prompt:%v completion:%v total:%v", summary.failureStatus, summary.promptTokens, summary.completionTokens, summary.totalTokens)
			}
		})
	}
}

func TestResponsesCancelledWebSocketDoesNotPublishReplay(t *testing.T) {
	var sends atomic.Int32
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		sends.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"type\":\"response.cancelled\",\"response\":{\"id\":\"resp-cancelled\",\"usage\":{\"input_tokens\":7,\"output_tokens\":2,\"total_tokens\":9}}}\n\n")
	})
	h.stats = newStatsCollector()
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	create := newResponsesWebSocketCreateRequest(nil)
	if err := conn.WriteJSON(create); err != nil {
		t.Fatal(err)
	}
	if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.cancelled" {
		t.Fatalf("terminal = %#v", frame)
	}
	create["previous_response_id"] = "resp-cancelled"
	if err := conn.WriteJSON(create); err != nil {
		t.Fatal(err)
	}
	if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "error" || frame["status_code"] != float64(http.StatusBadRequest) {
		t.Fatalf("cancelled response became resumable: %#v", frame)
	}
	stats := h.stats.snapshot()
	if sends.Load() != 1 || stats.Totals.Requests != 1 || stats.Totals.Errors != 0 || stats.Totals.PromptTokens != 7 || stats.Totals.CompletionTokens != 2 || stats.Totals.TotalTokens != 9 {
		t.Fatalf("cancelled turn accounting = sends:%d totals:%+v", sends.Load(), stats.Totals)
	}
}

func TestResponsesCancelledWithoutID(t *testing.T) {
	const terminal = `{"type":"response.cancelled","response":{"usage":{"input_tokens":7,"output_tokens":2,"total_tokens":9}}}`
	h := newTestProxyHandler(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: "+terminal+"\n\n")
	})
	h.stats = newStatsCollector()
	conn := mustDialResponsesWebSocket(t, startResponsesWebSocketProxyServer(t, h), nil)
	defer func() { _ = conn.Close() }()
	if err := conn.WriteJSON(newResponsesWebSocketCreateRequest(nil)); err != nil {
		t.Fatal(err)
	}
	if frame := mustReadWebSocketJSONSkipMetadata(t, conn); frame["type"] != "response.cancelled" {
		t.Fatalf("cancelled response without ID = %#v", frame)
	}
	// A local warmup synchronizes with completion of cancellation accounting.
	request := newResponsesWebSocketCreateRequest(nil)
	request["generate"] = false
	if err := conn.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	_ = mustReadWebSocketJSONSkipMetadata(t, conn)
	_ = mustReadWebSocketJSONSkipMetadata(t, conn)
	stats := h.stats.snapshot()
	if stats.Totals.Errors != 0 || stats.Totals.Requests != 1 || stats.Totals.TotalTokens != 9 {
		t.Fatalf("cancelled response without ID accounting = %+v", stats.Totals)
	}
}
