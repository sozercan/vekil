package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/google/uuid"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

type chatCompletionsMode struct {
	clientRequestedStream bool
	forceUpstreamStream   bool
}

type chatCompletionsResponseHandlers struct {
	stream      func(*http.Response)
	aggregate   func(*models.OpenAIResponse)
	passthrough func(*http.Response) error
}

func parseOpenAIChatCompletionsMode(body []byte) chatCompletionsMode {
	var partial struct {
		Stream *bool           `json:"stream,omitempty"`
		Tools  json.RawMessage `json:"tools,omitempty"`
	}
	// Best-effort mode detection only: malformed JSON should still fall through
	// to the real request validation path instead of making this helper another
	// source of hard failures.
	_ = json.Unmarshal(body, &partial)

	clientRequestedStream := partial.Stream != nil && *partial.Stream
	return chatCompletionsMode{
		clientRequestedStream: clientRequestedStream,
		forceUpstreamStream:   !clientRequestedStream && hasNonEmptyTools(partial.Tools),
	}
}

func prepareOpenAIChatCompletionsRequest(body []byte) ([]byte, chatCompletionsMode) {
	mode := parseOpenAIChatCompletionsMode(body)
	body = injectParallelToolCalls(body)
	if mode.forceUpstreamStream {
		body = injectForceStream(body)
	} else if mode.clientRequestedStream {
		// Ask upstream for a usage chunk so streamed traffic records tokens.
		body = ensureStreamUsage(body)
	}
	return body, mode
}

func prepareAnthropicChatCompletionsRequest(req *models.AnthropicRequest) ([]byte, chatCompletionsMode, error) {
	oaiReq, err := TranslateAnthropicToOpenAI(req)
	if err != nil {
		return nil, chatCompletionsMode{}, err
	}

	mode := chatCompletionsMode{
		clientRequestedStream: req.Stream,
		forceUpstreamStream:   !req.Stream,
	}
	if mode.forceUpstreamStream {
		stream := true
		oaiReq.Stream = &stream
		oaiReq.StreamOptions = &models.StreamOptions{IncludeUsage: true}
	}

	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, chatCompletionsMode{}, err
	}
	return body, mode, nil
}

func mapAnthropicUpstreamStatus(statusCode int) string {
	switch statusCode {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		return "overloaded_error"
	case http.StatusInternalServerError:
		return "api_error"
	default:
		return "api_error"
	}
}

func anthropicExtraHeadersFromRequest(r *http.Request) http.Header {
	var headers http.Header
	for _, name := range []string{
		"Anthropic-Version",
		"Anthropic-Beta",
		"Anthropic-Dangerous-Direct-Browser-Access",
	} {
		for _, value := range r.Header.Values(name) {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				if headers == nil {
					headers = make(http.Header, 2)
				}
				headers.Add(name, trimmed)
			}
		}
	}
	return headers
}

func (h *ProxyHandler) shouldForwardAnthropicMessagesDirect(model string) bool {
	provider, _, _ := h.resolveProviderModel(NormalizeModelName(model), providerEndpointMessages)
	return provider != nil && provider.kind == providerTypeAnthropicCompatible
}

func (h *ProxyHandler) shouldForwardAnthropicCountTokensDirect(model string) bool {
	provider, _, _ := h.resolveProviderModel(NormalizeModelName(model), providerEndpointMessages)
	return provider != nil && provider.kind == providerTypeAnthropicCompatible
}

func (h *ProxyHandler) forwardAnthropicMessagesDirect(w http.ResponseWriter, r *http.Request, body []byte, req *models.AnthropicRequest) {
	streaming := req != nil && req.Stream
	publicModel, upstreamModel := h.directAnthropicResponseModels(req)

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(streaming)
	defer upstreamCancel()

	resp, err := h.postAnthropicMessages(upstreamCtx, body, anthropicExtraHeadersFromRequest(r))
	if err != nil {
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		h.log.Error("upstream request failed", logger.F("endpoint", "anthropic"), logger.Err(err))
		if statusCode == http.StatusBadRequest {
			writeAnthropicError(w, statusCode, "invalid_request_error", err.Error())
			return
		}
		writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), formatUpstreamRequestFailure(err, "upstream request failed"))
		return
	}

	if resp.StatusCode == http.StatusOK && streaming {
		writeDirectAnthropicStreamResponse(w, resp, publicModel, upstreamModel)
		return
	}
	if resp.StatusCode == http.StatusOK {
		if err := writeDirectAnthropicJSONResponse(w, resp, publicModel, upstreamModel); err != nil {
			h.log.Error("upstream response rewrite failed", logger.F("endpoint", "anthropic"), logger.Err(err))
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "failed to read upstream response")
		}
		return
	}

	writeUpstreamResponse(w, resp)
}

