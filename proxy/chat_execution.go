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

const responsesChatMinimumOutputTokens = 16

type chatExecutionError struct {
	StatusCode int
	Type       string
	Code       string
	Param      string
	Message    string
	Headers    http.Header
	Usage      *models.OpenAIUsage
	route      resolvedChatRoute
	// Upstream prose quotes the request back, so only vekil's own Message is loggable.
	upstreamAuthored bool
	// vekil authored this, but interpolated client text into it: an unknown field's NAME
	// becomes the param, an unsupported value is echoed into the message. Neither is ours.
	clientDerived bool
	// The parent path vekil built, without the client's field name appended.
	safeParam string
	// The Message is a compile-time constant with nothing interpolated, so it is vekil's own
	// words and safe to log. Default is CLOSED: every other message is dropped. Validators
	// interpolate client values ("Responses input item type %q is not supported") and
	// upstream prose quotes the request back, so opting in per constructor is what keeps a
	// newly added validator from silently putting request content in the logs.
	staticMessage bool
}

func (e *chatExecutionError) Error() string {
	if e == nil {
		return ""
	}
	return e.Message
}

type chatExecutionOptions struct {
	ResponsesMinimumOutputTokens int
	ResponsesDropSamplingParams  bool
	ResponsesUsageOnly           bool
	CarriedReasoning             map[string]carriedReplay
	CarrierInbound               carrierInbound
	// Set by the Anthropic surfaces only. See responsesChatRequestOptions.
	DegradeUnrestorableReplay bool
}

type chatExecutionResult struct {
	Response         *http.Response
	Completion       *models.OpenAIResponse
	CompletionBody   []byte
	CarriedReasoning carriedTurn
	Stream           *chatStreamEventStream
	Headers          http.Header
	Usage            *models.OpenAIUsage
	IncludeUsage     bool
	Backend          chatBackend
	route            resolvedChatRoute
	// Reduced from an upstream non-2xx body, which the surfaces see as a successful
	// result. Rides here because the boundary that read it had no request summary.
	upstreamError upstreamErrorClassifiers
}

// Non-streaming Anthropic is force-streamed, so its carrier lands on the stream.
func (r chatExecutionResult) carrier() carriedTurn {
	if r.CarriedReasoning.present() {
		return r.CarriedReasoning
	}
	if r.Stream != nil {
		return r.Stream.carriedReasoning
	}
	return carriedTurn{}
}

func (h *ProxyHandler) executeChatCompletions(ctx context.Context, chatBody []byte, options chatExecutionOptions) (chatExecutionResult, error) {
	model := extractRequestModel(chatBody)
	route, err := h.resolveChatRoute(ctx, model)
	if err != nil {
		return chatExecutionResult{}, err
	}
	if chatRequestContainsResponsesReplayID(chatBody) && route.backend != chatBackendResponses {
		if !chatRouteAllowsEndpoint(route.provider, route.owner, route.known, providerEndpointResponses) {
			replayErr := missingResponsesChatReplayError()
			attachChatExecutionErrorRoute(replayErr, route)
			return chatExecutionResult{}, replayErr
		}
		route = newResolvedChatRoute(route.provider, route.owner, route.known, model, providerEndpointResponses, chatBackendResponses)
	}
	var result chatExecutionResult
	if route.backend == chatBackendNativeChat {
		result, err = h.executeResolvedNativeChat(ctx, route, chatBody, options)
	} else {
		result, err = h.executeResolvedResponsesChat(ctx, route, chatBody, options)
	}
	if err != nil {
		attachChatExecutionErrorRoute(err, route)
	}
	return result, err
}

func rawJSONFieldsExactOrFold(object map[string]json.RawMessage, name string) []json.RawMessage {
	if raw, ok := object[name]; ok {
		return []json.RawMessage{raw}
	}
	var matches []json.RawMessage
	for candidate, raw := range object {
		if strings.EqualFold(candidate, name) {
			matches = append(matches, raw)
		}
	}
	return matches
}

