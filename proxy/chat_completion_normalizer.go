package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/sozercan/vekil/models"
)

const (
	openAIChatCompletionObject      = "chat.completion"
	openAIChatCompletionChunkObject = "chat.completion.chunk"
)

type openAIChatCompletionChunkNormalizer struct {
	requestedModel    string
	streamID          string
	syntheticStreamID bool
	created           int64
	model             string
}

func newOpenAIChatCompletionChunkNormalizer(requestedModel string) *openAIChatCompletionChunkNormalizer {
	return &openAIChatCompletionChunkNormalizer{requestedModel: strings.TrimSpace(requestedModel)}
}

func openAIChatStreamEventMayCarryChunk(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "", "message", openAIChatCompletionChunkObject:
		return true
	default:
		return false
	}
}

func (n *openAIChatCompletionChunkNormalizer) normalize(eventType, data string) (string, bool) {
	// OpenAI chat completion streams usually use unnamed SSE events. Also accept
	// explicit event names that are equivalent to the default SSE "message" event
	// or that directly label a chat chunk. Leave other side-band events
	// (event: ping, event: metadata, event: error, etc.) untouched so provider
	// metadata is not converted into a synthetic chat chunk.
	if n == nil || !openAIChatStreamEventMayCarryChunk(eventType) {
		return data, false
	}
	if strings.TrimSpace(data) == "" || strings.TrimSpace(data) == "[DONE]" {
		return data, false
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(data), &payload); err != nil || payload == nil {
		return data, false
	}
	if hasNonNullJSONField(payload, "error") {
		return data, false
	}

	changed := false
	if !hasNonEmptyStringJSONField(payload, "object") {
		payload["object"] = mustMarshalRaw(openAIChatCompletionChunkObject)
		changed = true
	}
	if hasNonEmptyStringJSONField(payload, "id") {
		upstreamID := jsonRawString(payload["id"])
		if n.streamID == "" {
			n.streamID = upstreamID
		} else if n.syntheticStreamID && upstreamID != n.streamID {
			payload["id"] = mustMarshalRaw(n.streamID)
			changed = true
		} else {
			n.streamID = upstreamID
		}
	} else {
		if n.streamID == "" {
			n.streamID = "chatcmpl-" + uuid.NewString()
			n.syntheticStreamID = true
		}
		payload["id"] = mustMarshalRaw(n.streamID)
		changed = true
	}
	if created, ok := jsonRawNumberAsInt64(payload["created"]); ok && created > 0 {
		n.created = created
	} else {
		if n.created == 0 {
			n.created = time.Now().Unix()
		}
		payload["created"] = mustMarshalRaw(n.created)
		changed = true
	}
	if hasNonEmptyStringJSONField(payload, "model") {
		n.model = jsonRawString(payload["model"])
	} else {
		model := strings.TrimSpace(n.model)
		if model == "" {
			model = n.requestedModel
		}
		if model != "" {
			payload["model"] = mustMarshalRaw(model)
			changed = true
		}
	}

	if !hasNonNullJSONField(payload, "choices") {
		payload["choices"] = json.RawMessage("[]")
		changed = true
	} else if choices, choicesChanged, err := normalizeOpenAIChatCompletionChunkChoices(payload["choices"]); err == nil && choicesChanged {
		payload["choices"] = choices
		changed = true
	}

	if !changed {
		return data, false
	}
	out, err := json.Marshal(payload)
	if err != nil {
		return data, false
	}
	return string(out), true
}

