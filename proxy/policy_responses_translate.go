package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/sozercan/vekil/models"
)

const (
	policyResponsesToolKindFunction = "function"

	policyResponsesMaxRequestBytes = 1 << 20
	policyResponsesMaxJSONDepth    = 128
	policyResponsesMaxInputItems   = 1024
	policyResponsesMaxContentParts = 1024
	// Responses-backed Chat bridges used by managed Codex launches can accept
	// Responses-scale catalogs beyond the 128-function native OpenAI/Azure Chat
	// limit. Operators using narrower native-Chat destinations must constrain the
	// client catalog to that destination's contract.
	policyResponsesMaxFunctionTools   = 1024
	policyResponsesMaxIncludeItems    = 8
	policyResponsesMaxMetadataEntries = 64
	policyResponsesMaxMetadataKeyLen  = 256
	policyResponsesMaxMetadataValue   = 4096
	policyResponsesMaxToolNameLen     = 512
	policyResponsesMaxDescriptionLen  = 64 << 10
	policyResponsesMaxChatToolNameLen = 64
	policyResponsesMaxSchemaNameLen   = 64
	policyResponsesAliasPrefix        = "vkl1__"
	policyResponsesAliasStemLen       = 13
)

type policyResponsesToolDescriptor struct {
	Name      string
	Namespace string
	Kind      string
}

type policyResponsesToolMap map[string]policyResponsesToolDescriptor

type policyResponsesChatRequest struct {
	Body          []byte
	Stream        bool
	PublicModel   string
	Tools         policyResponsesToolMap
	CallableTools policyResponsesToolMap
	Response      policyResponsesResponseConfig
}

type policyResponsesResponseConfig struct {
	Text              json.RawMessage
	Tools             json.RawMessage
	ToolChoice        json.RawMessage
	ParallelToolCalls bool
	RequiresToolCall  bool
}

type policyResponsesParsedTool struct {
	descriptor  policyResponsesToolDescriptor
	description string
	parameters  json.RawMessage
	strict      *bool
	param       string
}

type policyResponsesInputItem struct {
	kind       string
	role       string
	text       string
	callID     string
	arguments  string
	descriptor policyResponsesToolDescriptor
	param      string
}

type policyResponsesParsedToolChoice struct {
	mode       string
	descriptor *policyResponsesToolDescriptor
}

type policyResponsesCanonicalChatRequest struct {
	Model               string                 `json:"model"`
	Messages            []models.OpenAIMessage `json:"messages"`
	MaxCompletionTokens *int                   `json:"max_completion_tokens,omitempty"`
	Temperature         *float64               `json:"temperature,omitempty"`
	TopP                *float64               `json:"top_p,omitempty"`
	Stream              bool                   `json:"stream"`
	Tools               []models.OpenAITool    `json:"tools,omitempty"`
	ToolChoice          json.RawMessage        `json:"tool_choice,omitempty"`
	ParallelToolCalls   *bool                  `json:"parallel_tool_calls,omitempty"`
	ResponseFormat      json.RawMessage        `json:"response_format,omitempty"`
	ReasoningEffort     string                 `json:"reasoning_effort,omitempty"`
}

