package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// Copilot issues item IDs, including a hosted search's, as opaque strings of
// several hundred characters with no prefix.
func conversationCopilotItemID(kind string) string {
	return strings.Repeat("7w/qPNfy"+kind+"AczXpeIX80Wsop55Hf+", 12)
}

func conversationCopilotWebSearchOutput(encrypted string) []any {
	return []any{
		map[string]any{"type": "reasoning", "id": conversationCopilotItemID("rs"), "encrypted_content": encrypted, "summary": []any{}},
		map[string]any{"type": "web_search_call", "id": conversationCopilotItemID("ws"), "status": "completed", "action": map[string]any{"type": "search", "query": "vekil docs"}},
		map[string]any{"type": "message", "id": conversationCopilotItemID("msg"), "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Found the docs."}}},
	}
}

func conversationAzureOutput(region string) []any {
	return []any{
		map[string]any{"type": "reasoning", "id": "rs_" + region + "1", "encrypted_content": "private-" + region + "-1", "summary": []any{}},
		map[string]any{"type": "message", "id": "msg_" + region + "1", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": region + " answer."}}},
	}
}

// conversationClientReplay returns output items as a client resends them.
// Codex keeps prefixed IDs and drops the rest; other clients keep every ID.
func conversationClientReplay(items []any, codex bool) []any {
	replayed := make([]any, 0, len(items))
	for _, item := range items {
		copied := make(map[string]any)
		for key, value := range item.(map[string]any) {
			copied[key] = value
		}
		if id, _ := copied["id"].(string); codex && !strings.Contains(id, "_") {
			delete(copied, "id")
		}
		replayed = append(replayed, copied)
	}
	return replayed
}

func conversationUserText(text string) map[string]any {
	return map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": text}}}
}

func conversationSwitchConfig(t *testing.T) *ProvidersConfig {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	return &ProvidersConfig{
		SchemaVersion:         2,
		StateBindings:         &StateBindingsConfig{Mode: "durable", File: filepath.Join(dir, "bindings.db")},
		ConversationMigration: &ConversationMigrationConfig{Routes: []string{"azure"}},
		Providers: []ProviderConfig{
			{ID: "east", Type: "azure-openai", BaseURL: "https://east.openai.azure.com/openai/v1", APIKey: "east-test-key", HostedTools: []string{"web_search"}},
			{ID: "west", Type: "azure-openai", BaseURL: "https://west.openai.azure.com/openai/v1", APIKey: "west-test-key", HostedTools: []string{"web_search"}},
		},
		ModelRoutes: []ModelRouteConfig{{
			ID: "azure", PublicID: "coding", Endpoints: []string{providerEndpointResponses},
			Targets: []ModelRouteTargetConfig{{ID: "east", Provider: "east", UpstreamModel: "deployment-east"}, {ID: "west", Provider: "west", UpstreamModel: "deployment-west"}},
			Routing: ModelRouteRoutingConfig{Mode: "priority_failover", MaxTargetAttempts: 2, MaxUpstreamSends: 3},
		}},
	}
}

// conversationReencryptedStream streams output the way Copilot and Azure do:
// the encrypted reasoning in each output_item.done event, which clients keep,
// differs from its copy in response.completed.
func conversationReencryptedStream(t *testing.T, req *http.Request, id string, output []any) *http.Response {
	t.Helper()
	_, _ = io.ReadAll(req.Body)
	var stream strings.Builder
	completed := make([]any, 0, len(output))
	for index, item := range output {
		fmt.Fprintf(&stream, "event: response.output_item.done\ndata: %s\n\n", mustJSON(t, map[string]any{"type": "response.output_item.done", "output_index": index, "item": item}))
		copied := make(map[string]any)
		for key, value := range item.(map[string]any) {
			copied[key] = value
		}
		if copied["type"] == "reasoning" {
			copied["encrypted_content"] = fmt.Sprint(copied["encrypted_content"], "-completed")
		}
		completed = append(completed, copied)
	}
	response := map[string]any{"id": id, "model": "physical-model", "status": "completed", "output": completed}
	fmt.Fprintf(&stream, "event: response.completed\ndata: %s\n\ndata: [DONE]\n\n", mustJSON(t, map[string]any{"type": "response.completed", "response": response}))
	return routeExecutorTestResponse(req, http.StatusOK, http.Header{"Content-Type": {"text/event-stream"}}, stream.String())
}

// conversationSwitchTransport serves east until eastDown is set, then fails
// east before delivery. West answers its nth request with west.
func conversationSwitchTransport(t *testing.T, eastDown *atomic.Bool, eastOutput []any, west func(*http.Request, int) *http.Response, westBodies *[]string) http.RoundTripper {
	return routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasSuffix(req.URL.Path, "/models") {
			return routeExecutorTestResponse(req, 200, nil, `{"data":[]}`), nil
		}
		if strings.HasPrefix(req.URL.Host, "east.") {
			if eastDown.Load() {
				return nil, errors.New("dial failed before request write")
			}
			return conversationResponse(t, req, "east-1", eastOutput...), nil
		}
		body, _ := io.ReadAll(req.Body)
		req.Body = io.NopCloser(bytes.NewReader(body))
		*westBodies = append(*westBodies, string(body))
		return west(req, len(*westBodies)), nil
	})
}

