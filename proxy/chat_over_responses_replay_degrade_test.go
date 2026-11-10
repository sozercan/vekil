package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
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

const nativeChatReplayDegradeMessage = "forwarding responses replay IDs to native Chat without restoring their reasoning"

// chatOnlyReplayTranscript is what Claude Code sends after a session switches from a
// Responses-backed model to one that serves only native Chat: the earlier parallel turn left a
// self-describing ID, an opaque ID, and a reasoning carrier in a transcript the client cannot
// repair.
func chatOnlyReplayTranscript(t *testing.T, model string) (anthropic string, ids []string) {
	t.Helper()
	selfID, ok := responsesChatReplaySelfDescribingID("call_upstream_lookup")
	if !ok {
		t.Fatal("fixture upstream call ID no longer mints a self-describing replay ID")
	}
	ids = []string{selfID, responsesChatReplayCallIDPrefix + strings.Repeat("B", 22)}
	for _, id := range ids {
		if !isResponsesChatReplayCallID(id) {
			t.Fatalf("%q is not recognised as a replay ID, so no replay path runs at all", id)
		}
	}
	anthropic = `{"model":"` + model + `","max_tokens":128,"messages":[
		{"role":"user","content":"look both up"},
		{"role":"assistant","content":[
			{"type":"thinking","thinking":"","signature":"` + reasoningCarrierPrefix + `SWITCHEDAWAYCARRIER"},
			{"type":"tool_use","id":"` + ids[0] + `","name":"lookup","input":{}},
			{"type":"tool_use","id":"` + ids[1] + `","name":"lookup","input":{}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"` + ids[0] + `","content":"first"},
			{"type":"tool_result","tool_use_id":"` + ids[1] + `","content":"second"}]},
		{"role":"user","content":"now compare them"}],
		"tools":[{"name":"lookup","input_schema":{"type":"object"}}]}`
	return anthropic, ids
}

func chatOnlyReplayChatRequest(model string, ids []string) string {
	return `{"model":"` + model + `","messages":[
		{"role":"assistant","tool_calls":[
			{"id":"` + ids[0] + `","type":"function","function":{"name":"lookup","arguments":"{}"}},
			{"id":"` + ids[1] + `","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
		{"role":"tool","tool_call_id":"` + ids[0] + `","content":"first"},
		{"role":"tool","tool_call_id":"` + ids[1] + `","content":"second"}]}`
}

// chatOnlyUpstream serves native Chat only, answering a forced stream with SSE and anything
// else with JSON, and records every request it receives.
type chatOnlyUpstream struct {
	*httptest.Server
	mu     sync.Mutex
	paths  []string
	bodies []string
	// rejectMaxCompletionTokens answers max_completion_tokens with the 400 that sends the
	// count-token probe through its max_tokens protocol-recovery retry.
	rejectMaxCompletionTokens bool
	// catalogStatus, once set, answers /models: 200 serves catalogBody, anything else fails
	// discovery.
	catalogStatus int
	catalogBody   string
}

func newChatOnlyUpstream(t *testing.T, upstreamModel string) *chatOnlyUpstream {
	t.Helper()
	upstream := &chatOnlyUpstream{}
	upstream.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstream.mu.Lock()
		upstream.paths = append(upstream.paths, r.URL.Path)
		upstream.bodies = append(upstream.bodies, string(body))
		rejectMaxCompletionTokens := upstream.rejectMaxCompletionTokens
		catalogStatus, catalogBody := upstream.catalogStatus, upstream.catalogBody
		upstream.mu.Unlock()
		if r.URL.Path == providerEndpointModels && catalogStatus != 0 {
			if catalogStatus != http.StatusOK {
				http.Error(w, "catalog unavailable", catalogStatus)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, catalogBody)
			return
		}
		if r.URL.Path != providerEndpointChatCompletions {
			http.Error(w, "this upstream serves native Chat only", http.StatusNotFound)
			return
		}
		if rejectMaxCompletionTokens && bytes.Contains(body, []byte(`"max_completion_tokens"`)) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"error":{"message":"Unsupported parameter: 'max_completion_tokens'","type":"invalid_request_error","param":"max_completion_tokens","code":"unsupported_parameter"}}`)
			return
		}
		var request struct {
			Stream bool `json:"stream"`
		}
		_ = json.Unmarshal(body, &request)
		if !request.Stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chatcmpl-native","object":"chat.completion","created":1,"model":"`+upstreamModel+`","choices":[{"index":0,"message":{"role":"assistant","content":"compared"},"finish_reason":"stop"}],"usage":{"prompt_tokens":42,"completion_tokens":1,"total_tokens":43}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		for _, chunk := range []string{
			`{"id":"chatcmpl-native","object":"chat.completion.chunk","created":1,"model":"` + upstreamModel + `","choices":[{"index":0,"delta":{"role":"assistant","content":"compared"},"finish_reason":null}]}`,
			`{"id":"chatcmpl-native","object":"chat.completion.chunk","created":1,"model":"` + upstreamModel + `","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
			`{"id":"chatcmpl-native","object":"chat.completion.chunk","created":1,"model":"` + upstreamModel + `","choices":[],"usage":{"prompt_tokens":42,"completion_tokens":1,"total_tokens":43}}`,
			`[DONE]`,
		} {
			_, _ = io.WriteString(w, "data: "+chunk+"\n\n")
		}
	}))
	t.Cleanup(upstream.Close)
	return upstream
}

