package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/sozercan/vekil/logger"
)

const selfDescribingTestUpstreamID = "call_testUpstream123"

func selfDescribingTestRoute() responsesChatReplayRoute {
	return responsesChatReplayRoute{
		ProviderID:    "provider-a",
		PublicModel:   "gpt-public",
		UpstreamModel: "gpt-upstream",
	}
}

func selfDescribingTestStore(t *testing.T) *responsesChatReplayStore {
	t.Helper()
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func selfDescribingTestFixture(t *testing.T, store *responsesChatReplayStore, route responsesChatReplayRoute, upstreamCallID string) (string, []byte) {
	t.Helper()
	published, err := store.Publish(responsesChatReplayPublishRequest{
		Route:            route,
		AssistantContent: json.RawMessage(`"checking"`),
		OutputItems: []json.RawMessage{
			json.RawMessage(`{"type":"reasoning","id":"rs_test","encrypted_content":"OPAQUE","content":[],"summary":[]}`),
			responsesFunctionCallItem(upstreamCallID, "lookup", `{"q":"a"}`),
		},
		Calls: []responsesChatReplayPublishCall{{
			UpstreamCallID: upstreamCallID, Name: "lookup", VisibleArguments: `{"q":"a"}`, OutputItemIndex: 1,
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
				"id": callID, "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"a"}`},
			}}},
			map[string]any{"role": "tool", "tool_call_id": callID, "content": "result-a"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return callID, body
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
	t.Fatalf("no function_call_output reached upstream: %s", input)
	return nil
}

func TestSelfDescribingReplayIDRoundTrip(t *testing.T) {
	if got, want := responsesChatReplaySelfIDVersion, "v1_"; got != want {
		t.Fatalf("self-describing replay ID version = %q, want %q", got, want)
	}
	for _, testCase := range []struct {
		name     string
		upstream string
		embed    bool
	}{
		{name: "normal Copilot ID", upstream: selfDescribingTestUpstreamID, embed: true},
		{name: "shortest", upstream: "call_x", embed: true},
		{name: "exact Anthropic limit", upstream: "call_" + strings.Repeat("x", 24), embed: true},
		{name: "over Anthropic limit", upstream: "call_" + strings.Repeat("x", 25)},
		{name: "empty suffix", upstream: "call_"},
		{name: "wrong marker", upstream: "upstream-call-1"},
		{name: "illegal character", upstream: "call_a:b"},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			minted, ok := responsesChatReplaySelfDescribingID(testCase.upstream)
			if ok != testCase.embed {
				t.Fatalf("mint(%q) embedded = %v, want %v", testCase.upstream, ok, testCase.embed)
			}
			if !ok {
				return
			}
			if len(minted) > responsesChatReplayMaxIDLength || !isResponsesChatReplayCallID(minted) {
				t.Fatalf("minted invalid replay ID %q", minted)
			}
			if got, resolved := responsesChatReplayUpstreamCallID(minted); !resolved || got != testCase.upstream {
				t.Fatalf("resolve(%q) = %q, %v, want %q, true", minted, got, resolved, testCase.upstream)
			}
			if size := responsesChatReplayMintedIDSize(testCase.upstream); size < len(minted) || size < responsesChatReplayIDLength {
				t.Fatalf("accounted %d bytes for possible lengths %d and %d", size, len(minted), responsesChatReplayIDLength)
			}
		})
	}
}

func TestSelfDescribingReplayIDRejectsTamperingAndUnreleasedVersion(t *testing.T) {
	id, ok := responsesChatReplaySelfDescribingID(selfDescribingTestUpstreamID)
	if !ok {
		t.Fatal("fixture upstream ID was not embedded")
	}
	last := id[len(id)-1]
	replacement := byte('A')
	if last == replacement {
		replacement = 'B'
	}
	tampered := id[:len(id)-1] + string(replacement)
	for _, rejected := range []string{
		tampered,
		strings.Replace(id, "v1_", "v2_", 1),
		"call_vekil_call_customer_job",
		"call_vekil_v1_call_customer_job_AAAAAAAA",
	} {
		if _, resolved := responsesChatReplayUpstreamCallID(rejected); resolved {
			t.Errorf("tampered or unminted ID %q resolved", rejected)
		}
		if isResponsesChatReplayCallID(rejected) {
			t.Errorf("tampered or unminted ID %q was recognised", rejected)
		}
	}
}

func TestForgottenTurnRecoversUpstreamMappingFromSelfDescribingID(t *testing.T) {
	route := selfDescribingTestRoute()
	callID, body := selfDescribingTestFixture(t, selfDescribingTestStore(t), route, selfDescribingTestUpstreamID)
	if !strings.Contains(callID, selfDescribingTestUpstreamID) {
		t.Fatalf("minted ID %q does not carry %q", callID, selfDescribingTestUpstreamID)
	}

	var logs bytes.Buffer
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
		UpstreamModel:             route.UpstreamModel,
		ReplayStore:               selfDescribingTestStore(t),
		ReplayRoute:               route,
		DegradeUnrestorableReplay: true,
		Log:                       logger.NewWithWriter(logger.LevelInfo, &logs),
	})
	if err != nil {
		t.Fatalf("translate forgotten turn: %v", err)
	}
	input := upstreamInputJSON(t, plan)
	if got := restoredFunctionCall(t, input)["call_id"]; got != selfDescribingTestUpstreamID {
		t.Fatalf("function call ID = %#v, want %q: %s", got, selfDescribingTestUpstreamID, input)
	}
	if got := restoredFunctionCallOutput(t, input)["call_id"]; got != selfDescribingTestUpstreamID {
		t.Fatalf("function output ID = %#v, want %q: %s", got, selfDescribingTestUpstreamID, input)
	}
	if strings.Contains(input, `"encrypted_content":"OPAQUE"`) {
		t.Fatalf("ID-only restore invented hidden reasoning: %s", input)
	}
	var entry map[string]any
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatalf("decode restore log %q: %v", logs.String(), err)
	}
	if entry["msg"] != "responses replay resolved from self-describing tool-call IDs" ||
		entry["self_describing_turns"] != float64(1) || entry["degraded_turns"] != float64(0) {
		t.Fatalf("unexpected restore log: %#v", entry)
	}
}

