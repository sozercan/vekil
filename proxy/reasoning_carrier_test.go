package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func sampleReasoningItems() []json.RawMessage {
	return []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_abc","encrypted_content":"OPAQUE-CIPHERTEXT"}`),
		json.RawMessage(`{"type":"function_call","call_id":"call_upstream_1","name":"lookup","arguments":"{}"}`),
	}
}

func TestReasoningCarrierRoundTrip(t *testing.T) {
	items := sampleReasoningItems()
	signature, err := encodeReasoningCarrier(carriedTurn{Items: items})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if !strings.HasPrefix(signature, reasoningCarrierPrefix) {
		t.Fatalf("signature is not version-tagged: %q", signature[:min(20, len(signature))])
	}

	replay := mustDecodeCarrier(t, signature)
	if len(replay.Items) != len(items) {
		t.Fatalf("item count = %d, want %d", len(replay.Items), len(items))
	}
	want := []json.RawMessage{
		items[0],
		json.RawMessage(`{"type":"function_call"}`),
	}
	for i := range want {
		if string(replay.Items[i]) != string(want[i]) {
			t.Fatalf("item %d changed unexpectedly:\n got %s\nwant %s", i, replay.Items[i], want[i])
		}
	}
}

func TestReasoningCarrierEmptyInputProducesNoBlock(t *testing.T) {
	signature, err := encodeReasoningCarrier(carriedTurn{})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if signature != "" {
		t.Fatalf("signature = %q, want empty so the caller omits the block", signature)
	}
	block, err := reasoningCarrierBlock(carriedTurn{})
	if err != nil || block != nil {
		t.Fatalf("block = %v, err = %v; want no block", block, err)
	}
}

func TestReasoningCarrierRejectsGarbageWithoutErroring(t *testing.T) {
	cases := []struct {
		name      string
		signature string
	}{
		{"empty", ""},
		{"foreign prefix (real Anthropic thinking signature)", "ErUBCkYIBxgCKkDd9x2ZQ=="},
		{"our prefix, corrupt base64", reasoningCarrierPrefix + "!!!not-base64!!!"},
		{"our prefix, valid base64 but not deflate", reasoningCarrierPrefix + base64.RawURLEncoding.EncodeToString([]byte("plain"))},
		{"prefix only", reasoningCarrierPrefix},
		{"near-miss prefix", "vekil2.abc"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			replay, ok := decodeReasoningCarrier(tc.signature, nil)
			if ok || replay.Items != nil {
				t.Fatalf("decode accepted %q -> %v", tc.name, replay.Items)
			}
		})
	}
}

func TestReasoningCarrierBoundsDecompression(t *testing.T) {
	carrierOfDecodedSize := func(t *testing.T, delta int) string {
		t.Helper()
		overhead := len(`{"items":[{"type":"reasoning","encrypted_content":""}]}`)
		filler := make([]byte, reasoningCarrierMaxDecodedBytes+delta-overhead)
		if _, err := rand.Read(filler); err != nil {
			t.Fatal(err)
		}
		item, err := json.Marshal(map[string]string{
			"type": "reasoning", "encrypted_content": base64.RawStdEncoding.EncodeToString(filler)[:len(filler)],
		})
		if err != nil {
			t.Fatal(err)
		}
		signature, err := encodeReasoningCarrier(carriedTurn{Items: []json.RawMessage{item}})
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return signature
	}

	if reasoningCarrierMaxDecodedBytes < responsesChatReplayMaxGroupBytes ||
		reasoningCarrierMaxDecodedBytes > reasoningCarrierRequestBudget/2 {
		t.Fatalf("cap = %d bytes, outside [%d, %d]", reasoningCarrierMaxDecodedBytes,
			responsesChatReplayMaxGroupBytes, reasoningCarrierRequestBudget/2)
	}
	if _, ok := decodeReasoningCarrier(carrierOfDecodedSize(t, -256), nil); !ok {
		t.Fatal("decode rejected a carrier just under the cap; a long real turn would wedge")
	}
	if _, ok := decodeReasoningCarrier(carrierOfDecodedSize(t, 256), nil); ok {
		t.Fatal("decode accepted a payload past reasoningCarrierMaxDecodedBytes")
	}
}

