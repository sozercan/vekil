package proxy

import (
	"bytes"
	"compress/flate"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

const carrierCapCiphertext = "OPAQUE-CIPHERTEXT"

// The reasoning ciphertext stream_reasoning_tool_call.sse ships alongside its tool call.
const carrierFixtureCiphertext = "synthetic_encrypted_content_not_replayable"

// capFixture publishes one tool turn and returns the route, its output items and the
// publication, i.e. everything the emit side holds when it mints a carrier.
func capFixture(t *testing.T, store *responsesChatReplayStore) (responsesChatReplayRoute, []json.RawMessage, responsesChatReplayPublished) {
	t.Helper()
	route := responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
	items := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_cap","encrypted_content":"` + carrierCapCiphertext + `","content":[],"summary":[]}`),
		responsesFunctionCallItem("upstream-call-1", "lookup", `{"q":"secret-prompt-text"}`),
	}
	published, err := store.Publish(responsesChatReplayPublishRequest{
		Route:            route,
		AssistantContent: json.RawMessage(`"checking"`),
		OutputItems:      items,
		Calls: []responsesChatReplayPublishCall{{
			UpstreamCallID: "upstream-call-1", Name: "lookup", VisibleArguments: `{"q":"secret-prompt-text"}`, OutputItemIndex: 1,
		}},
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	return route, items, published
}

func atBudget() carrierEmit {
	return carrierEmit{Inbound: carrierInbound{Carriers: 291, Bytes: reasoningCarrierInboundBudget}}
}

// carrierPayloadBytes inflates a signature back to the bytes that were marshalled into
// it. Everything asserted about a capped carrier is asserted on these, not on a struct.
func carrierPayloadBytes(t *testing.T, signature string) []byte {
	t.Helper()
	if !strings.HasPrefix(signature, reasoningCarrierPrefix) {
		t.Fatalf("signature is not a carrier: %q", signature)
	}
	compressed, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(signature, reasoningCarrierPrefix))
	if err != nil {
		t.Fatal(err)
	}
	reader := flate.NewReader(bytes.NewReader(compressed))
	defer func() { _ = reader.Close() }()
	payload, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

// emittedSignature mints through the client-facing path, so what is asserted is the
// signature the client would actually store, read off the marshalled response.
func emittedSignature(t *testing.T, turn carriedTurn) string {
	t.Helper()
	resp := prependCarriedReasoning(&models.AnthropicResponse{
		Content: []models.ContentBlock{{Type: "tool_use", ID: "call-1", Name: "lookup"}},
	}, turn)
	encoded, err := json.Marshal(resp.Content)
	if err != nil {
		t.Fatal(err)
	}
	var blocks []models.ContentBlock
	if err := json.Unmarshal(encoded, &blocks); err != nil {
		t.Fatal(err)
	}
	if len(blocks) == 0 || blocks[0].Type != "thinking" {
		t.Fatalf("no carrier block was emitted: %s", encoded)
	}
	return blocks[0].Signature
}

// Past the budget the carrier is the id mapping and nothing else: the reasoning
// ciphertext is the whole of what it stops carrying.
func TestCarrierPastBudgetKeepsOnlyTheIdMapping(t *testing.T) {
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	route, items, published := capFixture(t, store)

	payload := string(carrierPayloadBytes(t, emittedSignature(t, carriedTurnFromPublished(route, items, published, atBudget()))))
	for _, forbidden := range []string{carrierCapCiphertext, "encrypted_content", `"type":"reasoning"`, "secret-prompt-text"} {
		if strings.Contains(payload, forbidden) {
			t.Fatalf("capped carrier still carries %q: %s", forbidden, payload)
		}
	}
	for _, want := range []string{`"upstream_id":"upstream-call-1"`, `"name":"lookup"`, `"proxy_id":"` + published.Projection.Calls[0].ID + `"`} {
		if !strings.Contains(payload, want) {
			t.Fatalf("capped carrier dropped the mapping field %s: %s", want, payload)
		}
	}
	var decoded reasoningCarrierPayload
	if err := json.Unmarshal([]byte(payload), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Items) != 0 {
		t.Fatalf("capped carrier carried %d items, want none: %s", len(decoded.Items), payload)
	}
	if len(decoded.Calls) != 1 || decoded.Calls[0].ItemIndex != 1 {
		t.Fatalf("capped carrier lost the call ordering: %#v", decoded.Calls)
	}
	if decoded.ProjectionDigest == "" || decoded.RouteDigest == "" || decoded.RouteTag == "" {
		t.Fatalf("capped carrier dropped a binding the restore checks: %s", payload)
	}
}

// The wedge this prevents: the store forgot the group, and without a usable carrier the
// turn cannot be rebuilt at all. The mapping alone has to be enough.
func TestMappingOnlyCarrierRebuildsAForgottenTurn(t *testing.T) {
	minting := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = minting.Close() })
	route, items, published := capFixture(t, minting)
	callID := published.Projection.Calls[0].ID

	signature := emittedSignature(t, carriedTurnFromPublished(route, items, published, atBudget()))
	blocks, err := json.Marshal([]models.ContentBlock{
		{Type: "thinking", Thinking: stringPtr(""), Signature: signature},
		{Type: "tool_use", ID: callID, Name: "lookup", Input: json.RawMessage(`{"q":"secret-prompt-text"}`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	carried, _ := extractCarriedReasoning([]models.AnthropicMessage{{Role: "assistant", Content: blocks}})
	if _, ok := carried[callID]; !ok {
		t.Fatal("a mapping-only carrier was discarded on the way back in")
	}

	body, err := json.Marshal(map[string]any{
		"model": "gpt-public",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "checking", "tool_calls": []any{map[string]any{
				"id": callID, "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"secret-prompt-text"}`},
			}}},
			map[string]any{"role": "tool", "tool_call_id": callID, "content": "result-1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// A store that never saw the group is exactly the state that used to wedge.
	forgotten := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = forgotten.Close() })
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
		UpstreamModel: "gpt-upstream", ReplayStore: forgotten, ReplayRoute: route, CarriedReasoning: carried,
	})
	if err != nil {
		t.Fatalf("a forgotten group wedged instead of rebuilding from the mapping: %v", err)
	}

	input := upstreamInputJSON(t, plan)
	var restored []map[string]any
	if err := json.Unmarshal([]byte(input), &restored); err != nil {
		t.Fatal(err)
	}
	var call, output map[string]any
	for _, item := range restored {
		switch item["type"] {
		case "function_call":
			call = item
		case "function_call_output":
			output = item
		}
	}
	if call == nil || output == nil {
		t.Fatalf("rebuilt turn lost its mandatory call/result pair: %s", input)
	}
	if call["call_id"] != "upstream-call-1" || output["call_id"] != "upstream-call-1" {
		t.Fatalf("rebuilt turn lost the minted upstream binding: %s", input)
	}
	if call["name"] != "lookup" || call["arguments"] != `{"q":"secret-prompt-text"}` {
		t.Fatalf("rebuilt call = %#v, want the visible tool call", call)
	}
	if !strings.Contains(input, "checking") {
		t.Fatalf("rebuilt turn dropped the visible assistant text: %s", input)
	}
	if strings.Contains(input, carrierCapCiphertext) {
		t.Fatalf("a capped carrier must not resurrect reasoning: %s", input)
	}
}

func TestMappingOnlyCarrierPreservesTextAfterCallOrder(t *testing.T) {
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	route := responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
	items := []json.RawMessage{
		responsesFunctionCallItem("call_upstream_order", "lookup", `{}`),
		json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"spoken"}]}`),
	}
	published, err := store.Publish(responsesChatReplayPublishRequest{
		Route:            route,
		AssistantContent: json.RawMessage(`"spoken"`),
		OutputItems:      items,
		Calls: []responsesChatReplayPublishCall{{
			UpstreamCallID: "call_upstream_order", Name: "lookup", VisibleArguments: `{}`, OutputItemIndex: 0,
		}},
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	callID := published.Projection.Calls[0].ID
	replay := mustDecodeCarrier(t, emittedSignature(t, carriedTurnFromPublished(route, items, published, atBudget())))
	if len(replay.Items) != 0 || replay.TextItemIndex == nil || *replay.TextItemIndex != 1 {
		t.Fatalf("mapping-only carrier = items %d text slot %v, want no items and slot 1", len(replay.Items), replay.TextItemIndex)
	}

	body, err := json.Marshal(map[string]any{
		"model": "gpt-public",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "spoken", "tool_calls": []any{map[string]any{
				"id": callID, "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{}`},
			}}},
			map[string]any{"role": "tool", "tool_call_id": callID, "content": "done"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	forgotten := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = forgotten.Close() })
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
		UpstreamModel: "gpt-upstream",
		ReplayStore:   forgotten,
		ReplayRoute:   route,
		CarriedReasoning: map[string]carriedReplay{
			callID: replay,
		},
	})
	if err != nil {
		t.Fatalf("translateChatRequestToResponses() error = %v", err)
	}
	var restored []map[string]any
	if err := json.Unmarshal([]byte(upstreamInputJSON(t, plan)), &restored); err != nil {
		t.Fatal(err)
	}
	callIndex, textIndex := -1, -1
	for index, item := range restored {
		if item["type"] == "function_call" {
			callIndex = index
		}
		if item["role"] == "assistant" && item["content"] == "spoken" {
			textIndex = index
		}
	}
	if callIndex < 0 || textIndex < 0 || callIndex >= textIndex {
		t.Fatalf("rebuilt order call=%d text=%d, want call before text: %#v", callIndex, textIndex, restored)
	}
}

