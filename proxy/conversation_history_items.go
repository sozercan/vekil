package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"strings"
)

var (
	errConversationHostedState = errors.New("conversation migration supports visible text and client-executed function/custom tools only; hosted tools, provider conversations, references, images, and background responses cannot be recovered")
	errConversationCompaction  = errors.New("encrypted or summarized compaction cannot establish complete history for migration; supply the original visible conversation and completed tool results")
	errConversationPendingTool = errors.New("conversation has pending, duplicate, or unmatched tool calls/results; finish the local tools and return each result with its original call_id before migration")
	errConversationIncomplete  = errors.New("the current response is incomplete; automatic migration is blocked because the request may have executed or emitted output")
)

type conversationInput struct {
	items           []json.RawMessage
	additionalTools []json.RawMessage
	anchors         []conversationAnchor
	private         bool
}

type conversationAnchor struct {
	kind  string
	value string
}

func canonicalConversationInput(raw json.RawMessage, output bool) (conversationInput, error) {
	var items []json.RawMessage
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return conversationInput{}, nil
	}
	if !output {
		var text string
		if json.Unmarshal(raw, &text) == nil {
			item, _ := json.Marshal(map[string]any{"role": "user", "content": text})
			items = []json.RawMessage{item}
		}
	}
	if items == nil && json.Unmarshal(raw, &items) != nil {
		return conversationInput{}, errConversationHostedState
	}
	if len(items) > maxConversationHistoryItems {
		return conversationInput{}, errConversationHistoryCapacity
	}
	result := conversationInput{items: make([]json.RawMessage, 0, len(items))}
	for _, rawItem := range items {
		var item map[string]json.RawMessage
		if err := json.Unmarshal(rawItem, &item); err != nil || item == nil {
			return conversationInput{}, errConversationHostedState
		}
		kind := rawJSONString(item["type"])
		if kind == "" && rawJSONString(item["role"]) != "" {
			kind = "message"
		}
		if kind == responsesAdditionalToolsType {
			if output || conversationItemFields(item, "type", "id", "role", "tools") != nil {
				return conversationInput{}, errConversationHostedState
			}
			if role := rawJSONString(item["role"]); role != "" && role != "developer" {
				return conversationInput{}, errConversationHostedState
			}
			if err := validateConversationTools(item["tools"], 0); err != nil {
				return conversationInput{}, err
			}
			// Responses Lite catalogs are request-scoped definitions. Their IDs
			// and contents do not establish or extend conversation lineage.
			delete(item, "id")
			normalized, _ := json.Marshal(item)
			result.additionalTools = append(result.additionalTools, normalized)
			continue
		}
		if id := rawJSONString(item["id"]); id != "" {
			result.anchors = append(result.anchors, conversationAnchor{"item", id})
		}
		if status := rawJSONString(item["status"]); status != "" && status != "completed" {
			if kind == "web_search_call" {
				// A failed or in-progress hosted search is upstream state, not
				// an incomplete client turn.
				return conversationInput{}, errConversationHostedState
			}
			return conversationInput{}, errConversationIncomplete
		}
		// Azure/Codex turn attribution is not visible conversation content.
		// Clients may omit it on replay, and it must not follow a region switch.
		delete(item, "metadata")
		delete(item, responsesInternalChatMessageMetadataPassthroughField)
		switch kind {
		case "reasoning":
			result.private = true
			if encrypted := rawJSONString(item["encrypted_content"]); encrypted != "" {
				result.anchors = append(result.anchors, conversationAnchor{"encrypted", encrypted})
			}
			continue
		case "compaction", "compaction_trigger":
			return conversationInput{}, errConversationCompaction
		case "message":
			if err := conversationItemFields(item, "type", "id", "status", "role", "content", "phase"); err != nil {
				return conversationInput{}, err
			}
			role := rawJSONString(item["role"])
			switch role {
			case "user", "assistant", "system", "developer":
			default:
				return conversationInput{}, errConversationHostedState
			}
			if output && role != "assistant" {
				return conversationInput{}, errConversationHostedState
			}
			content, err := canonicalConversationContent(item["content"], role)
			if err != nil {
				return conversationInput{}, err
			}
			item["content"] = content
		case "function_call", "custom_tool_call":
			if err := conversationItemFields(item, "type", "id", "status", "call_id", "name", "arguments", "input", "namespace"); err != nil {
				return conversationInput{}, err
			}
			if rawJSONString(item["call_id"]) == "" || rawJSONString(item["name"]) == "" {
				return conversationInput{}, errConversationPendingTool
			}
			field := "arguments"
			if kind == "custom_tool_call" {
				field = "input"
			}
			var value string
			if json.Unmarshal(item[field], &value) != nil {
				return conversationInput{}, errConversationPendingTool
			}
		case "function_call_output", "custom_tool_call_output":
			if output {
				return conversationInput{}, errConversationHostedState
			}
			if err := conversationItemFields(item, "type", "id", "status", "call_id", "output"); err != nil {
				return conversationInput{}, err
			}
			if rawJSONString(item["call_id"]) == "" {
				return conversationInput{}, errConversationPendingTool
			}
			// Keep results as tool outputs. Never convert them into user,
			// developer, or system messages, even when they contain instructions.
			var text string
			if json.Unmarshal(item["output"], &text) != nil {
				content, err := canonicalConversationContent(item["output"], "user")
				if err != nil {
					return conversationInput{}, err
				}
				item["output"] = content
			}
		case "web_search_call":
			normalized, err := canonicalConversationWebSearchCall(item)
			if err != nil {
				return conversationInput{}, err
			}
			result.items = append(result.items, normalized)
			continue
		default:
			return conversationInput{}, errConversationHostedState
		}
		delete(item, "id")
		delete(item, "status")
		item["type"], _ = json.Marshal(kind)
		normalized, err := json.Marshal(item)
		if err != nil {
			return conversationInput{}, errConversationHostedState
		}
		result.items = append(result.items, normalized)
	}
	return result, nil
}

