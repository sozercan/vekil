package proxy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

const (
	policyFactSchemaVersion       = "coding_agent_v1_facts_v1"
	policyFactDefaultRequestBytes = 16_000
	policyFactMinRequestBytes     = 1_024
	policyFactMaxRequestBytes     = 65_536
	policyFactMaxRecentTurns      = 8

	policyFactAnchorBytes        = 2_000
	policyFactFirstTaskBytes     = 4_000
	policyFactRecentMessageBytes = 1_500
	policyFactToolNameBytes      = 128
	policyFactMaxTools           = 128
	policyFactMaxSourceBytes     = 1 << 20
	policyFactMaxArrayItems      = 1024
)

// policyFactOptions contains only the request-local limits needed by the facts
// builder. It intentionally does not depend on the concurrently compiled policy
// configuration types.
type policyFactOptions struct {
	RecentTurns     int
	MaxRequestBytes int
}

func (o policyFactOptions) normalized() (policyFactOptions, error) {
	if o.RecentTurns < 0 || o.RecentTurns > policyFactMaxRecentTurns {
		return policyFactOptions{}, fmt.Errorf("recent_turns must be in 0..%d", policyFactMaxRecentTurns)
	}
	if o.MaxRequestBytes == 0 {
		o.MaxRequestBytes = policyFactDefaultRequestBytes
	}
	if o.MaxRequestBytes < policyFactMinRequestBytes || o.MaxRequestBytes > policyFactMaxRequestBytes {
		return policyFactOptions{}, fmt.Errorf("max_request_bytes must be in %d..%d", policyFactMinRequestBytes, policyFactMaxRequestBytes)
	}
	return o, nil
}

type policyFactRole string

const (
	policyFactRoleSystem    policyFactRole = "system"
	policyFactRoleDeveloper policyFactRole = "developer"
	policyFactRoleUser      policyFactRole = "user"
	policyFactRoleAssistant policyFactRole = "assistant"
	policyFactRoleTool      policyFactRole = "tool"
)

type policyFactMessage struct {
	Role          policyFactRole `json:"role"`
	Text          string         `json:"text"`
	OriginalBytes int            `json:"original_bytes"`
	Truncated     bool           `json:"truncated"`
}

type policyFactTool struct {
	Name          string `json:"name"`
	OriginalBytes int    `json:"original_bytes"`
	Truncated     bool   `json:"truncated"`
}

// policyFactCounts keeps original-shape counts separate from the bounded
// projections. This lets metrics and deterministic mapping observe omission
// without retaining any omitted content.
type policyFactCounts struct {
	RequestOriginalBytes int `json:"request_original_bytes"`
	Messages             int `json:"messages"`
	SystemMessages       int `json:"system_messages"`
	DeveloperMessages    int `json:"developer_messages"`
	UserMessages         int `json:"user_messages"`
	AssistantMessages    int `json:"assistant_messages"`
	ToolMessages         int `json:"tool_messages"`
	TextMessages         int `json:"text_messages"`

	AnchorMessages         int `json:"anchor_messages"`
	IncludedAnchorMessages int `json:"included_anchor_messages"`
	RecentMessages         int `json:"recent_messages"`
	IncludedRecentMessages int `json:"included_recent_messages"`

	FunctionTools         int `json:"function_tools"`
	IncludedFunctionTools int `json:"included_function_tools"`
	AssistantToolCalls    int `json:"assistant_tool_calls"`

	AnchorOriginalBytes   int `json:"anchor_original_bytes"`
	TaskOriginalBytes     int `json:"task_original_bytes"`
	ContextOriginalBytes  int `json:"context_original_bytes"`
	ToolNameOriginalBytes int `json:"tool_name_original_bytes"`
}

type policyFactTruncation struct {
	Anchors          bool `json:"anchors"`
	FirstUserTask    bool `json:"first_user_task"`
	RecentMessages   bool `json:"recent_messages"`
	FunctionTools    bool `json:"function_tools"`
	SerializedBudget bool `json:"serialized_budget"`
}

