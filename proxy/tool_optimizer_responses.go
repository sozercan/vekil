package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/sozercan/vekil/logger"
)

const responsesToolExecutionScopePrefix = "response:"

func toolExecutionScopeFromHeaders(headers http.Header) string {
	if headers == nil {
		return ""
	}
	if sessionID := strings.TrimSpace(headers.Get("session_id")); sessionID != "" {
		return "session:" + sessionID
	}
	parentThread := strings.TrimSpace(headers.Get("X-Codex-Parent-Thread-Id"))
	windowID := strings.TrimSpace(headers.Get("X-Codex-Window-Id"))
	if parentThread != "" || windowID != "" {
		return "codex:" + parentThread + "|" + windowID
	}
	if clientRequestID := strings.TrimSpace(headers.Get("X-Client-Request-Id")); clientRequestID != "" {
		return "client-request:" + clientRequestID
	}
	return ""
}

func chatToolExecutionScopeFromHeaders(headers http.Header) string {
	scope := toolExecutionScopeFromHeaders(headers)
	if isClientRequestToolExecutionScope(scope) {
		return ""
	}
	return scope
}

func toolExecutionScopeFromResponseID(responseID string) string {
	responseID = strings.TrimSpace(responseID)
	if responseID == "" {
		return ""
	}
	return responsesToolExecutionScopePrefix + responseID
}

func isClientRequestToolExecutionScope(scope string) bool {
	return strings.HasPrefix(strings.TrimSpace(scope), "client-request:")
}

func uniqueToolExecutionScopes(scopes ...string) []string {
	seen := make(map[string]struct{}, len(scopes))
	out := make([]string, 0, len(scopes))
	for _, scope := range scopes {
		scope = strings.TrimSpace(scope)
		if scope == "" {
			continue
		}
		if _, ok := seen[scope]; ok {
			continue
		}
		seen[scope] = struct{}{}
		out = append(out, scope)
	}
	return out
}

func toolExecutionScopeFromResponsePayload(payload map[string]json.RawMessage) string {
	if payload == nil {
		return ""
	}
	var responseID string
	if err := json.Unmarshal(payload["id"], &responseID); err != nil {
		return ""
	}
	return toolExecutionScopeFromResponseID(responseID)
}

func responsesRequestToolExecutionScope(headerScope, previousResponseID string) string {
	previousScope := toolExecutionScopeFromResponseID(previousResponseID)
	scope := strings.TrimSpace(headerScope)
	if isClientRequestToolExecutionScope(scope) && previousScope != "" {
		return previousScope
	}
	if scope != "" {
		return scope
	}
	return previousScope
}

func newToolExecutionContext(item toolCommandItem, originalCommand, rewrittenCommand, rewriteProvider string) ToolExecutionContext {
	filterHint := ResolveFilterHint(originalCommand)
	if filterHint == "" && rewrittenCommand != originalCommand {
		filterHint = ResolveFilterHint(rewrittenCommand)
	}
	return ToolExecutionContext{
		CallID:           item.CallID,
		ToolName:         item.ToolName,
		OriginalCommand:  originalCommand,
		RewrittenCommand: rewrittenCommand,
		RewriteProvider:  rewriteProvider,
		FilterHint:       filterHint,
	}
}

func replaceTopLevelRawJSONField(bodyBytes []byte, field string, replacement json.RawMessage) ([]byte, bool) {
	field = strings.TrimSpace(field)
	if field == "" {
		return bodyBytes, false
	}
	replacement = bytes.TrimSpace(replacement)
	if len(replacement) == 0 || !json.Valid(replacement) {
		return bodyBytes, false
	}

	decoder := json.NewDecoder(bytes.NewReader(bodyBytes))
	decoder.UseNumber()
	token, err := decoder.Token()
	if err != nil {
		return bodyBytes, false
	}
	if delim, ok := token.(json.Delim); !ok || delim != '{' {
		return bodyBytes, false
	}

	var out bytes.Buffer
	out.Grow(len(bodyBytes) + len(replacement))
	lastCopyOffset := 0
	replaced := false
	for decoder.More() {
		keyToken, err := decoder.Token()
		if err != nil {
			return bodyBytes, false
		}
		key, ok := keyToken.(string)
		if !ok {
			return bodyBytes, false
		}

		valueStart, ok := topLevelJSONFieldValueStart(bodyBytes, int(decoder.InputOffset()))
		if !ok {
			return bodyBytes, false
		}

		var rawValue json.RawMessage
		if err := decoder.Decode(&rawValue); err != nil {
			return bodyBytes, false
		}
		valueEnd := int(decoder.InputOffset())
		if valueEnd < valueStart || valueEnd > len(bodyBytes) {
			return bodyBytes, false
		}

		if key != field {
			continue
		}
		// Duplicate top-level keys are not a valid Responses shape, but if
		// one appears, rewrite every occurrence so downstream first-key/last-key
		// parser differences cannot observe a stale value.
		out.Write(bodyBytes[lastCopyOffset:valueStart])
		out.Write(replacement)
		lastCopyOffset = valueEnd
		replaced = true
	}

	endToken, err := decoder.Token()
	if err != nil {
		return bodyBytes, false
	}
	if delim, ok := endToken.(json.Delim); !ok || delim != '}' {
		return bodyBytes, false
	}
	if trailingToken, err := decoder.Token(); err != io.EOF {
		_ = trailingToken
		return bodyBytes, false
	}
	if !replaced {
		return bodyBytes, false
	}
	out.Write(bodyBytes[lastCopyOffset:])
	return out.Bytes(), true
}