func (h *ProxyHandler) forwardAnthropicCountTokensDirect(w http.ResponseWriter, r *http.Request, body []byte) {
	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(false)
	defer upstreamCancel()

	resp, err := h.postAnthropicMessagesCountTokens(upstreamCtx, body, anthropicExtraHeadersFromRequest(r))
	if err != nil {
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		h.log.Error("upstream request failed", logger.F("endpoint", "anthropic_count_tokens"), logger.Err(err))
		if statusCode == http.StatusBadRequest {
			writeAnthropicError(w, statusCode, "invalid_request_error", err.Error())
			return
		}
		writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), formatUpstreamRequestFailure(err, "upstream request failed"))
		return
	}

	writeUpstreamResponse(w, resp)
}

func (h *ProxyHandler) directAnthropicResponseModels(req *models.AnthropicRequest) (string, string) {
	if req == nil {
		return "", ""
	}
	publicModel := strings.TrimSpace(req.Model)
	upstreamModel := publicModel
	_, owner, known := h.resolveProviderModel(NormalizeModelName(req.Model), providerEndpointMessages)
	if !known {
		return publicModel, upstreamModel
	}
	if owner.publicID != "" {
		publicModel = owner.publicID
	}
	if owner.upstreamModel != "" {
		upstreamModel = owner.upstreamModel
	}
	return publicModel, upstreamModel
}

func (h *ProxyHandler) routeChatCompletionsResponse(w http.ResponseWriter, resp *http.Response, mode chatCompletionsMode, handlers chatCompletionsResponseHandlers) error {
	if resp.StatusCode == http.StatusOK && mode.clientRequestedStream {
		if handlers.stream == nil {
			return fmt.Errorf("missing stream response handler")
		}
		handlers.stream(resp)
		return nil
	}

	if resp.StatusCode == http.StatusOK && mode.forceUpstreamStream {
		if handlers.aggregate == nil {
			return fmt.Errorf("missing aggregate response handler")
		}
		oaiResp, err := aggregateStreamToResponse(resp.Body)
		if err != nil {
			return err
		}
		handlers.aggregate(oaiResp)
		return nil
	}

	if handlers.passthrough != nil {
		return handlers.passthrough(resp)
	}

	writeUpstreamResponse(w, resp)
	return nil
}

// HandleAnthropicMessages handles POST /v1/messages by translating the Anthropic
// request to OpenAI format, forwarding to Copilot, and translating the response back.
func (h *ProxyHandler) HandleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		status := readBodyStatusCode(err)
		writeAnthropicError(w, status, mapAnthropicUpstreamStatus(status), err.Error())
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req models.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		message, _ := jsonDecodeErrorDetails(err, "invalid JSON in request body")
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", message)
		return
	}

	h.log.Debug("anthropic request",
		logger.F("model", req.Model),
		logger.F("stream", req.Stream),
		logger.F("messages", len(req.Messages)),
		logger.F("tools", len(req.Tools)),
	)

	directAnthropic := h.shouldForwardAnthropicMessagesDirect(req.Model)
	providerEndpoint := providerEndpointChatCompletions
	if directAnthropic {
		providerEndpoint = providerEndpointMessages
	}
	h.observeRequestSummary(r.Context(), "anthropic", req.Model, req.Stream, providerEndpoint)

	if directAnthropic {
		h.forwardAnthropicMessagesDirect(w, r, body, &req)
		return
	}

	scope := chatToolExecutionScopeFromHeaders(r.Header)
	oaiBody, mode, err := prepareAnthropicChatCompletionsRequest(&req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("translation error: %v", err))
		return
	}
	oaiBody = h.rewriteOpenAIChatRequestBodyWithToolOptimizers(r.Context(), oaiBody, h.toolContexts, scope)

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(mode.clientRequestedStream || mode.forceUpstreamStream)
	defer upstreamCancel()

	resp, err := h.postChatCompletions(upstreamCtx, oaiBody)
	if err != nil {
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		h.log.Error("upstream request failed", logger.F("endpoint", "anthropic"), logger.Err(err))
		if statusCode == http.StatusBadRequest {
			writeAnthropicError(w, statusCode, "invalid_request_error", err.Error())
			return
		}
		writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), formatUpstreamRequestFailure(err, "upstream request failed"))
		return
	}

	observeUpstreamHeaders(r.Context(), resp.Header)

	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		detail := formatUpstreamErrorMessage(resp.StatusCode, errBody)
		h.log.Error("upstream error",
			logger.F("endpoint", "anthropic"),
			logger.F("status", resp.StatusCode),
			logger.F("detail", detail),
			logger.F("request_bytes", len(oaiBody)),
		)
		h.log.Debug("upstream error body", logger.F("endpoint", "anthropic"), logger.F("status", resp.StatusCode), logger.F("body", string(errBody)))
		writeAnthropicError(w, resp.StatusCode, mapAnthropicUpstreamStatus(resp.StatusCode), detail)
		return
	}

	err = h.routeChatCompletionsResponse(w, resp, mode, chatCompletionsResponseHandlers{
		stream: func(resp *http.Response) {
			StreamOpenAIToAnthropicWithFinalResponse(
				w,
				resp.Body,
				req.Model,
				"msg_"+uuid.New().String(),
				h.openAIChatStreamFinalResponseCallback(r.Context(), h.toolContexts, scope),
			)
		},
		aggregate: func(oaiResp *models.OpenAIResponse) {
			observeOpenAIUsage(r.Context(), oaiResp.Usage)
			h.maybeRewriteOrCaptureOpenAIChatToolCommands(r.Context(), oaiResp, h.toolContexts, scope, false)
			anthropicResp := TranslateOpenAIToAnthropic(oaiResp, req.Model)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(anthropicResp)
		},
	})
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "failed to aggregate upstream response")
	}
}