// policyClassifierFacts is the only user-content-bearing value passed to the
// classifier seam. It contains bounded text and function names, never schemas,
// arguments, IDs, provider state, or routing metadata.
type policyClassifierFacts struct {
	SchemaVersion  string               `json:"schema_version"`
	Anchors        []policyFactMessage  `json:"anchors,omitempty"`
	FirstUserTask  *policyFactMessage   `json:"first_user_task,omitempty"`
	RecentMessages []policyFactMessage  `json:"recent_messages,omitempty"`
	FunctionTools  []policyFactTool     `json:"function_tools,omitempty"`
	Counts         policyFactCounts     `json:"counts"`
	Truncation     policyFactTruncation `json:"truncation"`
}

func (f policyClassifierFacts) taskOrContextTruncated() bool {
	return f.Truncation.Anchors || f.Truncation.FirstUserTask || f.Truncation.RecentMessages
}

func (f policyClassifierFacts) messageCount() int {
	return f.Counts.Messages
}

func (f policyClassifierFacts) toolCount() int {
	return f.Counts.FunctionTools
}

func (f policyClassifierFacts) inputBytes() int {
	return f.Counts.RequestOriginalBytes
}

func (f policyClassifierFacts) truncated() bool {
	return f.Truncation.Anchors || f.Truncation.FirstUserTask || f.Truncation.RecentMessages || f.Truncation.FunctionTools || f.Truncation.SerializedBudget
}

func (f policyClassifierFacts) marshal() ([]byte, error) {
	return json.Marshal(f)
}

type policyFactBuildError struct {
	Param   string
	Message string
}

func (e *policyFactBuildError) Error() string {
	if e == nil {
		return ""
	}
	if e.Param == "" {
		return e.Message
	}
	return e.Param + ": " + e.Message
}

func newPolicyFactBuildError(param, message string) error {
	return &policyFactBuildError{Param: param, Message: message}
}

type parsedPolicyFactMessage struct {
	role      policyFactRole
	text      string
	textBytes int
}

// buildPolicyClassifierFacts validates the supported first-release Chat shape
// and builds a deterministic, bounded facts snapshot from the original body.
func buildPolicyClassifierFacts(body []byte, opts policyFactOptions) (policyClassifierFacts, error) {
	if len(body) > policyFactMaxSourceBytes {
		return policyClassifierFacts{}, newPolicyFactBuildError("", fmt.Sprintf("policy request exceeds %d-byte fact-processing limit", policyFactMaxSourceBytes))
	}
	normalized, err := opts.normalized()
	if err != nil {
		return policyClassifierFacts{}, err
	}

	root, err := decodePolicyFactObject(body, "")
	if err != nil {
		return policyClassifierFacts{}, err
	}
	if rawAudio, ok := root["audio"]; ok && !policyFactJSONNull(rawAudio) {
		return policyClassifierFacts{}, newPolicyFactBuildError("audio", "audio output controls are not supported")
	}
	if rawSearch, ok := root["web_search_options"]; ok && !policyFactJSONNull(rawSearch) {
		return policyClassifierFacts{}, newPolicyFactBuildError("web_search_options", "hosted web search is not supported")
	}
	if rawModalities, ok := root["modalities"]; ok && !policyFactJSONNull(rawModalities) {
		modalities, err := decodePolicyFactArray(rawModalities, "modalities", false)
		if err != nil {
			return policyClassifierFacts{}, err
		}
		for index, rawModality := range modalities {
			var modality string
			if json.Unmarshal(rawModality, &modality) != nil || strings.TrimSpace(modality) != "text" {
				return policyClassifierFacts{}, newPolicyFactBuildError(fmt.Sprintf("modalities[%d]", index), "only text modality is supported")
			}
		}
	}

	rawMessages, ok := root["messages"]
	if !ok {
		return policyClassifierFacts{}, newPolicyFactBuildError("messages", "messages is required")
	}
	messageItems, err := decodePolicyFactArray(rawMessages, "messages", false)
	if err != nil {
		return policyClassifierFacts{}, err
	}
	if rawFunctions, ok := root["functions"]; ok && !policyFactJSONNull(rawFunctions) {
		return policyClassifierFacts{}, newPolicyFactBuildError("functions", "legacy functions are not supported; use function tools")
	}
	if rawFunctionChoice, ok := root["function_call"]; ok && !policyFactJSONNull(rawFunctionChoice) {
		return policyClassifierFacts{}, newPolicyFactBuildError("function_call", "legacy function_call is not supported; use tool_choice")
	}

	facts := policyClassifierFacts{SchemaVersion: policyFactSchemaVersion}
	facts.Counts.RequestOriginalBytes = len(body)
	parsedMessages := make([]parsedPolicyFactMessage, 0, len(messageItems))
	firstUserIndex := -1
	for index, rawMessage := range messageItems {
		message, toolCalls, err := parsePolicyFactMessage(rawMessage, index)
		if err != nil {
			return policyClassifierFacts{}, err
		}
		parsedMessages = append(parsedMessages, message)
		facts.Counts.Messages++
		facts.Counts.AssistantToolCalls += toolCalls
		switch message.role {
		case policyFactRoleSystem:
			facts.Counts.SystemMessages++
		case policyFactRoleDeveloper:
			facts.Counts.DeveloperMessages++
		case policyFactRoleUser:
			facts.Counts.UserMessages++
			if firstUserIndex < 0 && message.text != "" {
				firstUserIndex = index
			}
		case policyFactRoleAssistant:
			facts.Counts.AssistantMessages++
		case policyFactRoleTool:
			facts.Counts.ToolMessages++
		}
		if message.text != "" {
			facts.Counts.TextMessages++
		}
	}

	buildPolicyAnchorFacts(&facts, parsedMessages)
	buildPolicyTaskFact(&facts, parsedMessages, firstUserIndex)
	buildPolicyRecentFacts(&facts, parsedMessages, firstUserIndex, normalized.RecentTurns)

	rawTools, hasTools := root["tools"]
	if hasTools {
		if err := buildPolicyToolFacts(&facts, rawTools); err != nil {
			return policyClassifierFacts{}, err
		}
	}
	if rawToolChoice, ok := root["tool_choice"]; ok {
		if err := validatePolicyFactToolChoice(rawToolChoice, rawTools, hasTools); err != nil {
			return policyClassifierFacts{}, err
		}
	}

	if err := fitPolicyFactsToSerializedBudget(&facts, normalized.MaxRequestBytes); err != nil {
		return policyClassifierFacts{}, err
	}
	return facts, nil
}