func chatRequestContainsResponsesReplayID(body []byte) bool {
	var request map[string]json.RawMessage
	if json.Unmarshal(body, &request) != nil {
		return false
	}
	for _, rawMessages := range rawJSONFieldsExactOrFold(request, "messages") {
		var messages []json.RawMessage
		if json.Unmarshal(rawMessages, &messages) != nil {
			continue
		}
		for _, rawMessage := range messages {
			var message map[string]json.RawMessage
			if json.Unmarshal(rawMessage, &message) != nil {
				continue
			}
			for _, rawRole := range rawJSONFieldsExactOrFold(message, "role") {
				var role string
				if json.Unmarshal(rawRole, &role) != nil {
					continue
				}
				switch strings.TrimSpace(role) {
				case "tool":
					for _, rawCallID := range rawJSONFieldsExactOrFold(message, "tool_call_id") {
						var callID string
						if json.Unmarshal(rawCallID, &callID) == nil && isResponsesChatReplayCallID(callID) {
							return true
						}
					}
				case "assistant":
					for _, rawCalls := range rawJSONFieldsExactOrFold(message, "tool_calls") {
						var calls []json.RawMessage
						if json.Unmarshal(rawCalls, &calls) != nil {
							continue
						}
						for _, rawCall := range calls {
							var call map[string]json.RawMessage
							if json.Unmarshal(rawCall, &call) != nil {
								continue
							}
							for _, rawCallID := range rawJSONFieldsExactOrFold(call, "id") {
								var callID string
								if json.Unmarshal(rawCallID, &callID) == nil && isResponsesChatReplayCallID(callID) {
									return true
								}
							}
						}
					}
				}
			}
		}
	}
	return false
}

func (h *ProxyHandler) executeResolvedNativeChat(ctx context.Context, route resolvedChatRoute, chatBody []byte, options chatExecutionOptions) (chatExecutionResult, error) {
	resp, err := h.postResolvedProviderRequest(ctx, route.provider, route.owner, providerEndpointChatCompletions, chatBody, nil)
	if err != nil {
		return chatExecutionResult{}, err
	}
	return chatExecutionResult{
		Response: resp,
		Headers:  convertedChatSafeHeaders(resp.Header),
		Backend:  chatBackendNativeChat,
		route:    route,
	}, nil
}

func (h *ProxyHandler) retryResolvedNativeChat(ctx context.Context, prior chatExecutionResult, chatBody []byte) (chatExecutionResult, error) {
	if prior.Backend != chatBackendNativeChat || prior.route.backend != chatBackendNativeChat || prior.route.provider == nil {
		return chatExecutionResult{}, fmt.Errorf("native Chat retry requires a captured native Chat route")
	}
	return h.executeResolvedNativeChat(ctx, prior.route, chatBody, chatExecutionOptions{})
}

