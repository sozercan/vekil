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

	"github.com/google/uuid"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/models"
)

type chatCompletionsMode struct {
	clientRequestedStream      bool
	clientRequestedStreamUsage bool
	forceUpstreamStream        bool
	// injectedStreamUsage is true when the proxy added stream_options.include_usage
	// to a streamed upstream request. If a strict OpenAI-compatible provider rejects
	// that optional field with 400, the request can be retried once without it.
	injectedStreamUsage bool
	// injectedClientStreamUsage is true when the proxy added
	// stream_options.include_usage to a client-requested stream that did not opt
	// in. On the verbatim OpenAI passthrough the resulting upstream usage-only
	// chunk must be dropped from the client stream (it never requested it).
	injectedClientStreamUsage bool
}

type chatCompletionsResponseHandlers struct {
	stream       func(*http.Response)
	streamEvents func(*chatStreamEventStream)
	aggregate    func(*models.OpenAIResponse)
	passthrough  func(*http.Response) error
}

func parseOpenAIChatCompletionsMode(body []byte) chatCompletionsMode {
	var partial struct {
		Stream        *bool                 `json:"stream,omitempty"`
		StreamOptions *models.StreamOptions `json:"stream_options,omitempty"`
		Tools         json.RawMessage       `json:"tools,omitempty"`
	}
	// Best-effort mode detection only: malformed JSON should still fall through
	// to the real request validation path instead of making this helper another
	// source of hard failures.
	_ = json.Unmarshal(body, &partial)

	clientRequestedStream := partial.Stream != nil && *partial.Stream
	return chatCompletionsMode{
		clientRequestedStream:      clientRequestedStream,
		clientRequestedStreamUsage: clientRequestedStream && partial.StreamOptions != nil && partial.StreamOptions.IncludeUsage,
		forceUpstreamStream:        !clientRequestedStream && hasNonEmptyTools(partial.Tools),
	}
}

func prepareOpenAIChatCompletionsRequest(body []byte) ([]byte, chatCompletionsMode) {
	mode := parseOpenAIChatCompletionsMode(body)
	body = injectParallelToolCalls(body)
	if mode.forceUpstreamStream {
		body = injectForceStream(body)
		mode.injectedStreamUsage = true
	} else if mode.clientRequestedStream {
		// Ask upstream for a usage chunk so streamed traffic records tokens.
		// Record whether we actually injected it (the client supplied no
		// stream_options): if so, the resulting upstream usage-only chunk is
		// dropped from the verbatim passthrough so the client does not receive a
		// terminal chunk it never opted into.
		var injected bool
		body, injected = ensureStreamUsage(body)
		mode.injectedStreamUsage = injected
		mode.injectedClientStreamUsage = injected
	}
	return body, mode
}

func prepareAnthropicChatCompletionsRequest(req *models.AnthropicRequest) ([]byte, chatCompletionsMode, error) {
	return prepareAnthropicChatCompletionsRequestWithModelOverride(req, "")
}

func prepareAnthropicChatCompletionsRequestWithModelOverride(req *models.AnthropicRequest, modelOverride string) ([]byte, chatCompletionsMode, error) {
	oaiReq, err := TranslateAnthropicToOpenAI(req)
	if err != nil {
		return nil, chatCompletionsMode{}, err
	}
	if modelOverride = strings.TrimSpace(modelOverride); modelOverride != "" {
		oaiReq.Model = modelOverride
	}

	prewarm := !req.Stream &&
		((oaiReq.MaxTokens != nil && *oaiReq.MaxTokens == 0) ||
			(oaiReq.MaxCompletionTokens != nil && *oaiReq.MaxCompletionTokens == 0))
	mode := chatCompletionsMode{
		clientRequestedStream: req.Stream,
		forceUpstreamStream:   !req.Stream && !prewarm,
	}
	if mode.forceUpstreamStream || mode.clientRequestedStream {
		// Force-streamed or client-streamed: ask upstream for a usage chunk so
		// the request records tokens. For force-stream we also set stream:true;
		// for a client stream the request already streams, we only add the
		// usage option. A strict OpenAI-compatible provider may reject
		// stream_options; callers retry once without it when that happens.
		if mode.forceUpstreamStream {
			stream := true
			oaiReq.Stream = &stream
		}
		oaiReq.StreamOptions = &models.StreamOptions{IncludeUsage: true}
		mode.injectedStreamUsage = true
	}

	body, err := json.Marshal(oaiReq)
	if err != nil {
		return nil, chatCompletionsMode{}, err
	}
	return body, mode, nil
}

