package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/sozercan/vekil/models"
)

func TestPolicyResponsesRefusalUsesPortableOutputText(t *testing.T) {
	finishReason := "stop"
	response, err := buildPolicyResponsesResponse(&models.OpenAIResponse{
		ID: "chat-refusal", Created: 1, Model: "terminal",
		Choices: []models.OpenAIChoice{{
			Index:        0,
			Message:      models.OpenAIMessage{Role: "assistant", Refusal: json.RawMessage(`"cannot comply"`)},
			FinishReason: &finishReason,
		}},
	}, "policy", nil)
	if err != nil {
		t.Fatal(err)
	}
	content := response.Output[0]["content"].([]any)
	part := content[0].(map[string]any)
	if part["type"] != "output_text" || part["text"] != "cannot comply" {
		t.Fatalf("refusal output=%+v", response.Output)
	}
	if annotations, ok := part["annotations"].([]any); !ok || len(annotations) != 0 {
		t.Fatalf("refusal annotations = %#v, want empty array", part["annotations"])
	}
	body, err := json.Marshal(map[string]any{"model": "policy", "input": response.Output, "store": false})
	if err != nil {
		t.Fatal(err)
	}
	translated, err := translatePolicyResponsesRequestToChat(body)
	if err != nil {
		t.Fatal(err)
	}
	var chat models.OpenAIRequest
	if err := json.Unmarshal(translated.Body, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 1 || string(chat.Messages[0].Content) != `"cannot comply"` {
		t.Fatalf("replayed refusal=%+v", chat.Messages)
	}
}

func TestPolicyResponsesPreservesEmptyAssistantText(t *testing.T) {
	finishReason := "stop"
	response, err := buildPolicyResponsesResponse(&models.OpenAIResponse{
		ID: "chat-empty", Created: 1, Model: "terminal",
		Choices: []models.OpenAIChoice{{
			Index:        0,
			Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`""`)},
			FinishReason: &finishReason,
		}},
	}, "policy", nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(response.Output) != 1 || response.Output[0]["type"] != "message" {
		t.Fatalf("empty assistant output=%+v", response.Output)
	}
	content := response.Output[0]["content"].([]any)
	if len(content) != 1 || content[0].(map[string]any)["text"] != "" {
		t.Fatalf("empty assistant content=%+v", content)
	}
}

func TestPolicyResponsesResponseIncludesNormalizedToolMetadata(t *testing.T) {
	finishReason := "stop"
	response, err := buildPolicyResponsesResponse(&models.OpenAIResponse{
		Choices: []models.OpenAIChoice{{
			Index: 0, Message: models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)}, FinishReason: &finishReason,
		}},
		Usage: &models.OpenAIUsage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
	}, "policy", nil, policyResponsesResponseConfig{
		Tools:             json.RawMessage(`[{"type":"function","name":"lookup","parameters":{"type":"object"}}]`),
		ToolChoice:        json.RawMessage(`{"type":"function","name":"lookup"}`),
		ParallelToolCalls: false,
	})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	if err := writePolicyResponsesResult(recorder, response, false); err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded["parallel_tool_calls"] != false {
		t.Fatalf("parallel_tool_calls = %#v", decoded["parallel_tool_calls"])
	}
	if tools, ok := decoded["tools"].([]any); !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", decoded["tools"])
	}
	choice, _ := decoded["tool_choice"].(map[string]any)
	if choice["type"] != "function" || choice["name"] != "lookup" {
		t.Fatalf("tool_choice = %#v", decoded["tool_choice"])
	}

	stream := httptest.NewRecorder()
	if err := writePolicyResponsesResult(stream, response, true); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stream.Body.String(), `"parallel_tool_calls":false`) || !strings.Contains(stream.Body.String(), `"tool_choice":{"type":"function","name":"lookup"}`) {
		t.Fatalf("stream metadata missing: %s", stream.Body.String())
	}
}