// conversationWestOutputs answers west's first requests with outputs in order,
// then with text.
func conversationWestOutputs(t *testing.T, outputs ...[]any) func(*http.Request, int) *http.Response {
	return func(req *http.Request, n int) *http.Response {
		if n <= len(outputs) {
			return conversationResponse(t, req, fmt.Sprintf("west-%d", n), outputs[n-1]...)
		}
		return conversationResponse(t, req, fmt.Sprintf("west-%d", n), conversationText("Done."))
	}
}

const conversationIDLessSearch = `{"action":{"query":"vekil docs","type":"search"},"status":"completed","type":"web_search_call"}`

func TestConversationMigrationContinuesAfterCopilotWebSearchOnNewOwner(t *testing.T) {
	for _, tc := range []struct {
		name  string
		codex bool
	}{{"codex replay", true}, {"client keeps ids", false}} {
		t.Run(tc.name, func(t *testing.T) {
			var eastDown atomic.Bool
			var westBodies []string
			eastOutput := conversationAzureOutput("east")
			westOutput := conversationCopilotWebSearchOutput("private-west-1")
			h, _ := newConversationAPIHandler(t, conversationSwitchTransport(t, &eastDown, eastOutput, conversationWestOutputs(t, westOutput), &westBodies), conversationSwitchConfig(t))
			tools := []any{map[string]any{"type": "web_search"}}
			history := []any{conversationUserText("Find the docs.")}
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": history, "tools": tools, "store": false}, nil), false)

			eastDown.Store(true)
			history = append(history, conversationClientReplay(eastOutput, tc.codex)...)
			history = append(history, conversationUserText("Search again."))
			switched := conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": history, "tools": tools, "store": false}, nil), false)
			if rawJSONString(switched["id"]) != "west-1" || !bytes.Contains(switched["vekil"], []byte(`"history":"saved"`)) {
				t.Fatalf("switch turn with a Copilot web search was not saved: id=%s vekil=%s", switched["id"], switched["vekil"])
			}

			// The replay carries east's older reasoning next to west's search.
			history = append(history, conversationClientReplay(westOutput, tc.codex)...)
			history = append(history, conversationUserText("Summarize."))
			next := conversationPOST(t, h, map[string]any{"input": history, "tools": tools, "store": false}, nil)
			response := conversationCompleted(t, next, false)
			if rawJSONString(response["id"]) != "west-2" || !bytes.Contains(response["vekil"], []byte(`"history":"saved"`)) {
				t.Fatalf("continuation did not stay on west with saved history: %s", next.Body.String())
			}
			body := westBodies[1]
			if !strings.Contains(body, conversationIDLessSearch) || strings.Contains(body, conversationCopilotItemID("ws")) {
				t.Errorf("west continuation does not replay an ID-less search call: %s", body)
			}
			if strings.Contains(body, "private-east-1") || !strings.Contains(body, "private-west-1") {
				t.Errorf("west continuation carries the wrong reasoning: %s", body)
			}
		})
	}
}