func normalizeOpenAIChatCompletionChunkChoices(raw json.RawMessage) (json.RawMessage, bool, error) {
	var choices []json.RawMessage
	if err := json.Unmarshal(raw, &choices); err != nil {
		return nil, false, err
	}

	changed := false
	for i := range choices {
		choiceRaw, choiceChanged, err := normalizeOpenAIChatCompletionChunkChoice(choices[i], i)
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

func normalizeOpenAIChatCompletionChunkChoice(raw json.RawMessage, arrayIndex int) (json.RawMessage, bool, error) {
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
	if !hasNonNullJSONField(choice, "delta") {
		choice["delta"] = defaultOpenAIChatCompletionChunkDelta(choice)
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

func defaultOpenAIChatCompletionChunkDelta(choice map[string]json.RawMessage) json.RawMessage {
	delta := make(map[string]json.RawMessage)
	if content, ok := firstProviderChoiceContent(choice); ok {
		delta["content"] = content
	}
	if role, ok := firstProviderChoiceStringField(choice, "role"); ok {
		delta["role"] = role
	}
	if toolCalls, ok := choice["tool_calls"]; ok && rawToolCallsNonEmpty(toolCalls) {
		delta["tool_calls"] = toolCalls
	}
	if functionCall, ok := choice["function_call"]; ok && rawFunctionCallNonEmpty(functionCall) {
		delta["function_call"] = functionCall
	}
	if len(delta) == 0 {
		return json.RawMessage("{}")
	}
	out, err := json.Marshal(delta)
	if err != nil {
		return json.RawMessage("{}")
	}
	return out
}

func firstProviderChoiceStringField(choice map[string]json.RawMessage, key string) (json.RawMessage, bool) {
	raw, ok := choice[key]
	if !ok || len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return nil, false
	}
	if isJSONString(raw) {
		return raw, true
	}
	return nil, false
}

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
	if hasNonNullJSONField(payload, "error") {
		return body, false, nil
	}

	changed := false
	if !hasNonEmptyStringJSONField(payload, "object") {
		payload["object"] = mustMarshalRaw(openAIChatCompletionObject)
		changed = true
	}
	created, createdOK := jsonRawNumberAsInt64(payload["created"])
	if !hasNonNullJSONField(payload, "created") || createdOK && created == 0 {
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

// inspectCanonicalOpenAIChatCompletionResponse recognizes the common case where
// an upstream already returned a complete Chat Completions response. It uses an
// exact-key token walk so it preserves the map-based normalizer's casing and
// duplicate-key semantics while avoiding nested map allocations. Any ambiguous
// or provider-specific shape falls back to the full normalizer.
func inspectCanonicalOpenAIChatCompletionResponse(body []byte, requestedModel string) (models.OpenAIUsage, bool) {
	if usage, ok := inspectCanonicalOpenAIChatCompletionResponseFast(body, requestedModel); ok {
		return usage, true
	}
	usage, ok := inspectCanonicalOpenAIChatCompletionResponseWithDecoder(body, requestedModel)
	if !ok || usage == nil {
		return models.OpenAIUsage{}, false
	}
	return *usage, true
}

func inspectCanonicalOpenAIChatCompletionResponseWithDecoder(body []byte, requestedModel string) (*models.OpenAIUsage, bool) {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()

	root, err := decoder.Token()
	if err != nil {
		return nil, false
	}
	if delim, ok := root.(json.Delim); !ok || delim != '{' {
		return nil, false
	}

	idOK := false
	objectOK := false
	createdOK := false
	modelOK := strings.TrimSpace(requestedModel) == ""
	choicesOK := false
	usageOK := false
	var usage *models.OpenAIUsage

	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, false
		}

		switch key {
		case "error":
			// Successful 2xx error envelopes deliberately bypass normalization.
			// They are uncommon and retain the existing map-based path.
			return nil, false
		case "id":
			idOK = decodeCanonicalNonEmptyString(decoder)
			if !idOK {
				return nil, false
			}
		case "object":
			objectOK = decodeCanonicalNonEmptyString(decoder)
			if !objectOK {
				return nil, false
			}
		case "created":
			createdOK = decodeCanonicalCreated(decoder)
			if !createdOK {
				return nil, false
			}
		case "model":
			modelOK = decodeCanonicalNonEmptyString(decoder)
			if !modelOK {
				return nil, false
			}
		case "choices":
			choicesOK = inspectCanonicalOpenAIChatChoices(decoder)
			if !choicesOK {
				return nil, false
			}
		case "usage":
			usage, usageOK = inspectCanonicalOpenAIUsage(decoder)
			if !usageOK {
				return nil, false
			}
		default:
			if strings.EqualFold(key, "usage") {
				return nil, false
			}
			if err := skipJSONValue(decoder); err != nil {
				return nil, false
			}
		}
	}

	end, err := decoder.Token()
	if err != nil {
		return nil, false
	}
	if delim, ok := end.(json.Delim); !ok || delim != '}' {
		return nil, false
	}
	if _, err := decoder.Token(); err != io.EOF {
		return nil, false
	}
	if !idOK || !objectOK || !createdOK || !modelOK || !choicesOK || !usageOK {
		return nil, false
	}
	return usage, true
}

func decodeCanonicalNonEmptyString(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	value, ok := token.(string)
	return ok && strings.TrimSpace(value) != ""
}

func decodeCanonicalNonNullScalar(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil || token == nil {
		return false
	}
	_, delimited := token.(json.Delim)
	return !delimited
}

func decodeCanonicalCreated(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil || token == nil {
		return false
	}
	if _, delimited := token.(json.Delim); delimited {
		return false
	}
	number, ok := token.(json.Number)
	if !ok {
		return true
	}
	value, err := number.Int64()
	return err != nil || value != 0
}

func inspectCanonicalOpenAIUsage(decoder *json.Decoder) (*models.OpenAIUsage, bool) {
	token, err := decoder.Token()
	if err != nil {
		return nil, false
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return nil, false
	}

	usage := &models.OpenAIUsage{}
	promptOK := false
	completionOK := false
	totalOK := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, false
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, false
		}

		switch key {
		case "prompt_tokens":
			usage.PromptTokens, promptOK = decodeCanonicalInt(decoder)
			if !promptOK {
				return nil, false
			}
		case "completion_tokens":
			usage.CompletionTokens, completionOK = decodeCanonicalInt(decoder)
			if !completionOK {
				return nil, false
			}
		case "total_tokens":
			usage.TotalTokens, totalOK = decodeCanonicalInt(decoder)
			if !totalOK {
				return nil, false
			}
		case "prompt_tokens_details":
			var details *models.OpenAIPromptTokensDetails
			if err := decoder.Decode(&details); err != nil {
				return nil, false
			}
			usage.PromptTokensDetails = details
		case "completion_tokens_details":
			var details *models.OpenAICompletionTokensDetails
			if err := decoder.Decode(&details); err != nil {
				return nil, false
			}
			usage.CompletionTokensDetails = details
		default:
			if strings.EqualFold(key, "prompt_tokens") ||
				strings.EqualFold(key, "completion_tokens") ||
				strings.EqualFold(key, "total_tokens") ||
				strings.EqualFold(key, "prompt_tokens_details") ||
				strings.EqualFold(key, "completion_tokens_details") {
				return nil, false
			}
			if err := skipJSONValue(decoder); err != nil {
				return nil, false
			}
		}
	}

	end, err := decoder.Token()
	if err != nil {
		return nil, false
	}
	if delim, ok := end.(json.Delim); !ok || delim != '}' {
		return nil, false
	}
	canonicalTotal := usage.TotalTokens != 0 || usage.PromptTokens == 0 && usage.CompletionTokens == 0
	return usage, promptOK && completionOK && totalOK && canonicalTotal
}