// HandleAnthropicMessagesCountTokens handles POST /v1/messages/count_tokens.
// OpenAI-compatible upstreams do not expose a token-count endpoint, so this uses
// the same minimal chat-completions probe as the Gemini countTokens adapter and
// returns the upstream prompt token count in Anthropic's response shape.
func (h *ProxyHandler) HandleAnthropicMessagesCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		status := readBodyStatusCode(err)
		writeAnthropicError(w, status, mapAnthropicUpstreamStatus(status), err.Error())
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req models.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		message, _ := jsonDecodeErrorDetails(err, "invalid JSON in request body")
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", message)
		return
	}

	directAnthropic := h.shouldForwardAnthropicCountTokensDirect(req.Model)
	providerEndpoint := providerEndpointChatCompletions
	if directAnthropic {
		providerEndpoint = providerEndpointMessages
	}
	h.observeRequestSummary(r.Context(), "anthropic_count_tokens", req.Model, false, providerEndpoint)

	if directAnthropic {
		h.forwardAnthropicCountTokensDirect(w, r, body)
		return
	}

	oaiReq, err := prepareAnthropicCountTokensProbeRequest(&req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("translation error: %v", err))
		return
	}

	oaiResp, err := h.runAnthropicCountTokensProbe(oaiReq)
	if err != nil {
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		h.log.Error("upstream request failed", logger.F("endpoint", "anthropic_count_tokens"), logger.Err(err))
		if statusCode == http.StatusBadRequest {
			writeAnthropicError(w, statusCode, "invalid_request_error", err.Error())
			return
		}
		writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), formatUpstreamRequestFailure(err, "upstream request failed"))
		return
	}

	if oaiResp.Usage == nil {
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream response did not include usage")
		return
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(models.AnthropicCountTokensResponse{
		InputTokens: oaiResp.Usage.PromptTokens,
	})
}

func prepareAnthropicCountTokensProbeRequest(req *models.AnthropicRequest) (*models.OpenAIRequest, error) {
	oaiReq, err := TranslateAnthropicToOpenAI(req)
	if err != nil {
		return nil, err
	}

	stream := false
	temperature := 0.0
	one := 1

	oaiReq.Stream = &stream
	oaiReq.StreamOptions = nil
	oaiReq.Temperature = &temperature
	oaiReq.MaxCompletionTokens = &one
	oaiReq.MaxTokens = nil

	return oaiReq, nil
}

func (h *ProxyHandler) runAnthropicCountTokensProbe(probeReq *models.OpenAIRequest) (*models.OpenAIResponse, error) {
	oaiResp, fallback, err := h.executeAnthropicCountTokensProbe(probeReq)
	if fallback {
		one := 1
		probeReq.MaxCompletionTokens = nil
		probeReq.MaxTokens = &one
		return h.executeAnthropicCountTokensProbeFinal(probeReq)
	}
	return oaiResp, err
}