func (u *chatOnlyUpstream) rejectMaxCompletionTokensParam() {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.rejectMaxCompletionTokens = true
}

func (u *chatOnlyUpstream) serveCatalog(status int, body string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.catalogStatus, u.catalogBody = status, body
}

func (u *chatOnlyUpstream) inferencePaths() []string {
	u.mu.Lock()
	defer u.mu.Unlock()
	var paths []string
	for _, path := range u.paths {
		if path != providerEndpointModels {
			paths = append(paths, path)
		}
	}
	return paths
}

func (u *chatOnlyUpstream) requests() (paths, bodies []string) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return append([]string(nil), u.paths...), append([]string(nil), u.bodies...)
}

// assertReplayIDsForwardedVerbatim requires the native Chat request to carry every replay ID,
// in order, on both the assistant calls and their results -- and no reasoning carrier.
func assertReplayIDsForwardedVerbatim(t *testing.T, body string, ids []string) {
	t.Helper()
	var sent struct {
		Messages []struct {
			Role       string `json:"role"`
			ToolCallID string `json:"tool_call_id"`
			ToolCalls  []struct {
				ID string `json:"id"`
			} `json:"tool_calls"`
		} `json:"messages"`
	}
	if err := json.Unmarshal([]byte(body), &sent); err != nil {
		t.Fatalf("decode native Chat request: %v: %s", err, body)
	}
	var calls, results []string
	for _, message := range sent.Messages {
		for _, call := range message.ToolCalls {
			calls = append(calls, call.ID)
		}
		if message.Role == "tool" {
			results = append(results, message.ToolCallID)
		}
	}
	if strings.Join(calls, ",") != strings.Join(ids, ",") || strings.Join(results, ",") != strings.Join(ids, ",") {
		t.Fatalf("tool calls = %v, results = %v, want both %v verbatim: %s", calls, results, ids, body)
	}
	if strings.Contains(body, reasoningCarrierPrefix) || strings.Contains(body, "SWITCHEDAWAYCARRIER") {
		t.Fatalf("a reasoning carrier reached the native Chat backend: %s", body)
	}
}

func nativeChatReplayDegradeEntries(t *testing.T, logs string) []map[string]any {
	t.Helper()
	var entries []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) == nil && entry["msg"] == nativeChatReplayDegradeMessage {
			entries = append(entries, entry)
		}
	}
	return entries
}

// assertNativeChatReplayDegradeEntry pins the whole entry, so a call ID or any content added
// to the line fails here rather than reaching operator logs.
func assertNativeChatReplayDegradeEntry(t *testing.T, entry map[string]any, route map[string]any) {
	t.Helper()
	want := map[string]any{"level": "info", "msg": nativeChatReplayDegradeMessage}
	for key, value := range route {
		want[key] = value
	}
	if _, ok := entry["time"].(string); !ok {
		t.Fatalf("degrade log carries no time: %#v", entry)
	}
	if len(entry) != len(want)+1 {
		t.Fatalf("degrade log = %#v, want exactly %#v plus time", entry, want)
	}
	for key, expected := range want {
		if got := entry[key]; got != expected {
			t.Fatalf("log[%s] = %#v, want %#v in %#v", key, got, expected, entry)
		}
	}
}

