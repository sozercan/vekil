package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sozercan/vekil/models"
)

type policyResponsesResponse struct {
	ID                string                       `json:"id"`
	Object            string                       `json:"object"`
	CreatedAt         int64                        `json:"created_at"`
	Status            string                       `json:"status"`
	Model             string                       `json:"model"`
	ParallelToolCalls bool                         `json:"parallel_tool_calls"`
	Text              json.RawMessage              `json:"text,omitempty"`
	ToolChoice        json.RawMessage              `json:"tool_choice"`
	Tools             json.RawMessage              `json:"tools"`
	Output            []map[string]any             `json:"output"`
	Usage             policyResponsesResponseUsage `json:"usage"`
	IncompleteDetails any                          `json:"incomplete_details"`
	Error             any                          `json:"error"`
}

type policyResponsesResponseUsage struct {
	InputTokens         int            `json:"input_tokens"`
	InputTokensDetails  map[string]int `json:"input_tokens_details"`
	OutputTokens        int            `json:"output_tokens"`
	OutputTokensDetails map[string]int `json:"output_tokens_details"`
	TotalTokens         int            `json:"total_tokens"`
}

func buildPolicyResponsesResponse(completion *models.OpenAIResponse, publicModel string, tools policyResponsesToolMap, config ...policyResponsesResponseConfig) (policyResponsesResponse, error) {
	if completion == nil {
		return policyResponsesResponse{}, fmt.Errorf("policy Chat completion is unavailable")
	}
	if len(completion.Choices) == 0 {
		return policyResponsesResponse{}, fmt.Errorf("policy Chat completion returned no choices")
	}
	choice := completion.Choices[0]
	responseConfig := policyResponsesResponseConfig{Tools: json.RawMessage(`[]`), ToolChoice: json.RawMessage(`"auto"`), ParallelToolCalls: true}
	if len(config) > 0 {
		responseConfig = config[0]
		if len(bytes.TrimSpace(responseConfig.Tools)) == 0 {
			responseConfig.Tools = json.RawMessage(`[]`)
		}
		if len(bytes.TrimSpace(responseConfig.ToolChoice)) == 0 {
			responseConfig.ToolChoice = json.RawMessage(`"auto"`)
		}
	}
	responseID := newPolicyResponsesID("resp")
	createdAt := completion.Created
	if createdAt <= 0 {
		createdAt = time.Now().Unix()
	}
	response := policyResponsesResponse{
		ID:                responseID,
		Object:            "response",
		CreatedAt:         createdAt,
		Status:            "completed",
		Model:             strings.TrimSpace(publicModel),
		ParallelToolCalls: responseConfig.ParallelToolCalls,
		Text:              cloneReplayRawMessage(responseConfig.Text),
		ToolChoice:        cloneReplayRawMessage(responseConfig.ToolChoice),
		Tools:             cloneReplayRawMessage(responseConfig.Tools),
		Output:            make([]map[string]any, 0, 1+len(choice.Message.ToolCalls)),
		Usage:             policyResponsesUsageFromChat(completion.Usage),
		Error:             nil,
	}
	if response.Model == "" {
		response.Model = strings.TrimSpace(completion.Model)
	}
	finishReason := ""
	if choice.FinishReason != nil {
		finishReason = strings.TrimSpace(*choice.FinishReason)
	}
	switch finishReason {
	case "stop":
		if len(choice.Message.ToolCalls) > 0 {
			return policyResponsesResponse{}, fmt.Errorf("policy Chat completion returned function calls with finish reason %q", finishReason)
		}
		if responseConfig.RequiresToolCall {
			return policyResponsesResponse{}, fmt.Errorf("policy Chat completion stopped without the required function call")
		}
	case "tool_calls", "function_call":
		if len(choice.Message.ToolCalls) == 0 {
			return policyResponsesResponse{}, fmt.Errorf("policy Chat completion returned finish reason %q without function calls", finishReason)
		}
		if !responseConfig.ParallelToolCalls && len(choice.Message.ToolCalls) > 1 {
			return policyResponsesResponse{}, fmt.Errorf("policy Chat completion returned parallel function calls while parallel_tool_calls is false")
		}
	case "length":
		response.Status = "incomplete"
		response.IncompleteDetails = map[string]any{"reason": "max_output_tokens"}
	case "content_filter":
		response.Status = "incomplete"
		response.IncompleteDetails = map[string]any{"reason": "content_filter"}
	case "":
		return policyResponsesResponse{}, fmt.Errorf("policy Chat completion returned no finish reason")
	default:
		return policyResponsesResponse{}, fmt.Errorf("policy Chat completion returned unsupported finish reason %q", finishReason)
	}

	text, textPresent, err := policyResponsesTextFromChatContent(choice.Message.Content)
	if err != nil {
		return policyResponsesResponse{}, err
	}
	refusal, refusalPresent, err := policyResponsesRefusalFromChatContent(choice.Message.Refusal)
	if err != nil {
		return policyResponsesResponse{}, err
	}
	messageContent := make([]any, 0, 2)
	if textPresent {
		messageContent = append(messageContent, map[string]any{"type": "output_text", "text": text})
	}
	if refusalPresent {
		// Codex CLI's Responses decoder accepts output_text but not the newer
		// refusal content variant. Preserve the refusal text in the portable shape.
		messageContent = append(messageContent, map[string]any{"type": "output_text", "text": refusal})
	}
	if len(messageContent) > 0 {
		response.Output = append(response.Output, map[string]any{
			"id":      newPolicyResponsesID("msg"),
			"type":    "message",
			"status":  response.Status,
			"role":    "assistant",
			"content": messageContent,
		})
	}
	// Never expose a function call as executable when Chat terminated before a
	// complete answer. Codex consumes output_item.done calls before the terminal
	// event, so a length/content-filter truncation must not dispatch partial JSON.
	if finishReason == "tool_calls" || finishReason == "function_call" {
		seenCallIDs := make(map[string]struct{}, len(choice.Message.ToolCalls))
		for _, call := range choice.Message.ToolCalls {
			if call.Type != "" && call.Type != policyResponsesToolKindFunction {
				return policyResponsesResponse{}, fmt.Errorf("policy Chat completion returned unsupported tool call type %q", call.Type)
			}
			callID := strings.TrimSpace(call.ID)
			if callID == "" {
				callID = newPolicyResponsesID("call")
			}
			if _, duplicate := seenCallIDs[callID]; duplicate {
				return policyResponsesResponse{}, fmt.Errorf("policy Chat completion returned duplicate function call ID %q", callID)
			}
			seenCallIDs[callID] = struct{}{}
			chatName := strings.TrimSpace(call.Function.Name)
			descriptor, known := tools[chatName]
			if !known {
				return policyResponsesResponse{}, fmt.Errorf("policy Chat completion returned undeclared function tool %q", chatName)
			}
			name := strings.TrimSpace(descriptor.Name)
			namespace := strings.TrimSpace(descriptor.Namespace)
			item := map[string]any{
				"id":        newPolicyResponsesID("fc"),
				"type":      "function_call",
				"status":    "completed",
				"call_id":   callID,
				"name":      name,
				"arguments": call.Function.Arguments,
			}
			if namespace != "" {
				item["namespace"] = namespace
			}
			response.Output = append(response.Output, item)
		}
	}
	if len(response.Output) == 0 && response.Status == "completed" {
		return policyResponsesResponse{}, fmt.Errorf("policy Chat completion returned no text or function calls")
	}
	return response, nil
}

