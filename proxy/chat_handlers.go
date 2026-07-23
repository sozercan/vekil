package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

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

type policyOpenAIStreamChoiceState struct {
	finished      bool
	outputBearing bool
}

type policySanitizedOpenAIStream struct {
	body           io.ReadCloser
	reader         *bufio.Reader
	maxEventBytes  int
	pending        []byte
	pendingErr     error
	sourceEOF      bool
	terminal       bool
	sawChoice      bool
	choiceFinished bool
}

func newPolicySanitizedOpenAIStream(body io.ReadCloser) io.ReadCloser {
	if body == nil {
		return nil
	}
	return &policySanitizedOpenAIStream{
		body:          body,
		reader:        bufio.NewReaderSize(body, openAIStreamScannerInitialBuffer),
		maxEventBytes: openAIStreamScannerMaxBuffer,
	}
}

func (s *policySanitizedOpenAIStream) Read(p []byte) (int, error) {
	for len(s.pending) == 0 {
		if s.pendingErr != nil {
			err := s.pendingErr
			s.pendingErr = nil
			return 0, err
		}
		if s.terminal {
			return 0, io.EOF
		}
		event, err := s.readEvent()
		s.pending = event
		s.pendingErr = err
	}
	n := copy(p, s.pending)
	s.pending = s.pending[n:]
	return n, nil
}

func (s *policySanitizedOpenAIStream) Close() error {
	if s == nil || s.body == nil {
		return nil
	}
	return s.body.Close()
}

func (s *policySanitizedOpenAIStream) readEvent() ([]byte, error) {
	if s.sourceEOF {
		return s.failEvent(http.StatusBadGateway), nil
	}
	var raw bytes.Buffer
	eventType := ""
	dataLines := make([]string, 0, 1)
	for {
		line, err := readOpenAISSELine(s.reader)
		if err != nil && !errors.Is(err, io.EOF) {
			return s.failEvent(http.StatusBadGateway), nil
		}
		if line != "" {
			limit := s.maxEventBytes
			if limit <= 0 {
				limit = openAIStreamScannerMaxBuffer
			}
			if len(line) > limit-raw.Len() {
				return s.failEvent(http.StatusBadGateway), nil
			}
			raw.WriteString(line)
			content, _ := splitSSELineEnding(line)
			if parsed, ok := parseSSEEventLine(content); ok {
				eventType = parsed
			}
			if data, ok := parseSSELine(content); ok {
				dataLines = append(dataLines, data)
			}
			if strings.TrimSpace(content) == "" {
				if errors.Is(err, io.EOF) {
					s.sourceEOF = true
				}
				return s.sanitizeEvent(raw.Bytes(), eventType, dataLines), nil
			}
		}
		if err == nil {
			continue
		}
		if errors.Is(err, io.EOF) {
			s.sourceEOF = true
			if raw.Len() > 0 {
				event := s.sanitizeEvent(raw.Bytes(), eventType, dataLines)
				if s.terminal {
					return terminatePolicySSEEvent(event), nil
				}
				return s.failEvent(http.StatusBadGateway), nil
			}
			return s.failEvent(http.StatusBadGateway), nil
		}
		return s.failEvent(http.StatusBadGateway), nil
	}
}

func (s *policySanitizedOpenAIStream) sanitizeEvent(raw []byte, eventType string, dataLines []string) []byte {
	if len(dataLines) == 0 {
		return append([]byte(nil), raw...)
	}
	data := strings.Join(dataLines, "\n")
	if strings.TrimSpace(data) == "[DONE]" {
		if !s.allChoicesFinished() {
			return s.failEvent(http.StatusBadGateway)
		}
		s.terminal = true
		_ = s.body.Close()
		return append([]byte(nil), raw...)
	}
	if !json.Valid([]byte(data)) {
		return s.failEvent(http.StatusBadGateway)
	}
	if streamErr, ok := parseOpenAIStreamError(eventType, data); ok {
		return s.failEvent(streamErr.httpStatus())
	}
	choice, recognized := inspectPolicyOpenAIStreamChunk(eventType, data)
	if !recognized {
		return s.failEvent(http.StatusBadGateway)
	}
	if !s.observeChunkChoice(choice) {
		return s.failEvent(http.StatusBadGateway)
	}
	return append([]byte(nil), raw...)
}

func (s *policySanitizedOpenAIStream) failEvent(status int) []byte {
	s.terminal = true
	if s.body != nil {
		_ = s.body.Close()
	}
	return policySanitizedOpenAIStreamErrorEvent(status)
}

func (s *policySanitizedOpenAIStream) observeChunkChoice(choice *policyOpenAIStreamChoiceState) bool {
	if choice == nil {
		return true
	}
	if s.choiceFinished && choice.outputBearing {
		return false
	}
	s.sawChoice = true
	if choice.finished {
		s.choiceFinished = true
	}
	return true
}

func (s *policySanitizedOpenAIStream) allChoicesFinished() bool {
	return s != nil && s.sawChoice && s.choiceFinished
}

func recognizedPolicyOpenAIStreamChunk(eventType, data string) bool {
	_, ok := inspectPolicyOpenAIStreamChunk(eventType, data)
	return ok
}

func inspectPolicyOpenAIStreamChunk(eventType, data string) (*policyOpenAIStreamChoiceState, bool) {
	switch strings.ToLower(strings.TrimSpace(eventType)) {
	case "", "message", "completion", "chat.completion.chunk":
	default:
		return nil, false
	}

	raw, err := decodeChatJSONObject([]byte(data), "")
	if err != nil || raw == nil {
		return nil, false
	}
	if hasCaseFoldedJSONFieldAlias(raw,
		"id", "object", "created", "model", "choices", "usage", "moderation",
		"system_fingerprint", "service_tier", "prompt_filter_results", "prompt_annotations",
	) {
		return nil, false
	}
	var chunk models.OpenAIStreamChunk
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return nil, false
	}
	if object := strings.TrimSpace(chunk.Object); object != "" && object != "chat.completion.chunk" {
		return nil, false
	}
	if recognizedFoundryPromptFilterAnnotation(raw) {
		return nil, true
	}
	if recognizedOpenAIModerationChunk(raw) {
		return nil, true
	}
	inspection := inspectOpenAIChatStreamEvent(eventType, data)
	if inspection.chunk == nil || inspection.progress == upstreamProgressUnknown {
		return nil, false
	}
	choicesRaw, hasChoices := raw["choices"]
	if hasChoices {
		if rawJSONIsNullOrEmpty(choicesRaw) {
			return nil, false
		}
		var choices []json.RawMessage
		if json.Unmarshal(choicesRaw, &choices) != nil {
			return nil, false
		}
		if len(choices) > 0 {
			// Anthropic ingress represents one assistant message and never requests
			// multiple Chat choices, so only a single choice at index zero is safe.
			if len(choices) != 1 {
				return nil, false
			}
			choice, err := decodeChatJSONObject(choices[0], "")
			if err != nil || choice == nil {
				return nil, false
			}
			if hasCaseFoldedJSONFieldAlias(choice, "index", "delta", "finish_reason") {
				return nil, false
			}
			var index int
			indexRaw, ok := choice["index"]
			if !ok || rawJSONIsNullOrEmpty(indexRaw) || json.Unmarshal(indexRaw, &index) != nil || index != 0 {
				return nil, false
			}
			recognized := false
			finished := false
			outputBearing := false
			if deltaRaw, ok := choice["delta"]; ok && !rawJSONIsNullOrEmpty(deltaRaw) {
				delta, err := decodeChatJSONObject(deltaRaw, "")
				if err != nil || delta == nil {
					return nil, false
				}
				if hasCaseFoldedJSONFieldAlias(delta,
					"role", "content", "refusal", "name", "tool_calls", "tool_call_id",
					"function_call", "reasoning", "reasoning_content", "reasoning_text", "audio",
				) {
					return nil, false
				}
				deltaProgress := classifyOpenAIChatDeltaProgress(delta)
				outputBearing = deltaProgress == upstreamProgressSemanticOutput || deltaProgress == upstreamProgressToolActivity
				recognized = true
			}
			if finishRaw, ok := choice["finish_reason"]; ok {
				if !rawJSONIsNullOrEmpty(finishRaw) {
					var finish string
					if json.Unmarshal(finishRaw, &finish) != nil || strings.TrimSpace(finish) == "" {
						return nil, false
					}
					finished = true
				}
				recognized = true
			}
			if !recognized {
				return nil, false
			}
			return &policyOpenAIStreamChoiceState{finished: finished, outputBearing: outputBearing}, true
		}
	}

	usageRaw, hasUsage := raw["usage"]
	if !hasUsage || rawJSONIsNullOrEmpty(usageRaw) {
		return nil, false
	}
	var usage models.OpenAIUsage
	if json.Unmarshal(usageRaw, &usage) != nil {
		return nil, false
	}
	return nil, true
}

func terminatePolicySSEEvent(event []byte) []byte {
	trimmed := bytes.TrimRight(event, "\r\n")
	terminated := bytes.Clone(trimmed)
	terminated = append(terminated, '\n')
	return append(terminated, '\n')
}

func hasCaseFoldedJSONFieldAlias(object map[string]json.RawMessage, canonical ...string) bool {
	for key := range object {
		for _, name := range canonical {
			if key != name && strings.EqualFold(key, name) {
				return true
			}
		}
	}
	return false
}

func recognizedFoundryPromptFilterAnnotation(raw map[string]json.RawMessage) bool {
	choicesRaw, ok := raw["choices"]
	if !ok || rawJSONIsNullOrEmpty(choicesRaw) {
		return false
	}
	var choices []json.RawMessage
	if json.Unmarshal(choicesRaw, &choices) != nil || len(choices) != 0 {
		return false
	}
	if usageRaw, ok := raw["usage"]; ok && !rawJSONIsNullOrEmpty(usageRaw) {
		return false
	}

	recognized := false
	for _, name := range []string{"prompt_filter_results", "prompt_annotations"} {
		annotationsRaw, ok := raw[name]
		if !ok || rawJSONIsNullOrEmpty(annotationsRaw) {
			continue
		}
		var annotations []json.RawMessage
		if json.Unmarshal(annotationsRaw, &annotations) != nil || len(annotations) == 0 {
			return false
		}
		for _, annotationRaw := range annotations {
			var annotation map[string]json.RawMessage
			if json.Unmarshal(annotationRaw, &annotation) != nil || annotation == nil {
				return false
			}
		}
		recognized = true
	}
	return recognized
}

func recognizedOpenAIModerationChunk(raw map[string]json.RawMessage) bool {
	choicesRaw, ok := raw["choices"]
	if !ok || rawJSONIsNullOrEmpty(choicesRaw) {
		return false
	}
	var choices []json.RawMessage
	if json.Unmarshal(choicesRaw, &choices) != nil || len(choices) != 0 {
		return false
	}
	if usageRaw, ok := raw["usage"]; ok && !rawJSONIsNullOrEmpty(usageRaw) {
		return false
	}
	moderationRaw, ok := raw["moderation"]
	if !ok || rawJSONIsNullOrEmpty(moderationRaw) {
		return false
	}
	var moderation map[string]json.RawMessage
	if json.Unmarshal(moderationRaw, &moderation) != nil || moderation == nil {
		return false
	}
	for _, name := range []string{"input", "output"} {
		partRaw, ok := moderation[name]
		if !ok || rawJSONIsNullOrEmpty(partRaw) {
			return false
		}
		var part struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(partRaw, &part) != nil || (part.Type != "moderation_results" && part.Type != "error") {
			return false
		}
	}
	return true
}

func policySanitizedOpenAIStreamErrorEvent(status int) []byte {
	message, errType, code := policyChatUpstreamErrorDetails(status)
	payload, _ := json.Marshal(openAIChatStreamErrorEnvelope{Error: openAIChatStreamErrorBody{
		Type: errType, Code: code, Message: message,
	}})
	return []byte("event: error\ndata: " + string(payload) + "\n\n")
}

type explicitRouteSurfaceSend func(context.Context) (*http.Response, error)

type explicitRouteStreamAggregator func(io.ReadCloser) (*models.OpenAIResponse, upstreamSemanticProgress, error)