func decodeCanonicalInt(decoder *json.Decoder) (int, bool) {
	token, err := decoder.Token()
	if err != nil {
		return 0, false
	}
	number, ok := token.(json.Number)
	if !ok {
		return 0, false
	}
	value, err := strconv.Atoi(number.String())
	if err != nil {
		return 0, false
	}
	return value, true
}

func inspectCanonicalOpenAIChatChoices(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	if delim, ok := token.(json.Delim); !ok || delim != '[' {
		return false
	}

	for decoder.More() {
		choiceToken, err := decoder.Token()
		if err != nil {
			return false
		}
		if delim, ok := choiceToken.(json.Delim); !ok || delim != '{' {
			return false
		}

		indexOK := false
		finishReasonOK := false
		messageOK := false
		providerToolShape := false
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return false
			}
			key, ok := keyToken.(string)
			if !ok {
				return false
			}

			switch key {
			case "index":
				indexOK = decodeCanonicalNonNullScalar(decoder)
				if !indexOK {
					return false
				}
			case "finish_reason":
				finishReasonOK = decodeCanonicalNonEmptyString(decoder)
				if !finishReasonOK {
					return false
				}
			case "message":
				messageOK = inspectCanonicalOpenAIChatMessage(decoder)
				if !messageOK {
					return false
				}
			case "tool_calls", "function_call":
				providerToolShape = true
				if err := skipJSONValue(decoder); err != nil {
					return false
				}
			default:
				if err := skipJSONValue(decoder); err != nil {
					return false
				}
			}
		}

		end, err := decoder.Token()
		if err != nil {
			return false
		}
		if delim, ok := end.(json.Delim); !ok || delim != '}' {
			return false
		}
		if !indexOK || !finishReasonOK || !messageOK || providerToolShape {
			return false
		}
	}

	end, err := decoder.Token()
	if err != nil {
		return false
	}
	if delim, ok := end.(json.Delim); !ok || delim != ']' {
		return false
	}
	return true
}

