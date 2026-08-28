package proxy

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/sozercan/vekil/logger"
)

// degradeFixture publishes one stored turn and returns the request body that
// replays it, plus a copy whose assistant text has drifted from what was stored.
func degradeFixture(t *testing.T, store *responsesChatReplayStore, route responsesChatReplayRoute) (matching, drifted []byte, callID string) {
	t.Helper()
	published, err := store.Publish(responsesChatReplayPublishRequest{
		Route:            route,
		AssistantContent: json.RawMessage(`"checking"`),
		OutputItems: []json.RawMessage{
			json.RawMessage(`{"type":"reasoning","id":"rs_degrade","encrypted_content":"OPAQUE","content":[],"summary":[]}`),
			json.RawMessage(`{"type":"function_call","call_id":"upstream-call-1","name":"lookup","arguments":"{}","status":"completed"}`),
		},
		Calls: []responsesChatReplayPublishCall{{
			UpstreamCallID: "upstream-call-1", Name: "lookup", VisibleArguments: `{}`, OutputItemIndex: 1,
		}},
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	callID = published.Projection.Calls[0].ID
	matching, err = json.Marshal(map[string]any{
		"model": "gpt-public",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "checking", "tool_calls": []any{map[string]any{
				"id": callID, "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{}`},
			}}},
			map[string]any{"role": "tool", "tool_call_id": callID, "content": "result-1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	drifted = []byte(strings.Replace(string(matching), `"content":"checking"`, `"content":"checking twice"`, 1))
	if bytes.Equal(drifted, matching) {
		t.Fatal("fixture no longer carries the assistant text this drift rewrites")
	}
	return matching, drifted, callID
}

// With no carrier to fall back on, a drifted projection reaches upstream rebuilt
// from the visible messages, carrying no reasoning.
func TestProjectionMismatchDegradesToTheVisibleTranscript(t *testing.T) {
	store, route := newCarrierReplayFixture(t)
	_, drifted, callID := degradeFixture(t, store, route)

	plan, err := translateChatRequestToResponses(drifted, responsesChatRequestOptions{
		UpstreamModel: "gpt-upstream", ReplayStore: store, ReplayRoute: route,
		DegradeUnrestorableReplay: true,
	})
	if err != nil {
		t.Fatalf("a projection mismatch must not fail the request: %v", err)
	}

	// Assert on the bytes that go on the wire: a decoded struct cannot see a field
	// omitempty dropped on the way out, and the reasoning item is exactly a field.
	input := upstreamInputJSON(t, plan)
	for _, forbidden := range []string{`"reasoning"`, "OPAQUE", "encrypted_content", "upstream-call-1"} {
		if strings.Contains(input, forbidden) {
			t.Fatalf("degraded turn carries %q: %s", forbidden, input)
		}
	}
	var items []map[string]any
	if err := json.Unmarshal([]byte(input), &items); err != nil {
		t.Fatal(err)
	}
	var call, output map[string]any
	for _, item := range items {
		switch item["type"] {
		case "function_call":
			call = item
		case "function_call_output":
			output = item
		}
	}
	if call == nil || output == nil {
		t.Fatalf("degraded turn lost its call/result pair: %s", input)
	}
	if call["call_id"] != callID || output["call_id"] != callID {
		t.Fatalf("call ids = %v / %v, want both %q: %s", call["call_id"], output["call_id"], callID, input)
	}
	if call["name"] != "lookup" || call["arguments"] != "{}" {
		t.Fatalf("degraded call = %#v, want the visible tool call", call)
	}
	if !strings.Contains(input, "checking twice") {
		t.Fatalf("degraded turn dropped the visible assistant text: %s", input)
	}
}

// TestMatchingProjectionStillRestoresStoredState pins the other half of the deal:
// where the store is live and matching, nothing about the restore changes.
func TestMatchingProjectionStillRestoresStoredState(t *testing.T) {
	store, route := newCarrierReplayFixture(t)
	matching, _, _ := degradeFixture(t, store, route)

	plan, err := translateChatRequestToResponses(matching, responsesChatRequestOptions{
		UpstreamModel: "gpt-upstream", ReplayStore: store, ReplayRoute: route,
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	input := upstreamInputJSON(t, plan)
	for _, want := range []string{`"encrypted_content":"OPAQUE"`, `"call_id":"upstream-call-1"`} {
		if !strings.Contains(input, want) {
			t.Fatalf("matching projection lost %s: %s", want, input)
		}
	}
}

// TestProjectionMismatchDegradeIsLogged: a degrade is a quality loss, so it must be
// distinguishable from a clean turn in the log rather than being silent.
func TestProjectionMismatchDegradeIsLogged(t *testing.T) {
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	route := responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream", RouteID: "route-a"}
	matching, drifted, _ := degradeFixture(t, store, route)

	var logs bytes.Buffer
	options := responsesChatRequestOptions{
		UpstreamModel: "gpt-upstream", ReplayStore: store, ReplayRoute: route,
		DegradeUnrestorableReplay: true,
		Log:                       logger.NewWithWriter(logger.LevelInfo, &logs),
	}
	if _, err := translateChatRequestToResponses(matching, options); err != nil {
		t.Fatalf("translate matching: %v", err)
	}
	if logs.Len() != 0 {
		t.Fatalf("a matching projection logged %q", logs.String())
	}
	if _, err := translateChatRequestToResponses(drifted, options); err != nil {
		t.Fatalf("translate drifted: %v", err)
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatalf("unmarshal %q: %v", logs.String(), err)
	}
	want := map[string]any{
		"level":    "warn",
		"msg":      "responses replay projection mismatch; continuing without reasoning continuity",
		"provider": "provider-a",
		"model":    "gpt-public",
		"route_id": "route-a",
	}
	for key, expected := range want {
		if got := entry[key]; got != expected {
			t.Fatalf("log[%s] = %#v, want %#v in %#v", key, got, expected, entry)
		}
	}
	if got, ok := entry["tool_calls"].(float64); !ok || got != 1 {
		t.Fatalf("log[tool_calls] = %#v, want 1 in %#v", entry["tool_calls"], entry)
	}
}

// Through the whole ingress: native Chat stays loud. It owns its history and can repair it,
// so a drifted projection is reported rather than silently rebuilt -- and upstream is never
// called, because a degrade here would send a turn the client never sanctioned. The Anthropic
// surface opts into the other behaviour via DegradeUnrestorableReplay.
func TestHandleOpenAIChatCompletionsProjectionMismatchIsRejected(t *testing.T) {
	var upstreamBodies [][]byte
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamBodies = append(upstreamBodies, body)
		http.Error(w, "unexpected", http.StatusInternalServerError)
	}))
	defer upstream.Close()

	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointResponses})
	route := responsesChatReplayRoute{ProviderID: "test-provider", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
	_, drifted, _ := degradeFixture(t, h.responsesChatReplayStore(), route)

	rec := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(drifted)))

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; native Chat must not degrade a drifted projection: %s", rec.Code, rec.Body.String())
	}
	if len(upstreamBodies) != 0 {
		t.Fatalf("upstream was called %d time(s) for a rejected turn: %s", len(upstreamBodies), string(upstreamBodies[0]))
	}
}

