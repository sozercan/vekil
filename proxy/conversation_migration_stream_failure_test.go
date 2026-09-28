package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
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
			case chan struct{}:
				// Signal that every earlier part was read.
				close(value)
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
	// Part of a tool call. No client runs it before the call is complete.
	conversationStreamedArgumentsDelta = "event: response.function_call_arguments.delta\ndata: " + `{"type":"response.function_call_arguments.delta","item_id":"fc_streamed","output_index":1,"delta":"{}"}` + "\n\n"
	// Complete arguments without the finished item: a client could run them,
	// but Vekil cannot save them as history.
	conversationStreamedArgumentsDone = "event: response.function_call_arguments.done\ndata: " + `{"type":"response.function_call_arguments.done","item_id":"fc_streamed","output_index":1,"arguments":"{}"}` + "\n\n"
	// A completed tool call, which a client such as Codex runs as it arrives.
	conversationStreamedCall = "event: response.output_item.done\ndata: " + `{"type":"response.output_item.done","output_index":1,"item":{"id":"fc_streamed","type":"function_call","status":"completed","call_id":"call-streamed","name":"edit","arguments":"{}"}}` + "\n\n"
)

func TestConversationMigrationCommittedFailureAllowsRetry(t *testing.T) {
	blocked := map[string]bool{"complete arguments before failure": true, "tool call in keepalive": true, "tool call in failure": true}
	for _, scenario := range []string{
		"failed", "error event", "queued then failed", "keepalive then error event", "reasoning and message before failure", "message in keepalive",
		"partial tool call before failure", "completed tool call before failure", "complete arguments before failure", "tool call in keepalive", "tool call in failure",
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
					case "complete arguments before failure":
						return conversationStreamResponse(req, nil, created, conversationStreamedReasoning, conversationStreamedArgumentsDone, conversationRateLimitFailed), nil
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
			if scenario == "completed tool call before failure" {
				// A branch from before the delivered call could repeat it. The
				// continuation that returns the call's output is admitted.
				if retry.Code != http.StatusConflict || sends.Load() != 2 {
					t.Fatalf("branch after a delivered call was admitted: %d %s", retry.Code, conversationBodyTail(retry))
				}
				retry = conversationPOST(t, h, map[string]any{"previous_response_id": conversationStreamedResponseID, "input": []any{
					map[string]any{"type": "function_call_output", "call_id": "call-streamed", "output": "edited"},
				}}, nil)
			}
			if blocked[scenario] {
				// Executable output that Vekil could not save keeps the outcome
				// uncertain. The stream ends with response.failed, which Codex
				// reports, rather than an error event, which it ignores.
				if tail := conversationBodyTail(failed); !strings.Contains(tail, "event: response.failed") || !strings.Contains(tail, `"code":"conversation_execution_uncertain"`) {
					t.Fatalf("missing execution uncertainty diagnostic: %s", tail)
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
		"reasoning then close": "stream_ended", "message then reset": "stream_ended", "arguments then reset": "stream_ended",
		"tool call then close": "delivered_history_saved", "tool call then close, Codex retry": "delivered_history_saved",
		// A stream Vekil ends leaves the client with exactly what it received too.
		"proxy error after upstream close": "proxy_ended", "DONE without completion": "proxy_ended",
		"proxy deadline": "proxy_ended", "proxy deadline then clean close": "proxy_ended",
	}
	session := http.Header{"Session_id": {"client-a"}}
	for _, scenario := range []string{
		"reasoning then close", "message then reset", "tool call then close", "tool call then close, Codex retry",
		"tool call then close, branch retry", "arguments then reset", "complete arguments then close", "arguments closed by another item", "arguments replaced by another item",
		"unrecognized event then close", "DONE without completion", "proxy error after upstream close", "proxy deadline", "proxy deadline then clean close", "storage failure on release",
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
					case "tool call then close", "tool call then close, Codex retry", "tool call then close, branch retry":
						return conversationStreamResponse(req, nil, created, conversationStreamedReasoning, conversationStreamedCall), nil
					case "unrecognized event then close":
						vendor := "event: response.vendor_progress\ndata: " + `{"type":"response.vendor_progress","output_index":1}` + "\n\n"
						return conversationStreamResponse(req, nil, created, conversationStreamedReasoning, vendor), nil
					case "arguments then reset":
						return conversationStreamResponse(req, nil, created, conversationStreamedArgumentsDelta, reset), nil
					case "complete arguments then close":
						return conversationStreamResponse(req, nil, created, conversationStreamedArgumentsDone), nil
					case "arguments closed by another item":
						// A malformed stream finishes a different call at the same index.
						other := conversationOutputItemDone(t, 1, map[string]any{"id": "fc_other", "type": "function_call", "status": "completed", "call_id": "call-other", "name": "edit", "arguments": "{}"})
						return conversationStreamResponse(req, nil, created, conversationStreamedArgumentsDone, other), nil
					case "arguments replaced by another item":
						// A second call's arguments at the same index must not hide the first.
						otherArguments := strings.ReplaceAll(conversationStreamedArgumentsDone, "fc_streamed", "fc_other")
						other := conversationOutputItemDone(t, 1, map[string]any{"id": "fc_other", "type": "function_call", "status": "completed", "call_id": "call-other", "name": "edit", "arguments": "{}"})
						return conversationStreamResponse(req, nil, created, conversationStreamedArgumentsDone, otherArguments, other), nil
					case "DONE without completion":
						return conversationStreamResponse(req, nil, created, conversationStreamedTextDelta, "data: [DONE]\n\n"), nil
					case "proxy error after upstream close":
						// The upstream closes cleanly, but Vekil rejects an ambiguous
						// event before it and ends the stream itself.
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
			// After reasoning or a message, a client may retry from the earlier
			// history. After a delivered tool call, only a continuation that
			// returns the call's output is admitted; a branch could repeat it.
			retryFields := map[string]any{"previous_response_id": "seed", "input": "Next."}
			switch scenario {
			case "tool call then close":
				retryFields = map[string]any{"previous_response_id": conversationStreamedResponseID, "input": []any{
					map[string]any{"type": "function_call_output", "call_id": "call-streamed", "output": "edited"},
				}}
			case "tool call then close, Codex retry":
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
				// Only the branch retry follows a save; a saved snapshot would
				// admit its continuations.
				if saved := strings.Contains(logs.String(), `"reason":"delivered_history_saved"`); saved != (scenario == "tool call then close, branch retry") {
					t.Fatalf("delivered history saved=%v\nlogs: %s", saved, logs.String())
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
			if body := retriedBody.Load().(string); strings.HasPrefix(scenario, "tool call then close") && (!strings.Contains(body, "call-streamed") || !strings.Contains(body, "edited")) {
				t.Fatalf("retry lost the delivered call or its output: %s", body)
			}
		})
	}
}

func TestConversationMigrationShutdownMidStreamKeepsConversationUsable(t *testing.T) {
	for _, scenario := range []string{"reasoning", "tool call last"} {
		t.Run(scenario, func(t *testing.T) {
			var sends atomic.Int32
			streaming := make(chan struct{})
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
				}
				switch sends.Add(1) {
				case 1:
					return conversationResponse(t, req, "seed", conversationText("Known earlier answer.")), nil
				case 2:
					parts := []any{conversationLifecycleEvent(t, "response.created", "in_progress", 2*responsesPrecommitMaxPeekBytes), conversationStreamedReasoning}
					if scenario == "tool call last" {
						parts = append(parts, conversationStreamedCall)
					}
					return conversationStreamResponse(req, nil, append(parts, streaming, time.Minute)...), nil
				}
				return conversationResponse(t, req, "retried", conversationText("Retried answer.")), nil
			})
			logs := &conversationLogBuffer{}
			h, cfg := newConversationAPIHandler(t, transport, nil, func(h *ProxyHandler) { h.log = logger.NewWithWriter(logger.LevelDebug, logs) })
			defer func() {
				if t.Failed() {
					t.Log(logs.String())
				}
			}()
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)

			done := make(chan struct{})
			go func() {
				defer close(done)
				conversationPOST(t, h, map[string]any{"previous_response_id": "seed", "input": "Next.", "stream": true}, nil)
			}()
			select {
			case <-streaming:
			case <-time.After(5 * time.Second):
				t.Fatal("stream never started")
			}
			// Restarting Vekil mid-turn ends the stream. Like the server, drain
			// the handler before closing the store.
			h.BeginShutdown()
			<-done
			stopConversationAPIHandler(t, h)
			h, _ = newConversationAPIHandler(t, transport, &cfg)
			retry := conversationPOST(t, h, map[string]any{"previous_response_id": "seed", "input": "Next."}, nil)
			if scenario == "tool call last" {
				// The last event is not confirmed as delivered, so a completed
				// call among it keeps the turn uncertain.
				if retry.Code != http.StatusConflict || sends.Load() != 2 {
					t.Fatalf("unconfirmed tool call released: %d %s", retry.Code, retry.Body.String())
				}
				return
			}
			conversationCompleted(t, retry, false)
			if sends.Load() != 3 {
				t.Fatalf("retry after restart was not one owner send: sends=%d", sends.Load())
			}
		})
	}
}