func translatePolicyResponsesRequestToChat(body []byte) (policyResponsesChatRequest, error) {
	if len(body) > policyResponsesMaxRequestBytes {
		return policyResponsesChatRequest{}, newChatInvalidRequest("", fmt.Sprintf("policy Responses request exceeds %d bytes", policyResponsesMaxRequestBytes))
	}
	if err := validatePolicyResponsesJSON(body); err != nil {
		return policyResponsesChatRequest{}, err
	}
	root, err := decodeChatJSONObject(body, "")
	if err != nil {
		return policyResponsesChatRequest{}, err
	}
	if err := validatePolicyResponsesTopLevel(root); err != nil {
		return policyResponsesChatRequest{}, err
	}

	model, err := requiredPolicyResponsesString(root, "model", policyResponsesMaxToolNameLen)
	if err != nil {
		return policyResponsesChatRequest{}, err
	}
	if err := validatePolicyResponsesStore(root); err != nil {
		return policyResponsesChatRequest{}, err
	}
	if _, ok := root["previous_response_id"]; ok {
		return policyResponsesChatRequest{}, newChatInvalidRequest("previous_response_id", "previous_response_id is not supported for policy models")
	}
	if err := validatePolicyResponsesBenignMetadata(root); err != nil {
		return policyResponsesChatRequest{}, err
	}

	instructions, err := optionalPolicyResponsesText(root, "instructions", policyResponsesMaxRequestBytes)
	if err != nil {
		return policyResponsesChatRequest{}, err
	}
	input, err := parsePolicyResponsesInput(root["input"])
	if err != nil {
		return policyResponsesChatRequest{}, err
	}
	parsedTools, err := parsePolicyResponsesTools(root["tools"])
	if err != nil {
		return policyResponsesChatRequest{}, err
	}
	toolChoice, err := parsePolicyResponsesToolChoice(root["tool_choice"])
	if err != nil {
		return policyResponsesChatRequest{}, err
	}
	responseFormat, responseText, err := translatePolicyResponsesTextConfiguration(root["text"])
	if err != nil {
		return policyResponsesChatRequest{}, err
	}

	declaredTools := make(map[policyResponsesToolDescriptor]string, len(parsedTools))
	descriptors := make(map[policyResponsesToolDescriptor]struct{}, len(parsedTools)+len(input))
	for _, tool := range parsedTools {
		if prior, duplicate := declaredTools[tool.descriptor]; duplicate {
			return policyResponsesChatRequest{}, newChatInvalidRequest(tool.param+".name", fmt.Sprintf("duplicate function tool (already declared at %s)", prior))
		}
		declaredTools[tool.descriptor] = tool.param
		descriptors[tool.descriptor] = struct{}{}
	}
	for _, item := range input {
		if item.kind == "function_call" {
			descriptors[item.descriptor] = struct{}{}
		}
	}
	if toolChoice.descriptor != nil {
		if _, ok := declaredTools[*toolChoice.descriptor]; !ok {
			return policyResponsesChatRequest{}, newChatInvalidRequest("tool_choice", "tool_choice must name a declared function tool")
		}
		descriptors[*toolChoice.descriptor] = struct{}{}
	}
	if toolChoice.mode == "required" && len(parsedTools) == 0 {
		return policyResponsesChatRequest{}, newChatInvalidRequest("tool_choice", "tool_choice requires non-empty tools")
	}

	aliases, reverseTools, err := buildPolicyResponsesToolAliases(descriptors)
	if err != nil {
		return policyResponsesChatRequest{}, err
	}
	messages, err := translatePolicyResponsesInputMessages(instructions, input, aliases)
	if err != nil {
		return policyResponsesChatRequest{}, err
	}
	if len(messages) == 0 {
		return policyResponsesChatRequest{}, newChatInvalidRequest("input", "input must produce at least one Chat message")
	}
	chatTools := translatePolicyResponsesTools(parsedTools, aliases)
	chatToolChoice, err := translatePolicyResponsesToolChoice(toolChoice, aliases)
	if err != nil {
		return policyResponsesChatRequest{}, err
	}
	callableTools := policyResponsesCallableTools(parsedTools, aliases, toolChoice)

	stream, err := policyResponsesBool(root, "stream", false)
	if err != nil {
		return policyResponsesChatRequest{}, err
	}
	maxTokens, err := policyResponsesPositiveInt(root, "max_output_tokens")
	if err != nil {
		return policyResponsesChatRequest{}, err
	}
	if maxTokens != nil && *maxTokens < responsesChatMinimumOutputTokens {
		return policyResponsesChatRequest{}, newChatInvalidRequest(
			"max_output_tokens",
			fmt.Sprintf("max_output_tokens must be at least %d for policy models", responsesChatMinimumOutputTokens),
		)
	}
	temperature, err := policyResponsesFloat(root, "temperature", 0, 2)
	if err != nil {
		return policyResponsesChatRequest{}, err
	}
	topP, err := policyResponsesFloat(root, "top_p", 0, 1)
	if err != nil {
		return policyResponsesChatRequest{}, err
	}
	parallel, err := policyResponsesOptionalBool(root, "parallel_tool_calls")
	if err != nil {
		return policyResponsesChatRequest{}, err
	}
	responseParallel := true
	if parallel != nil {
		responseParallel = *parallel
	}
	responseTools := json.RawMessage(`[]`)
	if toolsRaw := bytes.TrimSpace(root["tools"]); len(toolsRaw) > 0 && !bytes.Equal(toolsRaw, []byte("null")) {
		responseTools = cloneReplayRawMessage(toolsRaw)
	}
	responseToolChoice := json.RawMessage(`"auto"`)
	if choiceRaw := bytes.TrimSpace(root["tool_choice"]); len(choiceRaw) > 0 && !bytes.Equal(choiceRaw, []byte("null")) {
		responseToolChoice = cloneReplayRawMessage(choiceRaw)
	}
	reasoningEffort, err := parsePolicyResponsesReasoning(root["reasoning"])
	if err != nil {
		return policyResponsesChatRequest{}, err
	}

	canonical := policyResponsesCanonicalChatRequest{
		Model:               model,
		Messages:            messages,
		MaxCompletionTokens: maxTokens,
		Temperature:         temperature,
		TopP:                topP,
		Stream:              stream,
		Tools:               chatTools,
		ToolChoice:          chatToolChoice,
		ParallelToolCalls:   parallel,
		ResponseFormat:      responseFormat,
		ReasoningEffort:     reasoningEffort,
	}
	chatBody, err := json.Marshal(canonical)
	if err != nil {
		return policyResponsesChatRequest{}, fmt.Errorf("marshal policy Chat request: %w", err)
	}
	if len(chatBody) > policyResponsesMaxRequestBytes {
		return policyResponsesChatRequest{}, newChatInvalidRequest("", fmt.Sprintf("translated policy Chat request exceeds %d bytes", policyResponsesMaxRequestBytes))
	}
	return policyResponsesChatRequest{
		Body:          chatBody,
		Stream:        stream,
		PublicModel:   model,
		Tools:         reverseTools,
		CallableTools: callableTools,
		Response: policyResponsesResponseConfig{
			Text: responseText, Tools: responseTools, ToolChoice: responseToolChoice, ParallelToolCalls: responseParallel,
			RequiresToolCall: toolChoice.mode == "required" || toolChoice.descriptor != nil,
		},
	}, nil
}

func policyResponsesCallableTools(parsed []policyResponsesParsedTool, aliases map[policyResponsesToolDescriptor]string, choice policyResponsesParsedToolChoice) policyResponsesToolMap {
	if choice.mode == "none" || len(parsed) == 0 {
		return nil
	}
	callable := make(policyResponsesToolMap)
	if choice.descriptor != nil {
		if alias, ok := aliases[*choice.descriptor]; ok {
			callable[alias] = *choice.descriptor
		}
		return callable
	}
	for _, tool := range parsed {
		if alias, ok := aliases[tool.descriptor]; ok {
			callable[alias] = tool.descriptor
		}
	}
	if len(callable) == 0 {
		return nil
	}
	return callable
}

