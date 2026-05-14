package proxy

import (
	"encoding/json"
	"strconv"
	"strings"
)

type toolCommandItem struct {
	ToolName  string
	CallID    string
	Command   string
	Arguments string
}

type toolOutputItem struct {
	CallID string
	Output string
}

func extractShellFunctionCommandItem(raw json.RawMessage, manager *ToolOptimizerManager) (toolCommandItem, bool) {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return toolCommandItem{}, false
	}
	return extractShellFunctionCommand(item, manager)
}

func extractShellFunctionCommand(item map[string]json.RawMessage, manager *ToolOptimizerManager) (toolCommandItem, bool) {
	if manager == nil || !manager.ShellFunctionCallsEnabled() {
		return toolCommandItem{}, false
	}

	var itemType string
	if err := json.Unmarshal(item["type"], &itemType); err != nil || itemType != "function_call" {
		return toolCommandItem{}, false
	}

	var toolName string
	if err := json.Unmarshal(item["name"], &toolName); err != nil || !manager.MatchShellToolName(toolName) {
		return toolCommandItem{}, false
	}

	var callID string
	if err := json.Unmarshal(item["call_id"], &callID); err != nil || strings.TrimSpace(callID) == "" {
		return toolCommandItem{}, false
	}

	var arguments string
	if err := json.Unmarshal(item["arguments"], &arguments); err != nil {
		return toolCommandItem{}, false
	}

	command, ok := extractStringArgumentAtPath(arguments, manager.ShellCommandArgPath())
	if !ok {
		return toolCommandItem{}, false
	}

	return toolCommandItem{
		ToolName:  strings.TrimSpace(toolName),
		CallID:    strings.TrimSpace(callID),
		Command:   command,
		Arguments: arguments,
	}, true
}

func extractCommandFromArguments(argumentsJSON string) (string, bool) {
	return extractStringArgumentAtPath(argumentsJSON, "/command")
}

func replaceShellFunctionCommand(raw json.RawMessage, replacement string, manager *ToolOptimizerManager) (json.RawMessage, bool) {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return raw, false
	}
	if _, ok := extractShellFunctionCommand(item, manager); !ok {
		return raw, false
	}
	if !replaceShellFunctionCommandArguments(item, manager, replacement) {
		return raw, false
	}
	newItem, err := json.Marshal(item)
	if err != nil {
		return raw, false
	}
	return newItem, true
}

func replaceShellFunctionCommandArguments(item map[string]json.RawMessage, manager *ToolOptimizerManager, replacement string) bool {
	if manager == nil {
		return false
	}

	var arguments string
	if err := json.Unmarshal(item["arguments"], &arguments); err != nil {
		return false
	}

	newArguments, ok := replaceStringArgumentAtPath(arguments, manager.ShellCommandArgPath(), replacement)
	if !ok {
		return false
	}

	encoded, err := json.Marshal(newArguments)
	if err != nil {
		return false
	}
	item["arguments"] = encoded
	return true
}

func replaceCommandInArguments(argumentsJSON, replacement string) (string, bool) {
	return replaceStringArgumentAtPath(argumentsJSON, "/command", replacement)
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
	if (item.Type != "function_call_output" && item.Type != "local_shell_call_output") || strings.TrimSpace(item.CallID) == "" {
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

func extractStringArgumentAtPath(argumentsJSON, pointer string) (string, bool) {
	segments, ok := parseToolOptimizerJSONPointer(pointer)
	if !ok || len(segments) == 0 {
		return "", false
	}

	var value interface{}
	decoder := json.NewDecoder(strings.NewReader(argumentsJSON))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return "", false
	}

	current := value
	for _, segment := range segments {
		switch typed := current.(type) {
		case map[string]interface{}:
			next, ok := typed[segment]
			if !ok {
				return "", false
			}
			current = next
		case []interface{}:
			idx, ok := parseJSONPointerArrayIndex(segment, len(typed))
			if !ok {
				return "", false
			}
			current = typed[idx]
		default:
			return "", false
		}
	}

	command, ok := current.(string)
	if !ok {
		return "", false
	}
	return command, true
}

func replaceStringArgumentAtPath(argumentsJSON, pointer, replacement string) (string, bool) {
	segments, ok := parseToolOptimizerJSONPointer(pointer)
	if !ok || len(segments) == 0 {
		return argumentsJSON, false
	}

	var value interface{}
	decoder := json.NewDecoder(strings.NewReader(argumentsJSON))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return argumentsJSON, false
	}

	if !setStringAtJSONPointer(value, segments, replacement) {
		return argumentsJSON, false
	}

	newArguments, err := json.Marshal(value)
	if err != nil {
		return argumentsJSON, false
	}
	return string(newArguments), true
}

func parseToolOptimizerJSONPointer(pointer string) ([]string, bool) {
	pointer = strings.TrimSpace(pointer)
	if pointer == "" || !strings.HasPrefix(pointer, "/") {
		return nil, false
	}
	if pointer == "/" {
		return []string{""}, true
	}

	rawSegments := strings.Split(pointer[1:], "/")
	segments := make([]string, len(rawSegments))
	for i, raw := range rawSegments {
		decoded, ok := decodeJSONPointerSegment(raw)
		if !ok {
			return nil, false
		}
		segments[i] = decoded
	}
	return segments, true
}

func decodeJSONPointerSegment(segment string) (string, bool) {
	var b strings.Builder
	for i := 0; i < len(segment); i++ {
		if segment[i] != '~' {
			b.WriteByte(segment[i])
			continue
		}
		if i+1 >= len(segment) {
			return "", false
		}
		switch segment[i+1] {
		case '0':
			b.WriteByte('~')
		case '1':
			b.WriteByte('/')
		default:
			return "", false
		}
		i++
	}
	return b.String(), true
}

func parseJSONPointerArrayIndex(segment string, length int) (int, bool) {
	if segment == "" || segment == "-" {
		return 0, false
	}
	if len(segment) > 1 && segment[0] == '0' {
		return 0, false
	}
	idx, err := strconv.Atoi(segment)
	if err != nil || idx < 0 || idx >= length {
		return 0, false
	}
	return idx, true
}

func setStringAtJSONPointer(value interface{}, segments []string, replacement string) bool {
	current := value
	for i, segment := range segments {
		last := i == len(segments)-1
		switch typed := current.(type) {
		case map[string]interface{}:
			if last {
				if _, ok := typed[segment].(string); !ok {
					return false
				}
				typed[segment] = replacement
				return true
			}
			next, ok := typed[segment]
			if !ok {
				return false
			}
			current = next
		case []interface{}:
			idx, ok := parseJSONPointerArrayIndex(segment, len(typed))
			if !ok {
				return false
			}
			if last {
				if _, ok := typed[idx].(string); !ok {
					return false
				}
				typed[idx] = replacement
				return true
			}
			current = typed[idx]
		default:
			return false
		}
	}
	return false
}
