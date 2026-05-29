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
	provider, _, _ := h.resolveProviderModel(model, providerEndpointMessages)
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
		if statusCode == http.StatusInternalServerError {
			writeAnthropicError(w, statusCode, "api_error", "authentication failed")
			return
		}
		writeAnthropicError(w, statusCode, "api_error", "upstream request failed")
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

func (h *ProxyHandler) directAnthropicResponseModels(req *models.AnthropicRequest) (string, string) {
	if req == nil {
		return "", ""
	}
	publicModel := strings.TrimSpace(req.Model)
	upstreamModel := publicModel
	_, owner, known := h.resolveProviderModel(req.Model, providerEndpointMessages)
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
		writeAnthropicError(w, status, "invalid_request_error", err.Error())
		return
	}
	defer func() { _ = r.Body.Close() }()

	var req models.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "invalid JSON in request body")
		return
	}

	h.log.Debug("anthropic request",
		logger.F("model", req.Model),
		logger.F("stream", req.Stream),
		logger.F("messages", len(req.Messages)),
		logger.F("tools", len(req.Tools)),
	)

	if h.shouldForwardAnthropicMessagesDirect(req.Model) {
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
		if statusCode == http.StatusInternalServerError {
			writeAnthropicError(w, statusCode, "api_error", "authentication failed")
			return
		}
		writeAnthropicError(w, statusCode, "api_error", "upstream request failed")
		return
	}

	if resp.StatusCode != http.StatusOK {
		defer func() { _ = resp.Body.Close() }()
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		h.log.Error("upstream error",
			logger.F("endpoint", "anthropic"),
			logger.F("status", resp.StatusCode),
			logger.F("body", string(errBody)),
			logger.F("request_bytes", len(oaiBody)),
		)
		writeAnthropicError(w, resp.StatusCode, "api_error", fmt.Sprintf("upstream error (%d)", resp.StatusCode))
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

// HandleOpenAIChatCompletions handles POST /v1/chat/completions by forwarding the
// request to Copilot with only auth headers injected (near zero-copy passthrough).
func (h *ProxyHandler) HandleOpenAIChatCompletions(w http.ResponseWriter, r *http.Request) {
	bodyBytes, err := readBody(r)
	if err != nil {
		status := readBodyStatusCode(err)
		writeOpenAIError(w, status, err.Error(), "invalid_request_error")
		return
	}
	defer func() { _ = r.Body.Close() }()

	scope := chatToolExecutionScopeFromHeaders(r.Header)
	bodyBytes, mode := prepareOpenAIChatCompletionsRequest(bodyBytes)
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
		if statusCode == http.StatusInternalServerError {
			writeOpenAIError(w, statusCode, "authentication failed", "server_error")
			return
		}
		writeOpenAIError(w, statusCode, "upstream request failed", "server_error")
		return
	}

	err = h.routeChatCompletionsResponse(w, resp, mode, chatCompletionsResponseHandlers{
		stream: func(resp *http.Response) {
			copyPassthroughHeaders(w.Header(), resp.Header)
			StreamOpenAIPassthroughWithFinalResponse(w, resp.Body, h.openAIChatStreamFinalResponseCallback(r.Context(), h.toolContexts, scope))
		},
		aggregate: func(oaiResp *models.OpenAIResponse) {
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
