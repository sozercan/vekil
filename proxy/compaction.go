package proxy

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"
)

type syntheticCompactionPayload struct {
	Summary string `json:"summary"`
}

const (
	proxyCompactionContextIntro         = "You are resuming an interrupted assistant turn from a context checkpoint."
	syntheticCompactionResponseIDPrefix = "resp-vekil-compact-"
)

func encodeSyntheticCompaction(summary string) string {
	payload, err := json.Marshal(syntheticCompactionPayload{Summary: summary})
	if err != nil {
		return syntheticCompactionPrefix
	}
	return syntheticCompactionPrefix + base64.RawURLEncoding.EncodeToString(payload)
}

func rewriteSyntheticCompactionRequest(body []byte) ([]byte, int) {
	if !responsesBodyMayContainSyntheticCompaction(body) {
		return body, 0
	}

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

// resetSyntheticCompactionResponseLineage removes server-side continuation
// state that points at a proxy-generated compaction response. Those synthetic
// response IDs exist only at Vekil's public surface and can never be resolved
// by the upstream Responses API. The reset is intentionally narrow: an ID with
// the proxy prefix is removed only when the same request also carries a
// proxy-owned compaction checkpoint that Vekil can expand into normal context.
func resetSyntheticCompactionResponseLineage(body []byte) ([]byte, bool) {
	if !bytes.Contains(body, []byte(syntheticCompactionResponseIDPrefix)) || !responsesBodyMayContainSyntheticCompaction(body) {
		return body, false
	}

	var req map[string]json.RawMessage
	if err := json.Unmarshal(body, &req); err != nil {
		return body, false
	}

	var previousResponseID string
	if err := json.Unmarshal(req["previous_response_id"], &previousResponseID); err != nil {
		return body, false
	}
	if !strings.HasPrefix(strings.TrimSpace(previousResponseID), syntheticCompactionResponseIDPrefix) {
		return body, false
	}

	var input interface{}
	if err := json.Unmarshal(req["input"], &input); err != nil {
		return body, false
	}
	if _, rewriteCount := rewriteSyntheticCompactionValue(input); rewriteCount == 0 {
		return body, false
	}

	delete(req, "previous_response_id")
	rewrittenBody, err := json.Marshal(req)
	if err != nil {
		return body, false
	}
	return rewrittenBody, true
}

func responsesBodyMayContainSyntheticCompaction(body []byte) bool {
	return bytes.Contains(body, []byte(syntheticCompactionPrefix)) ||
		bytes.Contains(body, []byte(legacySyntheticCompactionPrefix)) ||
		responsesBodyMayContainCompactionToken(body)
}

func responsesBodyMayContainContextCompaction(body []byte) bool {
	return responsesBodyMayContainCompactionToken(body)
}

func responsesBodyMayContainCompactionToken(body []byte) bool {
	// Fast-path ordinary requests that cannot contain proxy-owned compaction
	// items. If the body contains any JSON unicode escapes, fall back to the
	// legacy parser so escaped marker strings keep their previous behavior.
	return bytes.Contains(body, []byte("compaction")) || bytes.Contains(body, []byte(`\u`))
}

func rewriteSyntheticCompactionRequestFields(requestFields map[string]json.RawMessage) (map[string]json.RawMessage, int) {
	rawInput, ok := requestFields["input"]
	if !ok {
		return requestFields, 0
	}

	var input interface{}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return requestFields, 0
	}

	rewrittenInput, rewriteCount := rewriteSyntheticCompactionValue(input)
	if rewriteCount == 0 {
		return requestFields, 0
	}

	encodedInput, err := json.Marshal(rewrittenInput)
	if err != nil {
		return requestFields, 0
	}

	rewrittenFields := make(map[string]json.RawMessage, len(requestFields))
	for key, value := range requestFields {
		rewrittenFields[key] = value
	}
	rewrittenFields["input"] = encodedInput
	return rewrittenFields, rewriteCount
}

func sanitizeContextCompactionRequest(body []byte) ([]byte, int) {
	if !responsesBodyMayContainContextCompaction(body) {
		return body, 0
	}

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

	sanitizedInput, sanitizeCount := sanitizeContextCompactionValue(input)
	if sanitizeCount == 0 {
		return body, 0
	}

	encodedInput, err := json.Marshal(sanitizedInput)
	if err != nil {
		return body, 0
	}
	req["input"] = encodedInput

	sanitizedBody, err := json.Marshal(req)
	if err != nil {
		return body, 0
	}
	return sanitizedBody, sanitizeCount
}