func (h *ProxyHandler) executeAnthropicCountTokensProbe(probeReq *models.OpenAIRequest) (*models.OpenAIResponse, bool, error) {
	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(false)
	defer upstreamCancel()

	body, err := json.Marshal(probeReq)
	if err != nil {
		return nil, false, fmt.Errorf("failed to marshal count_tokens probe request: %w", err)
	}

	resp, err := h.postChatCompletions(upstreamCtx, body)
	if err != nil {
		return nil, false, err
	}

	if resp.StatusCode == http.StatusBadRequest && probeReq.MaxCompletionTokens != nil {
		_ = resp.Body.Close()
		return nil, true, nil
	}

	oaiResp, err := h.decodeAnthropicCountTokensProbeResponse(resp)
	return oaiResp, false, err
}

func (h *ProxyHandler) executeAnthropicCountTokensProbeFinal(probeReq *models.OpenAIRequest) (*models.OpenAIResponse, error) {
	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(false)
	defer upstreamCancel()

	body, err := json.Marshal(probeReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal count_tokens probe request: %w", err)
	}

	resp, err := h.postChatCompletions(upstreamCtx, body)
	if err != nil {
		return nil, err
	}

	return h.decodeAnthropicCountTokensProbeResponse(resp)
}

func (h *ProxyHandler) decodeAnthropicCountTokensProbeResponse(resp *http.Response) (*models.OpenAIResponse, error) {
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		detail := formatUpstreamErrorMessage(resp.StatusCode, errBody)
		h.log.Error("upstream error", logger.F("endpoint", "anthropic_count_tokens"), logger.F("status", resp.StatusCode), logger.F("detail", detail))
		h.log.Debug("upstream error body", logger.F("endpoint", "anthropic_count_tokens"), logger.F("status", resp.StatusCode), logger.F("body", string(errBody)))
		return nil, &upstreamError{statusCode: resp.StatusCode, body: errBody}
	}

	var oaiResp models.OpenAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&oaiResp); err != nil {
		return nil, fmt.Errorf("failed to parse upstream count_tokens probe response: %w", err)
	}

	return &oaiResp, nil
}

// HandleOpenAIChatCompletions handles POST /v1/chat/completions by forwarding the
// request to Copilot with only auth headers injected (near zero-copy passthrough).
func (h *ProxyHandler) HandleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := readBody(r)
	if err != nil {
		status := readBodyStatusCode(err)
		writeOpenAIRequestBodyError(w, status, err)
		return
	}
	defer func() { _ = r.Body.Close() }()

	if message, param, ok := validateOpenAIChatRequest(bodyBytes); ok {
		writeOpenAIErrorWithDetails(w, http.StatusBadRequest, message, "invalid_request_error", param, "")
		return
	}

	requestedModel := extractRequestModel(bodyBytes)
	scope := chatToolExecutionScopeFromHeaders(r.Header)
	bodyBytes, mode := prepareOpenAIChatCompletionsRequest(bodyBytes)
	h.observeRequestSummary(r.Context(), "openai_chat", requestedModel, mode.clientRequestedStream, providerEndpointChatCompletions)
	bodyBytes = h.rewriteOpenAIChatRequestBodyWithToolOptimizers(r.Context(), bodyBytes, h.toolContexts, scope)

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(mode.clientRequestedStream || mode.forceUpstreamStream)
	defer upstreamCancel()

	resp, err := h.postChatCompletions(upstreamCtx, bodyBytes)
	if err != nil {
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		h.log.Error("upstream request failed", logger.F("endpoint", "openai"), logger.Err(err))
		if statusCode == http.StatusBadRequest {
			writeOpenAIError(w, statusCode, err.Error(), "invalid_request_error")
			return
		}
		writeOpenAIUpstreamRequestFailure(w, statusCode, err)
		return
	}

	observeUpstreamHeaders(r.Context(), resp.Header)

	err = h.routeChatCompletionsResponse(w, resp, mode, chatCompletionsResponseHandlers{
		stream: func(resp *http.Response) {
			copyPassthroughHeaders(w.Header(), resp.Header)
			StreamOpenAIPassthroughWithFinalResponse(w, resp.Body, h.openAIChatStreamFinalResponseCallback(r.Context(), h.toolContexts, scope), openAIChatStreamUsageCallback(r.Context()))
		},
		aggregate: func(oaiResp *models.OpenAIResponse) {
			observeOpenAIUsage(r.Context(), oaiResp.Usage)
			h.maybeRewriteOrCaptureOpenAIChatToolCommands(r.Context(), oaiResp, h.toolContexts, scope, false)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(oaiResp)
		},
		passthrough: func(resp *http.Response) error {
			return h.maybeWriteOptimizedOpenAIChatPassthrough(r.Context(), w, resp, h.toolContexts, scope)
		},
	})
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, "failed to aggregate upstream response", "server_error")
	}
}