func translatePolicyResponsesTextConfiguration(raw json.RawMessage) (json.RawMessage, json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil, nil
	}
	text, err := decodeChatJSONObject(raw, "text")
	if err != nil {
		return nil, nil, newChatInvalidRequest("text", "text must be an object")
	}
	if err := validatePolicyResponsesObjectFields(text, "text", "format"); err != nil {
		return nil, nil, err
	}
	formatRaw, ok := text["format"]
	if !ok || bytes.Equal(bytes.TrimSpace(formatRaw), []byte("null")) {
		return nil, cloneReplayRawMessage(raw), nil
	}
	format, err := decodeChatJSONObject(formatRaw, "text.format")
	if err != nil {
		return nil, nil, newChatInvalidRequest("text.format", "format must be an object")
	}
	formatType, err := requiredPolicyResponsesStringAt(format, "type", "text.format.type", 128)
	if err != nil {
		return nil, nil, err
	}
	switch formatType {
	case "text", "json_object":
		if err := validatePolicyResponsesObjectFields(format, "text.format", "type"); err != nil {
			return nil, nil, err
		}
		chatFormat, _ := json.Marshal(map[string]string{"type": formatType})
		return chatFormat, cloneReplayRawMessage(raw), nil
	case "json_schema":
		if err := validatePolicyResponsesObjectFields(format, "text.format", "type", "name", "description", "schema", "strict"); err != nil {
			return nil, nil, err
		}
		name, err := requiredPolicyResponsesStringAt(format, "name", "text.format.name", policyResponsesMaxSchemaNameLen)
		if err != nil {
			return nil, nil, err
		}
		for _, r := range name {
			if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
				return nil, nil, newChatInvalidRequest("text.format.name", "name may contain only ASCII letters, digits, underscore, or hyphen")
			}
		}
		description, err := optionalPolicyResponsesTextAt(format, "description", "text.format.description", policyResponsesMaxDescriptionLen)
		if err != nil {
			return nil, nil, err
		}
		schemaRaw, ok := format["schema"]
		if !ok {
			return nil, nil, newChatInvalidRequest("text.format.schema", "schema is required")
		}
		if _, err := decodeChatJSONObject(schemaRaw, "text.format.schema"); err != nil {
			return nil, nil, newChatInvalidRequest("text.format.schema", "schema must be an object")
		}
		strict, err := policyResponsesOptionalBool(format, "strict")
		if err != nil {
			return nil, nil, prefixPolicyResponsesError(err, "text.format")
		}
		jsonSchema := map[string]any{"name": name, "schema": cloneReplayRawMessage(schemaRaw)}
		if description != "" {
			jsonSchema["description"] = description
		}
		if strict != nil {
			jsonSchema["strict"] = *strict
		}
		chatFormat, _ := json.Marshal(map[string]any{"type": "json_schema", "json_schema": jsonSchema})
		return chatFormat, cloneReplayRawMessage(raw), nil
	default:
		return nil, nil, newChatInvalidRequest("text.format.type", "unsupported text format type")
	}
}

func validatePolicyResponsesTopLevel(root map[string]json.RawMessage) error {
	allowed := map[string]struct{}{
		"model": {}, "instructions": {}, "input": {}, "text": {}, "tools": {}, "tool_choice": {},
		"parallel_tool_calls": {}, "reasoning": {}, "store": {}, "stream": {},
		"max_output_tokens": {}, "temperature": {}, "top_p": {}, "previous_response_id": {},
		"include": {}, "prompt_cache_key": {}, "client_metadata": {}, "metadata": {},
		"safety_identifier": {}, "user": {},
	}
	unknown := make([]string, 0)
	for field := range root {
		if _, ok := allowed[field]; !ok {
			unknown = append(unknown, field)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return newChatInvalidRequest(unknown[0], "unsupported JSON field")
}

func validatePolicyResponsesStore(root map[string]json.RawMessage) error {
	raw, ok := root["store"]
	if !ok {
		return nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return newChatInvalidRequest("store", "store must be false or omitted for policy models")
	}
	var store bool
	if err := json.Unmarshal(raw, &store); err != nil {
		return newChatInvalidRequest("store", "store must be false or omitted for policy models")
	}
	if store {
		return newChatInvalidRequest("store", "store must be false or omitted for policy models")
	}
	return nil
}

func validatePolicyResponsesBenignMetadata(root map[string]json.RawMessage) error {
	if raw, ok := root["include"]; ok {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return newChatInvalidRequest("include", "include must be an array")
		}
		var include []json.RawMessage
		if err := json.Unmarshal(raw, &include); err != nil {
			return newChatInvalidRequest("include", "include must be an array")
		}
		if len(include) > policyResponsesMaxIncludeItems {
			return newChatInvalidRequest("include", fmt.Sprintf("include may contain at most %d items", policyResponsesMaxIncludeItems))
		}
		seenEncrypted := false
		for i, itemRaw := range include {
			var item string
			if err := json.Unmarshal(itemRaw, &item); err != nil {
				return newChatInvalidRequest(fmt.Sprintf("include[%d]", i), "include values must be strings")
			}
			if item != "reasoning.encrypted_content" || seenEncrypted {
				return newChatInvalidRequest(fmt.Sprintf("include[%d]", i), "only one reasoning.encrypted_content include value is supported")
			}
			seenEncrypted = true
		}
	}
	for _, field := range []string{"prompt_cache_key", "safety_identifier", "user"} {
		if _, err := optionalPolicyResponsesString(root, field, policyResponsesMaxMetadataValue); err != nil {
			return err
		}
	}
	for _, field := range []string{"client_metadata", "metadata"} {
		if err := validatePolicyResponsesMetadataObject(root[field], field); err != nil {
			return err
		}
	}
	return nil
}

func validatePolicyResponsesMetadataObject(raw json.RawMessage, param string) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	object, err := decodeChatJSONObject(raw, param)
	if err != nil {
		return newChatInvalidRequest(param, param+" must be an object")
	}
	if len(object) > policyResponsesMaxMetadataEntries {
		return newChatInvalidRequest(param, fmt.Sprintf("%s may contain at most %d entries", param, policyResponsesMaxMetadataEntries))
	}
	for key, valueRaw := range object {
		if len(key) == 0 || len(key) > policyResponsesMaxMetadataKeyLen {
			return newChatInvalidRequest(param+"."+key, "metadata key is empty or too long")
		}
		var value string
		if bytes.Equal(bytes.TrimSpace(valueRaw), []byte("null")) {
			return newChatInvalidRequest(param+"."+key, "metadata values must be bounded strings")
		}
		if err := json.Unmarshal(valueRaw, &value); err != nil || len(value) > policyResponsesMaxMetadataValue {
			return newChatInvalidRequest(param+"."+key, "metadata values must be bounded strings")
		}
	}
	return nil
}

func parsePolicyResponsesInput(raw json.RawMessage) ([]policyResponsesInputItem, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, newChatInvalidRequest("input", "input is required and must be a string or array")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return []policyResponsesInputItem{{kind: "message", role: "user", text: text, param: "input"}}, nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, newChatInvalidRequest("input", "input must be a string or array")
	}
	if len(values) > policyResponsesMaxInputItems {
		return nil, newChatInvalidRequest("input", fmt.Sprintf("input may contain at most %d items", policyResponsesMaxInputItems))
	}
	items := make([]policyResponsesInputItem, 0, len(values))
	for index, itemRaw := range values {
		item, err := parsePolicyResponsesInputItem(itemRaw, index)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := validatePolicyResponsesCallSequence(items); err != nil {
		return nil, err
	}
	return items, nil
}