func topLevelJSONFieldValueStart(bodyBytes []byte, offset int) (int, bool) {
	if offset < 0 || offset >= len(bodyBytes) {
		return 0, false
	}
	i := offset
	for i < len(bodyBytes) && isJSONWhitespace(bodyBytes[i]) {
		i++
	}
	if i >= len(bodyBytes) || bodyBytes[i] != ':' {
		return 0, false
	}
	i++
	for i < len(bodyBytes) && isJSONWhitespace(bodyBytes[i]) {
		i++
	}
	if i >= len(bodyBytes) {
		return 0, false
	}
	return i, true
}

func isJSONWhitespace(b byte) bool {
	switch b {
	case ' ', '\t', '\n', '\r':
		return true
	default:
		return false
	}
}

func (h *ProxyHandler) maybeRewriteResponsesResponseBody(ctx context.Context, bodyBytes []byte, store *ToolExecutionContextStore, scope string) ([]byte, bool) {
	manager := h.toolOptimizers
	if manager == nil || !manager.ShouldInspectNonStreamingResponses() {
		return bodyBytes, false
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		return bodyBytes, false
	}
	rawOutput, ok := payload["output"]
	if !ok {
		return bodyBytes, false
	}
	var outputItems []json.RawMessage
	if err := json.Unmarshal(rawOutput, &outputItems); err != nil {
		return bodyBytes, false
	}
	turnCtx, cancel := h.withToolOptimizerStageContext(ctx, manager, toolOptimizerStageCommandRewrite)
	defer cancel()
	ctx = turnCtx
	responseScope := toolExecutionScopeFromResponsePayload(payload)
	captureScopes := uniqueToolExecutionScopes(scope, responseScope)
	changed := false
	for i, rawItem := range outputItems {
		newItem, itemChanged := h.maybeRewriteOrCaptureToolCommandItemInScopes(ctx, rawItem, store, captureScopes, true)
		if itemChanged {
			outputItems[i] = newItem
			changed = true
		}
	}
	if !changed {
		return bodyBytes, false
	}
	newOutput, err := json.Marshal(outputItems)
	if err != nil {
		return bodyBytes, false
	}
	rewritten, ok := replaceTopLevelRawJSONField(bodyBytes, "output", newOutput)
	if !ok {
		return bodyBytes, false
	}
	return rewritten, true
}

func (h *ProxyHandler) maybeRewriteOrCaptureToolCommandItem(ctx context.Context, rawItem json.RawMessage, store *ToolExecutionContextStore, scope string, allowRewrite bool) (json.RawMessage, bool) {
	return h.maybeRewriteOrCaptureToolCommandItemInScopes(ctx, rawItem, store, []string{scope}, allowRewrite)
}

func (h *ProxyHandler) maybeRewriteOrCaptureToolCommandItemInScopes(ctx context.Context, rawItem json.RawMessage, store *ToolExecutionContextStore, scopes []string, allowRewrite bool) (json.RawMessage, bool) {
	manager := h.toolOptimizers
	item, ok := extractShellFunctionCommandItem(rawItem, manager)
	if !ok {
		return rawItem, false
	}

	originalCommand := item.Command
	rewrittenCommand := originalCommand
	rewriteProvider := ""
	changed := false
	if allowRewrite && manager.CommandRewriteEnabled() {
		result := manager.RewriteCommand(ctx, ToolCommandRewriteRequest{
			ToolName: item.ToolName,
			CallID:   item.CallID,
			Command:  originalCommand,
		})
		if result.Changed {
			newItem, ok := replaceShellFunctionCommand(rawItem, strings.TrimSpace(result.Command), manager)
			if ok {
				rawItem = newItem
				rewrittenCommand = strings.TrimSpace(result.Command)
				rewriteProvider = result.Provider
				changed = true
			}
		}
	}

	if store != nil {
		toolCtx := newToolExecutionContext(item, originalCommand, rewrittenCommand, rewriteProvider)
		for _, scope := range uniqueToolExecutionScopes(scopes...) {
			store.Put(scope, toolCtx)
		}
	}

	return rawItem, changed
}

var (
	responsesFunctionCallMarker      = []byte("function_call")
	responsesLocalShellCallMarker    = []byte("local_shell_call")
	responsesJSONUnicodeEscapeMarker = []byte(`\u`)
)