func parsePolicyFactMessage(raw json.RawMessage, index int) (parsedPolicyFactMessage, int, error) {
	param := fmt.Sprintf("messages[%d]", index)
	object, err := decodePolicyFactObject(raw, param)
	if err != nil {
		return parsedPolicyFactMessage{}, 0, err
	}
	rawRole, ok := object["role"]
	if !ok {
		return parsedPolicyFactMessage{}, 0, newPolicyFactBuildError(param+".role", "role is required")
	}
	var roleText string
	if err := json.Unmarshal(rawRole, &roleText); err != nil {
		return parsedPolicyFactMessage{}, 0, newPolicyFactBuildError(param+".role", "role must be a string")
	}
	role := policyFactRole(strings.TrimSpace(roleText))
	switch role {
	case policyFactRoleSystem, policyFactRoleDeveloper, policyFactRoleUser, policyFactRoleAssistant, policyFactRoleTool:
	default:
		return parsedPolicyFactMessage{}, 0, newPolicyFactBuildError(param+".role", "unsupported Chat message role")
	}

	if rawAudio, ok := object["audio"]; ok && !policyFactJSONNull(rawAudio) {
		return parsedPolicyFactMessage{}, 0, newPolicyFactBuildError(param+".audio", "audio message content is not supported")
	}
	if rawFunctionCall, ok := object["function_call"]; ok && !policyFactJSONNull(rawFunctionCall) {
		return parsedPolicyFactMessage{}, 0, newPolicyFactBuildError(param+".function_call", "legacy function_call is not supported; use tool_calls")
	}

	text := ""
	if rawContent, ok := object["content"]; ok {
		text, err = decodePolicyFactTextContent(rawContent, param+".content")
		if err != nil {
			return parsedPolicyFactMessage{}, 0, err
		}
	}

	toolCallCount := 0
	if rawToolCalls, ok := object["tool_calls"]; ok {
		toolCallCount, err = validatePolicyFactToolCalls(rawToolCalls, param+".tool_calls")
		if err != nil {
			return parsedPolicyFactMessage{}, 0, err
		}
		if role != policyFactRoleAssistant && toolCallCount > 0 {
			return parsedPolicyFactMessage{}, 0, newPolicyFactBuildError(param+".tool_calls", "tool_calls is supported only on assistant messages")
		}
	}
	if role == policyFactRoleTool {
		rawID, ok := object["tool_call_id"]
		if !ok {
			return parsedPolicyFactMessage{}, 0, newPolicyFactBuildError(param+".tool_call_id", "tool_call_id is required for tool messages")
		}
		var callID string
		if err := json.Unmarshal(rawID, &callID); err != nil || strings.TrimSpace(callID) == "" {
			return parsedPolicyFactMessage{}, 0, newPolicyFactBuildError(param+".tool_call_id", "tool_call_id must be a non-empty string")
		}
	}

	return parsedPolicyFactMessage{role: role, text: text, textBytes: len(text)}, toolCallCount, nil
}