// A model switch onto a target with no Responses backend leaves replay IDs this process cannot
// restore there. Anthropic ingress and its count-token probe continue on native Chat with the
// IDs unchanged and no reasoning restored, which is the visible-transcript degrade with nothing
// left to restore. Native Chat stays strict: it owns its history and can restart the turn.
func TestResponsesChatReplayContinuesOnChatOnlyModelWhileNativeChatRefuses(t *testing.T) {
	upstream := newChatOnlyUpstream(t, "gpt-upstream")
	h := newChatExecutionTestHandler(t, upstream.URL, []string{providerEndpointChatCompletions})
	logs := &conversationLogBuffer{}
	h.log = logger.NewWithWriter(logger.LevelInfo, logs)
	anthropic, ids := chatOnlyReplayTranscript(t, "gpt-public")

	rec := httptest.NewRecorder()
	h.HandleAnthropicMessages(rec, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(anthropic)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "compared") {
		t.Fatalf("Anthropic continuation after a model switch: status = %d body = %s", rec.Code, rec.Body.String())
	}
	paths, bodies := upstream.requests()
	if len(paths) != 1 || paths[0] != providerEndpointChatCompletions {
		t.Fatalf("upstream paths = %v, want one native Chat request", paths)
	}
	assertReplayIDsForwardedVerbatim(t, bodies[0], ids)

	countRec := httptest.NewRecorder()
	h.HandleAnthropicMessagesCountTokens(countRec, httptest.NewRequest(http.MethodPost, providerEndpointMessages+"/count_tokens", strings.NewReader(anthropic)))
	if countRec.Code != http.StatusOK || !strings.Contains(countRec.Body.String(), `"input_tokens":42`) {
		t.Fatalf("count-token probe after a model switch: status = %d body = %s", countRec.Code, countRec.Body.String())
	}
	paths, bodies = upstream.requests()
	if len(paths) != 2 || paths[1] != providerEndpointChatCompletions {
		t.Fatalf("upstream paths = %v, want the probe on native Chat", paths)
	}
	assertReplayIDsForwardedVerbatim(t, bodies[1], ids)

	entries := nativeChatReplayDegradeEntries(t, logs.String())
	if len(entries) != 2 {
		t.Fatalf("degrade log entries = %d, want one per request: %s", len(entries), logs.String())
	}
	for _, entry := range entries {
		assertNativeChatReplayDegradeEntry(t, entry, map[string]any{"provider": "test-provider", "model": "gpt-public"})
	}

	chatRec := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(chatRec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatOnlyReplayChatRequest("gpt-public", ids))))
	if chatRec.Code != http.StatusBadRequest || !strings.Contains(chatRec.Body.String(), responsesChatReplayMissingCode) {
		t.Fatalf("native Chat status = %d body = %s, want 400 %s", chatRec.Code, chatRec.Body.String(), responsesChatReplayMissingCode)
	}
	if paths, _ = upstream.requests(); len(paths) != 2 {
		t.Fatalf("native Chat reached upstream for a turn it must refuse locally: %v", paths)
	}
	if entries = nativeChatReplayDegradeEntries(t, logs.String()); len(entries) != 2 {
		t.Fatalf("a refused native Chat request logged a degrade: %s", logs.String())
	}
}

