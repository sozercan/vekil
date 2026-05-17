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

	rawType, ok := item["type"]
	if !ok {
		return toolCommandItem{}, false
	}
	var itemType string
	if err := json.Unmarshal(rawType, &itemType); err != nil {
		return toolCommandItem{}, false
	}

	switch itemType {
	case "function_call":
		return extractConfiguredShellFunctionCommand(item, manager)
	case "local_shell_call":
		return extractLocalShellFunctionCommand(item)
	default:
		return toolCommandItem{}, false
	}
}

func extractConfiguredShellFunctionCommand(item map[string]json.RawMessage, manager *ToolOptimizerManager) (toolCommandItem, bool) {
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

func extractLocalShellFunctionCommand(item map[string]json.RawMessage) (toolCommandItem, bool) {
	callID, ok := extractNonEmptyJSONStringField(item, "call_id")
	if !ok {
		callID, ok = extractNonEmptyJSONStringField(item, "id")
	}
	if !ok {
		return toolCommandItem{}, false
	}

	command, ok := extractLocalShellCommand(item)
	if !ok {
		return toolCommandItem{}, false
	}

	toolName, ok := extractNonEmptyJSONStringField(item, "name")
	if !ok {
		toolName = "local_shell_call"
	}

	arguments := ""
	if rawArguments, ok := item["arguments"]; ok {
		arguments = strings.TrimSpace(string(rawArguments))
		var decoded string
		if err := json.Unmarshal(rawArguments, &decoded); err == nil {
			arguments = decoded
		}
	}

	return toolCommandItem{
		ToolName:  strings.TrimSpace(toolName),
		CallID:    strings.TrimSpace(callID),
		Command:   command,
		Arguments: arguments,
	}, true
}

func extractLocalShellCommand(item map[string]json.RawMessage) (string, bool) {
	if command, ok := extractShellCommandJSONField(item, "command"); ok {
		return command, true
	}
	if command, ok := extractShellCommandJSONField(item, "cmd"); ok {
		return command, true
	}
	if rawArguments, ok := item["arguments"]; ok {
		if command, ok := extractLocalShellCommandFromArguments(rawArguments); ok {
			return command, true
		}
	}
	if command, ok := extractNestedJSONStringField(item, "action", "command"); ok {
		return command, true
	}
	return "", false
}

func extractLocalShellCommandFromArguments(raw json.RawMessage) (string, bool) {
	var arguments string
	if err := json.Unmarshal(raw, &arguments); err == nil {
		if command, ok := extractShellCommandArgumentAtPath(arguments, "/command"); ok {
			return command, true
		}
		if command, ok := extractShellCommandArgumentAtPath(arguments, "/cmd"); ok {
			return command, true
		}
		if command, ok := extractShellCommandArgumentAtPath(arguments, "/action/command"); ok {
			return command, true
		}
		if strings.TrimSpace(arguments) != "" {
			return arguments, true
		}
		return "", false
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return "", false
	}
	if command, ok := extractShellCommandJSONField(object, "command"); ok {
		return command, true
	}
	if command, ok := extractShellCommandJSONField(object, "cmd"); ok {
		return command, true
	}
	if command, ok := extractNestedJSONStringField(object, "action", "command"); ok {
		return command, true
	}
	return "", false
}

func extractNonEmptyJSONStringField(item map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := item[key]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil || strings.TrimSpace(value) == "" {
		return "", false
	}
	return value, true
}

func extractShellCommandJSONField(item map[string]json.RawMessage, key string) (string, bool) {
	raw, ok := item[key]
	if !ok {
		return "", false
	}
	return extractShellCommandJSONValue(raw)
}

func extractShellCommandJSONValue(raw json.RawMessage) (string, bool) {
	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		if strings.TrimSpace(value) == "" {
			return "", false
		}
		return value, true
	}

	var argv []string
	if err := json.Unmarshal(raw, &argv); err == nil {
		return shellQuoteCommandArgs(argv)
	}

	return "", false
}

func extractNestedJSONStringField(item map[string]json.RawMessage, objectKey, stringKey string) (string, bool) {
	raw, ok := item[objectKey]
	if !ok {
		return "", false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return "", false
	}
	return extractShellCommandJSONField(object, stringKey)
}