func TestConversationMigrationReplaysCopilotWebSearchToAnotherTarget(t *testing.T) {
	for _, tc := range []struct {
		name  string
		codex bool
	}{{"codex replay", true}, {"client keeps ids", false}} {
		t.Run(tc.name, func(t *testing.T) {
			var eastDown atomic.Bool
			var westBodies []string
			eastOutput := conversationCopilotWebSearchOutput("private-east-1")
			h, _ := newConversationAPIHandler(t, conversationSwitchTransport(t, &eastDown, eastOutput, conversationWestOutputs(t), &westBodies), conversationSwitchConfig(t))
			tools := []any{map[string]any{"type": "web_search"}}
			history := []any{conversationUserText("Find the docs.")}
			first := conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": history, "tools": tools, "store": false}, nil), false)
			if !bytes.Contains(first["vekil"], []byte(`"history":"saved"`)) {
				t.Fatalf("turn with a Copilot web search was not saved: %s", first["vekil"])
			}

			eastDown.Store(true)
			history = append(history, conversationClientReplay(eastOutput, tc.codex)...)
			history = append(history, conversationUserText("Summarize."))
			switched := conversationPOST(t, h, map[string]any{"input": history, "tools": tools, "store": false}, nil)
			response := conversationCompleted(t, switched, false)
			if rawJSONString(response["id"]) != "west-1" || !bytes.Contains(response["vekil"], []byte(`"migration":"completed"`)) {
				t.Fatalf("web search conversation did not migrate: %s", switched.Body.String())
			}
			// Azure rejects a foreign search ID longer than 64 characters.
			if body := westBodies[0]; !strings.Contains(body, conversationIDLessSearch) ||
				strings.Contains(body, conversationCopilotItemID("ws")) || strings.Contains(body, "private-east-1") {
				t.Errorf("migrated request does not carry an ID-less search call: %s", body)
			}
		})
	}
}

