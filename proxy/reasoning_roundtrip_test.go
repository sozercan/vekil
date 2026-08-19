package proxy

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/vekil/models"
)

// What vekil emits on turn N must preserve the opaque reasoning fields and ordering
// placeholders needed on turn N+1 while stripping every field the transcript rebuilds.
func TestCarrierSurvivesAFullTurnRoundTrip(t *testing.T) {
	outputItems := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"CIPHERTEXT","content":[],"summary":[]}`),
		json.RawMessage(`{"type":"function_call","call_id":"call_upstream_7","name":"lookup","arguments":"{}"}`),
	}

	mintedID := mintedCallID(t)
	resp := prependCarriedReasoning(&models.AnthropicResponse{
		Content: []models.ContentBlock{
			{Type: "tool_use", ID: mintedID, Name: "lookup", Input: json.RawMessage(`{}`)},
		},
	}, carriedTurn{Items: outputItems})
	if len(resp.Content) != 2 || resp.Content[0].Type != "thinking" {
		t.Fatalf("carrier is not the leading block: %+v", resp.Content)
	}

	replayed, err := json.Marshal(resp.Content)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	carried, _ := extractCarriedReasoning([]models.AnthropicMessage{
		{Role: "assistant", Content: replayed},
	})

	got, ok := carried[mintedID]
	if !ok {
		t.Fatal("turn N+1 could not recover the carrier vekil emitted on turn N")
	}
	wantItems := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"CIPHERTEXT"}`),
		json.RawMessage(`{"type":"function_call","call_id":"call_upstream_7","name":"lookup"}`),
	}
	for i := range wantItems {
		if string(got.Items[i]) != string(wantItems[i]) {
			t.Fatalf("item %d changed in transit:\n got %s\nwant %s", i, got.Items[i], wantItems[i])
		}
	}
}

// The carrier keys on whatever vekil put on the tool_use block -- the MINTED id,
// not Copilot's. Both mechanisms hang off that one id, so either can answer.
func TestCarrierKeysOnTheMintedToolUseID(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()
	published, err := store.Publish(responsesChatReplayPublishRequest{
		Route:            responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"},
		AssistantContent: json.RawMessage(`""`),
		OutputItems: []json.RawMessage{
			json.RawMessage(`{"type":"function_call","call_id":"call_upstream_7","name":"lookup","arguments":"{}","status":"completed"}`),
		},
		Calls: []responsesChatReplayPublishCall{{UpstreamCallID: "call_upstream_7", Name: "lookup", VisibleArguments: `{}`, OutputItemIndex: 0}},
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	mintedID := published.Projection.Calls[0].ID
	if mintedID == "call_upstream_7" {
		t.Fatal("the store stopped minting, so this test proves nothing")
	}

	items := []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"rs_1"}`)}
	resp := prependCarriedReasoning(&models.AnthropicResponse{
		Content: []models.ContentBlock{{Type: "tool_use", ID: mintedID}},
	}, carriedTurn{Items: items})
	replayed, _ := json.Marshal(resp.Content)
	carried, _ := extractCarriedReasoning([]models.AnthropicMessage{{Role: "assistant", Content: replayed}})
	if _, ok := carried[mintedID]; !ok {
		t.Fatalf("minted call id did not key the carrier: %v", carried)
	}
}

func TestPrependCarriedReasoningIsNoOpWithoutItems(t *testing.T) {
	original := &models.AnthropicResponse{Content: []models.ContentBlock{{Type: "text"}}}
	if got := prependCarriedReasoning(original, carriedTurn{}); len(got.Content) != 1 {
		t.Fatalf("added a block with nothing to carry: %+v", got.Content)
	}
	if prependCarriedReasoning(nil, carriedTurn{Items: []json.RawMessage{json.RawMessage(`{}`)}}) != nil {
		t.Fatal("nil response should stay nil", nil)
	}
}

// Clients build a thinking block from its deltas: a signature on the start frame is lost.
func TestCarriedReasoningStreamsSignatureAsDelta(t *testing.T) {
	rec := httptest.NewRecorder()
	state := newAnthropicStreamState(rec, "gpt-public", "msg_test")
	if !state.start() {
		t.Fatal("stream did not start")
	}
	items := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"CIPHERTEXT"}`),
	}
	if !state.emitCarriedReasoning(carriedTurn{Items: items}) {
		t.Fatal("emitCarriedReasoning failed")
	}

	body := rec.Body.String()
	if !strings.Contains(body, "signature_delta") {
		t.Fatalf("no signature_delta frame; the client will drop the carrier:\n%s", body)
	}
	if !strings.Contains(body, reasoningCarrierPrefix) {
		t.Fatalf("carrier payload never reached the wire:\n%s", body)
	}

	replay, ok := decodeReasoningCarrier(carrierSignatureFromStream(t, body), nil)
	if !ok || len(replay.Items) != 1 || string(replay.Items[0]) != string(items[0]) {
		t.Fatalf("carrier did not survive the stream: ok=%v decoded=%v", ok, replay.Items)
	}
}
