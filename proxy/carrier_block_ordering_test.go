package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/models"
)

// anthropicBlockFrames returns the ordered content_block_start / content_block_stop frames of
// an Anthropic SSE body as "start:N" / "stop:N" strings. The assertion has to read the WIRE,
// not the stream state: every bug in this family is correct in the struct and wrong in the
// frame order the client actually parses.
func anthropicBlockFrames(t *testing.T, body string) []string {
	t.Helper()
	var frames []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Index *int   `json:"index"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil {
			continue
		}
		if event.Index == nil {
			continue
		}
		switch event.Type {
		case "content_block_start":
			frames = append(frames, fmt.Sprintf("start:%d", *event.Index))
		case "content_block_stop":
			frames = append(frames, fmt.Sprintf("stop:%d", *event.Index))
		}
	}
	return frames
}

// Anthropic content blocks are sequential, never nested: a content_block_start must be closed
// by its own content_block_stop before the next block opens. A client that builds blocks from
// this stream cannot attribute deltas once the frames interleave.
func requireSequentialBlocks(t *testing.T, body string) {
	t.Helper()
	frames := anthropicBlockFrames(t, body)
	open := -1
	for i, frame := range frames {
		index := 0
		if _, err := fmt.Sscanf(strings.TrimPrefix(strings.TrimPrefix(frame, "start:"), "stop:"), "%d", &index); err != nil {
			t.Fatalf("unparsable frame %q", frame)
		}
		if strings.HasPrefix(frame, "start:") {
			if open >= 0 {
				t.Fatalf("block %d opened while block %d was still open; frames = %v (frame %d)", index, open, frames, i)
			}
			open = index
			continue
		}
		if open < 0 {
			t.Fatalf("block %d stopped while no block was open; frames = %v (frame %d)", index, frames, i)
		}
		if open != index {
			t.Fatalf("block %d stopped while block %d was open; frames = %v (frame %d)", index, open, frames, i)
		}
		open = -1
	}
	if open >= 0 {
		t.Fatalf("block %d never stopped; frames = %v", open, frames)
	}
}

func carrierTestTurn() carriedTurn {
	return carriedTurn{Items: []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_order","encrypted_content":"CIPHERTEXT"}`),
	}}
}

// THE INVARIANT, at the level of the state machine: no block opens inside another. emitText
// closes open tool blocks and startToolCall closes the open text block, so those two can
// never overlap on their own. The carrier is the one entry point reachable with either kind
// open, so emitCarriedReasoning closes BOTH before opening its thinking block.
//
// It closed only tool blocks when this test was written. With a text block open it nested
// its thinking block inside, and finish() stopped the outer one afterwards -- [start:0
// start:1 stop:1 stop:0] on the wire, which is not a valid Anthropic stream. This is the
// regression guard for that fix, not a description of it: restore the tools-only close and
// requireSequentialBlocks fails here while the control below still passes.
func TestCarriedReasoningDoesNotOpenItsBlockInsideAnOpenTextBlock(t *testing.T) {
	rec := httptest.NewRecorder()
	state := newAnthropicStreamState(rec, "gpt-public", "msg_order")
	if !state.start() {
		t.Fatal("stream did not start")
	}
	if !state.emitText("thinking out loud") {
		t.Fatal("emitText failed")
	}
	if state.textBlockIndex < 0 {
		t.Fatal("fixture left no text block open; this test would prove nothing")
	}
	if !state.emitCarriedReasoning(carrierTestTurn()) {
		t.Fatal("emitCarriedReasoning failed")
	}
	if !state.finish() {
		t.Fatal("finish failed")
	}
	requireSequentialBlocks(t, rec.Body.String())
}

// The control: a tool call closes the text block on the way in, so the carrier that follows
// opens at the top level. This is the ordering the Responses path actually produces.
func TestCarriedReasoningAfterAToolCallKeepsBlocksSequential(t *testing.T) {
	rec := httptest.NewRecorder()
	state := newAnthropicStreamState(rec, "gpt-public", "msg_order")
	if !state.start() {
		t.Fatal("stream did not start")
	}
	if !state.emitText("thinking out loud") {
		t.Fatal("emitText failed")
	}
	index := 0
	if !state.consumeToolCall(models.OpenAIToolCall{
		Index: &index, ID: "call_vekil_call_ORDER", Type: "function",
		Function: models.OpenAIFunctionCall{Name: "lookup", Arguments: `{"q":"a"}`},
	}) {
		t.Fatal("consumeToolCall failed")
	}
	if state.textBlockIndex >= 0 {
		t.Fatal("tool call left the text block open; the control no longer models the real path")
	}
	if !state.emitCarriedReasoning(carrierTestTurn()) {
		t.Fatal("emitCarriedReasoning failed")
	}
	if !state.finish() {
		t.Fatal("finish failed")
	}
	requireSequentialBlocks(t, rec.Body.String())
}

