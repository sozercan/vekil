package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sozercan/vekil/models"
)

const openAIChatCompletionObject = "chat.completion"

// normalizeOpenAIChatCompletionResponse fills in OpenAI Chat Completions fields
// that strict SDK clients require while preserving upstream/vendor-specific
// fields. It is intentionally map-based instead of models.OpenAIResponse-based
// so unknown top-level and nested fields survive unchanged.
func normalizeOpenAIChatCompletionResponse(body []byte, requestedModel string, now time.Time) ([]byte, bool, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, false, err
	}
	if payload == nil {
		return nil, false, fmt.Errorf("chat completion response is not a JSON object")
	}

	changed := false
	if !hasNonNullJSONField(payload, "object") {
		payload["object"] = mustMarshalRaw(openAIChatCompletionObject)
		changed = true
	}
	if !hasNonNullJSONField(payload, "created") {
		created := now.Unix()
		if created <= 0 {
			created = time.Now().Unix()
		}
		payload["created"] = mustMarshalRaw(created)
		changed = true
	}
	if !hasNonEmptyStringJSONField(payload, "id") {
		payload["id"] = mustMarshalRaw("chatcmpl-" + uuid.NewString())
		changed = true
	}
	if !hasNonEmptyStringJSONField(payload, "model") {
		if model := strings.TrimSpace(requestedModel); model != "" {
			payload["model"] = mustMarshalRaw(model)
			changed = true
		}
	}

	usage, usageChanged := normalizeOpenAIChatCompletionUsage(payload["usage"])
	if usageChanged {
		payload["usage"] = usage
		changed = true
	}

	if !hasNonNullJSONField(payload, "choices") {
		payload["choices"] = json.RawMessage("[]")
		changed = true
	} else {
		choices, choicesChanged, err := normalizeOpenAIChatCompletionChoices(payload["choices"])
		if err != nil {
			return nil, false, err
		}
		if choicesChanged {
			payload["choices"] = choices
			changed = true
		}
	}

	if !changed {
		return body, false, nil
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func normalizeOpenAIChatCompletionStruct(resp *models.OpenAIResponse, requestedModel string) {
	if resp == nil {
		return
	}
	if strings.TrimSpace(resp.ID) == "" {
		resp.ID = "chatcmpl-" + uuid.NewString()
	}
	if strings.TrimSpace(resp.Object) == "" {
		resp.Object = openAIChatCompletionObject
	}
	if resp.Created == 0 {
		resp.Created = time.Now().Unix()
	}
	if strings.TrimSpace(resp.Model) == "" {
		resp.Model = strings.TrimSpace(requestedModel)
	}
	if resp.Usage == nil {
		resp.Usage = &models.OpenAIUsage{}
	} else if resp.Usage.TotalTokens == 0 && (resp.Usage.PromptTokens != 0 || resp.Usage.CompletionTokens != 0) {
		resp.Usage.TotalTokens = resp.Usage.PromptTokens + resp.Usage.CompletionTokens
	}

	for i := range resp.Choices {
		choice := &resp.Choices[i]
		if choice.FinishReason == nil || strings.TrimSpace(*choice.FinishReason) == "" {
			finishReason := defaultOpenAIChatStructFinishReason(*choice)
			choice.FinishReason = &finishReason
		}
		if strings.TrimSpace(choice.Message.Role) == "" {
			choice.Message.Role = "assistant"
		}
		if len(bytes.TrimSpace(choice.Message.Content)) == 0 {
			choice.Message.Content = json.RawMessage(`""`)
		}
	}
}

func defaultOpenAIChatStructFinishReason(choice models.OpenAIChoice) string {
	if len(choice.Message.ToolCalls) > 0 {
		return "tool_calls"
	}
	return "stop"
}

func normalizeOpenAIChatCompletionChoices(raw json.RawMessage) (json.RawMessage, bool, error) {
	var choices []json.RawMessage
	if err := json.Unmarshal(raw, &choices); err != nil {
		return nil, false, err
	}

	changed := false
	for i := range choices {
		choiceRaw, choiceChanged, err := normalizeOpenAIChatCompletionChoice(choices[i], i)
		if err != nil {
			return nil, false, err
		}
		if choiceChanged {
			choices[i] = choiceRaw
			changed = true
		}
	}

	if !changed {
		return raw, false, nil
	}
	out, err := json.Marshal(choices)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func normalizeOpenAIChatCompletionChoice(raw json.RawMessage, arrayIndex int) (json.RawMessage, bool, error) {
	var choice map[string]json.RawMessage
	if len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &choice); err != nil {
			return nil, false, err
		}
	}
	if choice == nil {
		choice = make(map[string]json.RawMessage)
	}

	changed := false
	if !hasNonNullJSONField(choice, "index") {
		choice["index"] = mustMarshalRaw(arrayIndex)
		changed = true
	}
	if !hasNonNullJSONField(choice, "finish_reason") {
		choice["finish_reason"] = mustMarshalRaw(defaultOpenAIChatChoiceFinishReason(choice))
		changed = true
	}

	message, messageChanged := normalizeOpenAIChatCompletionMessage(choice)
	if messageChanged {
		choice["message"] = message
		changed = true
	}

	if !changed {
		return raw, false, nil
	}
	out, err := json.Marshal(choice)
	if err != nil {
		return nil, false, err
	}
	return out, true, nil
}

