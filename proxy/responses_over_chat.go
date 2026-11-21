package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/sozercan/vekil/logger"
)

// Responses-over-Chat serves a public /v1/responses request for a model whose
// provider exposes only native /chat/completions. It is the opposite direction
// of Chat-over-Responses (chat_over_responses_*.go / responses_chat_*.go) and
// shares only the bounded Responses-to-Chat translator with the policy ingress.
//
// The mode is opt-in per public model ID. Native endpoint metadata is never
// changed: a Chat-only model keeps supported_endpoints=["/chat/completions"]
// while Vekil advertises its own compatibility separately.

const (
	// responsesOverChatMaxRequestBytes matches the ordinary Chat request ceiling.
	// Full Codex tool catalogs plus long histories exceed the 1 MiB policy limit;
	// the translated Chat body may then grow by wrappers and tool aliases.
	responsesOverChatMaxRequestBytes    = maxRequestBodySize
	responsesOverChatMaxTranslatedBytes = maxRequestBodySize + (maxRequestBodySize / 2)
	responsesOverChatMaxInputItems      = 16384
	responsesOverChatMaxOutputItems     = responsesOverChatMaxInputItems
	// responsesOverChatMaxOutputBytes bounds text plus tool arguments accepted
	// from one upstream turn before the response is failed.
	responsesOverChatMaxOutputBytes = maxLargeRequestBodySize
)

var responsesOverChatTranslationContract = responsesChatTranslationContract{
	label:              "Responses-over-Chat",
	subject:            "Responses-over-Chat models",
	maxRequestBytes:    responsesOverChatMaxRequestBytes,
	maxTranslatedBytes: responsesOverChatMaxTranslatedBytes,
	maxInputItems:      responsesOverChatMaxInputItems,
	useLegacyMaxTokens: true,
	// The adapter streams text after an announced call as a later message
	// item, so its own completed output must replay as input.
	coalesceAssistantToolTurns: true,
	omitToolChoiceWithoutTools: true,
}

// WithResponsesOverChatModels opts the listed public model IDs into
// Responses-over-Chat compatibility. A listed model is served through its
// native Chat backend only when it has no native Responses endpoint; models
// with native Responses support keep the native path.
func WithResponsesOverChatModels(models ...string) Option {
	return func(h *ProxyHandler) {
		for _, model := range models {
			for _, part := range strings.Split(model, ",") {
				if part = strings.TrimSpace(part); part != "" {
					if h.responsesOverChatModels == nil {
						h.responsesOverChatModels = make(map[string]struct{})
					}
					h.responsesOverChatModels[part] = struct{}{}
				}
			}
		}
	}
}

// ResponsesOverChatModels returns the configured opt-in model IDs, sorted.
func (h *ProxyHandler) ResponsesOverChatModels() []string {
	if h == nil || len(h.responsesOverChatModels) == 0 {
		return nil
	}
	models := make([]string, 0, len(h.responsesOverChatModels))
	for model := range h.responsesOverChatModels {
		models = append(models, model)
	}
	sort.Strings(models)
	return models
}

// validateResponsesOverChatModels rejects contradictory configuration at
// startup. Policy public IDs already own a Responses compatibility adapter and
// must keep their classifier-backed semantics.
func (h *ProxyHandler) validateResponsesOverChatModels() error {
	for _, model := range h.ResponsesOverChatModels() {
		if _, policy := h.policyPublicModelID(model); policy {
			return fmt.Errorf("responses-over-chat model %q is a policy model; policy models already serve /v1/responses through their own adapter", model)
		}
		route, known := h.resolveModelRouteForRequest(model, providerEndpointChatCompletions)
		if !known {
			// Public resolution never returns internal routes, so check the
			// operational route IDs only after public IDs and aliases miss.
			if internal, ok := h.providerSetup().lookupTerminalRoute(model); ok && internal != nil && !internal.isPublic() {
				return fmt.Errorf("responses-over-chat model %q names an internal route", model)
			}
		}
		if known && route != nil && !route.legacy {
			if !route.isPublic() {
				return fmt.Errorf("responses-over-chat model %q names an internal route", model)
			}
			if !route.supportsEndpoint(providerEndpointChatCompletions) && !route.supportsEndpoint(providerEndpointResponses) {
				return fmt.Errorf("responses-over-chat model %q has no native %s endpoint", model, providerEndpointChatCompletions)
			}
		}
	}
	return nil
}