func TestReasoningCarrierBlockShape(t *testing.T) {
	block, err := reasoningCarrierBlock(carriedTurn{Items: sampleReasoningItems()})
	if err != nil {
		t.Fatalf("block: %v", err)
	}
	if block.Type != "thinking" {
		t.Fatalf("type = %v, want thinking", block.Type)
	}
	if block.Thinking == nil || *block.Thinking != "" {
		t.Fatalf("thinking = %v, want a present empty string", block.Thinking)
	}
	if !strings.HasPrefix(block.Signature, reasoningCarrierPrefix) {
		t.Fatalf("signature not version-tagged: %q", block.Signature)
	}
}

func TestReasoningCarrierSizeGrowsWithCiphertext(t *testing.T) {
	item := func() json.RawMessage {
		buf := make([]byte, 1572)
		if _, err := rand.Read(buf); err != nil {
			t.Fatal(err)
		}
		return json.RawMessage(`{"type":"reasoning","encrypted_content":"` +
			base64.StdEncoding.EncodeToString(buf) + `"}`)
	}
	one, _ := encodeReasoningCarrier(carriedTurn{Items: []json.RawMessage{item()}})
	ten := make([]json.RawMessage, 0, 10)
	for i := 0; i < 10; i++ {
		ten = append(ten, item())
	}
	many, _ := encodeReasoningCarrier(carriedTurn{Items: ten})

	perItem := len(many) / 10
	if perItem < 1500 || perItem > 3000 {
		t.Fatalf("per-item carrier cost = %d bytes; expected ~2.1 KB for ciphertext. "+
			"If this dropped sharply the fixture stopped being high-entropy and the "+
			"test is measuring nothing.", perItem)
	}
	if len(many) < len(one)*8 {
		t.Fatalf("10 items (%d) did not grow roughly linearly from 1 (%d); "+
			"ciphertext must not be compressing away", len(many), len(one))
	}
	t.Logf("per reasoning item ~%d bytes; 50-turn session would carry ~%d KB",
		perItem, perItem*50/1024)
}

func assistantWithCarrier(t *testing.T, toolUseIDs []string, items []json.RawMessage) models.AnthropicMessage {
	t.Helper()
	blocks := []any{}
	if items != nil {
		block, err := reasoningCarrierBlock(carriedTurn{Items: items})
		if err != nil {
			t.Fatalf("carrier block: %v", err)
		}
		blocks = append(blocks, block)
	}
	for _, id := range toolUseIDs {
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": id, "name": "lookup", "input": map[string]any{},
		})
	}
	content, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return models.AnthropicMessage{Role: "assistant", Content: content}
}

func TestExtractCarriedReasoningKeysEveryToolUseInTheTurn(t *testing.T) {
	items := sampleReasoningItems()
	msgs := []models.AnthropicMessage{
		assistantWithCarrier(t, []string{"call_a", "call_b"}, items),
	}
	carried, _ := extractCarriedReasoning(msgs)
	if len(carried) != 2 {
		t.Fatalf("carried %d ids, want 2", len(carried))
	}
	for _, id := range []string{"call_a", "call_b"} {
		got, ok := carried[id]
		if !ok || string(got.Items[0]) != string(items[0]) {
			t.Fatalf("id %q did not map to the turn's items", id)
		}
	}
}

func TestExtractCarriedReasoningIsPerTurn(t *testing.T) {
	first := []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"turn1"}`)}
	second := []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"turn2"}`)}
	msgs := []models.AnthropicMessage{
		assistantWithCarrier(t, []string{"call_1"}, first),
		assistantWithCarrier(t, []string{"call_2"}, second),
	}
	carried, _ := extractCarriedReasoning(msgs)
	if string(carried["call_1"].Items[0]) != string(first[0]) {
		t.Fatalf("call_1 got the wrong turn: %s", carried["call_1"].Items[0])
	}
	if string(carried["call_2"].Items[0]) != string(second[0]) {
		t.Fatalf("call_2 got the wrong turn: %s", carried["call_2"].Items[0])
	}
}

