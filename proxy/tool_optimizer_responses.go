package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/sozercan/vekil/logger"
)

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
	changed := false
	for i, rawItem := range outputItems {
		newItem, itemChanged := h.maybeRewriteOrCaptureToolCommandItem(ctx, rawItem, store, scope, true)
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
	payload["output"] = newOutput
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return bodyBytes, false
	}
	return rewritten, true
}

func (h *ProxyHandler) maybeRewriteOrCaptureToolCommandItem(ctx context.Context, rawItem json.RawMessage, store *ToolExecutionContextStore, scope string, allowRewrite bool) (json.RawMessage, bool) {
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

	if store != nil && scope != "" {
		filterHint := ResolveFilterHint(originalCommand)
		if filterHint == "" && rewrittenCommand != originalCommand {
			filterHint = ResolveFilterHint(rewrittenCommand)
		}
		store.Put(scope, ToolExecutionContext{
			CallID:           item.CallID,
			ToolName:         item.ToolName,
			OriginalCommand:  originalCommand,
			RewrittenCommand: rewrittenCommand,
			RewriteProvider:  rewriteProvider,
			FilterHint:       filterHint,
		})
	}

	return rawItem, changed
}

func (h *ProxyHandler) maybeReduceResponsesToolOutputsInRequestBody(ctx context.Context, bodyBytes []byte, store *ToolExecutionContextStore, scope string) ([]byte, int) {
	manager := h.toolOptimizers
	if manager == nil || !manager.OutputReduceEnabled() || store == nil || scope == "" {
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
	// First pass: capture any function_call items present in the input array
	// so that replayed call+output pairs in the same request body are handled.
	for _, rawItem := range inputItems {
		h.maybeRewriteOrCaptureToolCommandItem(ctx, rawItem, store, scope, false)
	}

	changedCount := 0
	for i, rawItem := range inputItems {
		outputItem, ok := extractFunctionCallOutputItem(rawItem)
		if !ok {
			continue
		}
		toolCtx, ok := store.Get(scope, outputItem.CallID)
		if !ok {
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
	payload["input"] = newInput
	rewritten, err := json.Marshal(payload)
	if err != nil {
		return bodyBytes, 0
	}
	return rewritten, changedCount
}

func (h *ProxyHandler) rewriteResponsesRequestBodyWithToolOptimizers(ctx context.Context, bodyBytes []byte, endpoint string, injectResumePrompt bool, store *ToolExecutionContextStore, scope string) []byte {
	bodyBytes = h.rewriteResponsesRequestBody(bodyBytes, endpoint, injectResumePrompt)
	rewritten, count := h.maybeReduceResponsesToolOutputsInRequestBody(ctx, bodyBytes, store, scope)
	if count > 0 {
		h.log.Debug("reduced responses tool outputs", logger.F("endpoint", endpoint), logger.F("count", count))
		return rewritten
	}
	return bodyBytes
}
