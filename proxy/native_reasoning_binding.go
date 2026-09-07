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

	"github.com/sozercan/vekil/models"
)

const (
	stateBindingTypeNativeReasoning  stateBindingType = "native_reasoning"
	nativeReasoningBindingMaxBytes                    = 2 << 20
	nativeReasoningBindingMaxChoices                  = 128
)

type nativeReasoningOwnerContextKey struct{}

// Canonical Anthropic translation omits native signatures until the provider
// is selected. Inspect the original history as well as raw Chat extensions so
// routing cannot move that state before it is restored to the upstream body.
func extractNativeReasoningRequestState(ctx context.Context, body []byte) ([]stateBindingToken, error) {
	var payload struct {
		Messages []struct {
			Role            string `json:"role"`
			ReasoningOpaque string `json:"reasoning_opaque"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("invalid native Chat reasoning history")
	}
	var tokens []stateBindingToken
	seen := make(map[string]struct{})
	add := func(value string) {
		if value == "" {
			return
		}
		if _, exists := seen[value]; exists {
			return
		}
		seen[value] = struct{}{}
		tokens = append(tokens, stateBindingToken{stateType: stateBindingTypeNativeReasoning, value: value})
	}
	for _, message := range payload.Messages {
		if message.Role == "assistant" {
			add(message.ReasoningOpaque)
		}
	}
	if original, _ := ctx.Value(anthropicChatExtensionsContextKey{}).(*models.AnthropicRequest); original != nil {
		for _, message := range original.Messages {
			if message.Role != "assistant" {
				continue
			}
			var blocks []models.ContentBlock
			if json.Unmarshal(message.Content, &blocks) != nil {
				continue
			}
			for _, block := range blocks {
				if (block.Type == "thinking" || block.Type == "redacted_thinking") && !strings.HasPrefix(block.Signature, reasoningCarrierPrefix) {
					add(block.Signature)
				}
			}
		}
	}
	return tokens, nil
}

func (h *ProxyHandler) applyNativeReasoningRequestBinding(ctx context.Context, operation *routeOperation, body []byte) (context.Context, error) {
	if operation == nil || operation.route == nil || operation.route.legacy {
		return ctx, nil
	}
	if owner, ok := ctx.Value(nativeReasoningOwnerContextKey{}).(stateBindingOwner); ok && owner.routeID == operation.route.public.routeID {
		return ctx, nil
	}
	tokens, err := extractNativeReasoningRequestState(ctx, body)
	if err != nil {
		return ctx, &providerRequestError{statusCode: http.StatusBadRequest, err: err}
	}
	if len(tokens) == 0 {
		return ctx, nil
	}
	store, err := h.ensureStateBindingStore()
	if err != nil {
		return ctx, err
	}
	result := store.resolveForRoute(operation.route.public.routeID, tokens)
	if result.outcome != stateBindingLookupKnown {
		h.RecordStateBindingMiss()
		message := "conflicting native reasoning state for explicit model route"
		if result.outcome == stateBindingLookupUnknown {
			message = "unknown native reasoning state for explicit model route; state may have expired, been evicted, or been issued by another Vekil process"
		}
		return ctx, &providerRequestError{statusCode: http.StatusBadRequest, err: errors.New(message)}
	}
	if _, ok := operation.route.targetByID(result.owner.targetID); !ok {
		h.RecordStateBindingMiss()
		return ctx, &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("native reasoning state is bound to an unavailable route target")}
	}
	if err := operation.forcePinnedTarget(result.owner.targetID); err != nil {
		h.RecordStateBindingMiss()
		return ctx, &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("native reasoning state conflicts with the selected route target")}
	}
	h.RecordStateBindingHit()
	return context.WithValue(ctx, nativeReasoningOwnerContextKey{}, result.owner), nil
}

// Use the source credential identity for Copilot so service-token refresh does
// not invalidate a continuation. Never retain bearer values in shared state.
func nativeReasoningRequestIdentity(req *http.Request, body []byte) [32]byte {
	if metadata, ok := req.Context().Value(copilotInferenceRequestContextKey{}).(copilotInferenceRequest); ok {
		account := metadata.keys[1].identity
		return copilotTrafficFingerprint(string(account[:]), extractRequestModel(body))
	}
	return copilotTrafficFingerprint(req.URL.Scheme, strings.ToLower(req.URL.Host), req.URL.EscapedPath(),
		req.Header.Get("Authorization"), req.Header.Get("Api-Key"), req.Header.Get("X-Api-Key"), extractRequestModel(body))
}

func validateNativeReasoningRequestOwner(ctx context.Context, info explicitRouteResponseInfo, provider *providerRuntime) error {
	owner, ok := ctx.Value(nativeReasoningOwnerContextKey{}).(stateBindingOwner)
	if !ok {
		return nil
	}
	if original, _ := ctx.Value(anthropicChatExtensionsContextKey{}).(*models.AnthropicRequest); original != nil && provider.kind != providerTypeCopilot {
		return &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("this native Chat provider cannot restore Anthropic reasoning signatures")}
	}
	if owner.routeID != info.routeID || owner.targetID != info.targetID || owner.nativeIdentity != info.nativeReasoningIdentity {
		return &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("native reasoning state no longer matches the issuing target, credential, or model")}
	}
	return nil
}

func (h *ProxyHandler) bindNativeReasoningTokens(info explicitRouteResponseInfo, tokens []stateBindingToken) error {
	if len(tokens) == 0 {
		return nil
	}
	store, err := h.ensureStateBindingStore()
	if err != nil {
		return err
	}
	owner := stateBindingOwner{routeID: info.routeID, targetID: info.targetID, nativeIdentity: info.nativeReasoningIdentity}
	result, evictions := store.bindAllWithEvictionDelta(tokens, owner)
	for count := uint64(0); count < evictions; count++ {
		h.RecordStateBindingEviction()
	}
	if result.outcome != stateBindingLookupKnown {
		return newChatServerError("native_reasoning_state_conflict", "upstream native reasoning state conflicts with another owner")
	}
	return nil
}

func (h *ProxyHandler) bindNativeReasoningCompletion(resp *http.Response, completion *models.OpenAIResponse) error {
	info, explicit := explicitRouteResponseInfoFromResponse(resp)
	if !explicit || completion == nil {
		return nil
	}
	var tokens []stateBindingToken
	for _, choice := range completion.Choices {
		if choice.Message.ReasoningOpaque != "" {
			tokens = append(tokens, stateBindingToken{stateType: stateBindingTypeNativeReasoning, value: choice.Message.ReasoningOpaque})
		}
	}
	return h.bindNativeReasoningTokens(info, tokens)
}

func (h *ProxyHandler) bindNativeReasoningJSONChoices(resp *http.Response, raw json.RawMessage) error {
	info, explicit := explicitRouteResponseInfoFromResponse(resp)
	if !explicit {
		return nil
	}
	var choices []struct {
		Message struct {
			ReasoningOpaque string `json:"reasoning_opaque"`
		} `json:"message"`
	}
	if json.Unmarshal(raw, &choices) != nil {
		return newChatServerError("invalid_native_reasoning", "upstream returned invalid native Chat reasoning state")
	}
	var tokens []stateBindingToken
	for _, choice := range choices {
		if choice.Message.ReasoningOpaque != "" {
			tokens = append(tokens, stateBindingToken{stateType: stateBindingTypeNativeReasoning, value: choice.Message.ReasoningOpaque})
		}
	}
	return h.bindNativeReasoningTokens(info, tokens)
}

// Observe complete blocks before Read returns their closing frame. A client
// can replay a closed thinking block while the original stream remains open.
// Only unfinished signatures and one bounded SSE event are retained here.
type nativeReasoningBindingBody struct {
	io.ReadCloser
	h           *ProxyHandler
	info        explicitRouteResponseInfo
	line        []byte
	eventBytes  int
	accumulator sseDataAccumulator
	signatures  map[int]*strings.Builder
	activeBytes int
	done        bool
	err         error
}

func (b *nativeReasoningBindingBody) Read(p []byte) (int, error) {
	if b.err != nil {
		return 0, b.err
	}
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.err = b.observe(p[:n])
	}
	if b.err == nil && errors.Is(err, io.EOF) && !b.done {
		if len(b.line) > 0 {
			b.consumeLine(string(b.line))
			b.line = nil
		}
		if b.err == nil {
			b.accumulator.dispatch(b.consumeData)
		}
	}
	if b.err != nil {
		return 0, b.err
	}
	return n, err
}

func (b *nativeReasoningBindingBody) observe(data []byte) error {
	for len(data) > 0 && !b.done {
		end := bytes.IndexByte(data, '\n')
		length := len(data)
		if end >= 0 {
			length = end + 1
		}
		if length > openAIStreamScannerMaxBuffer-b.eventBytes {
			return newChatServerError("native_reasoning_binding_limit", "native Chat event exceeds the reasoning binding limit")
		}
		b.eventBytes += length
		if end < 0 {
			b.line = append(b.line, data...)
			return nil
		}
		var line string
		if len(b.line) == 0 {
			line = string(data[:length])
		} else {
			b.line = append(b.line, data[:length]...)
			line = string(b.line)
			b.line = b.line[:0]
		}
		if !b.consumeLine(line) {
			return b.err
		}
		data = data[length:]
	}
	return nil
}

func (b *nativeReasoningBindingBody) consumeLine(line string) bool {
	if strings.TrimRight(line, "\r\n") == "" {
		b.eventBytes = 0
	}
	return b.accumulator.consumeLine(line, b.consumeData)
}

func (b *nativeReasoningBindingBody) consumeData(event, data string) bool {
	if strings.TrimSpace(data) == "[DONE]" {
		for index := range b.signatures {
			if !b.bindChoice(index, true) {
				return false
			}
		}
		b.done = true
		return true
	}
	if _, isError := parseOpenAIStreamError(event, data); isError {
		b.done = true
		b.signatures = nil
		return true
	}
	var chunk models.OpenAIStreamChunk
	if json.Unmarshal([]byte(data), &chunk) != nil {
		return true
	}
	for _, choice := range chunk.Choices {
		if signature := choice.Delta.ReasoningOpaque; signature != "" {
			if len(signature) > nativeReasoningBindingMaxBytes-b.activeBytes {
				b.err = newChatServerError("native_reasoning_binding_limit", "native reasoning signatures exceed the binding limit")
				return false
			}
			if b.signatures == nil {
				b.signatures = make(map[int]*strings.Builder)
			}
			builder := b.signatures[choice.Index]
			if builder == nil {
				if len(b.signatures) >= nativeReasoningBindingMaxChoices {
					b.err = newChatServerError("native_reasoning_binding_limit", "native reasoning choices exceed the binding limit")
					return false
				}
				builder = &strings.Builder{}
				b.signatures[choice.Index] = builder
			}
			builder.WriteString(signature)
			b.activeBytes += len(signature)
		}
		if nativeReasoningBlockBoundary(choice) {
			if !b.bindChoice(choice.Index, true) {
				return false
			}
		} else if choice.FinishReason != nil {
			// Raw Chat clients can finish here, but the Anthropic adapter keeps
			// its thinking block open. Retain the fragments so a late signature
			// delta also binds the complete value exposed by that adapter.
			if !b.bindChoice(choice.Index, false) {
				return false
			}
		}
	}
	return true
}

func nativeReasoningBlockBoundary(choice models.OpenAIStreamChoice) bool {
	if len(choice.Delta.ToolCalls) > 0 {
		return true
	}
	for _, raw := range []json.RawMessage{choice.Delta.Content, choice.Delta.Refusal} {
		if rawJSONIsNullOrEmpty(raw) {
			continue
		}
		var text string
		if json.Unmarshal(raw, &text) != nil || text != "" {
			return true
		}
	}
	return false
}

func (b *nativeReasoningBindingBody) bindChoice(index int, closeBlock bool) bool {
	builder := b.signatures[index]
	if builder == nil {
		return true
	}
	b.err = b.h.bindNativeReasoningTokens(b.info, []stateBindingToken{{stateType: stateBindingTypeNativeReasoning, value: builder.String()}})
	if closeBlock {
		b.activeBytes -= builder.Len()
		delete(b.signatures, index)
	}
	return b.err == nil
}

func (b *nativeReasoningBindingBody) canceledAtFailure() bool {
	if observed, ok := b.ReadCloser.(interface{ canceledAtFailure() bool }); ok {
		return observed.canceledAtFailure()
	}
	return false
}

func (b *nativeReasoningBindingBody) cancelRouteAttempt() {
	cancelRouteAttemptBody(b.ReadCloser)
}

func (b *nativeReasoningBindingBody) routeAttemptTransportOwnership() *routeAttemptTransportOwner {
	return routeAttemptTransportOwnership(b.ReadCloser)
}
