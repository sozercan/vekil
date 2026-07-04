package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

const geminiCountTokensCacheTTL = 60 * time.Second

type geminiCountTokensCache struct {
	mu      sync.RWMutex
	entries map[string]geminiCountTokensCacheEntry
}

type geminiCountTokensCacheEntry struct {
	response models.GeminiCountTokensResponse
	expiry   time.Time
}

// HandleGeminiModels routes Gemini-native model actions to the corresponding
// translation handler.
func (h *ProxyHandler) HandleGeminiModels(w http.ResponseWriter, r *http.Request) {
	model, action, err := parseGeminiPath(r.URL.Path)
	if err != nil {
		h.writeGeminiProtocolError(w, err)
		return
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
		status := readBodyStatusCode(err)
		writeGeminiError(w, status, "INVALID_ARGUMENT", err.Error())
		return
	}
	defer func() { _ = r.Body.Close() }()

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
	oaiReq, err := TranslateGeminiToOpenAI(req, pathModel, stream)
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

	resp, err := h.postChatCompletions(upstreamCtx, oaiBody)
	if err != nil {
		h.writeGeminiUpstreamFailure(w, err)
		return
	}

	mode := chatCompletionsMode{
		clientRequestedStream: stream,
		forceUpstreamStream:   forceStream,
		injectedStreamUsage:   streamUsageInjected,
	}
	resp, oaiBody, mode = h.retryChatCompletionsWithoutInjectedStreamOptions(upstreamCtx, resp, oaiBody, mode)
	observeUpstreamHeaders(r.Context(), resp.Header)

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
		writeGeminiError(w, resp.StatusCode, mapGeminiUpstreamStatus(resp.StatusCode), formatUpstreamErrorMessage(resp.StatusCode, errBody))
		return
	}

	err = h.routeChatCompletionsResponse(w, resp, mode, chatCompletionsResponseHandlers{
		stream: func(resp *http.Response) {
			StreamOpenAIToGeminiWithFinalResponse(w, resp.Body, func(status int) { observeResponseFailureStatus(r.Context(), status) }, h.openAIChatStreamFinalResponseCallback(r.Context(), h.toolContexts, scope), openAIChatStreamUsageCallback(r.Context()))
		},
		aggregate: func(oaiResp *models.OpenAIResponse) {
			observeOpenAIUsage(r.Context(), oaiResp.Usage)
			h.maybeRewriteOrCaptureOpenAIChatToolCommands(r.Context(), oaiResp, h.toolContexts, scope, false)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(TranslateOpenAIToGemini(oaiResp))
		},
		passthrough: func(resp *http.Response) error {
			defer func() { _ = resp.Body.Close() }()
			var parsed models.OpenAIResponse
			if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
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
		message := "failed to parse upstream response"
		if mode.forceUpstreamStream {
			message = "failed to aggregate upstream response"
		}
		writeGeminiError(w, http.StatusInternalServerError, "INTERNAL", message)
	}
}

func (h *ProxyHandler) handleGeminiCountTokens(w http.ResponseWriter, r *http.Request, pathModel string) {
	body, err := readBody(r)
	if err != nil {
		status := readBodyStatusCode(err)
		writeGeminiError(w, status, "INVALID_ARGUMENT", err.Error())
		return
	}
	defer func() { _ = r.Body.Close() }()

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

	cacheKey, err := hashOpenAIRequest(oaiReq)
	if err != nil {
		writeGeminiError(w, http.StatusInternalServerError, "INTERNAL", "failed to hash countTokens request")
		return
	}

	if cached, ok := h.getGeminiCountTokensCache(cacheKey); ok {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(cached)
		return
	}

	oaiResp, err := h.runGeminiCountTokensProbe(oaiReq)
	if err != nil {
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
		h.setGeminiCountTokensCache(cacheKey, estimated)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(estimated)
		return
	}

	result := models.GeminiCountTokensResponse{
		TotalTokens: oaiResp.Usage.PromptTokens,
	}
	if !geminiRequestHasInlineMedia(req) {
		result.PromptTokensDetails = []models.GeminiTokenCountDetails{{
			Modality:   "TEXT",
			TokenCount: oaiResp.Usage.PromptTokens,
		}}
	}

	h.setGeminiCountTokensCache(cacheKey, result)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(result)
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
	probeReq := copyOpenAIRequestForGeminiCountTokensProbe(baseReq)

	streamFlag := false
	temperature := 0.0
	one := 1

	probeReq.Stream = &streamFlag
	probeReq.StreamOptions = nil
	probeReq.Temperature = &temperature
	probeReq.MaxCompletionTokens = &one
	probeReq.MaxTokens = nil

	oaiResp, fallback, err := h.executeGeminiCountTokensProbe(probeReq)
	if fallback {
		probeReq.MaxCompletionTokens = nil
		probeReq.MaxTokens = &one
		return h.executeGeminiCountTokensProbeFinal(probeReq)
	}
	return oaiResp, err
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

func (h *ProxyHandler) executeGeminiCountTokensProbe(probeReq *models.OpenAIRequest) (*models.OpenAIResponse, bool, error) {
	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(false)
	defer upstreamCancel()

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

func (h *ProxyHandler) executeGeminiCountTokensProbeFinal(probeReq *models.OpenAIRequest) (*models.OpenAIResponse, error) {
	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(false)
	defer upstreamCancel()

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
		}
	}
	var providerErr *providerRequestError
	if errors.As(err, &providerErr) {
		return &geminiProtocolError{
			statusCode: providerErr.statusCode,
			status:     mapGeminiUpstreamStatus(providerErr.statusCode),
			message:    fmt.Sprintf("upstream request failed: %v", err),
		}
	}

	return &geminiProtocolError{
		statusCode: http.StatusInternalServerError,
		status:     "INTERNAL",
		message:    fmt.Sprintf("upstream request failed: %v", err),
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
	h.geminiCounts.mu.RLock()
	entry, ok := h.geminiCounts.entries[key]
	h.geminiCounts.mu.RUnlock()
	if !ok {
		return models.GeminiCountTokensResponse{}, false
	}

	if time.Now().After(entry.expiry) {
		h.geminiCounts.mu.Lock()
		delete(h.geminiCounts.entries, key)
		h.geminiCounts.mu.Unlock()
		return models.GeminiCountTokensResponse{}, false
	}

	return entry.response, true
}

func (h *ProxyHandler) setGeminiCountTokensCache(key string, response models.GeminiCountTokensResponse) {
	h.geminiCounts.mu.Lock()
	defer h.geminiCounts.mu.Unlock()

	if h.geminiCounts.entries == nil {
		h.geminiCounts.entries = make(map[string]geminiCountTokensCacheEntry)
	}

	h.geminiCounts.entries[key] = geminiCountTokensCacheEntry{
		response: response,
		expiry:   time.Now().Add(geminiCountTokensCacheTTL),
	}
}
