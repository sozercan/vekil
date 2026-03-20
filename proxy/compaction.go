package proxy

import (
	"encoding/base64"
	"encoding/json"
	"strings"
)

type syntheticCompactionPayload struct {
	Summary string `json:"summary"`
}

const proxyCompactionContextIntro = "You are resuming an interrupted assistant turn from a context checkpoint."

func encodeSyntheticCompaction(summary string) string {
	payload, err := json.Marshal(syntheticCompactionPayload{Summary: summary})
	if err != nil {
		return syntheticCompactionPrefix
	}
	return syntheticCompactionPrefix + base64.RawURLEncoding.EncodeToString(payload)
}

func rewriteSyntheticCompactionRequest(body []byte) ([]byte, int) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return body, 0
	}

	rawInput, ok := req["input"]
	if !ok {
		return body, 0
	}

	var input interface{}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return body, 0
	}

	rewrittenInput, rewriteCount := rewriteSyntheticCompactionValue(input)
	if rewriteCount == 0 {
		return body, 0
	}

	encodedInput, err := json.Marshal(rewrittenInput)
	if err != nil {
		return body, 0
	}
	req["input"] = encodedInput

	rewrittenBody, err := json.Marshal(req)
	if err != nil {
		return body, 0
	}
	return rewrittenBody, rewriteCount
}

// When a compacted checkpoint is restored without a remaining user turn, add a
// small synthetic user prompt so the upstream model resumes the interrupted
// task instead of replying with a generic "what should I work on next?".
func injectSyntheticCompactionResumePrompt(body []byte) ([]byte, bool) {
	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return body, false
	}

	rawInput, ok := req["input"]
	if !ok {
		return body, false
	}

	var input interface{}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return body, false
	}

	inputItems, ok := input.([]interface{})
	if !ok {
		return body, false
	}
	if !shouldInjectSyntheticCompactionResumePrompt(inputItems) {
		return body, false
	}

	inputItems = append(inputItems, proxyCompactionResumeMessage())
	encodedInput, err := json.Marshal(inputItems)
	if err != nil {
		return body, false
	}
	req["input"] = encodedInput

	rewrittenBody, err := json.Marshal(req)
	if err != nil {
		return body, false
	}
	return rewrittenBody, true
}

func shouldInjectSyntheticCompactionResumePrompt(inputItems []interface{}) bool {
	lastCheckpointIdx := -1
	for i, item := range inputItems {
		if isProxyCompactionContextMessage(item) {
			lastCheckpointIdx = i
		}
	}

	if lastCheckpointIdx == -1 {
		return !inputHasMessageRole(inputItems, "user")
	}

	for _, item := range inputItems[lastCheckpointIdx+1:] {
		if messageHasRole(item, "user") {
			return false
		}
	}
	return true
}

func rewriteSyntheticCompactionValue(v interface{}) (interface{}, int) {
	switch typed := v.(type) {
	case []interface{}:
		rewritten := make([]interface{}, 0, len(typed))
		total := 0
		for _, item := range typed {
			next, count := rewriteSyntheticCompactionValue(item)
			total += count
			rewritten = append(rewritten, next)
		}
		return rewritten, total

	case map[string]interface{}:
		if itemType, _ := typed["type"].(string); itemType == "compaction" {
			if encryptedContent, _ := typed["encrypted_content"].(string); encryptedContent != "" {
				if summary, ok := extractSyntheticOrLegacyCompactionSummary(encryptedContent); ok {
					return proxyCompactionContextMessage(summary), 1
				}
			}
		}

		rewritten := make(map[string]interface{}, len(typed))
		total := 0
		for key, value := range typed {
			next, count := rewriteSyntheticCompactionValue(value)
			total += count
			rewritten[key] = next
		}
		return rewritten, total
	default:
		return v, 0
	}
}