func (h *ProxyHandler) executeResolvedResponsesChat(ctx context.Context, route resolvedChatRoute, chatBody []byte, options chatExecutionOptions) (chatExecutionResult, error) {
	replayRoute := responsesChatReplayRoute{
		ProviderID:    route.provider.id,
		PublicModel:   route.publicModel,
		UpstreamModel: route.upstreamModel,
	}
	plan, err := translateChatRequestToResponses(chatBody, responsesChatRequestOptions{
		UpstreamModel:       route.upstreamModel,
		CarriedReasoning:    options.CarriedReasoning,
		ReplayStore:         h.responsesChatReplayStore(),
		ReplayRoute:         replayRoute,
		Log:                 h.log,
		MinimumOutputTokens: options.ResponsesMinimumOutputTokens,
		DropSamplingParams:  options.ResponsesDropSamplingParams,
		// No candidate loop here, so the terminal translate is also the only one.
		DegradeUnrestorableReplay: options.DegradeUnrestorableReplay,
	})
	if err != nil {
		return chatExecutionResult{}, err
	}
	requestBody, _ := stripUnsupportedResponsesRequestFields(plan.Body, route.provider)
	headers := http.Header(nil)
	if plan.Stream {
		headers = make(http.Header)
		headers.Set("Accept", "text/event-stream")
	}
	resp, err := h.postResolvedProviderRequest(ctx, route.provider, route.owner, route.nativeEndpoint, requestBody, headers)
	if err != nil {
		return chatExecutionResult{}, responsesChatExecutionErrorFromUpstream(err)
	}
	resp, err = h.maybeRetryResolvedResponsesWithoutUnverifiableEncryptedContent(
		ctx,
		route.provider,
		route.owner,
		route.nativeEndpoint,
		requestBody,
		headers,
		resp,
	)
	if err != nil {
		return chatExecutionResult{}, responsesChatExecutionErrorFromUpstream(err)
	}
	result := chatExecutionResult{
		Response:     resp,
		Headers:      convertedChatSafeHeaders(resp.Header),
		IncludeUsage: plan.IncludeUsage,
		Backend:      chatBackendResponses,
		route:        route,
	}
	if resp.StatusCode != http.StatusOK {
		classifiers, err := canonicalizeResponsesChatHTTPError(resp, result.Headers)
		if err != nil {
			return chatExecutionResult{}, err
		}
		result.upstreamError = classifiers
		return result, nil
	}
	if plan.Stream {
		stream, streamErr := translateResponsesSSEToChat(ctx, resp.Body, responsesChatResponseOptions{
			PublicModel:        route.publicModel,
			ReplayStore:        h.responsesChatReplayStore(),
			ReplayRoute:        replayRoute,
			ReplayToolDefaults: plan.ReplayToolDefaults,
			Carrier:            carrierEmit{Inbound: options.CarrierInbound, Log: h.log},
		})
		if streamErr != nil {
			attachChatExecutionErrorHeaders(streamErr, result.Headers)
			return chatExecutionResult{}, streamErr
		}
		result.Response = nil
		result.Stream = stream
		return result, nil
	}

	body := newLifecycleAwareReadCloser(resp.Body, ctx)
	defer func() { _ = body.Close() }()
	responseBody, readErr := io.ReadAll(io.LimitReader(body, responsesChatMaxJSONBodyBytes+1))
	if body.canceledAtFailure() {
		return chatExecutionResult{}, context.Canceled
	}
	if readErr != nil {
		return chatExecutionResult{}, fmt.Errorf("read Responses-backed Chat body: %w", readErr)
	}
	converted, err := translateResponsesJSONToChat(responseBody, responsesChatResponseOptions{
		PublicModel:        route.publicModel,
		ReplayStore:        h.responsesChatReplayStore(),
		ReplayRoute:        replayRoute,
		ReplayToolDefaults: plan.ReplayToolDefaults,
		UsageOnly:          options.ResponsesUsageOnly,
		Carrier:            carrierEmit{Inbound: options.CarrierInbound, Log: h.log},
	})
	if err != nil {
		attachChatExecutionErrorHeaders(err, result.Headers)
		return chatExecutionResult{}, err
	}
	result.Response = nil
	result.Completion = converted.Response
	result.CompletionBody = converted.Body
	result.CarriedReasoning = converted.CarriedReasoning
	result.Usage = converted.Usage
	return result, nil
}

const responsesChatMaxErrorBodyBytes = 64 << 10

type responsesChatErrorDetails struct {
	message   string
	errorType string
	code      any
	param     any
}

func parseResponsesChatErrorDetails(status int, body []byte) responsesChatErrorDetails {
	details := responsesChatErrorDetails{
		message:   formatUpstreamErrorMessage(status, body),
		errorType: openAIErrorTypeForHTTPStatus(status),
	}
	var upstream struct {
		Error *struct {
			Message string          `json:"message"`
			Type    string          `json:"type"`
			Code    string          `json:"code"`
			Param   json.RawMessage `json:"param"`
		} `json:"error"`
	}
	// Copilot's Responses endpoint answers with the classifiers at the TOP level --
	// {"code":"invalid_request_body","message":...} -- with no `error` wrapper. Probed
	// against the live API; parsing only the nested form lost `code` for the very
	// provider this path serves.
	var flat struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Code    string          `json:"code"`
		Param   json.RawMessage `json:"param"`
	}
	if json.Unmarshal(body, &flat) == nil {
		if trimmed := strings.TrimSpace(flat.Message); trimmed != "" {
			details.message = trimmed
		}
		if trimmed := strings.TrimSpace(flat.Type); trimmed != "" {
			details.errorType = trimmed
		}
		if trimmed := strings.TrimSpace(flat.Code); trimmed != "" {
			details.code = trimmed
		}
		if len(flat.Param) > 0 {
			_ = json.Unmarshal(flat.Param, &details.param)
		}
	}
	if json.Unmarshal(body, &upstream) == nil && upstream.Error != nil {
		if strings.TrimSpace(upstream.Error.Message) != "" {
			details.message = strings.TrimSpace(upstream.Error.Message)
		}
		if strings.TrimSpace(upstream.Error.Type) != "" {
			details.errorType = strings.TrimSpace(upstream.Error.Type)
		}
		if strings.TrimSpace(upstream.Error.Code) != "" {
			details.code = strings.TrimSpace(upstream.Error.Code)
		}
		if len(bytes.TrimSpace(upstream.Error.Param)) > 0 && !bytes.Equal(bytes.TrimSpace(upstream.Error.Param), []byte("null")) {
			_ = json.Unmarshal(upstream.Error.Param, &details.param)
		}
	}
	return details
}