func conversationItemFields(item map[string]json.RawMessage, allowed ...string) error {
	for field := range item {
		found := false
		for _, name := range allowed {
			found = found || field == name
		}
		if !found {
			return errConversationHostedState
		}
	}
	return nil
}

func canonicalConversationContent(raw json.RawMessage, role string) (json.RawMessage, error) {
	var text string
	var parts []map[string]json.RawMessage
	if json.Unmarshal(raw, &text) == nil {
		parts = []map[string]json.RawMessage{{"type": json.RawMessage(`"input_text"`), "text": raw}}
	} else if json.Unmarshal(raw, &parts) != nil || parts == nil {
		return nil, errConversationHostedState
	}
	normalized := make([]map[string]any, 0, len(parts))
	for _, part := range parts {
		switch rawJSONString(part["type"]) {
		case "input_text", "output_text", "text":
			if err := conversationItemFields(part, "type", "text", "annotations", "logprobs"); err != nil {
				return nil, err
			}
			// Azure emits empty annotations and token logprobs on ordinary text.
			// Web search citations are dropped: Codex does not retain them, so
			// its replayed history must match the saved text without them. File
			// and container references are provider state this history cannot replay.
			if raw, present := part["annotations"]; present {
				if err := validateConversationAnnotations(raw, role); err != nil {
					return nil, err
				}
			}
			if err := json.Unmarshal(part["text"], &text); err != nil {
				return nil, errConversationHostedState
			}
			kind := "input_text"
			if role == "assistant" {
				kind = "output_text"
			}
			normalized = append(normalized, map[string]any{"type": kind, "text": text})
		case "refusal":
			if err := conversationItemFields(part, "type", "refusal"); err != nil {
				return nil, err
			}
			if role != "assistant" || json.Unmarshal(part["refusal"], &text) != nil {
				return nil, errConversationHostedState
			}
			normalized = append(normalized, map[string]any{"type": "refusal", "refusal": text})
		default:
			return nil, errConversationHostedState
		}
	}
	encoded, err := json.Marshal(normalized)
	return encoded, err
}

func validateConversationAnnotations(raw json.RawMessage, role string) error {
	if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
		return nil
	}
	var annotations []map[string]json.RawMessage
	if json.Unmarshal(raw, &annotations) != nil {
		return errConversationHostedState
	}
	if len(annotations) == 0 {
		return nil
	}
	if role != "assistant" {
		return errConversationHostedState
	}
	for _, annotation := range annotations {
		if rawJSONString(annotation["type"]) != "url_citation" {
			return errConversationHostedState
		}
		if err := conversationItemFields(annotation, "type", "url", "title", "start_index", "end_index"); err != nil {
			return err
		}
	}
	return nil
}

