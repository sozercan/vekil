package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/sozercan/vekil/logger"
)

type trimmableTurn struct {
	proxyID    string
	upstreamID string
	reasoning  []json.RawMessage
	call       json.RawMessage
	tail       []json.RawMessage
}

func (turn trimmableTurn) items() []json.RawMessage {
	return append(append(append([]json.RawMessage{}, turn.reasoning...), turn.call), turn.tail...)
}

// Reasoning items per turn vary, so "newest 100 tool turns" cannot pass as "newest 100
// reasoning items"; prose and user messages sit between turns, so a tool turn cannot pass
// as any assistant message. Both distinctions are the point of the window.
func publishTrimmableTurns(t *testing.T, store *responsesChatReplayStore, route responsesChatReplayRoute, count int, carrier bool) ([]json.RawMessage, []trimmableTurn, map[string]carriedReplay) {
	t.Helper()
	messages := make([]json.RawMessage, 0, count*4)
	turns := make([]trimmableTurn, 0, count)
	carried := make(map[string]carriedReplay, count)
	for i := range count {
		upstreamID := fmt.Sprintf("upstream-call-%d", i)
		arguments := fmt.Sprintf(`{"turn":%d}`, i)
		outputItems := make([]json.RawMessage, 0, i%3+2)
		for j := range i%3 + 1 {
			outputItems = append(outputItems, json.RawMessage(fmt.Sprintf(
				`{"type":"reasoning","id":"rs-%d-%d","encrypted_content":"cipher-%d-%d-%s","summary":[],"content":[]}`,
				i, j, i, j, strings.Repeat("z", 64))))
		}
		callIndex := len(outputItems)
		outputItems = append(outputItems, json.RawMessage(fmt.Sprintf(
			`{"type":"function_call","call_id":%q,"name":"probe","arguments":%q,"status":"completed"}`,
			upstreamID, arguments)))
		published, err := store.Publish(responsesChatReplayPublishRequest{
			Route:            route,
			AssistantContent: json.RawMessage(`null`),
			OutputItems:      outputItems,
			Calls: []responsesChatReplayPublishCall{{
				UpstreamCallID:   upstreamID,
				Name:             "probe",
				VisibleArguments: arguments,
				OutputItemIndex:  callIndex,
			}},
		})
		if err != nil {
			t.Fatalf("Publish() error = %v", err)
		}
		projected := published.Projection.Calls[0]
		turn := trimmableTurn{proxyID: projected.ID, upstreamID: upstreamID}
		for j := range i%3 + 1 {
			turn.reasoning = append(turn.reasoning, json.RawMessage(fmt.Sprintf(
				`{"content":[],"encrypted_content":"cipher-%d-%d-%s","id":"rs-%d-%d","summary":[],"type":"reasoning"}`,
				i, j, strings.Repeat("z", 64), i, j)))
		}
		expectedCallID := upstreamID
		if carrier {
			expectedCallID = projected.ID
		}
		turn.call = json.RawMessage(fmt.Sprintf(
			`{"arguments":%q,"call_id":%q,"name":"probe","type":"function_call"}`, arguments, expectedCallID))
		if !carrier {
			turn.reasoning = append([]json.RawMessage{}, outputItems[:callIndex]...)
			turn.call = outputItems[callIndex]
		}
		turn.tail = []json.RawMessage{
			json.RawMessage(fmt.Sprintf(`{"call_id":%q,"output":"tool-output-%d","type":"function_call_output"}`, expectedCallID, i)),
			json.RawMessage(fmt.Sprintf(`{"content":[{"text":"ping-%d","type":"input_text"}],"role":"user","type":"message"}`, i)),
			json.RawMessage(fmt.Sprintf(`{"content":"note-%d","role":"assistant"}`, i)),
		}
		if carrier {
			carried[projected.ID] = carriedReplay{
				Items:            clientSafeCarrierItems(outputItems),
				Calls:            map[string]carriedCall{projected.ID: {ProxyID: projected.ID, Name: "probe", ItemIndex: callIndex}},
				RouteDigest:      carriedRouteDigest(route),
				ProjectionDigest: carriedProjectionDigest([]byte(`""`), published.Projection.Calls),
			}
		}
		assistant, err := json.Marshal(map[string]any{
			"role":    "assistant",
			"content": nil,
			"tool_calls": []any{map[string]any{
				"id":       projected.ID,
				"type":     "function",
				"function": map[string]any{"name": projected.Name, "arguments": projected.Arguments},
			}},
		})
		if err != nil {
			t.Fatal(err)
		}
		tool, err := json.Marshal(map[string]any{"role": "tool", "tool_call_id": projected.ID, "content": fmt.Sprintf("tool-output-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		user, err := json.Marshal(map[string]any{"role": "user", "content": fmt.Sprintf("ping-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		prose, err := json.Marshal(map[string]any{"role": "assistant", "content": fmt.Sprintf("note-%d", i)})
		if err != nil {
			t.Fatal(err)
		}
		messages = append(messages, assistant, tool, user, prose)
		turns = append(turns, turn)
	}
	return messages, turns, carried
}

// The store and the carrier are the two reasoning sources and both reach the same trim.
func trimmableTurnsUpstreamInput(t *testing.T, count int, carrier bool, options *responsesChatRequestOptions) ([]byte, []trimmableTurn) {
	t.Helper()
	store, route := newCarrierReplayFixture(t)
	messages, turns, carried := publishTrimmableTurns(t, store, route, count, carrier)
	if options == nil {
		options = &responsesChatRequestOptions{}
	}
	options.UpstreamModel = "gpt-upstream"
	options.ReplayRoute = route
	if carrier {
		// Production sets both and the store answers first, so the carrier only ever runs
		// behind a store that has already lost the group: wire one that never held it.
		expired := newResponsesChatReplayStore()
		t.Cleanup(func() { _ = expired.Close() })
		options.CarriedReasoning, options.ReplayStore = carried, expired
	} else {
		options.ReplayStore = store
	}
	input, err := translateChatMessagesToResponses(messages, *options)
	if err != nil {
		t.Fatalf("translateChatMessagesToResponses() error = %v", err)
	}
	encoded, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	return encoded, turns
}

func forEachReasoningSource(t *testing.T, run func(t *testing.T, carrier bool)) {
	t.Helper()
	for _, source := range []struct {
		name    string
		carrier bool
	}{{"store", false}, {"carrier", true}} {
		t.Run(source.name, func(t *testing.T) { run(t, source.carrier) })
	}
}

// 250 turns sits inside block 2, so the cutoff is a flat 100 turns and 150 are retained:
// the window is N..2N-1, not N.
func TestTranslateChatMessagesToResponsesDropsReasoningOlderThanRetainedToolTurns(t *testing.T) {
	forEachReasoningSource(t, func(t *testing.T, carrier bool) {
		aged := reasoningToolTurnBlock
		encoded, turns := trimmableTurnsUpstreamInput(t, 2*reasoningToolTurnBlock+50, carrier, nil)
		for i, turn := range turns {
			retained := i >= aged
			for j, item := range turn.reasoning {
				if bytes.Contains(encoded, item) != retained {
					t.Fatalf("turn %d reasoning[%d] present = %v, want %v", i, j, !retained, retained)
				}
			}
		}
	})
}

func TestTranslateChatMessagesToResponsesKeepsToolCallsAtEveryAge(t *testing.T) {
	forEachReasoningSource(t, func(t *testing.T, carrier bool) {
		encoded, turns := trimmableTurnsUpstreamInput(t, 2*reasoningToolTurnBlock+50, carrier, nil)
		previous := -1
		for i, turn := range turns {
			call := bytes.Index(encoded, turn.call)
			output := bytes.Index(encoded, turn.tail[0])
			if call < 0 || output < 0 {
				t.Fatalf("turn %d call=%d output=%d, want both present", i, call, output)
			}
			if output < call || previous >= call {
				t.Fatalf("turn %d order: previous=%d call=%d output=%d", i, previous, call, output)
			}
			previous = output
		}
	})
}

// The whole upstream input, byte for byte: below the first block jump the trim must splice
// exactly what it spliced before it existed, and say nothing.
func TestTranslateChatMessagesToResponsesLeavesRetainedToolTurnsByteIdentical(t *testing.T) {
	forEachReasoningSource(t, func(t *testing.T, carrier bool) {
		for _, count := range []int{reasoningToolTurnBlock, 2*reasoningToolTurnBlock - 1} {
			t.Run(fmt.Sprintf("turns-%d", count), func(t *testing.T) {
				var sink bytes.Buffer
				options := responsesChatRequestOptions{Log: logger.NewWithWriter(logger.LevelDebug, &sink)}
				encoded, turns := trimmableTurnsUpstreamInput(t, count, carrier, &options)
				var expected []json.RawMessage
				for _, turn := range turns {
					expected = append(expected, turn.items()...)
				}
				want, err := json.Marshal(expected)
				if err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(encoded, want) {
					t.Fatalf("upstream input = %s\nwant %s", encoded, want)
				}
				if sink.Len() != 0 {
					t.Fatalf("untrimmed conversation logged %q", sink.String())
				}
			})
		}
	})
}

// Every request in a block must extend the previous one byte for byte; the cutoff, and so
// the array, may only move where the block does.
func TestTranslateChatMessagesToResponsesKeepsUpstreamInputPrefixStableWithinBlock(t *testing.T) {
	// Block 2 is the first jump, where the earlier array is still untrimmed; block 3 is the
	// steady state, where the cutoff is indexed on both sides. Step 7 is a client resuming
	// several turns on, where a jump need not land on a multiple of the block.
	sweeps := []struct{ first, last, step int }{
		{2*reasoningToolTurnBlock - 10, 2*reasoningToolTurnBlock + 15, 1},
		{3*reasoningToolTurnBlock - 10, 3*reasoningToolTurnBlock + 15, 1},
		{3*reasoningToolTurnBlock - 14, 3*reasoningToolTurnBlock + 21, 7},
	}
	forEachReasoningSource(t, func(t *testing.T, carrier bool) {
		for _, sweep := range sweeps {
			t.Run(fmt.Sprintf("from-%d-step-%d", sweep.first, sweep.step), func(t *testing.T) {
				store, route := newCarrierReplayFixture(t)
				messages, _, carried := publishTrimmableTurns(t, store, route, sweep.last, carrier)
				options := responsesChatRequestOptions{
					UpstreamModel: "gpt-upstream",
					ReplayRoute:   route,
				}
				if carrier {
					// Use one forgotten store for every prefix. A growing real transcript
					// reuses the opaque IDs it was originally handed; reminting each prefix
					// would make byte-prefix comparison meaningless.
					expired := newResponsesChatReplayStore()
					t.Cleanup(func() { _ = expired.Close() })
					options.CarriedReasoning = carried
					options.ReplayStore = expired
				} else {
					options.ReplayStore = store
				}
				var previous []byte
				previousCount := 0
				for count := sweep.first; count <= sweep.last; count += sweep.step {
					input, err := translateChatMessagesToResponses(messages[:count*4], options)
					if err != nil {
						t.Fatalf("translateChatMessagesToResponses() error = %v", err)
					}
					encoded, err := json.Marshal(input)
					if err != nil {
						t.Fatal(err)
					}
					if previous != nil {
						// "[a,b]" extends to "[a,b,c]": drop the close, require the separator, so a
						// break can only land where a whole new item starts.
						want := append(append([]byte{}, previous[:len(previous)-1]...), ',')
						extends := bytes.HasPrefix(encoded, want)
						// Derived here rather than from agedReasoningToolTurns, so a cutoff that
						// slid again would be caught instead of agreed with.
						held := previousCount/reasoningToolTurnBlock == count/reasoningToolTurnBlock
						if extends != held {
							t.Fatalf("turns %d -> %d: extends previous = %v, want %v", previousCount, count, extends, held)
						}
						// A break must be the cutoff dropping a block of reasoning; one that grew
						// the array is some other divergence wearing the same shape.
						if !held && len(encoded) >= len(previous) {
							t.Fatalf("turns %d -> %d: array grew %d -> %d, want a drop", previousCount, count, len(previous), len(encoded))
						}
					}
					previous, previousCount = encoded, count
				}
			})
		}
	})
}

// Two properties the marshaled arrays cannot show: the cutoff never indexes past the turns
// it was given, and it is a function of the block rather than of the exact count.
func TestAgedReasoningToolTurnsHoldsStillWithinBlock(t *testing.T) {
	for _, count := range []int{0, 1, 99, 100, 199, 200, 201, 299, 300, 1516} {
		aged := agedReasoningToolTurns(count)
		if count > 0 && aged >= count {
			t.Fatalf("turns %d: aged = %d, want < %d", count, aged, count)
		}
		if count < 2*reasoningToolTurnBlock && aged != 0 {
			t.Fatalf("turns %d: aged = %d, want 0 below the first jump", count, aged)
		}
		if start := agedReasoningToolTurns(count / reasoningToolTurnBlock * reasoningToolTurnBlock); start != aged {
			t.Fatalf("turns %d: aged = %d but %d at the block start, so the cutoff slides", count, aged, start)
		}
		if retained, floor := count-aged, min(count, reasoningToolTurnBlock); retained < floor {
			t.Fatalf("turns %d: retained %d turns, want at least %d", count, retained, floor)
		}
	}
}

func TestTranslateChatMessagesToResponsesLogsReasoningTrimWithoutContent(t *testing.T) {
	var sink bytes.Buffer
	aged := reasoningToolTurnBlock
	options := responsesChatRequestOptions{Log: logger.NewWithWriter(logger.LevelDebug, &sink)}
	_, turns := trimmableTurnsUpstreamInput(t, 2*reasoningToolTurnBlock+50, false, &options)

	var entry map[string]any
	if err := json.Unmarshal(sink.Bytes(), &entry); err != nil {
		t.Fatalf("log entry %q: %v", sink.String(), err)
	}
	dropped, droppedBytes, retained := 0, 0, 0
	for i, turn := range turns {
		if i < aged {
			dropped += len(turn.reasoning)
			for _, item := range turn.reasoning {
				droppedBytes += len(item)
			}
			continue
		}
		retained += len(turn.reasoning)
	}
	for field, want := range map[string]float64{
		"tool_turns":               float64(len(turns)),
		"aged_turns":               float64(aged),
		"retained_turns":           float64(len(turns) - aged),
		"reasoning_items":          float64(dropped),
		"reasoning_bytes":          float64(droppedBytes),
		"retained_reasoning_items": float64(retained),
	} {
		if entry[field] != want {
			t.Fatalf("log %s = %v, want %v", field, entry[field], want)
		}
	}
	for _, secret := range []string{"cipher-", "tool-output-", "ping-", "note-", "probe", `\"turn\"`, turns[0].proxyID, turns[0].upstreamID} {
		if strings.Contains(sink.String(), secret) {
			t.Fatalf("log leaked %q: %s", secret, sink.String())
		}
	}
}