type explicitRouteCanonicalFailure struct {
	response    *capturedRouteResponse
	stream      *explicitRouteStreamFailure
	err         error
	headers     http.Header
	attribution routeResultAttribution
	upstreamID  string
}

func (f *explicitRouteCanonicalFailure) result(ctx context.Context) (*http.Response, error) {
	if f == nil {
		return nil, nil
	}
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		summary.setFinalRouteResult(f.attribution.targetID, f.attribution.providerID, f.attribution.providerKind, f.upstreamID)
	}
	if f.response != nil {
		return f.response.response(), nil
	}
	if f.err != nil {
		return nil, f.err
	}
	if f.stream != nil {
		return nil, f.stream.asUpstreamError(f.headers)
	}
	return nil, nil
}

func explicitRouteCanonicalOrError(ctx context.Context, canonical *explicitRouteCanonicalFailure, err error) (*http.Response, error) {
	if canonical == nil || err == nil {
		return nil, err
	}
	var routeErr *routeExecutionFailureError
	if errors.As(err, &routeErr) && routeErr.failure.precedence() < 2 {
		return canonical.result(ctx)
	}
	return nil, err
}

func (h *ProxyHandler) executeChatCompletionsRouteRequest(ctx context.Context, body []byte, mode chatCompletionsMode) (*http.Response, error) {
	return h.executeChatCompletionsRouteRequestForModel(ctx, body, mode, extractRequestModel(body))
}

func (h *ProxyHandler) executeChatCompletionsRouteRequestForModel(ctx context.Context, body []byte, mode chatCompletionsMode, model string) (*http.Response, error) {
	send := func(attemptCtx context.Context) (*http.Response, error) {
		return h.postChatCompletionsForModel(attemptCtx, body, model)
	}
	return h.executeExplicitRouteSurfaceRequest(ctx, providerEndpointChatCompletions, mode.clientRequestedStream, explicitRouteStreamOpenAIChat, send)
}

func (h *ProxyHandler) executeAnthropicMessagesRouteRequest(ctx context.Context, body []byte, headers http.Header, streaming bool, model string) (*http.Response, error) {
	send := func(attemptCtx context.Context) (*http.Response, error) {
		return h.postJSONEndpointWithHeadersForModel(attemptCtx, providerEndpointMessages, body, headers, model)
	}
	return h.executeExplicitRouteSurfaceRequest(ctx, providerEndpointMessages, streaming, explicitRouteStreamAnthropic, send)
}

func (h *ProxyHandler) executeExplicitRouteSurfaceRequest(ctx context.Context, endpoint string, clientStream bool, protocol explicitRouteStreamProtocol, send explicitRouteSurfaceSend) (*http.Response, error) {
	resp, err := send(ctx)
	if err != nil {
		return nil, err
	}
	operation := routeOperationFromContext(ctx)
	if operation == nil || operation.route == nil || operation.route.legacy {
		return resp, nil
	}

	var canonical *explicitRouteCanonicalFailure
	for {
		if resp == nil {
			return nil, fmt.Errorf("upstream response is unavailable")
		}
		// The proxy-owned operation ID is authoritative. Never let a same-named
		// upstream header overwrite the value installed on the client response.
		sanitizeExplicitRouteResponseHeaders(resp.Header)

		info, target, ok := explicitRouteTargetForResponse(operation, resp)
		if !ok {
			return resp, nil
		}
		if explicitRouteSurfaceMayExplicitlyReject(target, endpoint, resp.StatusCode) {
			accepted := operation.hasAcceptedRouteAttempt(info.targetID)
			statusCode := resp.StatusCode
			upstreamID := responsesUpstreamRequestID(resp.Header)
			captured, cleanupDone := captureRouteResponse(resp)
			resp = captured.response()
			if !cleanupDone {
				if accepted {
					operation.reclassifyAcceptedRouteAttempt(info.targetID, statusCode, requestDeliveredOrAmbiguous, upstreamProgressUnknown, downstreamCommitmentNone, routeRetrySuppressedLifecycle, upstreamID, false, false)
				}
				return resp, nil
			}
			if !explicitRouteSurfaceCertifiesHTTPRejection(target, endpoint, captured) {
				// A bare/gateway 503 remains ambiguous. Return the bounded captured
				// response and never release the target for another generation.
				if accepted {
					operation.reclassifyAcceptedRouteAttempt(info.targetID, statusCode, requestDeliveredOrAmbiguous, upstreamProgressUnknown, downstreamCommitmentNone, routeRetrySuppressedDelivery, upstreamID, false, true)
				}
				return resp, nil
			}

			decision, retry := h.explicitRouteRetryDecision(ctx, operation, endpoint)
			if accepted && retry {
				if canonical == nil {
					canonical = &explicitRouteCanonicalFailure{
						response:    captured,
						attribution: routeResultAttributionForTarget(target),
						upstreamID:  upstreamID,
					}
				}
				operation.reclassifyAcceptedRouteAttempt(info.targetID, statusCode, requestExplicitlyRejected, upstreamProgressNone, downstreamCommitmentNone, decision, upstreamID, true, true)
				resp, err = send(ctx)
				if err != nil {
					return explicitRouteCanonicalOrError(operation.inbound, canonical, err)
				}
				continue
			}
			if accepted {
				operation.reclassifyAcceptedRouteAttempt(info.targetID, statusCode, requestExplicitlyRejected, upstreamProgressNone, downstreamCommitmentNone, decision, upstreamID, false, true)
				h.recordManualRouteExhaustion(operation, decision)
			}
			if canonical != nil {
				return canonical.result(operation.inbound)
			}
			return resp, nil
		}

		if resp.StatusCode != http.StatusOK || !clientStream || protocol == explicitRouteStreamNone {
			return resp, nil
		}
		if isExplicitRoutePreparedChatResponse(resp) {
			return resp, nil
		}

		prepared := newExplicitRoutePreparedStream(resp, protocol, responsesPrecommitMaxPeekBytes)
		result, hasResult, awaitErr := prepared.await(operation.inbound, ctx, responsesPrecommitPeekTimeout)
		if awaitErr != nil {
			prepared.abort()
			return nil, awaitErr
		}
		if hasResult && result.failure != nil {
			status, certified := explicitRouteTargetCertifiesStreamFailure(target, result.failure)
			if upstreamProgressAllowsTargetSwitch(result.progress) && certified {
				result.failure.statusCode = status
				decision, retry := h.explicitRouteRetryDecision(ctx, operation, endpoint)
				if retry {
					if !prepared.abortAndWait(upstreamErrorDetailDrainTimeout) {
						operation.reclassifyAcceptedRouteAttempt(info.targetID, result.failure.statusCode, requestDeliveredOrAmbiguous, upstreamProgressUnknown, downstreamCommitmentNone, routeRetrySuppressedLifecycle, responsesUpstreamRequestID(resp.Header), false, false)
						return nil, fmt.Errorf("failed to clean up rejected stream attempt before failover")
					}
					if canonical == nil {
						canonical = &explicitRouteCanonicalFailure{
							stream:      result.failure,
							headers:     resp.Header.Clone(),
							attribution: routeResultAttributionForTarget(target),
							upstreamID:  responsesUpstreamRequestID(resp.Header),
						}
					}
					operation.reclassifyAcceptedRouteAttempt(info.targetID, result.failure.statusCode, requestExplicitlyRejected, result.progress, downstreamCommitmentNone, decision, responsesUpstreamRequestID(resp.Header), true, true)
					resp, err = send(ctx)
					if err != nil {
						return explicitRouteCanonicalOrError(operation.inbound, canonical, err)
					}
					continue
				}
				prepared.abort()
				operation.reclassifyAcceptedRouteAttempt(info.targetID, result.failure.statusCode, requestExplicitlyRejected, result.progress, downstreamCommitmentNone, decision, responsesUpstreamRequestID(resp.Header), false, true)
				h.recordManualRouteExhaustion(operation, decision)
				if canonical != nil {
					return canonical.result(operation.inbound)
				}
				return nil, result.failure.asUpstreamError(resp.Header)
			}
		}

		if hasResult {
			operation.updateAcceptedRouteAttempt(info.targetID, result.progress, downstreamCommitmentNone)
		}
		return prepared.commitResponse(), nil
	}
}

func (h *ProxyHandler) aggregateExplicitChatCompletionsResponse(ctx context.Context, initialResp *http.Response, body []byte, mode chatCompletionsMode, aggregate explicitRouteStreamAggregator) (*models.OpenAIResponse, *http.Response, error) {
	resp := initialResp
	operation := routeOperationFromContext(ctx)
	var canonical *explicitRouteCanonicalFailure

	for {
		if resp == nil {
			return nil, nil, fmt.Errorf("upstream response is unavailable")
		}
		if resp.StatusCode != http.StatusOK {
			if canonical != nil {
				if _, target, ok := explicitRouteTargetForResponse(operation, resp); ok && explicitRouteSurfaceMayExplicitlyReject(target, providerEndpointChatCompletions, resp.StatusCode) {
					captured, cleanupDone := captureRouteResponse(resp)
					resp = captured.response()
					if cleanupDone && explicitRouteSurfaceCertifiesHTTPRejection(target, providerEndpointChatCompletions, captured) {
						_, canonicalErr := canonical.result(operation.inbound)
						return nil, nil, canonicalErr
					}
				}
			}
			return nil, resp, nil
		}

		lifecycleBody := newLifecycleAwareReadCloser(resp.Body, ctx)
		response, progress, aggregateErr := aggregate(lifecycleBody)
		if aggregateErr != nil && lifecycleBody.canceledAtFailure() {
			aggregateErr = context.Canceled
		}
		if operation == nil || operation.route == nil || operation.route.legacy {
			return response, nil, aggregateErr
		}

		info, target, ok := explicitRouteTargetForResponse(operation, resp)
		if !ok {
			return response, nil, aggregateErr
		}
		if aggregateErr == nil {
			progress = mergeUpstreamSemanticProgress(progress, upstreamProgressTerminalSuccess)
			operation.updateAcceptedRouteAttempt(info.targetID, progress, downstreamCommitmentNone)
			return response, nil, nil
		}

		failure := explicitRouteStreamFailureFromError(aggregateErr)
		certifiedStatus, certified := explicitRouteTargetCertifiesStreamFailure(target, failure)
		if failure == nil || !upstreamProgressAllowsTargetSwitch(progress) || !certified {
			decision := routeRetrySuppressedDelivery
			delivery := requestDeliveredOrAmbiguous
			if !upstreamProgressAllowsTargetSwitch(progress) {
				decision = routeRetrySuppressedProgress
			}
			if progress == upstreamProgressNone {
				progress = upstreamProgressUnknown
			}
			operation.reclassifyAcceptedRouteAttempt(info.targetID, routeErrorStatus(aggregateErr), delivery, progress, downstreamCommitmentNone, decision, responsesUpstreamRequestID(resp.Header), false, true)
			return nil, nil, aggregateErr
		}

		failure.statusCode = certifiedStatus
		decision, retry := h.explicitRouteRetryDecision(ctx, operation, providerEndpointChatCompletions)
		operation.reclassifyAcceptedRouteAttempt(info.targetID, failure.statusCode, requestExplicitlyRejected, progress, downstreamCommitmentNone, decision, responsesUpstreamRequestID(resp.Header), retry, true)
		if canonical == nil {
			canonical = &explicitRouteCanonicalFailure{
				err:         aggregateErr,
				attribution: routeResultAttributionForTarget(target),
				upstreamID:  responsesUpstreamRequestID(resp.Header),
			}
		}
		if !retry {
			h.recordManualRouteExhaustion(operation, decision)
			_, canonicalErr := canonical.result(operation.inbound)
			return nil, nil, canonicalErr
		}

		nextMode := mode
		nextMode.clientRequestedStream = false
		resp, aggregateErr = h.executeChatCompletionsRouteRequest(ctx, body, nextMode)
		if aggregateErr != nil {
			_, selectedErr := explicitRouteCanonicalOrError(operation.inbound, canonical, aggregateErr)
			return nil, nil, selectedErr
		}

		var recoveryErr error
		resp, body, mode, recoveryErr = h.retryChatCompletionsWithoutInjectedStreamOptionsForModelResult(ctx, resp, body, nextMode, extractRequestModel(body))
		if recoveryErr != nil {
			_, selectedErr := explicitRouteCanonicalOrError(operation.inbound, canonical, recoveryErr)
			return nil, nil, selectedErr
		}
		observeUpstreamHeaders(operation.inbound, resp.Header)
	}
}

