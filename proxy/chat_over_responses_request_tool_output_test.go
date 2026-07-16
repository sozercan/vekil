package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func TestTranslateChatRequestToResponsesRejectsNullOrOmittedToolContent(t *testing.T) {
	tests := []struct {
		name         string
		contentField string
	}{
		{name: "null", contentField: `,"content":null`},
		{name: "omitted"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := chatToolOutputRequestBody(tt.contentField)

			_, err := translateChatRequestToResponses(body, responsesChatRequestOptions{})
			var executionErr *chatExecutionError
			if !errors.As(err, &executionErr) {
				t.Fatalf("translateChatRequestToResponses() error = %v, want chatExecutionError", err)
			}
			if executionErr.StatusCode != http.StatusBadRequest {
				t.Fatalf("translateChatRequestToResponses() status = %d, want %d", executionErr.StatusCode, http.StatusBadRequest)
			}
			if executionErr.Param != "messages[1].content" {
				t.Fatalf("translateChatRequestToResponses() param = %q, want messages[1].content", executionErr.Param)
			}
		})
	}
}

func TestTranslateChatRequestToResponsesPreservesValidToolContent(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{name: "empty string", content: `""`, want: ""},
		{name: "object", content: `{"ok":true}`, want: `{"ok":true}`},
		{name: "array", content: `["first",{"nested":true},2,false]`, want: `["first",{"nested":true},2,false]`},
		{name: "number", content: `42`, want: `42`},
		{name: "boolean", content: `true`, want: `true`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body := chatToolOutputRequestBody(fmt.Sprintf(`,"content":%s`, tt.content))

			plan, err := translateChatRequestToResponses(body, responsesChatRequestOptions{})
			if err != nil {
				t.Fatalf("translateChatRequestToResponses() error = %v", err)
			}

			var request struct {
				Input []struct {
					Type   string `json:"type"`
					Output string `json:"output"`
				} `json:"input"`
			}
			if err := json.Unmarshal(plan.Body, &request); err != nil {
				t.Fatalf("decode Responses request: %v", err)
			}
			if len(request.Input) != 2 {
				t.Fatalf("Responses input length = %d, want 2", len(request.Input))
			}
			if request.Input[1].Type != "function_call_output" {
				t.Fatalf("Responses input[1].type = %q, want function_call_output", request.Input[1].Type)
			}
			if request.Input[1].Output != tt.want {
				t.Fatalf("Responses input[1].output = %q, want %q", request.Input[1].Output, tt.want)
			}
		})
	}
}

func chatToolOutputRequestBody(contentField string) []byte {
	return []byte(fmt.Sprintf(`{
		"model":"gpt-public",
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"lookup","arguments":"{}"}}]},
			{"role":"tool","tool_call_id":"call_1"%s}
		]
	}`, contentField))
}