func (h *ProxyHandler) anthropicChatTranslationModel(model string) string {
	rawModel := strings.TrimSpace(model)
	if rawModel != "" {
		if setup := h.providerSetup(); setup != nil {
			if _, known := setup.lookupModel(rawModel); known {
				return rawModel
			}
		}
	}
	return NormalizeModelName(rawModel)
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
	provider, _, _ := h.resolveProviderModelForRequest(model, providerEndpointMessages)
	return provider != nil && provider.kind == providerTypeAnthropicCompatible
}

func (h *ProxyHandler) shouldForwardAnthropicCountTokensDirect(model string) bool {
	provider, _, _ := h.resolveProviderModelForRequest(model, providerEndpointMessages)
	return provider != nil && provider.kind == providerTypeAnthropicCompatible
}

func (h *ProxyHandler) forwardAnthropicMessagesDirect(w http.ResponseWriter, r *http.Request, body []byte, req *models.AnthropicRequest) {
	streaming := req != nil && req.Stream
	publicModel, upstreamModel := h.directAnthropicResponseModels(req)

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContextFrom(r.Context(), streaming)
	defer upstreamCancel()

	resp, err := h.postAnthropicMessages(upstreamCtx, body, anthropicExtraHeadersFromRequest(r))
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
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
		h.writeDirectAnthropicStreamResponse(r.Context(), upstreamCtx, w, resp, publicModel, upstreamModel)
		return
	}
	if resp.StatusCode == http.StatusOK {
		if err := writeDirectAnthropicJSONResponse(r.Context(), upstreamCtx, w, resp, publicModel, upstreamModel); err != nil {
			if h.handleResponseBodyWriteError(w, r, upstreamCtx, "anthropic", err) {
				return
			}
			if h.handleShutdownError(w, r, upstreamCtx, err) {
				return
			}
			h.log.Error("upstream response rewrite failed", logger.F("endpoint", "anthropic"), logger.Err(err))
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "failed to read upstream response")
		}
		return
	}

	_ = writeUpstreamResponse(w, resp)
}

func (h *ProxyHandler) forwardAnthropicCountTokensDirect(w http.ResponseWriter, r *http.Request, body []byte) {
	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(false)
	defer upstreamCancel()

	resp, err := h.postAnthropicMessagesCountTokens(upstreamCtx, body, anthropicExtraHeadersFromRequest(r))
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		h.log.Error("upstream request failed", logger.F("endpoint", "anthropic_count_tokens"), logger.Err(err))
		if statusCode == http.StatusBadRequest {
			writeAnthropicError(w, statusCode, "invalid_request_error", err.Error())
			return
		}
		writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), formatUpstreamRequestFailure(err, "upstream request failed"))
		return
	}

	var writeErr error
	if resp.StatusCode >= http.StatusOK && resp.StatusCode < http.StatusMultipleChoices {
		writeErr = writePassthroughSniffingUsage(w, resp, nil)
	} else {
		writeErr = writeUpstreamResponse(w, resp)
	}
	if writeErr != nil {
		if h.handleResponseBodyWriteError(w, r, upstreamCtx, "anthropic_count_tokens", writeErr) {
			return
		}
		writeAnthropicError(w, http.StatusBadGateway, "api_error", "failed to read upstream response")
	}
}