func explicitRouteStreamFailureFromError(err error) *explicitRouteStreamFailure {
	var streamErr *openAIStreamError
	if !errors.As(err, &streamErr) {
		return nil
	}
	return &explicitRouteStreamFailure{
		statusCode: streamErr.httpStatus(),
		errType:    strings.TrimSpace(streamErr.Type),
		code:       strings.TrimSpace(streamErr.Code),
		message:    streamErr.Error(),
	}
}

func explicitRouteTargetForResponse(operation *routeOperation, resp *http.Response) (explicitRouteResponseInfo, targetBinding, bool) {
	if operation == nil || operation.route == nil {
		return explicitRouteResponseInfo{}, targetBinding{}, false
	}
	info, ok := explicitRouteResponseInfoFromResponse(resp)
	if !ok || info.routeID != operation.route.public.routeID {
		return explicitRouteResponseInfo{}, targetBinding{}, false
	}
	target, ok := operation.route.targetByID(info.targetID)
	return info, target, ok && target.provider != nil
}

func explicitRouteSurfaceMayExplicitlyReject(target targetBinding, endpoint string, statusCode int) bool {
	if routeAdapterMayExplicitlyReject(target, endpoint, statusCode) {
		return true
	}
	if endpoint != providerEndpointChatCompletions && endpoint != providerEndpointMessages {
		return false
	}
	if target.provider == nil || (statusCode != http.StatusServiceUnavailable && statusCode != 529) {
		return false
	}
	// Phase 6 reuses the same provider overload envelope classifier as Responses,
	// but still requires bounded body capture and code/type certification below.
	if routeAdapterMayExplicitlyReject(target, providerEndpointResponses, statusCode) {
		return true
	}
	return target.provider.kind == providerTypeAnthropicCompatible && statusCode == 529
}

func explicitRouteSurfaceCertifiesHTTPRejection(target targetBinding, endpoint string, response *capturedRouteResponse) bool {
	if routeAdapterCertifiesHTTPRejection(target, endpoint, response) {
		return true
	}
	if endpoint != providerEndpointChatCompletions && endpoint != providerEndpointMessages {
		return false
	}
	return routeAdapterCertifiesHTTPRejection(target, providerEndpointResponses, response)
}

func explicitRouteTargetCertifiesStreamFailure(target targetBinding, failure *explicitRouteStreamFailure) (int, bool) {
	if failure == nil || target.provider == nil {
		return 0, false
	}
	event := responsesWebSocketStreamEvent{}
	event.Response.Error.Code = strings.TrimSpace(failure.code)
	event.Response.Error.Type = strings.TrimSpace(failure.errType)
	event.Response.Error.Message = strings.TrimSpace(failure.message)
	return routeAdapterCertifiesStreamFailure(target, event)
}

func (h *ProxyHandler) explicitRouteRetryDecision(ctx context.Context, operation *routeOperation, endpoint string) (routeRetryDecision, bool) {
	if operation == nil || operation.route == nil {
		return routeRetrySuppressedNoTarget, false
	}
	if operation.route.policy.mode != routeModePriorityFailover {
		return routeRetrySuppressedMode, false
	}
	if !operation.allowsAutomaticTargetSwitch(routeAttemptKindFromContext(ctx)) {
		return routeRetrySuppressedState, false
	}
	if h.ShuttingDown() || (ctx != nil && ctx.Err() != nil) || (operation.inbound != nil && operation.inbound.Err() != nil) {
		return routeRetrySuppressedLifecycle, false
	}

	operation.mu.Lock()
	defer operation.mu.Unlock()
	if operation.commitment != downstreamCommitmentNone {
		return routeRetrySuppressedCommitment, false
	}
	if operation.remainingTargetAttempts <= 0 || operation.remainingUpstreamSends <= 0 {
		return routeRetrySuppressedBudget, false
	}
	for _, target := range operation.route.targets {
		if target.provider == nil || !target.provider.supportsEndpoint(endpoint) {
			continue
		}
		if _, attempted := operation.attemptedTargets[target.id]; !attempted {
			return routeRetrySwitchTarget, true
		}
	}
	return routeRetrySuppressedNoTarget, false
}

func (h *ProxyHandler) recordManualRouteExhaustion(operation *routeOperation, decision routeRetryDecision) {
	if operation == nil {
		return
	}
	if (decision == routeRetrySuppressedBudget || decision == routeRetrySuppressedNoTarget) && operation.markExhausted() {
		h.RecordRouteExhaustion(operation.inbound)
	}
}

func (o *routeOperation) hasAcceptedRouteAttempt(targetID string) bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := len(o.trace) - 1; i >= 0; i-- {
		if o.trace[i].TargetID == targetID {
			return o.trace[i].Decision == routeRetryAccepted
		}
	}
	return false
}

func (o *routeOperation) reclassifyAcceptedRouteAttempt(targetID string, statusCode int, delivery requestDelivery, progress upstreamSemanticProgress, commitment downstreamCommitment, decision routeRetryDecision, upstreamID string, releaseTarget, cleanupDone bool) bool {
	if o == nil {
		return false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := len(o.trace) - 1; i >= 0; i-- {
		trace := &o.trace[i]
		if trace.TargetID != targetID || trace.Decision != routeRetryAccepted {
			continue
		}
		trace.StatusCode = statusCode
		trace.Delivery = delivery
		trace.Progress = progress
		trace.Commitment = commitment
		trace.Decision = decision
		trace.CleanupDone = cleanupDone
		if strings.TrimSpace(upstreamID) != "" {
			trace.UpstreamID = upstreamID
		}
		if releaseTarget && !o.hardPinned && o.pinnedTargetID == targetID {
			o.pinnedTargetID = ""
		}
		return true
	}
	return false
}

func (o *routeOperation) updateAcceptedRouteAttempt(targetID string, progress upstreamSemanticProgress, commitment downstreamCommitment) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	for i := len(o.trace) - 1; i >= 0; i-- {
		trace := &o.trace[i]
		if trace.TargetID != targetID || trace.Decision != routeRetryAccepted {
			continue
		}
		trace.Progress = mergeUpstreamSemanticProgress(trace.Progress, progress)
		if routeCommitmentRank(commitment) > routeCommitmentRank(trace.Commitment) {
			trace.Commitment = commitment
		}
		return
	}
}

func markExplicitRouteDownstreamCommitment(ctx context.Context, commitment downstreamCommitment) {
	operation := routeOperationFromContext(ctx)
	if operation == nil {
		return
	}
	operation.setCommitment(commitment)
	if targetID := operation.pinnedTarget(); targetID != "" {
		operation.updateAcceptedRouteAttempt(targetID, upstreamProgressNone, commitment)
	}
}

func explicitRoutePublicModel(route *modelRoute, fallback string) string {
	if route != nil && !route.legacy && strings.TrimSpace(route.public.id) != "" {
		return route.public.id
	}
	return fallback
}

func normalizeExplicitOpenAIChatResponseModel(resp *http.Response, publicModel string) error {
	if resp == nil || resp.Body == nil || resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
		return nil
	}
	if _, ok := explicitRouteResponseInfoFromResponse(resp); !ok {
		return nil
	}
	publicModel = strings.TrimSpace(publicModel)
	if publicModel == "" {
		return nil
	}

	bodyReader := newLifecycleAwareReadCloser(resp.Body, responseRequestContext(resp))
	defer func() { _ = bodyReader.Close() }()
	prefix, err := io.ReadAll(io.LimitReader(bodyReader, maxLargeRequestBodySize+1))
	if bodyReader.canceledAtFailure() {
		return newResponseBodyWriteError(resp, context.Canceled, false, true, true)
	}
	if err != nil {
		return newResponseBodyWriteError(resp, err, false, true, false)
	}
	normalizeExplicitModelHeaders(resp.Header, publicModel)
	if len(prefix) > maxLargeRequestBodySize {
		body := []byte(`{"error":{"message":"explicit route chat response exceeds model-normalization limit","type":"server_error"}}`)
		resp.StatusCode = http.StatusBadGateway
		resp.Status = "502 Bad Gateway"
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		resp.Header.Set("Content-Type", "application/json")
		resp.Header.Set("Content-Length", strconv.Itoa(len(body)))
		return nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(prefix, &payload) == nil && payload != nil && !hasNonNullJSONField(payload, "error") {
		payload["model"] = mustMarshalRaw(publicModel)
		if rewritten, marshalErr := json.Marshal(payload); marshalErr == nil {
			prefix = rewritten
		}
	}
	resp.Body = io.NopCloser(bytes.NewReader(prefix))
	resp.ContentLength = int64(len(prefix))
	resp.Header.Del("Content-Length")
	return nil
}

func explicitAnthropicResponseModels(operation *routeOperation, resp *http.Response, fallbackPublic, fallbackUpstream string) (string, string) {
	if operation == nil || operation.route == nil {
		return fallbackPublic, fallbackUpstream
	}
	info, target, ok := explicitRouteTargetForResponse(operation, resp)
	if !ok {
		return fallbackPublic, fallbackUpstream
	}
	publicModel := strings.TrimSpace(info.publicID)
	if publicModel == "" {
		publicModel = fallbackPublic
	}
	upstreamModel := strings.TrimSpace(target.upstreamModel)
	if upstreamModel == "" {
		upstreamModel = fallbackUpstream
	}
	return publicModel, upstreamModel
}

func explicitRouteHasChatBackend(route *modelRoute, endpoint string) bool {
	if route == nil || route.legacy || !route.supportsEndpoint(endpoint) {
		return false
	}
	for _, target := range route.targets {
		if target.provider != nil && target.provider.supportsEndpoint(endpoint) {
			return true
		}
	}
	return false
}

func explicitChatExecutionEndpoint(route *modelRoute, body []byte) (string, error) {
	if route == nil || route.legacy {
		return "", nil
	}
	if chatRequestContainsResponsesReplayID(body) {
		if explicitRouteHasChatBackend(route, providerEndpointResponses) {
			return providerEndpointResponses, nil
		}
		return "", missingResponsesChatReplayError()
	}
	if explicitRouteHasChatBackend(route, providerEndpointChatCompletions) {
		return providerEndpointChatCompletions, nil
	}
	if explicitRouteHasChatBackend(route, providerEndpointResponses) {
		return providerEndpointResponses, nil
	}
	return "", &providerRequestError{
		statusCode: http.StatusBadRequest,
		err:        fmt.Errorf("model %q does not support %s", route.public.id, providerEndpointChatCompletions),
	}
}

func policyAwareChatExecutionEndpoint(operation *routeOperation, body []byte) (string, error) {
	if operation == nil {
		return explicitChatExecutionEndpoint(nil, body)
	}
	if plan, planned := operation.policyPlan(); planned && chatRequestContainsResponsesReplayID(body) &&
		plan.allowsResponsesReplayPassthrough() && explicitRouteHasChatBackend(operation.route, providerEndpointChatCompletions) {
		return providerEndpointChatCompletions, nil
	}
	return explicitChatExecutionEndpoint(operation.route, body)
}

func explicitResolvedChatRouteForTarget(route *modelRoute, target targetBinding, endpoint string, backend chatBackend) resolvedChatRoute {
	if route == nil || target.provider == nil {
		return resolvedChatRoute{}
	}
	return newResolvedChatRoute(
		target.provider,
		providerModelFromRouteTarget(route, target),
		true,
		route.public.id,
		endpoint,
		backend,
	)
}