// Below the budget the carrier keeps ciphertext and bindings but no hidden reasoning text.
// The expected wire shape is written out so field or envelope drift remains visible.
func TestCarrierBelowBudgetIsByteIdentical(t *testing.T) {
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	route, items, published := capFixture(t, store)
	safeItems := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_cap","encrypted_content":"` + carrierCapCiphertext + `"}`),
		json.RawMessage(`{"type":"function_call","call_id":"upstream-call-1","name":"lookup"}`),
	}
	calls := []carriedCall{{
		ProxyID: published.Calls[0].ProxyCallID, UpstreamID: published.Calls[0].UpstreamCallID,
		Name: published.Calls[0].Name, ItemIndex: published.Calls[0].OutputItemIndex,
		VisibleArgumentDigest:  hex.EncodeToString(published.Calls[0].visibleArgumentHash[:]),
		OriginalArgumentDigest: hex.EncodeToString(published.Calls[0].originalArgumentHash[:]),
	}}
	projectionDigest := precapProjectionDigest(published.Projection)
	originalProjectionDigest := precapProjectionDigest(published.OriginalProjection)
	unsigned := reasoningCarrierPayload{
		Items: safeItems, Calls: calls, RouteDigest: precapFixtureRouteDigest,
		ProjectionDigest: projectionDigest, OriginalProjectionDigest: originalProjectionDigest,
	}

	want := precapCarrierSignature(t, fmt.Sprintf(
		`{"items":[%s,%s],"calls":[{"proxy_id":%q,"upstream_id":%q,"name":%q,"item_index":%d,`+
			`"visible_argument_digest":%q,"original_argument_digest":%q}],`+
			`"route_digest":%q,"route_tag":%q,"projection_digest":%q,"original_projection_digest":%q}`,
		safeItems[0], safeItems[1],
		published.Calls[0].ProxyCallID, published.Calls[0].UpstreamCallID, published.Calls[0].Name, published.Calls[0].OutputItemIndex,
		calls[0].VisibleArgumentDigest, calls[0].OriginalArgumentDigest,
		precapFixtureRouteDigest, reasoningCarrierRouteTag(unsigned), projectionDigest, originalProjectionDigest,
	))

	for _, emit := range []carrierEmit{{}, {Inbound: carrierInbound{Carriers: 1, Bytes: reasoningCarrierInboundBudget - 1}}} {
		got := emittedSignature(t, carriedTurnFromPublished(route, items, published, emit))
		if got != want {
			t.Fatalf("below-budget carrier changed shape at %d inbound bytes:\n got %s\nwant %s\npayload %s",
				emit.Inbound.Bytes, got, want, carrierPayloadBytes(t, got))
		}
		if !strings.Contains(string(carrierPayloadBytes(t, got)), carrierCapCiphertext) {
			t.Fatalf("below-budget carrier stopped carrying reasoning at %d inbound bytes", emit.Inbound.Bytes)
		}
	}
}