func (h *ProxyHandler) directAnthropicResponseModels(req *models.AnthropicRequest) (string, string) {
	if req == nil {
		return "", ""
	}
	publicModel := strings.TrimSpace(req.Model)
	upstreamModel := publicModel
	_, owner, known := h.resolveProviderModelForRequest(req.Model, providerEndpointMessages)
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

func (h *ProxyHandler) routeChatCompletionsResponse(w http.ResponseWriter, resp *http.Response, upstreamCtx context.Context, mode chatCompletionsMode, handlers chatCompletionsResponseHandlers) error {
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
		body := newLifecycleAwareReadCloser(resp.Body, upstreamCtx)
		oaiResp, err := aggregateStreamToResponse(body)
		if err != nil {
			if body.canceledAtFailure() {
				return context.Canceled
			}
			return err
		}
		handlers.aggregate(oaiResp)
		return nil
	}

	if handlers.passthrough != nil {
		return handlers.passthrough(resp)
	}

	return writeUpstreamResponse(w, resp)
}

func (h *ProxyHandler) handleCanonicalChatStreamLifecycleError(
	w http.ResponseWriter,
	r *http.Request,
	upstreamCtx context.Context,
	committed bool,
	err error,
	writeCommittedShutdown func(),
) bool {
	if err == nil {
		return false
	}
	observeCtx := context.Background()
	if r != nil {
		observeCtx = r.Context()
	}
	lifecycle := h.lifecycleStreamHooks(observeCtx, func() bool {
		return upstreamCtx != nil &&
			errors.Is(context.Cause(upstreamCtx), errProxyLifecycleShutdown) &&
			contextTerminationMatches(upstreamCtx, err)
	}, func() {
		h.WriteShutdownServiceUnavailable(w, r)
	})
	if !lifecycle.suppressTransportCancellation(committed) {
		return false
	}
	if committed && writeCommittedShutdown != nil {
		writeCommittedShutdown()
	}
	return true
}

const anthropicInterleavedThinkingBeta = "interleaved-thinking-2025-05-14"

func anthropicBetaEnabled(headers http.Header, feature string) bool {
	feature = strings.TrimSpace(feature)
	if feature == "" {
		return false
	}
	for _, value := range headers.Values("Anthropic-Beta") {
		for _, token := range strings.Split(value, ",") {
			if strings.TrimSpace(token) == feature {
				return true
			}
		}
	}
	return false
}

func validateAnthropicMessageTokenLimits(req *models.AnthropicRequest, headers http.Header) error {
	if req == nil || req.MaxTokens == nil {
		return fmt.Errorf("max_tokens is required")
	}
	if *req.MaxTokens < 0 {
		return fmt.Errorf("max_tokens must be greater than or equal to 0")
	}
	toolChoiceType := ""
	if req.ToolChoice != nil {
		toolChoiceType = strings.ToLower(strings.TrimSpace(req.ToolChoice.Type))
	}
	forcedToolChoice := toolChoiceType == "any" || toolChoiceType == "tool"
	if *req.MaxTokens == 0 {
		if req.Stream {
			return fmt.Errorf("max_tokens must be greater than 0 when stream is true")
		}
		if req.Thinking != nil && req.Thinking.Type == "enabled" {
			return fmt.Errorf("max_tokens must be greater than 0 when thinking is enabled")
		}
		if forcedToolChoice {
			return fmt.Errorf("max_tokens must be greater than 0 when tool_choice forces tool use")
		}
	}

	if req.Thinking != nil && req.Thinking.Type == "enabled" {
		if forcedToolChoice {
			return fmt.Errorf("thinking is not compatible with forced tool_choice")
		}
		if req.Thinking.BudgetTokens == nil {
			return fmt.Errorf("thinking.budget_tokens is required when thinking.type is enabled")
		}
		if *req.Thinking.BudgetTokens < 1024 {
			return fmt.Errorf("thinking.budget_tokens must be greater than or equal to 1024")
		}
		interleavedThinking := anthropicBetaEnabled(headers, anthropicInterleavedThinkingBeta) &&
			len(req.Tools) > 0 &&
			(req.ToolChoice == nil || toolChoiceType == "auto")
		if *req.Thinking.BudgetTokens >= *req.MaxTokens && !interleavedThinking {
			return fmt.Errorf("thinking.budget_tokens must be less than max_tokens unless interleaved thinking with tools is enabled")
		}
	}
	return nil
}