func parsePolicyResponsesInputItem(raw json.RawMessage, index int) (policyResponsesInputItem, error) {
	param := fmt.Sprintf("input[%d]", index)
	object, err := decodeChatJSONObject(raw, param)
	if err != nil {
		return policyResponsesInputItem{}, newChatInvalidRequest(param, "input item must be an object")
	}
	itemType, err := requiredPolicyResponsesString(object, "type", 128)
	if err != nil {
		return policyResponsesInputItem{}, prefixPolicyResponsesError(err, param)
	}
	switch itemType {
	case "message":
		if err := validatePolicyResponsesObjectFields(object, param, "type", "role", "content", "id", "status", "phase"); err != nil {
			return policyResponsesInputItem{}, err
		}
		role, err := requiredPolicyResponsesStringAt(object, "role", param+".role", 32)
		if err != nil {
			return policyResponsesInputItem{}, err
		}
		switch role {
		case "system", "developer", "user", "assistant":
		default:
			return policyResponsesInputItem{}, newChatInvalidRequest(param+".role", "unsupported message role")
		}
		contentRaw, ok := object["content"]
		if !ok {
			return policyResponsesInputItem{}, newChatInvalidRequest(param+".content", "message content is required")
		}
		text, err := parsePolicyResponsesTextContent(contentRaw, role, param+".content", false)
		if err != nil {
			return policyResponsesInputItem{}, err
		}
		if err := validatePolicyResponsesOptionalItemMetadata(object, param); err != nil {
			return policyResponsesInputItem{}, err
		}
		return policyResponsesInputItem{kind: itemType, role: role, text: text, param: param}, nil
	case "function_call":
		if err := validatePolicyResponsesObjectFields(object, param, "type", "id", "call_id", "name", "namespace", "arguments", "status"); err != nil {
			return policyResponsesInputItem{}, err
		}
		callID, err := requiredPolicyResponsesStringAt(object, "call_id", param+".call_id", policyResponsesMaxToolNameLen)
		if err != nil {
			return policyResponsesInputItem{}, err
		}
		name, err := requiredPolicyResponsesStringAt(object, "name", param+".name", policyResponsesMaxToolNameLen)
		if err != nil {
			return policyResponsesInputItem{}, err
		}
		namespace, err := optionalPolicyResponsesNamespace(object, "namespace", param+".namespace")
		if err != nil {
			return policyResponsesInputItem{}, err
		}
		argumentsRaw, ok := object["arguments"]
		if !ok {
			return policyResponsesInputItem{}, newChatInvalidRequest(param+".arguments", "function call arguments are required")
		}
		var arguments string
		if bytes.Equal(bytes.TrimSpace(argumentsRaw), []byte("null")) {
			return policyResponsesInputItem{}, newChatInvalidRequest(param+".arguments", "function call arguments must be a string")
		}
		if err := json.Unmarshal(argumentsRaw, &arguments); err != nil {
			return policyResponsesInputItem{}, newChatInvalidRequest(param+".arguments", "function call arguments must be a string")
		}
		if err := validatePolicyResponsesOptionalItemMetadata(object, param); err != nil {
			return policyResponsesInputItem{}, err
		}
		return policyResponsesInputItem{
			kind: itemType, callID: callID, arguments: arguments, param: param,
			descriptor: policyResponsesToolDescriptor{Name: name, Namespace: namespace, Kind: policyResponsesToolKindFunction},
		}, nil
	case "function_call_output":
		if err := validatePolicyResponsesObjectFields(object, param, "type", "id", "call_id", "output", "status"); err != nil {
			return policyResponsesInputItem{}, err
		}
		callID, err := requiredPolicyResponsesStringAt(object, "call_id", param+".call_id", policyResponsesMaxToolNameLen)
		if err != nil {
			return policyResponsesInputItem{}, err
		}
		outputRaw, ok := object["output"]
		if !ok {
			return policyResponsesInputItem{}, newChatInvalidRequest(param+".output", "function call output is required")
		}
		output, err := parsePolicyResponsesTextContent(outputRaw, "tool", param+".output", true)
		if err != nil {
			return policyResponsesInputItem{}, err
		}
		if err := validatePolicyResponsesOptionalItemMetadata(object, param); err != nil {
			return policyResponsesInputItem{}, err
		}
		return policyResponsesInputItem{kind: itemType, callID: callID, text: output, param: param}, nil
	case "input_image", "image_generation_call":
		return policyResponsesInputItem{}, newChatInvalidRequest(param+".type", "image input and output items are not supported for policy models")
	default:
		return policyResponsesInputItem{}, newChatInvalidRequest(param+".type", fmt.Sprintf("Responses input item type %q is not supported for policy models", itemType))
	}
}

func validatePolicyResponsesOptionalItemMetadata(object map[string]json.RawMessage, param string) error {
	for _, field := range []string{"id", "status", "phase"} {
		raw, ok := object[field]
		if !ok {
			continue
		}
		var value string
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return newChatInvalidRequest(param+"."+field, field+" must be a bounded string")
		}
		if err := json.Unmarshal(raw, &value); err != nil || len(value) > policyResponsesMaxMetadataValue {
			return newChatInvalidRequest(param+"."+field, field+" must be a bounded string")
		}
	}
	return nil
}

func validatePolicyResponsesCallSequence(items []policyResponsesInputItem) error {
	calls := make(map[string]string)
	outputs := make(map[string]string)
	pending := make(map[string]string)
	pendingOrder := make([]string, 0)
	sawOutput := false

	missingOutputError := func() error {
		for _, callID := range pendingOrder {
			if callParam, ok := pending[callID]; ok {
				return newChatInvalidRequest(callParam+".call_id", "function call is missing a following function_call_output")
			}
		}
		return nil
	}

	for _, item := range items {
		switch item.kind {
		case "function_call":
			if len(pending) > 0 && sawOutput {
				return newChatInvalidRequest(item.param, "parallel function_call items must be consecutive before their outputs")
			}
			if len(pending) == 0 {
				pendingOrder = pendingOrder[:0]
				sawOutput = false
			}
			if prior, duplicate := calls[item.callID]; duplicate {
				return newChatInvalidRequest(item.param+".call_id", fmt.Sprintf("duplicate function call ID (already declared at %s)", prior))
			}
			calls[item.callID] = item.param
			pending[item.callID] = item.param
			pendingOrder = append(pendingOrder, item.callID)
		case "function_call_output":
			if _, ok := pending[item.callID]; !ok {
				if prior, duplicate := outputs[item.callID]; duplicate {
					return newChatInvalidRequest(item.param+".call_id", fmt.Sprintf("duplicate function call output (already declared at %s)", prior))
				}
				return newChatInvalidRequest(item.param+".call_id", "function call output references an unknown prior call or a call from an earlier completed group")
			}
			outputs[item.callID] = item.param
			delete(pending, item.callID)
			sawOutput = true
		case "message":
			if err := missingOutputError(); err != nil {
				return err
			}
			pendingOrder = pendingOrder[:0]
			sawOutput = false
		}
	}
	return missingOutputError()
}