func decodePolicyFactTextContent(raw json.RawMessage, param string) (string, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return "", nil
	}
	var text string
	if err := json.Unmarshal(trimmed, &text); err == nil {
		return text, nil
	}
	parts, err := decodePolicyFactArray(trimmed, param, true)
	if err != nil {
		return "", newPolicyFactBuildError(param, "content must be a string, null, or an array of text parts")
	}
	var combined strings.Builder
	for index, rawPart := range parts {
		partParam := fmt.Sprintf("%s[%d]", param, index)
		part, err := decodePolicyFactObject(rawPart, partParam)
		if err != nil {
			return "", err
		}
		for field := range part {
			if field != "type" && field != "text" {
				return "", newPolicyFactBuildError(partParam+"."+field, "text content parts may contain only type and text")
			}
		}
		var partType string
		if rawType, ok := part["type"]; !ok || json.Unmarshal(rawType, &partType) != nil || partType != "text" {
			return "", newPolicyFactBuildError(partParam+".type", "only text content parts are supported")
		}
		var partText string
		if rawText, ok := part["text"]; !ok || json.Unmarshal(rawText, &partText) != nil {
			return "", newPolicyFactBuildError(partParam+".text", "text content part requires a string text field")
		}
		combined.WriteString(partText)
	}
	return combined.String(), nil
}

func validatePolicyFactToolCalls(raw json.RawMessage, param string) (int, error) {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return 0, nil
	}
	calls, err := decodePolicyFactArray(trimmed, param, true)
	if err != nil {
		return 0, err
	}
	for index, rawCall := range calls {
		callParam := fmt.Sprintf("%s[%d]", param, index)
		call, err := decodePolicyFactObject(rawCall, callParam)
		if err != nil {
			return 0, err
		}
		if rawType, ok := call["type"]; ok {
			var callType string
			if json.Unmarshal(rawType, &callType) != nil || (callType != "" && callType != "function") {
				return 0, newPolicyFactBuildError(callParam+".type", "only function tool calls are supported")
			}
		}
		var callID string
		if rawID, ok := call["id"]; !ok || json.Unmarshal(rawID, &callID) != nil || strings.TrimSpace(callID) == "" {
			return 0, newPolicyFactBuildError(callParam+".id", "tool call ID is required")
		}
		rawFunction, ok := call["function"]
		if !ok {
			return 0, newPolicyFactBuildError(callParam+".function", "function is required")
		}
		function, err := decodePolicyFactObject(rawFunction, callParam+".function")
		if err != nil {
			return 0, err
		}
		var name string
		if rawName, ok := function["name"]; !ok || json.Unmarshal(rawName, &name) != nil || strings.TrimSpace(name) == "" {
			return 0, newPolicyFactBuildError(callParam+".function.name", "function name is required")
		}
		rawArguments, ok := function["arguments"]
		if !ok {
			return 0, newPolicyFactBuildError(callParam+".function.arguments", "function arguments string is required")
		}
		var arguments string
		if json.Unmarshal(rawArguments, &arguments) != nil {
			return 0, newPolicyFactBuildError(callParam+".function.arguments", "function arguments must be a string")
		}
	}
	return len(calls), nil
}

func buildPolicyAnchorFacts(facts *policyClassifierFacts, messages []parsedPolicyFactMessage) {
	remaining := policyFactAnchorBytes
	for _, message := range messages {
		if message.role != policyFactRoleSystem && message.role != policyFactRoleDeveloper {
			continue
		}
		if message.text == "" {
			continue
		}
		facts.Counts.AnchorMessages++
		facts.Counts.AnchorOriginalBytes += message.textBytes
		if remaining <= 0 {
			facts.Truncation.Anchors = true
			continue
		}
		text, truncated := truncatePolicyUTF8(message.text, remaining)
		fact := policyFactMessage{Role: message.role, Text: text, OriginalBytes: message.textBytes, Truncated: truncated}
		facts.Anchors = append(facts.Anchors, fact)
		facts.Counts.IncludedAnchorMessages++
		remaining -= len(text)
		if truncated {
			facts.Truncation.Anchors = true
		}
	}
	if facts.Counts.IncludedAnchorMessages < facts.Counts.AnchorMessages {
		facts.Truncation.Anchors = true
	}
}

