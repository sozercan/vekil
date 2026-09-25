package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Responses input item types a function-tools-only upstream converts into its
// chat history. LocalAI drops any other item type without an error, which would
// silently remove turns from the conversation the model sees.
var functionToolsOnlyResponsesInputItems = map[string]struct{}{
	"message":              {},
	"function_call":        {},
	"function_call_output": {},
	"reasoning":            {},
	"item_reference":       {},
}

// validateFunctionToolsOnlyResponsesRequest rejects Responses tools and input
// items that a provider configured with responses_function_tools_only would
// drop instead of refusing.
func validateFunctionToolsOnlyResponsesRequest(body []byte, providerID string) error {
	var request struct {
		Tools      []json.RawMessage `json:"tools"`
		ToolChoice json.RawMessage   `json:"tool_choice"`
		Input      json.RawMessage   `json:"input"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		// Malformed bodies are rejected by the ordinary request path.
		return nil
	}
	for index, rawTool := range request.Tools {
		var tool struct {
			Type string `json:"type"`
		}
		if err := json.Unmarshal(rawTool, &tool); err != nil {
			return functionToolsOnlyError(providerID, fmt.Sprintf("tools[%d]", index), "unsupported_tool_type", "is not a tool object")
		}
		// Namespaces of function tools are flattened before the request is sent.
		if tool.Type != "function" && tool.Type != "namespace" {
			return functionToolsOnlyError(providerID, fmt.Sprintf("tools[%d].type", index), "unsupported_tool_type",
				fmt.Sprintf("%q is not supported; only function tools are", tool.Type))
		}
	}
	if choice := bytes.TrimSpace(request.ToolChoice); len(choice) > 0 && choice[0] == '{' {
		var toolChoice struct {
			Type  string `json:"type"`
			Tools []struct {
				Type string `json:"type"`
			} `json:"tools"`
		}
		if err := json.Unmarshal(choice, &toolChoice); err == nil {
			if toolChoice.Type != "function" && toolChoice.Type != "allowed_tools" {
				return functionToolsOnlyError(providerID, "tool_choice.type", "unsupported_tool_type",
					fmt.Sprintf("%q is not supported; only function tool choices are", toolChoice.Type))
			}
			for index, allowed := range toolChoice.Tools {
				if allowed.Type != "function" {
					return functionToolsOnlyError(providerID, fmt.Sprintf("tool_choice.tools[%d].type", index), "unsupported_tool_type",
						fmt.Sprintf("%q is not supported; only function tools are", allowed.Type))
				}
			}
		}
	}
	input := bytes.TrimSpace(request.Input)
	if len(input) == 0 || input[0] != '[' {
		return nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal(input, &items); err != nil {
		return nil
	}
	for index, rawItem := range items {
		var item struct {
			Type string          `json:"type"`
			Role json.RawMessage `json:"role"`
		}
		if err := json.Unmarshal(rawItem, &item); err != nil {
			return functionToolsOnlyError(providerID, fmt.Sprintf("input[%d]", index), "unsupported_input_item", "is not an input item object")
		}
		itemType := strings.TrimSpace(item.Type)
		if itemType == "" && len(item.Role) > 0 {
			itemType = "message"
		}
		if _, ok := functionToolsOnlyResponsesInputItems[itemType]; !ok {
			return functionToolsOnlyError(providerID, fmt.Sprintf("input[%d].type", index), "unsupported_input_item",
				fmt.Sprintf("%q is not supported; only message, function_call, function_call_output, reasoning, and item_reference items are", itemType))
		}
	}
	return nil
}

func functionToolsOnlyError(providerID, param, code, detail string) error {
	return &providerRequestError{
		statusCode: http.StatusBadRequest,
		code:       code,
		err:        fmt.Errorf("provider %q accepts function tools only: %s %s", providerID, param, detail),
	}
}
