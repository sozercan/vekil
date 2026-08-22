package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

const (
	geminiCountTokensCacheTTL = 60 * time.Second
	// geminiCountTokensCacheMaxEntries bounds unique countTokens payloads while
	// retaining ample reuse for active client sessions.
	geminiCountTokensCacheMaxEntries = 1024
)

func isGeminiCLIUserAgent(userAgent string) bool {
	userAgent = strings.ToLower(strings.TrimSpace(userAgent))
	return strings.Contains(userAgent, "geminicli/") || strings.HasPrefix(userAgent, "gemini-cli")
}

type geminiCountTokensCache struct {
	mu           sync.RWMutex
	entries      map[string]geminiCountTokensCacheEntry
	now          func() time.Time
	nextSequence uint64
}

type geminiCountTokensCacheEntry struct {
	response models.GeminiCountTokensResponse
	expiry   time.Time
	sequence uint64
}

// HandleGeminiModels routes Gemini-native model actions to the corresponding
// translation handler.
func (h *ProxyHandler) HandleGeminiModels(w http.ResponseWriter, r *http.Request) {
	model, action, err := parseGeminiPath(r.URL.Path)
	if err != nil {
		h.writeGeminiProtocolError(w, err)
		return
	}
	normalizedModel := normalizeGeminiModelName(model)
	switch action {
	case "generateContent":
		h.observePolicyRequestSummary(r.Context(), "gemini", normalizedModel, false)
	case "streamGenerateContent":
		h.observePolicyRequestSummary(r.Context(), "gemini", normalizedModel, true)
	case "countTokens":
		h.observePolicyRequestSummary(r.Context(), "gemini_count_tokens", normalizedModel, false)
	}

	admissionInbound := r.Context()
	if action == "countTokens" {
		admissionInbound = suppressRouteAttemptStats(admissionInbound)
	}
	admissionCtx, admittedOperation, _, err := h.withAdmittedExplicitRouteOperation(r.Context(), admissionInbound, normalizedModel, providerEndpointChatCompletions)
	if err != nil {
		h.writeGeminiUpstreamFailure(w, err)
		return
	}
	if admittedOperation != nil {
		r = r.WithContext(admissionCtx)
		w.Header().Set("X-Vekil-Request-ID", admittedOperation.operationID())
	}

	switch action {
	case "generateContent":
		h.handleGeminiGenerateContent(w, r, model, false)
	case "streamGenerateContent":
		h.handleGeminiGenerateContent(w, r, model, true)
	case "countTokens":
		h.handleGeminiCountTokens(w, r, model)
	default:
		writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", fmt.Sprintf("unsupported Gemini action %q", action))
	}
}

