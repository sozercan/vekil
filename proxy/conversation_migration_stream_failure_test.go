package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sozercan/vekil/logger"
)

// conversationStreamResponse writes string parts as SSE body chunks and waits
// between them for any time.Duration part, like an upstream that emits its
// preamble before deciding the request's outcome. An error part ends the body
// with that read error, like a reset connection.
func conversationStreamResponse(req *http.Request, header http.Header, parts ...any) *http.Response {
	if header == nil {
		header = make(http.Header)
	}
	header.Set("Content-Type", "text/event-stream")
	pr, pw := io.Pipe()
	go func() {
		for _, part := range parts {
			switch value := part.(type) {
			case string:
				if _, err := io.WriteString(pw, value); err != nil {
					return
				}
			case time.Duration:
				select {
				case <-time.After(value):
				case <-req.Context().Done():
					_ = pw.CloseWithError(req.Context().Err())
					return
				}
			case error:
				_ = pw.CloseWithError(value)
				return
			case conversationCloseOnCancel:
				<-req.Context().Done()
				_ = pw.Close()
				return
			}
		}
		_ = pw.Close()
	}()
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: pr, ContentLength: -1, Request: req}
}

// conversationCloseOnCancel ends the body with a clean EOF once the request is
// canceled, like a body that unblocks without reporting the cancellation.
type conversationCloseOnCancel struct{}

// conversationStreamedResponseID identifies the response announced by
// conversationLifecycleEvent.
const conversationStreamedResponseID = "resp-streamed"

// conversationLifecycleEvent returns an output-free lifecycle event. Padding
// stands in for the instructions and tools that upstreams echo in the response
// object, which can exceed the precommit byte bound.
func conversationLifecycleEvent(t *testing.T, eventType, status string, padding int) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]any{"type": eventType, "response": map[string]any{
		"id": conversationStreamedResponseID, "object": "response", "status": status, "output": []any{}, "instructions": strings.Repeat("x", padding),
	}})
	if err != nil {
		t.Fatal(err)
	}
	return "event: " + eventType + "\ndata: " + string(encoded) + "\n\n"
}

// conversationBodyTail keeps failure messages readable despite padded preambles.
func conversationBodyTail(recorder *httptest.ResponseRecorder) string {
	body := recorder.Body.String()
	if len(body) > 600 {
		return "..." + body[len(body)-600:]
	}
	return body
}

const (
	conversationRateLimitFailed = "event: response.failed\ndata: " + `{"type":"response.failed","response":{"id":"resp-failed","object":"response","status":"failed","error":{"code":"rate_limit_exceeded","message":"Rate limit reached. Please try again in 1s."},"output":[],"usage":null}}` + "\n\n"
	conversationRateLimitError  = "event: error\ndata: " + `{"type":"error","code":"rate_limit_exceeded","message":"Rate limit reached. Please try again in 1s.","param":null}` + "\n\n"
	// Azure sends these about every 30 seconds while a long generation is quiet.
	conversationKeepalive = "event: keepalive\ndata: " + `{"type":"keepalive","sequence_number":2}` + "\n\n"
	// Streamed output a client can keep but cannot execute.
	conversationStreamedReasoning = "event: response.output_item.done\ndata: " + `{"type":"response.output_item.done","output_index":0,"item":{"id":"rs_streamed","type":"reasoning","encrypted_content":"streamed-encrypted","summary":[]}}` + "\n\n"
	conversationStreamedTextDelta = "event: response.output_text.delta\ndata: " + `{"type":"response.output_text.delta","item_id":"msg_streamed","output_index":1,"content_index":0,"delta":"partial"}` + "\n\n"
	// Part of a tool call. The client has not received all of it.
	conversationStreamedArgumentsDelta = "event: response.function_call_arguments.delta\ndata: " + `{"type":"response.function_call_arguments.delta","item_id":"fc_streamed","output_index":1,"delta":"{}"}` + "\n\n"
	// A completed tool call, which a client such as Codex runs as it arrives.
	conversationStreamedCall = "event: response.output_item.done\ndata: " + `{"type":"response.output_item.done","output_index":1,"item":{"id":"fc_streamed","type":"function_call","status":"completed","call_id":"call-streamed","name":"edit","arguments":"{}"}}` + "\n\n"
)

