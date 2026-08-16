package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sozercan/vekil/logger"
)

// Measured from live Copilot traffic: 29 characters, already legal as an Anthropic
// tool_use id, which is what makes embedding it in the minted id possible at all.
const copilotUpstreamCallID = "call_CALKyjb6bMGoY2uPatm0UsLi"

// selfDescribingFixture publishes one turn and returns the minted proxy ID plus a Chat body
// replaying it. Mirrors clientDriftFixture, but parameterised on the upstream ID so a test
// can choose whether the minter is able to embed it.
func selfDescribingFixture(t *testing.T, store *responsesChatReplayStore, route responsesChatReplayRoute, upstreamCallID string) (string, []byte) {
	t.Helper()
	const name, arguments = "lookup", `{"q":"a"}`
	published, err := store.Publish(responsesChatReplayPublishRequest{
		Route:            route,
		AssistantContent: json.RawMessage(`"checking"`),
		OutputItems: []json.RawMessage{
			json.RawMessage(`{"type":"reasoning","id":"rs_self","encrypted_content":"OPAQUE","content":[],"summary":[]}`),
			responsesFunctionCallItem(upstreamCallID, name, arguments),
		},
		Calls: []responsesChatReplayPublishCall{{
			UpstreamCallID: upstreamCallID, Name: name, VisibleArguments: arguments, OutputItemIndex: 1,
		}},
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	callID := published.Projection.Calls[0].ID
	body, err := json.Marshal(map[string]any{
		"model": route.PublicModel,
		"messages": []any{
			map[string]any{"role": "assistant", "content": "checking", "tool_calls": []any{map[string]any{
				"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": arguments},
			}}},
			map[string]any{"role": "tool", "tool_call_id": callID, "content": "result-1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return callID, body
}

func selfDescribingRoute() responsesChatReplayRoute {
	return responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
}

// A store that never held the group, which is what a long session's store amounts to once
// the TTL or the LRU has taken the turn back.
func forgottenReplayStore(t *testing.T) *responsesChatReplayStore {
	t.Helper()
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func restoredFunctionCallOutput(t *testing.T, input string) map[string]any {
	t.Helper()
	var items []map[string]any
	if err := json.Unmarshal([]byte(input), &items); err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item["type"] == "function_call_output" {
			return item
		}
	}
	t.Fatalf("no function_call_output item reached upstream: %s", input)
	return nil
}

// The session that wedged: the store no longer holds the group and the client resent no
// carrier for this turn -- the measured log read carrier="absent" carried_turns=12 against a
// transcript holding 516. All that is left is the ID, which the client always echoes because
// it is the tool_use.id. It has to be enough to name Copilot's call on its own; that is what
// makes responses_replay_state_missing unreachable rather than merely survivable.
func TestForgottenTurnStillNamesItsUpstreamCallFromTheIDAlone(t *testing.T) {
	route := selfDescribingRoute()
	minting := forgottenReplayStore(t)
	callID, body := selfDescribingFixture(t, minting, route, copilotUpstreamCallID)

	var logs bytes.Buffer
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
		UpstreamModel:             "gpt-upstream",
		ReplayStore:               forgottenReplayStore(t),
		ReplayRoute:               route,
		CarriedReasoning:          nil,
		DegradeUnrestorableReplay: true,
		Log:                       logger.NewWithWriter(logger.LevelInfo, &logs),
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(callID, copilotUpstreamCallID) {
		t.Fatalf("minted ID %q does not carry its upstream ID %q", callID, copilotUpstreamCallID)
	}
	input := upstreamInputJSON(t, plan)
	if got := restoredFunctionCall(t, input); got["call_id"] != copilotUpstreamCallID {
		t.Fatalf("forgotten turn reached upstream as %q, want Copilot's own %q: %s", got["call_id"], copilotUpstreamCallID, input)
	}
	// A call and its output that disagree is the failure this replaces, not an improvement.
	if got := restoredFunctionCallOutput(t, input); got["call_id"] != copilotUpstreamCallID {
		t.Fatalf("tool result answered %q, want %q: %s", got["call_id"], copilotUpstreamCallID, input)
	}
	// The turn survived, so nothing else reports that both lookups came up empty.
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatalf("unmarshal %q: %v", logs.String(), err)
	}
	if entry["carrier"] != "absent" || entry["diverged"] != "store_missing" {
		t.Fatalf("log = carrier %#v diverged %#v, want \"absent\" and \"store_missing\" in %#v", entry["carrier"], entry["diverged"], entry)
	}
	for _, leaked := range []string{"checking", "lookup", `{"q":"a"}`, "result-1"} {
		if strings.Contains(logs.String(), leaked) {
			t.Fatalf("restore log leaked prompt data %q: %s", leaked, logs.String())
		}
	}
}

// Answering from the ID reads nothing but the ID, so it cannot tell one candidate target
// from another. prepareExplicitResponsesChatRequest tells them apart by which one refuses
// the transcript, so a tier that refuses for nobody would pin the first candidate every
// time -- and skip the target still holding the group, along with its reasoning. It has to
// stay behind the degrade retry, which runs only once every candidate has already refused.
func TestSelfDescribingIDDoesNotAnswerBeforeEveryTargetHasRefused(t *testing.T) {
	route := selfDescribingRoute()
	_, body := selfDescribingFixture(t, forgottenReplayStore(t), route, copilotUpstreamCallID)

	_, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
		UpstreamModel:             "gpt-upstream",
		ReplayStore:               forgottenReplayStore(t),
		ReplayRoute:               route,
		DegradeUnrestorableReplay: false,
	})
	if !isMissingResponsesChatReplayError(err) {
		t.Fatalf("first pass resolved from the ID alone (err = %v); the target probe no longer discriminates", err)
	}
}

// The store holds Copilot's reasoning; the ID holds the mapping and nothing else. Answering
// from the ID first would trade quality for reach on turns that were never lost, and would
// do it silently -- every assertion about call IDs still passes.
func TestStoreStillAnswersAheadOfTheID(t *testing.T) {
	route := selfDescribingRoute()
	store := forgottenReplayStore(t)
	_, body := selfDescribingFixture(t, store, route, copilotUpstreamCallID)

	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
		UpstreamModel:             "gpt-upstream",
		ReplayStore:               store,
		ReplayRoute:               route,
		DegradeUnrestorableReplay: true,
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	input := upstreamInputJSON(t, plan)
	if !strings.Contains(input, `"encrypted_content":"OPAQUE"`) {
		t.Fatalf("the ID answered ahead of the store and dropped Copilot's reasoning: %s", input)
	}
	if got := restoredFunctionCall(t, input); got["call_id"] != copilotUpstreamCallID {
		t.Fatalf("stored turn reached upstream as %q, want %q: %s", got["call_id"], copilotUpstreamCallID, input)
	}
}

// Sessions already recorded hold random minted IDs that can never be rewritten, so an
// upstream ID the minter cannot embed must still produce the legacy form and still resolve
// through the store, reasoning and all.
func TestLegacyRandomIDsKeepResolvingThroughTheStore(t *testing.T) {
	route := selfDescribingRoute()
	store := forgottenReplayStore(t)
	// No "call_" marker, so the minter declines to embed it and falls back to random.
	const upstreamCallID = "upstream-call-1"
	callID, body := selfDescribingFixture(t, store, route, upstreamCallID)

	if len(callID) != responsesChatReplayIDLength {
		t.Fatalf("minted %q (%d chars), want the legacy %d-char form", callID, len(callID), responsesChatReplayIDLength)
	}
	if !isResponsesChatReplayCallID(callID) {
		t.Fatalf("legacy minted ID %q is no longer recognised as a replay ID", callID)
	}
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
		UpstreamModel:             "gpt-upstream",
		ReplayStore:               store,
		ReplayRoute:               route,
		DegradeUnrestorableReplay: true,
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	input := upstreamInputJSON(t, plan)
	if !strings.Contains(input, `"encrypted_content":"OPAQUE"`) {
		t.Fatalf("legacy replay lost its reasoning: %s", input)
	}
	if got := restoredFunctionCall(t, input); got["call_id"] != upstreamCallID {
		t.Fatalf("legacy replay reached upstream as %q, want %q: %s", got["call_id"], upstreamCallID, input)
	}
}

// Two calls in one turn each keep their own upstream ID, so a parallel tool-call group does
// not collapse onto one call_id when it comes back from the ID alone.
func TestParallelCallsEachCarryTheirOwnUpstreamID(t *testing.T) {
	route := selfDescribingRoute()
	store := forgottenReplayStore(t)
	first, second := copilotUpstreamCallID, "call_SECONDpatm0UsLiCALKyjb6"
	published, err := store.Publish(responsesChatReplayPublishRequest{
		Route:            route,
		AssistantContent: json.RawMessage(`"checking"`),
		OutputItems: []json.RawMessage{
			responsesFunctionCallItem(first, "lookup", `{"q":"a"}`),
			responsesFunctionCallItem(second, "lookup", `{"q":"b"}`),
		},
		Calls: []responsesChatReplayPublishCall{
			{UpstreamCallID: first, Name: "lookup", VisibleArguments: `{"q":"a"}`, OutputItemIndex: 0},
			{UpstreamCallID: second, Name: "lookup", VisibleArguments: `{"q":"b"}`, OutputItemIndex: 1},
		},
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	calls := published.Projection.Calls
	body, err := json.Marshal(map[string]any{
		"model": route.PublicModel,
		"messages": []any{
			map[string]any{"role": "assistant", "content": "checking", "tool_calls": []any{
				map[string]any{"id": calls[0].ID, "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"a"}`}},
				map[string]any{"id": calls[1].ID, "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"b"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": calls[0].ID, "content": "result-a"},
			map[string]any{"role": "tool", "tool_call_id": calls[1].ID, "content": "result-b"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
		UpstreamModel:             "gpt-upstream",
		ReplayStore:               forgottenReplayStore(t),
		ReplayRoute:               route,
		DegradeUnrestorableReplay: true,
	})
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	input := upstreamInputJSON(t, plan)
	for _, upstream := range []string{first, second} {
		if !strings.Contains(input, `"call_id":"`+upstream+`"`) {
			t.Fatalf("parallel group lost %q on the way upstream: %s", upstream, input)
		}
	}
}
