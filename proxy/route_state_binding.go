package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

func (h *ProxyHandler) ensureStateBindingStore() (*stateBindingStore, error) {
	if h == nil {
		return nil, fmt.Errorf("proxy handler is required")
	}
	h.stateBindingsOnce.Do(func() {
		h.stateBindings, h.stateBindingsErr = newStateBindingStore(stateBindingStoreConfig{})
	})
	return h.stateBindings, h.stateBindingsErr
}

func (h *ProxyHandler) applyExplicitRequestStateBinding(operation *routeOperation, body []byte, headers http.Header) error {
	if operation == nil || operation.route == nil || operation.route.legacy {
		return nil
	}
	tokens, err := extractExplicitResponsesRequestState(body, headers)
	if err != nil {
		return &providerRequestError{statusCode: http.StatusBadRequest, err: err}
	}
	if len(tokens) == 0 {
		return nil
	}
	store, err := h.ensureStateBindingStore()
	if err != nil {
		return err
	}
	result := store.resolveForRoute(operation.route.public.routeID, tokens)
	switch result.outcome {
	case stateBindingLookupKnown:
		if _, ok := operation.route.targetByID(result.owner.targetID); !ok {
			h.RecordStateBindingMiss()
			return &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("provider state is bound to an unavailable route target")}
		}
		if err := operation.forcePinnedTarget(result.owner.targetID); err != nil {
			return &providerRequestError{statusCode: http.StatusBadRequest, err: err}
		}
		h.RecordStateBindingHit()
		return nil
	case stateBindingLookupUnknown:
		h.RecordStateBindingMiss()
		return &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("unknown provider-bound state for explicit model route")}
	default:
		h.RecordStateBindingMiss()
		return &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("conflicting provider-bound state for explicit model route")}
	}
}

func extractExplicitResponsesRequestState(body []byte, headers http.Header) ([]stateBindingToken, error) {
	if err := rejectDuplicateJSONMappingKeys(body); err != nil {
		return nil, fmt.Errorf("invalid ambiguous JSON request: %w", err)
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, fmt.Errorf("invalid JSON in request body: %w", err)
	}

	tokens := make([]stateBindingToken, 0, 4)
	seen := make(map[string]struct{}, 4)
	add := func(stateType stateBindingType, value string) {
		value = strings.TrimSpace(value)
		if value == "" {
			return
		}
		key := string(stateType) + "\x00" + value
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		tokens = append(tokens, stateBindingToken{stateType: stateType, value: value})
	}

	if object, ok := payload.(map[string]any); ok {
		if raw, exists := object["previous_response_id"]; exists {
			if raw != nil {
				value, ok := raw.(string)
				if !ok || strings.TrimSpace(value) == "" {
					return nil, fmt.Errorf("previous_response_id must be a non-empty string")
				}
				add(stateBindingTypeResponseID, value)
			}
		}
		if err := visitExplicitResponsesItems(object["input"], true, func(stateType stateBindingType, value string) {
			if stateType == stateBindingTypeEncryptedContent && isProxyOwnedEncryptedContent(value) {
				return
			}
			add(stateType, value)
		}); err != nil {
			return nil, err
		}
	}

	var turnState string
	for _, value := range headers.Values("X-Codex-Turn-State") {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if turnState != "" && turnState != value {
			return nil, fmt.Errorf("X-Codex-Turn-State contains differing repeated values")
		}
		turnState = value
	}
	if turnState != "" {
		add(stateBindingTypeTurnState, turnState)
	}
	return tokens, nil
}

func visitExplicitResponsesItems(value any, rejectMalformed bool, visit func(stateBindingType, string)) error {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	for _, item := range items {
		if err := visitExplicitResponsesItem(item, rejectMalformed, visit); err != nil {
			return err
		}
	}
	return nil
}