// Codex parses each web search action variant into a fixed field set and
// keeps only prefixed item IDs on replay.
var conversationWebSearchActionFields = map[string][]string{
	"search":       {"query", "queries"},
	"open_page":    {"url"},
	"find_in_page": {"url", "pattern"},
}

// Codex replays a web search call with its prefixed ID, status and the action
// fields it parsed. Keep exactly those so saved history matches the replay.
func canonicalConversationWebSearchCall(item map[string]json.RawMessage) (json.RawMessage, error) {
	if err := conversationItemFields(item, "type", "id", "status", "action"); err != nil {
		return nil, err
	}
	var action map[string]json.RawMessage
	prefix, suffix, prefixed := strings.Cut(rawJSONString(item["id"]), "_")
	if json.Unmarshal(item["action"], &action) != nil || action == nil || !prefixed || prefix == "" || suffix == "" ||
		rawJSONString(item["status"]) != "completed" {
		return nil, errConversationHostedState
	}
	fields, known := conversationWebSearchActionFields[rawJSONString(action["type"])]
	if !known {
		return nil, errConversationHostedState
	}
	normalizedAction := map[string]json.RawMessage{"type": action["type"]}
	for _, field := range fields {
		value, present := action[field]
		if !present || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			continue
		}
		// Codex parses queries as a string list and every other field as a string.
		var typed any
		if field == "queries" {
			typed = new([]string)
		} else {
			typed = new(string)
		}
		if json.Unmarshal(value, typed) != nil {
			return nil, errConversationHostedState
		}
		normalizedAction[field], _ = json.Marshal(typed)
	}
	encodedAction, err := json.Marshal(normalizedAction)
	if err != nil {
		return nil, errConversationHostedState
	}
	return json.Marshal(map[string]json.RawMessage{
		"type": json.RawMessage(`"web_search_call"`), "id": item["id"], "status": json.RawMessage(`"completed"`), "action": encodedAction,
	})
}

func validateConversationTools(raw json.RawMessage, depth int) error {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}
	if depth > 1 {
		return errConversationHostedState
	}
	var tools []map[string]json.RawMessage
	if json.Unmarshal(raw, &tools) != nil {
		return errConversationHostedState
	}
	for _, tool := range tools {
		switch kind := rawJSONString(tool["type"]); kind {
		case "function", "custom":
			if strings.TrimSpace(rawJSONString(tool["name"])) == "" {
				return errConversationHostedState
			}
		case "web_search", "web_search_preview":
			// Web search executes upstream, but its definition and completed
			// call items are visible history. Migration targets must declare
			// hosted_tools support before they receive them.
		case "namespace":
			if err := validateConversationTools(tool["tools"], depth+1); err != nil {
				return err
			}
		default:
			return errConversationHostedState
		}
	}
	return nil
}

func validateConversationRequestFields(fields map[string]json.RawMessage) error {
	// Unknown fields can carry provider-owned references. Opted-in routes use
	// this explicit request contract instead of guessing which values to drop.
	allowed := map[string]bool{
		"model": true, "input": true, "instructions": true, "tools": true, "tool_choice": true,
		"previous_response_id": true, "conversation": true, "prompt": true, "background": true,
		"context_management": true, "stream": true, "stream_options": true, "store": true,
		"include": true, "parallel_tool_calls": true, "max_output_tokens": true, "max_tool_calls": true,
		"temperature": true, "top_p": true, "top_logprobs": true, "reasoning": true, "text": true,
		"truncation": true, "user": true, "metadata": true, "service_tier": true,
		"safety_identifier": true, "prompt_cache_key": true, "prompt_cache_retention": true,
		"client_metadata": true,
	}
	for field := range fields {
		if !allowed[field] {
			return errConversationHostedState
		}
	}
	if value := rawJSONString(fields["truncation"]); value != "" && value != "disabled" {
		return errConversationCompaction
	}
	for _, field := range []string{"conversation", "prompt", "context_management"} {
		if value := bytes.TrimSpace(fields[field]); len(value) != 0 && !bytes.Equal(value, []byte("null")) && !bytes.Equal(value, []byte("[]")) {
			if field == "context_management" {
				return errConversationCompaction
			}
			return errConversationHostedState
		}
	}
	if value := fields["background"]; len(value) != 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("false")) && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return errConversationHostedState
	}
	if value := fields["instructions"]; len(value) != 0 && !bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		var instructions string
		if json.Unmarshal(value, &instructions) != nil {
			return errConversationHostedState
		}
	}
	if value := fields["client_metadata"]; len(value) != 0 {
		var metadata map[string]string
		if len(value) > 16*1024 || json.Unmarshal(value, &metadata) != nil || len(metadata) > 64 {
			return errConversationHostedState
		}
	}
	return validateConversationTools(fields["tools"], 0)
}