func explicitResolvedChatRouteForResponse(operation *routeOperation, resp *http.Response, endpoint string, backend chatBackend) (resolvedChatRoute, targetBinding, bool) {
	if operation == nil || operation.route == nil {
		return resolvedChatRoute{}, targetBinding{}, false
	}
	if _, target, ok := explicitRouteTargetForResponse(operation, resp); ok {
		return explicitResolvedChatRouteForTarget(operation.route, target, endpoint, backend), target, true
	}
	if pinned := operation.pinnedTarget(); pinned != "" {
		if target, ok := operation.route.targetByID(pinned); ok && target.provider != nil {
			return explicitResolvedChatRouteForTarget(operation.route, target, endpoint, backend), target, true
		}
	}
	return resolvedChatRoute{}, targetBinding{}, false
}

func attachExplicitChatExecutionErrorRoute(err error, route *modelRoute, target targetBinding, endpoint string, backend chatBackend) {
	if err == nil {
		return
	}
	if target.provider == nil && route != nil {
		target, _ = route.primaryTarget()
	}
	attachChatExecutionErrorRoute(err, explicitResolvedChatRouteForTarget(route, target, endpoint, backend))
}

// withAdmittedExplicitRouteOperation establishes an explicit-route operation as
// soon as the HTTP surface can resolve the requested model. Admission does not
// require the surface endpoint to be supported: endpoint eligibility and strict
// request validation must still return the proxy-owned operation ID. Later
// execution attaches this operation to its upstream context and reuses it via
// withExplicitRouteOperation.
func (h *ProxyHandler) withAdmittedExplicitRouteOperation(ctx, inbound context.Context, model, endpoint string) (context.Context, *routeOperation, *modelRoute, error) {
	model = strings.TrimSpace(model)
	if model != "" && !h.modelAllowedForRequest(model, endpoint) {
		return ctx, nil, nil, modelNotAllowedRequestError(model)
	}
	route, known := h.resolveModelRouteForRequest(model, endpoint)
	if !known || route == nil || route.legacy {
		return ctx, nil, route, nil
	}

	operation := routeOperationFromContext(ctx)
	if operation != nil {
		if operation.route != route {
			return ctx, nil, route, fmt.Errorf("route operation for %q cannot execute route %q", operation.route.public.id, route.public.id)
		}
	} else {
		operation = newRouteOperation(route, inbound)
		ctx = withRouteOperation(ctx, operation)
	}

	if summary := RequestSummaryFromContext(inbound); summary != nil {
		summary.SetOperationID(operation.operationID())
		summary.SetRouteID(route.public.routeID)
	}
	return ctx, operation, route, nil
}

func withPlannedChatOperation(ctx, inbound context.Context, plan chatOperationPlan) (context.Context, *routeOperation, error) {
	if !plan.valid() {
		return ctx, nil, fmt.Errorf("policy planner returned an invalid Chat operation plan")
	}
	if existing := routeOperationFromContext(ctx); existing != nil {
		return ctx, nil, fmt.Errorf("route operation for %q was admitted before policy planning", existing.route.public.id)
	}
	operation := newRouteOperationFromChatPlan(plan, inbound)
	if operation == nil {
		return ctx, nil, fmt.Errorf("policy planner returned an unusable Chat operation plan")
	}
	if summary := RequestSummaryFromContext(inbound); summary != nil {
		summary.SetOperationID(operation.operationID())
		summary.SetRouteID(plan.publicID)
		summary.SetPolicyDecision(plan)
	}
	return withRouteOperation(ctx, operation), operation, nil
}

func (h *ProxyHandler) withChatExecutionRoute(ctx, inbound context.Context, model string, body []byte) (context.Context, *routeOperation, *modelRoute, error) {
	if operation := routeOperationFromContext(ctx); operation != nil {
		if _, planned := operation.policyPlan(); planned {
			endpoint, err := policyAwareChatExecutionEndpoint(operation, body)
			if err != nil {
				backend := chatBackendNativeChat
				if endpoint == providerEndpointResponses || operation.route.supportsEndpoint(providerEndpointResponses) && !operation.route.supportsEndpoint(providerEndpointChatCompletions) {
					backend = chatBackendResponses
				}
				attachExplicitChatExecutionErrorRoute(err, operation.route, targetBinding{}, endpoint, backend)
				return ctx, nil, operation.route, err
			}
			return ctx, operation, operation.route, nil
		}
	}
	route, known := h.resolveModelRouteForRequest(model, providerEndpointChatCompletions)
	if !known || route == nil || route.legacy {
		return ctx, nil, route, nil
	}
	endpoint, err := explicitChatExecutionEndpoint(route, body)
	if err != nil {
		backend := chatBackendNativeChat
		if endpoint == providerEndpointResponses || route.supportsEndpoint(providerEndpointResponses) && !route.supportsEndpoint(providerEndpointChatCompletions) {
			backend = chatBackendResponses
		}
		attachExplicitChatExecutionErrorRoute(err, route, targetBinding{}, endpoint, backend)
		return ctx, nil, route, err
	}
	return h.withExplicitRouteOperation(ctx, inbound, model, endpoint)
}

func convertedExplicitChatSafeHeaders(resp *http.Response, publicModel string) http.Header {
	if resp == nil {
		return nil
	}
	headers := convertedChatSafeHeaders(resp.Header)
	for _, name := range []string{"Openai-Model", "X-Openai-Model"} {
		if resp.Header.Get(name) == "" {
			continue
		}
		if headers == nil {
			headers = make(http.Header)
		}
		headers.Set(name, publicModel)
	}
	return headers
}

func explicitResponsesChatReplayRoute(route *modelRoute, target targetBinding) responsesChatReplayRoute {
	if route == nil || target.provider == nil {
		return responsesChatReplayRoute{}
	}
	upstreamModel := strings.TrimSpace(target.upstreamModel)
	if upstreamModel == "" {
		upstreamModel = strings.TrimSpace(route.public.id)
	}
	return responsesChatReplayRoute{
		ProviderID:    target.provider.id,
		PublicModel:   route.public.id,
		UpstreamModel: upstreamModel,
	}
}

func isMissingResponsesChatReplayError(err error) bool {
	var executionErr *chatExecutionError
	return errors.As(err, &executionErr) && executionErr.Code == responsesChatReplayMissingCode
}

func (h *ProxyHandler) prepareExplicitResponsesChatRequest(operation *routeOperation, route *modelRoute, chatBody []byte, options chatExecutionOptions) (responsesChatRequestPlan, targetBinding, error) {
	translateForTarget := func(target targetBinding) (responsesChatRequestPlan, error) {
		return translateChatRequestToResponses(chatBody, responsesChatRequestOptions{
			UpstreamModel:       route.public.id,
			ReplayStore:         h.responsesChatReplayStore(),
			ReplayRoute:         explicitResponsesChatReplayRoute(route, target),
			MinimumOutputTokens: options.ResponsesMinimumOutputTokens,
			DropSamplingParams:  options.ResponsesDropSamplingParams,
		})
	}

	if !chatRequestContainsResponsesReplayID(chatBody) {
		target, _ := route.primaryTarget()
		plan, err := translateForTarget(target)
		return plan, target, err
	}

	candidates := route.targets
	if operation != nil {
		if pinned := operation.pinnedTarget(); pinned != "" {
			if target, ok := route.targetByID(pinned); ok {
				candidates = []targetBinding{target}
			} else {
				candidates = nil
			}
		}
	}
	var missing error
	for _, target := range candidates {
		if target.provider == nil || !target.provider.supportsEndpoint(providerEndpointResponses) {
			continue
		}
		plan, err := translateForTarget(target)
		if err == nil {
			if operation != nil {
				if pinErr := operation.forcePinnedTarget(target.id); pinErr != nil {
					return responsesChatRequestPlan{}, target, pinErr
				}
			}
			return plan, target, nil
		}
		if isMissingResponsesChatReplayError(err) {
			missing = err
			continue
		}
		return responsesChatRequestPlan{}, target, err
	}
	if missing == nil {
		missing = missingResponsesChatReplayError()
	}
	return responsesChatRequestPlan{}, targetBinding{}, missing
}

