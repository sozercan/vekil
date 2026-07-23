package proxy

import (
	"bytes"
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

// handlePolicyResponses serves the bounded, stateless Responses compatibility
// surface used by Responses-only agent clients such as Codex. Policy selection
// still consumes canonical Chat facts and the selected terminal route remains a
// native Chat route; only the public ingress and egress protocol are adapted.
func (h *ProxyHandler) handlePolicyResponses(w http.ResponseWriter, r *http.Request, body []byte, canonicalPublicID string) {
	ensurePolicyLocalRequestIdentity(w, r, canonicalPublicID)
	requestedModel := extractResponsesRequestModel(body)
	if !h.modelAllowedForRequest(requestedModel, providerEndpointResponses) {
		writeOpenAIError(w, http.StatusBadRequest, modelNotAllowedRequestError(requestedModel).Error(), "invalid_request_error")
		return
	}
	translated, err := translatePolicyResponsesRequestToChat(body)
	if err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	policyPlan, err := h.planOpenAIChatPolicy(r.Context(), translated.PublicModel, translated.Body)
	if err != nil {
		if h.handleShutdownError(w, r, nil, err) {
			return
		}
		status := upstreamStatusCode(err, http.StatusBadRequest)
		if status == http.StatusBadRequest {
			writeOpenAIError(w, status, err.Error(), "invalid_request_error")
		} else {
			writePolicyResponsesExecutionError(w, err, canonicalPublicID)
		}
		return
	}
	if !policyPlan.valid() {
		writeOpenAIError(w, http.StatusInternalServerError, "policy Responses request did not produce a routing plan", "server_error")
		return
	}
	plannedCtx, operation, err := withPlannedChatOperation(r.Context(), r.Context(), policyPlan)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	r = r.WithContext(plannedCtx)
	w.Header().Set("X-Vekil-Request-ID", operation.operationID())
	publicModel := strings.TrimSpace(policyPlan.publicID)
	if publicModel == "" {
		publicModel = strings.TrimSpace(canonicalPublicID)
	}

	terminalParallelToolCalls := cloneBoolPtr(policyPlan.terminalParallelToolCalls)
	parallelPreparation := policyParallelToolCallsPreparation(policyPlan.contract, terminalParallelToolCalls)
	chatBody, mode := preparePolicyOpenAIChatCompletionsRequest(translated.Body, policyPlan.contract, terminalParallelToolCalls)
	if parallelPreparation != chatParallelToolCallsDefault {
		translated.Response.ParallelToolCalls = false
	}
	h.observeRequestSummaryWithProviderModel(r.Context(), "responses", publicModel, publicModel, translated.Stream, providerEndpointChatCompletions)
	toolScope := chatToolExecutionScopeFromHeaders(r.Header)
	chatBody = h.rewriteOpenAIChatRequestBodyWithToolOptimizers(r.Context(), chatBody, h.toolContexts, toolScope)

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContextFrom(r.Context(), mode.clientRequestedStream || mode.forceUpstreamStream)
	defer upstreamCancel()
	upstreamCtx = withRouteOperation(upstreamCtx, routeOperationFromContext(r.Context()))
	upstreamCtx, _, _, err = h.withChatExecutionRoute(upstreamCtx, r.Context(), translated.PublicModel, chatBody)
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		observePolicyResponsesExecutionError(r.Context(), err)
		writePolicyResponsesExecutionError(w, err, publicModel)
		return
	}

	result, err := h.executeRoutedChatCompletions(upstreamCtx, chatBody, mode, chatExecutionOptions{}, translated.PublicModel)
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		observePolicyResponsesExecutionError(r.Context(), err)
		writePolicyResponsesExecutionError(w, err, publicModel)
		return
	}
	result, chatBody, mode = h.retryRoutedChatExecutionWithoutInjectedStreamOptions(upstreamCtx, result, chatBody, mode, translated.PublicModel)
	observeUpstreamHeaders(r.Context(), result.Headers)

	completion, err := h.collectPolicyResponsesChatCompletion(upstreamCtx, result, chatBody, mode)
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		observePolicyResponsesExecutionError(r.Context(), err)
		writePolicyResponsesExecutionError(w, err, publicModel)
		return
	}
	completion.Model = publicModel
	observeOpenAIUsage(r.Context(), completion.Usage)
	h.maybeRewriteOrCaptureOpenAIChatToolCommands(r.Context(), completion, h.toolContexts, toolScope, false)
	markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentSemantic)
	response, err := buildPolicyResponsesResponse(completion, publicModel, translated.CallableTools, translated.Response)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "failed to translate policy response", "server_error")
		return
	}
	if safeHeaders := policyChatSafeHeaders(result.Headers, publicModel); len(safeHeaders) > 0 {
		mergeHeaderValues(w.Header(), safeHeaders)
	}
	if err := writePolicyResponsesResult(w, response, translated.Stream); err != nil && h.log != nil {
		h.log.Debug("failed to write policy Responses result", logger.Err(err))
	}
}