func TestConversationMigrationCommittedFailureAllowsRetry(t *testing.T) {
	blocked := map[string]bool{"partial tool call before failure": true, "tool call in keepalive": true, "tool call in failure": true}
	for _, scenario := range []string{
		"failed", "error event", "queued then failed", "keepalive then error event", "reasoning and message before failure", "message in keepalive",
		"completed tool call before failure", "partial tool call before failure", "tool call in keepalive", "tool call in failure",
	} {
		t.Run(scenario, func(t *testing.T) {
			var sends, west atomic.Int32
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
				}
				if strings.HasPrefix(req.URL.Host, "west.") {
					west.Add(1)
				}
				// A preamble larger than the precommit byte bound commits the
				// stream before its outcome is known, without quota evidence.
				created := conversationLifecycleEvent(t, "response.created", "in_progress", 2*responsesPrecommitMaxPeekBytes)
				switch sends.Add(1) {
				case 1:
					return conversationResponse(t, req, "seed", conversationText("Known earlier answer.")), nil
				case 2:
					switch scenario {
					case "error event":
						return conversationStreamResponse(req, nil, created, conversationRateLimitError), nil
					case "queued then failed":
						return conversationStreamResponse(req, nil, conversationLifecycleEvent(t, "response.queued", "queued", 0), created, conversationRateLimitFailed), nil
					case "keepalive then error event":
						return conversationStreamResponse(req, nil, created, conversationKeepalive, conversationKeepalive, conversationRateLimitError), nil
					case "reasoning and message before failure":
						return conversationStreamResponse(req, nil, created, conversationStreamedReasoning, conversationStreamedTextDelta, conversationRateLimitFailed), nil
					case "message in keepalive":
						keepalive := "event: keepalive\ndata: " + `{"type":"keepalive","sequence_number":2,"response":{"id":"resp-streamed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"partial"}]}]}}` + "\n\n"
						return conversationStreamResponse(req, nil, created, keepalive, conversationRateLimitError), nil
					case "completed tool call before failure":
						return conversationStreamResponse(req, nil, created, conversationStreamedReasoning, conversationStreamedCall, conversationRateLimitFailed), nil
					case "partial tool call before failure":
						return conversationStreamResponse(req, nil, created, conversationStreamedReasoning, conversationStreamedArgumentsDelta, conversationRateLimitFailed), nil
					case "tool call in keepalive":
						keepalive := "event: keepalive\ndata: " + `{"type":"keepalive","sequence_number":2,"response":{"id":"resp-streamed","output":[{"type":"function_call","call_id":"call-1","name":"edit","arguments":"{}"}]}}` + "\n\n"
						return conversationStreamResponse(req, nil, created, keepalive, conversationRateLimitError), nil
					case "tool call in failure":
						failed := "event: response.failed\ndata: " + `{"type":"response.failed","response":{"id":"resp-failed","object":"response","status":"failed","error":{"code":"rate_limit_exceeded","message":"Rate limit reached."},"output":[{"type":"function_call","call_id":"call-1","name":"edit","arguments":"{}"}],"usage":null}}` + "\n\n"
						return conversationStreamResponse(req, nil, created, conversationStreamedReasoning, failed), nil
					}
					return conversationStreamResponse(req, nil, created, conversationRateLimitFailed), nil
				}
				return conversationResponse(t, req, "retried", conversationText("Retried answer.")), nil
			})
			h, _ := newConversationAPIHandler(t, transport, nil)
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)

			failed := conversationPOST(t, h, map[string]any{"previous_response_id": "seed", "input": "Next.", "stream": true}, nil)
			if failed.Code != http.StatusOK || sends.Load() != 2 || west.Load() != 0 {
				t.Fatalf("committed failure: code=%d sends=%d west=%d %s", failed.Code, sends.Load(), west.Load(), conversationBodyTail(failed))
			}
			retry := conversationPOST(t, h, map[string]any{"previous_response_id": "seed", "input": "Try again."}, nil)
			if blocked[scenario] {
				// Executable output that Vekil could not save keeps the outcome
				// uncertain.
				if !strings.Contains(failed.Body.String(), "conversation_execution_uncertain") {
					t.Fatalf("missing execution uncertainty diagnostic: %s", conversationBodyTail(failed))
				}
				if retry.Code != http.StatusConflict || sends.Load() != 2 || !strings.Contains(retry.Body.String(), "conversation_execution_uncertain") {
					t.Fatalf("uncertain turn retried: %d %s", retry.Code, conversationBodyTail(retry))
				}
				return
			}
			// The client receives the upstream's own failure, including its
			// rate-limit code, rather than a proxy uncertainty error.
			if !strings.Contains(failed.Body.String(), "rate_limit_exceeded") || strings.Contains(failed.Body.String(), "conversation_execution_uncertain") {
				t.Fatalf("upstream failure was not forwarded: %s", conversationBodyTail(failed))
			}
			conversationCompleted(t, retry, false)
			if sends.Load() != 3 || west.Load() != 0 {
				t.Fatalf("retry was not a single owner send: sends=%d west=%d", sends.Load(), west.Load())
			}
		})
	}
}