func (h *ProxyHandler) responsesOverChatOptedIn(model string) bool {
	if h == nil || len(h.responsesOverChatModels) == 0 {
		return false
	}
	_, ok := h.responsesOverChatModels[strings.TrimSpace(model)]
	return ok
}

// responsesExecutionMode is the shared, route-bound decision for how Vekil
// serves a public Responses request. Request admission, launchers and the
// Codex catalog projection must all derive from the same decision.
type responsesExecutionMode uint8

const (
	responsesExecutionNative responsesExecutionMode = iota
	responsesExecutionOverChat
	responsesExecutionUnsupported
)

func (m responsesExecutionMode) String() string {
	switch m {
	case responsesExecutionNative:
		return "native_responses"
	case responsesExecutionOverChat:
		return "responses_over_chat"
	default:
		return "unsupported"
	}
}

// responsesCompatibility describes the resolved execution mode for one public
// model plus the adapter-relevant capabilities read from verified provider
// metadata.
type responsesCompatibility struct {
	mode        responsesExecutionMode
	publicModel string
	nativeChat  *resolvedChatRoute
	// reason explains an unsupported decision without naming provider internals.
	reason string
	// reasoningEfforts are the provider-advertised Chat reasoning_effort values.
	reasoningEfforts []string
}

type responsesOverChatCapabilities struct {
	toolCalls        *bool
	reasoningEfforts []string
}

func responsesOverChatCapabilitiesFromRaw(raw json.RawMessage) responsesOverChatCapabilities {
	var metadata struct {
		Capabilities struct {
			Supports struct {
				ToolCalls       *bool    `json:"tool_calls"`
				ReasoningEffort []string `json:"reasoning_effort"`
			} `json:"supports"`
		} `json:"capabilities"`
	}
	if len(bytes.TrimSpace(raw)) == 0 || json.Unmarshal(raw, &metadata) != nil {
		return responsesOverChatCapabilities{}
	}
	efforts := make([]string, 0, len(metadata.Capabilities.Supports.ReasoningEffort))
	for _, effort := range metadata.Capabilities.Supports.ReasoningEffort {
		if effort = strings.TrimSpace(effort); effort != "" {
			efforts = append(efforts, effort)
		}
	}
	return responsesOverChatCapabilities{
		toolCalls:        metadata.Capabilities.Supports.ToolCalls,
		reasoningEfforts: efforts,
	}
}

// resolveResponsesCompatibility decides how a public Responses request for
// model is executed. Models that are not opted in always resolve native so the
// existing passthrough path, including its error behavior, is unchanged.
func (h *ProxyHandler) resolveResponsesCompatibility(ctx context.Context, model string) responsesCompatibility {
	model = strings.TrimSpace(model)
	compat := responsesCompatibility{mode: responsesExecutionNative, publicModel: model}
	if !h.responsesOverChatOptedIn(model) {
		return compat
	}
	if _, policy := h.policyPublicModelID(model); policy {
		return compat
	}

	if route, known := h.resolveModelRouteForRequest(model, providerEndpointChatCompletions); known && route != nil && !route.legacy {
		if !route.isPublic() {
			compat.mode, compat.reason = responsesExecutionUnsupported, "model is not a public route"
			return compat
		}
		compat.publicModel = route.public.id
		// Explicit route endpoints are the validated public contract shared by
		// every target, so one decision covers the whole failover chain.
		if route.supportsEndpoint(providerEndpointResponses) {
			return compat
		}
		if !route.supportsEndpoint(providerEndpointChatCompletions) {
			compat.mode, compat.reason = responsesExecutionUnsupported, "model has no native Chat Completions endpoint"
			return compat
		}
		caps := responsesOverChatCapabilitiesFromRaw(route.public.raw)
		if caps.toolCalls != nil && !*caps.toolCalls {
			compat.mode, compat.reason = responsesExecutionUnsupported, "model does not advertise function-tool support"
			return compat
		}
		compat.mode = responsesExecutionOverChat
		compat.reasoningEfforts = caps.reasoningEfforts
		return compat
	}

	route, err := h.resolveChatRoute(ctx, model)
	if err != nil {
		compat.mode, compat.reason = responsesExecutionUnsupported, "model has no native Chat Completions endpoint"
		return compat
	}
	if !route.known {
		compat.mode, compat.reason = responsesExecutionUnsupported, "model is not in the provider model catalog"
		return compat
	}
	if chatRouteAllowsEndpoint(route.provider, route.owner, route.known, providerEndpointResponses) {
		return compat
	}
	if route.backend != chatBackendNativeChat {
		compat.mode, compat.reason = responsesExecutionUnsupported, "model has no native Chat Completions endpoint"
		return compat
	}
	caps := responsesOverChatCapabilitiesFromRaw(route.owner.raw)
	if caps.toolCalls != nil && !*caps.toolCalls {
		compat.mode, compat.reason = responsesExecutionUnsupported, "model does not advertise function-tool support"
		return compat
	}
	compat.mode = responsesExecutionOverChat
	compat.reasoningEfforts = caps.reasoningEfforts
	compat.nativeChat = &route
	return compat
}