// An unknown model on a dynamic provider may still serve Responses once its catalog answers, so
// a failed discovery proves nothing about restoring its replay: the turn is refused as before
// rather than continued without its reasoning. Once the catalog lists the model as Chat-only,
// the same transcript continues.
func TestResponsesChatReplayChatOnlyDegradeRequiresCatalogedModel(t *testing.T) {
	upstream := newChatOnlyUpstream(t, "dyn-model")
	upstream.serveCatalog(http.StatusServiceUnavailable, "")
	h, err := NewProxyHandler(
		auth.NewTestAuthenticator("fixture"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		WithDeferredDynamicProviderModelValidation(true),
		WithProvidersConfig(ProvidersConfig{Providers: []ProviderConfig{{
			ID:             "dynamic",
			Type:           string(providerTypeOpenAICompatible),
			Default:        true,
			BaseURL:        upstream.URL,
			AuthType:       string(providerAuthTypeNone),
			ModelDiscovery: "openai",
		}}}),
	)
	if err != nil {
		t.Fatalf("NewProxyHandler() error = %v", err)
	}
	t.Cleanup(h.BeginShutdown)
	h.maxRetries = 1
	var clock atomic.Int64
	clock.Store(time.Unix(1_700_000_000, 0).UnixNano())
	h.chatRoutes.now = func() time.Time { return time.Unix(0, clock.Load()) }
	logs := &conversationLogBuffer{}
	h.log = logger.NewWithWriter(logger.LevelInfo, logs)
	anthropic, ids := chatOnlyReplayTranscript(t, "dyn-model")

	rec := httptest.NewRecorder()
	h.HandleAnthropicMessages(rec, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(anthropic)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), responsesChatReplayMissingMessage) {
		t.Fatalf("uncataloged model: status = %d body = %s, want the local replay refusal", rec.Code, rec.Body.String())
	}
	if paths := upstream.inferencePaths(); len(paths) != 0 {
		t.Fatalf("an uncataloged model reached inference: %v", paths)
	}
	if entries := nativeChatReplayDegradeEntries(t, logs.String()); len(entries) != 0 {
		t.Fatalf("a refused request logged a degrade: %s", logs.String())
	}

	upstream.serveCatalog(http.StatusOK, `{"object":"list","data":[{"id":"dyn-model","supported_endpoints":["/chat/completions"]}]}`)
	clock.Add(int64(time.Hour))
	rec = httptest.NewRecorder()
	h.HandleAnthropicMessages(rec, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(anthropic)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "compared") {
		t.Fatalf("cataloged Chat-only model: status = %d body = %s", rec.Code, rec.Body.String())
	}
	paths, bodies := upstream.requests()
	if inference := upstream.inferencePaths(); len(inference) != 1 || inference[0] != providerEndpointChatCompletions {
		t.Fatalf("inference paths = %v, want one native Chat request", inference)
	}
	assertReplayIDsForwardedVerbatim(t, bodies[len(paths)-1], ids)
	entries := nativeChatReplayDegradeEntries(t, logs.String())
	if len(entries) != 1 {
		t.Fatalf("degrade log entries = %d, want one: %s", len(entries), logs.String())
	}
	assertNativeChatReplayDegradeEntry(t, entries[0], map[string]any{"provider": "dynamic", "model": "dyn-model"})
}