func translateOpenAIToAnthropicForRequest(resp *models.OpenAIResponse, req *models.AnthropicRequest) *models.AnthropicResponse {
	translated := TranslateOpenAIToAnthropic(resp, req.Model)
	if req.MaxTokens == nil || *req.MaxTokens != 0 {
		return translated
	}
	stopReason := "max_tokens"
	translated.Content = []models.ContentBlock{}
	translated.StopReason = &stopReason
	return translated
}

// HandleAnthropicMessages handles POST /v1/messages by translating the Anthropic
// request to OpenAI format, forwarding to Copilot, and translating the response back.
func (h *ProxyHandler) HandleAnthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		if h.handleShutdownError(w, r, nil, err) {
			return
		}
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
	if err := validateAnthropicMessageTokenLimits(&req, r.Header); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
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
	providerModel := req.Model
	if directAnthropic {
		providerEndpoint = providerEndpointMessages
	} else {
		providerModel = h.anthropicChatTranslationModel(req.Model)
	}
	h.observeRequestSummaryWithProviderModel(r.Context(), "anthropic", req.Model, providerModel, req.Stream, providerEndpoint)

	if directAnthropic {
		h.forwardAnthropicMessagesDirect(w, r, body, &req)
		return
	}

	scope := chatToolExecutionScopeFromHeaders(r.Header)
	oaiBody, mode, err := prepareAnthropicChatCompletionsRequestWithModelOverride(&req, providerModel)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("translation error: %v", err))
		return
	}
	oaiBody = h.rewriteOpenAIChatRequestBodyWithToolOptimizers(r.Context(), oaiBody, h.toolContexts, scope)

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContextFrom(r.Context(), mode.clientRequestedStream || mode.forceUpstreamStream)
	defer upstreamCancel()

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
			writeAnthropicError(w, executionErr.StatusCode, mapAnthropicUpstreamStatus(executionErr.StatusCode), executionErr.Message)
			return
		}
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		h.log.Error("upstream request failed", logger.F("endpoint", "anthropic"), logger.Err(err))
		if statusCode == http.StatusBadRequest {
			writeAnthropicError(w, statusCode, "invalid_request_error", err.Error())
			return
		}
		writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), formatUpstreamRequestFailure(err, "upstream request failed"))
		return
	}

	result, oaiBody, mode = h.retryChatExecutionWithoutInjectedStreamOptions(upstreamCtx, result, oaiBody, mode)
	observeChatExecutionRoute(r.Context(), result)
	observeUpstreamHeaders(r.Context(), result.Headers)
	if result.Backend == chatBackendResponses && len(result.Headers) > 0 {
		mergeHeaderValues(w.Header(), result.Headers)
	}

	if result.Response != nil && result.Response.StatusCode != http.StatusOK {
		resp := result.Response
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

	err = h.routeChatExecutionResult(w, result, upstreamCtx, mode, chatCompletionsResponseHandlers{
		stream: func(resp *http.Response) {
			body := newLifecycleAwareReadCloser(resp.Body, upstreamCtx)
			streamOpenAIToAnthropicWithLifecycle(
				w,
				body,
				req.Model,
				"msg_"+uuid.New().String(),
				func(status int) { observeResponseFailureStatus(r.Context(), status) },
				h.openAIChatStreamFinalResponseCallback(r.Context(), h.toolContexts, scope),
				h.lifecycleStreamHooks(r.Context(), body.canceledAtFailure, func() { h.WriteShutdownServiceUnavailable(w, r) }),
				openAIChatStreamUsageCallback(r.Context()),
			)
		},
		streamEvents: func(stream *chatStreamEventStream) {
			tracked := &commitTrackingResponseWriter{ResponseWriter: w}
			err := streamChatEventsToAnthropic(tracked, stream, req.Model, "msg_"+uuid.New().String(), chatStreamEventCallbacks{
				OnUsage: openAIChatStreamUsageCallback(r.Context()),
				OnFinal: h.openAIChatStreamFinalResponseCallback(r.Context(), h.toolContexts, scope),
			})
			if h.handleCanonicalChatStreamLifecycleError(w, r, upstreamCtx, tracked.committed, err, func() {
				writeAnthropicShutdownSSEEvent(tracked)
			}) {
				return
			}
			var streamErr *chatExecutionError
			if errors.As(err, &streamErr) {
				observeOpenAIUsage(r.Context(), streamErr.Usage)
				observeResponseFailureStatus(r.Context(), chatStreamErrorStatus(streamErr))
			} else if terminalErr := chatExecutionErrorFromStreamTermination(err); terminalErr != nil {
				observeResponseFailureStatus(r.Context(), terminalErr.StatusCode)
				_ = writeSSEEvent(tracked, "error", map[string]any{"type": "error", "error": map[string]any{"type": mapAnthropicUpstreamStatus(terminalErr.StatusCode), "message": terminalErr.Message}})
			}
		},
		aggregate: func(oaiResp *models.OpenAIResponse) {
			observeOpenAIUsage(r.Context(), oaiResp.Usage)
			h.maybeRewriteOrCaptureOpenAIChatToolCommands(r.Context(), oaiResp, h.toolContexts, scope, false)
			anthropicResp := translateOpenAIToAnthropicForRequest(oaiResp, &req)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(anthropicResp)
		},
		passthrough: func(resp *http.Response) error {
			body := newLifecycleAwareReadCloser(resp.Body, upstreamCtx)
			defer func() { _ = body.Close() }()
			var oaiResp models.OpenAIResponse
			if err := json.NewDecoder(body).Decode(&oaiResp); err != nil {
				if body.canceledAtFailure() {
					return context.Canceled
				}
				return err
			}
			observeOpenAIUsage(r.Context(), oaiResp.Usage)
			h.maybeRewriteOrCaptureOpenAIChatToolCommands(r.Context(), &oaiResp, h.toolContexts, scope, false)
			w.Header().Set("Content-Type", "application/json")
			return json.NewEncoder(w).Encode(translateOpenAIToAnthropicForRequest(&oaiResp, &req))
		},
	})
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		status := http.StatusBadGateway
		message := "failed to process upstream response"
		var executionErr *chatExecutionError
		if errors.As(err, &executionErr) {
			status = chatStreamErrorStatus(executionErr)
			message = chatStreamErrorMessage(executionErr)
			observeOpenAIUsage(r.Context(), executionErr.Usage)
		} else {
			var streamErr *openAIStreamError
			if errors.As(err, &streamErr) {
				status = streamErr.httpStatus()
				message = streamErr.Error()
			}
		}
		writeAnthropicError(w, status, mapAnthropicUpstreamStatus(status), message)
	}
}