func TestPolicyResponsesStreamEmitsCoherentEvents(t *testing.T) {
	finishReason := "tool_calls"
	response, err := buildPolicyResponsesResponse(&models.OpenAIResponse{
		ID: "chat-stream", Created: 1, Model: "terminal",
		Choices: []models.OpenAIChoice{{
			Index: 0,
			Message: models.OpenAIMessage{
				Role:    "assistant",
				Content: json.RawMessage(`"working"`),
				ToolCalls: []models.OpenAIToolCall{{
					ID: "call-lookup", Type: "function", Function: models.OpenAIFunctionCall{Name: "lookup", Arguments: `{"q":"x"}`},
				}},
			},
			FinishReason: &finishReason,
		}},
		Usage: &models.OpenAIUsage{PromptTokens: 2, CompletionTokens: 1, TotalTokens: 3},
	}, "policy", policyResponsesToolMap{"lookup": {Name: "lookup", Kind: policyResponsesToolKindFunction}}, policyResponsesResponseConfig{ParallelToolCalls: true})
	if err != nil {
		t.Fatal(err)
	}
	recorder := httptest.NewRecorder()
	if err := writePolicyResponsesResult(recorder, response, true); err != nil {
		t.Fatal(err)
	}
	var gotTypes []string
	sequence := 0
	if err := consumeResponsesSSEMessages(strings.NewReader(recorder.Body.String()), func(msg responsesSSEMessage) error {
		var event struct {
			Type           string `json:"type"`
			SequenceNumber int    `json:"sequence_number"`
		}
		if err := json.Unmarshal([]byte(msg.data), &event); err != nil {
			return err
		}
		var payload map[string]any
		if err := json.Unmarshal([]byte(msg.data), &payload); err != nil {
			return err
		}
		switch event.Type {
		case "response.output_text.delta", "response.output_text.done":
			if logprobs, ok := payload["logprobs"].([]any); !ok || len(logprobs) != 0 {
				t.Fatalf("%s logprobs = %#v, want empty array", event.Type, payload["logprobs"])
			}
		case "response.function_call_arguments.done":
			if payload["name"] != "lookup" {
				t.Fatalf("function arguments done name = %#v, want lookup", payload["name"])
			}
		}
		if event.SequenceNumber != sequence {
			t.Fatalf("sequence for %s = %d, want %d", event.Type, event.SequenceNumber, sequence)
		}
		sequence++
		gotTypes = append(gotTypes, event.Type)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wantTypes := []string{
		"response.created",
		"response.output_item.added",
		"response.content_part.added",
		"response.output_text.delta",
		"response.output_text.done",
		"response.content_part.done",
		"response.output_item.done",
		"response.output_item.added",
		"response.function_call_arguments.delta",
		"response.function_call_arguments.done",
		"response.output_item.done",
		"response.completed",
	}
	if strings.Join(gotTypes, ",") != strings.Join(wantTypes, ",") {
		t.Fatalf("event types = %v, want %v\n%s", gotTypes, wantTypes, recorder.Body.String())
	}

	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()
	stream, err := translateResponsesSSEToChat(context.Background(), io.NopCloser(strings.NewReader(recorder.Body.String())), responsesChatResponseOptions{
		PublicModel: "policy",
		ReplayStore: store,
		ReplayRoute: responsesChatReplayRoute{ProviderID: "bridge", PublicModel: "policy", UpstreamModel: "terminal"},
	})
	if err != nil {
		t.Fatalf("strict Responses stream parser rejected synthetic stream: %v\n%s", err, recorder.Body.String())
	}
	chunks := collectResponsesChatStreamChunks(t, stream)
	var text strings.Builder
	toolCalls := 0
	for _, chunk := range chunks {
		for _, choice := range chunk.Choices {
			if len(choice.Delta.Content) > 0 {
				var delta string
				_ = json.Unmarshal(choice.Delta.Content, &delta)
				text.WriteString(delta)
			}
			toolCalls += len(choice.Delta.ToolCalls)
		}
	}
	if text.String() != "working" || toolCalls == 0 {
		t.Fatalf("round-trip text/tool calls = %q/%d; chunks=%#v", text.String(), toolCalls, chunks)
	}
}

func TestPolicyResponsesResponseNormalizesMissingUsage(t *testing.T) {
	finishReason := "stop"
	response, err := buildPolicyResponsesResponse(&models.OpenAIResponse{Choices: []models.OpenAIChoice{{
		Index: 0, Message: models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"ok"`)}, FinishReason: &finishReason,
	}}}, "policy", nil, policyResponsesResponseConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if response.Usage.InputTokens != 0 || response.Usage.OutputTokens != 0 || response.Usage.TotalTokens != 0 {
		t.Fatalf("usage = %+v, want zero-valued fallback", response.Usage)
	}
	if response.Usage.InputTokensDetails["cached_tokens"] != 0 || response.Usage.InputTokensDetails["cache_write_tokens"] != 0 || response.Usage.OutputTokensDetails["reasoning_tokens"] != 0 {
		t.Fatalf("usage details = %+v/%+v", response.Usage.InputTokensDetails, response.Usage.OutputTokensDetails)
	}
}

func TestPolicyResponsesTextOutputCanBeReplayedAsStatelessInput(t *testing.T) {
	finishReason := "stop"
	response, err := buildPolicyResponsesResponse(&models.OpenAIResponse{
		ID: "chat-text", Created: 1, Model: "terminal",
		Choices: []models.OpenAIChoice{{
			Index:        0,
			Message:      models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"hello"`)},
			FinishReason: &finishReason,
		}},
	}, "policy", nil)
	if err != nil {
		t.Fatal(err)
	}
	input := append([]map[string]any{}, response.Output...)
	input = append(input, map[string]any{
		"type":    "message",
		"role":    "user",
		"content": []any{map[string]any{"type": "input_text", "text": "continue"}},
	})
	body, err := json.Marshal(map[string]any{"model": "policy", "input": input, "store": false})
	if err != nil {
		t.Fatal(err)
	}
	translated, err := translatePolicyResponsesRequestToChat(body)
	if err != nil {
		t.Fatalf("replay adapter output: %v", err)
	}
	var chat models.OpenAIRequest
	if err := json.Unmarshal(translated.Body, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 || chat.Messages[0].Role != "assistant" || chat.Messages[1].Role != "user" {
		t.Fatalf("replayed messages=%+v", chat.Messages)
	}
}

func TestPolicyResponsesRejectsMalformedTerminalChoice(t *testing.T) {
	finishStop := "stop"
	for _, role := range []string{"", "user", "tool", "system", "Assistant"} {
		t.Run("role "+role, func(t *testing.T) {
			_, err := buildPolicyResponsesResponse(&models.OpenAIResponse{Choices: []models.OpenAIChoice{{
				Index: 0, Message: models.OpenAIMessage{Role: role, Content: json.RawMessage(`"ok"`)}, FinishReason: &finishStop,
			}}}, "policy", nil)
			if err == nil || !strings.Contains(err.Error(), "non-assistant role") {
				t.Fatalf("error = %v", err)
			}
		})
	}

	finishCalls := "tool_calls"
	_, err := buildPolicyResponsesResponse(&models.OpenAIResponse{Choices: []models.OpenAIChoice{{
		Index: 0,
		Message: models.OpenAIMessage{
			Role:    "assistant",
			Refusal: json.RawMessage(`"cannot comply"`),
			ToolCalls: []models.OpenAIToolCall{{
				ID: "call-lookup", Type: "function", Function: models.OpenAIFunctionCall{Name: "lookup", Arguments: `{}`},
			}},
		},
		FinishReason: &finishCalls,
	}}}, "policy", policyResponsesToolMap{"lookup": {Name: "lookup", Kind: policyResponsesToolKindFunction}})
	if err == nil || !strings.Contains(err.Error(), "refusal with executable function calls") {
		t.Fatalf("error = %v", err)
	}
}

func TestPolicyResponsesIncompleteAllowsRefusalWithSuppressedPartialCall(t *testing.T) {
	finishReason := "length"
	response, err := buildPolicyResponsesResponse(&models.OpenAIResponse{Choices: []models.OpenAIChoice{{
		Index: 0,
		Message: models.OpenAIMessage{
			Role:    "assistant",
			Refusal: json.RawMessage(`"cannot finish"`),
			ToolCalls: []models.OpenAIToolCall{{
				ID: "call-partial", Type: "function", Function: models.OpenAIFunctionCall{Name: "lookup", Arguments: `{"unterminated":`},
			}},
		},
		FinishReason: &finishReason,
	}}}, "policy", policyResponsesToolMap{"lookup": {Name: "lookup", Kind: policyResponsesToolKindFunction}})
	if err != nil {
		t.Fatal(err)
	}
	if response.Status != "incomplete" || len(response.Output) != 1 || response.Output[0]["type"] != "message" {
		t.Fatalf("response = %+v, want incomplete refusal without executable call", response)
	}
}

func TestPolicyResponsesRejectsMalformedTerminalFinishReason(t *testing.T) {
	call := models.OpenAIToolCall{
		ID:   "call-lookup",
		Type: "function",
		Function: models.OpenAIFunctionCall{
			Name:      "lookup",
			Arguments: `{}`,
		},
	}
	tools := policyResponsesToolMap{"lookup": {Name: "lookup", Kind: policyResponsesToolKindFunction}}
	for _, tc := range []struct {
		name             string
		finishReason     string
		hasFinishReason  bool
		withCall         bool
		requiresToolCall bool
		wantError        string
	}{
		{name: "missing", wantError: "no finish reason"},
		{name: "empty", finishReason: "  ", hasFinishReason: true, wantError: "no finish reason"},
		{name: "unknown", finishReason: "mystery", hasFinishReason: true, wantError: "unsupported finish reason"},
		{name: "stop with call", finishReason: "stop", hasFinishReason: true, withCall: true, wantError: "function calls with finish reason"},
		{name: "required call omitted", finishReason: "stop", hasFinishReason: true, requiresToolCall: true, wantError: "required function call"},
		{name: "tool calls without call", finishReason: "tool_calls", hasFinishReason: true, wantError: "without function calls"},
		{name: "legacy function call without call", finishReason: "function_call", hasFinishReason: true, wantError: "without function calls"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var finishReason *string
			if tc.hasFinishReason {
				value := tc.finishReason
				finishReason = &value
			}
			var calls []models.OpenAIToolCall
			if tc.withCall {
				calls = []models.OpenAIToolCall{call}
			}
			var config []policyResponsesResponseConfig
			if tc.requiresToolCall {
				config = append(config, policyResponsesResponseConfig{RequiresToolCall: true, ParallelToolCalls: true})
			}
			_, err := buildPolicyResponsesResponse(&models.OpenAIResponse{Choices: []models.OpenAIChoice{{
				Index: 0,
				Message: models.OpenAIMessage{
					Role:      "assistant",
					Content:   json.RawMessage(`"ok"`),
					ToolCalls: calls,
				},
				FinishReason: finishReason,
			}}}, "policy", tools, config...)
			if err == nil || !strings.Contains(err.Error(), tc.wantError) {
				t.Fatalf("error = %v, want %q", err, tc.wantError)
			}
		})
	}
}