// applyResponsesOverChatReasoning validates the translated reasoning_effort
// against the provider-advertised values. Unsupported levels are rejected
// rather than mapped; an explicit "none" on a model without reasoning controls
// is the absence of an override and is removed.
func applyResponsesOverChatReasoning(body []byte, compat responsesCompatibility) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("decode Responses-over-Chat request: %w", err)
	}
	raw, ok := payload["reasoning_effort"]
	if !ok {
		return body, nil
	}
	var effort string
	if err := json.Unmarshal(raw, &effort); err != nil {
		return nil, newChatInvalidRequest("reasoning.effort", "reasoning.effort must be a string")
	}
	effort = strings.TrimSpace(effort)
	if slices.Contains(compat.reasoningEfforts, effort) {
		return body, nil
	}
	if len(compat.reasoningEfforts) == 0 {
		if effort == "none" {
			delete(payload, "reasoning_effort")
			return json.Marshal(payload)
		}
		return nil, newChatInvalidRequest("reasoning.effort", "reasoning.effort is not supported by this model")
	}
	err := newChatInvalidRequestClientField("reasoning", "effort",
		"reasoning.effort must be one of: "+strings.Join(compat.reasoningEfforts, ", "))
	err.Param = "reasoning.effort"
	return nil, err
}

func writeResponsesOverChatInvalidRequest(w http.ResponseWriter, err error) {
	var executionErr *chatExecutionError
	if errors.As(err, &executionErr) && executionErr != nil {
		code := executionErr.Code
		writeOpenAIErrorWithDetails(w, http.StatusBadRequest, executionErr.Message, "invalid_request_error", executionErr.Param, code)
		return
	}
	writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
}