func (h *ProxyHandler) handleGeminiGenerateContent(w http.ResponseWriter, r *http.Request, pathModel string, stream bool) {
	body, err := readBody(r)
	if err != nil {
		if h.handleShutdownError(w, r, nil, err) {
			return
		}
		status := readBodyStatusCode(err)
		writeGeminiError(w, status, "INVALID_ARGUMENT", err.Error())
		return
	}
	defer func() { _ = r.Body.Close() }()
	if err := h.validateRouteAwareRequestJSON(body, normalizeGeminiModelName(pathModel), providerEndpointChatCompletions); err != nil {
		writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}

	req, err := decodeGeminiGenerateContentRequest(body)
	if err != nil {
		h.writeGeminiProtocolError(w, err)
		return
	}

	h.log.Debug("gemini request",
		logger.F("model", pathModel),
		logger.F("stream", stream),
		logger.F("contents", len(req.Contents)),
		logger.F("tools", len(req.Tools)),
	)
	h.observeRequestSummary(r.Context(), "gemini", pathModel, stream, providerEndpointChatCompletions)

	scope := chatToolExecutionScopeFromHeaders(r.Header)
	oaiReq, err := translateGeminiToOpenAIWithOptions(req, pathModel, stream, geminiTranslationOptions{unwrapCLITextOutput: isGeminiCLIUserAgent(r.UserAgent())})
	if err != nil {
		h.writeGeminiProtocolError(w, err)
		return
	}
	h.maybeReduceOpenAIChatToolOutputs(r.Context(), oaiReq, h.toolContexts, scope)

	forceStream := !stream && len(oaiReq.Tools) > 0 && !openAIToolChoiceIsNone(oaiReq.ToolChoice)
	if forceStream {
		streamFlag := true
		oaiReq.Stream = &streamFlag
	}
	streamUsageInjected := false
	if forceStream || stream {
		// Force-streamed or client-streamed: request a usage chunk so the
		// request records tokens (the usage callback on the stream branch reads
		// it). The translated Gemini stream consumes the OpenAI usage chunk, so
		// it is not forwarded raw to the client. A strict OpenAI-compatible
		// provider may reject stream_options; retry once without it on 400.
		oaiReq.StreamOptions = &models.StreamOptions{IncludeUsage: true}
		streamUsageInjected = true
	}

	oaiBody, err := json.Marshal(oaiReq)
	if err != nil {
		writeGeminiError(w, http.StatusInternalServerError, "INTERNAL", "failed to marshal request")
		return
	}

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContextFrom(r.Context(), stream || forceStream)
	defer upstreamCancel()
	upstreamCtx = withRouteOperation(upstreamCtx, routeOperationFromContext(r.Context()))
	upstreamCtx, routeOperation, _, err := h.withExplicitRouteOperation(upstreamCtx, r.Context(), oaiReq.Model, providerEndpointChatCompletions)
	if err != nil {
		h.writeGeminiUpstreamFailure(w, err)
		return
	}
	if routeOperation != nil {
		w.Header().Set("X-Vekil-Request-ID", routeOperation.operationID())
	}

	mode := chatCompletionsMode{
		clientRequestedStream: stream,
		forceUpstreamStream:   forceStream,
		injectedStreamUsage:   streamUsageInjected,
	}

	if routeOperation != nil {
		resp, err := h.executeChatCompletionsRouteRequest(upstreamCtx, oaiBody, mode)
		if err != nil {
			if h.handleShutdownError(w, r, upstreamCtx, err) {
				return
			}
			h.writeGeminiUpstreamFailure(w, err)
			return
		}

		resp, oaiBody, mode = h.retryChatCompletionsWithoutInjectedStreamOptions(upstreamCtx, resp, oaiBody, mode)
		observeUpstreamHeaders(r.Context(), resp.Header)

		writeAggregatedResponse := func(oaiResp *models.OpenAIResponse) {
			markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentSemantic)
			observeOpenAIUsage(r.Context(), oaiResp.Usage)
			h.maybeRewriteOrCaptureOpenAIChatToolCommands(r.Context(), oaiResp, h.toolContexts, scope, false)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(TranslateOpenAIToGemini(oaiResp))
		}

		if mode.forceUpstreamStream {
			oaiResp, finalResp, aggregateErr := h.aggregateExplicitChatCompletionsResponse(upstreamCtx, resp, oaiBody, mode, aggregateGeminiStreamToResponseWithProgress)
			if aggregateErr != nil {
				if h.handleShutdownError(w, r, upstreamCtx, aggregateErr) {
					return
				}
				status := http.StatusBadGateway
				message := aggregateErr.Error()
				var streamErr *openAIStreamError
				if errors.As(aggregateErr, &streamErr) {
					status = streamErr.httpStatus()
					message = streamErr.Error()
				}
				writeGeminiError(w, status, mapGeminiUpstreamStatus(status), message)
				return
			}
			if finalResp != nil {
				resp = finalResp
			} else {
				writeAggregatedResponse(oaiResp)
				return
			}
		}

		if resp.StatusCode != http.StatusOK {
			defer func() { _ = resp.Body.Close() }()
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			detail := formatUpstreamErrorMessage(resp.StatusCode, errBody)
			h.log.Error("upstream error",
				logger.F("endpoint", "gemini"),
				logger.F("status", resp.StatusCode),
				logger.F("detail", detail),
				logger.F("request_bytes", len(oaiBody)),
			)
			h.log.Debug("upstream error body", logger.F("endpoint", "gemini"), logger.F("status", resp.StatusCode), logger.F("body", string(errBody)))
			writeGeminiError(w, resp.StatusCode, mapGeminiUpstreamStatus(resp.StatusCode), detail)
			return
		}

		err = h.routeChatCompletionsResponse(w, resp, upstreamCtx, mode, chatCompletionsResponseHandlers{
			stream: func(resp *http.Response) {
				markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentProtocolFrame)
				body := newLifecycleAwareReadCloser(resp.Body, upstreamCtx)
				streamOpenAIToGeminiWithLifecycle(w, body, func(status int) { observeResponseFailureStatus(r.Context(), status) }, h.openAIChatStreamFinalResponseCallback(r.Context(), h.toolContexts, scope), h.lifecycleStreamHooks(r.Context(), body.canceledAtFailure, func() { h.WriteShutdownServiceUnavailable(w, r) }), openAIChatStreamUsageCallback(r.Context()))
			},
			passthrough: func(resp *http.Response) error {
				body := newLifecycleAwareReadCloser(resp.Body, upstreamCtx)
				defer func() { _ = body.Close() }()
				var parsed models.OpenAIResponse
				if err := json.NewDecoder(body).Decode(&parsed); err != nil {
					if body.canceledAtFailure() {
						return context.Canceled
					}
					return err
				}
				observeOpenAIUsage(r.Context(), parsed.Usage)
				h.maybeRewriteOrCaptureOpenAIChatToolCommands(r.Context(), &parsed, h.toolContexts, scope, false)

				markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentSemantic)
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(TranslateOpenAIToGemini(&parsed))
				return nil
			},
		})
		if err != nil {
			if h.handleShutdownError(w, r, upstreamCtx, err) {
				return
			}
			writeGeminiError(w, http.StatusInternalServerError, "INTERNAL", "failed to parse upstream response")
		}
		return
	}

	result, err := h.executeChatCompletions(upstreamCtx, oaiBody, chatExecutionOptions{})
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
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

	result, oaiBody, mode = h.retryChatExecutionWithoutInjectedStreamOptions(upstreamCtx, result, oaiBody, mode)
	observeChatExecutionRoute(r.Context(), result)
	observeUpstreamHeaders(r.Context(), chatExecutionUpstreamHeaders(result))
	if result.Backend == chatBackendResponses && len(result.Headers) > 0 {
		mergeHeaderValues(w.Header(), result.Headers)
	}

	if result.Response != nil && result.Response.StatusCode != http.StatusOK {
		resp := result.Response
		defer func() { _ = resp.Body.Close() }()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		detail := formatUpstreamErrorMessage(resp.StatusCode, errBody)
		h.log.Error("upstream error",
			logger.F("endpoint", "gemini"),
			logger.F("status", resp.StatusCode),
			logger.F("detail", detail),
			logger.F("request_bytes", len(oaiBody)),
		)
		h.log.Debug("upstream error body", logger.F("endpoint", "gemini"), logger.F("status", resp.StatusCode), logger.F("body", string(errBody)))
		writeGeminiError(w, resp.StatusCode, mapGeminiUpstreamStatus(resp.StatusCode), detail)
		return
	}

	writeAggregatedResponse := func(oaiResp *models.OpenAIResponse) {
		observeOpenAIUsage(r.Context(), oaiResp.Usage)
		h.maybeRewriteOrCaptureOpenAIChatToolCommands(r.Context(), oaiResp, h.toolContexts, scope, false)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(TranslateOpenAIToGemini(oaiResp))
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
			aggregateErr = fmt.Errorf("forced stream returned no stream")
		}
		if aggregateErr != nil {
			if h.handleShutdownError(w, r, upstreamCtx, aggregateErr) {
				return
			}
			status := http.StatusBadGateway
			message := aggregateErr.Error()
			var streamErr *chatExecutionError
			if errors.As(aggregateErr, &streamErr) {
				status = streamErr.StatusCode
				message = streamErr.Message
				observeOpenAIUsage(r.Context(), streamErr.Usage)
			} else {
				var nativeStreamErr *openAIStreamError
				if errors.As(aggregateErr, &nativeStreamErr) {
					status = nativeStreamErr.httpStatus()
					message = nativeStreamErr.Error()
				}
			}
			writeGeminiError(w, status, mapGeminiUpstreamStatus(status), message)
			return
		}
		writeAggregatedResponse(oaiResp)
		return
	}
	if result.Completion != nil {
		writeAggregatedResponse(result.Completion)
		return
	}
	if result.Stream != nil {
		tracked := &commitTrackingResponseWriter{ResponseWriter: w}
		err := streamChatEventsToGemini(tracked, result.Stream, chatStreamEventCallbacks{
			OnUsage: openAIChatStreamUsageCallback(r.Context()),
			OnFinal: h.openAIChatStreamFinalResponseCallback(r.Context(), h.toolContexts, scope),
		})
		if h.handleCanonicalChatStreamLifecycleError(w, r, upstreamCtx, tracked.committed, err, func() {
			_ = writeGeminiSSEData(tracked, models.GeminiErrorResponse{
				Error: models.GeminiError{
					Code:    http.StatusServiceUnavailable,
					Message: "server shutting down",
					Status:  "UNAVAILABLE",
				},
			})
		}) {
			return
		}
		var streamErr *chatExecutionError
		if errors.As(err, &streamErr) {
			observeOpenAIUsage(r.Context(), streamErr.Usage)
			observeResponseFailureStatus(r.Context(), chatStreamErrorStatus(streamErr))
		} else if terminalErr := chatExecutionErrorFromStreamTermination(err); terminalErr != nil {
			observeResponseFailureStatus(r.Context(), terminalErr.StatusCode)
			_ = writeGeminiSSEData(tracked, models.GeminiErrorResponse{Error: models.GeminiError{Code: terminalErr.StatusCode, Message: terminalErr.Message, Status: mapGeminiUpstreamStatus(terminalErr.StatusCode)}})
		}
		return
	}
	resp := result.Response
	err = h.routeChatCompletionsResponse(w, resp, upstreamCtx, mode, chatCompletionsResponseHandlers{
		stream: func(resp *http.Response) {
			body := newLifecycleAwareReadCloser(resp.Body, upstreamCtx)
			streamOpenAIToGeminiWithLifecycle(w, body, func(status int) { observeResponseFailureStatus(r.Context(), status) }, h.openAIChatStreamFinalResponseCallback(r.Context(), h.toolContexts, scope), h.lifecycleStreamHooks(r.Context(), body.canceledAtFailure, func() { h.WriteShutdownServiceUnavailable(w, r) }), openAIChatStreamUsageCallback(r.Context()))
		},
		passthrough: func(resp *http.Response) error {
			body := newLifecycleAwareReadCloser(resp.Body, upstreamCtx)
			defer func() { _ = body.Close() }()
			var parsed models.OpenAIResponse
			if err := json.NewDecoder(body).Decode(&parsed); err != nil {
				if body.canceledAtFailure() {
					return context.Canceled
				}
				return err
			}
			observeOpenAIUsage(r.Context(), parsed.Usage)
			h.maybeRewriteOrCaptureOpenAIChatToolCommands(r.Context(), &parsed, h.toolContexts, scope, false)

			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(TranslateOpenAIToGemini(&parsed))
			return nil
		},
	})
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		writeGeminiError(w, http.StatusInternalServerError, "INTERNAL", "failed to parse upstream response")
	}
}