// HandleAnthropicMessagesCountTokens handles POST /v1/messages/count_tokens.
// OpenAI-compatible upstreams do not expose a token-count endpoint, so this uses
// the same minimal chat-completions probe as the Gemini countTokens adapter and
// returns the upstream prompt token count in Anthropic's response shape.
func (h *ProxyHandler) HandleAnthropicMessagesCountTokens(w http.ResponseWriter, r *http.Request) {
	body, err := readBody(r)
	if err != nil {
		if h.handleShutdownError(w, r, nil, err) {
			return
		}
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
	providerModel := req.Model
	if directAnthropic {
		providerEndpoint = providerEndpointMessages
	} else {
		providerModel = h.anthropicChatTranslationModel(req.Model)
	}
	h.observeRequestSummaryWithProviderModel(r.Context(), "anthropic_count_tokens", req.Model, providerModel, false, providerEndpoint)

	if directAnthropic {
		h.forwardAnthropicCountTokensDirect(w, r, body)
		return
	}

	oaiReq, err := prepareAnthropicCountTokensProbeRequestWithModelOverride(&req, providerModel)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("translation error: %v", err))
		return
	}

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(false)
	defer upstreamCancel()

	oaiResp, err := h.runAnthropicCountTokensProbeWithContext(upstreamCtx, oaiReq)
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
			statusCode := chatStreamErrorStatus(executionErr)
			observeOpenAIUsage(r.Context(), executionErr.Usage)
			writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), chatStreamErrorMessage(executionErr))
			return
		}
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
	observeOpenAIUsage(r.Context(), oaiResp.Usage)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(models.AnthropicCountTokensResponse{
		InputTokens: oaiResp.Usage.PromptTokens,
	})
}

