package proxy

import (
	"encoding/json"
	"errors"
	"testing"
)

func TestTranslateResponsesJSONToChatValidatesMessageStatusAgainstEnvelope(t *testing.T) {
	tests := []struct {
		name           string
		envelopeStatus string
		messageStatus  string
		wantFinish     string
		wantError      bool
	}{
		{name: "completed message in completed response", envelopeStatus: "completed", messageStatus: "completed", wantFinish: "stop"},
		{name: "missing message status in completed response", envelopeStatus: "completed", messageStatus: "", wantError: true},
		{name: "incomplete message in incomplete response", envelopeStatus: "incomplete", messageStatus: "incomplete", wantFinish: "length"},
		{name: "missing message status in incomplete response", envelopeStatus: "incomplete", messageStatus: "", wantError: true},
		{name: "in progress message in completed response", envelopeStatus: "completed", messageStatus: "in_progress", wantError: true},
		{name: "incomplete message in completed response", envelopeStatus: "completed", messageStatus: "incomplete", wantError: true},
		{name: "completed message in incomplete response", envelopeStatus: "incomplete", messageStatus: "completed", wantError: true},
		{name: "in progress message in incomplete response", envelopeStatus: "incomplete", messageStatus: "in_progress", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envelope := map[string]any{
				"id":         "resp-message-status",
				"created_at": int64(1_700_000_000),
				"status":     tt.envelopeStatus,
				"output": []any{map[string]any{
					"type":    "message",
					"id":      "msg-message-status",
					"status":  tt.messageStatus,
					"role":    "assistant",
					"content": []any{map[string]any{"type": "output_text", "text": "status fixture"}},
				}},
				"usage": map[string]any{"input_tokens": 7, "output_tokens": 3, "total_tokens": 10},
			}
			if tt.envelopeStatus == "incomplete" {
				envelope["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
			}
			body, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}

			result, err := translateResponsesJSONToChat(body, responsesChatResponseOptions{PublicModel: "gpt-public"})
			if tt.wantError {
				var executionErr *chatExecutionError
				if !errors.As(err, &executionErr) || executionErr.Code != "unsupported_responses_output" {
					t.Fatalf("error = %#v, want unsupported_responses_output", err)
				}
				if executionErr.Usage == nil || executionErr.Usage.PromptTokens != 7 || executionErr.Usage.CompletionTokens != 3 || executionErr.Usage.TotalTokens != 10 {
					t.Fatalf("error usage = %#v", executionErr.Usage)
				}
				return
			}
			if err != nil {
				t.Fatalf("translateResponsesJSONToChat() error = %v", err)
			}
			if result.Response == nil || len(result.Response.Choices) != 1 {
				t.Fatalf("response = %#v", result.Response)
			}
			choice := result.Response.Choices[0]
			if choice.FinishReason == nil || *choice.FinishReason != tt.wantFinish {
				t.Fatalf("finish reason = %#v, want %q", choice.FinishReason, tt.wantFinish)
			}
			if got := string(choice.Message.Content); got != `"status fixture"` {
				t.Fatalf("content = %s", got)
			}
		})
	}
}

func TestTranslateResponsesJSONToChatValidatesReasoningStatusAgainstEnvelope(t *testing.T) {
	tests := []struct {
		name            string
		envelopeStatus  string
		reasoningStatus string
		wantFinish      string
		wantError       bool
	}{
		{name: "completed reasoning in completed response", envelopeStatus: "completed", reasoningStatus: "completed", wantFinish: "stop"},
		{name: "omitted reasoning status in completed response", envelopeStatus: "completed", reasoningStatus: "", wantFinish: "stop"},
		{name: "in progress reasoning in completed response", envelopeStatus: "completed", reasoningStatus: "in_progress", wantError: true},
		{name: "incomplete reasoning in completed response", envelopeStatus: "completed", reasoningStatus: "incomplete", wantError: true},
		{name: "incomplete reasoning in incomplete response", envelopeStatus: "incomplete", reasoningStatus: "incomplete", wantFinish: "length"},
		{name: "omitted reasoning status in incomplete response", envelopeStatus: "incomplete", reasoningStatus: "", wantFinish: "length"},
		{name: "completed reasoning in incomplete response", envelopeStatus: "incomplete", reasoningStatus: "completed", wantFinish: "length"},
		{name: "in progress reasoning in incomplete response", envelopeStatus: "incomplete", reasoningStatus: "in_progress", wantError: true},
		{name: "unknown reasoning status is rejected", envelopeStatus: "completed", reasoningStatus: "mystery", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			envelope := map[string]any{
				"id":         "resp-reasoning-status",
				"created_at": int64(1_700_000_000),
				"status":     tt.envelopeStatus,
				"output": []any{map[string]any{
					"type":              "reasoning",
					"id":                "rs-reasoning-status",
					"status":            tt.reasoningStatus,
					"encrypted_content": "synthetic-encrypted-content",
					"summary":           []any{},
				}},
				"usage": map[string]any{"input_tokens": 7, "output_tokens": 3, "total_tokens": 10},
			}
			if tt.envelopeStatus == "incomplete" {
				envelope["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
			}
			body, err := json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}

			result, err := translateResponsesJSONToChat(body, responsesChatResponseOptions{PublicModel: "gpt-public"})
			if tt.wantError {
				var executionErr *chatExecutionError
				if !errors.As(err, &executionErr) || executionErr.Code != "unsupported_responses_output" {
					t.Fatalf("error = %#v, want unsupported_responses_output", err)
				}
				if executionErr.Usage == nil || executionErr.Usage.TotalTokens != 10 {
					t.Fatalf("error usage = %#v", executionErr.Usage)
				}
				return
			}
			if err != nil {
				t.Fatalf("translateResponsesJSONToChat() error = %v", err)
			}
			finish := result.Response.Choices[0].FinishReason
			if finish == nil || *finish != tt.wantFinish {
				t.Fatalf("finish reason = %#v, want %q", finish, tt.wantFinish)
			}
		})
	}
}

func TestTranslateResponsesJSONToChatInvalidReasoningStatusDoesNotPublishReplay(t *testing.T) {
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	envelope := map[string]any{
		"id":         "resp-invalid-reasoning-replay",
		"created_at": int64(1_700_000_000),
		"status":     "completed",
		"output": []any{
			map[string]any{
				"type":      "function_call",
				"id":        "fc-invalid-reasoning-replay",
				"call_id":   "call-invalid-reasoning-replay",
				"name":      "lookup",
				"arguments": `{}`,
				"status":    "completed",
			},
			map[string]any{
				"type":              "reasoning",
				"id":                "rs-invalid-reasoning-replay",
				"status":            "in_progress",
				"encrypted_content": "synthetic-encrypted-content",
				"summary":           []any{},
			},
		},
	}
	body, err := json.Marshal(envelope)
	if err != nil {
		t.Fatal(err)
	}

	_, err = translateResponsesJSONToChat(body, responsesChatResponseOptions{
		PublicModel: "gpt-public",
		ReplayStore: store,
		ReplayRoute: responsesChatReplayRoute{ProviderID: "provider", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"},
	})
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.Code != "unsupported_responses_output" {
		t.Fatalf("error = %#v, want unsupported_responses_output", err)
	}
	if stats := store.Stats(); stats.Groups != 0 || stats.Calls != 0 || stats.TotalBytes != 0 {
		t.Fatalf("replay state was published before reasoning validation: %#v", stats)
	}
}
