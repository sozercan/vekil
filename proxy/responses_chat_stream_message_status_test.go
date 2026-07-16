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
			finish := transition.chunks[0].Choices[0].FinishReason
			if finish == nil || *finish != tt.wantFinish {
				t.Fatalf("finish reason = %#v, want %q", finish, tt.wantFinish)
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