func prepareAnthropicCountTokensProbeRequestWithModelOverride(req *models.AnthropicRequest, modelOverride string) (*models.OpenAIRequest, error) {
	oaiReq, err := TranslateAnthropicToOpenAI(req)
	if err != nil {
		return nil, err
	}
	if modelOverride = strings.TrimSpace(modelOverride); modelOverride != "" {
		oaiReq.Model = modelOverride
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

func (h *ProxyHandler) runAnthropicCountTokensProbeWithContext(upstreamCtx context.Context, probeReq *models.OpenAIRequest) (*models.OpenAIResponse, error) {
	body, err := json.Marshal(probeReq)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal count_tokens probe request: %w", err)
	}
	result, err := h.executeChatCompletions(upstreamCtx, body, chatExecutionOptions{ResponsesMinimumOutputTokens: responsesChatMinimumOutputTokens, ResponsesDropSamplingParams: true, ResponsesUsageOnly: true})
	if err != nil {
		return nil, err
	}
	if result.Backend == chatBackendNativeChat && result.Response != nil && result.Response.StatusCode == http.StatusBadRequest && probeReq.MaxCompletionTokens != nil {
		_ = result.Response.Body.Close()
		one := 1
		probeReq.MaxCompletionTokens = nil
		probeReq.MaxTokens = &one
		fallbackBody, marshalErr := json.Marshal(probeReq)
		if marshalErr != nil {
			return nil, fmt.Errorf("failed to marshal count_tokens fallback request: %w", marshalErr)
		}
		result, err = h.retryResolvedNativeChat(upstreamCtx, result, fallbackBody)
		if err != nil {
			return nil, err
		}
	}
	return h.decodeAnthropicCountTokensExecution(result)
}

func (h *ProxyHandler) decodeAnthropicCountTokensExecution(result chatExecutionResult) (*models.OpenAIResponse, error) {
	if result.Completion != nil {
		result.Completion.Usage = result.Usage
		return result.Completion, nil
	}
	if result.Response != nil {
		return h.decodeAnthropicCountTokensProbeResponse(result.Response)
	}
	return nil, fmt.Errorf("count_tokens probe returned no Chat completion")
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
		if h.handleShutdownError(w, r, nil, err) {
			return
		}
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

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContextFrom(r.Context(), mode.clientRequestedStream || mode.forceUpstreamStream)
	defer upstreamCancel()

	result, err := h.executeChatCompletions(upstreamCtx, bodyBytes, chatExecutionOptions{})
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		var executionErr *chatExecutionError
		if errors.As(err, &executionErr) {
			observeChatExecutionError(r.Context(), executionErr)
			observeOpenAIUsage(r.Context(), executionErr.Usage)
			h.log.Error("chat execution failed", logger.F("endpoint", "openai"), logger.Err(err))
			writeOpenAIChatExecutionError(w, err)
			return
		}
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		h.log.Error("upstream request failed", logger.F("endpoint", "openai"), logger.Err(err))
		if statusCode == http.StatusBadRequest {
			writeOpenAIError(w, statusCode, err.Error(), "invalid_request_error")
			return
		}
		writeOpenAIUpstreamRequestFailure(w, statusCode, err)
		return
	}

	result, _, mode = h.retryChatExecutionWithoutInjectedStreamOptions(upstreamCtx, result, bodyBytes, mode)
	observeChatExecutionRoute(r.Context(), result)
	observeUpstreamHeaders(r.Context(), result.Headers)

	err = h.routeChatExecutionResult(w, result, upstreamCtx, mode, chatCompletionsResponseHandlers{
		stream: func(resp *http.Response) {
			copyPassthroughHeaders(w.Header(), resp.Header)
			body := newLifecycleAwareReadCloser(resp.Body, upstreamCtx)
			finalResponse := h.openAIChatStreamFinalResponseCallback(r.Context(), h.toolContexts, scope)
			usage := openAIChatStreamUsageCallback(r.Context())
			// A post-commit upstream stream error (the 200 header is already sent)
			// should be recorded as a failed request with its classified status
			// (e.g. 429 for a rate limit), not a 2xx success.
			onError := func(status int) { observeResponseFailureStatus(r.Context(), status) }
			// dropInjectedUsage drops the upstream usage-only chunk the proxy asked
			// for when the client did not opt into stream_options.include_usage.
			streamOpenAIChatPassthroughWithLifecycle(w, body, requestedModel, mode.injectedClientStreamUsage, onError, finalResponse, h.lifecycleStreamHooks(r.Context(), body.canceledAtFailure, func() { h.WriteShutdownServiceUnavailable(w, r) }), usage)
		},
		streamEvents: func(stream *chatStreamEventStream) {
			copyPassthroughHeaders(w.Header(), result.Headers)
			tracked := &commitTrackingResponseWriter{ResponseWriter: w}
			err := streamChatEventsToOpenAI(tracked, stream, chatStreamEventCallbacks{
				DropUsage: !mode.clientRequestedStreamUsage,
				OnUsage:   openAIChatStreamUsageCallback(r.Context()),
				OnFinal:   h.openAIChatStreamFinalResponseCallback(r.Context(), h.toolContexts, scope),
			})
			if h.handleCanonicalChatStreamLifecycleError(w, r, upstreamCtx, tracked.committed, err, func() {
				_ = writeOpenAIChatSSEError(tracked, &chatExecutionError{
					StatusCode: http.StatusServiceUnavailable,
					Type:       "service_unavailable",
					Message:    "server shutting down",
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
				if tracked.committed {
					_ = writeOpenAIChatSSEError(tracked, terminalErr)
				} else {
					writeOpenAIChatExecutionError(w, terminalErr)
				}
			}
		},
		aggregate: func(oaiResp *models.OpenAIResponse) {
			normalizeOpenAIChatCompletionStruct(oaiResp, requestedModel)
			observeOpenAIUsage(r.Context(), oaiResp.Usage)
			h.maybeRewriteOrCaptureOpenAIChatToolCommands(r.Context(), oaiResp, h.toolContexts, scope, false)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(oaiResp)
		},
		passthrough: func(resp *http.Response) error {
			return h.maybeWriteOptimizedOpenAIChatPassthrough(r.Context(), w, resp, requestedModel, h.toolContexts, scope)
		},
	})
	if err != nil {
		if h.handleResponseBodyWriteError(w, r, upstreamCtx, "openai", err) {
			return
		}
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		var executionErr *chatExecutionError
		if errors.As(err, &executionErr) {
			observeChatExecutionError(r.Context(), executionErr)
			observeOpenAIUsage(r.Context(), executionErr.Usage)
			writeOpenAIChatExecutionError(w, executionErr)
			return
		}
		status := http.StatusBadGateway
		message := "failed to aggregate upstream response"
		errType := "server_error"
		code := ""
		var streamErr *openAIStreamError
		if errors.As(err, &streamErr) {
			status = streamErr.httpStatus()
			message = streamErr.Error()
			if strings.TrimSpace(streamErr.Type) != "" {
				errType = streamErr.Type
			}
			code = streamErr.Code
		}
		writeOpenAIErrorWithDetails(w, status, message, errType, "", code)
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
// false or other options). The bool result reports whether include_usage was
// actually injected (true only when the client supplied no stream_options), so
// the caller can drop the resulting upstream usage-only chunk from a verbatim
// passthrough the client never opted into.
func ensureStreamUsage(body []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	if _, ok := m["stream_options"]; ok {
		return body, false
	}
	m["stream_options"] = json.RawMessage(`{"include_usage":true}`)
	result, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return result, true
}

func stripStreamOptions(body []byte) ([]byte, bool) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body, false
	}
	if _, ok := m["stream_options"]; !ok {
		return body, false
	}
	delete(m, "stream_options")
	result, err := json.Marshal(m)
	if err != nil {
		return body, false
	}
	return result, true
}