func replaceShellFunctionCommand(raw json.RawMessage, replacement string, manager *ToolOptimizerManager) (json.RawMessage, bool) {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return raw, false
	}

	rawType, ok := item["type"]
	if !ok {
		return raw, false
	}
	var itemType string
	if err := json.Unmarshal(rawType, &itemType); err != nil {
		return raw, false
	}

	switch itemType {
	case "function_call":
		if _, ok := extractConfiguredShellFunctionCommand(item, manager); !ok {
			return raw, false
		}
		if !replaceShellFunctionCommandArguments(item, manager, replacement) {
			return raw, false
		}
	case "local_shell_call":
		if manager == nil || !manager.ShellFunctionCallsEnabled() {
			return raw, false
		}
		if _, ok := extractLocalShellFunctionCommand(item); !ok {
			return raw, false
		}
		if !replaceLocalShellFunctionCommand(item, replacement) {
			return raw, false
		}
	default:
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

func replaceLocalShellFunctionCommand(item map[string]json.RawMessage, replacement string) bool {
	if replaced, matched := replaceJSONStringCommandField(item, "command", replacement); matched {
		return replaced
	}
	if replaced, matched := replaceJSONStringCommandField(item, "cmd", replacement); matched {
		return replaced
	}
	if replaced, matched := replaceLocalShellCommandInArguments(item, replacement); matched {
		return replaced
	}
	if replaced, matched := replaceNestedJSONStringCommandField(item, "action", "command", replacement); matched {
		return replaced
	}
	return false
}

func replaceJSONStringCommandField(item map[string]json.RawMessage, key, replacement string) (bool, bool) {
	raw, ok := item[key]
	if !ok {
		return false, false
	}
	if _, ok := extractShellCommandJSONValue(raw); !ok {
		return false, false
	}

	var existing string
	if err := json.Unmarshal(raw, &existing); err != nil {
		return false, true
	}
	if strings.TrimSpace(existing) == "" {
		return false, false
	}

	encoded, err := json.Marshal(replacement)
	if err != nil {
		return false, true
	}
	item[key] = encoded
	return true, true
}

func replaceNestedJSONStringCommandField(item map[string]json.RawMessage, objectKey, stringKey, replacement string) (bool, bool) {
	raw, ok := item[objectKey]
	if !ok {
		return false, false
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return false, false
	}
	replaced, matched := replaceJSONStringCommandField(object, stringKey, replacement)
	if !matched {
		return false, false
	}
	if !replaced {
		return false, true
	}

	encoded, err := json.Marshal(object)
	if err != nil {
		return false, true
	}
	item[objectKey] = encoded
	return true, true
}

func replaceLocalShellCommandInArguments(item map[string]json.RawMessage, replacement string) (bool, bool) {
	raw, ok := item["arguments"]
	if !ok {
		return false, false
	}

	var arguments string
	if err := json.Unmarshal(raw, &arguments); err == nil {
		if _, ok := extractShellCommandArgumentAtPath(arguments, "/command"); ok {
			return replaceLocalShellStringArguments(item, arguments, "/command", replacement), true
		}
		if _, ok := extractShellCommandArgumentAtPath(arguments, "/cmd"); ok {
			return replaceLocalShellStringArguments(item, arguments, "/cmd", replacement), true
		}
		if _, ok := extractShellCommandArgumentAtPath(arguments, "/action/command"); ok {
			return replaceLocalShellStringArguments(item, arguments, "/action/command", replacement), true
		}
		if strings.TrimSpace(arguments) == "" {
			return false, false
		}
		encoded, err := json.Marshal(replacement)
		if err != nil {
			return false, true
		}
		item["arguments"] = encoded
		return true, true
	}

	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return false, false
	}
	if replaced, matched := replaceJSONStringCommandField(object, "command", replacement); matched {
		if !replaced {
			return false, true
		}
		return replaceLocalShellRawArguments(item, object), true
	}
	if replaced, matched := replaceJSONStringCommandField(object, "cmd", replacement); matched {
		if !replaced {
			return false, true
		}
		return replaceLocalShellRawArguments(item, object), true
	}
	if replaced, matched := replaceNestedJSONStringCommandField(object, "action", "command", replacement); matched {
		if !replaced {
			return false, true
		}
		return replaceLocalShellRawArguments(item, object), true
	}
	return false, false
}

