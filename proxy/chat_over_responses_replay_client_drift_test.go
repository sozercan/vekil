package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

// clientDriftFixture publishes one turn, hands back the carrier the client would hold,
// and a Chat body replaying that turn with `returned` as the call's arguments.
func clientDriftFixture(t *testing.T, store *responsesChatReplayStore, route responsesChatReplayRoute, name, emitted, returned string) (map[string]carriedReplay, []byte, string) {
	t.Helper()
	items := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_drift","encrypted_content":"OPAQUE","content":[],"summary":[]}`),
		responsesFunctionCallItem("upstream-call-1", name, emitted),
	}
	published, err := store.Publish(responsesChatReplayPublishRequest{
		Route:            route,
		AssistantContent: json.RawMessage(`"checking"`),
		OutputItems:      items,
		Calls: []responsesChatReplayPublishCall{{
			UpstreamCallID: "upstream-call-1", Name: name, VisibleArguments: emitted, OutputItemIndex: 1,
		}},
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	callID := published.Projection.Calls[0].ID

	resp := prependCarriedReasoning(&models.AnthropicResponse{
		Content: []models.ContentBlock{{Type: "tool_use", ID: callID, Name: name, Input: json.RawMessage(emitted)}},
	}, carriedTurnFromPublished(route, items, published, carrierEmit{}))
	blocks, err := json.Marshal(resp.Content)
	if err != nil {
		t.Fatal(err)
	}
	carried, _ := extractCarriedReasoning([]models.AnthropicMessage{{Role: "assistant", Content: blocks}})
	if _, ok := carried[callID]; !ok {
		t.Fatalf("fixture emitted no usable carrier for %s", callID)
	}

	body, err := json.Marshal(map[string]any{
		"model": route.PublicModel,
		"messages": []any{
			map[string]any{"role": "assistant", "content": "checking", "tool_calls": []any{map[string]any{
				"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": returned},
			}}},
			map[string]any{"role": "tool", "tool_call_id": callID, "content": "result-1"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return carried, body, callID
}

// requireStoreRejectsArguments keeps the fixture honest: if the store ever starts
// accepting the client's rewrite, these tests must fail rather than pass vacuously.
func requireStoreRejectsArguments(t *testing.T, store *responsesChatReplayStore, route responsesChatReplayRoute, callID, name, returned string) {
	t.Helper()
	_, err := resolveResponsesChatReplay(store, route, responsesChatReplayAssistantProjection{
		Content: json.RawMessage(`"checking"`),
		Calls:   []responsesChatReplayProjectedCall{{ID: callID, Name: name, Arguments: returned}},
	})
	if !errors.Is(err, &responsesChatReplayProjectionError{}) {
		t.Fatalf("store accepted the drifted arguments (err = %v); fixture no longer reproduces the drift", err)
	}
}

// Claude Code rewrites tool_use.input between turns and returns the rewrite. Every case
// below is taken from a live gpt-5.6-sol session; the store binds arguments and rejects
// all of them, so reasoning continuity has to come from the carrier, which does not.
func TestClientRewrittenArgumentsStillRestoreCarriedReasoning(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		tool     string
		emitted  string
		returned string
	}{{
		name:     "materialised schema default",
		tool:     "Edit",
		emitted:  `{"file_path":"/tmp/a","new_string":"b","old_string":"a"}`,
		returned: `{"file_path":"/tmp/a","new_string":"b","old_string":"a","replace_all":false}`,
	}, {
		name:     "client-invented alias keys",
		tool:     "SendMessage",
		emitted:  `{"message":"go","summary":"s","to":"agent"}`,
		returned: `{"content":"go","message":"go","recipient":"agent","summary":"s","to":"agent","type":"message"}`,
	}, {
		name:     "field the client generated after the call",
		tool:     "ExitPlanMode",
		emitted:  `{"allowedPrompts":[{"prompt":"build","tool":"Bash"}]}`,
		returned: `{"allowedPrompts":[{"prompt":"build","tool":"Bash"}],"plan":"# Plan","planFilePath":"/Users/mo/.claude/plans/x.md"}`,
	}, {
		name:     "text the client edited",
		tool:     "Write",
		emitted:  `{"content":"line \n","file_path":"/tmp/p.patch"}`,
		returned: `{"content":"line\n","file_path":"/tmp/p.patch"}`,
	}} {
		t.Run(testCase.name, func(t *testing.T) {
			store := newResponsesChatReplayStore()
			t.Cleanup(func() { _ = store.Close() })
			route := responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
			carried, body, callID := clientDriftFixture(t, store, route, testCase.tool, testCase.emitted, testCase.returned)
			requireStoreRejectsArguments(t, store, route, callID, testCase.tool, testCase.returned)

			plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
				UpstreamModel: "gpt-upstream", ReplayStore: store, ReplayRoute: route, CarriedReasoning: carried,
			})
			if err != nil {
				t.Fatalf("translate: %v", err)
			}
			// Assert on the wire bytes: a decoded item cannot show an omitempty field
			// dropped on the way out, and the ciphertext is exactly such a field.
			input := upstreamInputJSON(t, plan)
			if !strings.Contains(input, `"encrypted_content":"OPAQUE"`) {
				t.Fatalf("client argument drift threw away reasoning continuity: %s", input)
			}
			// The output item carries the same call_id, so read the binding off the call itself.
			if got := restoredFunctionCall(t, input); got["call_id"] != "upstream-call-1" {
				t.Fatalf("restored turn lost its upstream call binding: %s", input)
			} else if got["arguments"] != testCase.returned {
				// The client's arguments are what upstream must see, not the stored ones.
				t.Fatalf("restored call forwarded %q, want the client's %q", got["arguments"], testCase.returned)
			}
		})
	}
}

func restoredFunctionCall(t *testing.T, input string) map[string]any {
	t.Helper()
	var items []map[string]any
	if err := json.Unmarshal([]byte(input), &items); err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item["type"] == "function_call" {
			return item
		}
	}
	t.Fatalf("no function_call item reached upstream: %s", input)
	return nil
}