func (h *ProxyHandler) handleGeminiCountTokens(w http.ResponseWriter, r *http.Request, pathModel string) {
	body, err := readBody(r)
	if err != nil {
		if h.handleShutdownError(w, r, nil, err) {
			return
		}
		status := readBodyStatusCode(err)
		writeGeminiError(w, status, "INVALID_ARGUMENT", err.Error())
		return
	}
	defer func() { _ = r.Body.Close() }()
	if err := h.validateRouteAwareRequestJSON(body, normalizeGeminiModelName(pathModel), providerEndpointChatCompletions); err != nil {
		writeGeminiError(w, http.StatusBadRequest, "INVALID_ARGUMENT", err.Error())
		return
	}

	req, err := decodeGeminiCountTokensRequest(body)
	if err != nil {
		h.writeGeminiProtocolError(w, err)
		return
	}

	oaiReq, err := TranslateGeminiCountTokens(req, pathModel)
	if err != nil {
		h.writeGeminiProtocolError(w, err)
		return
	}
	h.observeRequestSummary(r.Context(), "gemini_count_tokens", pathModel, false, providerEndpointChatCompletions)

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(false)
	defer upstreamCancel()
	upstreamCtx = withRouteOperation(upstreamCtx, routeOperationFromContext(r.Context()))
	upstreamCtx, routeOperation, route, err := h.withExplicitRouteOperation(upstreamCtx, suppressRouteAttemptStats(r.Context()), oaiReq.Model, providerEndpointChatCompletions)
	if err != nil {
		h.writeGeminiUpstreamFailure(w, err)
		return
	}
	if routeOperation != nil {
		w.Header().Set("X-Vekil-Request-ID", routeOperation.operationID())
	}

	requestCacheKey, err := hashOpenAIRequest(oaiReq)
	if err != nil {
		writeGeminiError(w, http.StatusInternalServerError, "INTERNAL", "failed to hash countTokens request")
		return
	}

	lookupTargetID := geminiCountTokensSelectedTargetID(route, routeOperation)
	if cacheKey, ok := geminiCountTokensRouteCacheKey(requestCacheKey, route, lookupTargetID); ok {
		if cached, hit := h.getGeminiCountTokensCache(cacheKey); hit {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(cached)
			return
		}
	}

	oaiResp, err := h.runGeminiCountTokensProbeWithContext(upstreamCtx, oaiReq)
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		var estimateErr *geminiCountTokensEstimateError
		if errors.As(err, &estimateErr) {
			estimated := buildEstimatedGeminiCountTokensResult(req, estimateOpenAIRequestTokens(oaiReq))
			h.log.Debug("using estimated Gemini token count", logger.F("reason", estimateErr.reason), logger.F("total_tokens", estimated.TotalTokens), logger.Err(estimateErr.Unwrap()))
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(estimated)
			return
		}
		h.writeGeminiProtocolError(w, err)
		return
	}

	if oaiResp.Usage == nil {
		estimated := buildEstimatedGeminiCountTokensResult(req, estimateOpenAIRequestTokens(oaiReq))
		h.log.Debug("using estimated Gemini token count", logger.F("reason", "missing_usage"), logger.F("total_tokens", estimated.TotalTokens))
		h.cacheGeminiCountTokensResult(requestCacheKey, route, routeOperation, estimated)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(estimated)
		return
	}
	observeOpenAIUsage(r.Context(), oaiResp.Usage)

	result := models.GeminiCountTokensResponse{
		TotalTokens: oaiResp.Usage.PromptTokens,
	}
	if !geminiRequestHasInlineMedia(req) {
		result.PromptTokensDetails = []models.GeminiTokenCountDetails{{
			Modality:   "TEXT",
			TokenCount: oaiResp.Usage.PromptTokens,
		}}
	}

	h.cacheGeminiCountTokensResult(requestCacheKey, route, routeOperation, result)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
}