// The schema-v2 route takes the same decision in its own endpoint selection, before a route
// operation is admitted, and the log names the route rather than one of its targets.
func TestResponsesChatReplayContinuesOnChatOnlyExplicitRouteWhileNativeChatRefuses(t *testing.T) {
	primary := newChatOnlyUpstream(t, "physical-primary")
	secondary := newChatOnlyUpstream(t, "physical-secondary")
	h := newExplicitRouteSurfaceHandler(t, providerTypeOpenAICompatible, providerEndpointChatCompletions, primary.URL, secondary.URL)
	logs := &conversationLogBuffer{}
	h.log = logger.NewWithWriter(logger.LevelInfo, logs)
	anthropic, ids := chatOnlyReplayTranscript(t, "public-model")

	rec := httptest.NewRecorder()
	h.HandleAnthropicMessages(rec, httptest.NewRequest(http.MethodPost, providerEndpointMessages, strings.NewReader(anthropic)))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "compared") {
		t.Fatalf("Anthropic continuation on a Chat-only route: status = %d body = %s", rec.Code, rec.Body.String())
	}
	paths, bodies := primary.requests()
	if len(paths) != 1 || paths[0] != providerEndpointChatCompletions {
		t.Fatalf("primary paths = %v, want one native Chat request", paths)
	}
	assertReplayIDsForwardedVerbatim(t, bodies[0], ids)
	if paths, _ := secondary.requests(); len(paths) != 0 {
		t.Fatalf("secondary paths = %v, want none after a primary success", paths)
	}
	entries := nativeChatReplayDegradeEntries(t, logs.String())
	if len(entries) != 1 {
		t.Fatalf("degrade log entries = %d, want 1: %s", len(entries), logs.String())
	}
	assertNativeChatReplayDegradeEntry(t, entries[0], map[string]any{"model": "public-model", "route_id": "route-public"})

	// The count-token probe continues the same way. Its max_tokens protocol-recovery retry
	// re-enters endpoint selection, yet it is still one request and one log entry.
	primary.rejectMaxCompletionTokensParam()
	countRec := httptest.NewRecorder()
	h.HandleAnthropicMessagesCountTokens(countRec, httptest.NewRequest(http.MethodPost, providerEndpointMessages+"/count_tokens", strings.NewReader(anthropic)))
	if countRec.Code != http.StatusOK || !strings.Contains(countRec.Body.String(), `"input_tokens":42`) {
		t.Fatalf("count-token probe on a Chat-only route: status = %d body = %s", countRec.Code, countRec.Body.String())
	}
	paths, bodies = primary.requests()
	if len(paths) != 3 || paths[1] != providerEndpointChatCompletions || paths[2] != providerEndpointChatCompletions {
		t.Fatalf("primary paths = %v, want the probe and its max_tokens retry on native Chat", paths)
	}
	assertReplayIDsForwardedVerbatim(t, bodies[2], ids)
	if entries = nativeChatReplayDegradeEntries(t, logs.String()); len(entries) != 2 {
		t.Fatalf("degrade log entries = %d, want one per request: %s", len(entries), logs.String())
	}
	assertNativeChatReplayDegradeEntry(t, entries[1], map[string]any{"model": "public-model", "route_id": "route-public"})

	chatRec := httptest.NewRecorder()
	h.HandleOpenAIChatCompletions(chatRec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(chatOnlyReplayChatRequest("public-model", ids))))
	if chatRec.Code != http.StatusBadRequest || !strings.Contains(chatRec.Body.String(), responsesChatReplayMissingCode) {
		t.Fatalf("native Chat status = %d body = %s, want 400 %s", chatRec.Code, chatRec.Body.String(), responsesChatReplayMissingCode)
	}
	if paths, _ := primary.requests(); len(paths) != 3 {
		t.Fatalf("native Chat reached upstream for a turn it must refuse locally: %v", paths)
	}
	if entries = nativeChatReplayDegradeEntries(t, logs.String()); len(entries) != 2 {
		t.Fatalf("a refused native Chat request logged a degrade: %s", logs.String())
	}
}

// A policy plan already resolved or refused its replay against the tier that recorded it, so
// it keeps failing closed on a Chat-only terminal even where a direct route would degrade.
func TestResponsesChatReplayChatOnlyDegradeExcludesPolicyPlans(t *testing.T) {
	h := newExplicitRouteSurfaceHandler(t, providerTypeOpenAICompatible, providerEndpointChatCompletions, "http://primary.invalid", "http://secondary.invalid")
	route, known := h.resolveModelRouteForRequest("public-model", providerEndpointChatCompletions)
	if !known || route == nil || route.legacy {
		t.Fatalf("fixture route = %#v known = %v, want the explicit route", route, known)
	}
	_, ids := chatOnlyReplayTranscript(t, "public-model")
	body := []byte(chatOnlyReplayChatRequest("public-model", ids))

	if endpoint, degraded, err := explicitChatExecutionEndpoint(route, body, true); err != nil || endpoint != providerEndpointChatCompletions || !degraded {
		t.Fatalf("direct route = %q degraded=%v err=%v, want a native Chat degrade", endpoint, degraded, err)
	}
	direct := newRouteOperation(route, context.Background())
	if endpoint, err := policyAwareChatExecutionEndpoint(direct, body, true); err != nil || endpoint != providerEndpointChatCompletions {
		t.Fatalf("direct operation = %q err=%v, want native Chat", endpoint, err)
	}
	planned := newRouteOperationFromChatPlan(newChatOperationPlan(chatOperationPlanOptions{
		PublicID: "coding-policy", Route: route, EffectiveMode: policyModeEnforce,
	}), context.Background())
	if planned == nil {
		t.Fatal("fixture plan did not produce a policy operation")
	}
	if endpoint, err := policyAwareChatExecutionEndpoint(planned, body, true); !isMissingResponsesChatReplayError(err) {
		t.Fatalf("policy plan = %q err=%v, want %s", endpoint, err, responsesChatReplayMissingCode)
	}
}