func responsesChatExecutionErrorFromUpstream(err error) error {
	var upstreamErr *upstreamError
	if !errors.As(err, &upstreamErr) {
		return err
	}
	details := parseResponsesChatErrorDetails(upstreamErr.statusCode, upstreamErr.body)
	code, _ := details.code.(string)
	param, _ := details.param.(string)
	return &chatExecutionError{
		StatusCode: upstreamErr.statusCode,
		Type:       details.errorType,
		Code:       code,
		Param:      param,
		Message:    details.message,
		Headers:    convertedChatSafeHeaders(upstreamErr.headers),

		upstreamAuthored: true,
	}
}

// canonicalizeResponsesChatHTTPError rewrites an upstream non-2xx into vekil's canonical
// envelope and returns the classifiers reduced from the body it consumed.
//
// It returns them rather than recording them because this runs on the lifecycle-rooted
// upstream context, which deliberately carries no request summary (see
// newInferenceUpstreamContextFrom). A non-2xx reaches the Chat surfaces as a SUCCESSFUL
// chatExecutionResult, so no execution error is built and nothing else records WHY the turn
// failed -- and this is the only place that sees the body. The caller carries them to a
// surface holding the inbound context.
func canonicalizeResponsesChatHTTPError(resp *http.Response, safeHeaders http.Header) (upstreamErrorClassifiers, error) {
	if resp == nil || resp.Body == nil {
		return upstreamErrorClassifiers{}, fmt.Errorf("upstream Responses error body is unavailable")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, responsesChatMaxErrorBodyBytes+1))
	_ = resp.Body.Close()
	if err != nil {
		return upstreamErrorClassifiers{}, fmt.Errorf("read upstream Responses error body: %w", err)
	}
	if len(body) > responsesChatMaxErrorBodyBytes {
		body = body[:responsesChatMaxErrorBodyBytes]
	}
	classifiers := safeUpstreamErrorClassifiers(resp.StatusCode, body)
	details := parseResponsesChatErrorDetails(resp.StatusCode, body)
	envelope, err := json.Marshal(map[string]any{"error": map[string]any{
		"message": details.message,
		"type":    details.errorType,
		"param":   details.param,
		"code":    details.code,
	}})
	if err != nil {
		return upstreamErrorClassifiers{}, fmt.Errorf("marshal canonical upstream error: %w", err)
	}
	resp.Body = io.NopCloser(bytes.NewReader(envelope))
	resp.Header = safeHeaders.Clone()
	if resp.Header == nil {
		resp.Header = make(http.Header)
	}
	resp.Header.Set("Content-Type", "application/json")
	resp.Header.Set("Content-Length", fmt.Sprintf("%d", len(envelope)))
	resp.ContentLength = int64(len(envelope))
	return classifiers, nil
}

func openAIErrorTypeForHTTPStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusConflict:
		return "conflict_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	default:
		return "server_error"
	}
}

func convertedChatSafeHeaders(src http.Header) http.Header {
	if len(src) == 0 {
		return nil
	}
	dst := make(http.Header)
	for key, values := range src {
		lower := strings.ToLower(strings.TrimSpace(key))
		allowed := lower == "x-request-id" || lower == "request-id" || lower == "x-github-request-id" ||
			lower == "openai-processing-ms" || lower == "retry-after" || lower == "x-azure-request-id" ||
			lower == "openai-request-id" || strings.HasPrefix(lower, "x-ratelimit-") || strings.HasPrefix(lower, "ratelimit-")
		if !allowed {
			continue
		}
		for _, value := range values {
			dst.Add(key, value)
		}
	}
	if len(dst) == 0 {
		return nil
	}
	return dst
}

func (h *ProxyHandler) routeChatExecutionResult(
	w http.ResponseWriter,
	result chatExecutionResult,
	upstreamCtx context.Context,
	mode chatCompletionsMode,
	handlers chatCompletionsResponseHandlers,
) error {
	if result.Backend == chatBackendResponses && len(result.Headers) > 0 {
		mergeHeaderValues(w.Header(), result.Headers)
	}
	if result.Backend == chatBackendNativeChat || result.Response != nil {
		if result.Response == nil {
			return fmt.Errorf("chat execution returned no upstream response")
		}
		return h.routeChatCompletionsResponse(w, result.Response, upstreamCtx, mode, handlers)
	}
	if result.Completion != nil {
		if handlers.aggregate == nil {
			return fmt.Errorf("missing canonical Chat completion handler")
		}
		handlers.aggregate(result.Completion)
		return nil
	}
	if result.Stream != nil {
		if mode.clientRequestedStream {
			if handlers.streamEvents == nil {
				return fmt.Errorf("missing canonical Chat event handler")
			}
			handlers.streamEvents(result.Stream)
			return nil
		}
		if mode.forceUpstreamStream {
			if handlers.aggregate == nil {
				return fmt.Errorf("missing aggregate response handler")
			}
			response, err := aggregateChatStreamEvents(result.Stream)
			if err != nil {
				return err
			}
			handlers.aggregate(response)
			return nil
		}
	}
	return fmt.Errorf("chat execution returned no canonical result")
}