// geminiCountTokensSelectedTargetID mirrors the next target dispatch would choose
// without consuming any route-attempt budget.
func geminiCountTokensSelectedTargetID(route *modelRoute, operation *routeOperation) string {
	if route == nil || route.legacy || operation == nil {
		return ""
	}
	targets := orderedRouteTargets(route, operation, providerEndpointChatCompletions)
	if len(targets) == 0 {
		return ""
	}
	return targets[0].id
}

// geminiCountTokensRouteCacheKey preserves the legacy request-only key while
// isolating explicit-route entries by their physical tokenizer/model owner.
func geminiCountTokensRouteCacheKey(requestKey string, route *modelRoute, targetID string) (string, bool) {
	if route == nil || route.legacy {
		return requestKey, true
	}
	target, ok := route.targetByID(targetID)
	if !ok || target.provider == nil {
		return "", false
	}
	return strings.Join([]string{
		requestKey,
		route.public.routeID,
		target.id,
		target.provider.id,
		target.upstreamModel,
	}, "\x00"), true
}

func (h *ProxyHandler) cacheGeminiCountTokensResult(requestKey string, route *modelRoute, operation *routeOperation, response models.GeminiCountTokensResponse) {
	targetID := ""
	if operation != nil {
		targetID = operation.pinnedTarget()
	}
	cacheKey, ok := geminiCountTokensRouteCacheKey(requestKey, route, targetID)
	if !ok {
		return
	}
	h.setGeminiCountTokensCache(cacheKey, response)
}