// Copilot's stated precondition, end to end: a Responses turn whose function_call item is
// followed by a message item carrying text. This is the reachability question -- the
// translation defers every tool chunk to response.completed while text deltas go out as they
// arrive, so the question is whether upstream ordering can survive into the Anthropic stream.
func TestResponsesTurnWithTextAfterItsToolCallKeepsBlocksSequential(t *testing.T) {
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })

	stream, err := prepareResponsesChatStream(context.Background(),
		io.NopCloser(bytes.NewReader([]byte(textAfterToolCallSSE))), responsesChatStreamConfig{
			PublicModel:      "gpt-public",
			ReplayStore:      store,
			ReplayRoute:      responsesChatReplayRoute{ProviderID: "provider", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"},
			PrecommitTimeout: time.Second,
		})
	if err != nil {
		t.Fatalf("prepareResponsesChatStream() error = %v", err)
	}

	rec := httptest.NewRecorder()
	if err := streamChatEventsToAnthropic(rec, stream, "gpt-public", "msg_order"); err != nil {
		t.Fatalf("streamChatEventsToAnthropic() error = %v", err)
	}
	body := rec.Body.String()

	// Guard against passing vacuously: the turn must really have carried text, a tool call
	// and a carrier, or the ordering it claims to exercise never happened.
	for _, required := range []string{`"type":"text"`, `"type":"tool_use"`, "signature_delta", reasoningCarrierPrefix} {
		if !strings.Contains(body, required) {
			t.Fatalf("stream never emitted %s; this test would prove nothing:\n%s", required, body)
		}
	}
	requireSequentialBlocks(t, body)
}

// function_call at output_index 0, then a message with text at output_index 1 -- the exact
// upstream shape the claim names. Shapes copied from stream_one_tool_call.sse and stream_text.sse.
const textAfterToolCallSSE = `event: response.created
data: {"type":"response.created","sequence_number":0,"response":{"id":"resp_order_001","object":"response","created_at":1700000000,"status":"in_progress","error":null,"incomplete_details":null,"model":"gpt-synthetic-responses","output":[],"parallel_tool_calls":true,"usage":null,"metadata":{}}}

event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":1,"output_index":0,"item":{"type":"function_call","id":"fc_order_001","call_id":"call_ORDERaaaaaaaaaaaaaaaaaa","name":"lookup_synthetic_widget","arguments":"","status":"in_progress"}}

event: response.function_call_arguments.delta
data: {"type":"response.function_call_arguments.delta","sequence_number":2,"item_id":"fc_order_001","output_index":0,"delta":"{\"widget\":\"alpha\"}"}

event: response.function_call_arguments.done
data: {"type":"response.function_call_arguments.done","sequence_number":3,"item_id":"fc_order_001","output_index":0,"arguments":"{\"widget\":\"alpha\"}"}

event: response.output_item.done
data: {"type":"response.output_item.done","sequence_number":4,"output_index":0,"item":{"type":"function_call","id":"fc_order_001","call_id":"call_ORDERaaaaaaaaaaaaaaaaaa","name":"lookup_synthetic_widget","arguments":"{\"widget\":\"alpha\"}","status":"completed"}}

event: response.output_item.added
data: {"type":"response.output_item.added","sequence_number":5,"output_index":1,"item":{"type":"message","id":"msg_order_001","status":"in_progress","role":"assistant","phase":"final_answer","content":[]}}

event: response.content_part.added
data: {"type":"response.content_part.added","sequence_number":6,"item_id":"msg_order_001","output_index":1,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":""}}

event: response.output_text.delta
data: {"type":"response.output_text.delta","sequence_number":7,"item_id":"msg_order_001","output_index":1,"content_index":0,"delta":"Looking that up now.","logprobs":[]}

event: response.output_text.done
data: {"type":"response.output_text.done","sequence_number":8,"item_id":"msg_order_001","output_index":1,"content_index":0,"text":"Looking that up now.","logprobs":[]}

event: response.content_part.done
data: {"type":"response.content_part.done","sequence_number":9,"item_id":"msg_order_001","output_index":1,"content_index":0,"part":{"type":"output_text","annotations":[],"logprobs":[],"text":"Looking that up now."}}

event: response.output_item.done
data: {"type":"response.output_item.done","sequence_number":10,"output_index":1,"item":{"type":"message","id":"msg_order_001","status":"completed","role":"assistant","phase":"final_answer","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":"Looking that up now."}]}}

event: response.completed
data: {"type":"response.completed","sequence_number":11,"response":{"id":"resp_order_001","object":"response","created_at":1700000000,"status":"completed","error":null,"incomplete_details":null,"model":"gpt-synthetic-responses","output":[{"type":"function_call","id":"fc_order_001","call_id":"call_ORDERaaaaaaaaaaaaaaaaaa","name":"lookup_synthetic_widget","arguments":"{\"widget\":\"alpha\"}","status":"completed"},{"type":"message","id":"msg_order_001","status":"completed","role":"assistant","phase":"final_answer","content":[{"type":"output_text","annotations":[],"logprobs":[],"text":"Looking that up now."}]}],"parallel_tool_calls":true,"usage":{"input_tokens":20,"input_tokens_details":{"cached_tokens":0},"output_tokens":9,"output_tokens_details":{"reasoning_tokens":4},"total_tokens":29},"metadata":{}}}

`
