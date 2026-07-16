package proxy

import (
	"errors"
	"testing"
)

func TestTranslateResponsesJSONToChatAttachesUsageToPostDecodeErrors(t *testing.T) {
	tests := []struct {
		name string
		body string
		code string
	}{
		{
			name: "unexpected success error",
			body: `{"id":"resp","status":"completed","error":{"code":"invalid_prompt","message":"unexpected"},"output":[],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`,
			code: "invalid_responses_body",
		},
		{
			name: "unsupported output item",
			body: `{"id":"resp","status":"completed","output":[{"type":"computer_call"}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`,
			code: "unsupported_responses_output",
		},
		{
			name: "unsupported terminal status",
			body: `{"id":"resp","status":"queued","output":[],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`,
			code: "unsupported_response_status",
		},
		{
			name: "replay unavailable",
			body: `{"id":"resp","status":"completed","output":[{"type":"function_call","call_id":"call-1","name":"lookup","arguments":"{}","status":"completed"}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}`,
			code: "responses_replay_unavailable",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := translateResponsesJSONToChat([]byte(tt.body), responsesChatResponseOptions{})
			var executionErr *chatExecutionError
			if !errors.As(err, &executionErr) {
				t.Fatalf("error = %#v, want chatExecutionError", err)
			}
			if executionErr.Code != tt.code {
				t.Fatalf("code = %q, want %q", executionErr.Code, tt.code)
			}
			if executionErr.Usage == nil || executionErr.Usage.PromptTokens != 7 || executionErr.Usage.CompletionTokens != 3 || executionErr.Usage.TotalTokens != 10 {
				t.Fatalf("usage = %#v", executionErr.Usage)
			}
		})
	}
}