// responsesPayloadMayContainOptimizableToolItems avoids decoding replay items
// that cannot be tool calls or tool outputs. Unicode escapes force inspection to
// avoid missing an escaped type value.
func responsesPayloadMayContainOptimizableToolItems(payload []byte) bool {
	return bytes.Contains(payload, responsesFunctionCallMarker) ||
		bytes.Contains(payload, responsesLocalShellCallMarker) ||
		bytes.Contains(payload, responsesJSONUnicodeEscapeMarker)
}

func (h *ProxyHandler) maybeReduceResponsesToolOutputsInRequestBody(ctx context.Context, bodyBytes []byte, store *ToolExecutionContextStore, scope string) ([]byte, int) {
	manager := h.toolOptimizers
	if manager == nil || !manager.OutputReduceEnabled() || !responsesPayloadMayContainOptimizableToolItems(bodyBytes) {
		return bodyBytes, 0
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		return bodyBytes, 0
	}
	rawInput, ok := payload["input"]
	if !ok {
		return bodyBytes, 0
	}
	var inputItems []json.RawMessage
	if err := json.Unmarshal(rawInput, &inputItems); err != nil {
		return bodyBytes, 0
	}
	turnCtx, cancel := h.withToolOptimizerStageContext(ctx, manager, toolOptimizerStageOutputReduce)
	defer cancel()
	ctx = turnCtx

	localContexts := make(map[string]ToolExecutionContext)
	changedCount := 0
	for i, rawItem := range inputItems {
		if !responsesPayloadMayContainOptimizableToolItems(rawItem) {
			continue
		}
		var item map[string]json.RawMessage
		if err := json.Unmarshal(rawItem, &item); err != nil {
			continue
		}
		itemType, ok := extractNonEmptyJSONStringField(item, "type")
		if !ok {
			continue
		}

		switch itemType {
		case "function_call", "local_shell_call":
			commandItem, ok := extractShellFunctionCommand(item, manager)
			if !ok {
				continue
			}
			toolCtx := newToolExecutionContext(commandItem, commandItem.Command, commandItem.Command, "")
			localContexts[commandItem.CallID] = toolCtx
			if store != nil && scope != "" {
				if _, exists := store.Get(scope, toolCtx.CallID); !exists {
					store.Put(scope, toolCtx)
				}
			}
			continue
		case "function_call_output", "local_shell_call_output":
		default:
			continue
		}

		outputItem, ok := extractFunctionCallOutput(item)
		if !ok {
			continue
		}
		var toolCtx ToolExecutionContext
		var foundContext bool
		toolCtx, foundContext = localContexts[outputItem.CallID]
		if !foundContext && store != nil && scope != "" {
			toolCtx, foundContext = store.Get(scope, outputItem.CallID)
		}
		if !foundContext {
			continue
		}
		command := firstNonEmpty(toolCtx.RewrittenCommand, toolCtx.OriginalCommand)
		result := manager.ReduceOutput(ctx, ToolOutputReduceRequest{
			ToolName:   toolCtx.ToolName,
			CallID:     outputItem.CallID,
			Command:    command,
			FilterHint: toolCtx.FilterHint,
			Output:     outputItem.Output,
		})
		if !result.Changed {
			continue
		}
		newItem, ok := replaceFunctionCallOutput(rawItem, result.Output)
		if !ok {
			continue
		}
		inputItems[i] = newItem
		changedCount++
	}
	if changedCount == 0 {
		return bodyBytes, 0
	}
	newInput, err := json.Marshal(inputItems)
	if err != nil {
		return bodyBytes, 0
	}
	rewritten, ok := replaceTopLevelRawJSONField(bodyBytes, "input", newInput)
	if !ok {
		return bodyBytes, 0
	}
	return rewritten, changedCount
}

func (h *ProxyHandler) rewriteResponsesRequestBodyWithToolOptimizers(ctx context.Context, bodyBytes []byte, endpoint string, injectResumePrompt bool, store *ToolExecutionContextStore, scope string) []byte {
	return h.rewriteResponsesRequestBodyWithToolOptimizersForModel(ctx, bodyBytes, extractResponsesRequestModel(bodyBytes), endpoint, injectResumePrompt, store, scope)
}

func (h *ProxyHandler) rewriteResponsesRequestBodyWithToolOptimizersForModel(ctx context.Context, bodyBytes []byte, requestedModel string, endpoint string, injectResumePrompt bool, store *ToolExecutionContextStore, scope string) []byte {
	bodyBytes = h.rewriteResponsesRequestBodyForModel(bodyBytes, requestedModel, endpoint, injectResumePrompt)
	rewritten, count := h.maybeReduceResponsesToolOutputsInRequestBody(ctx, bodyBytes, store, scope)
	if count > 0 {
		h.log.Debug("reduced responses tool outputs", logger.F("endpoint", endpoint), logger.F("count", count))
		return rewritten
	}
	return bodyBytes
}