// visitExplicitResponsesItem inspects only reasoning and compaction item
// shapes, which own opaque continuation state. A same-named field on another
// item or in request, response, item, or content metadata is ordinary user data
// and must not pin a provider target.
func visitExplicitResponsesItem(value any, rejectMalformed bool, visit func(stateBindingType, string)) error {
	item, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	if !explicitResponsesItemOwnsEncryptedContent(item) {
		return nil
	}
	raw, exists := item["encrypted_content"]
	if !exists {
		return nil
	}
	token, ok := raw.(string)
	if !ok || strings.TrimSpace(token) == "" {
		if rejectMalformed {
			return fmt.Errorf("encrypted_content must be a non-empty string")
		}
		return nil
	}
	visit(stateBindingTypeEncryptedContent, token)
	return nil
}

func explicitResponsesItemOwnsEncryptedContent(item map[string]any) bool {
	itemType, _ := item["type"].(string)
	switch strings.TrimSpace(itemType) {
	case "reasoning", "compaction", "context_compaction":
		return true
	default:
		return false
	}
}

func isProxyOwnedEncryptedContent(value string) bool {
	return strings.HasPrefix(value, syntheticCompactionPrefix) ||
		strings.HasPrefix(value, legacySyntheticCompactionPrefix)
}

func extractExplicitResponsesOutputState(body []byte) ([]stateBindingToken, error) {
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	tokens := make([]stateBindingToken, 0, 4)
	seen := make(map[string]struct{}, 4)
	add := func(stateType stateBindingType, value string) {
		value = strings.TrimSpace(value)
		if value == "" || (stateType == stateBindingTypeEncryptedContent && isProxyOwnedEncryptedContent(value)) {
			return
		}
		key := string(stateType) + "\x00" + value
		if _, ok := seen[key]; ok {
			return
		}
		seen[key] = struct{}{}
		tokens = append(tokens, stateBindingToken{stateType: stateType, value: value})
	}

	object, ok := payload.(map[string]any)
	if !ok {
		return tokens, nil
	}
	visitItem := func(stateType stateBindingType, value string) {
		add(stateType, value)
	}
	visitResponse := func(response map[string]any) error {
		if token, ok := response["id"].(string); ok {
			add(stateBindingTypeResponseID, token)
		}
		return visitExplicitResponsesItems(response["output"], false, visitItem)
	}

	// A non-streaming response is the root object. Streaming lifecycle events
	// put the response under response, while output-item events put the exposed
	// state-bearing artifact under item.
	if err := visitResponse(object); err != nil {
		return nil, err
	}
	if response, ok := object["response"].(map[string]any); ok {
		if err := visitResponse(response); err != nil {
			return nil, err
		}
	}
	if item, exists := object["item"]; exists {
		if err := visitExplicitResponsesItem(item, false, visitItem); err != nil {
			return nil, err
		}
	}
	return tokens, nil
}

func (h *ProxyHandler) bindExplicitStateTokens(info explicitRouteResponseInfo, tokens []stateBindingToken) error {
	if len(tokens) == 0 {
		return nil
	}
	store, err := h.ensureStateBindingStore()
	if err != nil {
		return err
	}
	owner := stateBindingOwner{routeID: info.routeID, targetID: info.targetID}
	result, evictions := store.bindAllWithEvictionDelta(tokens, owner)
	if result.outcome == stateBindingLookupConflict {
		return fmt.Errorf("provider state token collided with another route target")
	}
	for count := uint64(0); count < evictions; count++ {
		h.RecordStateBindingEviction()
	}
	return nil
}

func explicitResponseHeaderStateTokens(headers http.Header) ([]stateBindingToken, error) {
	if headers == nil {
		return nil, nil
	}
	values := headers.Values("X-Codex-Turn-State")
	if len(values) == 0 {
		return nil, nil
	}
	var token string
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if token != "" && token != value {
			return nil, fmt.Errorf("upstream returned conflicting X-Codex-Turn-State values")
		}
		token = value
	}
	if token == "" {
		return nil, nil
	}
	return []stateBindingToken{{stateType: stateBindingTypeTurnState, value: token}}, nil
}

func (h *ProxyHandler) bindExplicitResponseHeaders(info explicitRouteResponseInfo, headers http.Header) error {
	tokens, err := explicitResponseHeaderStateTokens(headers)
	if err != nil {
		return err
	}
	return h.bindExplicitStateTokens(info, tokens)
}
