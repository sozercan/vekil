package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/sozercan/copilot-proxy/models"
)

type geminiJSONNode struct {
	aliases        map[string]string
	allowed        map[string]struct{}
	objectChildren map[string]geminiJSONNode
	arrayChildren  map[string]geminiJSONNode
}

var geminiInlineDataNode = geminiJSONNode{
	aliases: map[string]string{
		"mime_type": "mimeType",
	},
	allowed: geminiJSONSet(
		"mimeType",
		"data",
	),
}

var geminiFunctionCallNode = geminiJSONNode{
	allowed: geminiJSONSet(
		"id",
		"name",
		"args",
	),
}

var geminiFunctionResponseNode = geminiJSONNode{
	allowed: geminiJSONSet(
		"id",
		"name",
		"response",
		"parts",
	),
}

var geminiPartNode = geminiJSONNode{
	aliases: map[string]string{
		"function_call":         "functionCall",
		"function_response":     "functionResponse",
		"inline_data":           "inlineData",
		"file_data":             "fileData",
		"executable_code":       "executableCode",
		"code_execution_result": "codeExecutionResult",
	},
	allowed: geminiJSONSet(
		"text",
		"functionCall",
		"functionResponse",
		"inlineData",
		"fileData",
		"executableCode",
		"codeExecutionResult",
	),
	objectChildren: map[string]geminiJSONNode{
		"functionCall":     geminiFunctionCallNode,
		"functionResponse": geminiFunctionResponseNode,
		"inlineData":       geminiInlineDataNode,
	},
}

var geminiContentNode = geminiJSONNode{
	allowed: geminiJSONSet(
		"role",
		"parts",
	),
	arrayChildren: map[string]geminiJSONNode{
		"parts": geminiPartNode,
	},
}

var geminiFunctionDeclarationNode = geminiJSONNode{
	aliases: map[string]string{
		"parameters_json_schema": "parametersJsonSchema",
	},
	allowed: geminiJSONSet(
		"name",
		"description",
		"parameters",
		"parametersJsonSchema",
	),
}

var geminiToolNode = geminiJSONNode{
	aliases: map[string]string{
		"function_declarations":   "functionDeclarations",
		"google_search":           "googleSearch",
		"google_search_retrieval": "googleSearchRetrieval",
		"url_context":             "urlContext",
		"code_execution":          "codeExecution",
		"google_maps":             "googleMaps",
		"computer_use":            "computerUse",
		"enterprise_web_search":   "enterpriseWebSearch",
	},
	allowed: geminiJSONSet(
		"functionDeclarations",
		"googleSearch",
		"googleSearchRetrieval",
		"urlContext",
		"codeExecution",
		"googleMaps",
		"computerUse",
		"enterpriseWebSearch",
	),
	arrayChildren: map[string]geminiJSONNode{
		"functionDeclarations": geminiFunctionDeclarationNode,
	},
}

var geminiFunctionCallingConfigNode = geminiJSONNode{
	aliases: map[string]string{
		"allowed_function_names": "allowedFunctionNames",
	},
	allowed: geminiJSONSet(
		"mode",
		"allowedFunctionNames",
	),
}

var geminiToolConfigNode = geminiJSONNode{
	aliases: map[string]string{
		"function_calling_config": "functionCallingConfig",
	},
	allowed: geminiJSONSet(
		"functionCallingConfig",
	),
	objectChildren: map[string]geminiJSONNode{
		"functionCallingConfig": geminiFunctionCallingConfigNode,
	},
}

var geminiThinkingConfigNode = geminiJSONNode{
	aliases: map[string]string{
		"include_thoughts": "includeThoughts",
		"thinking_budget":  "thinkingBudget",
		"thinking_level":   "thinkingLevel",
	},
	allowed: geminiJSONSet(
		"includeThoughts",
		"thinkingBudget",
		"thinkingLevel",
	),
}

var geminiGenerationConfigNode = geminiJSONNode{
	aliases: map[string]string{
		"top_p":                "topP",
		"top_k":                "topK",
		"max_output_tokens":    "maxOutputTokens",
		"stop_sequences":       "stopSequences",
		"response_mime_type":   "responseMimeType",
		"response_schema":      "responseSchema",
		"response_json_schema": "responseJsonSchema",
		"thinking_config":      "thinkingConfig",
		"candidate_count":      "candidateCount",
		"presence_penalty":     "presencePenalty",
		"frequency_penalty":    "frequencyPenalty",
		"response_modalities":  "responseModalities",
		"speech_config":        "speechConfig",
		"image_config":         "imageConfig",
		"media_resolution":     "mediaResolution",
		"response_logprobs":    "responseLogprobs",
	},
	allowed: geminiJSONSet(
		"temperature",
		"topP",
		"topK",
		"maxOutputTokens",
		"stopSequences",
		"responseMimeType",
		"responseSchema",
		"responseJsonSchema",
		"thinkingConfig",
		"candidateCount",
		"presencePenalty",
		"frequencyPenalty",
		"seed",
		"responseModalities",
		"speechConfig",
		"imageConfig",
		"mediaResolution",
		"responseLogprobs",
		"logprobs",
	),
	objectChildren: map[string]geminiJSONNode{
		"thinkingConfig": geminiThinkingConfigNode,
	},
}