// sha256("provider-a\x00gpt-public\x00gpt-upstream\x00\x00")[:8], capFixture's route under
// a build of 623d890. A constant, so renaming what feeds the digest fails here.
const precapFixtureRouteDigest = "e6a4db7f32a55779"

// The projection digest and envelope, restated rather than called: sha256 over the projected
// content and each id/name/canonical-argument digest behind a NUL, then flate
// BestCompression, raw-url base64, prefix.
func precapProjectionDigest(projection responsesChatReplayAssistantProjection) string {
	sum := sha256.New()
	sum.Write(projection.Content)
	for _, call := range projection.Calls {
		canonical, err := canonicalReplayArguments(call.Arguments)
		if err != nil {
			canonical = []byte(call.Arguments)
		}
		arguments := sha256.Sum256(canonical)
		sum.Write([]byte{0})
		sum.Write([]byte(call.ID))
		sum.Write([]byte{0})
		sum.Write([]byte(call.Name))
		sum.Write([]byte{0})
		sum.Write(arguments[:])
	}
	return hex.EncodeToString(sum.Sum(nil)[:16])
}

func precapCarrierSignature(t *testing.T, payload string) string {
	t.Helper()
	var compressed bytes.Buffer
	writer, err := flate.NewWriter(&compressed, flate.BestCompression)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return "vekil1." + base64.RawURLEncoding.EncodeToString(compressed.Bytes())
}