func TestConversationMigrationUpstreamEndAllowsRetry(t *testing.T) {
	reset := errors.New("connection reset by peer")
	released := map[string]string{
		"reasoning then close": "stream_ended", "message then reset": "stream_ended",
		"tool call then close": "delivered_history_saved", "tool call then close, Codex retry": "delivered_history_saved",
	}
	session := http.Header{"Session_id": {"client-a"}}
	for _, scenario := range []string{
		"reasoning then close", "message then reset", "tool call then close", "tool call then close, Codex retry",
		"arguments then reset", "unrecognized event then close", "proxy error after upstream close", "proxy deadline", "proxy deadline then clean close", "storage failure on release",
	} {
		t.Run(scenario, func(t *testing.T) {
			var h *ProxyHandler
			var sends, west atomic.Int32
			var retriedBody atomic.Value
			retriedBody.Store("")
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
				}
				if strings.HasPrefix(req.URL.Host, "west.") {
					west.Add(1)
				}
				created := conversationLifecycleEvent(t, "response.created", "in_progress", 2*responsesPrecommitMaxPeekBytes)
				switch sends.Add(1) {
				case 1:
					return conversationResponse(t, req, "seed", conversationText("Known earlier answer.")), nil
				case 2:
					switch scenario {
					case "reasoning then close":
						// Seen live from Copilot: one reasoning item, then a clean close
						// about ten minutes later without a terminal event.
						return conversationStreamResponse(req, nil, created, conversationStreamedReasoning), nil
					case "message then reset":
						return conversationStreamResponse(req, nil, created, conversationStreamedTextDelta, reset), nil
					case "tool call then close", "tool call then close, Codex retry":
						return conversationStreamResponse(req, nil, created, conversationStreamedReasoning, conversationStreamedCall), nil
					case "unrecognized event then close":
						vendor := "event: response.vendor_progress\ndata: " + `{"type":"response.vendor_progress","output_index":1}` + "\n\n"
						return conversationStreamResponse(req, nil, created, conversationStreamedReasoning, vendor), nil
					case "arguments then reset":
						return conversationStreamResponse(req, nil, created, conversationStreamedArgumentsDelta, reset), nil
					case "proxy error after upstream close":
						// The upstream closes cleanly, but Vekil rejects an ambiguous
						// event before it, so the stream did not end on its own.
						ambiguous := "data: " + `{"type":"response.output_text.delta","delta":"a","delta":"b"}` + "\n\n"
						return conversationStreamResponse(req, nil, created, conversationStreamedReasoning, ambiguous), nil
					case "storage failure on release":
						// The stream carries no state to bind, so clearing the marker
						// is the next durable commit.
						h.stateBindings.durable.mu.Lock()
						h.stateBindings.durable.beforeCommit = func() error { return syscall.ENOSPC }
						h.stateBindings.durable.mu.Unlock()
						return conversationStreamResponse(req, nil, "data: "+`{"type":"response.output_text.delta","delta":"partial"}`+"\n\n"), nil
					}
					// Vekil's streaming deadline ends the quiet upstream.
					if scenario == "proxy deadline then clean close" {
						return conversationStreamResponse(req, nil, created, conversationStreamedReasoning, conversationCloseOnCancel{}), nil
					}
					return conversationStreamResponse(req, nil, created, conversationStreamedReasoning, time.Minute), nil
				}
				body, _ := io.ReadAll(req.Body)
				retriedBody.Store(string(body))
				req.Body = io.NopCloser(bytes.NewReader(body))
				return conversationResponse(t, req, "retried", conversationText("Retried answer.")), nil
			})
			logs := &conversationLogBuffer{}
			options := []Option{func(h *ProxyHandler) { h.log = logger.NewWithWriter(logger.LevelInfo, logs) }}
			if strings.HasPrefix(scenario, "proxy deadline") {
				options = append(options, WithStreamingUpstreamTimeout(500*time.Millisecond))
			}
			h, _ = newConversationAPIHandler(t, transport, nil, options...)
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, session), false)

			ended := conversationPOST(t, h, map[string]any{"previous_response_id": "seed", "input": "Next.", "stream": true}, session)
			if ended.Code != http.StatusOK || sends.Load() != 2 {
				t.Fatalf("ended turn: code=%d sends=%d %s", ended.Code, sends.Load(), conversationBodyTail(ended))
			}
			if scenario == "storage failure on release" {
				// The client receives the storage failure, not execution uncertainty.
				if body := ended.Body.String(); !strings.Contains(body, "conversation_history_storage_unavailable") || strings.Contains(body, "conversation_execution_uncertain") {
					t.Fatalf("marker-clear failure was not reported: %s", conversationBodyTail(ended))
				}
				return
			}
			// A client may retry without what it received, branching from the
			// earlier history.
			retryFields := map[string]any{"previous_response_id": "seed", "input": "Next."}
			if scenario == "tool call then close, Codex retry" {
				// Codex runs the delivered call and resends it with its output.
				retryFields = map[string]any{"input": []any{
					map[string]any{"role": "user", "content": "Seed."}, conversationText("Known earlier answer."),
					map[string]any{"role": "user", "content": "Next."},
					map[string]any{"id": "rs_streamed", "type": "reasoning", "encrypted_content": "streamed-encrypted", "summary": []any{}},
					map[string]any{"id": "fc_streamed", "type": "function_call", "status": "completed", "call_id": "call-streamed", "name": "edit", "arguments": "{}"},
					map[string]any{"type": "function_call_output", "call_id": "call-streamed", "output": "edited"},
				}}
			}
			retry := conversationPOST(t, h, retryFields, session)
			reason, ok := released[scenario]
			if !ok {
				if retry.Code != http.StatusConflict || sends.Load() != 2 || !strings.Contains(retry.Body.String(), "conversation_execution_uncertain") {
					t.Fatalf("uncertain turn released: %d %s", retry.Code, retry.Body.String())
				}
				return
			}
			// The client sees the upstream's own truncated stream, not a proxy
			// uncertainty error.
			if strings.Contains(ended.Body.String(), "conversation_execution_uncertain") || !strings.Contains(logs.String(), `"reason":"`+reason+`"`) {
				t.Fatalf("ended turn was not released: %s\nlogs: %s", conversationBodyTail(ended), logs.String())
			}
			conversationCompleted(t, retry, false)
			if sends.Load() != 3 || west.Load() != 0 {
				t.Fatalf("retry was not a single owner send: sends=%d west=%d", sends.Load(), west.Load())
			}
			if body := retriedBody.Load().(string); scenario == "tool call then close, Codex retry" && (!strings.Contains(body, "call-streamed") || !strings.Contains(body, "edited")) {
				t.Fatalf("retry lost the delivered call or its output: %s", body)
			}
		})
	}
}

