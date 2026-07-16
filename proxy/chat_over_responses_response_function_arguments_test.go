package proxy

import (
	"errors"
	"testing"
)

func TestTranslateResponsesJSONToChatRequiresCompletedFunctionArgumentsString(t *testing.T) {
	tests := []struct {
		name           string
		argumentsField string
	}{
		{name: "missing", argumentsField: ""},
		{name: "null", argumentsField: `,"arguments":null`},
		{name: "number", argumentsField: `,"arguments":42`},
		{name: "object", argumentsField: `,"arguments":{"query":"value"}`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"id":"resp-invalid-arguments","status":"completed","output":[{"type":"function_call","call_id":"upstream-call","name":"lookup","status":"completed"` + tt.argumentsField + `}]}`)
			store := newResponsesChatReplayStore()
			_, err := translateResponsesJSONToChat(body, responsesChatResponseOptions{
				PublicModel: "gpt",
				ReplayStore: store,
				ReplayRoute: responsesChatReplayRoute{ProviderID: "provider", PublicModel: "gpt", UpstreamModel: "gpt"},
			})

			var executionErr *chatExecutionError
			if !errors.As(err, &executionErr) || executionErr.Code != "unsupported_responses_output" {
				t.Errorf("translateResponsesJSONToChat() error = %#v, want unsupported_responses_output", err)
			}
			if stats := store.Stats(); stats.Groups != 0 || stats.Calls != 0 || stats.TotalBytes != 0 {
				t.Errorf("replay state published for invalid arguments: %#v", stats)
			}
		})
	}
}

func TestTranslateResponsesJSONToChatPreservesPresentFunctionArgumentsStrings(t *testing.T) {
	tests := []struct {
		name          string
		argumentsJSON string
		want          string
	}{
		{name: "empty string", argumentsJSON: `""`, want: ""},
		{name: "opaque invalid JSON", argumentsJSON: `"{not-json"`, want: "{not-json"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := []byte(`{"id":"resp-valid-arguments","status":"completed","output":[{"type":"function_call","call_id":"upstream-call","name":"lookup","arguments":` + tt.argumentsJSON + `,"status":"completed"}]}`)
			store := newResponsesChatReplayStore()
			route := responsesChatReplayRoute{ProviderID: "provider", PublicModel: "gpt", UpstreamModel: "gpt"}
			result, err := translateResponsesJSONToChat(body, responsesChatResponseOptions{
				PublicModel: "gpt",
				ReplayStore: store,
				ReplayRoute: route,
			})
			if err != nil {
				t.Fatalf("translateResponsesJSONToChat() error = %v", err)
			}

			choice := result.Response.Choices[0]
			if len(choice.Message.ToolCalls) != 1 {
				t.Fatalf("tool calls = %#v", choice.Message.ToolCalls)
			}
			call := choice.Message.ToolCalls[0]
			if call.Function.Arguments != tt.want {
				t.Fatalf("arguments = %q, want %q", call.Function.Arguments, tt.want)
			}
			if _, err := store.Resolve(route, responsesChatReplayAssistantProjection{
				Content: choice.Message.Content,
				Calls: []responsesChatReplayProjectedCall{{
					ID:        call.ID,
					Name:      call.Function.Name,
					Arguments: call.Function.Arguments,
				}},
			}); err != nil {
				t.Fatalf("resolve replay: %v", err)
			}
		})
	}
}