func (h *ProxyHandler) retryChatExecutionWithoutInjectedStreamOptions(
	ctx context.Context,
	result chatExecutionResult,
	body []byte,
	mode chatCompletionsMode,
) (chatExecutionResult, []byte, chatCompletionsMode) {
	if h == nil || result.Backend != chatBackendNativeChat || result.Response == nil ||
		result.Response.StatusCode != http.StatusBadRequest || !mode.injectedStreamUsage {
		return result, body, mode
	}
	fallbackBody, ok := stripStreamOptions(body)
	if !ok {
		return result, body, mode
	}
	retryResult, err := h.retryResolvedNativeChat(ctx, result, fallbackBody)
	if err != nil {
		if h.log != nil {
			h.log.Debug("retry without stream_options failed", logger.Err(err))
		}
		return result, body, mode
	}
	if result.Response.Body != nil {
		drainAndClose(result.Response.Body)
	}
	mode.injectedStreamUsage = false
	mode.injectedClientStreamUsage = false
	if h.log != nil {
		h.log.Debug("retried chat completions without injected stream_options", logger.F("status", retryResult.Response.StatusCode))
	}
	return retryResult, fallbackBody, mode
}

func writeOpenAIChatExecutionError(w http.ResponseWriter, err error) bool {
	var executionErr *chatExecutionError
	if !errors.As(err, &executionErr) {
		return false
	}
	if len(executionErr.Headers) > 0 {
		mergeHeaderValues(w.Header(), executionErr.Headers)
	}
	writeOpenAIErrorWithDetails(w, executionErr.StatusCode, executionErr.Message, executionErr.Type, executionErr.Param, executionErr.Code)
	return true
}

func attachChatExecutionErrorRoute(err error, route resolvedChatRoute) {
	var executionErr *chatExecutionError
	if errors.As(err, &executionErr) && executionErr.route.provider == nil {
		executionErr.route = route
	}
}

func attachChatExecutionErrorHeaders(err error, headers http.Header) {
	if len(headers) == 0 {
		return
	}
	var executionErr *chatExecutionError
	if errors.As(err, &executionErr) && len(executionErr.Headers) == 0 {
		executionErr.Headers = headers.Clone()
	}
}

func chatExecutionErrorFromStreamTermination(err error) *chatExecutionError {
	if err == nil || errors.Is(err, errChatStreamClientWriteFailed) || errors.Is(err, errChatStreamConsumerStopped) || errors.Is(err, context.Canceled) {
		return nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return &chatExecutionError{StatusCode: http.StatusGatewayTimeout, Type: "server_error", Code: "gateway_timeout", Message: "upstream Responses stream timed out"}
	}
	return &chatExecutionError{StatusCode: http.StatusBadGateway, Type: "server_error", Code: "responses_stream_failed", Message: err.Error(), upstreamAuthored: true}
}

// Same as newChatInvalidRequest for a field the CLIENT named. The client-facing Param keeps
// the full path -- they already know their own key -- but the summary records only
// parentParam, which vekil built. Splitting the joined path on its last dot at log time does
// NOT work: a JSON key may itself contain dots, so "metadata.customer_ssn.value" is
// indistinguishable from a genuine two-level path and half the key survives.
func newChatInvalidRequestClientField(parentParam, clientField, message string) *chatExecutionError {
	param := clientField
	if parentParam != "" {
		param = parentParam + "." + clientField
	}
	err := newChatInvalidRequest(param, message)
	err.clientDerived = true
	err.safeParam = parentParam
	return err
}

func attachChatExecutionErrorUsage(err error, usage *models.OpenAIUsage) {
	if usage == nil {
		return
	}
	var executionErr *chatExecutionError
	if errors.As(err, &executionErr) && executionErr.Usage == nil {
		executionErr.Usage = usage
	}
}