func inputHasMessageRole(v interface{}, role string) bool {
	switch typed := v.(type) {
	case []interface{}:
		for _, item := range typed {
			if inputHasMessageRole(item, role) {
				return true
			}
		}
	case map[string]interface{}:
		if itemType, _ := typed["type"].(string); itemType == "message" {
			if messageRole, _ := typed["role"].(string); messageRole == role {
				return true
			}
		}
		for _, value := range typed {
			if inputHasMessageRole(value, role) {
				return true
			}
		}
	}
	return false
}

func messageHasRole(v interface{}, role string) bool {
	typed, ok := v.(map[string]interface{})
	if !ok {
		return false
	}
	if itemType, _ := typed["type"].(string); itemType != "message" {
		return false
	}
	messageRole, _ := typed["role"].(string)
	return messageRole == role
}

func isProxyCompactionContextMessage(v interface{}) bool {
	typed, ok := v.(map[string]interface{})
	if !ok || !messageHasRole(v, "developer") {
		return false
	}

	content, ok := typed["content"].([]interface{})
	if !ok || len(content) == 0 {
		return false
	}

	firstPart, ok := content[0].(map[string]interface{})
	if !ok {
		return false
	}
	if partType, _ := firstPart["type"].(string); partType != "input_text" {
		return false
	}
	text, _ := firstPart["text"].(string)
	return strings.HasPrefix(text, proxyCompactionContextIntro)
}

func extractSyntheticOrLegacyCompactionSummary(encryptedContent string) (string, bool) {
	if strings.HasPrefix(encryptedContent, syntheticCompactionPrefix) {
		raw := strings.TrimPrefix(encryptedContent, syntheticCompactionPrefix)
		payloadBytes, err := base64.RawURLEncoding.DecodeString(raw)
		if err != nil {
			return "", false
		}

		var payload syntheticCompactionPayload
		if err := json.Unmarshal(payloadBytes, &payload); err != nil {
			return "", false
		}
		return payload.Summary, true
	}

	// Legacy fallback: older proxy versions wrote plaintext summaries directly
	// into encrypted_content. Real upstream tokens are opaque, space-free blobs.
	if encryptedContent != "" && (!looksOpaqueCompactionToken(encryptedContent) || strings.ContainsAny(encryptedContent, " \t\r\n")) {
		return encryptedContent, true
	}
	return "", false
}

func looksOpaqueCompactionToken(token string) bool {
	if len(token) < 32 {
		return false
	}
	for _, r := range token {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case r == '-', r == '_', r == '=', r == '.':
		default:
			return false
		}
	}
	return true
}

func proxyCompactionContextMessage(summary string) map[string]interface{} {
	summary = sanitizeProxySummaryText(summary)
	text := proxyCompactionContextIntro + " This checkpoint is the active working state for the same conversation, not passive background history.\n\nResume behavior:\n- Continue the same task immediately from this checkpoint.\n- Treat the checkpoint as authoritative for prior progress, constraints, and next steps.\n- Do not ask the user what to work on next unless the checkpoint explicitly says the assistant was blocked waiting for user input or that the task is complete.\n\nCheckpoint summary:\n" + summary
	return map[string]interface{}{
		"type": "message",
		"role": "developer",
		"content": []interface{}{
			map[string]interface{}{
				"type": "input_text",
				"text": text,
			},
		},
	}
}

func proxyCompactionContextRawMessage(summary string) (json.RawMessage, error) {
	encoded, err := json.Marshal(proxyCompactionContextMessage(summary))
	if err != nil {
		return nil, err
	}
	return json.RawMessage(encoded), nil
}

func proxyCompactionResumeMessage() map[string]interface{} {
	return map[string]interface{}{
		"type": "message",
		"role": "user",
		"content": []interface{}{
			map[string]interface{}{
				"type": "input_text",
				"text": "Continue from the checkpoint above and resume the interrupted task from the next unfinished step. Do not ask for a new assignment unless the checkpoint says you were blocked waiting for user input or the work is already complete.",
			},
		},
	}
}