func buildPolicyTaskFact(facts *policyClassifierFacts, messages []parsedPolicyFactMessage, firstUserIndex int) {
	if firstUserIndex < 0 || firstUserIndex >= len(messages) {
		return
	}
	message := messages[firstUserIndex]
	text, truncated := truncatePolicyUTF8(message.text, policyFactFirstTaskBytes)
	facts.FirstUserTask = &policyFactMessage{Role: policyFactRoleUser, Text: text, OriginalBytes: message.textBytes, Truncated: truncated}
	facts.Counts.TaskOriginalBytes = message.textBytes
	facts.Truncation.FirstUserTask = truncated
}

func buildPolicyRecentFacts(facts *policyClassifierFacts, messages []parsedPolicyFactMessage, firstUserIndex, recentTurns int) {
	candidates := make([]parsedPolicyFactMessage, 0, len(messages))
	for index, message := range messages {
		if index == firstUserIndex || message.role == policyFactRoleSystem || message.role == policyFactRoleDeveloper || message.text == "" {
			continue
		}
		candidates = append(candidates, message)
		facts.Counts.ContextOriginalBytes += message.textBytes
	}
	facts.Counts.RecentMessages = len(candidates)
	start := len(candidates) - recentTurns
	if start < 0 {
		start = 0
	}
	if start > 0 {
		facts.Truncation.RecentMessages = true
	}
	for _, message := range candidates[start:] {
		text, truncated := truncatePolicyUTF8(message.text, policyFactRecentMessageBytes)
		facts.RecentMessages = append(facts.RecentMessages, policyFactMessage{
			Role:          message.role,
			Text:          text,
			OriginalBytes: message.textBytes,
			Truncated:     truncated,
		})
		if truncated {
			facts.Truncation.RecentMessages = true
		}
	}
	facts.Counts.IncludedRecentMessages = len(facts.RecentMessages)
}

func buildPolicyToolFacts(facts *policyClassifierFacts, raw json.RawMessage) error {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return nil
	}
	tools, err := decodePolicyFactArray(trimmed, "tools", true)
	if err != nil {
		return err
	}
	facts.Counts.FunctionTools = len(tools)
	seen := make(map[string]struct{}, len(tools))
	for index, rawTool := range tools {
		toolParam := fmt.Sprintf("tools[%d]", index)
		tool, err := decodePolicyFactObject(rawTool, toolParam)
		if err != nil {
			return err
		}
		var toolType string
		if rawType, ok := tool["type"]; !ok || json.Unmarshal(rawType, &toolType) != nil || toolType != "function" {
			return newPolicyFactBuildError(toolParam+".type", "only function tools are supported")
		}
		rawFunction, ok := tool["function"]
		if !ok {
			return newPolicyFactBuildError(toolParam+".function", "function is required")
		}
		function, err := decodePolicyFactObject(rawFunction, toolParam+".function")
		if err != nil {
			return err
		}
		var name string
		if rawName, ok := function["name"]; !ok || json.Unmarshal(rawName, &name) != nil || strings.TrimSpace(name) == "" {
			return newPolicyFactBuildError(toolParam+".function.name", "function name is required")
		}
		if _, duplicate := seen[name]; duplicate {
			return newPolicyFactBuildError(toolParam+".function.name", "function names must be unique")
		}
		seen[name] = struct{}{}
		facts.Counts.ToolNameOriginalBytes += len(name)
		if index >= policyFactMaxTools {
			facts.Truncation.FunctionTools = true
			continue
		}
		bounded, truncated := truncatePolicyUTF8(name, policyFactToolNameBytes)
		facts.FunctionTools = append(facts.FunctionTools, policyFactTool{Name: bounded, OriginalBytes: len(name), Truncated: truncated})
		if truncated {
			facts.Truncation.FunctionTools = true
		}
	}
	facts.Counts.IncludedFunctionTools = len(facts.FunctionTools)
	return nil
}