func buildEstimatedGeminiCountTokensResult(req *models.GeminiGenerateContentRequest, total int) models.GeminiCountTokensResponse {
	if total <= 0 {
		total = 1
	}
	result := models.GeminiCountTokensResponse{TotalTokens: total}
	if !geminiRequestHasInlineMedia(req) {
		result.PromptTokensDetails = []models.GeminiTokenCountDetails{{
			Modality:   "TEXT",
			TokenCount: total,
		}}
	}
	return result
}

func estimateOpenAIRequestTokens(req *models.OpenAIRequest) int {
	if req == nil {
		return 1
	}
	bytes := len(req.Model)
	for _, msg := range req.Messages {
		bytes += len(msg.Role) + len(msg.Name) + len(msg.Content) + len(msg.ToolCallID)
		for _, call := range msg.ToolCalls {
			bytes += len(call.ID) + len(call.Type) + len(call.Function.Name) + len(call.Function.Arguments)
		}
	}
	for _, tool := range req.Tools {
		encoded, _ := json.Marshal(tool)
		bytes += len(encoded)
	}
	if len(req.ResponseFormat) > 0 {
		bytes += len(req.ResponseFormat)
	}
	estimate := bytes / 4
	if bytes%4 != 0 {
		estimate++
	}
	if estimate <= 0 {
		estimate = 1
	}
	return estimate
}

