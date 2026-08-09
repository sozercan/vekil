package proxy

import (
	"bytes"
	"encoding/json"
	"strings"
)

const (
	responsesAdditionalToolsType                = "additional_tools"
	responsesNamespaceToolType                  = "namespace"
	responsesNamespaceDescriptionNameLimitBytes = 256
)

func normalizeResponsesAdditionalToolsNamespaceDescriptions(bodyBytes []byte) ([]byte, int) {
	if !bytes.Contains(bodyBytes, []byte(responsesAdditionalToolsType)) && !bytes.Contains(bodyBytes, []byte(`\u`)) {
		return bodyBytes, 0
	}

	var request struct {
		Input json.RawMessage `json:"input"`
	}
	if err := json.Unmarshal(bodyBytes, &request); err != nil || len(request.Input) == 0 {
		return bodyBytes, 0
	}

	var input []json.RawMessage
	if err := json.Unmarshal(request.Input, &input); err != nil {
		return bodyBytes, 0
	}

	normalizedCount := 0
	for i, rawItem := range input {
		if responsesInputItemType(rawItem) != responsesAdditionalToolsType {
			continue
		}
		rewrittenItem, itemCount := normalizeResponsesAdditionalToolsItemNamespaceDescriptions(rawItem)
		if itemCount == 0 {
			continue
		}
		input[i] = rewrittenItem
		normalizedCount += itemCount
	}
	if normalizedCount == 0 {
		return bodyBytes, 0
	}

	rewrittenInput, err := json.Marshal(input)
	if err != nil {
		return bodyBytes, 0
	}
	rewrittenBody, ok := replaceTopLevelRawJSONField(bodyBytes, "input", rewrittenInput)
	if !ok {
		return bodyBytes, 0
	}
	return rewrittenBody, normalizedCount
}

func normalizeResponsesAdditionalToolsItemNamespaceDescriptions(rawItem json.RawMessage) (json.RawMessage, int) {
	var item map[string]json.RawMessage
	if err := json.Unmarshal(rawItem, &item); err != nil {
		return rawItem, 0
	}

	var tools []json.RawMessage
	if err := json.Unmarshal(item["tools"], &tools); err != nil {
		return rawItem, 0
	}

	normalizedCount := 0
	for i, rawTool := range tools {
		if responsesToolType(rawTool) != responsesNamespaceToolType {
			continue
		}

		var tool map[string]json.RawMessage
		if err := json.Unmarshal(rawTool, &tool); err != nil {
			continue
		}
		rawDescription, ok := tool["description"]
		if !ok {
			continue
		}
		var description *string
		if err := json.Unmarshal(rawDescription, &description); err != nil ||
			description == nil ||
			strings.TrimSpace(*description) != "" {
			continue
		}

		normalizedDescription, err := json.Marshal(responsesAdditionalToolsNamespaceDescription(tool["name"]))
		if err != nil {
			continue
		}
		tool["description"] = normalizedDescription
		rewrittenTool, err := json.Marshal(tool)
		if err != nil {
			continue
		}
		tools[i] = rewrittenTool
		normalizedCount++
	}
	if normalizedCount == 0 {
		return rawItem, 0
	}

	rewrittenTools, err := json.Marshal(tools)
	if err != nil {
		return rawItem, 0
	}
	item["tools"] = rewrittenTools
	rewrittenItem, err := json.Marshal(item)
	if err != nil {
		return rawItem, 0
	}
	return rewrittenItem, normalizedCount
}

func responsesAdditionalToolsNamespaceDescription(rawName json.RawMessage) string {
	var name string
	if err := json.Unmarshal(rawName, &name); err == nil {
		name = strings.TrimSpace(name)
		if name != "" && len(name) <= responsesNamespaceDescriptionNameLimitBytes {
			return "Tools in the " + name + " namespace."
		}
	}
	return "Tools in this namespace."
}