func TestConversationClientResendsDelivered(t *testing.T) {
	for userAgent, want := range map[string]bool{
		"codex_exec/0.157.1 (Mac OS 27.0.0; arm64) ghostty/1.3.2": true,
		"codex_cli_rs/0.157.0":               true,
		"codex_cli_rs/1.0.0 (Linux; x86_64)": true,
		"codex-tui/0.160.2 (Mac OS 27.0.0)":  true,
		"codex_vscode/0.157.1":               true,
		"codexifier/1.0.0":                   false,
		"codex_proxy/0.157.1":                false,
		"codex_cli_rs/0.156.9":               false,
		"codex_cli_rs/0.0.0":                 false,
		"codex_cli_rs/0.157.1-alpha.2":       false,
		"codex_exec/0.157.-0":                false,
		"codex_exec/0.+157.0":                false,
		"codex_cli_rs":                       false,
		"Mozilla/5.0 codex_cli_rs/0.157.1":   false,
		"OpenAI/Python 1.40.0":               false,
		"":                                   false,
	} {
		if got := conversationClientResendsDelivered(userAgent); got != want {
			t.Errorf("conversationClientResendsDelivered(%q) = %t, want %t", userAgent, got, want)
		}
	}
}

func TestConversationMigrationCodexRetryPassesUnsettledAttempt(t *testing.T) {
	for _, scenario := range []string{"complete arguments then close", "ambiguous write"} {
		t.Run(scenario, func(t *testing.T) {
			var sends atomic.Int32
			transport := routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				if strings.HasSuffix(req.URL.Path, "/models") {
					return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
				}
				switch sends.Add(1) {
				case 1:
					return conversationResponse(t, req, "seed", conversationText("Known earlier answer.")), nil
				case 2:
					if scenario == "ambiguous write" {
						// The request may have reached the upstream before the reset.
						if trace := httptrace.ContextClientTrace(req.Context()); trace != nil {
							trace.WroteHeaders()
						}
						return nil, io.ErrUnexpectedEOF
					}
					created := conversationLifecycleEvent(t, "response.created", "in_progress", 2*responsesPrecommitMaxPeekBytes)
					return conversationStreamResponse(req, nil, created, conversationStreamedArgumentsDone), nil
				}
				return conversationResponse(t, req, "retried", conversationText("Retried answer.")), nil
			})
			logs := &conversationLogBuffer{}
			h, _ := newConversationAPIHandler(t, transport, nil, func(h *ProxyHandler) { h.log = logger.NewWithWriter(logger.LevelInfo, logs) })
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": "Seed."}, nil), false)
			conversationPOST(t, h, map[string]any{"previous_response_id": "seed", "input": "Next.", "stream": true}, nil)

			retry := func(userAgent string) *httptest.ResponseRecorder {
				return conversationPOST(t, h, map[string]any{"previous_response_id": "seed", "input": "Next."}, http.Header{"User-Agent": {userAgent}})
			}
			// Clients not verified to resend what they received stay blocked.
			for _, userAgent := range []string{"OpenAI/Python 1.40.0", "codex_cli_rs/0.156.0"} {
				if blocked := retry(userAgent); blocked.Code != http.StatusConflict || sends.Load() != 2 {
					t.Fatalf("%s retry admitted: %d %s", userAgent, blocked.Code, blocked.Body.String())
				}
			}
			conversationCompleted(t, retry("codex_exec/0.157.1 (Mac OS 27.0.0; arm64)"), false)
			if sends.Load() != 3 || !strings.Contains(logs.String(), `"reason":"client_resends_delivered"`) {
				t.Fatalf("Codex retry: sends=%d logs=%s", sends.Load(), logs.String())
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
	for _, scenario := range []string{"reasoning then close", "tool call then close", "complete arguments then close"} {
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
					case "complete arguments then close":
						return conversationStreamResponse(req, nil, created, conversationStreamedReasoning, conversationStreamedArgumentsDone), nil
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
			if scenario == "complete arguments then close" {
				retry := conversationWebSocketFailedTurn(t, conn, turn())
				if sends.Load() != 2 || !strings.Contains(retry, "conversation_execution_uncertain") {
					t.Fatalf("uncertain turn released: sends=%d %s", sends.Load(), retry)
				}
				return
			}
			if strings.Contains(ended, "conversation_execution_uncertain") {
				t.Fatalf("ended turn reported uncertainty: %s", ended)
			}
			next := turn()
			if scenario == "tool call then close" {
				// A branch could repeat the delivered call; its continuation cannot.
				if branch := conversationWebSocketFailedTurn(t, conn, turn()); sends.Load() != 2 || !strings.Contains(branch, "conversation_execution_uncertain") {
					t.Fatalf("branch after a delivered call was admitted: sends=%d %s", sends.Load(), branch)
				}
				next = map[string]any{"previous_response_id": conversationStreamedResponseID, "input": []any{
					map[string]any{"type": "function_call_output", "call_id": "call-streamed", "output": "edited"},
				}}
			}
			conversationWebSocketTurn(t, conn, next)
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