func TestConversationMigrationForwardsUnsupportedStateOnMigratedOwner(t *testing.T) {
	for _, tc := range []struct {
		name    string
		copilot bool
	}{{"azure owner", false}, {"copilot owner", true}} {
		t.Run(tc.name, func(t *testing.T) {
			var eastDown atomic.Bool
			var westBodies []string
			eastOutput, westOutput := conversationAzureOutput("east"), conversationAzureOutput("west")
			west := conversationWestOutputs(t, westOutput)
			if tc.copilot {
				// Codex drops Copilot's opaque IDs and keeps the streamed reasoning,
				// so only the thread's visible prefix finds the saved owner.
				westOutput = []any{
					map[string]any{"type": "reasoning", "id": conversationCopilotItemID("rs"), "encrypted_content": "private-west-1", "summary": []any{}},
					map[string]any{"type": "message", "id": conversationCopilotItemID("msg"), "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "West answer."}}},
				}
				west = func(req *http.Request, n int) *http.Response {
					if n == 1 {
						return conversationReencryptedStream(t, req, "west-1", westOutput)
					}
					return conversationResponse(t, req, fmt.Sprintf("west-%d", n), conversationText("Done."))
				}
			}
			h, _ := newConversationAPIHandler(t, conversationSwitchTransport(t, &eastDown, eastOutput, west, &westBodies), conversationSwitchConfig(t))
			thread := http.Header{"Session-Id": {"thread-1"}}
			history := []any{conversationUserText("Start.")}
			conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": history, "store": false}, thread), false)
			eastDown.Store(true)
			history = append(history, conversationClientReplay(eastOutput, true)...)
			history = append(history, conversationUserText("Continue."))
			switched := conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": history, "store": false, "stream": tc.copilot}, thread), tc.copilot)
			if rawJSONString(switched["id"]) != "west-1" || !bytes.Contains(switched["vekil"], []byte(`"history":"saved"`)) {
				t.Fatalf("second turn did not migrate with saved history: id=%s vekil=%s", switched["id"], switched["vekil"])
			}

			// An image cannot be saved, so this turn runs unprotected. The client still
			// replays east's older reasoning next to west's.
			history = append(history, conversationClientReplay(westOutput, true)...)
			history = append(history, conversationImageMessage())
			result := conversationPOST(t, h, map[string]any{"input": history, "store": false}, thread)
			body := result.Body.String()
			if result.Code != http.StatusOK || strings.Contains(body, `"history":"saved"`) || len(westBodies) != 2 {
				t.Fatalf("unsupported turn on the migrated owner was not forwarded unprotected: %d %s", result.Code, body)
			}
			if forwarded := westBodies[1]; strings.Contains(forwarded, "private-east-1") || !strings.Contains(forwarded, `"private-west-1"`) || !strings.Contains(forwarded, "input_image") {
				t.Errorf("unprotected west request carries the wrong history: %s", forwarded)
			}
		})
	}
}

// conversationEastCounter counts requests that reach east, including failed
// dials, so a test can prove which target served a turn.
func conversationEastCounter(inner http.RoundTripper, sends *atomic.Int32) http.RoundTripper {
	return routeExecutorRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.HasPrefix(req.URL.Host, "east.") && !strings.HasSuffix(req.URL.Path, "/models") {
			sends.Add(1)
		}
		return inner.RoundTrip(req)
	})
}

func conversationImageMessage() map[string]any {
	return map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aGVsbG8="}}}
}

func TestConversationMigrationPinsMixedUnprotectedTurnToSavedOwner(t *testing.T) {
	var eastDown atomic.Bool
	var eastSends atomic.Int32
	var westBodies, westTurnStates []string
	eastOutput := conversationAzureOutput("east")
	westOutput := []any{map[string]any{"type": "message", "id": "msg_west1", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "West answer."}}}}
	westReplies := conversationWestOutputs(t, westOutput)
	west := func(req *http.Request, n int) *http.Response {
		westTurnStates = append(westTurnStates, req.Header.Get("X-Codex-Turn-State"))
		return westReplies(req, n)
	}
	transport := conversationEastCounter(conversationSwitchTransport(t, &eastDown, eastOutput, west, &westBodies), &eastSends)
	h, _ := newConversationAPIHandler(t, transport, conversationSwitchConfig(t))
	history := []any{conversationUserText("Start.")}
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": history, "store": false}, nil), false)
	eastDown.Store(true)
	history = append(history, conversationClientReplay(eastOutput, true)...)
	history = append(history, conversationUserText("Continue."))
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": history, "store": false}, nil), false)

	// East is available again. West's turn-state header conflicts with east's
	// reasoning, and west emitted no reasoning, so only the pin keeps the
	// conversation on west once east's reasoning is dropped.
	eastDown.Store(false)
	before := eastSends.Load()
	history = append(history, conversationClientReplay(westOutput, true)...)
	history = append(history, conversationImageMessage())
	result := conversationPOST(t, h, map[string]any{"input": history, "store": false}, http.Header{"X-Codex-Turn-State": {"turn-west-1"}})
	if result.Code != http.StatusOK || eastSends.Load() != before || len(westBodies) != 2 {
		t.Fatalf("mixed unprotected turn did not stay on west: %d %s east sends=%d west sends=%d", result.Code, result.Body.String(), eastSends.Load()-before, len(westBodies))
	}
	if strings.Contains(westBodies[1], "private-east-1") || westTurnStates[1] != "" {
		t.Errorf("west received east's reasoning or a turn-state header: turn state=%q body=%s", westTurnStates[1], westBodies[1])
	}
}