// conversationWebSocketFailedTurn sends one turn and returns its failure frame.
func conversationWebSocketFailedTurn(t *testing.T, conn *websocket.Conn, fields map[string]any) string {
	t.Helper()
	fields["type"], fields["model"], fields["store"] = "response.create", "coding", false
	if err := conn.WriteJSON(fields); err != nil {
		t.Fatal(err)
	}
	for {
		frame := mustReadWebSocketJSONSkipMetadata(t, conn)
		switch frame["type"] {
		case "error", "response.failed":
			encoded, _ := json.Marshal(frame)
			return string(encoded)
		case "response.completed":
			t.Fatalf("websocket turn completed: %v", frame)
		}
	}
}

func TestConversationMigrationWebSocketUpstreamEndAllowsRetry(t *testing.T) {
	for _, scenario := range []string{"reasoning then close", "tool call then close", "partial tool call then close"} {
		t.Run(scenario, func(t *testing.T) {
			var sends atomic.Int32
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
				}
				created := conversationLifecycleEvent(t, "response.created", "in_progress", 2*responsesPrecommitMaxPeekBytes)
				switch sends.Add(1) {
				case 1:
					return conversationResponse(t, req, "seed", conversationText("Known earlier answer.")), nil
				case 2:
					switch scenario {
					case "tool call then close":
						return conversationStreamResponse(req, nil, created, conversationStreamedReasoning, conversationStreamedCall), nil
					case "partial tool call then close":
						return conversationStreamResponse(req, nil, created, conversationStreamedReasoning, conversationStreamedArgumentsDelta), nil
					}
					return conversationStreamResponse(req, nil, created, conversationStreamedReasoning), nil
				}
				return conversationResponse(t, req, "retried", conversationText("Retried answer.")), nil
			})
			h, _ := newConversationAPIHandler(t, transport, nil)
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)
			server := startResponsesWebSocketProxyServer(t, h)
			conn := mustDialResponsesWebSocket(t, server, nil)
			defer func() { _ = conn.Close() }()

			turn := func() map[string]any { return map[string]any{"previous_response_id": "seed", "input": "Next."} }
			ended := conversationWebSocketFailedTurn(t, conn, turn())
			if scenario == "partial tool call then close" {
				retry := conversationWebSocketFailedTurn(t, conn, turn())
				if sends.Load() != 2 || !strings.Contains(retry, "conversation_execution_uncertain") {
					t.Fatalf("uncertain turn released: sends=%d %s", sends.Load(), retry)
				}
				return
			}
			if strings.Contains(ended, "conversation_execution_uncertain") {
				t.Fatalf("ended turn reported uncertainty: %s", ended)
			}
			conversationWebSocketTurn(t, conn, turn())
			if sends.Load() != 3 {
				t.Fatalf("retry was not a single owner send: sends=%d", sends.Load())
			}
		})
	}
}