func sanitizeContextCompactionRequestFields(requestFields map[string]json.RawMessage) (map[string]json.RawMessage, int) {
	rawInput, ok := requestFields["input"]
	if !ok {
		return requestFields, 0
	}

	var input interface{}
	if err := json.Unmarshal(rawInput, &input); err != nil {
		return requestFields, 0
	}

	sanitizedInput, sanitizeCount := sanitizeContextCompactionValue(input)
	if sanitizeCount == 0 {
		return requestFields, 0
	}

	encodedInput, err := json.Marshal(sanitizedInput)
	if err != nil {
		return requestFields, 0
	}

	sanitizedFields := make(map[string]json.RawMessage, len(requestFields))
	for key, value := range requestFields {
		sanitizedFields[key] = value
	}
	sanitizedFields["input"] = encodedInput
	return sanitizedFields, sanitizeCount
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

func sanitizeContextCompactionValue(v interface{}) (interface{}, int) {
	switch typed := v.(type) {
	case []interface{}:
		sanitized := make([]interface{}, 0, len(typed))
		total := 0
		for _, item := range typed {
			next, count := sanitizeContextCompactionValue(item)
			total += count
			sanitized = append(sanitized, next)
		}
		return sanitized, total

	case map[string]interface{}:
		if itemType, _ := typed["type"].(string); itemType == "context_compaction" {
			if summary, ok := extractContextCompactionSummary(typed); ok {
				return proxyCompactionContextMessage(summary), 1
			}
			return v, 0
		}

		sanitized := make(map[string]interface{}, len(typed))
		total := 0
		for key, value := range typed {
			next, count := sanitizeContextCompactionValue(value)
			total += count
			sanitized[key] = next
		}
		return sanitized, total
	default:
		return v, 0
	}
}

var contextCompactionSummaryFields = []string{"summary", "text", "content", "checkpoint_summary", "checkpoint", "encrypted_content"}

func extractContextCompactionSummary(item map[string]interface{}) (string, bool) {
	return extractContextCompactionSummaryFromObject(item)
}

func extractContextCompactionSummaryValue(v interface{}) (string, bool) {
	switch typed := v.(type) {
	case string:
		if strings.TrimSpace(typed) == "" {
			return "", false
		}
		return typed, true
	case []interface{}:
		return extractContextCompactionSummaryFromArray(typed)
	case map[string]interface{}:
		return extractContextCompactionSummaryFromObject(typed)
	default:
		return "", false
	}
}

func extractContextCompactionSummaryFromArray(items []interface{}) (string, bool) {
	parts := make([]string, 0, len(items))
	for _, item := range items {
		part, ok := extractContextCompactionSummaryValue(item)
		if !ok {
			continue
		}
		if strings.TrimSpace(part) == "" {
			continue
		}
		parts = append(parts, part)
	}
	summary := strings.TrimSpace(strings.Join(parts, "\n"))
	if summary == "" {
		return "", false
	}
	return summary, true
}

func extractContextCompactionSummaryFromObject(item map[string]interface{}) (string, bool) {
	for _, field := range contextCompactionSummaryFields {
		value, ok := item[field]
		if !ok {
			continue
		}

		if field == "encrypted_content" {
			encryptedContent, _ := value.(string)
			if encryptedContent == "" {
				continue
			}
			if summary, ok := extractSyntheticOrLegacyCompactionSummary(encryptedContent); ok && strings.TrimSpace(summary) != "" {
				return summary, true
			}
			continue
		}

		if summary, ok := extractContextCompactionSummaryValue(value); ok {
			return summary, true
		}
	}
	return "", false
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
	for _, prefix := range []string{syntheticCompactionPrefix, legacySyntheticCompactionPrefix} {
		if !strings.HasPrefix(encryptedContent, prefix) {
			continue
		}

		raw := strings.TrimPrefix(encryptedContent, prefix)
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
	if isLikelyLegacyPlaintextCompactionSummary(encryptedContent) {
		return encryptedContent, true
	}
	return "", false
}

func isLikelyLegacyPlaintextCompactionSummary(encryptedContent string) bool {
	encryptedContent = strings.TrimSpace(encryptedContent)
	return encryptedContent != "" && strings.ContainsAny(encryptedContent, " \t\r\n")
}

func proxyCompactionContextMessage(summary string) map[string]interface{} {
	summary = sanitizeProxySummaryText(summary)
	text := proxyCompactionContextIntro + " This checkpoint summarizes earlier conversation state for continuity; it is not a new user request.\n\nCheckpoint handling:\n- Use the summary for prior facts, constraints, decisions, files, and unfinished work.\n- Messages after this checkpoint are the active request and take precedence over any next steps or conclusions in the checkpoint.\n- Continue work from the checkpoint only when no later user message gives a different instruction; if a synthetic resume prompt follows, follow that prompt.\n\nCheckpoint summary:\n" + summary
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