func TestConversationMigrationKeepsConflictFreeUnprotectedTurnsOnNormalRoute(t *testing.T) {
	var eastDown atomic.Bool
	var westBodies []string
	eastOutput := conversationAzureOutput("east")
	h, _ := newConversationAPIHandler(t, conversationSwitchTransport(t, &eastDown, eastOutput, conversationWestOutputs(t, []any{conversationText("West answer.")}), &westBodies), conversationSwitchConfig(t))
	thread := http.Header{"Session-Id": {"thread-1"}}
	history := []any{conversationUserText("Start.")}
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": history, "store": false}, thread), false)
	eastDown.Store(true)
	history = append(history, eastOutput...)
	history = append(history, conversationUserText("Continue."))
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": history, "store": false}, thread), false)

	// West emitted no reasoning, so all replayed state belongs to east. Nothing
	// conflicts, and the unprotected turn keeps ordinary ownership routing to
	// the recovered east.
	eastDown.Store(false)
	history = append(history, conversationText("West answer."), conversationImageMessage())
	result := conversationPOST(t, h, map[string]any{"input": history, "store": false}, thread)
	if result.Code != http.StatusOK || !strings.Contains(result.Body.String(), `"id":"east-1"`) || len(westBodies) != 1 {
		t.Fatalf("conflict-free unprotected turn was pinned: %d %s west sends=%d", result.Code, result.Body.String(), len(westBodies))
	}
}

func conversationCompactPOST(t *testing.T, h *ProxyHandler, input []any, headers http.Header) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/responses/compact", bytes.NewReader(mustJSON(t, map[string]any{"model": "coding", "input": input})))
	req.Header = headers.Clone()
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	h.HandleCompact(recorder, req)
	return recorder
}

// Codex compacts a long thread through /responses/compact with its full
// history, which after a switch still carries the earlier owner's reasoning.
func TestConversationMigrationCompactsMigratedConversationOnSavedOwner(t *testing.T) {
	var eastDown atomic.Bool
	var eastSends atomic.Int32
	var westBodies []string
	eastOutput, westOutput := conversationAzureOutput("east"), conversationAzureOutput("west")
	transport := conversationEastCounter(conversationSwitchTransport(t, &eastDown, eastOutput, conversationWestOutputs(t, westOutput), &westBodies), &eastSends)
	h, _ := newConversationAPIHandler(t, transport, conversationSwitchConfig(t))
	thread := http.Header{"Session-Id": {"thread-1"}}
	history := []any{conversationUserText("Start.")}
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": history, "store": false}, thread), false)
	eastDown.Store(true)
	history = append(history, conversationClientReplay(eastOutput, true)...)
	history = append(history, conversationUserText("Continue."))
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": history, "store": false}, thread), false)

	eastDown.Store(false)
	before := eastSends.Load()
	history = append(history, conversationClientReplay(westOutput, true)...)
	result := conversationCompactPOST(t, h, history, thread)
	if result.Code != http.StatusOK || eastSends.Load() != before || len(westBodies) != 2 {
		t.Fatalf("compaction did not run on west: %d %s east sends=%d west sends=%d", result.Code, result.Body.String(), eastSends.Load()-before, len(westBodies))
	}
	if strings.Contains(westBodies[1], "private-east-1") || !strings.Contains(westBodies[1], "private-west-1") {
		t.Errorf("west compaction carries the wrong reasoning: %s", westBodies[1])
	}

	// Codex replaces its history with the compacted output and continues.
	var compacted struct {
		Output []any `json:"output"`
	}
	if err := json.Unmarshal(result.Body.Bytes(), &compacted); err != nil || len(compacted.Output) == 0 {
		t.Fatalf("compact response has no output: %v %s", err, result.Body.String())
	}
	next := conversationPOST(t, h, map[string]any{"input": append(compacted.Output, conversationUserText("Next.")), "store": false}, thread)
	if next.Code != http.StatusOK {
		t.Fatalf("turn after compaction failed: %d %s", next.Code, next.Body.String())
	}
}

