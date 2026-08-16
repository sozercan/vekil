package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sozercan/vekil/models"
)

// The live soapbox shape: a parallel tool group whose tool_use blocks and trailing carrier
// land in DIFFERENT assistant wire messages. extractCarriedReasoning indexes a carrier by
// its OWN call mapping, so the carrier IS found here even though no tool_use sits beside it
// -- requiring co-location is the behaviour this branch removed. What refuses it is the next
// guard along: splitting the turn across wire messages changes the assistant projection, so
// carriedRestoredCalls answers "projection", not "absent". Probed, not assumed.
//
// Either way a client cannot repair a transcript it already sent, so the turn must continue
// WITHOUT the reasoning rather than 400. That is the whole assertion, and status alone
// cannot see it: a restored carrier and a degraded turn both return 200 under the same
// upstream call_id, because the minted ID is self-describing. The reasoning ciphertext is
// the only thing that tells them apart, which is why this uses the reasoning fixture.
func TestOrphanedCarrierTurnDegradesInsteadOfWedging(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/stream_reasoning_tool_call.sse")
	if err != nil {
		t.Fatal(err)
	}
	var upstreamBodies []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBodies = append(upstreamBodies, string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()
	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})

	first := httptest.NewRecorder()
	h.HandleAnthropicMessages(first, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"gpt-public","max_tokens":128,"messages":[{"role":"user","content":"run it"}],`+
			`"tools":[{"name":"lookup_synthetic_widget","input_schema":{"type":"object"}}]}`)))
	if first.Code != http.StatusOK {
		t.Fatalf("first turn status = %d body=%s", first.Code, first.Body.String())
	}
	var turn models.AnthropicResponse
	if err := json.Unmarshal(first.Body.Bytes(), &turn); err != nil {
		t.Fatal(err)
	}
	var carrierBlocks, toolBlocks []models.ContentBlock
	var toolUseID string
	for _, block := range turn.Content {
		switch {
		case block.Type == "thinking" && strings.HasPrefix(block.Signature, reasoningCarrierPrefix):
			carrierBlocks = append(carrierBlocks, block)
		case block.Type == "tool_use":
			toolUseID = block.ID
			toolBlocks = append(toolBlocks, block)
		}
	}
	if len(carrierBlocks) == 0 || toolUseID == "" {
		t.Fatalf("fixture turn lacks carrier or tool_use: %s", first.Body.String())
	}

	// What TTL, eviction and restart leave behind.
	h.responsesChatReplayMu.Lock()
	h.responsesChatReplay = nil
	h.responsesChatReplayMu.Unlock()

	// The same blocks, split across wire messages the way a client splits a parallel
	// group whose results interleave: tool_use in its own assistant message, and the
	// carrier trailing alone in an assistant message that holds no tool_use at all.
	continuation, err := json.Marshal(map[string]any{
		"model": "gpt-public", "max_tokens": 128,
		"messages": []any{
			map[string]any{"role": "user", "content": "run it"},
			map[string]any{"role": "assistant", "content": toolBlocks},
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "tool_result", "tool_use_id": toolUseID, "content": "ok"},
			}},
			map[string]any{"role": "assistant", "content": carrierBlocks},
			map[string]any{"role": "user", "content": "and again"},
		},
		"tools": []any{map[string]any{"name": "lookup_synthetic_widget", "input_schema": map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	second := httptest.NewRecorder()
	h.HandleAnthropicMessages(second, httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(string(continuation))))
	if second.Code != http.StatusOK {
		t.Fatalf("orphaned-carrier continuation wedged: status = %d body = %s", second.Code, second.Body.String())
	}
	if len(upstreamBodies) != 2 {
		t.Fatalf("upstream saw %d requests, want 2", len(upstreamBodies))
	}
	// Without this the negative assertion below is vacuous: a fixture carrying no reasoning
	// would satisfy it no matter which path ran.
	if !strings.Contains(string(fixture), orphanFixtureCiphertext) {
		t.Fatalf("fixture carries no reasoning ciphertext; the assertion below would prove nothing")
	}
	if carried := mustDecodeCarrier(t, carrierBlocks[0].Signature); !carriedItemsMention(carried, orphanFixtureCiphertext) {
		t.Fatalf("the emitted carrier holds no reasoning, so this turn had none to lose: %+v", carried.Items)
	}
	if strings.Contains(upstreamBodies[1], orphanFixtureCiphertext) {
		t.Fatalf("a carrier refused as drifted still put its reasoning upstream: %s", upstreamBodies[1])
	}
	// Degraded, not wedged: the call mapping still came back, so the turn continues.
	if !strings.Contains(upstreamBodies[1], toolUseID) && !strings.Contains(upstreamBodies[1], "call_synth_lookup_stream_001") {
		t.Fatalf("continuation lost its call mapping entirely: %s", upstreamBodies[1])
	}
}

const orphanFixtureCiphertext = "synthetic_encrypted_content_not_replayable"

func carriedItemsMention(carried carriedReplay, needle string) bool {
	for _, item := range carried.Items {
		if strings.Contains(string(item), needle) {
			return true
		}
	}
	return false
}