func geminiRequestHasInlineMedia(req *models.GeminiGenerateContentRequest) bool {
	if req == nil {
		return false
	}

	if geminiContentHasInlineMedia(req.SystemInstruction) {
		return true
	}

	for idx := range req.Contents {
		if geminiContentHasInlineMedia(&req.Contents[idx]) {
			return true
		}
	}

	return false
}

func geminiContentHasInlineMedia(content *models.GeminiContent) bool {
	if content == nil {
		return false
	}

	for _, part := range content.Parts {
		if hasRawJSON(part.InlineData) {
			return true
		}
	}

	return false
}

func (h *ProxyHandler) runGeminiCountTokensProbe(baseReq *models.OpenAIRequest) (*models.OpenAIResponse, error) {
	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(false)
	defer upstreamCancel()
	return h.runGeminiCountTokensProbeWithContext(upstreamCtx, baseReq)
}

func (h *ProxyHandler) runGeminiCountTokensProbeWithContext(upstreamCtx context.Context, baseReq *models.OpenAIRequest) (*models.OpenAIResponse, error) {
	probeReq := copyOpenAIRequestForGeminiCountTokensProbe(baseReq)

	streamFlag := false
	temperature := 0.0
	one := 1

	probeReq.Stream = &streamFlag
	probeReq.StreamOptions = nil
	probeReq.Temperature = &temperature
	probeReq.MaxCompletionTokens = &one
	probeReq.MaxTokens = nil

	if routeOperationFromContext(upstreamCtx) != nil {
		oaiResp, fallback, err := h.executeGeminiCountTokensProbe(upstreamCtx, probeReq)
		if fallback {
			probeReq.MaxCompletionTokens = nil
			probeReq.MaxTokens = &one
			return h.executeGeminiCountTokensProbeFinal(withRouteAttemptKind(upstreamCtx, routeAttemptProtocolRecovery), probeReq)
		}
		return oaiResp, err
	}

	body, err := json.Marshal(probeReq)
	if err != nil {
		return nil, &geminiProtocolError{statusCode: http.StatusInternalServerError, status: "INTERNAL", message: "failed to marshal countTokens probe request"}
	}
	result, err := h.executeChatCompletions(upstreamCtx, body, chatExecutionOptions{ResponsesMinimumOutputTokens: responsesChatMinimumOutputTokens, ResponsesDropSamplingParams: true, ResponsesUsageOnly: true})
	if err != nil {
		return nil, mapGeminiCountTokensTransportError(err)
	}
	if result.Backend == chatBackendNativeChat && result.Response != nil && result.Response.StatusCode == http.StatusBadRequest && probeReq.MaxCompletionTokens != nil {
		_ = result.Response.Body.Close()
		probeReq.MaxCompletionTokens = nil
		probeReq.MaxTokens = &one
		fallbackBody, marshalErr := json.Marshal(probeReq)
		if marshalErr != nil {
			return nil, &geminiProtocolError{statusCode: http.StatusInternalServerError, status: "INTERNAL", message: "failed to marshal countTokens fallback request"}
		}
		result, err = h.retryResolvedNativeChat(upstreamCtx, result, fallbackBody)
		if err != nil {
			return nil, mapGeminiCountTokensTransportError(err)
		}
	}
	return h.decodeGeminiProbeExecution(result)
}

func copyOpenAIRequestForGeminiCountTokensProbe(baseReq *models.OpenAIRequest) *models.OpenAIRequest {
	// The probe path only replaces top-level fields below (streaming, token
	// limits, and temperature). Shared slices/raw messages remain read-only, so a
	// struct copy avoids the JSON round-trip deep clone on every countTokens miss.
	if baseReq == nil {
		return &models.OpenAIRequest{}
	}
	probeReq := *baseReq
	return &probeReq
}

func (h *ProxyHandler) executeGeminiCountTokensProbe(upstreamCtx context.Context, probeReq *models.OpenAIRequest) (*models.OpenAIResponse, bool, error) {
	body, err := json.Marshal(probeReq)
	if err != nil {
		return nil, false, &geminiProtocolError{
			statusCode: http.StatusInternalServerError,
			status:     "INTERNAL",
			message:    "failed to marshal countTokens probe request",
		}
	}

	resp, err := h.postChatCompletions(upstreamCtx, body)
	if err != nil {
		return nil, false, mapGeminiCountTokensTransportError(err)
	}

	if resp.StatusCode == http.StatusBadRequest && probeReq.MaxCompletionTokens != nil {
		_ = resp.Body.Close()
		return nil, true, nil
	}

	return h.decodeGeminiProbeResponse(resp)
}