func TestConversationMigrationQuotaEvidenceHoldsPreambleForFailover(t *testing.T) {
	for _, scenario := range []string{"slow failure", "large preamble"} {
		t.Run(scenario, func(t *testing.T) {
			var east, west atomic.Int32
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
				}
				if strings.HasPrefix(req.URL.Host, "west.") {
					west.Add(1)
					return conversationResponse(t, req, "recovered-west", conversationText("Recovered.")), nil
				}
				if east.Add(1) == 1 {
					return conversationResponse(t, req, "seed", conversationText("Known earlier answer.")), nil
				}
				// Azure can admit a stream whose headers already report exhausted
				// quota, then fail it once prefill finishes.
				quota := http.Header{"X-Ratelimit-Remaining-Tokens": {"-10196"}, "Retry-After-Ms": {"611"}}
				if scenario == "large preamble" {
					return conversationStreamResponse(req, quota, conversationLifecycleEvent(t, "response.created", "in_progress", 2*responsesPrecommitMaxPeekBytes), conversationRateLimitFailed), nil
				}
				return conversationStreamResponse(req, quota, conversationLifecycleEvent(t, "response.created", "in_progress", 0),
					responsesPrecommitPeekTimeout+500*time.Millisecond, conversationRateLimitFailed), nil
			})
			h, _ := newConversationAPIHandler(t, transport, nil)
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)

			response := conversationPOST(t, h, map[string]any{"previous_response_id": "seed", "input": "Next.", "stream": true}, nil)
			conversationCompleted(t, response, true)
			if east.Load() != 2 || west.Load() != 1 {
				t.Fatalf("quota failure was not held for failover: east=%d west=%d %s", east.Load(), west.Load(), conversationBodyTail(response))
			}
		})
	}
}

func TestConversationMigrationPrecommitFailureWithUsageAllowsRetry(t *testing.T) {
	var sends, west atomic.Int32
	transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/models") {
			return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
		}
		if strings.HasPrefix(req.URL.Host, "west.") {
			west.Add(1)
		}
		switch sends.Add(1) {
		case 1:
			return conversationResponse(t, req, "seed", conversationText("Known earlier answer.")), nil
		case 2:
			// Reported usage makes the failure unsafe to replay on another
			// target, but the terminal event still carries no output.
			failed := "event: response.failed\ndata: " + `{"type":"response.failed","response":{"id":"resp-failed","object":"response","status":"failed","error":{"code":"rate_limit_exceeded","message":"Rate limit reached."},"output":[],"usage":{"input_tokens":120,"output_tokens":0,"total_tokens":120}}}` + "\n\n"
			return conversationStreamResponse(req, nil, conversationLifecycleEvent(t, "response.created", "in_progress", 0), failed), nil
		}
		return conversationResponse(t, req, "retried", conversationText("Retried answer.")), nil
	})
	h, _ := newConversationAPIHandler(t, transport, nil)
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)

	failed := conversationPOST(t, h, map[string]any{"previous_response_id": "seed", "input": "Next.", "stream": true}, nil)
	if failed.Code < http.StatusBadRequest || strings.Contains(failed.Body.String(), "conversation_execution_uncertain") || sends.Load() != 2 || west.Load() != 0 {
		t.Fatalf("precommit failure: code=%d sends=%d west=%d %s", failed.Code, sends.Load(), west.Load(), conversationBodyTail(failed))
	}
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"previous_response_id": "seed", "input": "Try again."}, nil), false)
	if sends.Load() != 3 || west.Load() != 0 {
		t.Fatalf("retry was not a single owner send: sends=%d west=%d", sends.Load(), west.Load())
	}
}