// A silent degrade is the failure mode this project keeps paying for, so the cap says
// what it dropped -- and says it in counts and bytes, never in ciphertext or prompt text.
func TestCarrierMapOnlyDegradeIsLogged(t *testing.T) {
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	route, items, published := capFixture(t, store)

	var logs bytes.Buffer
	log := logger.NewWithWriter(logger.LevelDebug, &logs)
	carriedTurnFromPublished(route, items, published, carrierEmit{Log: log})
	if logs.Len() != 0 {
		t.Fatalf("a below-budget carrier logged a degrade: %s", logs.String())
	}

	emit := atBudget()
	emit.Log = log
	carriedTurnFromPublished(route, items, published, emit)
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatalf("unmarshal %q: %v", logs.String(), err)
	}
	if entry["level"] != "debug" || entry["msg"] != "reasoning carrier capped to its id mapping; the client's replay is at budget" {
		t.Fatalf("degrade log = %#v", entry)
	}
	if entry["model"] != "gpt-public" {
		t.Fatalf("log[model] = %#v, want gpt-public", entry["model"])
	}
	for field, want := range map[string]float64{
		"inbound_carriers":        291,
		"inbound_carrier_bytes":   float64(reasoningCarrierInboundBudget),
		"budget_bytes":            float64(reasoningCarrierInboundBudget),
		"dropped_reasoning_items": 1,
		"dropped_reasoning_bytes": float64(len(items[0])),
		"mapped_calls":            1,
	} {
		if got, ok := entry[field].(float64); !ok || got != want {
			t.Fatalf("log[%s] = %#v, want %v in %#v", field, entry[field], want, entry)
		}
	}
	for _, forbidden := range []string{carrierCapCiphertext, "encrypted_content", "secret-prompt-text", "checking"} {
		if strings.Contains(logs.String(), forbidden) {
			t.Fatalf("degrade log leaked %q: %s", forbidden, logs.String())
		}
	}
}

// Ballast the client replays. Only the prefix and length reach the counter, so this need
// not decode -- which also proves the weight is counted, not inferred from a decode.
func carrierBallastSignature(size int) string {
	return reasoningCarrierPrefix + strings.Repeat("A", size-len(reasoningCarrierPrefix))
}

