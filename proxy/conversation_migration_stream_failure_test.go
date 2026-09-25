package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// conversationStreamResponse writes string parts as SSE body chunks and waits
// between them for any time.Duration part, like an upstream that emits its
// preamble before deciding the request's outcome.
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
			}
		}
		_ = pw.Close()
	}()
	return &http.Response{StatusCode: http.StatusOK, Header: header, Body: pr, ContentLength: -1, Request: req}
}

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
)

func TestConversationMigrationCommittedFailureBeforeOutputAllowsRetry(t *testing.T) {
	for _, scenario := range []string{"failed", "error event", "queued then failed", "output before failure"} {
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
					case "output before failure":
						return conversationStreamResponse(req, nil, created, "data: "+`{"type":"response.output_text.delta","delta":"partial"}`+"\n\n", conversationRateLimitFailed), nil
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
			if scenario == "output before failure" {
				// Output reached the client, so the outcome remains uncertain.
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