func inspectCanonicalOpenAIChatMessage(decoder *json.Decoder) bool {
	token, err := decoder.Token()
	if err != nil {
		return false
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return false
	}

	roleOK := false
	contentOK := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return false
		}
		key, ok := keyToken.(string)
		if !ok {
			return false
		}
		switch key {
		case "role":
			roleOK = decodeCanonicalNonEmptyString(decoder)
			if !roleOK {
				return false
			}
		case "content":
			contentOK = decodeCanonicalNonNullScalar(decoder)
			if !contentOK {
				return false
			}
		default:
			if err := skipJSONValue(decoder); err != nil {
				return false
			}
		}
	}

	end, err := decoder.Token()
	if err != nil {
		return false
	}
	if delim, ok := end.(json.Delim); !ok || delim != '}' {
		return false
	}
	return roleOK && contentOK
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
	if !hasNonEmptyStringJSONField(choice, "finish_reason") {
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
	if !hasNonNullJSONField(message, "tool_calls") {
		if toolCalls, ok := choice["tool_calls"]; ok && rawToolCallsNonEmpty(toolCalls) {
			message["tool_calls"] = toolCalls
			changed = true
		}
	}
	if !hasNonNullJSONField(message, "function_call") {
		if functionCall, ok := choice["function_call"]; ok && rawFunctionCallNonEmpty(functionCall) {
			message["function_call"] = functionCall
			changed = true
		}
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

func choiceMessageHasToolCall(choice map[string]json.RawMessage) bool {
	raw, ok := choice["message"]
	if !ok || len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return false
	}
	var message map[string]json.RawMessage
	if err := json.Unmarshal(raw, &message); err != nil || message == nil {
		return false
	}
	return rawToolCallsNonEmpty(message["tool_calls"]) || rawFunctionCallNonEmpty(message["function_call"])
}

func choiceHasToolCall(choice map[string]json.RawMessage) bool {
	return rawToolCallsNonEmpty(choice["tool_calls"]) || rawFunctionCallNonEmpty(choice["function_call"]) || choiceMessageHasToolCall(choice)
}

func rawToolCallsNonEmpty(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return false
	}
	var calls []json.RawMessage
	return json.Unmarshal(raw, &calls) == nil && len(calls) > 0
}

func rawFunctionCallNonEmpty(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
		return false
	}
	var call map[string]json.RawMessage
	return json.Unmarshal(raw, &call) == nil && len(call) > 0
}

func defaultOpenAIChatChoiceFinishReason(choice map[string]json.RawMessage) string {
	if choiceHasToolCall(choice) {
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
	total, hasTotal := jsonRawNumberAsInt64(usage["total_tokens"])
	if !hasTotal || total == 0 && (prompt != 0 || completion != 0) {
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

func jsonRawString(raw json.RawMessage) string {
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return ""
	}
	return strings.TrimSpace(value)
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