func policyResponsesTextFromChatContent(raw json.RawMessage) (string, bool, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", false, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, true, nil
	}
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", false, fmt.Errorf("policy Chat completion content is not text: %w", err)
	}
	var out strings.Builder
	for index, part := range parts {
		var partType string
		_ = json.Unmarshal(part["type"], &partType)
		switch strings.TrimSpace(partType) {
		case "text", "output_text":
			var value string
			if err := json.Unmarshal(part["text"], &value); err != nil {
				return "", false, fmt.Errorf("policy Chat completion content[%d].text is invalid", index)
			}
			out.WriteString(value)
		default:
			return "", false, fmt.Errorf("policy Chat completion content[%d] has unsupported type %q", index, partType)
		}
	}
	return out.String(), true, nil
}

func policyResponsesRefusalFromChatContent(raw json.RawMessage) (string, bool, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return "", false, nil
	}
	var refusal string
	if err := json.Unmarshal(raw, &refusal); err != nil {
		return "", false, fmt.Errorf("policy Chat completion refusal is not text: %w", err)
	}
	return refusal, true, nil
}

func policyResponsesUsageFromChat(usage *models.OpenAIUsage) policyResponsesResponseUsage {
	converted := policyResponsesResponseUsage{
		InputTokensDetails:  map[string]int{"cached_tokens": 0},
		OutputTokensDetails: map[string]int{"reasoning_tokens": 0},
	}
	if usage == nil {
		return converted
	}
	converted.InputTokens = usage.PromptTokens
	converted.OutputTokens = usage.CompletionTokens
	converted.TotalTokens = usage.TotalTokens
	if converted.TotalTokens == 0 {
		converted.TotalTokens = converted.InputTokens + converted.OutputTokens
	}
	if usage.PromptTokensDetails != nil {
		converted.InputTokensDetails["cached_tokens"] = usage.PromptTokensDetails.CachedTokens
	}
	if usage.CompletionTokensDetails != nil {
		converted.OutputTokensDetails["reasoning_tokens"] = usage.CompletionTokensDetails.ReasoningTokens
	}
	return converted
}

