package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

const (
	geminiSyntheticWebSearchToolName = "__vekil_google_search"
	geminiMaxWebSearchRounds         = 3
)

var geminiSyntheticWebSearchParameters = json.RawMessage(`{"type":"object","properties":{"query":{"type":"string","description":"A focused, self-contained natural-language search query."}},"required":["query"],"additionalProperties":false}`)

func geminiSyntheticWebSearchTool() models.OpenAITool {
	return models.OpenAITool{
		Type: "function",
		Function: models.OpenAIFunction{
			Name:        geminiSyntheticWebSearchToolName,
			Description: "Search the web using GitHub Copilot's web search. Call this when up-to-date web information is needed and synthesize the cited answer for the user.",
			Parameters:  geminiSyntheticWebSearchParameters,
		},
	}
}

func (h *ProxyHandler) supportsGeminiGoogleSearch() bool {
	return h != nil && h.auth != nil && h.UsesCopilot()
}

func geminiRequestHasGoogleSearch(req *models.GeminiGenerateContentRequest) bool {
	if req == nil {
		return false
	}
	for _, tool := range req.Tools {
		if hasRawJSON(tool.GoogleSearch) || hasRawJSON(tool.GoogleSearchRetrieval) {
			return true
		}
	}
	return false
}

func (h *ProxyHandler) handleGeminiGoogleSearchGenerateContent(
	w http.ResponseWriter,
	r *http.Request,
	oaiReq *models.OpenAIRequest,
	stream bool,
	scope string,
) {
	workingReq := *oaiReq
	workingReq.Messages = append([]models.OpenAIMessage(nil), oaiReq.Messages...)
	workingReq.Tools = append([]models.OpenAITool(nil), oaiReq.Tools...)

	var (
		totalUsage      *models.OpenAIUsage
		lastHeaders     http.Header
		executedQueries []string
		finalResp       *models.OpenAIResponse
	)

	for round := 0; round <= geminiMaxWebSearchRounds; round++ {
		roundReq := workingReq
		if round == geminiMaxWebSearchRounds {
			roundReq.Tools = filterOutGeminiSyntheticWebSearchTool(workingReq.Tools)
			if len(roundReq.Tools) == 0 {
				roundReq.ToolChoice = nil
				roundReq.ParallelToolCalls = nil
			}
		}

		oaiResp, headers, statusCode, err := h.executeGeminiGoogleSearchRound(r, &roundReq)
		if err != nil {
			if h.handleShutdownError(w, r, nil, err) {
				return
			}
			if statusCode > 0 {
				mergeHeaderValues(w.Header(), convertedChatSafeHeaders(headers))
				writeGeminiError(w, statusCode, mapGeminiUpstreamStatus(statusCode), err.Error())
				return
			}
			var executionErr *chatExecutionError
			if errors.As(err, &executionErr) {
				observeChatExecutionError(r.Context(), executionErr)
				if len(executionErr.Headers) > 0 {
					mergeHeaderValues(w.Header(), executionErr.Headers)
				}
				observeOpenAIUsage(r.Context(), executionErr.Usage)
				writeGeminiError(w, executionErr.StatusCode, mapGeminiUpstreamStatus(executionErr.StatusCode), executionErr.Message)
				return
			}
			h.writeGeminiUpstreamFailure(w, err)
			return
		}
		lastHeaders = headers
		totalUsage = accumulateOpenAIUsage(totalUsage, oaiResp.Usage)
		observeOpenAIUsage(r.Context(), oaiResp.Usage)
		observeCopilotUsage(r.Context(), oaiResp.CopilotUsage)

		searchCalls, otherCalls := splitGeminiSyntheticWebSearchCalls(oaiResp)
		if len(searchCalls) == 0 || len(otherCalls) > 0 || round == geminiMaxWebSearchRounds {
			if len(searchCalls) > 0 && len(oaiResp.Choices) > 0 {
				oaiResp.Choices[0].Message.ToolCalls = otherCalls
			}
			finalResp = oaiResp
			break
		}

		assistantMsg := oaiResp.Choices[0].Message
		if assistantMsg.Role == "" {
			assistantMsg.Role = "assistant"
		}
		workingReq.Messages = append(workingReq.Messages, assistantMsg)

		for _, call := range searchCalls {
			query, parseErr := extractGeminiWebSearchQuery(call.Function.Arguments)
			var toolOutput string
			if parseErr != nil {
				toolOutput = "web search error: " + parseErr.Error()
			} else {
				executedQueries = append(executedQueries, query)
				toolOutput = h.executeGeminiWebSearchCall(r.Context(), query)
			}
			contentJSON, _ := json.Marshal(toolOutput)
			workingReq.Messages = append(workingReq.Messages, models.OpenAIMessage{
				Role:       "tool",
				ToolCallID: call.ID,
				Content:    contentJSON,
			})
		}
	}

	if finalResp == nil {
		writeGeminiError(w, http.StatusBadGateway, "UNAVAILABLE", "web search completed without a response")
		return
	}
	if totalUsage != nil {
		finalResp.Usage = totalUsage
	}

	h.maybeRewriteOrCaptureOpenAIChatToolCommands(r.Context(), finalResp, h.toolContexts, scope, false)
	mergeHeaderValues(w.Header(), convertedChatSafeHeaders(lastHeaders))

	geminiResp := TranslateOpenAIToGemini(finalResp)
	if len(executedQueries) > 0 && len(geminiResp.Candidates) > 0 {
		geminiResp.Candidates[0].GroundingMetadata = &models.GeminiGroundingMetadata{
			WebSearchQueries: executedQueries,
		}
	}

	if stream {
		setSSEHeaders(w)
		_ = writeGeminiSSEData(w, geminiResp)
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(geminiResp)
}

func (h *ProxyHandler) executeGeminiGoogleSearchRound(
	r *http.Request,
	roundReq *models.OpenAIRequest,
) (*models.OpenAIResponse, http.Header, int, error) {
	reqCopy := *roundReq
	forceStream := len(reqCopy.Tools) > 0 && !openAIToolChoiceIsNone(reqCopy.ToolChoice)
	if forceStream {
		streamFlag := true
		reqCopy.Stream = &streamFlag
		reqCopy.StreamOptions = &models.StreamOptions{IncludeUsage: true}
	} else {
		streamFlag := false
		reqCopy.Stream = &streamFlag
		reqCopy.StreamOptions = nil
	}

	oaiBody, err := json.Marshal(&reqCopy)
	if err != nil {
		return nil, nil, http.StatusInternalServerError, errors.New("failed to marshal request")
	}

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContextFrom(r.Context(), forceStream)
	defer upstreamCancel()
	upstreamCtx = withRouteOperation(upstreamCtx, routeOperationFromContext(r.Context()))
	upstreamCtx, _, _, err = h.withChatExecutionRoute(upstreamCtx, r.Context(), reqCopy.Model, oaiBody, false)
	if err != nil {
		return nil, nil, 0, err
	}

	mode := chatCompletionsMode{
		clientRequestedStream: false,
		forceUpstreamStream:   forceStream,
		injectedStreamUsage:   forceStream,
	}

	result, err := h.executeRoutedChatCompletions(upstreamCtx, oaiBody, mode, chatExecutionOptions{}, reqCopy.Model)
	if err != nil {
		return nil, nil, 0, err
	}

	result, oaiBody, mode = h.retryChatExecutionWithoutInjectedStreamOptions(upstreamCtx, result, oaiBody, mode)
	observeChatExecutionRoute(r.Context(), result)
	result.observeUpstreamError(r.Context())
	headers := chatExecutionUpstreamHeaders(result)
	observeUpstreamHeaders(r.Context(), headers)

	if result.Response != nil && result.Response.StatusCode != http.StatusOK {
		resp := result.Response
		defer func() { _ = resp.Body.Close() }()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		observeUpstreamErrorDetail(r.Context(), resp.StatusCode, errBody)
		detail := formatUpstreamErrorMessage(resp.StatusCode, errBody)
		h.log.Error("upstream error",
			logger.F("endpoint", "gemini"),
			logger.F("status", resp.StatusCode),
			logger.F("request_bytes", len(oaiBody)),
			logger.F("response_bytes", len(errBody)),
		)
		return nil, resp.Header, resp.StatusCode, errors.New(detail)
	}

	if mode.forceUpstreamStream {
		var oaiResp *models.OpenAIResponse
		var aggregateErr error
		if result.Stream != nil {
			oaiResp, aggregateErr = aggregateChatStreamEvents(result.Stream)
		} else if result.Response != nil {
			body := newLifecycleAwareReadCloser(result.Response.Body, upstreamCtx)
			oaiResp, _, aggregateErr = aggregateGeminiStreamToResponseWithProgress(body)
			if aggregateErr != nil && body.canceledAtFailure() {
				aggregateErr = context.Canceled
			}
		} else {
			aggregateErr = errors.New("forced stream returned no stream")
		}
		if aggregateErr != nil {
			var nativeStreamErr *openAIStreamError
			if errors.As(aggregateErr, &nativeStreamErr) {
				return nil, nativeStreamErr.headers, nativeStreamErr.httpStatus(), errors.New(nativeStreamErr.Error())
			}
			return nil, headers, 0, aggregateErr
		}
		return oaiResp, headers, 0, nil
	}

	if result.Completion != nil {
		return result.Completion, headers, 0, nil
	}

	if result.Response == nil {
		return nil, headers, http.StatusBadGateway, errors.New("upstream returned no response")
	}
	body := newLifecycleAwareReadCloser(result.Response.Body, upstreamCtx)
	defer func() { _ = body.Close() }()
	var parsed models.OpenAIResponse
	if err := json.NewDecoder(body).Decode(&parsed); err != nil {
		if body.canceledAtFailure() {
			return nil, headers, 0, context.Canceled
		}
		return nil, headers, http.StatusInternalServerError, errors.New("failed to parse upstream response")
	}
	return &parsed, headers, 0, nil
}

func (h *ProxyHandler) executeGeminiWebSearchCall(ctx context.Context, query string) string {
	raw, err := h.callWebSearchUpstream(ctx, query)
	if err != nil {
		h.log.Warn("gemini googleSearch web search failed", logger.Err(err))
		return "web search failed: " + err.Error()
	}
	return extractMCPToolResultText(raw)
}

func extractMCPToolResultText(raw json.RawMessage) string {
	var result struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &result); err == nil && len(result.Content) > 0 {
		var parts []string
		for _, item := range result.Content {
			if strings.TrimSpace(item.Text) != "" {
				parts = append(parts, item.Text)
			}
		}
		if len(parts) > 0 {
			return strings.Join(parts, "\n")
		}
	}
	return string(raw)
}

