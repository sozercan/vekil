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
			value, ok := raw.(string)
			if !ok || strings.TrimSpace(value) == "" {
				return nil, fmt.Errorf("previous_response_id must be a non-empty string")
			}
			add(stateBindingTypeResponseID, value)
		}
		if err := walkExplicitStateValues(object, func(stateType stateBindingType, value string) {
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

func walkExplicitStateValues(value any, visit func(stateBindingType, string)) error {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if key == "encrypted_content" {
				token, ok := child.(string)
				if !ok || strings.TrimSpace(token) == "" {
					return fmt.Errorf("encrypted_content must be a non-empty string")
				}
				visit(stateBindingTypeEncryptedContent, token)
				continue
			}
			if err := walkExplicitStateValues(child, visit); err != nil {
				return err
			}
		}
	case []any:
		for _, child := range typed {
			if err := walkExplicitStateValues(child, visit); err != nil {
				return err
			}
		}
	}
	return nil
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
	if object, ok := payload.(map[string]any); ok {
		if token, ok := object["id"].(string); ok {
			add(stateBindingTypeResponseID, token)
		}
		if response, ok := object["response"].(map[string]any); ok {
			if token, ok := response["id"].(string); ok {
				add(stateBindingTypeResponseID, token)
			}
		}
	}
	var walk func(any)
	walk = func(value any) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				switch key {
				case "encrypted_content":
					if token, ok := child.(string); ok {
						add(stateBindingTypeEncryptedContent, token)
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(payload)
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
	before := store.evictionCount()
	if result := store.bindAll(tokens, owner); result.outcome == stateBindingLookupConflict {
		return fmt.Errorf("provider state token collided with another route target")
	}
	after := store.evictionCount()
	for count := before; count < after; count++ {
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