func (h *ProxyHandler) collectPolicyResponsesChatCompletion(ctx context.Context, result chatExecutionResult, body []byte, mode chatCompletionsMode) (*models.OpenAIResponse, error) {
	if result.Stream != nil {
		return aggregatePolicyChatStreamEvents(result.Stream)
	}
	if result.Completion != nil {
		if result.Backend != chatBackendResponses {
			return nil, fmt.Errorf("policy Chat execution returned an unsupported pre-aggregated native completion")
		}
		return result.Completion, nil
	}
	if len(result.CompletionBody) > 0 {
		if result.Backend != chatBackendResponses {
			return nil, fmt.Errorf("policy Chat execution returned an unsupported pre-aggregated native completion body")
		}
		var completion models.OpenAIResponse
		if err := json.Unmarshal(result.CompletionBody, &completion); err != nil {
			return nil, fmt.Errorf("decode policy Chat completion: %w", err)
		}
		return &completion, nil
	}
	if result.Response == nil {
		return nil, fmt.Errorf("policy Chat execution returned no response")
	}
	if result.Response.StatusCode != http.StatusOK {
		return nil, policyResponsesChatUpstreamError(result.Response)
	}
	if mode.clientRequestedStream || mode.forceUpstreamStream || strings.Contains(strings.ToLower(result.Response.Header.Get("Content-Type")), "text/event-stream") {
		completion, terminalResponse, err := h.aggregateExplicitChatCompletionsResponse(ctx, result.Response, body, mode, aggregatePolicyStreamToResponseWithProgress)
		if err != nil {
			return nil, err
		}
		if terminalResponse != nil {
			return nil, policyResponsesChatUpstreamError(terminalResponse)
		}
		if completion == nil {
			return nil, fmt.Errorf("policy Chat stream returned no completion")
		}
		return completion, nil
	}
	defer func() { _ = result.Response.Body.Close() }()
	payload, err := io.ReadAll(io.LimitReader(result.Response.Body, maxLargeRequestBodySize+1))
	if err != nil {
		return nil, fmt.Errorf("read policy Chat completion: %w", err)
	}
	if len(payload) > maxLargeRequestBodySize {
		return nil, fmt.Errorf("policy Chat completion exceeds response limit")
	}
	payload = bytes.TrimSpace(payload)
	var completion models.OpenAIResponse
	if err := json.Unmarshal(payload, &completion); err != nil {
		return nil, fmt.Errorf("decode policy Chat completion: %w", err)
	}
	return &completion, nil
}

func policyResponsesChatUpstreamError(response *http.Response) *chatExecutionError {
	status := response.StatusCode
	headers := response.Header.Clone()
	drainAndClose(response.Body)
	return &chatExecutionError{
		StatusCode: status,
		Type:       openAIErrorTypeForHTTPStatus(status),
		Message:    "upstream request failed",
		Headers:    headers,
	}
}

func observePolicyResponsesExecutionError(ctx context.Context, err error) {
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) || executionErr == nil {
		return
	}
	observeChatExecutionError(ctx, executionErr)
	observeOpenAIUsage(ctx, executionErr.Usage)
}

func writePolicyResponsesExecutionError(w http.ResponseWriter, err error, publicModel string) {
	status := upstreamStatusCode(err, http.StatusBadGateway)
	headers := policyChatErrorHeaders(err)
	var executionErr *chatExecutionError
	if errors.As(err, &executionErr) && executionErr.StatusCode > 0 {
		status = executionErr.StatusCode
	}
	writePolicyChatSanitizedError(w, status, headers, publicModel)
}