func newPolicyResponsesID(prefix string) string {
	return strings.TrimSpace(prefix) + "_" + strings.ReplaceAll(uuid.NewString(), "-", "")
}

func writePolicyResponsesStreamEvent(w http.ResponseWriter, eventType string, sequence *int, payload map[string]any) error {
	payload["type"] = eventType
	payload["sequence_number"] = *sequence
	if err := writeSSEEvent(w, eventType, payload); err != nil {
		return err
	}
	*sequence = *sequence + 1
	return nil
}

func writePolicyResponsesOutputEvents(w http.ResponseWriter, item map[string]any, outputIndex int, sequence *int) error {
	itemType, _ := item["type"].(string)
	itemID, _ := item["id"].(string)
	if strings.TrimSpace(itemID) == "" {
		return fmt.Errorf("policy Responses output item is missing an ID")
	}
	switch itemType {
	case "message":
		role, _ := item["role"].(string)
		content, ok := item["content"].([]any)
		if role != "assistant" || !ok {
			return fmt.Errorf("policy Responses message output is malformed")
		}
		if err := writePolicyResponsesStreamEvent(w, "response.output_item.added", sequence, map[string]any{
			"output_index": outputIndex,
			"item":         map[string]any{"id": itemID, "type": "message", "status": "in_progress", "role": "assistant", "content": []any{}},
		}); err != nil {
			return err
		}
		for contentIndex, rawPart := range content {
			part, ok := rawPart.(map[string]any)
			if !ok || part["type"] != "output_text" {
				return fmt.Errorf("policy Responses message content is malformed")
			}
			text, ok := part["text"].(string)
			if !ok {
				return fmt.Errorf("policy Responses message text is malformed")
			}
			emptyPart := map[string]any{"type": "output_text", "text": "", "annotations": []any{}}
			finalPart := map[string]any{"type": "output_text", "text": text, "annotations": []any{}}
			base := map[string]any{"item_id": itemID, "output_index": outputIndex, "content_index": contentIndex}
			if err := writePolicyResponsesStreamEvent(w, "response.content_part.added", sequence, map[string]any{
				"item_id": itemID, "output_index": outputIndex, "content_index": contentIndex, "part": emptyPart,
			}); err != nil {
				return err
			}
			if err := writePolicyResponsesStreamEvent(w, "response.output_text.delta", sequence, map[string]any{
				"item_id": base["item_id"], "output_index": base["output_index"], "content_index": base["content_index"], "delta": text, "logprobs": []any{},
			}); err != nil {
				return err
			}
			if err := writePolicyResponsesStreamEvent(w, "response.output_text.done", sequence, map[string]any{
				"item_id": base["item_id"], "output_index": base["output_index"], "content_index": base["content_index"], "text": text, "logprobs": []any{},
			}); err != nil {
				return err
			}
			if err := writePolicyResponsesStreamEvent(w, "response.content_part.done", sequence, map[string]any{
				"item_id": base["item_id"], "output_index": base["output_index"], "content_index": base["content_index"], "part": finalPart,
			}); err != nil {
				return err
			}
		}
	case "function_call":
		callID, _ := item["call_id"].(string)
		name, _ := item["name"].(string)
		arguments, ok := item["arguments"].(string)
		if strings.TrimSpace(callID) == "" || strings.TrimSpace(name) == "" || !ok {
			return fmt.Errorf("policy Responses function-call output is malformed")
		}
		added := map[string]any{
			"id": itemID, "type": "function_call", "status": "in_progress", "call_id": callID, "name": name, "arguments": "",
		}
		if namespace, _ := item["namespace"].(string); namespace != "" {
			added["namespace"] = namespace
		}
		if err := writePolicyResponsesStreamEvent(w, "response.output_item.added", sequence, map[string]any{
			"output_index": outputIndex, "item": added,
		}); err != nil {
			return err
		}
		if err := writePolicyResponsesStreamEvent(w, "response.function_call_arguments.delta", sequence, map[string]any{
			"item_id": itemID, "output_index": outputIndex, "delta": arguments,
		}); err != nil {
			return err
		}
		if err := writePolicyResponsesStreamEvent(w, "response.function_call_arguments.done", sequence, map[string]any{
			"item_id": itemID, "output_index": outputIndex, "name": name, "arguments": arguments,
		}); err != nil {
			return err
		}
	default:
		return fmt.Errorf("policy Responses output item type %q is unsupported", itemType)
	}
	return writePolicyResponsesStreamEvent(w, "response.output_item.done", sequence, map[string]any{
		"output_index": outputIndex, "item": item,
	})
}