func anthropicCarrierBallastRequest(t *testing.T, stream bool, ballast int) string {
	t.Helper()
	messages := []any{map[string]any{"role": "user", "content": "run it"}}
	if ballast > 0 {
		messages = append(messages,
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "", "signature": carrierBallastSignature(ballast)},
			}},
			map[string]any{"role": "user", "content": "again"})
	}
	body, err := json.Marshal(map[string]any{
		"model": "gpt-public", "max_tokens": 128, "stream": stream, "messages": messages,
		"tools": []any{map[string]any{"name": "lookup_synthetic_widget", "input_schema": map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// A client builds a thinking block from frames, so assert on frames: a start whose type
// is thinking and whose thinking field is present, then the signature as a delta at the
// same index. Each of those three has shipped broken and passed a struct-level test.
func carrierThinkingBlockFrames(t *testing.T, body string) {
	t.Helper()
	start, delta := -1, -1
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event struct {
			Type         string `json:"type"`
			Index        *int   `json:"index"`
			ContentBlock *struct {
				Type      string  `json:"type"`
				Thinking  *string `json:"thinking"`
				Signature string  `json:"signature"`
			} `json:"content_block"`
			Delta *struct {
				Type string `json:"type"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event) != nil || event.Index == nil {
			continue
		}
		switch {
		case event.Type == "content_block_start" && event.ContentBlock != nil && event.ContentBlock.Type == "thinking":
			if event.ContentBlock.Signature != "" {
				t.Fatalf("the signature rode the start frame, where clients drop it: %s", line)
			}
			if event.ContentBlock.Thinking == nil {
				t.Fatalf("thinking is absent, which crashes a client reading its length: %s", line)
			}
			start = *event.Index
		case event.Delta != nil && event.Delta.Type == "signature_delta":
			delta = *event.Index
		}
	}
	if start < 0 || delta != start {
		t.Fatalf("no thinking block carried a signature_delta (start=%d, delta=%d):\n%s", start, delta, body)
	}
}

// The carrier as the client receives it. Streaming builds a thinking block from frames;
// the aggregate path returns one block in the JSON body. Both are wire bytes.
func carrierFromAnthropicTurn(t *testing.T, stream bool, body string) string {
	t.Helper()
	if stream {
		carrierThinkingBlockFrames(t, body)
		return carrierSignatureFromStream(t, body)
	}
	var response models.AnthropicResponse
	if err := json.Unmarshal([]byte(body), &response); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	for _, block := range response.Content {
		if block.Type == "thinking" && strings.HasPrefix(block.Signature, reasoningCarrierPrefix) {
			return block.Signature
		}
	}
	t.Fatalf("no carrier block reached the client: %s", body)
	return ""
}

// A declared model_routes deployment reaches the same emit site down a different execution
// path, so the cap has to be wired on both. Same public id, so the request body is shared.
func newDeclaredRouteTestHandler(t *testing.T, baseURL string) *ProxyHandler {
	t.Helper()
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithProvidersConfig(ProvidersConfig{
			SchemaVersion: 2,
			Providers: []ProviderConfig{{
				ID: "test-provider", Type: string(providerTypeOpenAICompatible), Default: true,
				BaseURL: baseURL, AuthType: string(providerAuthTypeNone),
			}},
			ModelRoutes: []ModelRouteConfig{{
				ID: "declared-route", PublicID: "gpt-public", Endpoints: []string{providerEndpointResponses},
				Targets: []ModelRouteTargetConfig{{ID: "primary", Provider: "test-provider", UpstreamModel: "gpt-upstream"}},
			}},
		}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)
	return h
}

// ~89% of real traffic is streaming, and a mapping-only carrier has zero items, so an
// item-count gate anywhere on this path stops carriers entirely. Drive every client-facing
// path and assert on the bytes each one delivers, including whether ciphertext still ships.
func TestCappedCarrierStillReachesTheClient(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/stream_reasoning_tool_call.sse")
	if err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(fixture)
	}))
	defer upstream.Close()

	for _, surface := range []struct {
		name  string
		build func(*testing.T) *ProxyHandler
	}{
		{"synthesised route", func(t *testing.T) *ProxyHandler {
			return newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
		}},
		{"declared route", func(t *testing.T) *ProxyHandler { return newDeclaredRouteTestHandler(t, upstream.URL) }},
	} {
		for _, tc := range []struct {
			name      string
			stream    bool
			ballast   int
			wantItems int
		}{
			{"streaming below budget keeps its items", true, 0, 3},
			{"streaming at budget carries its mapping", true, reasoningCarrierInboundBudget, 0},
			{"aggregate below budget keeps its items", false, 0, 3},
			{"aggregate at budget carries its mapping", false, reasoningCarrierInboundBudget, 0},
		} {
			t.Run(surface.name+"/"+tc.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				surface.build(t).HandleAnthropicMessages(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages",
					strings.NewReader(anthropicCarrierBallastRequest(t, tc.stream, tc.ballast))))
				if recorder.Code != http.StatusOK {
					t.Fatalf("status = %d, body = %s", recorder.Code, recorder.Body.String())
				}
				body := recorder.Body.String()
				signature := carrierFromAnthropicTurn(t, tc.stream, body)
				// The ciphertext is the weight the cap exists to stop shipping.
				if carried := strings.Contains(string(carrierPayloadBytes(t, signature)), carrierFixtureCiphertext); carried != (tc.ballast == 0) {
					t.Fatalf("delivered carrier carries reasoning ciphertext = %v at %d inbound bytes", carried, tc.ballast)
				}
				replay, ok := decodeReasoningCarrier(signature, nil)
				if !ok {
					t.Fatalf("the delivered signature does not decode as a carrier:\n%s", body)
				}
				if len(replay.Items) != tc.wantItems {
					t.Fatalf("delivered carrier items = %d, want %d", len(replay.Items), tc.wantItems)
				}
				if len(replay.Calls) != 1 {
					t.Fatalf("delivered carrier calls = %d, want the id mapping", len(replay.Calls))
				}
				for proxyID, call := range replay.Calls {
					if !isResponsesChatReplayCallID(proxyID) || call.UpstreamID != "call_synth_lookup_stream_001" ||
						call.Name != "lookup_synthetic_widget" {
						t.Fatalf("mapping does not bind a minted id to the upstream call: %s -> %+v", proxyID, call)
					}
				}
			})
		}
	}
}

// The cap can only fire on what the counter sees, and the counter's only input is the
// request body. Count off decoded wire bytes and assert both halves: the arithmetic, and
// that the same count is what flips the decision.
func TestInboundCarrierBytesAreCountedFromTheWire(t *testing.T) {
	carrier := carrierBallastSignature(4096)
	// A client's own thinking signature, which vekil never minted.
	foreign := "ErUBCkYIBRgCKkBhbnRocm9waWMtc2lnbmF0dXJl"
	body, err := json.Marshal(map[string]any{
		"model": "gpt-public", "max_tokens": 16,
		"messages": []any{
			map[string]any{"role": "user", "content": "go"},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "", "signature": carrier},
				map[string]any{"type": "thinking", "thinking": "", "signature": foreign},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "redacted_thinking", "thinking": "", "signature": carrier},
			}},
			// A user turn is not the client replaying what vekil emitted.
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "thinking", "thinking": "", "signature": carrier},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var req models.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatal(err)
	}
	_, inbound := extractCarriedReasoning(req.Messages)
	if inbound.Carriers != 2 || inbound.Bytes != 2*len(carrier) {
		t.Fatalf("counted %d carriers / %d bytes off the wire, want 2 / %d", inbound.Carriers, inbound.Bytes, 2*len(carrier))
	}

	for _, want := range []bool{false, true} {
		size := reasoningCarrierInboundBudget - 1
		if want {
			size = reasoningCarrierInboundBudget
		}
		var atBoundary models.AnthropicRequest
		if err := json.Unmarshal([]byte(anthropicCarrierBallastRequest(t, false, size)), &atBoundary); err != nil {
			t.Fatal(err)
		}
		_, inbound := extractCarriedReasoning(atBoundary.Messages)
		if inbound.Bytes != size {
			t.Fatalf("counted %d bytes off a %d-byte carrier", inbound.Bytes, size)
		}
		if inbound.mapOnly() != want {
			t.Fatalf("%d inbound bytes decided mapOnly = %v, want %v", inbound.Bytes, inbound.mapOnly(), want)
		}
	}
}