// A degrade must name both sides from evidence vekil owns: what the store rejected, and
// which carrier guard then refused. Neither may be inferred from anything the client sent.
func TestDegradeLogNamesBothSidesFromVekilsOwnGuards(t *testing.T) {
	emitted := `{"file_path":"/tmp/a","new_string":"b","old_string":"a"}`
	returned := `{"file_path":"/tmp/a","new_string":"b","old_string":"a","replace_all":false}`
	for _, testCase := range []struct {
		name        string
		body        func(string) string
		carrier     func(*testing.T, map[string]carriedReplay, responsesChatReplayRoute) map[string]carriedReplay
		diverged    string
		wantCarrier string
		published   bool
	}{{
		name: "arguments rewritten, carrier minted elsewhere",
		carrier: func(t *testing.T, c map[string]carriedReplay, r responsesChatReplayRoute) map[string]carriedReplay {
			r.UpstreamModel = "gpt-other"
			return reroutedCarrier(t, c, r)
		},
		diverged:    "arguments",
		wantCarrier: "route",
		published:   true,
	}, {
		name: "arguments rewritten, no carrier at all",
		carrier: func(*testing.T, map[string]carriedReplay, responsesChatReplayRoute) map[string]carriedReplay {
			return nil
		},
		diverged:    "arguments",
		wantCarrier: "absent",
		published:   true,
	}, {
		name:        "assistant text rewritten",
		body:        func(b string) string { return strings.Replace(b, `"content":"checking"`, `"content":"CHANGED"`, 1) },
		diverged:    "content",
		wantCarrier: "projection",
	}, {
		name:        "call renamed",
		body:        func(b string) string { return strings.Replace(b, `"name":"Edit"`, `"name":"Renamed"`, 1) },
		diverged:    "calls",
		wantCarrier: "projection",
	}} {
		t.Run(testCase.name, func(t *testing.T) {
			store := newResponsesChatReplayStore()
			t.Cleanup(func() { _ = store.Close() })
			route := responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
			carried, body, callID := clientDriftFixture(t, store, route, "Edit", emitted, returned)
			requireStoreRejectsArguments(t, store, route, callID, "Edit", returned)
			if testCase.body != nil {
				mutated := testCase.body(string(body))
				if mutated == string(body) {
					t.Fatal("fixture no longer carries the text this case rewrites")
				}
				body = []byte(mutated)
			}
			if testCase.carrier != nil {
				carried = testCase.carrier(t, carried, route)
			}

			var logs bytes.Buffer
			if _, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
				UpstreamModel: "gpt-upstream", ReplayStore: store, ReplayRoute: route,
				DegradeUnrestorableReplay: true,
				CarriedReasoning:          carried,
				Log:                       logger.NewWithWriter(logger.LevelInfo, &logs),
			}); err != nil {
				t.Fatalf("translate: %v", err)
			}
			var entry map[string]any
			if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
				t.Fatalf("unmarshal %q: %v", logs.String(), err)
			}
			if entry["diverged"] != testCase.diverged || entry["carrier"] != testCase.wantCarrier {
				t.Fatalf("log = diverged %#v carrier %#v, want %q and %q in %#v",
					entry["diverged"], entry["carrier"], testCase.diverged, testCase.wantCarrier, entry)
			}
			// Only an untouched projection reproduces the digest the turn was published under.
			fingerprint, _ := entry["projection"].(string)
			published := carriedProjectionDigest(canonicalDriftContent(t), []responsesChatReplayProjectedCall{{ID: callID, Name: "Edit", Arguments: returned}})
			if fingerprint == "" || (fingerprint == published) != testCase.published {
				t.Fatalf("projection hash %q matches published = %v, want %v in %#v", fingerprint, fingerprint == published, testCase.published, entry)
			}
			for _, leaked := range []string{"checking", "CHANGED", "Renamed", "old_string", "/tmp/a"} {
				if strings.Contains(logs.String(), leaked) {
					t.Fatalf("degrade log leaked prompt data %q: %s", leaked, logs.String())
				}
			}
		})
	}
}