func (h *ProxyHandler) executeExplicitResponsesChat(ctx context.Context, route *modelRoute, chatBody []byte, requestedModel string, options chatExecutionOptions) (chatExecutionResult, error) {
	operation := routeOperationFromContext(ctx)
	plan, plannedTarget, err := h.prepareExplicitResponsesChatRequest(operation, route, chatBody, options)
	if err != nil {
		attachExplicitChatExecutionErrorRoute(err, route, plannedTarget, providerEndpointResponses, chatBackendResponses)
		return chatExecutionResult{}, err
	}

	var headers http.Header
	if plan.Stream {
		headers = make(http.Header)
		headers.Set("Accept", "text/event-stream")
	}
	resp, err := h.postResponsesWithHeadersForModel(ctx, plan.Body, headers, requestedModel)
	if err != nil {
		return chatExecutionResult{}, err
	}

	resolved, target, ok := explicitResolvedChatRouteForResponse(operation, resp, providerEndpointResponses, chatBackendResponses)
	if !ok {
		_ = resp.Body.Close()
		return chatExecutionResult{}, fmt.Errorf("explicit Responses-backed Chat response has no route target attribution")
	}
	safeHeaders := convertedExplicitChatSafeHeaders(resp, route.public.id)
	result := chatExecutionResult{
		Response:     resp,
		Headers:      safeHeaders,
		IncludeUsage: plan.IncludeUsage,
		Backend:      chatBackendResponses,
		route:        resolved,
	}
	if resp.StatusCode != http.StatusOK {
		if err := canonicalizeResponsesChatHTTPError(resp, safeHeaders); err != nil {
			return chatExecutionResult{}, err
		}
		return result, nil
	}

	responseOptions := responsesChatResponseOptions{
		PublicModel:        route.public.id,
		ReplayStore:        h.responsesChatReplayStore(),
		ReplayRoute:        explicitResponsesChatReplayRoute(route, target),
		ReplayToolDefaults: plan.ReplayToolDefaults,
		UsageOnly:          options.ResponsesUsageOnly,
	}
	if plan.Stream {
		stream, streamErr := translateResponsesSSEToChat(ctx, resp.Body, responseOptions)
		if streamErr != nil {
			attachChatExecutionErrorHeaders(streamErr, safeHeaders)
			attachChatExecutionErrorRoute(streamErr, resolved)
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
	converted, err := translateResponsesJSONToChat(responseBody, responseOptions)
	if err != nil {
		attachChatExecutionErrorHeaders(err, safeHeaders)
		attachChatExecutionErrorRoute(err, resolved)
		return chatExecutionResult{}, err
	}
	result.Response = nil
	result.Completion = converted.Response
	result.CompletionBody = converted.Body
	result.Usage = converted.Usage
	return result, nil
}

func (h *ProxyHandler) executeChatCompletionsForRequestedModel(ctx context.Context, body []byte, options chatExecutionOptions, requestedModel string) (chatExecutionResult, error) {
	route, err := h.resolveChatRoute(ctx, requestedModel)
	if err != nil {
		return chatExecutionResult{}, err
	}
	if chatRequestContainsResponsesReplayID(body) && route.backend != chatBackendResponses {
		if !chatRouteAllowsEndpoint(route.provider, route.owner, route.known, providerEndpointResponses) {
			replayErr := missingResponsesChatReplayError()
			attachChatExecutionErrorRoute(replayErr, route)
			return chatExecutionResult{}, replayErr
		}
		route = newResolvedChatRoute(route.provider, route.owner, route.known, requestedModel, providerEndpointResponses, chatBackendResponses)
	}

	var result chatExecutionResult
	if route.backend == chatBackendNativeChat {
		result, err = h.executeResolvedNativeChat(ctx, route, body, options)
	} else {
		result, err = h.executeResolvedResponsesChat(ctx, route, body, options)
	}
	if err != nil {
		attachChatExecutionErrorRoute(err, route)
	}
	return result, err
}

func (h *ProxyHandler) executeRoutedChatCompletions(ctx context.Context, body []byte, mode chatCompletionsMode, options chatExecutionOptions, requestedModel string) (chatExecutionResult, error) {
	operation := routeOperationFromContext(ctx)
	if operation == nil || operation.route == nil || operation.route.legacy {
		return h.executeChatCompletionsForRequestedModel(ctx, body, options, requestedModel)
	}

	endpoint, err := policyAwareChatExecutionEndpoint(operation, body)
	if err != nil {
		attachExplicitChatExecutionErrorRoute(err, operation.route, targetBinding{}, endpoint, chatBackendNativeChat)
		return chatExecutionResult{}, err
	}
	if endpoint == providerEndpointResponses {
		return h.executeExplicitResponsesChat(ctx, operation.route, body, requestedModel, options)
	}

	resp, err := h.executeChatCompletionsRouteRequestForModel(ctx, body, mode, requestedModel)
	if err != nil {
		return chatExecutionResult{}, err
	}
	result := chatExecutionResult{
		Response: resp,
		Headers:  convertedExplicitChatSafeHeaders(resp, operation.route.public.id),
		Backend:  chatBackendNativeChat,
	}
	if resolved, _, ok := explicitResolvedChatRouteForResponse(operation, resp, providerEndpointChatCompletions, chatBackendNativeChat); ok {
		result.route = resolved
	}
	return result, nil
}

func (h *ProxyHandler) retryRoutedChatExecutionWithoutInjectedStreamOptions(ctx context.Context, result chatExecutionResult, body []byte, mode chatCompletionsMode, requestedModel string) (chatExecutionResult, []byte, chatCompletionsMode) {
	operation := routeOperationFromContext(ctx)
	if operation == nil || operation.route == nil || operation.route.legacy || result.Backend != chatBackendNativeChat || result.Response == nil {
		return h.retryChatExecutionWithoutInjectedStreamOptions(ctx, result, body, mode)
	}

	resp, retryBody, retryMode := h.retryChatCompletionsWithoutInjectedStreamOptionsForModel(ctx, result.Response, body, mode, requestedModel)
	result.Response = resp
	result.Headers = convertedExplicitChatSafeHeaders(resp, operation.route.public.id)
	if resolved, _, ok := explicitResolvedChatRouteForResponse(operation, resp, providerEndpointChatCompletions, chatBackendNativeChat); ok {
		result.route = resolved
	}
	return result, retryBody, retryMode
}

func (h *ProxyHandler) aggregateExplicitRoutedChatExecution(ctx context.Context, result chatExecutionResult, body []byte, mode chatCompletionsMode) (chatExecutionResult, error) {
	operation := routeOperationFromContext(ctx)
	if operation == nil || operation.route == nil || operation.route.legacy || result.Backend != chatBackendNativeChat || result.Response == nil || !mode.forceUpstreamStream {
		return result, nil
	}

	response, finalResp, err := h.aggregateExplicitChatCompletionsResponse(ctx, result.Response, body, mode, aggregateStreamToResponseWithProgress)
	if err != nil {
		return chatExecutionResult{}, err
	}
	if finalResp != nil {
		result.Response = finalResp
		result.Completion = nil
		result.Usage = nil
		result.Headers = convertedExplicitChatSafeHeaders(finalResp, operation.route.public.id)
		if resolved, _, ok := explicitResolvedChatRouteForResponse(operation, finalResp, providerEndpointChatCompletions, chatBackendNativeChat); ok {
			result.route = resolved
		}
		return result, nil
	}

	if response == nil {
		return chatExecutionResult{}, fmt.Errorf("explicit route aggregation returned no Chat completion")
	}
	response.Model = operation.route.public.id
	result.Response = nil
	result.Completion = response
	result.Usage = response.Usage
	result.Headers = nil
	// routeChatExecutionResult treats a native backend as an HTTP response.
	// Aggregation has already converted this result to a canonical completion.
	result.Backend = 0
	if pinned := operation.pinnedTarget(); pinned != "" {
		if target, ok := operation.route.targetByID(pinned); ok {
			result.route = explicitResolvedChatRouteForTarget(operation.route, target, providerEndpointChatCompletions, chatBackendNativeChat)
		}
	}
	return result, nil
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
	return prepareOpenAIChatCompletionsRequestWithParallelToolCalls(body, chatParallelToolCallsDefault)
}

type chatParallelToolCallsPreparation uint8

const (
	chatParallelToolCallsDefault chatParallelToolCallsPreparation = iota
	chatParallelToolCallsForceFalse
	chatParallelToolCallsOmit
)

func preparePolicyOpenAIChatCompletionsRequest(body []byte, contract publicModelContract, terminalParallelToolCalls *bool) ([]byte, chatCompletionsMode) {
	return prepareOpenAIChatCompletionsRequestWithParallelToolCalls(body, policyParallelToolCallsPreparation(contract, terminalParallelToolCalls))
}

func applyPolicyOpenAIChatParallelToolCalls(body []byte, contract publicModelContract, terminalParallelToolCalls *bool) []byte {
	switch policyParallelToolCallsPreparation(contract, terminalParallelToolCalls) {
	case chatParallelToolCallsForceFalse:
		return enforceParallelToolCallsFalse(body)
	case chatParallelToolCallsOmit:
		return omitParallelToolCalls(body)
	default:
		return injectParallelToolCalls(body)
	}
}

func policyParallelToolCallsPreparation(contract publicModelContract, terminalParallelToolCalls *bool) chatParallelToolCallsPreparation {
	preparation := chatParallelToolCallsDefault
	if contract.policy.parallelToolCalls == nil || !*contract.policy.parallelToolCalls {
		preparation = chatParallelToolCallsOmit
		if terminalParallelToolCalls != nil && *terminalParallelToolCalls {
			preparation = chatParallelToolCallsForceFalse
		}
	}
	return preparation
}

func prepareOpenAIChatCompletionsRequestWithParallelToolCalls(body []byte, preparation chatParallelToolCallsPreparation) ([]byte, chatCompletionsMode) {
	mode := parseOpenAIChatCompletionsMode(body)
	switch preparation {
	case chatParallelToolCallsForceFalse:
		body = enforceParallelToolCallsFalse(body)
	case chatParallelToolCallsOmit:
		body = omitParallelToolCalls(body)
	default:
		body = injectParallelToolCalls(body)
	}
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

// extractOpenAIChatCompletionsRequestModel follows the encoding/json decode
// semantics used by chat preparation: when a request contains duplicate model
// keys, the last occurrence is the decoded value.
func extractOpenAIChatCompletionsRequestModel(body []byte) string {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return ""
	}
	var model string
	if err := json.Unmarshal(payload["model"], &model); err != nil {
		return ""
	}
	return strings.TrimSpace(model)
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
			if _, known := setup.lookupRoute(rawModel); known {
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
	provider, owner, known := h.resolveProviderModelForRequest(model, providerEndpointMessages)
	if provider == nil {
		return false
	}
	if provider.kind == providerTypeAnthropicCompatible {
		return true
	}
	// Copilot's Claude models natively serve Anthropic Messages. Forward directly
	// only when the catalog explicitly advertises /v1/messages so unknown models
	// (empty endpoint list = "supports everything") still translate through Chat.
	if provider.kind == providerTypeCopilot && known && supportsEndpoint(owner.supportedEndpoints, providerEndpointMessages) {
		return true
	}
	return false
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
	upstreamCtx = withRouteOperation(upstreamCtx, routeOperationFromContext(r.Context()))
	upstreamCtx, routeOperation, route, err := h.withExplicitRouteOperation(upstreamCtx, r.Context(), publicModel, providerEndpointMessages)
	if err != nil {
		statusCode := upstreamStatusCode(err, http.StatusBadRequest)
		writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), err.Error())
		return
	}
	if routeOperation != nil {
		w.Header().Set("X-Vekil-Request-ID", routeOperation.operationID())
	}

	publicModel = explicitRoutePublicModel(route, publicModel)
	resp, err := h.executeAnthropicMessagesRouteRequest(upstreamCtx, body, anthropicExtraHeadersFromRequest(r), streaming, req.Model)
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

	publicModel, upstreamModel = explicitAnthropicResponseModels(routeOperation, resp, publicModel, upstreamModel)
	if resp.StatusCode == http.StatusOK && streaming {
		markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentProtocolFrame)
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

func (h *ProxyHandler) postAnthropicMessagesCountTokensForModel(ctx context.Context, body []byte, extraHeaders http.Header, model string) (*http.Response, error) {
	if operation := routeOperationFromContext(ctx); operation != nil && operation.route != nil && !operation.route.legacy {
		return h.executeExplicitRouteRequestPath(ctx, operation.route, providerEndpointMessages, providerEndpointMessagesCount, body, extraHeaders, model, false)
	}
	if route, known := h.resolveModelRouteForRequest(model, providerEndpointMessages); known && route != nil && !route.legacy {
		return h.executeExplicitRouteRequestPath(ctx, route, providerEndpointMessages, providerEndpointMessagesCount, body, extraHeaders, model, false)
	}

	provider, owner, rewrittenBody, err := h.resolveProviderRequestForModel(body, providerEndpointMessages, model)
	if err != nil {
		return nil, err
	}
	if provider.kind != providerTypeAnthropicCompatible {
		return nil, &providerRequestError{
			statusCode: http.StatusBadRequest,
			err:        fmt.Errorf("provider %q does not support %s", provider.id, providerEndpointMessagesCount),
		}
	}

	return h.doWithRetry(func() (*http.Request, error) {
		return h.newProviderJSONRequest(ctx, provider, http.MethodPost, providerEndpointMessagesCount, rewrittenBody, extraHeaders, "", owner)
	})
}

func (h *ProxyHandler) forwardAnthropicCountTokensDirect(w http.ResponseWriter, r *http.Request, body []byte, model string) {
	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(false)
	defer upstreamCancel()
	upstreamCtx = withRouteOperation(upstreamCtx, routeOperationFromContext(r.Context()))
	upstreamCtx, routeOperation, _, err := h.withExplicitRouteOperation(upstreamCtx, suppressRouteAttemptStats(r.Context()), model, providerEndpointMessages)
	if err != nil {
		statusCode := upstreamStatusCode(err, http.StatusBadRequest)
		writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), err.Error())
		return
	}
	if routeOperation != nil {
		w.Header().Set("X-Vekil-Request-ID", routeOperation.operationID())
	}

	resp, err := h.postAnthropicMessagesCountTokensForModel(upstreamCtx, body, anthropicExtraHeadersFromRequest(r), model)
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

	admissionModel := extractOpenAIChatCompletionsRequestModel(body)
	h.observePolicyRequestSummary(r.Context(), "anthropic", admissionModel, false)
	admissionCtx, admittedOperation, _, err := h.withAdmittedExplicitRouteOperation(r.Context(), r.Context(), admissionModel, providerEndpointMessages)
	if err != nil {
		statusCode := upstreamStatusCode(err, http.StatusBadRequest)
		writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), err.Error())
		return
	}
	if admittedOperation != nil {
		r = r.WithContext(admissionCtx)
		w.Header().Set("X-Vekil-Request-ID", admittedOperation.operationID())
	}

	var req models.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		message, _ := jsonDecodeErrorDetails(err, "invalid JSON in request body")
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", message)
		return
	}
	publicModel := req.Model
	if canonicalPolicyID, ok := h.policyPublicModelID(req.Model); ok {
		publicModel = canonicalPolicyID
		ensurePolicyLocalRequestIdentity(w, r, publicModel)
	}
	// Route-aware duplicate-key validation must use the same selected model as
	// handler forwarding. encoding/json resolves duplicate struct fields with the
	// last occurrence, so validate only after decoding req.Model.
	if err := h.validateRouteAwareRequestJSON(body, req.Model, providerEndpointMessages); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
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

	provider, _, known := h.resolveProviderModelForRequest(req.Model, providerEndpointMessages)
	if strings.TrimSpace(req.Model) != "" && !known && providerUsesDynamicModels(provider) {
		if err := h.refreshUnknownChatRouteProvider(r.Context(), provider); err != nil {
			if h.handleShutdownError(w, r, r.Context(), err) {
				return
			}
			statusCode := upstreamStatusCode(err, http.StatusBadRequest)
			writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), err.Error())
			return
		}
	}

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
	policyPlan, err := h.planOpenAIChatPolicy(r.Context(), req.Model, oaiBody)
	if err != nil {
		if h.handleShutdownError(w, r, nil, err) {
			return
		}
		statusCode := upstreamStatusCode(err, http.StatusBadRequest)
		if statusCode == http.StatusBadRequest {
			writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), err.Error())
		} else {
			writePolicyAnthropicSanitizedError(w, statusCode, policyChatErrorHeaders(err), publicModel)
		}
		return
	}
	if policyPlan.valid() {
		if admittedOperation != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", "policy model was also admitted as a direct route")
			return
		}
		plannedCtx, plannedOperation, planErr := withPlannedChatOperation(r.Context(), r.Context(), policyPlan)
		if planErr != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", planErr.Error())
			return
		}
		r = r.WithContext(plannedCtx)
		w.Header().Set("X-Vekil-Request-ID", plannedOperation.operationID())
		publicModel = policyPlan.publicID

		// The translated Anthropic request has already established whether the
		// client asked for streaming and whether Vekil must force-stream upstream.
		// Apply only the policy request contract from the returned body; the mode
		// parsed by the OpenAI helper would misclassify an injected stream as a
		// client-requested stream and leak SSE to a non-streaming Anthropic client.
		oaiBody = applyPolicyOpenAIChatParallelToolCalls(oaiBody, policyPlan.contract, cloneBoolPtr(policyPlan.terminalParallelToolCalls))
	}
	oaiBody = h.rewriteOpenAIChatRequestBodyWithToolOptimizers(r.Context(), oaiBody, h.toolContexts, scope)

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContextFrom(r.Context(), mode.clientRequestedStream || mode.forceUpstreamStream)
	defer upstreamCancel()
	upstreamCtx = withRouteOperation(upstreamCtx, routeOperationFromContext(r.Context()))
	upstreamCtx, routeOperation, route, err := h.withChatExecutionRoute(upstreamCtx, r.Context(), providerModel, oaiBody)
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		var executionErr *chatExecutionError
		if errors.As(err, &executionErr) {
			observeChatExecutionError(r.Context(), executionErr)
			observeOpenAIUsage(r.Context(), executionErr.Usage)
			if policyPlan.valid() {
				writePolicyAnthropicSanitizedError(w, executionErr.StatusCode, executionErr.Headers, publicModel)
			} else {
				writeAnthropicError(w, executionErr.StatusCode, mapAnthropicUpstreamStatus(executionErr.StatusCode), executionErr.Message)
			}
			return
		}
		statusCode := upstreamStatusCode(err, http.StatusBadRequest)
		if policyPlan.valid() {
			writePolicyAnthropicSanitizedError(w, statusCode, policyChatErrorHeaders(err), publicModel)
		} else {
			writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), err.Error())
		}
		return
	}
	if routeOperation != nil {
		w.Header().Set("X-Vekil-Request-ID", routeOperation.operationID())
	}

	if !policyPlan.valid() {
		publicModel = explicitRoutePublicModel(route, publicModel)
	}
	responseReq := req
	responseReq.Model = publicModel
	result, err := h.executeRoutedChatCompletions(upstreamCtx, oaiBody, mode, chatExecutionOptions{}, providerModel)
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		var executionErr *chatExecutionError
		if errors.As(err, &executionErr) {
			observeChatExecutionError(r.Context(), executionErr)
			if !policyPlan.valid() && len(executionErr.Headers) > 0 {
				mergeHeaderValues(w.Header(), executionErr.Headers)
			}
			observeOpenAIUsage(r.Context(), executionErr.Usage)
			if policyPlan.valid() {
				writePolicyAnthropicSanitizedError(w, executionErr.StatusCode, executionErr.Headers, publicModel)
			} else {
				writeAnthropicError(w, executionErr.StatusCode, mapAnthropicUpstreamStatus(executionErr.StatusCode), executionErr.Message)
			}
			return
		}
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		h.log.Error("upstream request failed", logger.F("endpoint", "anthropic"), logger.Err(err))
		if policyPlan.valid() {
			writePolicyAnthropicSanitizedError(w, statusCode, policyChatErrorHeaders(err), publicModel)
			return
		}
		if statusCode == http.StatusBadRequest {
			writeAnthropicError(w, statusCode, "invalid_request_error", err.Error())
			return
		}
		writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), formatUpstreamRequestFailure(err, "upstream request failed"))
		return
	}

	result, oaiBody, mode = h.retryRoutedChatExecutionWithoutInjectedStreamOptions(upstreamCtx, result, oaiBody, mode, providerModel)
	observeChatExecutionRoute(r.Context(), result)
	observeUpstreamHeaders(r.Context(), result.Headers)
	if result.Backend == chatBackendResponses && len(result.Headers) > 0 {
		mergeHeaderValues(w.Header(), result.Headers)
	}

	result, err = h.aggregateExplicitRoutedChatExecution(upstreamCtx, result, oaiBody, mode)
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		status := http.StatusBadGateway
		message := "failed to process upstream response"
		var executionErr *chatExecutionError
		if errors.As(err, &executionErr) {
			status = chatStreamErrorStatus(executionErr)
			observeOpenAIUsage(r.Context(), executionErr.Usage)
			if policyPlan.valid() {
				writePolicyAnthropicSanitizedError(w, status, executionErr.Headers, publicModel)
				return
			}
			message = chatStreamErrorMessage(executionErr)
		} else {
			var streamErr *openAIStreamError
			if errors.As(err, &streamErr) {
				status = streamErr.httpStatus()
				if policyPlan.valid() {
					message, _, _ = policyChatUpstreamErrorDetails(status)
				} else {
					message = streamErr.Error()
				}
			}
		}
		if policyPlan.valid() {
			writePolicyAnthropicSanitizedError(w, status, policyChatErrorHeaders(err), publicModel)
			return
		}
		writeAnthropicError(w, status, mapAnthropicUpstreamStatus(status), message)
		return
	}
	observeChatExecutionRoute(r.Context(), result)
	if len(result.Headers) > 0 {
		observeUpstreamHeaders(r.Context(), result.Headers)
	}

	if result.Response != nil && result.Response.StatusCode != http.StatusOK {
		resp := result.Response
		if policyPlan.valid() {
			_ = writePolicyAnthropicTerminalError(w, resp, publicModel)
			return
		}
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
			markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentProtocolFrame)
			lifecycleBody := newLifecycleAwareReadCloser(resp.Body, upstreamCtx)
			var body io.ReadCloser = lifecycleBody
			if policyPlan.valid() {
				mergeHeaderValues(w.Header(), policyChatSafeHeaders(resp.Header, publicModel))
				body = newPolicySanitizedOpenAIStream(body)
			}
			streamOpenAIToAnthropicWithLifecycle(
				w,
				body,
				publicModel,
				"msg_"+uuid.New().String(),
				func(status int) { observeResponseFailureStatus(r.Context(), status) },
				h.openAIChatStreamFinalResponseCallback(r.Context(), h.toolContexts, scope),
				h.lifecycleStreamHooks(r.Context(), lifecycleBody.canceledAtFailure, func() { h.WriteShutdownServiceUnavailable(w, r) }),
				openAIChatStreamUsageCallback(r.Context()),
			)
		},
		streamEvents: func(stream *chatStreamEventStream) {
			markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentProtocolFrame)
			tracked := &commitTrackingResponseWriter{ResponseWriter: w}
			err := streamChatEventsToAnthropic(tracked, stream, publicModel, "msg_"+uuid.New().String(), chatStreamEventCallbacks{
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
			markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentSemantic)
			observeOpenAIUsage(r.Context(), oaiResp.Usage)
			h.maybeRewriteOrCaptureOpenAIChatToolCommands(r.Context(), oaiResp, h.toolContexts, scope, false)
			anthropicResp := translateOpenAIToAnthropicForRequest(oaiResp, &responseReq)
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
			markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentSemantic)
			w.Header().Set("Content-Type", "application/json")
			return json.NewEncoder(w).Encode(translateOpenAIToAnthropicForRequest(&oaiResp, &responseReq))
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
			observeOpenAIUsage(r.Context(), executionErr.Usage)
			if policyPlan.valid() {
				writePolicyAnthropicSanitizedError(w, status, executionErr.Headers, publicModel)
				return
			}
			message = chatStreamErrorMessage(executionErr)
		} else {
			var streamErr *openAIStreamError
			if errors.As(err, &streamErr) {
				status = streamErr.httpStatus()
				if policyPlan.valid() {
					message, _, _ = policyChatUpstreamErrorDetails(status)
				} else {
					message = streamErr.Error()
				}
			}
		}
		if policyPlan.valid() {
			writePolicyAnthropicSanitizedError(w, status, policyChatErrorHeaders(err), publicModel)
			return
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

	admissionModel := extractOpenAIChatCompletionsRequestModel(body)
	h.observePolicyRequestSummary(r.Context(), "anthropic_count_tokens", admissionModel, false)
	admissionInbound := suppressRouteAttemptStats(r.Context())
	admissionCtx, admittedOperation, _, err := h.withAdmittedExplicitRouteOperation(r.Context(), admissionInbound, admissionModel, providerEndpointMessages)
	if err != nil {
		statusCode := upstreamStatusCode(err, http.StatusBadRequest)
		writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), err.Error())
		return
	}
	if admittedOperation != nil {
		r = r.WithContext(admissionCtx)
		w.Header().Set("X-Vekil-Request-ID", admittedOperation.operationID())
	}

	var req models.AnthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		message, _ := jsonDecodeErrorDetails(err, "invalid JSON in request body")
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", message)
		return
	}
	publicModel := req.Model
	if canonicalPolicyID, ok := h.policyPublicModelID(req.Model); ok {
		publicModel = canonicalPolicyID
		ensurePolicyLocalRequestIdentity(w, r, publicModel)
	}
	// Route-aware duplicate-key validation must use the same selected model as
	// handler forwarding. encoding/json resolves duplicate struct fields with the
	// last occurrence, so validate only after decoding req.Model.
	if err := h.validateRouteAwareRequestJSON(body, req.Model, providerEndpointMessages); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
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
		h.forwardAnthropicCountTokensDirect(w, r, body, req.Model)
		return
	}

	oaiReq, err := prepareAnthropicCountTokensProbeRequestWithModelOverride(&req, providerModel)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", fmt.Sprintf("translation error: %v", err))
		return
	}
	if _, policyModel := h.policyPublicModelID(req.Model); policyModel {
		// A native-Chat policy terminal may itself be a Vekil-compatible bridge
		// backed by Responses. Use the known Responses minimum up front so a
		// one-send terminal budget does not require protocol recovery.
		minimum := responsesChatMinimumOutputTokens
		oaiReq.MaxCompletionTokens = &minimum
	}
	policyBody, err := json.Marshal(oaiReq)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to prepare count_tokens policy request")
		return
	}
	policyPlan, err := h.planOpenAIChatPolicy(r.Context(), req.Model, policyBody)
	if err != nil {
		if h.handleShutdownError(w, r, nil, err) {
			return
		}
		statusCode := upstreamStatusCode(err, http.StatusBadRequest)
		if statusCode == http.StatusBadRequest {
			writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), err.Error())
		} else {
			writePolicyAnthropicSanitizedError(w, statusCode, policyChatErrorHeaders(err), publicModel)
		}
		return
	}
	if policyPlan.valid() {
		if admittedOperation != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", "policy model was also admitted as a direct route")
			return
		}
		policyBody = applyPolicyOpenAIChatParallelToolCalls(policyBody, policyPlan.contract, cloneBoolPtr(policyPlan.terminalParallelToolCalls))
		var preparedProbe models.OpenAIRequest
		if err := json.Unmarshal(policyBody, &preparedProbe); err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", "failed to apply count_tokens policy contract")
			return
		}
		oaiReq = &preparedProbe
		plannedCtx, plannedOperation, planErr := withPlannedChatOperation(r.Context(), suppressRouteAttemptStats(r.Context()), policyPlan)
		if planErr != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "api_error", planErr.Error())
			return
		}
		r = r.WithContext(plannedCtx)
		w.Header().Set("X-Vekil-Request-ID", plannedOperation.operationID())
		publicModel = policyPlan.publicID
	}

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContext(false)
	defer upstreamCancel()
	upstreamCtx = withRouteOperation(upstreamCtx, routeOperationFromContext(r.Context()))
	upstreamCtx, routeOperation, _, err := h.withChatExecutionRoute(upstreamCtx, suppressRouteAttemptStats(r.Context()), providerModel, policyBody)
	if err != nil {
		var executionErr *chatExecutionError
		if errors.As(err, &executionErr) {
			observeChatExecutionError(r.Context(), executionErr)
			if policyPlan.valid() {
				writePolicyAnthropicSanitizedError(w, executionErr.StatusCode, executionErr.Headers, publicModel)
			} else {
				writeAnthropicError(w, executionErr.StatusCode, mapAnthropicUpstreamStatus(executionErr.StatusCode), executionErr.Message)
			}
			return
		}
		statusCode := upstreamStatusCode(err, http.StatusBadRequest)
		if policyPlan.valid() {
			writePolicyAnthropicSanitizedError(w, statusCode, policyChatErrorHeaders(err), publicModel)
		} else {
			writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), err.Error())
		}
		return
	}
	if routeOperation != nil {
		w.Header().Set("X-Vekil-Request-ID", routeOperation.operationID())
	}

	oaiResp, err := h.runAnthropicCountTokensProbeWithContext(upstreamCtx, oaiReq)
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		var executionErr *chatExecutionError
		if errors.As(err, &executionErr) {
			observeChatExecutionError(r.Context(), executionErr)
			if !policyPlan.valid() && len(executionErr.Headers) > 0 {
				mergeHeaderValues(w.Header(), executionErr.Headers)
			}
			statusCode := chatStreamErrorStatus(executionErr)
			observeOpenAIUsage(r.Context(), executionErr.Usage)
			if policyPlan.valid() {
				writePolicyAnthropicSanitizedError(w, statusCode, executionErr.Headers, publicModel)
			} else {
				writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), chatStreamErrorMessage(executionErr))
			}
			return
		}
		statusCode := upstreamStatusCode(err, http.StatusBadGateway)
		h.log.Error("upstream request failed", logger.F("endpoint", "anthropic_count_tokens"), logger.Err(err))
		if policyPlan.valid() {
			writePolicyAnthropicSanitizedError(w, statusCode, policyChatErrorHeaders(err), publicModel)
			return
		}
		if statusCode == http.StatusBadRequest {
			writeAnthropicError(w, statusCode, "invalid_request_error", err.Error())
			return
		}
		writeAnthropicError(w, statusCode, mapAnthropicUpstreamStatus(statusCode), formatUpstreamRequestFailure(err, "upstream request failed"))
		return
	}

	if oaiResp.Usage == nil {
		if policyPlan.valid() {
			writePolicyAnthropicSanitizedError(w, http.StatusBadGateway, nil, publicModel)
		} else {
			writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream response did not include usage")
		}
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
	options := chatExecutionOptions{
		ResponsesMinimumOutputTokens: responsesChatMinimumOutputTokens,
		ResponsesDropSamplingParams:  true,
		ResponsesUsageOnly:           true,
	}
	result, err := h.executeRoutedChatCompletions(upstreamCtx, body, chatCompletionsMode{}, options, probeReq.Model)
	if err != nil {
		return nil, err
	}
	if result.Backend == chatBackendNativeChat && result.Response != nil && result.Response.StatusCode == http.StatusBadRequest && probeReq.MaxCompletionTokens != nil {
		original := result.Response
		if operation := routeOperationFromContext(upstreamCtx); operation != nil {
			captured, cleanupDone := captureRouteResponse(original)
			original = captured.response()
			if !cleanupDone {
				return h.decodeAnthropicCountTokensProbeResponse(original)
			}
		} else if original.Body != nil {
			_ = original.Body.Close()
		}

		one := 1
		probeReq.MaxCompletionTokens = nil
		probeReq.MaxTokens = &one
		fallbackBody, marshalErr := json.Marshal(probeReq)
		if marshalErr != nil {
			return nil, fmt.Errorf("failed to marshal count_tokens fallback request: %w", marshalErr)
		}
		if routeOperationFromContext(upstreamCtx) != nil {
			result, err = h.executeRoutedChatCompletions(withRouteAttemptKind(upstreamCtx, routeAttemptProtocolRecovery), fallbackBody, chatCompletionsMode{}, options, probeReq.Model)
		} else {
			result, err = h.retryResolvedNativeChat(upstreamCtx, result, fallbackBody)
		}
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
		return nil, &upstreamError{
			statusCode: resp.StatusCode,
			body:       errBody,
			retryAfter: resp.Header.Get("Retry-After"),
			headers:    resp.Header.Clone(),
		}
	}

	var oaiResp models.OpenAIResponse
	if err := json.NewDecoder(resp.Body).Decode(&oaiResp); err != nil {
		return nil, fmt.Errorf("failed to parse upstream count_tokens probe response: %w", err)
	}

	return &oaiResp, nil
}