func validatePolicyFactToolChoice(rawChoice, rawTools json.RawMessage, hasTools bool) error {
	if policyFactJSONNull(rawChoice) {
		return nil
	}
	var choice string
	if json.Unmarshal(rawChoice, &choice) == nil {
		switch strings.TrimSpace(choice) {
		case "auto", "none", "required":
			return nil
		default:
			return newPolicyFactBuildError("tool_choice", "unsupported tool_choice string")
		}
	}
	choiceObject, err := decodePolicyFactObject(rawChoice, "tool_choice")
	if err != nil {
		return err
	}
	for key := range choiceObject {
		if key != "type" && key != "function" {
			return newPolicyFactBuildError("tool_choice."+key, "unsupported tool_choice field")
		}
	}
	var choiceType string
	if rawType, ok := choiceObject["type"]; !ok || json.Unmarshal(rawType, &choiceType) != nil || choiceType != "function" {
		return newPolicyFactBuildError("tool_choice.type", "must be function")
	}
	rawFunction, ok := choiceObject["function"]
	if !ok {
		return newPolicyFactBuildError("tool_choice.function", "function is required")
	}
	function, err := decodePolicyFactObject(rawFunction, "tool_choice.function")
	if err != nil {
		return err
	}
	for key := range function {
		if key != "name" {
			return newPolicyFactBuildError("tool_choice.function."+key, "unsupported function choice field")
		}
	}
	var name string
	if rawName, ok := function["name"]; !ok || json.Unmarshal(rawName, &name) != nil || strings.TrimSpace(name) == "" {
		return newPolicyFactBuildError("tool_choice.function.name", "function name is required")
	}
	if !hasTools || !policyFactToolNameDeclared(rawTools, name) {
		return newPolicyFactBuildError("tool_choice.function.name", "must select a declared function tool")
	}
	return nil
}

func policyFactToolNameDeclared(rawTools json.RawMessage, selected string) bool {
	tools, err := decodePolicyFactArray(rawTools, "tools", true)
	if err != nil {
		return false
	}
	for index, rawTool := range tools {
		tool, err := decodePolicyFactObject(rawTool, fmt.Sprintf("tools[%d]", index))
		if err != nil {
			continue
		}
		function, err := decodePolicyFactObject(tool["function"], fmt.Sprintf("tools[%d].function", index))
		if err != nil {
			continue
		}
		var name string
		if json.Unmarshal(function["name"], &name) == nil && name == selected {
			return true
		}
	}
	return false
}

func fitPolicyFactsToSerializedBudget(facts *policyClassifierFacts, maxBytes int) error {
	encoded, err := facts.marshal()
	if err != nil {
		return err
	}
	if len(encoded) <= maxBytes {
		return nil
	}
	facts.Truncation.SerializedBudget = true

	// Tool names are useful but lower priority than task and conversational
	// context. Drop them from the tail first while preserving original counts.
	for len(facts.FunctionTools) > 0 && len(encoded) > maxBytes {
		last := len(facts.FunctionTools) - 1
		facts.FunctionTools[last] = policyFactTool{}
		facts.FunctionTools = facts.FunctionTools[:last]
		facts.Counts.IncludedFunctionTools = len(facts.FunctionTools)
		facts.Truncation.FunctionTools = true
		encoded, err = facts.marshal()
		if err != nil {
			return err
		}
	}

	// Preserve the earliest anchors and newest recent messages. Dropping any
	// content marks the context as truncated so the mapper can conservatively
	// choose the powerful tier.
	for len(facts.Anchors) > 0 && len(encoded) > maxBytes {
		last := len(facts.Anchors) - 1
		facts.Anchors[last] = policyFactMessage{}
		facts.Anchors = facts.Anchors[:last]
		facts.Counts.IncludedAnchorMessages = len(facts.Anchors)
		facts.Truncation.Anchors = true
		encoded, err = facts.marshal()
		if err != nil {
			return err
		}
	}
	for len(facts.RecentMessages) > 0 && len(encoded) > maxBytes {
		facts.RecentMessages[0] = policyFactMessage{}
		facts.RecentMessages = facts.RecentMessages[1:]
		facts.Counts.IncludedRecentMessages = len(facts.RecentMessages)
		facts.Truncation.RecentMessages = true
		encoded, err = facts.marshal()
		if err != nil {
			return err
		}
	}

	if len(encoded) > maxBytes && facts.FirstUserTask != nil && facts.FirstUserTask.Text != "" {
		original := facts.FirstUserTask.Text
		best, ok, searchErr := largestPolicyUTF8PrefixThatFits(original, func(candidate string) (bool, error) {
			facts.FirstUserTask.Text = candidate
			probe, marshalErr := facts.marshal()
			return len(probe) <= maxBytes, marshalErr
		})
		if searchErr != nil {
			return searchErr
		}
		facts.FirstUserTask.Text = best
		facts.FirstUserTask.Truncated = true
		facts.Truncation.FirstUserTask = true
		encoded, err = facts.marshal()
		if err != nil {
			return err
		}
		if !ok || len(encoded) > maxBytes {
			facts.FirstUserTask.Text = ""
			encoded, err = facts.marshal()
			if err != nil {
				return err
			}
		}
	}
	if len(encoded) > maxBytes {
		return fmt.Errorf("policy facts metadata exceeds max_request_bytes")
	}
	return nil
}

