package proxy

import (
	"bytes"
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

	oaiReq, err := TranslateGeminiToOpenAI(req, pathModel, stream)
	if err != nil {
		h.writeGeminiProtocolError(w, err)
		return
	}

	forceStream := !stream && len(oaiReq.Tools) > 0
	if forceStream {
		streamFlag := true
		oaiReq.Stream = &streamFlag
		oaiReq.StreamOptions = &models.StreamOptions{IncludeUsage: true}
	}

	oaiBody, err := json.Marshal(oaiReq)
	if err != nil {
		writeGeminiError(w, http.StatusInternalServerError, "INTERNAL", "failed to marshal request")
		return
	}

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(stream || forceStream)
	defer upstreamCancel()

	resp, err := h.postChatCompletions(upstreamCtx, oaiBody)
	if err != nil {
		h.writeGeminiUpstreamFailure(w, err)
		return
	}

	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		h.log.Error("upstream error", logger.F("endpoint", "gemini"), logger.F("status", resp.StatusCode), logger.F("body", string(errBody)), logger.F("request", string(oaiBody)))
		writeGeminiError(w, resp.StatusCode, mapGeminiUpstreamStatus(resp.StatusCode), fmt.Sprintf("upstream error (%d)", resp.StatusCode))
		return
	}

	mode := chatCompletionsMode{
		clientRequestedStream: stream,
		forceUpstreamStream:   forceStream,
	}
	err = h.routeChatCompletionsResponse(w, resp, mode, chatCompletionsResponseHandlers{
		stream: func(resp *http.Response) {
			StreamOpenAIToGemini(w, resp.Body)
		},
		aggregate: func(oaiResp *models.OpenAIResponse) {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(TranslateOpenAIToGemini(oaiResp))
		},
		passthrough: func(resp *http.Response) error {
			defer func() { _ = resp.Body.Close() }()
			var parsed models.OpenAIResponse
			if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
				return err
			}

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
		h.writeGeminiProtocolError(w, err)
		return
	}

	if oaiResp.Usage == nil {
		writeGeminiError(w, http.StatusInternalServerError, "INTERNAL", "upstream response did not include usage")
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
	probeReq, err := cloneOpenAIRequest(baseReq)
	if err != nil {
		return nil, &geminiProtocolError{
			statusCode: http.StatusInternalServerError,
			status:     "INTERNAL",
			message:    "failed to clone countTokens request",
		}
	}

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
		return nil, false, mapGeminiTransportError(err)
	}

	if shouldFallbackGeminiCountTokensProbe(resp, probeReq) {
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
		return nil, mapGeminiTransportError(err)
	}

	oaiResp, _, err := h.decodeGeminiProbeResponse(resp)
	return oaiResp, err
}

func (h *ProxyHandler) decodeGeminiProbeResponse(resp *http.Response) (*models.OpenAIResponse, bool, error) {
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		h.log.Error("upstream error", logger.F("endpoint", "gemini_count_tokens"), logger.F("status", resp.StatusCode), logger.F("body", string(errBody)))
		return nil, false, &geminiProtocolError{
			statusCode: resp.StatusCode,
			status:     mapGeminiUpstreamStatus(resp.StatusCode),
			message:    fmt.Sprintf("upstream error (%d)", resp.StatusCode),
		}
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

func shouldFallbackGeminiCountTokensProbe(resp *http.Response, probeReq *models.OpenAIRequest) bool {
	if resp == nil || resp.StatusCode != http.StatusBadRequest || probeReq == nil || probeReq.MaxCompletionTokens == nil {
		return false
	}

	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	_ = resp.Body.Close()
	resp.Body = io.NopCloser(bytes.NewReader(body))

	return isGeminiCountTokensMaxCompletionTokensFallback(body)
}

func isGeminiCountTokensMaxCompletionTokensFallback(body []byte) bool {
	message, param, code := extractGeminiCountTokensFallbackError(body)
	if geminiCountTokensMaxCompletionTokensFallbackMatch(message, param, code) {
		return true
	}

	return geminiCountTokensMaxCompletionTokensFallbackMatch(strings.ToLower(strings.TrimSpace(string(body))), "", "")
}

func extractGeminiCountTokensFallbackError(body []byte) (message, param, code string) {
	var envelope struct {
		Error   json.RawMessage `json:"error"`
		Message string          `json:"message"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return "", "", ""
	}

	if len(envelope.Error) != 0 {
		var errorText string
		if err := json.Unmarshal(envelope.Error, &errorText); err == nil {
			return errorText, "", ""
		}

		var details struct {
			Message string `json:"message"`
			Param   string `json:"param"`
			Code    string `json:"code"`
		}
		if err := json.Unmarshal(envelope.Error, &details); err == nil {
			return details.Message, details.Param, details.Code
		}
	}

	return envelope.Message, "", ""
}

func geminiCountTokensMaxCompletionTokensFallbackMatch(message, param, code string) bool {
	normalizedMessage := strings.ToLower(strings.TrimSpace(message))
	normalizedMessage = strings.NewReplacer("'", "", "\"", "", "`", "").Replace(normalizedMessage)
	normalizedMessage = strings.Join(strings.Fields(normalizedMessage), " ")
	param = strings.ToLower(strings.TrimSpace(param))
	code = strings.ToLower(strings.TrimSpace(code))

	if param == "max_completion_tokens" {
		switch code {
		case "unsupported_parameter", "unknown_parameter":
			return true
		}
	}

	return strings.Contains(normalizedMessage, "max_completion_tokens unsupported") ||
		strings.Contains(normalizedMessage, "max_completion_tokens not supported") ||
		strings.Contains(normalizedMessage, "max_completion_tokens is not supported") ||
		strings.Contains(normalizedMessage, "parameter max_completion_tokens is not supported") ||
		strings.Contains(normalizedMessage, "unsupported parameter: max_completion_tokens") ||
		strings.Contains(normalizedMessage, "unknown parameter: max_completion_tokens") ||
		strings.Contains(normalizedMessage, "unrecognized request argument supplied: max_completion_tokens") ||
		strings.Contains(normalizedMessage, "unsupported_parameter: max_completion_tokens") ||
		strings.Contains(normalizedMessage, "unsupported_parameter max_completion_tokens") ||
		strings.Contains(normalizedMessage, "unknown_parameter: max_completion_tokens") ||
		strings.Contains(normalizedMessage, "unknown_parameter max_completion_tokens")
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