func (h *ProxyHandler) executeGeminiCountTokensProbeFinal(upstreamCtx context.Context, probeReq *models.OpenAIRequest) (*models.OpenAIResponse, error) {
	body, err := json.Marshal(probeReq)
	if err != nil {
		return nil, &geminiProtocolError{
			statusCode: http.StatusInternalServerError,
			status:     "INTERNAL",
			message:    "failed to marshal countTokens probe request",
		}
	}

	resp, err := h.postChatCompletions(upstreamCtx, body)
	if err != nil {
		return nil, mapGeminiCountTokensTransportError(err)
	}

	oaiResp, _, err := h.decodeGeminiProbeResponse(resp)
	return oaiResp, err
}

func (h *ProxyHandler) decodeGeminiProbeExecution(result chatExecutionResult) (*models.OpenAIResponse, error) {
	if result.Completion != nil {
		result.Completion.Usage = result.Usage
		return result.Completion, nil
	}
	if result.Response != nil {
		oaiResp, _, err := h.decodeGeminiProbeResponse(result.Response)
		return oaiResp, err
	}
	return nil, &geminiProtocolError{statusCode: http.StatusInternalServerError, status: "INTERNAL", message: "countTokens probe returned no Chat completion"}
}

func (h *ProxyHandler) decodeGeminiProbeResponse(resp *http.Response) (*models.OpenAIResponse, bool, error) {
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		detail := formatUpstreamErrorMessage(resp.StatusCode, errBody)
		h.log.Error("upstream error", logger.F("endpoint", "gemini_count_tokens"), logger.F("status", resp.StatusCode), logger.F("detail", detail))
		h.log.Debug("upstream error body", logger.F("endpoint", "gemini_count_tokens"), logger.F("status", resp.StatusCode), logger.F("body", string(errBody)))
		protocolErr := &geminiProtocolError{
			statusCode: resp.StatusCode,
			status:     mapGeminiUpstreamStatus(resp.StatusCode),
			message:    detail,
		}
		if geminiCountTokensShouldEstimateStatus(resp.StatusCode) {
			return nil, false, &geminiCountTokensEstimateError{reason: fmt.Sprintf("upstream_status_%d", resp.StatusCode), err: protocolErr}
		}
		return nil, false, protocolErr
	}

	var oaiResp models.OpenAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&oaiResp); err != nil {
		return nil, false, &geminiProtocolError{
			statusCode: http.StatusInternalServerError,
			status:     "INTERNAL",
			message:    "failed to parse upstream countTokens probe response",
			cause:      err,
		}
	}

	return &oaiResp, false, nil
}

type geminiCountTokensEstimateError struct {
	reason string
	err    error
}

func (e *geminiCountTokensEstimateError) Error() string {
	if e == nil || e.err == nil {
		return "Gemini countTokens estimate fallback"
	}
	return e.err.Error()
}

func (e *geminiCountTokensEstimateError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func geminiCountTokensShouldEstimateStatus(statusCode int) bool {
	return statusCode == http.StatusTooManyRequests || statusCode >= http.StatusInternalServerError
}

func mapGeminiCountTokensTransportError(err error) error {
	var executionErr *chatExecutionError
	if errors.As(err, &executionErr) {
		statusCode := chatStreamErrorStatus(executionErr)
		protocolErr := &geminiProtocolError{
			statusCode: statusCode,
			status:     mapGeminiUpstreamStatus(statusCode),
			message:    chatStreamErrorMessage(executionErr),
			cause:      err,
		}
		if geminiCountTokensShouldEstimateStatus(statusCode) {
			return &geminiCountTokensEstimateError{reason: fmt.Sprintf("upstream_status_%d", statusCode), err: protocolErr}
		}
		return protocolErr
	}
	var providerErr *providerRequestError
	if errors.As(err, &providerErr) {
		return mapGeminiTransportError(err)
	}
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) && geminiCountTokensShouldEstimateStatus(upstreamErr.statusCode) {
		return &geminiCountTokensEstimateError{reason: fmt.Sprintf("upstream_status_%d", upstreamErr.statusCode), err: mapGeminiTransportError(err)}
	}
	if permanentTransportError(err) {
		return mapGeminiTransportError(err)
	}
	return &geminiCountTokensEstimateError{reason: "transport_error", err: mapGeminiTransportError(err)}
}

func (h *ProxyHandler) writeGeminiProtocolError(w http.ResponseWriter, err error) {
	var geminiErr *geminiProtocolError
	if errors.As(err, &geminiErr) {
		writeGeminiError(w, geminiErr.statusCode, geminiErr.status, geminiErr.message)
		return
	}
	writeGeminiError(w, http.StatusInternalServerError, "INTERNAL", err.Error())
}