func TestSelfDescribingIDDoesNotBypassStoreOrTargetSelection(t *testing.T) {
	route := selfDescribingTestRoute()
	store := selfDescribingTestStore(t)
	_, body := selfDescribingTestFixture(t, store, route, selfDescribingTestUpstreamID)

	storedPlan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
		UpstreamModel: route.UpstreamModel,
		ReplayStore:   store,
		ReplayRoute:   route,
	})
	if err != nil {
		t.Fatalf("translate stored turn: %v", err)
	}
	if input := upstreamInputJSON(t, storedPlan); !strings.Contains(input, `"encrypted_content":"OPAQUE"`) {
		t.Fatalf("self-describing ID preempted the store and dropped reasoning: %s", input)
	}

	_, err = translateChatRequestToResponses(body, responsesChatRequestOptions{
		UpstreamModel: route.UpstreamModel,
		ReplayStore:   selfDescribingTestStore(t),
		ReplayRoute:   route,
		// False during exact-target probing and on native Chat/policy paths.
		DegradeUnrestorableReplay: false,
	})
	if !isMissingResponsesChatReplayError(err) {
		t.Fatalf("ID-only mapping bypassed target selection: %v", err)
	}
}

func TestMixedOpaqueAndSelfDescribingGroupFallsBackAsAWhole(t *testing.T) {
	route := selfDescribingTestRoute()
	store := selfDescribingTestStore(t)
	published, err := store.Publish(responsesChatReplayPublishRequest{
		Route:            route,
		AssistantContent: json.RawMessage(`"checking"`),
		OutputItems: []json.RawMessage{
			responsesFunctionCallItem("call_one", "lookup", `{"q":"a"}`),
			responsesFunctionCallItem("upstream-two", "lookup", `{"q":"b"}`),
		},
		Calls: []responsesChatReplayPublishCall{
			{UpstreamCallID: "call_one", Name: "lookup", VisibleArguments: `{"q":"a"}`, OutputItemIndex: 0},
			{UpstreamCallID: "upstream-two", Name: "lookup", VisibleArguments: `{"q":"b"}`, OutputItemIndex: 1},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	calls := published.Projection.Calls
	if _, ok := responsesChatReplayUpstreamCallID(calls[0].ID); !ok {
		t.Fatalf("first ID %q is not self-describing", calls[0].ID)
	}
	if _, ok := responsesChatReplayUpstreamCallID(calls[1].ID); ok || len(calls[1].ID) != responsesChatReplayIDLength {
		t.Fatalf("second ID %q is not the opaque fallback", calls[1].ID)
	}
	body, err := json.Marshal(map[string]any{
		"model": route.PublicModel,
		"messages": []any{
			map[string]any{"role": "assistant", "content": "checking", "tool_calls": []any{
				map[string]any{"id": calls[0].ID, "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"a"}`}},
				map[string]any{"id": calls[1].ID, "type": "function", "function": map[string]any{"name": "lookup", "arguments": `{"q":"b"}`}},
			}},
			map[string]any{"role": "tool", "tool_call_id": calls[0].ID, "content": "a"},
			map[string]any{"role": "tool", "tool_call_id": calls[1].ID, "content": "b"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
		UpstreamModel:             route.UpstreamModel,
		ReplayStore:               selfDescribingTestStore(t),
		ReplayRoute:               route,
		DegradeUnrestorableReplay: true,
	})
	if err != nil {
		t.Fatalf("translate mixed group: %v", err)
	}
	input := upstreamInputJSON(t, plan)
	for _, call := range calls {
		if strings.Count(input, `"call_id":"`+call.ID+`"`) != 2 {
			t.Fatalf("mixed group did not rebuild call/output under %q: %s", call.ID, input)
		}
	}
}