func validateConversationToolSequence(items []json.RawMessage, allowPending bool) error {
	pending := make(map[string]string)
	seen := make(map[string]bool)
	for _, raw := range items {
		var item struct {
			Type   string `json:"type"`
			CallID string `json:"call_id"`
			Role   string `json:"role"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return errConversationHostedState
		}
		switch item.Type {
		case "function_call", "custom_tool_call":
			if seen[item.CallID] {
				return errConversationPendingTool
			}
			pending[item.CallID] = item.Type + "_output"
			seen[item.CallID] = true
		case "function_call_output", "custom_tool_call_output":
			if pending[item.CallID] != item.Type {
				return errConversationPendingTool
			}
			delete(pending, item.CallID)
		case "message":
			if item.Role != "assistant" && len(pending) > 0 {
				return errConversationPendingTool
			}
		}
	}
	if !allowPending && len(pending) > 0 {
		return errConversationPendingTool
	}
	return nil
}

func conversationNewInput(items []json.RawMessage) bool {
	for _, raw := range items {
		var item struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		_ = json.Unmarshal(raw, &item)
		if item.Type != "message" || item.Role == "assistant" {
			return false
		}
	}
	return true
}

func conversationDeltaInput(items []json.RawMessage) bool {
	for _, raw := range items {
		var item struct {
			Type string `json:"type"`
			Role string `json:"role"`
		}
		_ = json.Unmarshal(raw, &item)
		if item.Type == "function_call" || item.Type == "custom_tool_call" || item.Type == "web_search_call" || (item.Type == "message" && item.Role == "assistant") {
			return false
		}
	}
	return true
}

func conversationHasPrefix(items, prefix []json.RawMessage) bool {
	if len(items) < len(prefix) {
		return false
	}
	for i := range prefix {
		if !bytes.Equal(items[i], prefix[i]) {
			return false
		}
	}
	return true
}

func (s *conversationHistoryStore) prefixIndexes(routeID, scope string, items []json.RawMessage) [][]byte {
	if scope == "" {
		return nil
	}
	hash := sha256.New()
	indexes := make([][]byte, 0, len(items))
	var length [8]byte
	for _, item := range items {
		binary.BigEndian.PutUint64(length[:], uint64(len(item)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write(item)
		indexes = append(indexes, s.indexKey(routeID, "prefix:"+scope, string(hash.Sum(nil))))
	}
	return indexes
}

func (s *conversationHistoryStore) anchorIndexes(routeID string, anchors []conversationAnchor) [][]byte {
	keys := make([][]byte, 0, len(anchors))
	for _, anchor := range anchors {
		keys = append(keys, s.indexKey(routeID, anchor.kind, anchor.value))
	}
	return keys
}

func conversationFailureReason(err error) string {
	switch {
	case errors.Is(err, errConversationHistoryStorage):
		return "storage_unavailable"
	case errors.Is(err, errConversationHistoryCapacity):
		return "history_capacity"
	case errors.Is(err, errConversationHistoryMissing):
		return "history_unavailable"
	case errors.Is(err, errConversationHistoryPartial):
		return "partial_history"
	case errors.Is(err, errConversationHistoryBusy):
		return "concurrent_turn"
	case errors.Is(err, errConversationHistoryUncertain), errors.Is(err, errConversationIncomplete):
		return "execution_uncertain"
	case errors.Is(err, errConversationHostedState):
		return "unsupported_state"
	case errors.Is(err, errConversationCompaction):
		return "unreadable_compaction"
	case errors.Is(err, errConversationPendingTool):
		return "pending_tools"
	default:
		return "recovery_blocked"
	}
}