func replaceLocalShellStringArguments(item map[string]json.RawMessage, arguments, pointer, replacement string) bool {
	newArguments, ok := replaceStringArgumentAtPath(arguments, pointer, replacement)
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

func replaceLocalShellRawArguments(item map[string]json.RawMessage, object map[string]json.RawMessage) bool {
	encoded, err := json.Marshal(object)
	if err != nil {
		return false
	}
	item["arguments"] = encoded
	return true
}

func extractFunctionCallOutputItem(raw json.RawMessage) (toolOutputItem, bool) {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(raw, &item); err != nil {
		return toolOutputItem{}, false
	}
	itemType, ok := extractNonEmptyJSONStringField(item, "type")
	if !ok || (itemType != "function_call_output" && itemType != "local_shell_call_output") {
		return toolOutputItem{}, false
	}
	callID, ok := extractNonEmptyJSONStringField(item, "call_id")
	if !ok {
		return toolOutputItem{}, false
	}
	rawOutput, ok := item["output"]
	if !ok {
		return toolOutputItem{}, false
	}
	output, ok := extractToolOutput(rawOutput)
	if !ok {
		return toolOutputItem{}, false
	}
	return toolOutputItem{CallID: strings.TrimSpace(callID), Output: output}, true
}

func extractToolOutput(raw json.RawMessage) (string, bool) {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" {
		return "", false
	}
	if trimmed == "null" {
		return "", false
	}

	var output string
	if err := json.Unmarshal(raw, &output); err == nil {
		return output, true
	}
	return trimmed, true
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
	current, ok := extractArgumentAtPath(argumentsJSON, pointer)
	if !ok {
		return "", false
	}

	command, ok := current.(string)
	if !ok {
		return "", false
	}
	return command, true
}

func extractShellCommandArgumentAtPath(argumentsJSON, pointer string) (string, bool) {
	current, ok := extractArgumentAtPath(argumentsJSON, pointer)
	if !ok {
		return "", false
	}
	return formatShellCommandValue(current)
}

func extractArgumentAtPath(argumentsJSON, pointer string) (interface{}, bool) {
	segments, ok := parseToolOptimizerJSONPointer(pointer)
	if !ok || len(segments) == 0 {
		return nil, false
	}

	var value interface{}
	decoder := json.NewDecoder(strings.NewReader(argumentsJSON))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return nil, false
	}

	current := value
	for _, segment := range segments {
		switch typed := current.(type) {
		case map[string]interface{}:
			next, ok := typed[segment]
			if !ok {
				return nil, false
			}
			current = next
		case []interface{}:
			idx, ok := parseJSONPointerArrayIndex(segment, len(typed))
			if !ok {
				return nil, false
			}
			current = typed[idx]
		default:
			return nil, false
		}
	}
	return current, true
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

func formatShellCommandValue(value interface{}) (string, bool) {
	switch typed := value.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return "", false
		}
		return typed, true
	case []interface{}:
		args := make([]string, 0, len(typed))
		for _, raw := range typed {
			arg, ok := raw.(string)
			if !ok {
				return "", false
			}
			args = append(args, arg)
		}
		return shellQuoteCommandArgs(args)
	default:
		return "", false
	}
}

func shellQuoteCommandArgs(args []string) (string, bool) {
	if len(args) == 0 {
		return "", false
	}
	quoted := make([]string, 0, len(args))
	for _, arg := range args {
		quoted = append(quoted, shellQuoteCommandArg(arg))
	}
	command := strings.TrimSpace(strings.Join(quoted, " "))
	if command == "" {
		return "", false
	}
	return command, true
}

func shellQuoteCommandArg(arg string) string {
	if arg == "" {
		return "''"
	}
	if isSafeShellToken(arg) {
		return arg
	}
	return "'" + strings.ReplaceAll(arg, "'", "'\"'\"'") + "'"
}

func isSafeShellToken(arg string) bool {
	for _, r := range arg {
		if r >= 'a' && r <= 'z' {
			continue
		}
		if r >= 'A' && r <= 'Z' {
			continue
		}
		if r >= '0' && r <= '9' {
			continue
		}
		switch r {
		case '_', '@', '%', '+', '=', ':', ',', '.', '-':
			continue
		default:
			return false
		}
	}
	return true
}
