package proxy

import (
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestResponsesChatStreamValidatesTerminalMessageStatus(t *testing.T) {
	tests := []struct {
		name                  string
		responseStatus        string
		outputItemDoneStatus  string
		terminalMessageStatus string
		wantFinish            string
		wantError             bool
	}{
		{
			name:                  "completed response with completed message",
			responseStatus:        "completed",
			outputItemDoneStatus:  "completed",
			terminalMessageStatus: "completed",
			wantFinish:            "stop",
		},
		{
			name:                  "incomplete response with incomplete message",
			responseStatus:        "incomplete",
			outputItemDoneStatus:  "incomplete",
			terminalMessageStatus: "incomplete",
			wantFinish:            "length",
		},
		{
			name:                  "completed response rejects missing message status",
			responseStatus:        "completed",
			outputItemDoneStatus:  "",
			terminalMessageStatus: "",
			wantError:             true,
		},
		{
			name:                  "completed response rejects in progress message",
			responseStatus:        "completed",
			outputItemDoneStatus:  "in_progress",
			terminalMessageStatus: "in_progress",
			wantError:             true,
		},
		{
			name:                  "completed response rejects incomplete message",
			responseStatus:        "completed",
			outputItemDoneStatus:  "incomplete",
			terminalMessageStatus: "incomplete",
			wantError:             true,
		},
		{
			name:                  "incomplete response rejects missing message status",
			responseStatus:        "incomplete",
			outputItemDoneStatus:  "",
			terminalMessageStatus: "",
			wantError:             true,
		},
		{
			name:                  "incomplete response rejects completed message",
			responseStatus:        "incomplete",
			outputItemDoneStatus:  "completed",
			terminalMessageStatus: "completed",
			wantError:             true,
		},
		{
			name:                  "incomplete response rejects in progress message",
			responseStatus:        "incomplete",
			outputItemDoneStatus:  "in_progress",
			terminalMessageStatus: "in_progress",
			wantError:             true,
		},
		{
			name:                  "terminal completed message must match output item done",
			responseStatus:        "completed",
			outputItemDoneStatus:  "in_progress",
			terminalMessageStatus: "completed",
			wantError:             true,
		},
		{
			name:                  "terminal incomplete message must match output item done",
			responseStatus:        "incomplete",
			outputItemDoneStatus:  "completed",
			terminalMessageStatus: "incomplete",
			wantError:             true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newResponsesChatStreamState(responsesChatStreamConfig{PublicModel: "gpt-public", Now: time.Now})
			mustHandleResponsesChatStatusTransition(t, state.handleCreated, map[string]any{
				"type": "response.created",
				"response": map[string]any{
					"id":         "resp-stream-message-status",
					"created_at": int64(1_700_000_000),
					"status":     "in_progress",
				},
			})
			mustHandleResponsesChatStatusTransition(t, state.handleOutputItemAdded, map[string]any{
				"type":         "response.output_item.added",
				"output_index": 0,
				"item": map[string]any{
					"type":    "message",
					"id":      "msg-stream-message-status-added",
					"status":  "in_progress",
					"role":    "assistant",
					"content": []any{},
				},
			})
			mustHandleResponsesChatStatusTransition(t, state.handleOutputItemDone, map[string]any{
				"type":         "response.output_item.done",
				"output_index": 0,
				"item": map[string]any{
					"type":    "message",
					"id":      "msg-stream-message-status-done",
					"status":  tt.outputItemDoneStatus,
					"role":    "assistant",
					"content": []any{},
				},
			})

			terminalType := "response." + tt.responseStatus
			terminal := map[string]any{
				"type": terminalType,
				"response": map[string]any{
					"id":     "resp-stream-message-status",
					"status": tt.responseStatus,
					"output": []any{map[string]any{
						"type":    "message",
						"id":      "msg-stream-message-status-terminal",
						"status":  tt.terminalMessageStatus,
						"role":    "assistant",
						"content": []any{},
					}},
					"usage": map[string]any{"input_tokens": 7, "output_tokens": 3, "total_tokens": 10},
				},
			}
			if tt.responseStatus == "incomplete" {
				terminal["response"].(map[string]any)["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
			}
			data, err := json.Marshal(terminal)
			if err != nil {
				t.Fatal(err)
			}

			var transition responsesChatStreamTransition
			if tt.responseStatus == "completed" {
				transition, err = state.handleCompleted(data)
			} else {
				transition, err = state.handleIncomplete(data)
			}
			if tt.wantError {
				var executionErr *chatExecutionError
				if !errors.As(err, &executionErr) || executionErr.Code != "unsupported_responses_output" {
					t.Fatalf("error = %#v, want unsupported_responses_output", err)
				}
				if executionErr.Usage == nil || executionErr.Usage.PromptTokens != 7 || executionErr.Usage.CompletionTokens != 3 || executionErr.Usage.TotalTokens != 10 {
					t.Fatalf("error usage = %#v", executionErr.Usage)
				}
				if len(transition.chunks) != 1 || transition.chunks[0].Usage == nil || transition.chunks[0].Usage.TotalTokens != 10 {
					t.Fatalf("transition = %#v", transition)
				}
				return
			}
			if err != nil {
				t.Fatalf("terminal transition error = %v", err)
			}
			if !transition.terminal || len(transition.chunks) != 2 {
				t.Fatalf("transition = %#v", transition)
			}
			gotFinish := ""
			for _, chunk := range transition.chunks {
				for _, choice := range chunk.Choices {
					if choice.FinishReason != nil {
						gotFinish = *choice.FinishReason
					}
				}
			}
			if gotFinish != tt.wantFinish {
				t.Fatalf("finish reason = %q, want %q; transition = %#v", gotFinish, tt.wantFinish, transition)
			}
			if transition.chunks[1].Usage == nil || transition.chunks[1].Usage.TotalTokens != 10 {
				t.Fatalf("usage chunk = %#v", transition.chunks[1])
			}
		})
	}
}

func mustHandleResponsesChatStatusTransition(
	t *testing.T,
	handle func([]byte) (responsesChatStreamTransition, error),
	payload map[string]any,
) {
	t.Helper()
	data, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := handle(data); err != nil {
		t.Fatalf("stream transition error = %v", err)
	}
}

func TestResponsesChatStreamValidatesTerminalReasoningStatus(t *testing.T) {
	tests := []struct {
		name                    string
		responseStatus          string
		outputItemDoneStatus    string
		terminalReasoningStatus string
		wantFinish              string
		wantError               bool
	}{
		{name: "completed response with completed reasoning", responseStatus: "completed", outputItemDoneStatus: "completed", terminalReasoningStatus: "completed", wantFinish: "stop"},
		{name: "incomplete response with incomplete reasoning", responseStatus: "incomplete", outputItemDoneStatus: "incomplete", terminalReasoningStatus: "incomplete", wantFinish: "length"},
		{name: "completed response accepts omitted reasoning status", responseStatus: "completed", outputItemDoneStatus: "", terminalReasoningStatus: "", wantFinish: "stop"},
		{name: "incomplete response accepts omitted reasoning status", responseStatus: "incomplete", outputItemDoneStatus: "", terminalReasoningStatus: "", wantFinish: "length"},
		{name: "completed response rejects in progress reasoning", responseStatus: "completed", outputItemDoneStatus: "in_progress", terminalReasoningStatus: "in_progress", wantError: true},
		{name: "completed response rejects incomplete reasoning", responseStatus: "completed", outputItemDoneStatus: "incomplete", terminalReasoningStatus: "incomplete", wantError: true},
		{name: "incomplete response accepts completed reasoning", responseStatus: "incomplete", outputItemDoneStatus: "completed", terminalReasoningStatus: "completed", wantFinish: "length"},
		{name: "incomplete response rejects in progress reasoning", responseStatus: "incomplete", outputItemDoneStatus: "in_progress", terminalReasoningStatus: "in_progress", wantError: true},
		{name: "terminal reasoning may omit status when done status is completed", responseStatus: "completed", outputItemDoneStatus: "completed", terminalReasoningStatus: "", wantFinish: "stop"},
		{name: "done reasoning may omit status when terminal status is completed", responseStatus: "completed", outputItemDoneStatus: "", terminalReasoningStatus: "completed", wantFinish: "stop"},
		{name: "terminal reasoning must match output item done", responseStatus: "completed", outputItemDoneStatus: "in_progress", terminalReasoningStatus: "completed", wantError: true},
		{name: "unknown reasoning status is rejected", responseStatus: "completed", outputItemDoneStatus: "mystery", terminalReasoningStatus: "mystery", wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newResponsesChatStreamState(responsesChatStreamConfig{PublicModel: "gpt-public", Now: time.Now})
			mustHandleResponsesChatStatusTransition(t, state.handleCreated, map[string]any{
				"type": "response.created",
				"response": map[string]any{
					"id":         "resp-stream-reasoning-status",
					"created_at": int64(1_700_000_000),
					"status":     "in_progress",
				},
			})
			mustHandleResponsesChatStatusTransition(t, state.handleOutputItemAdded, map[string]any{
				"type":         "response.output_item.added",
				"output_index": 0,
				"item": map[string]any{
					"type":   "reasoning",
					"id":     "rs-stream-reasoning-status-added",
					"status": "in_progress",
				},
			})
			mustHandleResponsesChatStatusTransition(t, state.handleOutputItemDone, map[string]any{
				"type":         "response.output_item.done",
				"output_index": 0,
				"item": map[string]any{
					"type":              "reasoning",
					"id":                "rs-stream-reasoning-status-done",
					"status":            tt.outputItemDoneStatus,
					"encrypted_content": "synthetic-encrypted-content",
					"summary":           []any{},
				},
			})

			terminal := map[string]any{
				"type": "response." + tt.responseStatus,
				"response": map[string]any{
					"id":     "resp-stream-reasoning-status",
					"status": tt.responseStatus,
					"output": []any{map[string]any{
						"type":              "reasoning",
						"id":                "rs-stream-reasoning-status-terminal",
						"status":            tt.terminalReasoningStatus,
						"encrypted_content": "synthetic-encrypted-content",
						"summary":           []any{},
					}},
					"usage": map[string]any{"input_tokens": 7, "output_tokens": 3, "total_tokens": 10},
				},
			}
			if tt.responseStatus == "incomplete" {
				terminal["response"].(map[string]any)["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
			}
			data, err := json.Marshal(terminal)
			if err != nil {
				t.Fatal(err)
			}

			var transition responsesChatStreamTransition
			if tt.responseStatus == "completed" {
				transition, err = state.handleCompleted(data)
			} else {
				transition, err = state.handleIncomplete(data)
			}
			if tt.wantError {
				var executionErr *chatExecutionError
				if !errors.As(err, &executionErr) || executionErr.Code != "unsupported_responses_output" {
					t.Fatalf("error = %#v, want unsupported_responses_output", err)
				}
				if executionErr.Usage == nil || executionErr.Usage.TotalTokens != 10 {
					t.Fatalf("error usage = %#v", executionErr.Usage)
				}
				if len(transition.chunks) != 1 || transition.chunks[0].Usage == nil || transition.chunks[0].Usage.TotalTokens != 10 {
					t.Fatalf("transition = %#v", transition)
				}
				return
			}
			if err != nil {
				t.Fatalf("terminal transition error = %v", err)
			}
			if !transition.terminal || len(transition.chunks) != 2 {
				t.Fatalf("transition = %#v", transition)
			}
			gotFinish := ""
			for _, chunk := range transition.chunks {
				for _, choice := range chunk.Choices {
					if choice.FinishReason != nil {
						gotFinish = *choice.FinishReason
					}
				}
			}
			if gotFinish != tt.wantFinish {
				t.Fatalf("finish reason = %q, want %q; transition = %#v", gotFinish, tt.wantFinish, transition)
			}
		})
	}
}

func TestResponsesChatStreamInvalidReasoningStatusDoesNotPublishReplay(t *testing.T) {
	store := newResponsesChatReplayStore()
	t.Cleanup(func() { _ = store.Close() })
	state := newResponsesChatStreamState(responsesChatStreamConfig{
		PublicModel: "gpt-public",
		ReplayStore: store,
		ReplayRoute: responsesChatReplayRoute{ProviderID: "provider", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"},
		Now:         time.Now,
	})
	mustHandleResponsesChatStatusTransition(t, state.handleCreated, map[string]any{
		"type": "response.created",
		"response": map[string]any{
			"id":         "resp-stream-invalid-reasoning-replay",
			"created_at": int64(1_700_000_000),
			"status":     "in_progress",
		},
	})
	mustHandleResponsesChatStatusTransition(t, state.handleOutputItemAdded, map[string]any{
		"type":         "response.output_item.added",
		"output_index": 0,
		"item": map[string]any{
			"type":      "function_call",
			"id":        "fc-stream-invalid-reasoning-replay",
			"call_id":   "call-stream-invalid-reasoning-replay",
			"name":      "lookup",
			"arguments": "",
			"status":    "in_progress",
		},
	})
	mustHandleResponsesChatStatusTransition(t, state.handleFunctionArgumentsDone, map[string]any{
		"type":         "response.function_call_arguments.done",
		"item_id":      "fc-stream-invalid-reasoning-replay",
		"output_index": 0,
		"arguments":    `{}`,
	})
	mustHandleResponsesChatStatusTransition(t, state.handleOutputItemDone, map[string]any{
		"type":         "response.output_item.done",
		"output_index": 0,
		"item": map[string]any{
			"type":      "function_call",
			"id":        "fc-stream-invalid-reasoning-replay",
			"call_id":   "call-stream-invalid-reasoning-replay",
			"name":      "lookup",
			"arguments": `{}`,
			"status":    "completed",
		},
	})
	mustHandleResponsesChatStatusTransition(t, state.handleOutputItemAdded, map[string]any{
		"type":         "response.output_item.added",
		"output_index": 1,
		"item": map[string]any{
			"type":   "reasoning",
			"id":     "rs-stream-invalid-reasoning-replay",
			"status": "in_progress",
		},
	})
	mustHandleResponsesChatStatusTransition(t, state.handleOutputItemDone, map[string]any{
		"type":         "response.output_item.done",
		"output_index": 1,
		"item": map[string]any{
			"type":              "reasoning",
			"id":                "rs-stream-invalid-reasoning-replay",
			"status":            "in_progress",
			"encrypted_content": "synthetic-encrypted-content",
			"summary":           []any{},
		},
	})
	terminal, err := json.Marshal(map[string]any{
		"type": "response.completed",
		"response": map[string]any{
			"id":     "resp-stream-invalid-reasoning-replay",
			"status": "completed",
			"output": []any{
				map[string]any{
					"type":      "function_call",
					"id":        "fc-stream-invalid-reasoning-replay-terminal",
					"call_id":   "call-stream-invalid-reasoning-replay",
					"name":      "lookup",
					"arguments": `{}`,
					"status":    "completed",
				},
				map[string]any{
					"type":              "reasoning",
					"id":                "rs-stream-invalid-reasoning-replay-terminal",
					"status":            "in_progress",
					"encrypted_content": "synthetic-encrypted-content",
					"summary":           []any{},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, err = state.handleCompleted(terminal)
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr.Code != "unsupported_responses_output" {
		t.Fatalf("error = %#v, want unsupported_responses_output", err)
	}
	if stats := store.Stats(); stats.Groups != 0 || stats.Calls != 0 || stats.TotalBytes != 0 {
		t.Fatalf("replay state was published before reasoning validation: %#v", stats)
	}
}

func TestResponsesChatStreamValidatesTerminalFunctionCallStatus(t *testing.T) {
	status := func(value string) *string { return &value }
	tests := []struct {
		name           string
		responseStatus string
		doneStatus     *string
		terminalStatus *string
		wantDoneError  bool
		wantError      bool
		wantPublished  bool
		wantFinish     string
	}{
		{name: "completed response with completed call", responseStatus: "completed", doneStatus: status("completed"), terminalStatus: status("completed"), wantPublished: true, wantFinish: "tool_calls"},
		{name: "completed response with omitted call statuses", responseStatus: "completed", wantPublished: true, wantFinish: "tool_calls"},
		{name: "completed response with omitted terminal status", responseStatus: "completed", doneStatus: status("completed"), wantPublished: true, wantFinish: "tool_calls"},
		{name: "completed response rejects incomplete call", responseStatus: "completed", doneStatus: status("incomplete"), terminalStatus: status("incomplete"), wantError: true},
		{name: "output item done rejects in progress call", responseStatus: "completed", doneStatus: status("in_progress"), terminalStatus: status("in_progress"), wantDoneError: true},
		{name: "output item done rejects unknown call status", responseStatus: "completed", doneStatus: status("mystery"), terminalStatus: status("mystery"), wantDoneError: true},
		{name: "incomplete response with completed call", responseStatus: "incomplete", doneStatus: status("completed"), terminalStatus: status("completed"), wantPublished: true, wantFinish: "tool_calls"},
		{name: "incomplete response with incomplete call", responseStatus: "incomplete", doneStatus: status("incomplete"), terminalStatus: status("incomplete"), wantFinish: "length"},
		{name: "incomplete response with omitted call statuses", responseStatus: "incomplete", wantFinish: "length"},
		{name: "incomplete response with omitted done status and completed terminal", responseStatus: "incomplete", terminalStatus: status("completed"), wantPublished: true, wantFinish: "tool_calls"},
		{name: "terminal call status must match output item done", responseStatus: "incomplete", doneStatus: status("incomplete"), terminalStatus: status("completed"), wantError: true},
		{name: "terminal rejects unknown call status", responseStatus: "incomplete", terminalStatus: status("mystery"), wantError: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newResponsesChatReplayStore()
			t.Cleanup(func() { _ = store.Close() })
			state := newResponsesChatStreamState(responsesChatStreamConfig{
				PublicModel: "gpt-public",
				ReplayStore: store,
				ReplayRoute: responsesChatReplayRoute{ProviderID: "provider", PublicModel: "gpt-public", UpstreamModel: "gpt-upstream"},
				Now:         time.Now,
			})
			mustHandleResponsesChatStatusTransition(t, state.handleCreated, map[string]any{
				"type": "response.created",
				"response": map[string]any{
					"id":         "resp-stream-function-status",
					"created_at": int64(1_700_000_000),
					"status":     "in_progress",
				},
			})
			mustHandleResponsesChatStatusTransition(t, state.handleOutputItemAdded, map[string]any{
				"type":         "response.output_item.added",
				"output_index": 0,
				"item": map[string]any{
					"type":      "function_call",
					"id":        "fc-stream-function-status",
					"call_id":   "call-stream-function-status",
					"name":      "lookup",
					"arguments": "",
					"status":    "in_progress",
				},
			})
			mustHandleResponsesChatStatusTransition(t, state.handleFunctionArgumentsDone, map[string]any{
				"type":         "response.function_call_arguments.done",
				"item_id":      "fc-stream-function-status",
				"output_index": 0,
				"arguments":    `{}`,
			})
			doneItem := map[string]any{
				"type":      "function_call",
				"id":        "fc-stream-function-status-done",
				"call_id":   "call-stream-function-status",
				"name":      "lookup",
				"arguments": `{}`,
			}
			if tt.doneStatus != nil {
				doneItem["status"] = *tt.doneStatus
			}
			donePayload, err := json.Marshal(map[string]any{
				"type":         "response.output_item.done",
				"output_index": 0,
				"item":         doneItem,
			})
			if err != nil {
				t.Fatal(err)
			}
			_, doneErr := state.handleOutputItemDone(donePayload)
			if tt.wantDoneError {
				var executionErr *chatExecutionError
				if !errors.As(doneErr, &executionErr) || executionErr.Code != "unsupported_responses_output" {
					t.Fatalf("output item done error = %#v, want unsupported_responses_output", doneErr)
				}
				if stats := store.Stats(); stats.Groups != 0 || stats.Calls != 0 {
					t.Fatalf("replay stats = %#v", stats)
				}
				return
			}
			if doneErr != nil {
				t.Fatalf("output item done error = %v", doneErr)
			}

			terminalItem := map[string]any{
				"type":      "function_call",
				"id":        "fc-stream-function-status-terminal",
				"call_id":   "call-stream-function-status",
				"name":      "lookup",
				"arguments": `{}`,
			}
			if tt.terminalStatus != nil {
				terminalItem["status"] = *tt.terminalStatus
			}
			terminal := map[string]any{
				"type": "response." + tt.responseStatus,
				"response": map[string]any{
					"id":     "resp-stream-function-status",
					"status": tt.responseStatus,
					"output": []any{terminalItem},
				},
			}
			if tt.responseStatus == "incomplete" {
				terminal["response"].(map[string]any)["incomplete_details"] = map[string]any{"reason": "max_output_tokens"}
			}
			terminalPayload, err := json.Marshal(terminal)
			if err != nil {
				t.Fatal(err)
			}
			var transition responsesChatStreamTransition
			if tt.responseStatus == "completed" {
				transition, err = state.handleCompleted(terminalPayload)
			} else {
				transition, err = state.handleIncomplete(terminalPayload)
			}
			if tt.wantError {
				var executionErr *chatExecutionError
				if !errors.As(err, &executionErr) || executionErr.Code != "unsupported_responses_output" {
					t.Fatalf("terminal error = %#v, want unsupported_responses_output", err)
				}
				if stats := store.Stats(); stats.Groups != 0 || stats.Calls != 0 {
					t.Fatalf("replay stats = %#v", stats)
				}
				return
			}
			if err != nil {
				t.Fatalf("terminal error = %v", err)
			}
			gotFinish := ""
			for _, chunk := range transition.chunks {
				for _, choice := range chunk.Choices {
					if choice.FinishReason != nil {
						gotFinish = *choice.FinishReason
					}
				}
			}
			if gotFinish != tt.wantFinish {
				t.Fatalf("finish reason = %q, want %q; transition = %#v", gotFinish, tt.wantFinish, transition)
			}
			stats := store.Stats()
			if tt.wantPublished {
				if stats.Groups != 1 || stats.Calls != 1 {
					t.Fatalf("replay stats = %#v", stats)
				}
			} else if stats.Groups != 0 || stats.Calls != 0 {
				t.Fatalf("replay stats = %#v", stats)
			}
		})
	}
}

func TestResponsesChatStreamValidatesTerminalResponseID(t *testing.T) {
	tests := []struct {
		name       string
		terminalID string
		wantError  bool
	}{
		{name: "matching response ID", terminalID: "resp-stream-id"},
		{name: "missing response ID", terminalID: "", wantError: true},
		{name: "changed opaque response ID", terminalID: "resp-other"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			state := newResponsesChatStreamState(responsesChatStreamConfig{PublicModel: "gpt-public", Now: time.Now})
			mustHandleResponsesChatStatusTransition(t, state.handleCreated, map[string]any{
				"type": "response.created",
				"response": map[string]any{
					"id":         "resp-stream-id",
					"created_at": int64(1_700_000_000),
					"status":     "in_progress",
				},
			})
			terminalPayload, err := json.Marshal(map[string]any{
				"type": "response.completed",
				"response": map[string]any{
					"id":     tt.terminalID,
					"status": "completed",
					"output": []any{},
					"usage":  map[string]any{"input_tokens": 7, "output_tokens": 3, "total_tokens": 10},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			transition, err := state.handleCompleted(terminalPayload)
			if tt.wantError {
				var executionErr *chatExecutionError
				if !errors.As(err, &executionErr) || executionErr.Code != "invalid_responses_stream" {
					t.Fatalf("error = %#v, want invalid_responses_stream", err)
				}
				if executionErr.Usage == nil || executionErr.Usage.TotalTokens != 10 {
					t.Fatalf("error usage = %#v", executionErr.Usage)
				}
				if len(transition.chunks) != 1 || transition.chunks[0].Usage == nil || transition.chunks[0].Usage.TotalTokens != 10 {
					t.Fatalf("transition = %#v", transition)
				}
				return
			}
			if err != nil {
				t.Fatalf("terminal error = %v", err)
			}
			if !transition.terminal {
				t.Fatalf("transition = %#v", transition)
			}
		})
	}
}
