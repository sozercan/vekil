package proxy

import (
	"encoding/json"
	"strings"
)

type toolCommandItem struct {
	ToolName string
	CallID   string
	Command  string
}

type toolOutputItem struct {
	CallID string
	Output string
}

func extractShellFunctionCommandItem(raw json.RawMessage, manager *ToolOptimizerManager) (toolCommandItem, bool) {
	if manager == nil || !manager.ShellFunctionCallsEnabled() || manager.ShellCommandArgPath() != "/command" {
		return toolCommandItem{}, false
	}
	var item struct {
		Type      string `json:"type"`
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
		CallID    string `json:"call_id"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return toolCommandItem{}, false
	}
	if item.Type != "function_call" || !manager.MatchShellToolName(item.Name) || strings.TrimSpace(item.CallID) == "" {
		return toolCommandItem{}, false
	}
	command, ok := extractCommandFromArguments(item.Arguments)
	if !ok {
		return toolCommandItem{}, false
	}
	return toolCommandItem{
		ToolName: strings.TrimSpace(item.Name),
		CallID:   strings.TrimSpace(item.CallID),
		Command:  command,
	}, true
}

func extractCommandFromArguments(argumentsJSON string) (string, bool) {
	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argumentsJSON), &args); err != nil {
		return "", false
	}
	rawCommand, ok := args["command"]
	if !ok {
		return "", false
	}
	var command string
	if err := json.Unmarshal(rawCommand, &command); err != nil {
		return "", false
	}
	return command, true
}

func replaceShellFunctionCommand(raw json.RawMessage, replacement string, manager *ToolOptimizerManager) (json.RawMessage, bool) {
	if manager == nil || manager.ShellCommandArgPath() != "/command" {
		return raw, false
	}
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return raw, false
	}
	var argumentsJSON string
	if err := json.Unmarshal(item["arguments"], &argumentsJSON); err != nil {
		return raw, false
	}
	var args map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argumentsJSON), &args); err != nil {
		return raw, false
	}
	replacementBytes, err := json.Marshal(replacement)
	if err != nil {
		return raw, false
	}
	args["command"] = replacementBytes
	newArgumentsBytes, err := json.Marshal(args)
	if err != nil {
		return raw, false
	}
	newArgumentsStringBytes, err := json.Marshal(string(newArgumentsBytes))
	if err != nil {
		return raw, false
	}
	item["arguments"] = newArgumentsStringBytes
	newItem, err := json.Marshal(item)
	if err != nil {
		return raw, false
	}
	return newItem, true
}

func extractFunctionCallOutputItem(raw json.RawMessage) (toolOutputItem, bool) {
	var item struct {
		Type   string `json:"type"`
		CallID string `json:"call_id"`
		Output string `json:"output"`
	}
	if err := json.Unmarshal(raw, &item); err != nil {
		return toolOutputItem{}, false
	}
	if item.Type != "function_call_output" || strings.TrimSpace(item.CallID) == "" {
		return toolOutputItem{}, false
	}
	return toolOutputItem{CallID: strings.TrimSpace(item.CallID), Output: item.Output}, true
}

func replaceFunctionCallOutput(raw json.RawMessage, replacement string) (json.RawMessage, bool) {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return raw, false
	}
	outputBytes, err := json.Marshal(replacement)
	if err != nil {
		return raw, false
	}
	item["output"] = outputBytes
	newItem, err := json.Marshal(item)
	if err != nil {
		return raw, false
	}
	return newItem, true
}