var geminiRequestNode = geminiJSONNode{
	aliases: map[string]string{
		"system_instruction": "systemInstruction",
		"tool_config":        "toolConfig",
		"generation_config":  "generationConfig",
		"cached_content":     "cachedContent",
		"safety_settings":    "safetySettings",
	},
	allowed: geminiJSONSet(
		"model",
		"systemInstruction",
		"contents",
		"tools",
		"toolConfig",
		"generationConfig",
		"cachedContent",
		"safetySettings",
	),
	objectChildren: map[string]geminiJSONNode{
		"systemInstruction": geminiContentNode,
		"toolConfig":        geminiToolConfigNode,
		"generationConfig":  geminiGenerationConfigNode,
	},
	arrayChildren: map[string]geminiJSONNode{
		"contents": geminiContentNode,
		"tools":    geminiToolNode,
	},
}

func decodeGeminiGenerateContentRequest(body []byte) (*models.GeminiGenerateContentRequest, error) {
	normalized, err := normalizeGeminiJSON(body, geminiRequestNode, "request", false)
	if err != nil {
		return nil, err
	}

	var req models.GeminiGenerateContentRequest
	if err := json.Unmarshal(normalized, &req); err != nil {
		return nil, invalidGeminiArgument("invalid JSON in request body")
	}

	return &req, nil
}

func decodeGeminiCountTokensRequest(body []byte) (*models.GeminiCountTokensRequest, error) {
	return decodeGeminiGenerateContentRequest(body)
}

func normalizeGeminiJSON(data []byte, node geminiJSONNode, path string, allowNull bool) ([]byte, error) {
	if isNullJSON(data) {
		if allowNull {
			return []byte("null"), nil
		}
		return nil, invalidGeminiArgument("%s must be a JSON object", path)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, invalidGeminiArgument("%s must be a JSON object", path)
	}

	normalized := make(map[string]json.RawMessage, len(raw))
	for key, value := range raw {
		canonicalKey := key
		if alias, ok := node.aliases[key]; ok {
			canonicalKey = alias
		}

		if _, ok := node.allowed[canonicalKey]; !ok {
			return nil, invalidGeminiArgument("%s.%s is not supported or unknown", path, key)
		}
		if _, exists := normalized[canonicalKey]; exists {
			return nil, invalidGeminiArgument("%s contains duplicate field %q", path, canonicalKey)
		}

		if child, ok := node.objectChildren[canonicalKey]; ok {
			childJSON, err := normalizeGeminiJSON(value, child, path+"."+canonicalKey, true)
			if err != nil {
				return nil, err
			}
			normalized[canonicalKey] = json.RawMessage(childJSON)
			continue
		}
		if child, ok := node.arrayChildren[canonicalKey]; ok {
			childJSON, err := normalizeGeminiJSONArray(value, child, path+"."+canonicalKey)
			if err != nil {
				return nil, err
			}
			normalized[canonicalKey] = json.RawMessage(childJSON)
			continue
		}

		normalized[canonicalKey] = value
	}

	out, err := json.Marshal(normalized)
	if err != nil {
		return nil, fmt.Errorf("failed to normalize %s: %w", path, err)
	}

	return out, nil
}

func normalizeGeminiJSONArray(data []byte, node geminiJSONNode, path string) ([]byte, error) {
	if isNullJSON(data) {
		return []byte("null"), nil
	}

	var items []json.RawMessage
	if err := json.Unmarshal(data, &items); err != nil {
		return nil, invalidGeminiArgument("%s must be a JSON array", path)
	}

	for idx, item := range items {
		normalized, err := normalizeGeminiJSON(item, node, fmt.Sprintf("%s[%d]", path, idx), false)
		if err != nil {
			return nil, err
		}
		items[idx] = json.RawMessage(normalized)
	}

	out, err := json.Marshal(items)
	if err != nil {
		return nil, fmt.Errorf("failed to normalize %s: %w", path, err)
	}

	return out, nil
}

func geminiJSONSet(keys ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		set[key] = struct{}{}
	}
	return set
}

func isNullJSON(data []byte) bool {
	trimmed := bytes.TrimSpace(data)
	return len(trimmed) > 0 && bytes.Equal(trimmed, []byte("null"))
}