func validateOpenAIChatRequest(body []byte) (string, string, bool) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return "", "", false
	}
	if rawMessages, ok := payload["messages"]; !ok || !rawJSONHasNonEmptyArray(rawMessages) {
		return "messages must be a non-empty array", "messages", true
	}
	if rawToolChoice, ok := payload["tool_choice"]; ok && openAIToolChoiceRequiresTools(rawToolChoice) && !hasNonEmptyTools(payload["tools"]) {
		return "tool_choice requires non-empty tools", "tool_choice", true
	}
	if rawResponseFormat, ok := payload["response_format"]; ok && responseFormatMissingJSONSchema(rawResponseFormat) {
		return "response_format json_schema requires json_schema.schema", "response_format.json_schema.schema", true
	}
	return "", "", false
}

func rawJSONHasNonEmptyArray(raw json.RawMessage) bool {
	var values []json.RawMessage
	return json.Unmarshal(raw, &values) == nil && len(values) > 0
}

func openAIToolChoiceRequiresTools(raw json.RawMessage) bool {
	var choice string
	if err := json.Unmarshal(raw, &choice); err == nil {
		choice = strings.TrimSpace(choice)
		return choice == "required"
	}
	var object struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &object); err != nil {
		return false
	}
	switch strings.TrimSpace(object.Type) {
	case "required", "function":
		return true
	default:
		return false
	}
}

func responseFormatMissingJSONSchema(raw json.RawMessage) bool {
	var format struct {
		Type       string          `json:"type"`
		JSONSchema json.RawMessage `json:"json_schema"`
	}
	if err := json.Unmarshal(raw, &format); err != nil || strings.TrimSpace(format.Type) != "json_schema" {
		return false
	}
	var schema struct {
		Schema json.RawMessage `json:"schema"`
	}
	if err := json.Unmarshal(format.JSONSchema, &schema); err != nil {
		return true
	}
	return len(bytes.TrimSpace(schema.Schema)) == 0 || string(bytes.TrimSpace(schema.Schema)) == "null"
}

func hasNonEmptyTools(raw json.RawMessage) bool {
	if len(bytes.TrimSpace(raw)) == 0 {
		return false
	}

	var tools []json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return false
	}

	return len(tools) > 0
}

// injectParallelToolCalls adds parallel_tool_calls: true to an OpenAI request
// body when tools are present but the flag is not already set.
func injectParallelToolCalls(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	tools, hasTools := m["tools"]
	if !hasTools || !hasNonEmptyTools(tools) {
		return body
	}
	if rawToolChoice, ok := m["tool_choice"]; ok && openAIToolChoiceIsNone(rawToolChoice) {
		return body
	}
	if _, hasPTC := m["parallel_tool_calls"]; hasPTC {
		return body
	}
	m["parallel_tool_calls"] = json.RawMessage("true")
	result, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return result
}

func openAIToolChoiceIsNone(raw json.RawMessage) bool {
	var choice string
	if err := json.Unmarshal(raw, &choice); err == nil {
		return strings.EqualFold(strings.TrimSpace(choice), "none")
	}
	var object struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(raw, &object); err == nil {
		return strings.EqualFold(strings.TrimSpace(object.Type), "none")
	}
	return false
}

// injectForceStream adds stream: true and stream_options to a request body
// for forced streaming to the upstream.
func injectForceStream(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	m["stream"] = json.RawMessage("true")
	m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	result, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return result
}

// ensureStreamUsage asks the upstream to emit a final usage chunk on a
// client-requested stream by setting stream_options.include_usage. Without it,
// many upstreams omit token usage from streamed responses and the proxy records
// zero tokens. It is merge-safe: if the client already supplied stream_options,
// their choice is left untouched (they may have deliberately set include_usage
// false or other options).
func ensureStreamUsage(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if _, ok := m["stream_options"]; ok {
		return body
	}
	m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	result, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return result
}