func normalizeOpenAIChatCompletionMessage(choice map[string]json.RawMessage) (json.RawMessage, bool) {
	var message map[string]json.RawMessage
	changed := false
	if raw, ok := choice["message"]; ok && len(bytes.TrimSpace(raw)) > 0 && string(bytes.TrimSpace(raw)) != "null" {
		if err := json.Unmarshal(raw, &message); err == nil && message != nil {
			// Preserve existing message object fields below.
		} else {
			message = make(map[string]json.RawMessage)
			changed = true
		}
	} else {
		message = make(map[string]json.RawMessage)
		changed = true
	}

	if !hasNonEmptyStringJSONField(message, "role") {
		message["role"] = mustMarshalRaw("assistant")
		changed = true
	}
	if !hasNonNullJSONField(message, "content") {
		if content, ok := firstProviderChoiceContent(choice); ok {
			message["content"] = content
		} else {
			message["content"] = mustMarshalRaw("")
		}
		changed = true
	}

	if !changed {
		return choice["message"], false
	}
	out, err := json.Marshal(message)
	if err != nil {
		return choice["message"], false
	}
	return out, true
}

func firstProviderChoiceContent(choice map[string]json.RawMessage) (json.RawMessage, bool) {
	for _, key := range []string{"content", "text", "output_text"} {
		raw, ok := choice[key]
		if !ok || len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
			continue
		}
		if isJSONString(raw) {
			return raw, true
		}
	}
	return nil, false
}

func defaultOpenAIChatChoiceFinishReason(choice map[string]json.RawMessage) string {
	if hasNonNullJSONField(choice, "tool_calls") || hasNonNullJSONField(choice, "function_call") {
		return "tool_calls"
	}
	if raw, ok := choice["stop_reason"]; ok {
		var reason string
		if err := json.Unmarshal(raw, &reason); err == nil {
			switch strings.TrimSpace(reason) {
			case "tool_use":
				return "tool_calls"
			case "max_tokens":
				return "length"
			case "end_turn", "stop_sequence":
				return "stop"
			}
		}
	}
	return "stop"
}

func normalizeOpenAIChatCompletionUsage(raw json.RawMessage) (json.RawMessage, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return json.RawMessage(`{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}`), true
	}

	var usage map[string]json.RawMessage
	if err := json.Unmarshal(raw, &usage); err != nil || usage == nil {
		return raw, false
	}

	changed := false
	prompt, hasPrompt := jsonRawNumberAsInt64(usage["prompt_tokens"])
	completion, hasCompletion := jsonRawNumberAsInt64(usage["completion_tokens"])
	if !hasPrompt {
		usage["prompt_tokens"] = json.RawMessage("0")
		prompt = 0
		changed = true
	}
	if !hasCompletion {
		usage["completion_tokens"] = json.RawMessage("0")
		completion = 0
		changed = true
	}
	if _, hasTotal := jsonRawNumberAsInt64(usage["total_tokens"]); !hasTotal {
		if hasPrompt || hasCompletion {
			usage["total_tokens"] = json.RawMessage(strconv.FormatInt(prompt+completion, 10))
		} else {
			usage["total_tokens"] = json.RawMessage("0")
		}
		changed = true
	}

	if !changed {
		return raw, false
	}
	out, err := json.Marshal(usage)
	if err != nil {
		return raw, false
	}
	return out, true
}

func hasNonNullJSONField(m map[string]json.RawMessage, key string) bool {
	raw, ok := m[key]
	if !ok {
		return false
	}
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) > 0 && string(trimmed) != "null"
}

func hasNonEmptyStringJSONField(m map[string]json.RawMessage, key string) bool {
	raw, ok := m[key]
	if !ok {
		return false
	}
	var value string
	return json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != ""
}

func isJSONString(raw json.RawMessage) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil
}

func jsonRawNumberAsInt64(raw json.RawMessage) (int64, bool) {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return 0, false
	}
	var number json.Number
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if err := dec.Decode(&number); err != nil {
		return 0, false
	}
	value, err := number.Int64()
	if err != nil {
		return 0, false
	}
	return value, true
}

func mustMarshalRaw(v interface{}) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