func writePolicyResponsesResult(w http.ResponseWriter, response policyResponsesResponse, stream bool) error {
	if !stream {
		w.Header().Set("Content-Type", "application/json")
		return json.NewEncoder(w).Encode(response)
	}
	setSSEHeaders(w)
	w.Header().Del("Content-Length")
	created := response
	created.Status = "in_progress"
	created.Output = []map[string]any{}
	created.Usage = policyResponsesResponseUsage{
		InputTokensDetails:  map[string]int{"cached_tokens": 0},
		OutputTokensDetails: map[string]int{"reasoning_tokens": 0},
	}
	created.IncompleteDetails = nil
	if err := writeSSEEvent(w, "response.created", map[string]any{
		"type":            "response.created",
		"sequence_number": 0,
		"response":        created,
	}); err != nil {
		return err
	}
	sequence := 1
	for index, item := range response.Output {
		if err := writePolicyResponsesOutputEvents(w, item, index, &sequence); err != nil {
			return err
		}
	}
	terminalEvent := "response.completed"
	if response.Status == "incomplete" {
		terminalEvent = "response.incomplete"
	}
	return writeSSEEvent(w, terminalEvent, map[string]any{
		"type":            terminalEvent,
		"sequence_number": sequence,
		"response":        response,
	})
}