// An opaque ID carries no upstream mapping, so when the store is gone and no carrier answers
// there is nothing to recover it from. The degrade still applies -- a client cannot repair a
// transcript it already sent -- so the turn continues with the PROXY id forwarded upstream as
// the call_id. That is safe only because the whole turn is rebuilt from the transcript in the
// same request, where call_id needs to be internally consistent and nothing more; vekil sends
// `store: false` and replays the history every turn, so there is no server-side registry of
// call ids to contradict. scripts/live-claude-reasoning-carrier-smoke.sh asserts this against
// live Copilot after an actual proxy restart.
//
// The alternative -- refusing to degrade an opaque id -- reinstates the permanent wedge this
// branch exists to remove, so this is deliberate and the contract says so.
func TestAnthropicOpaqueIDDegradesWhileNativeChatStillRefuses(t *testing.T) {
	fixture, err := os.ReadFile("testdata/chat_over_responses/stream_reasoning_tool_call.sse")
	if err != nil {
		t.Fatal(err)
	}
	opaqueID := responsesChatReplayCallIDPrefix + strings.Repeat("A", 22)
	if !isResponsesChatReplayCallID(opaqueID) {
		t.Fatalf("%q is not recognised as a replay ID, so no replay path runs at all", opaqueID)
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

	anthropic := `{"model":"gpt-public","max_tokens":128,"messages":[
		{"role":"user","content":"run it"},
		{"role":"assistant","content":[{"type":"tool_use","id":"` + opaqueID + `","name":"lookup_synthetic_widget","input":{}}]},
		{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + opaqueID + `","content":"ok"}]},
		{"role":"user","content":"and again"}],
		"tools":[{"name":"lookup_synthetic_widget","input_schema":{"type":"object"}}]}`
	rec := httptest.NewRecorder()
	h.HandleAnthropicMessages(rec, httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(anthropic)))
	if rec.Code != http.StatusOK {
		t.Fatalf("Anthropic opaque continuation wedged: status = %d body = %s", rec.Code, rec.Body.String())
	}
	if len(upstreamBodies) != 1 {
		t.Fatalf("upstream saw %d requests, want 1", len(upstreamBodies))
	}
	if !strings.Contains(upstreamBodies[0], `"call_id":"`+opaqueID+`"`) {
		t.Fatalf("degraded turn did not forward the proxy id upstream: %s", upstreamBodies[0])
	}
	if strings.Contains(upstreamBodies[0], orphanFixtureCiphertext) {
		t.Fatalf("an opaque id recovered reasoning it never had a mapping for: %s", upstreamBodies[0])
	}

	// Same lost state, native Chat: it owns its history and can repair it, so it stays loud.
	chat := `{"model":"gpt-public","messages":[
		{"role":"assistant","tool_calls":[{"id":"` + opaqueID + `","type":"function","function":{"name":"lookup_synthetic_widget","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"` + opaqueID + `","content":"ok"}]}`
	chatRec := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(chatRec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chat)))
	if chatRec.Code != http.StatusBadRequest {
		t.Fatalf("native Chat status = %d, want 400; it must not degrade: %s", chatRec.Code, chatRec.Body.String())
	}
	if !strings.Contains(chatRec.Body.String(), responsesChatReplayMissingCode) {
		t.Fatalf("native Chat body = %s, want the deterministic %s", chatRec.Body.String(), responsesChatReplayMissingCode)
	}
	if len(upstreamBodies) != 1 {
		t.Fatalf("native Chat reached upstream for a turn it should have refused locally: %s", upstreamBodies[1])
	}
}

// Claude Code may retain a newer carrier while dropping one from an older tool turn. After a
// restart the newer turn must still recover its reasoning under the opaque public call ID, while
// the older turn independently falls back to its visible transcript instead of wedging the request.
func TestDroppedOlderCarrierDegradesAlongsideNewerCarrierAfterRestart(t *testing.T) {
	_, route, items, published := publishCarrierParityTurn(t, "upstream-recent")
	recent := published.Projection.Calls[0]
	oldID := responsesChatReplayCallIDPrefix + strings.Repeat("D", 22)
	body, err := json.Marshal(map[string]any{
		"model": "gpt-public",
		"messages": []any{
			map[string]any{"role": "assistant", "content": "older visible turn", "tool_calls": []any{map[string]any{
				"id": oldID, "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"key":"old"}`},
			}}},
			map[string]any{"role": "tool", "tool_call_id": oldID, "content": "old-result"},
			map[string]any{"role": "assistant", "content": "checking", "tool_calls": []any{map[string]any{
				"id": recent.ID, "type": "function", "function": map[string]any{"name": recent.Name, "arguments": recent.Arguments},
			}}},
			map[string]any{"role": "tool", "tool_call_id": recent.ID, "content": "recent-result"},
			map[string]any{"role": "user", "content": "continue"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	var logs bytes.Buffer
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
		UpstreamModel: "gpt-upstream",
		ReplayRoute:   route,
		// No ReplayStore models expiry, eviction, or restart. Only the newer turn's
		// carrier survived in the client transcript.
		CarriedReasoning:          carriedForEveryCall(t, route, published, items),
		DegradeUnrestorableReplay: true,
		Log:                       logger.NewWithWriter(logger.LevelInfo, &logs),
	})
	if err != nil {
		t.Fatalf("translate mixed carrier history: %v", err)
	}
	input := upstreamInputJSON(t, plan)
	var upstreamItems []map[string]any
	if err := json.Unmarshal([]byte(input), &upstreamItems); err != nil {
		t.Fatal(err)
	}
	calls, outputs := map[string]bool{}, map[string]bool{}
	for _, item := range upstreamItems {
		callID, _ := item["call_id"].(string)
		switch item["type"] {
		case "function_call":
			calls[callID] = true
		case "function_call_output":
			outputs[callID] = true
		}
	}
	for _, callID := range []string{oldID, recent.ID} {
		if !calls[callID] || !outputs[callID] {
			t.Fatalf("call %q was not paired after mixed recovery: calls=%v outputs=%v input=%s", callID, calls, outputs, input)
		}
	}
	for _, want := range []string{
		`"call_id":"` + oldID + `"`,
		`"call_id":"` + recent.ID + `"`,
		`"encrypted_content":"OPAQUE"`,
		`"arguments":"{\"key\":\"old\"}"`,
	} {
		if !strings.Contains(input, want) {
			t.Fatalf("mixed recovery lost %s: %s", want, input)
		}
	}
	if strings.Contains(input, `"call_id":"upstream-recent"`) {
		t.Fatalf("mixed recovery exposed the provider-owned call ID: %s", input)
	}

	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatalf("unmarshal degrade log %q: %v", logs.String(), err)
	}
	for key, want := range map[string]any{
		"carrier":        "absent",
		"diverged":       "store_missing",
		"carried_turns":  float64(1),
		"degraded_turns": float64(1),
		"tool_turns":     float64(1),
		"tool_calls":     float64(1),
	} {
		if got := entry[key]; got != want {
			t.Fatalf("log[%s] = %#v, want %#v in %#v", key, got, want, entry)
		}
	}
}