func ensurePolicyLocalRequestIdentity(w http.ResponseWriter, r *http.Request, publicModel string) {
	publicModel = strings.TrimSpace(publicModel)
	if publicModel == "" {
		return
	}
	for _, name := range []string{"Openai-Model", "X-Openai-Model"} {
		w.Header().Set(name, publicModel)
	}
	if w.Header().Get("X-Vekil-Request-ID") != "" {
		return
	}
	operationID := uuid.NewString()
	w.Header().Set("X-Vekil-Request-ID", operationID)
	if r != nil {
		if summary := RequestSummaryFromContext(r.Context()); summary != nil {
			summary.SetOperationID(operationID)
		}
	}
}

func policyChatSafeHeaders(src http.Header, publicModel string) http.Header {
	if len(src) == 0 {
		return nil
	}
	dst := make(http.Header)
	for key, values := range src {
		lower := strings.ToLower(strings.TrimSpace(key))
		for _, value := range values {
			value = strings.TrimSpace(value)
			switch lower {
			case "retry-after":
				if policyChatRetryAfter(value) {
					dst.Add(key, value)
				}
			case "ratelimit-limit":
				if policyChatRateLimitLimit(value) {
					dst.Add(key, value)
				}
			case "ratelimit-remaining", "ratelimit-reset",
				"x-ratelimit-limit", "x-ratelimit-remaining", "x-ratelimit-reset",
				"x-ratelimit-limit-requests", "x-ratelimit-limit-tokens":
				if policyChatNonNegativeInteger(value) {
					dst.Add(key, value)
				}
			case "x-ratelimit-remaining-requests", "x-ratelimit-remaining-tokens":
				if policyChatInteger(value) {
					dst.Add(key, value)
				}
			case "x-ratelimit-reset-requests", "x-ratelimit-reset-tokens":
				if policyChatResetValue(value) {
					dst.Add(key, value)
				}
			}
		}
	}
	for _, name := range []string{"Openai-Model", "X-Openai-Model"} {
		if src.Get(name) == "" {
			continue
		}
		dst.Set(name, publicModel)
	}
	if len(dst) == 0 {
		return nil
	}
	return dst
}

