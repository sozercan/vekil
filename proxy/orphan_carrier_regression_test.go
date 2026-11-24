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

	h.responsesChatReplayMu.Lock()
	h.responsesChatReplay = nil
	h.responsesChatReplayMu.Unlock()

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
	if !strings.Contains(string(fixture), orphanFixtureCiphertext) {
		t.Fatalf("fixture carries no reasoning ciphertext; the assertion below would prove nothing")
	}
	if carried := mustDecodeCarrier(t, carrierBlocks[0].Signature); !carriedItemsMention(carried, orphanFixtureCiphertext) {
		t.Fatalf("the emitted carrier holds no reasoning, so this turn had none to lose: %+v", carried.Items)
	}
	if strings.Contains(upstreamBodies[1], orphanFixtureCiphertext) {
		t.Fatalf("a carrier refused as drifted still put its reasoning upstream: %s", upstreamBodies[1])
	}
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