func (h *ProxyHandler) writeGeminiUpstreamFailure(w http.ResponseWriter, err error) {
	writeErr := mapGeminiTransportError(err)
	h.writeGeminiProtocolError(w, writeErr)
}

func mapGeminiTransportError(err error) error {
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) {
		return &geminiProtocolError{
			statusCode: upstreamErr.statusCode,
			status:     mapGeminiUpstreamStatus(upstreamErr.statusCode),
			message:    fmt.Sprintf("upstream request failed: %v", err),
			cause:      err,
		}
	}
	var providerErr *providerRequestError
	if errors.As(err, &providerErr) {
		return &geminiProtocolError{
			statusCode: providerErr.statusCode,
			status:     mapGeminiUpstreamStatus(providerErr.statusCode),
			message:    err.Error(),
			cause:      err,
		}
	}

	return &geminiProtocolError{
		statusCode: http.StatusInternalServerError,
		status:     "INTERNAL",
		message:    fmt.Sprintf("upstream request failed: %v", err),
		cause:      err,
	}
}

func mapGeminiUpstreamStatus(statusCode int) string {
	switch statusCode {
	case http.StatusBadRequest:
		return "INVALID_ARGUMENT"
	case http.StatusUnauthorized:
		return "UNAUTHENTICATED"
	case http.StatusForbidden:
		return "PERMISSION_DENIED"
	case http.StatusNotFound:
		return "NOT_FOUND"
	case http.StatusTooManyRequests:
		return "RESOURCE_EXHAUSTED"
	case http.StatusBadGateway, http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return "UNAVAILABLE"
	default:
		return "INTERNAL"
	}
}

func writeGeminiError(w http.ResponseWriter, statusCode int, status, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(statusCode)
	_ = json.NewEncoder(w).Encode(models.GeminiErrorResponse{
		Error: models.GeminiError{
			Code:    statusCode,
			Message: message,
			Status:  status,
		},
	})
}

func (h *ProxyHandler) getGeminiCountTokensCache(key string) (models.GeminiCountTokensResponse, bool) {
	now := h.geminiCounts.nowTime()
	h.geminiCounts.mu.Lock()
	defer h.geminiCounts.mu.Unlock()

	pruneExpiredGeminiCountTokensEntries(h.geminiCounts.entries, now)
	entry, ok := h.geminiCounts.entries[key]
	if !ok {
		return models.GeminiCountTokensResponse{}, false
	}

	return entry.response, true
}

func (h *ProxyHandler) setGeminiCountTokensCache(key string, response models.GeminiCountTokensResponse) {
	now := h.geminiCounts.nowTime()
	h.geminiCounts.mu.Lock()
	defer h.geminiCounts.mu.Unlock()

	if h.geminiCounts.entries == nil {
		h.geminiCounts.entries = make(map[string]geminiCountTokensCacheEntry)
	}
	pruneExpiredGeminiCountTokensEntries(h.geminiCounts.entries, now)

	if _, exists := h.geminiCounts.entries[key]; !exists && len(h.geminiCounts.entries) >= geminiCountTokensCacheMaxEntries {
		if evictionKey, ok := oldestGeminiCountTokensCacheEntry(h.geminiCounts.entries); ok {
			delete(h.geminiCounts.entries, evictionKey)
		}
	}
	h.geminiCounts.nextSequence++

	h.geminiCounts.entries[key] = geminiCountTokensCacheEntry{
		response: response,
		expiry:   now.Add(geminiCountTokensCacheTTL),
		sequence: h.geminiCounts.nextSequence,
	}
}

func (c *geminiCountTokensCache) nowTime() time.Time {
	if c != nil && c.now != nil {
		return c.now()
	}
	return time.Now()
}

func pruneExpiredGeminiCountTokensEntries(entries map[string]geminiCountTokensCacheEntry, now time.Time) {
	for key, entry := range entries {
		if now.After(entry.expiry) {
			delete(entries, key)
		}
	}
}

func oldestGeminiCountTokensCacheEntry(entries map[string]geminiCountTokensCacheEntry) (string, bool) {
	oldestKey := ""
	var oldestSequence uint64
	found := false
	for key, entry := range entries {
		if !found || entry.sequence < oldestSequence || (entry.sequence == oldestSequence && key < oldestKey) {
			oldestKey = key
			oldestSequence = entry.sequence
			found = true
		}
	}
	return oldestKey, found
}