func policyChatSuccessHeaders(src http.Header, publicModel string) http.Header {
	dst := policyChatSafeHeaders(src, publicModel)
	for _, name := range []string{"Content-Type", "Content-Length", "Content-Encoding"} {
		values := src.Values(name)
		if len(values) == 0 {
			continue
		}
		if dst == nil {
			dst = make(http.Header)
		}
		for _, value := range values {
			dst.Add(name, value)
		}
	}
	return dst
}

func policyChatInteger(value string) bool {
	if value == "" {
		return false
	}
	_, err := strconv.ParseInt(value, 10, 64)
	return err == nil
}

func policyChatNonNegativeInteger(value string) bool {
	if value == "" {
		return false
	}
	_, err := strconv.ParseUint(value, 10, 64)
	return err == nil
}

func policyChatRetryAfter(value string) bool {
	if policyChatNonNegativeInteger(value) {
		return true
	}
	_, err := http.ParseTime(value)
	return err == nil
}

func policyChatResetValue(value string) bool {
	if policyChatNonNegativeInteger(value) {
		return true
	}
	delay, err := time.ParseDuration(value)
	return err == nil && delay >= 0
}

func policyChatRateLimitLimit(value string) bool {
	items := strings.Split(value, ",")
	if len(items) == 0 {
		return false
	}
	for _, item := range items {
		parts := strings.Split(item, ";")
		if !policyChatNonNegativeInteger(strings.TrimSpace(parts[0])) {
			return false
		}
		seenWindow := false
		for _, parameter := range parts[1:] {
			name, rawValue, ok := strings.Cut(strings.TrimSpace(parameter), "=")
			if !ok || seenWindow || !strings.EqualFold(strings.TrimSpace(name), "w") ||
				!policyChatNonNegativeInteger(strings.TrimSpace(rawValue)) {
				return false
			}
			seenWindow = true
		}
	}
	return true
}

func policyChatUpstreamErrorDetails(status int) (message, errType, code string) {
	return "upstream request failed", openAIErrorTypeForHTTPStatus(status), "upstream_error"
}

func policyChatErrorHeaders(err error) http.Header {
	var executionErr *chatExecutionError
	if errors.As(err, &executionErr) {
		return executionErr.Headers
	}
	var upstreamErr *upstreamError
	if errors.As(err, &upstreamErr) {
		return upstreamErr.headers
	}
	return nil
}

func writePolicyChatSanitizedError(w http.ResponseWriter, status int, headers http.Header, publicModel string) {
	if safeHeaders := policyChatSafeHeaders(headers, publicModel); len(safeHeaders) > 0 {
		mergeHeaderValues(w.Header(), safeHeaders)
	}
	w.Header().Del("Content-Length")
	if status < http.StatusBadRequest {
		status = http.StatusBadGateway
	}
	message, errType, code := policyChatUpstreamErrorDetails(status)
	writeOpenAIErrorWithDetails(w, status, message, errType, "", code)
}

func writePolicyAnthropicSanitizedError(w http.ResponseWriter, status int, headers http.Header, publicModel string) {
	if safeHeaders := policyChatSafeHeaders(headers, publicModel); len(safeHeaders) > 0 {
		mergeHeaderValues(w.Header(), safeHeaders)
	}
	w.Header().Del("Content-Length")
	if status < http.StatusBadRequest {
		status = http.StatusBadGateway
	}
	message, _, _ := policyChatUpstreamErrorDetails(status)
	writeAnthropicError(w, status, mapAnthropicUpstreamStatus(status), message)
}

func writePolicyAnthropicTerminalError(w http.ResponseWriter, resp *http.Response, publicModel string) error {
	if resp == nil {
		return &responseBodyWriteError{err: fmt.Errorf("upstream response is unavailable"), upstream: true}
	}
	if resp.Body != nil {
		defer func() { _ = readRetryableUpstreamErrorBody(resp.Body) }()
	}
	writePolicyAnthropicSanitizedError(w, resp.StatusCode, resp.Header, publicModel)
	return nil
}

