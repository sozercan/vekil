package proxy

import (
	"encoding/json"
	"testing"
)

func TestTranslateChatRequestToResponsesNormalizesReplayAssistantContentParts(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer store.Close()

	route := responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
	published, err := store.Publish(responsesChatReplayPublishRequest{
		Route:            route,
		AssistantContent: json.RawMessage(`"checking status"`),
		OutputItems: []json.RawMessage{
			json.RawMessage(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":"checking "},{"type":"output_text","text":"status"}]}`),
			json.RawMessage(`{"type":"function_call","call_id":"upstream-call-1","name":"lookup","arguments":"{}","status":"completed"}`),
		},
		Calls: []responsesChatReplayPublishCall{{
			UpstreamCallID:   "upstream-call-1",
			Name:             "lookup",
			VisibleArguments: `{}`,
			OutputItemIndex:  1,
		}},
	})
	if err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	call := published.Projection.Calls[0]
	request := map[string]any{
		"model": "gpt-public",
		"messages": []any{
			map[string]any{
				"role": "assistant",
				"content": []any{
					map[string]any{"type": "text", "text": "checking "},
					map[string]any{"type": "text", "text": "status"},
				},
				"tool_calls": []any{map[string]any{
					"id":   call.ID,
					"type": "function",
					"function": map[string]any{
						"name":      call.Name,
						"arguments": call.Arguments,
					},
				}},
			},
			map[string]any{"role": "tool", "tool_call_id": call.ID, "content": "done"},
		},
	}
	body, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}

	plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
		UpstreamModel: "gpt-upstream",
		ReplayStore:   store,
		ReplayRoute:   route,
	})
	if err != nil {
		t.Fatalf("translateChatRequestToResponses() error = %v", err)
	}
	var upstream struct {
		Input []map[string]any `json:"input"`
	}
	if err := json.Unmarshal(plan.Body, &upstream); err != nil {
		t.Fatal(err)
	}
	if len(upstream.Input) != 3 {
		t.Fatalf("upstream input = %#v", upstream.Input)
	}
	if upstream.Input[0]["type"] != "message" || upstream.Input[1]["type"] != "function_call" || upstream.Input[2]["type"] != "function_call_output" {
		t.Fatalf("upstream input = %#v", upstream.Input)
	}
}

func TestTranslateChatRequestToResponsesPreservesReplayNullEmptyContentCompatibility(t *testing.T) {
	for _, publishedContent := range []json.RawMessage{json.RawMessage(`null`), json.RawMessage(`""`)} {
		publishedContent := publishedContent
		t.Run(string(publishedContent), func(t *testing.T) {
			store := newResponsesChatReplayStore()
			defer store.Close()
			route := responsesChatReplayRoute{ProviderID: "provider-a", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"}
			published, err := store.Publish(responsesChatReplayPublishRequest{
				Route:            route,
				AssistantContent: publishedContent,
				OutputItems: []json.RawMessage{
					json.RawMessage(`{"type":"function_call","call_id":"upstream-call-1","name":"lookup","arguments":"{}","status":"completed"}`),
				},
				Calls: []responsesChatReplayPublishCall{{UpstreamCallID: "upstream-call-1", Name: "lookup", VisibleArguments: `{}`, OutputItemIndex: 0}},
			})
			if err != nil {
				t.Fatal(err)
			}
			call := published.Projection.Calls[0]
			request := map[string]any{
				"model": "gpt-public",
				"messages": []any{
					map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}}}},
					map[string]any{"role": "tool", "tool_call_id": call.ID, "content": "done"},
				},
			}
			body, _ := json.Marshal(request)
			if _, err := translateChatRequestToResponses(body, responsesChatRequestOptions{UpstreamModel: "gpt-upstream", ReplayStore: store, ReplayRoute: route}); err != nil {
				t.Fatalf("translateChatRequestToResponses() error = %v", err)
			}
		})
	}
}
