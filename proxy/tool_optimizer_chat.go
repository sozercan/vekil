package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

func openAIChatToolExecutionCaptureScopes(scope string) []string {
	normalizedScope := strings.TrimSpace(scope)
	if normalizedScope == "" {
		return nil
	}
	return []string{normalizedScope}
}

func openAIChatToolExecutionLookupScopes(scope string) []string {
	return openAIChatToolExecutionCaptureScopes(scope)
}

func (h *ProxyHandler) maybeReduceOpenAIChatToolOutputs(ctx context.Context, req *models.OpenAIRequest, store *ToolExecutionContextStore, scope string) int {
	manager := h.toolOptimizers
	if manager == nil || !manager.OutputReduceEnabled() || req == nil {
		return 0
	}

	changedCount := 0
	localContexts := make(map[string]ToolExecutionContext)
	for i := range req.Messages {
		msg := &req.Messages[i]
		for _, toolCall := range msg.ToolCalls {
			h.captureOpenAIChatToolCallContext(toolCall, localContexts, store, scope)
		}
		if msg.Role != "tool" || strings.TrimSpace(msg.ToolCallID) == "" {
			continue
		}

		var output string
		if err := json.Unmarshal(msg.Content, &output); err != nil {
			continue
		}

		reduced, ok := h.reduceOpenAIChatToolOutput(ctx, msg.ToolCallID, output, store, scope)
		if !ok {
			if toolCtx, hasLocalContext := localContexts[strings.TrimSpace(msg.ToolCallID)]; hasLocalContext {
				reduced, ok = h.reduceOpenAIChatToolOutputWithContext(ctx, msg.ToolCallID, output, toolCtx)
			}
		}
		if !ok {
			continue
		}

		contentBytes, err := json.Marshal(reduced)
		if err != nil {
			continue
		}
		msg.Content = contentBytes
		changedCount++
	}
	return changedCount
}

func (h *ProxyHandler) reduceOpenAIChatToolOutputWithContext(ctx context.Context, callID, output string, toolCtx ToolExecutionContext) (string, bool) {
	manager := h.toolOptimizers
	if manager == nil || !manager.OutputReduceEnabled() || strings.TrimSpace(callID) == "" {
		return output, false
	}

	command := firstNonEmpty(toolCtx.RewrittenCommand, toolCtx.OriginalCommand)
	result := manager.ReduceOutput(ctx, ToolOutputReduceRequest{
		ToolName:   toolCtx.ToolName,
		CallID:     strings.TrimSpace(callID),
		Command:    command,
		FilterHint: toolCtx.FilterHint,
		Output:     output,
	})
	if !result.Changed {
		return output, false
	}
	return result.Output, true
}

func (h *ProxyHandler) reduceOpenAIChatToolOutput(ctx context.Context, callID, output string, store *ToolExecutionContextStore, scope string) (string, bool) {
	manager := h.toolOptimizers
	if manager == nil || !manager.OutputReduceEnabled() || store == nil || strings.TrimSpace(callID) == "" {
		return output, false
	}

	callID = strings.TrimSpace(callID)
	var toolCtx ToolExecutionContext
	var ok bool
	for _, lookupScope := range openAIChatToolExecutionLookupScopes(scope) {
		toolCtx, ok = store.Get(lookupScope, callID)
		if ok {
			break
		}
	}
	if !ok {
		return output, false
	}

	return h.reduceOpenAIChatToolOutputWithContext(ctx, callID, output, toolCtx)
}

func (h *ProxyHandler) maybeRewriteOrCaptureOpenAIChatToolCommands(ctx context.Context, resp *models.OpenAIResponse, store *ToolExecutionContextStore, scope string, allowRewrite bool) int {
	manager := h.toolOptimizers
	if manager == nil || !manager.ShouldInspectNonStreamingResponses() || resp == nil {
		return 0
	}

	changedCount := 0
	for choiceIdx := range resp.Choices {
		message := &resp.Choices[choiceIdx].Message
		for callIdx := range message.ToolCalls {
			call := &message.ToolCalls[callIdx]
			if !manager.MatchShellToolName(call.Function.Name) || strings.TrimSpace(call.ID) == "" {
				continue
			}

			originalCommand, ok := extractStringArgumentAtPath(call.Function.Arguments, manager.ShellCommandArgPath())
			if !ok {
				continue
			}

			rewrittenCommand := originalCommand
			rewriteProvider := ""
			if allowRewrite && manager.CommandRewriteEnabled() {
				result := manager.RewriteCommand(ctx, ToolCommandRewriteRequest{
					ToolName: strings.TrimSpace(call.Function.Name),
					CallID:   strings.TrimSpace(call.ID),
					Command:  originalCommand,
				})
				if result.Changed {
					newArguments, ok := replaceStringArgumentAtPath(call.Function.Arguments, manager.ShellCommandArgPath(), strings.TrimSpace(result.Command))
					if ok {
						call.Function.Arguments = newArguments
						rewrittenCommand = strings.TrimSpace(result.Command)
						rewriteProvider = result.Provider
						changedCount++
					}
				}
			}

			if store != nil {
				toolCtx := newToolExecutionContext(toolCommandItem{
					ToolName: strings.TrimSpace(call.Function.Name),
					CallID:   strings.TrimSpace(call.ID),
					Command:  originalCommand,
				}, originalCommand, rewrittenCommand, rewriteProvider)
				for _, captureScope := range openAIChatToolExecutionCaptureScopes(scope) {
					store.Put(captureScope, toolCtx)
				}
			}
		}
	}
	return changedCount
}