func writePolicyChatTerminalError(w http.ResponseWriter, resp *http.Response, publicModel string) error {
	if resp == nil {
		return &responseBodyWriteError{err: fmt.Errorf("upstream response is unavailable"), upstream: true}
	}
	if resp.Body != nil {
		defer func() { _ = readRetryableUpstreamErrorBody(resp.Body) }()
	}
	writePolicyChatSanitizedError(w, resp.StatusCode, resp.Header, publicModel)
	return nil
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
	requestedModel := extractOpenAIChatCompletionsRequestModel(bodyBytes)
	h.observePolicyRequestSummary(r.Context(), "openai_chat", requestedModel, false)
	publicModel := requestedModel
	admissionCtx, admittedOperation, _, err := h.withAdmittedExplicitRouteOperation(r.Context(), r.Context(), requestedModel, providerEndpointChatCompletions)
	if err != nil {
		statusCode := upstreamStatusCode(err, http.StatusBadRequest)
		writeOpenAIError(w, statusCode, err.Error(), "invalid_request_error")
		return
	}
	if admittedOperation != nil {
		r = r.WithContext(admissionCtx)
		w.Header().Set("X-Vekil-Request-ID", admittedOperation.operationID())
	}
	if err := h.validateRouteAwareRequestJSON(bodyBytes, requestedModel, providerEndpointChatCompletions); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}

	if message, param, ok := validateOpenAIChatRequest(bodyBytes); ok {
		writeOpenAIErrorWithDetails(w, http.StatusBadRequest, message, "invalid_request_error", param, "")
		return
	}

	policyPlan, err := h.planOpenAIChatPolicy(r.Context(), requestedModel, bodyBytes)
	if err != nil {
		if h.handleShutdownError(w, r, nil, err) {
			return
		}
		statusCode := upstreamStatusCode(err, http.StatusBadRequest)
		writeOpenAIError(w, statusCode, err.Error(), "invalid_request_error")
		return
	}
	terminalParallelToolCalls := cloneBoolPtr(policyPlan.terminalParallelToolCalls)
	if policyPlan.valid() {
		if admittedOperation != nil {
			writeOpenAIError(w, http.StatusInternalServerError, "policy model was also admitted as a direct route", "server_error")
			return
		}
		plannedCtx, plannedOperation, planErr := withPlannedChatOperation(r.Context(), r.Context(), policyPlan)
		if planErr != nil {
			writeOpenAIError(w, http.StatusInternalServerError, planErr.Error(), "server_error")
			return
		}
		r = r.WithContext(plannedCtx)
		w.Header().Set("X-Vekil-Request-ID", plannedOperation.operationID())
		// Preserve the actual body model for provider rewrite. Public metrics and
		// response identity use the canonical policy profile ID separately.
		publicModel = policyPlan.publicID
	}

	scope := chatToolExecutionScopeFromHeaders(r.Header)
	var mode chatCompletionsMode
	if policyPlan.valid() {
		bodyBytes, mode = preparePolicyOpenAIChatCompletionsRequest(bodyBytes, policyPlan.contract, terminalParallelToolCalls)
	} else {
		bodyBytes, mode = prepareOpenAIChatCompletionsRequest(bodyBytes)
	}
	h.observeRequestSummary(r.Context(), "openai_chat", publicModel, mode.clientRequestedStream, providerEndpointChatCompletions)
	bodyBytes = h.rewriteOpenAIChatRequestBodyWithToolOptimizers(r.Context(), bodyBytes, h.toolContexts, scope)

	upstreamCtx, upstreamCancel := h.newInferenceUpstreamContextFrom(r.Context(), mode.clientRequestedStream || mode.forceUpstreamStream)
	defer upstreamCancel()
	upstreamCtx = withRouteOperation(upstreamCtx, routeOperationFromContext(r.Context()))
	upstreamCtx, routeOperation, route, err := h.withChatExecutionRoute(upstreamCtx, r.Context(), requestedModel, bodyBytes)
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		var executionErr *chatExecutionError
		if errors.As(err, &executionErr) {
			observeChatExecutionError(r.Context(), executionErr)
			if policyPlan.valid() {
				writePolicyChatSanitizedError(w, executionErr.StatusCode, executionErr.Headers, publicModel)
			} else {
				writeOpenAIChatExecutionError(w, executionErr)
			}
			return
		}
		statusCode := upstreamStatusCode(err, http.StatusBadRequest)
		if policyPlan.valid() {
			writePolicyChatSanitizedError(w, statusCode, policyChatErrorHeaders(err), publicModel)
		} else {
			writeOpenAIError(w, statusCode, err.Error(), "invalid_request_error")
		}
		return
	}
	if routeOperation != nil {
		w.Header().Set("X-Vekil-Request-ID", routeOperation.operationID())
	}

	responseModel := explicitRoutePublicModel(route, publicModel)
	result, err := h.executeRoutedChatCompletions(upstreamCtx, bodyBytes, mode, chatExecutionOptions{}, requestedModel)
	if err != nil {
		if h.handleShutdownError(w, r, upstreamCtx, err) {
			return
		}
		if policyPlan.valid() {
			var executionErr *chatExecutionError
			if errors.As(err, &executionErr) {
				observeChatExecutionError(r.Context(), executionErr)
				observeOpenAIUsage(r.Context(), executionErr.Usage)
			}
			statusCode := upstreamStatusCode(err, http.StatusBadGateway)
			if h.log != nil {
				h.log.Error("policy terminal request failed", logger.F("endpoint", "openai"), logger.Err(err))
			}
			writePolicyChatSanitizedError(w, statusCode, policyChatErrorHeaders(err), responseModel)
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

	result, bodyBytes, mode = h.retryRoutedChatExecutionWithoutInjectedStreamOptions(upstreamCtx, result, bodyBytes, mode, requestedModel)
	observeChatExecutionRoute(r.Context(), result)
	observeUpstreamHeaders(r.Context(), result.Headers)

	result, err = h.aggregateExplicitRoutedChatExecution(upstreamCtx, result, bodyBytes, mode)
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
			if policyPlan.valid() {
				writePolicyChatSanitizedError(w, executionErr.StatusCode, executionErr.Headers, responseModel)
			} else {
				writeOpenAIChatExecutionError(w, executionErr)
			}
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
			if policyPlan.valid() {
				message, errType, code = policyChatUpstreamErrorDetails(status)
			}
		}
		writeOpenAIErrorWithDetails(w, status, message, errType, "", code)
		return
	}
	observeChatExecutionRoute(r.Context(), result)
	if len(result.Headers) > 0 {
		observeUpstreamHeaders(r.Context(), result.Headers)
	}

	if routeOperation != nil && result.Backend == chatBackendNativeChat && result.Response != nil && !mode.clientRequestedStream && !mode.forceUpstreamStream {
		if normalizeErr := normalizeExplicitOpenAIChatResponseModel(result.Response, responseModel); normalizeErr != nil {
			if h.handleResponseBodyWriteError(w, r, upstreamCtx, "openai", normalizeErr) {
				return
			}
			if h.handleShutdownError(w, r, upstreamCtx, normalizeErr) {
				return
			}
			writeOpenAIError(w, http.StatusBadGateway, "failed to read upstream response", "server_error")
			return
		}
	}

	err = h.routeChatExecutionResult(w, result, upstreamCtx, mode, chatCompletionsResponseHandlers{
		stream: func(resp *http.Response) {
			if policyPlan.valid() {
				mergeHeaderValues(w.Header(), policyChatSafeHeaders(resp.Header, responseModel))
			} else {
				copyPassthroughHeaders(w.Header(), resp.Header)
			}
			markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentProtocolFrame)
			body := newLifecycleAwareReadCloser(resp.Body, upstreamCtx)
			finalResponse := h.openAIChatStreamFinalResponseCallback(r.Context(), h.toolContexts, scope)
			usage := openAIChatStreamUsageCallback(r.Context())
			// A post-commit upstream stream error (the 200 header is already sent)
			// should be recorded as a failed request with its classified status
			// (e.g. 429 for a rate limit), not a 2xx success.
			onError := func(status int) { observeResponseFailureStatus(r.Context(), status) }
			// dropInjectedUsage drops the upstream usage-only chunk the proxy asked
			// for when the client did not opt into stream_options.include_usage.
			lifecycle := h.lifecycleStreamHooks(r.Context(), body.canceledAtFailure, func() { h.WriteShutdownServiceUnavailable(w, r) })
			if routeOperation != nil {
				streamExplicitRouteOpenAIChatPassthroughWithLifecycle(w, body, responseModel, policyPlan.valid(), mode.injectedClientStreamUsage, onError, finalResponse, lifecycle, usage)
			} else {
				streamOpenAIChatPassthroughWithLifecycle(w, body, responseModel, mode.injectedClientStreamUsage, onError, finalResponse, lifecycle, usage)
			}
		},
		streamEvents: func(stream *chatStreamEventStream) {
			copyPassthroughHeaders(w.Header(), result.Headers)
			markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentProtocolFrame)
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
			markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentSemantic)
			normalizeOpenAIChatCompletionStruct(oaiResp, responseModel)
			observeOpenAIUsage(r.Context(), oaiResp.Usage)
			h.maybeRewriteOrCaptureOpenAIChatToolCommands(r.Context(), oaiResp, h.toolContexts, scope, false)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(oaiResp)
		},
		passthrough: func(resp *http.Response) error {
			markExplicitRouteDownstreamCommitment(upstreamCtx, downstreamCommitmentProtocolFrame)
			if policyPlan.valid() && resp != nil {
				if resp.StatusCode < http.StatusOK || resp.StatusCode >= http.StatusMultipleChoices {
					return writePolicyChatTerminalError(w, resp, responseModel)
				}
				resp.Header = policyChatSuccessHeaders(resp.Header, responseModel)
			}
			return h.maybeWriteOptimizedOpenAIChatPassthrough(r.Context(), w, resp, responseModel, h.toolContexts, scope)
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
			if policyPlan.valid() {
				writePolicyChatSanitizedError(w, executionErr.StatusCode, executionErr.Headers, responseModel)
			} else {
				writeOpenAIChatExecutionError(w, executionErr)
			}
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
			if policyPlan.valid() {
				message, errType, code = policyChatUpstreamErrorDetails(status)
			}
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

// enforceParallelToolCallsFalse preserves an explicit false value and adds it
// when tools are present so a capable terminal cannot fall back to parallel
// execution under a false public policy contract.
func enforceParallelToolCallsFalse(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	raw, hasPTC := m["parallel_tool_calls"]
	tools, hasTools := m["tools"]
	if !hasPTC && (!hasTools || !hasNonEmptyTools(tools)) {
		return body
	}
	if hasPTC && bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
		return body
	}
	m["parallel_tool_calls"] = json.RawMessage("false")
	result, err := json.Marshal(m)
	if err != nil {
		return body
	}
	return result
}

// omitParallelToolCalls removes the field for terminals that do not support
// it, including when a policy client explicitly supplied false.
func omitParallelToolCalls(body []byte) []byte {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return body
	}
	if _, ok := m["parallel_tool_calls"]; !ok {
		return body
	}
	delete(m, "parallel_tool_calls")
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

func (h *ProxyHandler) retryChatCompletionsWithoutInjectedStreamOptions(ctx context.Context, resp *http.Response, body []byte, mode chatCompletionsMode) (*http.Response, []byte, chatCompletionsMode) {
	return h.retryChatCompletionsWithoutInjectedStreamOptionsForModel(ctx, resp, body, mode, extractRequestModel(body))
}

func (h *ProxyHandler) retryChatCompletionsWithoutInjectedStreamOptionsForModel(ctx context.Context, resp *http.Response, body []byte, mode chatCompletionsMode, model string) (*http.Response, []byte, chatCompletionsMode) {
	retryResp, retryBody, retryMode, _ := h.retryChatCompletionsWithoutInjectedStreamOptionsForModelResult(ctx, resp, body, mode, model)
	return retryResp, retryBody, retryMode
}

func (h *ProxyHandler) retryChatCompletionsWithoutInjectedStreamOptionsForModelResult(ctx context.Context, resp *http.Response, body []byte, mode chatCompletionsMode, model string) (*http.Response, []byte, chatCompletionsMode, error) {
	if h == nil || resp == nil || resp.StatusCode != http.StatusBadRequest || !mode.injectedStreamUsage {
		return resp, body, mode, nil
	}
	fallbackBody, ok := stripStreamOptions(body)
	if !ok {
		return resp, body, mode, nil
	}
	originalResp := resp
	explicitOperation := routeOperationFromContext(ctx)
	if explicitOperation != nil {
		captured, cleanupDone := captureRouteResponse(resp)
		if !cleanupDone {
			return captured.response(), body, mode, nil
		}
		originalResp = captured.response()
	}
	retryCtx := withRouteAttemptKind(ctx, routeAttemptProtocolRecovery)
	retryResp, err := h.executeChatCompletionsRouteRequestForModel(retryCtx, fallbackBody, mode, model)
	if err != nil {
		if h != nil && h.log != nil {
			h.log.Debug("retry without stream_options failed", logger.Err(err))
		}
		return originalResp, body, mode, err
	}
	if explicitOperation == nil && resp.Body != nil {
		drainAndClose(resp.Body)
	}
	mode.injectedStreamUsage = false
	mode.injectedClientStreamUsage = false
	if h != nil && h.log != nil {
		h.log.Debug("retried chat completions without injected stream_options", logger.F("status", retryResp.StatusCode))
	}
	return retryResp, fallbackBody, mode, nil
}