// handleResponsesOverChat executes one opted-in Responses request through the
// canonical Chat execution seam pinned to a native Chat backend and translates
// the upstream Chat stream into Responses events incrementally.
func (h *ProxyHandler) handleResponsesOverChat(w http.ResponseWriter, r *http.Request, body []byte, compat responsesCompatibility) {
	requestedModel := extractResponsesRequestModel(body)
	translated, err := translateResponsesRequestToChat(body, responsesOverChatTranslationContract)
	if err != nil {
		writeResponsesOverChatInvalidRequest(w, err)
		return
	}
	h.observeRequestSummaryWithProviderModel(r.Context(), "responses", requestedModel, requestedModel, translated.Stream, providerEndpointChatCompletions)
	chatBody, err := applyResponsesOverChatReasoning(translated.Body, compat)
	if err != nil {
		writeResponsesOverChatInvalidRequest(w, err)
		return
	}
	if chatRequestContainsResponsesReplayID(chatBody) {
		// Those IDs can only be restored through a Responses backend. Sending them
		// to native Chat would silently lose hidden state, and switching backends
		// would create a Responses-to-Chat-to-Responses loop.
		writeOpenAIError(w, http.StatusBadRequest,
			"input contains tool calls minted by a Responses-backed Chat model; they cannot be replayed to a Responses-over-Chat model. Start a new conversation for this model.",
			"invalid_request_error")
		return
	}

	// Always stream upstream. Streaming clients receive incremental Responses
	// events; non-streaming clients receive the same state machine's final
	// object, which keeps validation identical and parallel tool calls reliable.
	chatBody = injectForceStream(chatBody)
	mode := parseOpenAIChatCompletionsMode(chatBody)
	chatBody, mode = prepareOpenAIChatCompletionsRequestWithMode(chatBody, chatParallelToolCallsDefault, mode)
	mode.injectedStreamUsage = true
	toolScope := chatToolExecutionScopeFromHeaders(r.Header)
	chatBody = h.rewriteOpenAIChatRequestBodyWithToolOptimizers(r.Context(), chatBody, h.toolContexts, toolScope)

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContextFrom(r.Context(), true)
	defer upstreamCancel()
	// The upstream context is lifecycle-rooted. Stop generation as soon as the
	// client leaves instead of waiting for the next write to fail.
	stopClientCancellation := context.AfterFunc(r.Context(), upstreamCancel)
	defer stopClientCancellation()
	upstreamCtx = withRouteOperation(upstreamCtx, routeOperationFromContext(r.Context()))
	upstreamCtx, _, route, err := h.withChatExecutionRoute(upstreamCtx, r.Context(), requestedModel, chatBody, false)
	if err != nil {
		h.writeResponsesOverChatExecutionError(w, r, upstreamCtx, err)
		return
	}
	publicModel := explicitRoutePublicModel(route, compat.publicModel)

	var result chatExecutionResult
	if compat.nativeChat != nil {
		result, err = h.executeResolvedNativeChat(upstreamCtx, *compat.nativeChat, chatBody)
		if err != nil {
			attachChatExecutionErrorRoute(err, *compat.nativeChat)
		}
	} else {
		result, err = h.executeRoutedChatCompletions(upstreamCtx, chatBody, mode, chatExecutionOptions{RequireNativeChat: true}, requestedModel)
	}
	if err != nil {
		h.writeResponsesOverChatExecutionError(w, r, upstreamCtx, err)
		return
	}
	result, _, _ = h.retryRoutedChatExecutionWithoutInjectedStreamOptions(upstreamCtx, result, chatBody, mode, requestedModel)
	observeChatExecutionRoute(r.Context(), result)
	result.observeUpstreamError(r.Context())
	observeUpstreamHeaders(r.Context(), chatExecutionUpstreamHeaders(result))

	if result.Backend != chatBackendNativeChat || result.Response == nil {
		// The resolver only admits native Chat backends. Anything else would mean a
		// Responses-to-Chat-to-Responses cycle; fail closed.
		closeChatExecutionResult(result)
		writeOpenAIError(w, http.StatusBadGateway, "Responses-over-Chat requires a native Chat Completions backend", "server_error")
		return
	}
	resp := result.Response
	if len(result.Headers) > 0 {
		mergeHeaderValues(w.Header(), result.Headers)
	}
	if resp.StatusCode != http.StatusOK {
		h.writeResponsesOverChatUpstreamStatus(w, r, resp)
		return
	}

	h.streamResponsesOverChat(w, r, upstreamCtx, resp, translated, publicModel, toolScope)
}

func closeChatExecutionResult(result chatExecutionResult) {
	if result.Response != nil && result.Response.Body != nil {
		drainAndClose(result.Response.Body)
	}
	if result.Stream != nil {
		result.Stream.stop(errChatStreamConsumerStopped)
	}
}

func (h *ProxyHandler) writeResponsesOverChatUpstreamStatus(w http.ResponseWriter, r *http.Request, resp *http.Response) {
	defer func() { _ = resp.Body.Close() }()
	errBody, _ := io.ReadAll(io.LimitReader(resp.Body, upstreamErrorDetailMaxBodyBytes))
	observeUpstreamErrorDetail(r.Context(), resp.StatusCode, errBody)
	if h.log != nil {
		h.log.Warn("upstream error",
			logger.F("endpoint", "responses_over_chat"),
			logger.F("status", resp.StatusCode),
			logger.F("response_bytes", len(errBody)),
		)
	}
	writeOpenAIError(w, resp.StatusCode, formatUpstreamErrorMessage(resp.StatusCode, errBody), openAIErrorTypeForHTTPStatus(resp.StatusCode))
}

func (h *ProxyHandler) writeResponsesOverChatExecutionError(w http.ResponseWriter, r *http.Request, upstreamCtx context.Context, err error) {
	if h.handleShutdownError(w, r, upstreamCtx, err) {
		return
	}
	var executionErr *chatExecutionError
	if errors.As(err, &executionErr) && executionErr != nil {
		observeChatExecutionError(r.Context(), executionErr)
		observeOpenAIUsage(r.Context(), executionErr.Usage)
		writeOpenAIChatExecutionError(w, err)
		return
	}
	h.writeResponsesUpstreamRequestFailure(w, r, upstreamCtx, "responses_over_chat", err)
}