func canonicalDriftContent(t *testing.T) []byte {
	t.Helper()
	canonical, err := canonicalReplayJSONValue(json.RawMessage(`"checking"`))
	if err != nil {
		t.Fatal(err)
	}
	return canonical
}

func reroutedCarrier(t *testing.T, carried map[string]carriedReplay, route responsesChatReplayRoute) map[string]carriedReplay {
	t.Helper()
	rerouted := make(map[string]carriedReplay, len(carried))
	for id, replay := range carried {
		replay.RouteDigest = carriedRouteDigest(route)
		rerouted[id] = replay
	}
	return rerouted
}

// A digest is client input; one that is not ours must not travel into a restore key.
func TestCarrierProjectionDigestMustLookLikeADigest(t *testing.T) {
	route := responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public"}
	items := []json.RawMessage{json.RawMessage(`{"type":"function_call","call_id":"upstream-call-1","name":"lookup","arguments":"{}"}`)}
	for _, claimed := range []string{"not-a-hash", strings.Repeat("z", 32), strings.Repeat("ab", 4096)} {
		signature, err := encodeReasoningCarrier(carriedTurn{
			Items: items, Route: route, Projection: claimed,
			Calls: []carriedCall{{ProxyID: "call_vekil_x", UpstreamID: "upstream-call-1", Name: "lookup"}},
		})
		if err != nil {
			t.Fatal(err)
		}
		replay, ok := decodeReasoningCarrier(signature, nil)
		if !ok {
			t.Fatalf("carrier claiming %q did not decode", claimed)
		}
		if replay.ProjectionDigest != "" {
			t.Fatalf("decoded projection digest = %q, want it dropped", replay.ProjectionDigest)
		}
	}
}