func parsePolicyResponsesTextContent(raw json.RawMessage, role, param string, toolOutput bool) (string, error) {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", newChatInvalidRequest(param, "text content must be a string or array")
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text, nil
	}
	var parts []json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return "", newChatInvalidRequest(param, "text content must be a string or array")
	}
	if len(parts) > policyResponsesMaxContentParts {
		return "", newChatInvalidRequest(param, fmt.Sprintf("text content may contain at most %d parts", policyResponsesMaxContentParts))
	}
	var combined strings.Builder
	for index, partRaw := range parts {
		partParam := fmt.Sprintf("%s[%d]", param, index)
		part, err := decodeChatJSONObject(partRaw, partParam)
		if err != nil {
			return "", newChatInvalidRequest(partParam, "content part must be an object")
		}
		partType, err := requiredPolicyResponsesStringAt(part, "type", partParam+".type", 64)
		if err != nil {
			return "", err
		}
		if partType == "input_image" || partType == "image_url" {
			return "", newChatInvalidRequest(partParam+".type", "image content is not supported for policy models")
		}
		if partType == "refusal" && role == "assistant" && !toolOutput {
			if err := validatePolicyResponsesObjectFields(part, partParam, "type", "refusal"); err != nil {
				return "", err
			}
			refusal, err := requiredPolicyResponsesStringValue(part, "refusal", partParam+".refusal", policyResponsesMaxRequestBytes, true)
			if err != nil {
				return "", err
			}
			combined.WriteString(refusal)
			continue
		}
		validType := partType == "input_text" || (toolOutput && partType == "output_text") || (role == "assistant" && partType == "output_text")
		if !validType {
			return "", newChatInvalidRequest(partParam+".type", fmt.Sprintf("text content type %q is not valid for role %q", partType, role))
		}
		allowedFields := []string{"type", "text"}
		if partType == "output_text" {
			allowedFields = append(allowedFields, "annotations")
		}
		if err := validatePolicyResponsesObjectFields(part, partParam, allowedFields...); err != nil {
			return "", err
		}
		if annotationsRaw, ok := part["annotations"]; ok {
			trimmedAnnotations := bytes.TrimSpace(annotationsRaw)
			var annotations []json.RawMessage
			if len(trimmedAnnotations) < 2 || trimmedAnnotations[0] != '[' || json.Unmarshal(annotationsRaw, &annotations) != nil || len(annotations) != 0 {
				return "", newChatInvalidRequest(partParam+".annotations", "only an empty annotations array is supported")
			}
		}
		partText, err := requiredPolicyResponsesStringValue(part, "text", partParam+".text", policyResponsesMaxRequestBytes, true)
		if err != nil {
			return "", err
		}
		combined.WriteString(partText)
	}
	return combined.String(), nil
}

func parsePolicyResponsesTools(raw json.RawMessage) ([]policyResponsesParsedTool, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	var values []json.RawMessage
	if err := json.Unmarshal(raw, &values); err != nil {
		return nil, newChatInvalidRequest("tools", "tools must be an array")
	}
	parsed := make([]policyResponsesParsedTool, 0, len(values))
	for index, toolRaw := range values {
		if len(parsed) >= policyResponsesMaxFunctionTools {
			return nil, newChatInvalidRequest("tools", fmt.Sprintf("tools may contain at most %d flattened function tools", policyResponsesMaxFunctionTools))
		}
		param := fmt.Sprintf("tools[%d]", index)
		object, err := decodeChatJSONObject(toolRaw, param)
		if err != nil {
			return nil, newChatInvalidRequest(param, "tool must be an object")
		}
		toolType, err := requiredPolicyResponsesStringAt(object, "type", param+".type", 128)
		if err != nil {
			return nil, err
		}
		switch toolType {
		case "function":
			tool, err := parsePolicyResponsesFunctionTool(object, param, "")
			if err != nil {
				return nil, err
			}
			parsed = append(parsed, tool)
		case "namespace":
			if err := validatePolicyResponsesObjectFields(object, param, "type", "name", "description", "tools"); err != nil {
				return nil, err
			}
			namespace, err := requiredPolicyResponsesStringAt(object, "name", param+".name", policyResponsesMaxToolNameLen)
			if err != nil {
				return nil, err
			}
			namespaceDescription, err := optionalPolicyResponsesStringAt(object, "description", param+".description", policyResponsesMaxDescriptionLen)
			if err != nil {
				return nil, err
			}
			childrenRaw, ok := object["tools"]
			if !ok {
				return nil, newChatInvalidRequest(param+".tools", "namespace tools are required")
			}
			var children []json.RawMessage
			if err := json.Unmarshal(childrenRaw, &children); err != nil || len(children) == 0 {
				return nil, newChatInvalidRequest(param+".tools", "namespace tools must be a non-empty array")
			}
			for childIndex, childRaw := range children {
				if len(parsed) >= policyResponsesMaxFunctionTools {
					return nil, newChatInvalidRequest("tools", fmt.Sprintf("tools may contain at most %d flattened function tools", policyResponsesMaxFunctionTools))
				}
				childParam := fmt.Sprintf("%s.tools[%d]", param, childIndex)
				childObject, err := decodeChatJSONObject(childRaw, childParam)
				if err != nil {
					return nil, newChatInvalidRequest(childParam, "namespace child tool must be an object")
				}
				childType, err := requiredPolicyResponsesStringAt(childObject, "type", childParam+".type", 128)
				if err != nil {
					return nil, err
				}
				if childType != "function" {
					return nil, newChatInvalidRequest(childParam+".type", fmt.Sprintf("namespace child tool type %q is not supported; only function is supported", childType))
				}
				child, err := parsePolicyResponsesFunctionTool(childObject, childParam, namespace)
				if err != nil {
					return nil, err
				}
				child.description = combinePolicyResponsesToolDescriptions(namespaceDescription, child.description)
				parsed = append(parsed, child)
			}
		case "custom", "web_search", "web_search_preview", "image_generation", "file_search", "computer_use_preview", "tool_search", "mcp":
			return nil, newChatInvalidRequest(param+".type", fmt.Sprintf("hosted or custom tool type %q is not supported for policy models", toolType))
		default:
			return nil, newChatInvalidRequest(param+".type", fmt.Sprintf("tool type %q is not supported for policy models", toolType))
		}
	}
	return parsed, nil
}