func TestExtractCarriedReasoningToleratesTranscriptsWithoutCarriers(t *testing.T) {
	cases := []struct {
		name string
		msgs []models.AnthropicMessage
	}{
		{"tool_use with no thinking block", []models.AnthropicMessage{
			assistantWithCarrier(t, []string{"call_legacy"}, nil)}},
		{"string content, no blocks", []models.AnthropicMessage{
			{Role: "assistant", Content: json.RawMessage(`"plain text"`)}}},
		{"user role is never a carrier", []models.AnthropicMessage{
			{Role: "user", Content: json.RawMessage(`[{"type":"text","text":"hi"}]`)}}},
		{"foreign thinking signature", []models.AnthropicMessage{
			{Role: "assistant", Content: json.RawMessage(
				`[{"type":"thinking","thinking":"","signature":"ErUBCkYIBxgC"},` +
					`{"type":"tool_use","id":"call_x","name":"f","input":{}}]`)}}},
		{"empty", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if carried, _ := extractCarriedReasoning(tc.msgs); carried != nil {
				t.Fatalf("expected no carrier, got %v", carried)
			}
		})
	}
}

func TestExtractCarriedReasoningIgnoresCarrierWithoutToolUse(t *testing.T) {
	block := func(t *testing.T, id string) *models.ContentBlock {
		t.Helper()
		b, err := reasoningCarrierBlock(carriedTurn{
			Items: []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"` + id + `"}`)},
		})
		if err != nil {
			t.Fatalf("carrier block: %v", err)
		}
		return b
	}
	bare, err := json.Marshal([]any{block(t, "spliced")})
	if err != nil {
		t.Fatal(err)
	}
	turn, err := json.Marshal([]any{block(t, "turn"), toolUseBlock(mintedCallID(t))})
	if err != nil {
		t.Fatal(err)
	}

	carried, _ := extractCarriedReasoning([]models.AnthropicMessage{
		{Role: "assistant", Content: turn},
		{Role: "assistant", Content: bare},
	})
	if len(carried) != 1 {
		t.Fatalf("carried %d ids, want only the tool-call turn's", len(carried))
	}
	for id, replay := range carried {
		if !strings.Contains(string(replay.Items[0]), `"turn"`) {
			t.Fatalf("id %q got the bare block's items: %s", id, replay.Items[0])
		}
	}
}

func TestCarrierRouteDigestRejectsAnotherRoute(t *testing.T) {
	minted := responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public", UpstreamModel: "gpt-5.6-sol", RouteID: "sol-route", PolicyTier: "powerful"}
	other := minted
	other.RouteID, other.UpstreamModel, other.PolicyTier = "luna-route", "gpt-5.6-luna", "lightweight"

	projected := []responsesChatReplayProjectedCall{{ID: "call_vekil_x", Name: "lookup", Arguments: "{}"}}
	content := json.RawMessage(`"checking"`)
	signature, err := encodeReasoningCarrier(carriedTurn{
		Items:      sampleReasoningItems(),
		Calls:      []carriedCall{{ProxyID: "call_vekil_x", UpstreamID: "call_upstream_1", Name: "lookup", ItemIndex: 1}},
		Route:      minted,
		Projection: carriedProjectionDigest(content, projected),
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	replay := mustDecodeCarrier(t, signature)
	carried := map[string]carriedReplay{"call_vekil_x": replay}

	if _, reason := carriedRestoredCalls(carried, projected, minted, content); reason != "" {
		t.Fatal("the minting route did not restore its own carrier")
	}
	if _, reason := carriedRestoredCalls(carried, projected, other, content); reason == "" {
		t.Fatal("a different route restored the carrier, so nothing binds it to its model or tier")
	}
}

func TestCarrierBlockKeepsThinkingOnTheWire(t *testing.T) {
	block, err := reasoningCarrierBlock(carriedTurn{Items: sampleReasoningItems()})
	if err != nil {
		t.Fatalf("block: %v", err)
	}
	encoded, err := json.Marshal(block)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &wire); err != nil {
		t.Fatal(err)
	}
	raw, present := wire["thinking"]
	if !present {
		t.Fatalf("no thinking key on the wire; clients dereference it:\n%s", encoded)
	}
	var thinking string
	if json.Unmarshal(raw, &thinking) != nil || thinking != "" {
		t.Fatalf("thinking = %s, want an empty string", raw)
	}
}

func TestNonCarrierBlocksOmitThinkingOnTheWire(t *testing.T) {
	text := "hello"
	for _, block := range []models.ContentBlock{
		{Type: "text", Text: &text},
		{Type: "tool_use", ID: "toolu_1", Name: "lookup", Input: json.RawMessage(`{}`)},
	} {
		encoded, err := json.Marshal(block)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(encoded), `"thinking"`) {
			t.Fatalf("%s block gained a thinking field: %s", block.Type, encoded)
		}
	}
}

func TestCarrierBudgetSpendsOnTheTurnsThatSurviveTheTrim(t *testing.T) {
	route := responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public"}
	message := func(index int) models.AnthropicMessage {
		id := "call_vekil_budget" + string(rune('a'+index))
		items := []json.RawMessage{
			json.RawMessage(`{"type":"reasoning","encrypted_content":"` + strings.Repeat("A", 900_000) + `"}`),
			json.RawMessage(`{"type":"function_call","call_id":"upstream-1","name":"lookup","arguments":"{}"}`),
		}
		signature, err := encodeReasoningCarrier(carriedTurn{
			Items: items, Route: route,
			Calls: []carriedCall{{ProxyID: id, UpstreamID: "upstream-1", Name: "lookup", ItemIndex: 1}},
		})
		if err != nil {
			t.Fatal(err)
		}
		blocks, err := json.Marshal([]models.ContentBlock{
			{Type: "thinking", Thinking: stringPtr(""), Signature: signature},
			{Type: "tool_use", ID: id, Name: "lookup", Input: json.RawMessage(`{}`)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return models.AnthropicMessage{Role: "assistant", Content: blocks}
	}
	var messages []models.AnthropicMessage
	for i := 0; i < 12; i++ {
		messages = append(messages, message(i))
	}

	carried, inbound := extractCarriedReasoning(messages[:2])
	if inbound.Starved || len(carried) != 2 {
		t.Fatalf("a short transcript must fit: starved = %v, carriers = %d", inbound.Starved, len(carried))
	}
	carried, inbound = extractCarriedReasoning(messages)
	if !inbound.Starved {
		t.Fatal("crossing the budget must be reported, not silent")
	}
	if _, ok := carried["call_vekil_budgetl"]; !ok {
		t.Fatal("the newest turn is charged first, so it must have survived")
	}
	if _, ok := carried["call_vekil_budgeta"]; ok {
		t.Fatal("the oldest turn cannot have been decoded past an exhausted budget")
	}
}

func TestCarrierDecodesEveryGroupTheStoreWillPublish(t *testing.T) {
	if reasoningCarrierMaxDecodedBytes <= responsesChatReplayMaxGroupBytes {
		t.Fatalf("decode cap %d does not cover a publishable group of %d",
			reasoningCarrierMaxDecodedBytes, responsesChatReplayMaxGroupBytes)
	}

	ciphertext := make([]byte, responsesChatReplayMaxGroupBytes)
	for i := range ciphertext {
		ciphertext[i] = byte(i*31 + i/251)
	}
	item := json.RawMessage(`{"type":"reasoning","encrypted_content":"` +
		base64.RawURLEncoding.EncodeToString(ciphertext) + `"}`)

	signature, err := encodeReasoningCarrier(carriedTurn{
		Items: []json.RawMessage{item},
		Calls: []carriedCall{{ProxyID: "call_vekil_a", UpstreamID: "fc_a", Name: "Read", ItemIndex: 0}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	replay := mustDecodeCarrier(t, signature)
	if call, ok := replay.Calls["call_vekil_a"]; !ok || call.UpstreamID != "" {
		t.Fatalf("opaque call binding lost or exposed a provider ID: %+v", replay.Calls)
	}
}

func TestCarrierPastTheCapIsNotEmitted(t *testing.T) {
	ciphertext := make([]byte, reasoningCarrierMaxDecodedBytes)
	for i := range ciphertext {
		ciphertext[i] = byte(i*17 + i/97)
	}
	oversized := json.RawMessage(`{"type":"reasoning","encrypted_content":"` +
		base64.RawURLEncoding.EncodeToString(ciphertext) + `"}`)

	signature, err := encodeReasoningCarrier(carriedTurn{
		Items: []json.RawMessage{oversized, oversized},
		Calls: []carriedCall{{ProxyID: "call_vekil_b", UpstreamID: "fc_b", Name: "Bash", ItemIndex: 0}},
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if signature != "" {
		t.Fatalf("oversized carrier was emitted: %d bytes", len(signature))
	}
}

func newCarrierReplayFixture(t *testing.T) (*responsesChatReplayStore, responsesChatReplayRoute) {
	t.Helper()
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	return store, responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
}

func mustDecodeCarrier(t *testing.T, signature string) carriedReplay {
	t.Helper()
	decoded, ok := decodeReasoningCarrier(signature, nil)
	if !ok {
		t.Fatalf("carrier does not decode: %.40q", signature)
	}
	return decoded
}

func TestCarrierExhaustingTheBudgetSaysSoEvenWhenItIsLast(t *testing.T) {
	route := responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public"}
	message := func(index int) models.AnthropicMessage {
		id := "call_vekil_exhaust" + string(rune('a'+index))
		items := []json.RawMessage{
			json.RawMessage(`{"type":"reasoning","encrypted_content":"` + strings.Repeat("A", 900_000) + `"}`),
			json.RawMessage(`{"type":"function_call","call_id":"upstream-1","name":"lookup","arguments":"{}"}`),
		}
		signature, err := encodeReasoningCarrier(carriedTurn{
			Items: items, Route: route,
			Calls: []carriedCall{{ProxyID: id, UpstreamID: "upstream-1", Name: "lookup", ItemIndex: 1}},
		})
		if err != nil {
			t.Fatal(err)
		}
		blocks, err := json.Marshal([]models.ContentBlock{
			{Type: "thinking", Thinking: stringPtr(""), Signature: signature},
			{Type: "tool_use", ID: id, Name: "lookup", Input: json.RawMessage(`{}`)},
		})
		if err != nil {
			t.Fatal(err)
		}
		return models.AnthropicMessage{Role: "assistant", Content: blocks}
	}

	var messages []models.AnthropicMessage
	for i := 0; i < 10; i++ {
		messages = append(messages, message(i))
	}

	_, inbound := extractCarriedReasoning(messages)
	if !inbound.Starved {
		t.Fatal("a transcript that exhausts the decode budget reported no starvation")
	}
}

func TestCarrierWedgeNamesTheGuardThatRejectedIt(t *testing.T) {
	store, route := newCarrierReplayFixture(t)
	var logs bytes.Buffer
	options := responsesChatRequestOptions{
		Log:              logger.NewWithWriter(logger.LevelWarn, &logs),
		ReplayStore:      store,
		ReplayRoute:      route,
		CarriedReasoning: nil,
	}
	projected := []responsesChatReplayProjectedCall{
		{ID: "call_vekil_absent", Name: "Read", Arguments: `{"file_path":"/etc/secret-path"}`},
	}

	logResponsesChatCarrierWedge(options, projected, "absent")

	out := logs.String()
	if !strings.Contains(out, `"carrier":"absent"`) {
		t.Fatalf("wedge log does not name the guard: %s", out)
	}
	if !strings.Contains(out, `"tool_calls":1`) {
		t.Fatalf("wedge log lost the call count: %s", out)
	}
	for _, leak := range []string{"secret-path", "file_path", "call_vekil_absent"} {
		if strings.Contains(out, leak) {
			t.Fatalf("wedge log leaked request content %q: %s", leak, out)
		}
	}
}

func TestOrphanedCarrierStillResolvesTheCallsItNames(t *testing.T) {
	route := responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
	signature, err := encodeReasoningCarrier(carriedTurn{
		Items: []json.RawMessage{json.RawMessage(`{"type":"function_call","call_id":"upstream-1","name":"TaskOutput","arguments":"{}"}`)},
		Calls: []carriedCall{{ProxyID: "call_vekil_orphan", UpstreamID: "upstream-1", Name: "TaskOutput"}},
		Route: route,
	})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}

	toolOnly, err := json.Marshal([]models.ContentBlock{
		{Type: "tool_use", ID: "call_vekil_orphan", Name: "TaskOutput", Input: json.RawMessage(`{}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	carrierOnly, err := json.Marshal([]models.ContentBlock{
		{Type: "thinking", Thinking: stringPtr(""), Signature: signature},
	})
	if err != nil {
		t.Fatal(err)
	}

	carried, _ := extractCarriedReasoning([]models.AnthropicMessage{
		{Role: "assistant", Content: toolOnly},
		{Role: "assistant", Content: carrierOnly},
	})

	replay, ok := carried["call_vekil_orphan"]
	if !ok {
		t.Fatal("the orphaned carrier is unreachable, so this turn wedges")
	}
	if call := replay.Calls["call_vekil_orphan"]; call.ProxyID != "call_vekil_orphan" || call.UpstreamID != "" {
		t.Fatalf("opaque binding lost or provider mapping leaked: %+v", replay.Calls)
	}
}

func TestCarriedRestoreKeepsTextAfterACallThatPrecededIt(t *testing.T) {
	call := json.RawMessage(`{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup","arguments":"{}","status":"completed"}`)
	message := json.RawMessage(`{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"spoken"}]}`)

	for _, tc := range []struct {
		name      string
		items     []json.RawMessage
		wantFirst string
	}{
		{"call then message", []json.RawMessage{call, message}, "call"},
		{"message then call", []json.RawMessage{message, call}, "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			callIndex := -1
			sawMessage := false
			for i, item := range tc.items {
				switch itemType, _ := carriedItemHeader(item); itemType {
				case "function_call":
					callIndex = i
				case "message":
					sawMessage = true
				}
			}
			if callIndex < 0 || !sawMessage {
				t.Fatalf("fixture lacks a call (%d) or a message (%v); this test would prove nothing", callIndex, sawMessage)
			}

			out := reconstructCarriedRestore(
				responsesChatRestoredCalls{
					OutputItems: tc.items,
					Calls: []responsesChatReplayResolvedCall{{
						ProxyCallID: "call_vekil_x", UpstreamCallID: "call-1", OutputItemIndex: callIndex,
					}},
				},
				[]responsesChatReplayProjectedCall{{ID: "call_vekil_x", Name: "lookup", Arguments: "{}"}},
				"spoken",
			)

			gotFirst := ""
			for _, item := range out.OutputItems {
				itemType, _ := carriedItemHeader(item)
				if itemType == "function_call" {
					gotFirst = "call"
				} else if strings.Contains(string(item), "spoken") {
					gotFirst = "text"
				}
				if gotFirst != "" {
					break
				}
			}
			if gotFirst != tc.wantFirst {
				t.Fatalf("replay leads with %q, want %q: upstream order was not preserved in %s",
					gotFirst, tc.wantFirst, string(mustJSON(t, out.OutputItems)))
			}
		})
	}
}

func TestCarriedRestorePreservesMultipleMessageItemBoundaries(t *testing.T) {
	items := []json.RawMessage{
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"before "}]}`),
		json.RawMessage(`{"type":"reasoning","id":"rs_between","encrypted_content":"OPAQUE"}`),
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"after"}]}`),
		json.RawMessage(`{"type":"function_call","call_id":"upstream-1","name":"lookup","arguments":"{}"}`),
	}
	signature, err := encodeReasoningCarrier(carriedTurn{Items: items})
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	replay := mustDecodeCarrier(t, signature)
	if !carriedItemsWellShaped(replay.Items) {
		t.Fatal("decoder rejected the multiple message placeholders emitted by the carrier")
	}

	out := reconstructCarriedRestore(
		responsesChatRestoredCalls{
			OutputItems: replay.Items,
			Calls: []responsesChatReplayResolvedCall{{
				ProxyCallID: "call_vekil_x", UpstreamCallID: "upstream-1", OutputItemIndex: 3,
			}},
		},
		[]responsesChatReplayProjectedCall{{ID: "call_vekil_x", Name: "lookup", Arguments: "{}"}},
		"before after",
	)
	if len(out.OutputItems) != 4 {
		t.Fatalf("restored items = %d, want 4: %s", len(out.OutputItems), mustJSON(t, out.OutputItems))
	}
	wantFragments := []string{`"content":"before "`, `"encrypted_content":"OPAQUE"`, `"content":"after"`, `"call_id":"upstream-1"`}
	for i, fragment := range wantFragments {
		if !strings.Contains(string(out.OutputItems[i]), fragment) {
			t.Fatalf("item %d = %s, want fragment %s", i, out.OutputItems[i], fragment)
		}
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