func TestPolicyResponsesIncompleteDoesNotCompleteOrDispatchPartialToolCall(t *testing.T) {
	for _, tc := range []struct {
		finishReason string
		wantReason   string
	}{
		{finishReason: "length", wantReason: "max_output_tokens"},
		{finishReason: "content_filter", wantReason: "content_filter"},
	} {
		t.Run(tc.finishReason, func(t *testing.T) {
			finishReason := tc.finishReason
			completion := &models.OpenAIResponse{
				ID:      "chat-partial",
				Created: 1,
				Model:   "terminal-model",
				Choices: []models.OpenAIChoice{{
					Index: 0,
					Message: models.OpenAIMessage{
						Role:    "assistant",
						Content: json.RawMessage(`"partial"`),
						ToolCalls: []models.OpenAIToolCall{{
							ID:   "call-partial",
							Type: "function",
							Function: models.OpenAIFunctionCall{
								Name:      "tool_alias",
								Arguments: `{"unterminated":`,
							},
						}},
					},
					FinishReason: &finishReason,
				}},
			}
			response, err := buildPolicyResponsesResponse(completion, "policy-model", policyResponsesToolMap{
				"tool_alias": {Name: "run", Namespace: "tools", Kind: policyResponsesToolKindFunction},
			})
			if err != nil {
				t.Fatal(err)
			}
			if response.Status != "incomplete" {
				t.Fatalf("status=%q", response.Status)
			}
			details, _ := response.IncompleteDetails.(map[string]any)
			if details["reason"] != tc.wantReason {
				t.Fatalf("incomplete_details=%+v", response.IncompleteDetails)
			}
			if len(response.Output) != 1 || response.Output[0]["type"] != "message" {
				t.Fatalf("partial function call was exposed: %+v", response.Output)
			}

			recorder := httptest.NewRecorder()
			if err := writePolicyResponsesResult(recorder, response, true); err != nil {
				t.Fatal(err)
			}
			body := recorder.Body.String()
			sawCreatedSequenceZero := false
			if err := consumeResponsesSSEMessages(strings.NewReader(body), func(msg responsesSSEMessage) error {
				if msg.event != "response.created" {
					return nil
				}
				var event struct {
					SequenceNumber int `json:"sequence_number"`
				}
				if err := json.Unmarshal([]byte(msg.data), &event); err != nil {
					return err
				}
				sawCreatedSequenceZero = event.SequenceNumber == 0
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if !sawCreatedSequenceZero {
				t.Fatalf("created event is missing sequence zero: %s", body)
			}
			if !strings.Contains(body, "event: response.incomplete") || strings.Contains(body, "event: response.completed") || strings.Contains(body, `"type":"function_call"`) {
				t.Fatalf("invalid incomplete SSE: %s", body)
			}
		})
	}
	t.Run("required tool choice remains incomplete", func(t *testing.T) {
		finishReason := "length"
		response, err := buildPolicyResponsesResponse(&models.OpenAIResponse{Choices: []models.OpenAIChoice{{
			Index: 0, Message: models.OpenAIMessage{Role: "assistant", Content: json.RawMessage(`"partial"`)}, FinishReason: &finishReason,
		}}}, "policy", nil, policyResponsesResponseConfig{RequiresToolCall: true, ParallelToolCalls: true})
		if err != nil {
			t.Fatal(err)
		}
		if response.Status != "incomplete" {
			t.Fatalf("status = %q, want incomplete", response.Status)
		}
	})
}

func TestPolicyResponsesRejectsTerminalCallsOutsideRequestCapability(t *testing.T) {
	finishReason := "tool_calls"
	completion := func(calls ...models.OpenAIToolCall) *models.OpenAIResponse {
		return &models.OpenAIResponse{Choices: []models.OpenAIChoice{{
			Index: 0, Message: models.OpenAIMessage{Role: "assistant", ToolCalls: calls}, FinishReason: &finishReason,
		}}}
	}
	call := func(id, name string) models.OpenAIToolCall {
		return models.OpenAIToolCall{ID: id, Type: "function", Function: models.OpenAIFunctionCall{Name: name, Arguments: `{}`}}
	}
	t.Run("undeclared tool", func(t *testing.T) {
		_, err := buildPolicyResponsesResponse(completion(call("call-shell", "shell")), "policy", policyResponsesToolMap{
			"lookup": {Name: "lookup", Kind: policyResponsesToolKindFunction},
		})
		if err == nil || !strings.Contains(err.Error(), "undeclared function tool") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("tool choice none", func(t *testing.T) {
		_, err := buildPolicyResponsesResponse(completion(call("call-lookup", "lookup")), "policy", nil)
		if err == nil || !strings.Contains(err.Error(), "undeclared function tool") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("duplicate call id", func(t *testing.T) {
		_, err := buildPolicyResponsesResponse(completion(call("call-dup", "lookup"), call("call-dup", "lookup")), "policy", policyResponsesToolMap{
			"lookup": {Name: "lookup", Kind: policyResponsesToolKindFunction},
		})
		if err == nil || !strings.Contains(err.Error(), "duplicate function call ID") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("required call satisfied", func(t *testing.T) {
		response, err := buildPolicyResponsesResponse(completion(call("call-required", "lookup")), "policy", policyResponsesToolMap{
			"lookup": {Name: "lookup", Kind: policyResponsesToolKindFunction},
		}, policyResponsesResponseConfig{RequiresToolCall: true, ParallelToolCalls: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Output) != 1 || response.Output[0]["call_id"] != "call-required" {
			t.Fatalf("output = %+v", response.Output)
		}
	})
	t.Run("parallel calls disabled", func(t *testing.T) {
		_, err := buildPolicyResponsesResponse(completion(call("call-a", "lookup"), call("call-b", "lookup")), "policy", policyResponsesToolMap{
			"lookup": {Name: "lookup", Kind: policyResponsesToolKindFunction},
		}, policyResponsesResponseConfig{ParallelToolCalls: false})
		if err == nil || !strings.Contains(err.Error(), "parallel function calls") {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("single call with parallel calls disabled", func(t *testing.T) {
		response, err := buildPolicyResponsesResponse(completion(call("call-a", "lookup")), "policy", policyResponsesToolMap{
			"lookup": {Name: "lookup", Kind: policyResponsesToolKindFunction},
		}, policyResponsesResponseConfig{ParallelToolCalls: false})
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Output) != 1 || response.ParallelToolCalls {
			t.Fatalf("response = %+v, want one call with parallel metadata false", response)
		}
	})
	t.Run("parallel calls enabled", func(t *testing.T) {
		response, err := buildPolicyResponsesResponse(completion(call("call-a", "lookup"), call("call-b", "lookup")), "policy", policyResponsesToolMap{
			"lookup": {Name: "lookup", Kind: policyResponsesToolKindFunction},
		}, policyResponsesResponseConfig{ParallelToolCalls: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Output) != 2 {
			t.Fatalf("output = %+v, want two function calls", response.Output)
		}
	})
	for _, arguments := range []string{"", "{not-json"} {
		t.Run("invalid arguments "+arguments, func(t *testing.T) {
			invalid := call("call-invalid-arguments", "lookup")
			invalid.Function.Arguments = arguments
			_, err := buildPolicyResponsesResponse(completion(invalid), "policy", policyResponsesToolMap{
				"lookup": {Name: "lookup", Kind: policyResponsesToolKindFunction},
			})
			if err == nil || !strings.Contains(err.Error(), "invalid JSON arguments") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	for _, callType := range []string{"custom", "Function", " ", " function "} {
		t.Run("unsupported tool call type "+callType, func(t *testing.T) {
			invalid := call("call-invalid", "lookup")
			invalid.Type = callType
			_, err := buildPolicyResponsesResponse(completion(invalid), "policy", policyResponsesToolMap{
				"lookup": {Name: "lookup", Kind: policyResponsesToolKindFunction},
			})
			if err == nil || !strings.Contains(err.Error(), "unsupported tool call type") {
				t.Fatalf("error = %v", err)
			}
		})
	}
	t.Run("omitted tool call type on wire", func(t *testing.T) {
		var omitted models.OpenAIResponse
		if err := json.Unmarshal([]byte(`{"choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"id":"call-omitted","function":{"name":"lookup","arguments":"{}"}}]},"finish_reason":"tool_calls"}]}`), &omitted); err != nil {
			t.Fatal(err)
		}
		response, err := buildPolicyResponsesResponse(&omitted, "policy", policyResponsesToolMap{
			"lookup": {Name: "lookup", Kind: policyResponsesToolKindFunction},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Output) != 1 || response.Output[0]["type"] != "function_call" {
			t.Fatalf("output = %+v", response.Output)
		}
	})
	t.Run("legacy function call finish reason", func(t *testing.T) {
		legacyFinishReason := "function_call"
		response, err := buildPolicyResponsesResponse(&models.OpenAIResponse{Choices: []models.OpenAIChoice{{
			Index: 0, Message: models.OpenAIMessage{Role: "assistant", ToolCalls: []models.OpenAIToolCall{call("call-legacy", "lookup")}}, FinishReason: &legacyFinishReason,
		}}}, "policy", policyResponsesToolMap{
			"lookup": {Name: "lookup", Kind: policyResponsesToolKindFunction},
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(response.Output) != 1 || response.Output[0]["call_id"] != "call-legacy" {
			t.Fatalf("output = %+v", response.Output)
		}
	})
}

func TestPolicyResponsesMixedTextToolReplayMatchesInnerStore(t *testing.T) {
	store := newResponsesChatReplayStore()
	defer func() { _ = store.Close() }()
	route := responsesChatReplayRoute{ProviderID: "bridge", PublicModel: "light-model", UpstreamModel: "gpt-5.6-luna"}
	arguments := `{"cmd":"ls"}`
	outputItem, _ := json.Marshal(map[string]any{
		"type": "function_call", "id": "fc-upstream", "call_id": "upstream-call", "name": "exec_command", "arguments": arguments,
	})
	published, err := store.Publish(responsesChatReplayPublishRequest{
		Route: route, AssistantContent: json.RawMessage(`"I will inspect first."`),
		OutputItems: []json.RawMessage{outputItem},
		Calls: []responsesChatReplayPublishCall{{
			UpstreamCallID: "upstream-call", Name: "exec_command", VisibleArguments: arguments, OutputItemIndex: 0,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	proxyID := published.Projection.Calls[0].ID
	finishReason := "tool_calls"
	adapted, err := buildPolicyResponsesResponse(&models.OpenAIResponse{
		ID: "chat-mixed", Created: 1, Model: "policy",
		Choices: []models.OpenAIChoice{{
			Index: 0,
			Message: models.OpenAIMessage{
				Role:    "assistant",
				Content: json.RawMessage(`"I will inspect first."`),
				ToolCalls: []models.OpenAIToolCall{{
					ID: proxyID, Type: "function", Function: models.OpenAIFunctionCall{Name: "exec_command", Arguments: arguments},
				}},
			},
			FinishReason: &finishReason,
		}},
	}, "policy", policyResponsesToolMap{"exec_command": {Name: "exec_command", Kind: policyResponsesToolKindFunction}})
	if err != nil {
		t.Fatal(err)
	}
	input := append([]map[string]any{}, adapted.Output...)
	input = append(input, map[string]any{"type": "function_call_output", "call_id": proxyID, "output": "ok"})
	requestBody, _ := json.Marshal(map[string]any{
		"model": "policy", "input": input, "store": false,
		"tools": []any{map[string]any{"type": "function", "name": "exec_command", "parameters": map[string]any{"type": "object"}}},
	})
	translated, err := translatePolicyResponsesRequestToChat(requestBody)
	if err != nil {
		t.Fatal(err)
	}
	var chat models.OpenAIRequest
	if err := json.Unmarshal(translated.Body, &chat); err != nil {
		t.Fatal(err)
	}
	if len(chat.Messages) != 2 || string(chat.Messages[0].Content) != `"I will inspect first."` || len(chat.Messages[0].ToolCalls) != 1 {
		t.Fatalf("mixed Chat reconstruction=%+v", chat.Messages)
	}
	if _, err := translateChatRequestToResponses(translated.Body, responsesChatRequestOptions{ReplayStore: store, ReplayRoute: route}); err != nil {
		t.Fatalf("inner replay projection mismatch: %v", err)
	}
}