func combinePolicyResponsesToolDescriptions(namespace, child string) string {
	if namespace == "" {
		return child
	}
	if child == "" {
		return namespace
	}
	return namespace + "\n\n" + child
}

func parsePolicyResponsesFunctionTool(object map[string]json.RawMessage, param, namespace string) (policyResponsesParsedTool, error) {
	if err := validatePolicyResponsesObjectFields(object, param, "type", "name", "description", "parameters", "strict", "defer_loading"); err != nil {
		return policyResponsesParsedTool{}, err
	}
	name, err := requiredPolicyResponsesStringAt(object, "name", param+".name", policyResponsesMaxToolNameLen)
	if err != nil {
		return policyResponsesParsedTool{}, err
	}
	description, err := optionalPolicyResponsesTextAt(object, "description", param+".description", policyResponsesMaxDescriptionLen)
	if err != nil {
		return policyResponsesParsedTool{}, err
	}
	parameters, ok := object["parameters"]
	if !ok {
		return policyResponsesParsedTool{}, newChatInvalidRequest(param+".parameters", "function parameters are required")
	}
	if _, err := decodeChatJSONObject(parameters, param+".parameters"); err != nil {
		return policyResponsesParsedTool{}, newChatInvalidRequest(param+".parameters", "function parameters must be a JSON object")
	}
	strict, err := policyResponsesOptionalBool(object, "strict")
	if err != nil {
		return policyResponsesParsedTool{}, prefixPolicyResponsesError(err, param)
	}
	deferLoading, err := policyResponsesOptionalBool(object, "defer_loading")
	if err != nil {
		return policyResponsesParsedTool{}, prefixPolicyResponsesError(err, param)
	}
	if deferLoading != nil && *deferLoading {
		return policyResponsesParsedTool{}, newChatInvalidRequest(param+".defer_loading", "deferred tool loading is not supported for policy models")
	}
	return policyResponsesParsedTool{
		descriptor:  policyResponsesToolDescriptor{Name: name, Namespace: namespace, Kind: policyResponsesToolKindFunction},
		description: description,
		parameters:  append(json.RawMessage(nil), parameters...),
		strict:      strict,
		param:       param,
	}, nil
}

func parsePolicyResponsesToolChoice(raw json.RawMessage) (policyResponsesParsedToolChoice, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return policyResponsesParsedToolChoice{}, nil
	}
	var mode string
	if err := json.Unmarshal(raw, &mode); err == nil {
		switch mode {
		case "none", "auto", "required":
			return policyResponsesParsedToolChoice{mode: mode}, nil
		default:
			return policyResponsesParsedToolChoice{}, newChatInvalidRequest("tool_choice", "tool_choice must be none, auto, required, or a function object")
		}
	}
	object, err := decodeChatJSONObject(raw, "tool_choice")
	if err != nil {
		return policyResponsesParsedToolChoice{}, newChatInvalidRequest("tool_choice", "tool_choice must be a string or object")
	}
	choiceType, err := requiredPolicyResponsesStringAt(object, "type", "tool_choice.type", 128)
	if err != nil {
		return policyResponsesParsedToolChoice{}, err
	}
	if choiceType != "function" {
		return policyResponsesParsedToolChoice{}, newChatInvalidRequest("tool_choice.type", fmt.Sprintf("tool choice type %q is not supported; only function is supported", choiceType))
	}
	if err := validatePolicyResponsesObjectFields(object, "tool_choice", "type", "name", "namespace"); err != nil {
		return policyResponsesParsedToolChoice{}, err
	}
	name, err := requiredPolicyResponsesStringAt(object, "name", "tool_choice.name", policyResponsesMaxToolNameLen)
	if err != nil {
		return policyResponsesParsedToolChoice{}, err
	}
	namespace, err := optionalPolicyResponsesNamespace(object, "namespace", "tool_choice.namespace")
	if err != nil {
		return policyResponsesParsedToolChoice{}, err
	}
	descriptor := policyResponsesToolDescriptor{Name: name, Namespace: namespace, Kind: policyResponsesToolKindFunction}
	return policyResponsesParsedToolChoice{descriptor: &descriptor}, nil
}

func buildPolicyResponsesToolAliases(descriptors map[policyResponsesToolDescriptor]struct{}) (map[policyResponsesToolDescriptor]string, policyResponsesToolMap, error) {
	aliases := make(map[policyResponsesToolDescriptor]string, len(descriptors))
	reverse := make(policyResponsesToolMap, len(aliases))
	for descriptor := range descriptors {
		alias := policyResponsesStableToolAlias(descriptor)
		if prior, collision := reverse[alias]; collision && prior != descriptor {
			return nil, nil, fmt.Errorf("policy tool alias collision between %s and %s", policyResponsesDescriptorSortKey(prior), policyResponsesDescriptorSortKey(descriptor))
		}
		aliases[descriptor] = alias
		reverse[alias] = descriptor
	}
	return aliases, reverse, nil
}

func policyResponsesStableToolAlias(descriptor policyResponsesToolDescriptor) string {
	if policyResponsesCanUseOriginalToolName(descriptor) {
		return descriptor.Name
	}
	stem := policyResponsesToolBaseAlias(descriptor)
	if len(stem) > policyResponsesAliasStemLen {
		stem = stem[:policyResponsesAliasStemLen]
	}
	identity, _ := json.Marshal([]string{descriptor.Kind, descriptor.Namespace, descriptor.Name})
	digest := sha256.Sum256(identity)
	return policyResponsesAliasPrefix + stem + "__" + base64.RawURLEncoding.EncodeToString(digest[:])
}