func (h *ProxyHandler) captureOpenAIChatToolCallContext(call models.OpenAIToolCall, localContexts map[string]ToolExecutionContext, store *ToolExecutionContextStore, scope string) {
	manager := h.toolOptimizers
	if manager == nil || !manager.OutputReduceEnabled() || strings.TrimSpace(call.ID) == "" || !manager.MatchShellToolName(call.Function.Name) {
		return
	}

	originalCommand, ok := extractStringArgumentAtPath(call.Function.Arguments, manager.ShellCommandArgPath())
	if !ok {
		return
	}

	toolCtx := newToolExecutionContext(toolCommandItem{
		ToolName: strings.TrimSpace(call.Function.Name),
		CallID:   strings.TrimSpace(call.ID),
		Command:  originalCommand,
	}, originalCommand, originalCommand, "")

	if localContexts != nil {
		localContexts[toolCtx.CallID] = toolCtx
	}
	if store == nil {
		return
	}
	for _, captureScope := range openAIChatToolExecutionCaptureScopes(scope) {
		if _, exists := store.Get(captureScope, toolCtx.CallID); !exists {
			store.Put(captureScope, toolCtx)
		}
	}
}

func (h *ProxyHandler) openAIChatStreamFinalResponseCallback(ctx context.Context, store *ToolExecutionContextStore, scope string) func(*models.OpenAIResponse) {
	if h == nil || h.toolOptimizers == nil || !h.toolOptimizers.OutputReduceEnabled() || store == nil || strings.TrimSpace(scope) == "" {
		return nil
	}
	return func(oaiResp *models.OpenAIResponse) {
		h.maybeRewriteOrCaptureOpenAIChatToolCommands(ctx, oaiResp, store, scope, false)
	}
}

func (h *ProxyHandler) rewriteOpenAIChatRequestBodyWithToolOptimizers(ctx context.Context, bodyBytes []byte, store *ToolExecutionContextStore, scope string) []byte {
	manager := h.toolOptimizers
	if manager == nil || !manager.OutputReduceEnabled() {
		return bodyBytes
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		return bodyBytes
	}

	rawMessages, ok := payload["messages"]
	if !ok {
		return bodyBytes
	}

	var messages []json.RawMessage
	if err := json.Unmarshal(rawMessages, &messages); err != nil {
		return bodyBytes
	}

	changedCount := 0
	localContexts := make(map[string]ToolExecutionContext)
	for i, rawMessage := range messages {
		var msg models.OpenAIMessage
		if err := json.Unmarshal(rawMessage, &msg); err != nil {
			continue
		}
		for _, toolCall := range msg.ToolCalls {
			h.captureOpenAIChatToolCallContext(toolCall, localContexts, store, scope)
		}
		if msg.Role != "tool" || strings.TrimSpace(msg.ToolCallID) == "" {
			continue
		}

		var output string
		if err := json.Unmarshal(msg.Content, &output); err != nil {
			continue
		}

		reduced, ok := h.reduceOpenAIChatToolOutput(ctx, msg.ToolCallID, output, store, scope)
		if !ok {
			if toolCtx, hasLocalContext := localContexts[strings.TrimSpace(msg.ToolCallID)]; hasLocalContext {
				reduced, ok = h.reduceOpenAIChatToolOutputWithContext(ctx, msg.ToolCallID, output, toolCtx)
			}
		}
		if !ok {
			continue
		}

		var messageMap map[string]json.RawMessage
		if err := json.Unmarshal(rawMessage, &messageMap); err != nil {
			continue
		}

		newContent, err := json.Marshal(reduced)
		if err != nil {
			continue
		}
		messageMap["content"] = newContent

		newMessage, err := json.Marshal(messageMap)
		if err != nil {
			continue
		}
		messages[i] = newMessage
		changedCount++
	}

	if changedCount == 0 {
		return bodyBytes
	}

	newMessages, err := json.Marshal(messages)
	if err != nil {
		return bodyBytes
	}
	payload["messages"] = newMessages

	rewritten, err := json.Marshal(payload)
	if err != nil {
		return bodyBytes
	}
	h.log.Debug("reduced openai chat tool outputs", logger.F("count", changedCount))
	return rewritten
}

func (h *ProxyHandler) maybeWriteOptimizedOpenAIChatPassthrough(ctx context.Context, w http.ResponseWriter, resp *http.Response, store *ToolExecutionContextStore, scope string) error {
	if h == nil || h.toolOptimizers == nil || !h.toolOptimizers.ShouldInspectNonStreamingResponses() || resp == nil || resp.Body == nil || resp.StatusCode != http.StatusOK {
		writeUpstreamResponse(w, resp)
		return nil
	}
	defer func() { _ = resp.Body.Close() }()

	bodyBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		copyPassthroughHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, bytes.NewReader(bodyBytes))
		return nil
	}

	var parsed models.OpenAIResponse
	if err := json.Unmarshal(bodyBytes, &parsed); err != nil {
		copyPassthroughHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(bodyBytes)
		return nil
	}

	changedCount := h.maybeRewriteOrCaptureOpenAIChatToolCommands(ctx, &parsed, store, scope, false)
	out := bodyBytes
	if changedCount > 0 {
		if rewritten, err := json.Marshal(&parsed); err == nil {
			out = rewritten
		} else {
			changedCount = 0
		}
	}

	copyPassthroughHeaders(w.Header(), resp.Header)
	if changedCount > 0 {
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(out)
	return nil
}