func largestPolicyUTF8PrefixThatFits(value string, fits func(string) (bool, error)) (string, bool, error) {
	boundaries := make([]int, 1, utf8.RuneCountInString(value)+1)
	for index := range value {
		if index > 0 {
			boundaries = append(boundaries, index)
		}
	}
	boundaries = append(boundaries, len(value))
	lo, hi := 0, len(boundaries)-1
	best := -1
	for lo <= hi {
		mid := lo + (hi-lo)/2
		candidate := value[:boundaries[mid]]
		ok, err := fits(candidate)
		if err != nil {
			return "", false, err
		}
		if ok {
			best = mid
			lo = mid + 1
		} else {
			hi = mid - 1
		}
	}
	if best < 0 {
		return "", false, nil
	}
	return strings.Clone(value[:boundaries[best]]), true, nil
}

func truncatePolicyUTF8(value string, maxBytes int) (string, bool) {
	if maxBytes < 0 {
		maxBytes = 0
	}
	if len(value) <= maxBytes {
		return value, false
	}
	end := maxBytes
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return strings.Clone(value[:end]), true
}

func policyFactJSONNull(raw []byte) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

func decodePolicyFactArray(raw []byte, param string, allowNull bool) ([]json.RawMessage, error) {
	trimmed := bytes.TrimSpace(raw)
	if allowNull && bytes.Equal(trimmed, []byte("null")) {
		return nil, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	token, err := decoder.Token()
	if err != nil {
		return nil, newPolicyFactBuildError(param, "must be a JSON array")
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '[' {
		return nil, newPolicyFactBuildError(param, "must be a JSON array")
	}
	values := make([]json.RawMessage, 0, min(policyFactMaxArrayItems, 16))
	for decoder.More() {
		if len(values) >= policyFactMaxArrayItems {
			return nil, newPolicyFactBuildError(param, fmt.Sprintf("contains more than %d items", policyFactMaxArrayItems))
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, newPolicyFactBuildError(param, "contains invalid JSON")
		}
		values = append(values, value)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, newPolicyFactBuildError(param, "must be a JSON array")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, newPolicyFactBuildError(param, "must contain one JSON array")
	}
	return values, nil
}

func decodePolicyFactObject(raw []byte, param string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	token, err := decoder.Token()
	if err != nil {
		return nil, newPolicyFactBuildError(param, "invalid JSON object")
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, newPolicyFactBuildError(param, "must be a JSON object")
	}
	object := make(map[string]json.RawMessage)
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return nil, newPolicyFactBuildError(param, "invalid JSON object")
		}
		key, ok := keyToken.(string)
		if !ok {
			return nil, newPolicyFactBuildError(param, "invalid JSON object key")
		}
		if _, duplicate := object[key]; duplicate {
			field := key
			if param != "" {
				field = param + "." + key
			}
			return nil, newPolicyFactBuildError(field, "duplicate JSON field")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, newPolicyFactBuildError(param, "invalid JSON object value")
		}
		object[key] = append(json.RawMessage(nil), value...)
	}
	if _, err := decoder.Token(); err != nil {
		return nil, newPolicyFactBuildError(param, "invalid JSON object")
	}
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return nil, newPolicyFactBuildError(param, "must contain one JSON object")
	}
	return object, nil
}