func policyResponsesCanUseOriginalToolName(descriptor policyResponsesToolDescriptor) bool {
	name := descriptor.Name
	if descriptor.Kind != policyResponsesToolKindFunction || descriptor.Namespace != "" || name == "" || len(name) > policyResponsesMaxChatToolNameLen || strings.HasPrefix(name, policyResponsesAliasPrefix) {
		return false
	}
	for _, r := range name {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') && r != '_' && r != '-' {
			return false
		}
	}
	return true
}

func policyResponsesToolBaseAlias(descriptor policyResponsesToolDescriptor) string {
	raw := descriptor.Name
	if descriptor.Namespace != "" {
		raw = descriptor.Namespace + "__" + descriptor.Name
	}
	var output strings.Builder
	lastReplacement := false
	for _, r := range raw {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			output.WriteRune(r)
			lastReplacement = false
			continue
		}
		if !lastReplacement {
			output.WriteByte('_')
			lastReplacement = true
		}
	}
	if output.Len() == 0 {
		return "tool"
	}
	return output.String()
}

func policyResponsesDescriptorSortKey(descriptor policyResponsesToolDescriptor) string {
	return descriptor.Kind + "\x00" + descriptor.Namespace + "\x00" + descriptor.Name
}

func translatePolicyResponsesInputMessages(instructions string, items []policyResponsesInputItem, aliases map[policyResponsesToolDescriptor]string) ([]models.OpenAIMessage, error) {
	messages := make([]models.OpenAIMessage, 0, len(items)+1)
	if instructions != "" {
		messages = append(messages, models.OpenAIMessage{Role: "developer", Content: policyResponsesJSONString(instructions)})
	}
	appendCalls := func(start int) ([]models.OpenAIToolCall, int, error) {
		calls := make([]models.OpenAIToolCall, 0, 1)
		index := start
		for index < len(items) && items[index].kind == "function_call" {
			call := items[index]
			alias, ok := aliases[call.descriptor]
			if !ok {
				return nil, start, fmt.Errorf("missing policy tool alias for %s", policyResponsesDescriptorSortKey(call.descriptor))
			}
			calls = append(calls, models.OpenAIToolCall{
				ID: call.callID, Type: "function",
				Function: models.OpenAIFunctionCall{Name: alias, Arguments: call.arguments},
			})
			index++
		}
		return calls, index, nil
	}

	for index := 0; index < len(items); {
		item := items[index]
		if item.kind == "function_call" {
			calls, next, err := appendCalls(index)
			if err != nil {
				return nil, err
			}
			messages = append(messages, models.OpenAIMessage{Role: "assistant", ToolCalls: calls})
			index = next
			continue
		}
		switch item.kind {
		case "message":
			message := models.OpenAIMessage{Role: item.role, Content: policyResponsesJSONString(item.text)}
			if item.role == "assistant" && index+1 < len(items) && items[index+1].kind == "function_call" {
				calls, next, err := appendCalls(index + 1)
				if err != nil {
					return nil, err
				}
				message.ToolCalls = calls
				index = next
			} else {
				index++
			}
			messages = append(messages, message)
		case "function_call_output":
			messages = append(messages, models.OpenAIMessage{Role: "tool", ToolCallID: item.callID, Content: policyResponsesJSONString(item.text)})
			index++
		default:
			return nil, fmt.Errorf("unsupported parsed policy Responses item %q", item.kind)
		}
	}
	return messages, nil
}

func translatePolicyResponsesTools(parsed []policyResponsesParsedTool, aliases map[policyResponsesToolDescriptor]string) []models.OpenAITool {
	if len(parsed) == 0 {
		return nil
	}
	tools := make([]models.OpenAITool, 0, len(parsed))
	for _, tool := range parsed {
		tools = append(tools, models.OpenAITool{
			Type: "function",
			Function: models.OpenAIFunction{
				Name:        aliases[tool.descriptor],
				Description: tool.description,
				Parameters:  append(json.RawMessage(nil), tool.parameters...),
				Strict:      tool.strict,
			},
		})
	}
	return tools
}

func translatePolicyResponsesToolChoice(choice policyResponsesParsedToolChoice, aliases map[policyResponsesToolDescriptor]string) (json.RawMessage, error) {
	if choice.descriptor != nil {
		alias, ok := aliases[*choice.descriptor]
		if !ok {
			return nil, newChatInvalidRequest("tool_choice", "tool_choice function alias is unavailable")
		}
		body, _ := json.Marshal(map[string]any{"type": "function", "function": map[string]any{"name": alias}})
		return body, nil
	}
	if choice.mode == "" {
		return nil, nil
	}
	return policyResponsesJSONString(choice.mode), nil
}

func parsePolicyResponsesReasoning(raw json.RawMessage) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 {
		return "", nil
	}
	if bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	object, err := validateChatRawObjectFields(raw, "reasoning", "effort", "summary")
	if err != nil {
		return "", err
	}
	if summaryRaw, ok := object["summary"]; ok {
		var summary string
		if bytes.Equal(bytes.TrimSpace(summaryRaw), []byte("null")) || json.Unmarshal(summaryRaw, &summary) != nil {
			return "", newChatInvalidRequest("reasoning.summary", "reasoning.summary must be auto, concise, or detailed")
		}
		switch strings.TrimSpace(summary) {
		case "auto", "concise", "detailed":
		default:
			return "", newChatInvalidRequest("reasoning.summary", "reasoning.summary must be auto, concise, or detailed")
		}
	}
	effortRaw, ok := object["effort"]
	if !ok {
		return "", nil
	}
	var effort string
	if bytes.Equal(bytes.TrimSpace(effortRaw), []byte("null")) || json.Unmarshal(effortRaw, &effort) != nil || strings.TrimSpace(effort) == "" || len(effort) > 64 {
		return "", newChatInvalidRequest("reasoning.effort", "reasoning.effort must be a bounded non-empty string")
	}
	return strings.TrimSpace(effort), nil
}

func policyResponsesBool(root map[string]json.RawMessage, field string, defaultValue bool) (bool, error) {
	raw, ok := root[field]
	if !ok {
		return defaultValue, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return false, newChatInvalidRequest(field, field+" must be a boolean")
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return false, newChatInvalidRequest(field, field+" must be a boolean")
	}
	return value, nil
}