func extractGeminiWebSearchQuery(arguments string) (string, error) {
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("invalid arguments: %w", err)
	}
	query := strings.TrimSpace(args.Query)
	if query == "" {
		return "", errors.New("query is required")
	}
	return query, nil
}

func splitGeminiSyntheticWebSearchCalls(resp *models.OpenAIResponse) (searchCalls, otherCalls []models.OpenAIToolCall) {
	if resp == nil || len(resp.Choices) == 0 {
		return nil, nil
	}
	for _, call := range resp.Choices[0].Message.ToolCalls {
		if call.Function.Name == geminiSyntheticWebSearchToolName {
			searchCalls = append(searchCalls, call)
		} else {
			otherCalls = append(otherCalls, call)
		}
	}
	return searchCalls, otherCalls
}

func filterOutGeminiSyntheticWebSearchTool(tools []models.OpenAITool) []models.OpenAITool {
	if len(tools) == 0 {
		return nil
	}
	filtered := make([]models.OpenAITool, 0, len(tools))
	for _, tool := range tools {
		if tool.Function.Name == geminiSyntheticWebSearchToolName {
			continue
		}
		filtered = append(filtered, tool)
	}
	return filtered
}

func accumulateOpenAIUsage(total, next *models.OpenAIUsage) *models.OpenAIUsage {
	if next == nil {
		return total
	}
	if total == nil {
		copied := *next
		return &copied
	}
	merged := *total
	merged.PromptTokens += next.PromptTokens
	merged.CompletionTokens += next.CompletionTokens
	merged.TotalTokens += next.TotalTokens
	merged.ReasoningTokens += next.ReasoningTokens
	return &merged
}