// Azure rejects Copilot's 428-character IDs, so an unprotected turn on an Azure
// owner must not forward the IDs a client kept from earlier Copilot output.
func TestConversationMigrationUnprotectedTurnDropsEarlierOwnerItemIDs(t *testing.T) {
	var eastDown atomic.Bool
	var westBodies []string
	eastOutput, westOutput := conversationCopilotWebSearchOutput("private-east-1"), conversationAzureOutput("west")
	h, _ := newConversationAPIHandler(t, conversationSwitchTransport(t, &eastDown, eastOutput, conversationWestOutputs(t, westOutput), &westBodies), conversationSwitchConfig(t))
	tools := []any{map[string]any{"type": "web_search"}}
	history := []any{conversationUserText("Find the docs.")}
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": history, "tools": tools, "store": false}, nil), false)
	eastDown.Store(true)
	history = append(history, eastOutput...)
	history = append(history, conversationUserText("Continue."))
	conversationCompleted(t, conversationPOST(t, h, map[string]any{"input": history, "tools": tools, "store": false}, nil), false)

	history = append(history, westOutput...)
	history = append(history, conversationImageMessage())
	result := conversationPOST(t, h, map[string]any{"input": history, "tools": tools, "store": false}, nil)
	if result.Code != http.StatusOK || len(westBodies) != 2 {
		t.Fatalf("unsupported turn on the migrated owner was not forwarded: %d %s", result.Code, result.Body.String())
	}
	forwarded := westBodies[1]
	for _, kind := range []string{"rs", "ws", "msg"} {
		if strings.Contains(forwarded, conversationCopilotItemID(kind)) {
			t.Errorf("west request carries east's %s item ID: %s", kind, forwarded)
		}
	}
	if !strings.Contains(forwarded, conversationIDLessSearch) || !strings.Contains(forwarded, "input_image") || strings.Contains(forwarded, "private-east-1") {
		t.Errorf("west request carries the wrong history: %s", forwarded)
	}
}

func TestReadableConversationHistoryHonorsItemLimit(t *testing.T) {
	items := make([]any, maxConversationHistoryItems+1)
	for i := range items {
		items[i] = conversationUserText("x")
	}
	if readable := readableConversationHistory(mustJSON(t, items)); len(readable.items) != 0 || len(readable.anchors) != 0 {
		t.Fatalf("readable history over the item limit = %d items, %d anchors", len(readable.items), len(readable.anchors))
	}
}

func TestReadableConversationHistoryStopsAtUnsupportedItems(t *testing.T) {
	items := []any{conversationUserText("Start.")}
	items = append(items, conversationAzureOutput("east")...)
	items = append(items,
		map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,aGVsbG8="}}},
		map[string]any{"type": "message", "id": "msg_after", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "Red."}}},
	)
	readable := readableConversationHistory(mustJSON(t, items))
	prefix, err := canonicalConversationInput(mustJSON(t, items[:3]), false)
	if err != nil {
		t.Fatal(err)
	}
	if len(readable.items) != len(prefix.items) || !conversationHasPrefix(readable.items, prefix.items) {
		t.Fatalf("readable items = %s, want the canonical prefix %s", readable.items, prefix.items)
	}
	// Anchors after the unsupported item still identify saved output.
	want := append(prefix.anchors, conversationAnchor{"item", "msg_after"})
	if fmt.Sprint(readable.anchors) != fmt.Sprint(want) {
		t.Fatalf("readable anchors = %v, want %v", readable.anchors, want)
	}
}
