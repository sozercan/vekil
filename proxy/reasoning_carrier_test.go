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
	// encrypted_content stays exact; visible items keep only reconstruction metadata.
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

// The decoder must never fail the request: a signature may be foreign, stale,
// truncated or hostile, and rejecting would recreate the wedge.
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

// A signature is attacker-reachable, so an inflate bomb is too. Over-cap carriers
// are dropped, which after a restart is the wedge again: pin the cap itself.
func TestReasoningCarrierBoundsDecompression(t *testing.T) {
	// Random filler, so flate cannot shrink it into a different test.
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

	// Bounded by what it protects, not by a number that drifts with it: at least a group
	// the store will publish, or we mint carriers we reject; and well under the request
	// budget, which is what actually bounds a decompression bomb.
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
	// Empty thinking text is deliberate: the carrier shows the user nothing new.
	if block.Thinking == nil || *block.Thinking != "" {
		t.Fatalf("thinking = %v, want a present empty string", block.Thinking)
	}
	if !strings.HasPrefix(block.Signature, reasoningCarrierPrefix) {
		t.Fatalf("signature not version-tagged: %q", block.Signature)
	}
}

// Compression does not rescue size -- encrypted_content is ciphertext -- and the
// signature rides in every later request, so pin the growth RATE, not a total.
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

// Absence, not an error: that is what lets the caller fall through to the store.
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

// A bare thinking block never replayed a turn, so its items must not escape.
// Paired with a real turn, or the "no carrier" early return masks it.
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

// The digest binds a carrier to the route that minted it: Copilot's ciphertext is
// model-bound, and it is also how a continuation is matched back to its own tier.
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

// Assert on the MARSHALLED BYTES. A struct round-trip cannot see a field that
// omitempty deleted on the way out, which is how {"type":"thinking",
// "signature":...} shipped and killed clients on i.thinking.length.
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

// Non-carrier blocks must NOT gain the field: Anthropic does not send
// `thinking` on a text block, which is why this is a pointer rather than
// dropping omitempty.
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

// The budget is spent newest-first, so a long transcript drops its OLDEST carriers. That is
// the right end to drop from twice over: the newest turns are the ones whose store group is
// most likely still live, and trimAgedReasoning discards the oldest turns' reasoning on the
// way out anyway -- spending the budget there paid for bytes that never left the process.
// Crossing the budget is still reported rather than silent.
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

// A group the store accepts must mint a carrier the decoder accepts. When these drifted
// apart, a 1-2 MiB turn published fine, minted a carrier vekil itself rejected, and
// wedged once the store forgot it -- the exact failure the carrier exists to prevent.
func TestCarrierDecodesEveryGroupTheStoreWillPublish(t *testing.T) {
	if reasoningCarrierMaxDecodedBytes <= responsesChatReplayMaxGroupBytes {
		t.Fatalf("decode cap %d does not cover a publishable group of %d",
			reasoningCarrierMaxDecodedBytes, responsesChatReplayMaxGroupBytes)
	}

	// A turn at the store's ceiling, incompressible so the cap is exercised on real bytes.
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

// A payload beyond the decoder's safety cap is omitted rather than replaced with a
// client-held provider mapping.
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

// The store and route nearly every carrier test needs. Three call sites had already drifted
// apart before this existed; a shared fixture is how they stay comparable.
func newCarrierReplayFixture(t *testing.T) (*responsesChatReplayStore, responsesChatReplayRoute) {
	t.Helper()
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	return store, responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
}

// decodeReasoningCarrier never errors -- it returns false -- so an unchecked call reads as
// success and asserts on a zero value. This makes the check impossible to forget.
func mustDecodeCarrier(t *testing.T, signature string) carriedReplay {
	t.Helper()
	decoded, ok := decodeReasoningCarrier(signature, nil)
	if !ok {
		t.Fatalf("carrier does not decode: %.40q", signature)
	}
	return decoded
}

// The budget is charged before the decode fails, so the carrier that exhausts it never
// reached the pre-attempt check that sets Starved -- and if it is the LAST carrier, no
// later iteration sets it either. Continuity is then lost with no warning at all.
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

	// The count is load-bearing, in BOTH directions. Below 10 the budget is never
	// exhausted and nothing starves; at 11 it is exhausted BEFORE the last carrier, so
	// the pre-attempt check sets the flag and this test would pass with the bug back.
	// Only 10 lands the exhaustion ON the final carrier, which is the gap.
	var messages []models.AnthropicMessage
	for i := 0; i < 10; i++ {
		messages = append(messages, message(i))
	}

	_, inbound := extractCarriedReasoning(messages)
	if !inbound.Starved {
		t.Fatal("a transcript that exhausts the decode budget reported no starvation")
	}
}

// The wedge arrived in production with no way to tell WHICH guard rejected the carrier:
// the reason was computed and dropped. It must name the guard, and never carry content.
func TestCarrierWedgeNamesTheGuardThatRejectedIt(t *testing.T) {
	store, route := newCarrierReplayFixture(t)
	var logs bytes.Buffer
	options := responsesChatRequestOptions{
		Log:         logger.NewWithWriter(logger.LevelWarn, &logs),
		ReplayStore: store,
		ReplayRoute: route,
		// Deliberately empty: the client sent no carrier for these calls at all.
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
	// The projection is prompt data; only vekil's own enumerations may be logged.
	for _, leak := range []string{"secret-path", "file_path", "call_vekil_absent"} {
		if strings.Contains(out, leak) {
			t.Fatalf("wedge log leaked request content %q: %s", leak, out)
		}
	}
}

// Interleaved parallel calls split one logical turn across several wire messages, and the
// carrier can land in a message holding no tool_use at all. Requiring co-location discarded
// it, leaving those ids unresolvable -- measured on a real 1190-message session where 10 of
// 711 ids were orphaned this way and wedged the conversation permanently.
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

	// The tool_use rides in ONE assistant message; the carrier lands in a LATER assistant
	// message with no tool_use beside it -- exactly the interleaved-parallel shape.
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

// A turn can call a tool and THEN speak. The flattened text has one slot to go back into, so
// it goes where the turn first spoke -- and that must be the message, not the call, or the
// replay hoists the text above a call upstream had already emitted and inverts the turn.
func TestCarriedRestoreKeepsTextAfterACallThatPrecededIt(t *testing.T) {
	call := json.RawMessage(`{"type":"function_call","id":"fc-1","call_id":"call-1","name":"lookup","arguments":"{}","status":"completed"}`)
	message := json.RawMessage(`{"type":"message","id":"msg-1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"spoken"}]}`)

	for _, tc := range []struct {
		name      string
		items     []json.RawMessage
		wantFirst string // "call" or "text": whichever upstream emitted first
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

			// The rebuilt text item carries no "type", so it is identified by its content.
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