func policyResponsesOptionalBool(root map[string]json.RawMessage, field string) (*bool, error) {
	raw, ok := root[field]
	if !ok {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, newChatInvalidRequest(field, field+" must be a boolean")
	}
	var value bool
	if err := json.Unmarshal(raw, &value); err != nil {
		return nil, newChatInvalidRequest(field, field+" must be a boolean")
	}
	return &value, nil
}

func policyResponsesPositiveInt(root map[string]json.RawMessage, field string) (*int, error) {
	raw, ok := root[field]
	if !ok {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, newChatInvalidRequest(field, field+" must be a positive integer")
	}
	var value int
	if err := json.Unmarshal(raw, &value); err != nil || value <= 0 {
		return nil, newChatInvalidRequest(field, field+" must be a positive integer")
	}
	return &value, nil
}

func policyResponsesFloat(root map[string]json.RawMessage, field string, minimum, maximum float64) (*float64, error) {
	raw, ok := root[field]
	if !ok {
		return nil, nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil, newChatInvalidRequest(field, fmt.Sprintf("%s must be a number in [%g, %g]", field, minimum, maximum))
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil || value < minimum || value > maximum {
		return nil, newChatInvalidRequest(field, fmt.Sprintf("%s must be a number in [%g, %g]", field, minimum, maximum))
	}
	return &value, nil
}

func requiredPolicyResponsesString(root map[string]json.RawMessage, field string, maxLen int) (string, error) {
	return requiredPolicyResponsesStringValue(root, field, field, maxLen, false)
}

func requiredPolicyResponsesStringAt(root map[string]json.RawMessage, field, param string, maxLen int) (string, error) {
	return requiredPolicyResponsesStringValue(root, field, param, maxLen, false)
}

func requiredPolicyResponsesStringValue(root map[string]json.RawMessage, field, param string, maxLen int, allowEmpty bool) (string, error) {
	raw, ok := root[field]
	if !ok {
		return "", newChatInvalidRequest(param, field+" is required")
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", newChatInvalidRequest(param, field+" must be a bounded string")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || (!allowEmpty && strings.TrimSpace(value) == "") || len(value) > maxLen {
		return "", newChatInvalidRequest(param, field+" must be a bounded non-empty string")
	}
	if allowEmpty {
		return value, nil
	}
	return strings.TrimSpace(value), nil
}

func optionalPolicyResponsesNamespace(root map[string]json.RawMessage, field, param string) (string, error) {
	raw, ok := root[field]
	if !ok {
		return "", nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", newChatInvalidRequest(param, "namespace must be a bounded non-empty string when provided")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" || len(value) > policyResponsesMaxToolNameLen {
		return "", newChatInvalidRequest(param, "namespace must be a bounded non-empty string when provided")
	}
	return strings.TrimSpace(value), nil
}

func optionalPolicyResponsesText(root map[string]json.RawMessage, field string, maxLen int) (string, error) {
	return optionalPolicyResponsesTextAt(root, field, field, maxLen)
}

func optionalPolicyResponsesTextAt(root map[string]json.RawMessage, field, param string, maxLen int) (string, error) {
	raw, ok := root[field]
	if !ok {
		return "", nil
	}
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", newChatInvalidRequest(param, field+" must be a bounded string")
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || len(value) > maxLen {
		return "", newChatInvalidRequest(param, field+" must be a bounded string")
	}
	return value, nil
}

func optionalPolicyResponsesString(root map[string]json.RawMessage, field string, maxLen int) (string, error) {
	return optionalPolicyResponsesStringAt(root, field, field, maxLen)
}

func optionalPolicyResponsesStringAt(root map[string]json.RawMessage, field, param string, maxLen int) (string, error) {
	raw, ok := root[field]
	if !ok || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return "", nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || len(value) > maxLen {
		return "", newChatInvalidRequest(param, field+" must be a bounded string")
	}
	return strings.TrimSpace(value), nil
}

func validatePolicyResponsesObjectFields(object map[string]json.RawMessage, param string, allowedFields ...string) error {
	allowed := make(map[string]struct{}, len(allowedFields))
	for _, field := range allowedFields {
		allowed[field] = struct{}{}
	}
	unknown := make([]string, 0)
	for field := range object {
		if _, ok := allowed[field]; !ok {
			unknown = append(unknown, field)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return newChatInvalidRequest(param+"."+unknown[0], "unsupported JSON field")
}

func prefixPolicyResponsesError(err error, prefix string) error {
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr == nil {
		return err
	}
	copyErr := *executionErr
	if copyErr.Param != "" {
		copyErr.Param = prefix + "." + copyErr.Param
	} else {
		copyErr.Param = prefix
	}
	return &copyErr
}

func policyResponsesJSONString(value string) json.RawMessage {
	encoded, _ := json.Marshal(value)
	return encoded
}

func validatePolicyResponsesJSON(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := walkPolicyResponsesJSON(decoder, "", 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return newChatInvalidRequest("", "request body must contain one JSON value")
	}
	return nil
}

func walkPolicyResponsesJSON(decoder *json.Decoder, path string, depth int) error {
	if depth > policyResponsesMaxJSONDepth {
		return newChatInvalidRequest(path, fmt.Sprintf("JSON nesting exceeds %d levels", policyResponsesMaxJSONDepth))
	}
	token, err := decoder.Token()
	if err != nil {
		return newChatInvalidRequest(path, "invalid JSON in request body")
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return newChatInvalidRequest(path, "invalid JSON object")
			}
			key, ok := keyToken.(string)
			if !ok {
				return newChatInvalidRequest(path, "invalid JSON object key")
			}
			fieldPath := key
			if path != "" {
				fieldPath = path + "." + key
			}
			if _, duplicate := seen[key]; duplicate {
				return newChatInvalidRequest(fieldPath, "duplicate JSON field")
			}
			seen[key] = struct{}{}
			if err := walkPolicyResponsesJSON(decoder, fieldPath, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return newChatInvalidRequest(path, "invalid JSON object")
		}
	case '[':
		index := 0
		for decoder.More() {
			itemPath := fmt.Sprintf("%s[%d]", path, index)
			if path == "" {
				itemPath = fmt.Sprintf("[%d]", index)
			}
			if err := walkPolicyResponsesJSON(decoder, itemPath, depth+1); err != nil {
				return err
			}
			index++
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return newChatInvalidRequest(path, "invalid JSON array")
		}
	default:
		return newChatInvalidRequest(path, "invalid JSON delimiter")
	}
	return nil
}
