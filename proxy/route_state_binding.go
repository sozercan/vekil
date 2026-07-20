package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

const stateBindingTypeConversationID stateBindingType = "conversation_id"

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
	result := stateBindingLookupResult{outcome: stateBindingLookupUnknown}
	bootstrapped := false
	var evictions uint64
	if len(tokens) == 1 && tokens[0].stateType == stateBindingTypeConversationID {
		bootstrapOwner := stateBindingOwner{}
		if target, ok := explicitConversationBootstrapTarget(operation.route); ok {
			if pinned := operation.pinnedTarget(); pinned == "" || pinned == target.id {
				targetRevision := target.revision
				if targetRevision != "" {
					targetRevision = targetRevisionForBinding(operation.route.public, target)
				}
				bootstrapOwner = stateBindingOwner{routeID: operation.route.public.routeID, targetID: target.id, targetRevision: targetRevision}
			}
		}
		result, bootstrapped, evictions = store.resolveOrBindConversationForRoute(
			operation.route.public.routeID,
			tokens[0],
			bootstrapOwner,
		)
	} else {
		result = store.resolveForRoute(operation.route.public.routeID, tokens)
	}
	for count := uint64(0); count < evictions; count++ {
		h.RecordStateBindingEviction()
	}

	switch result.outcome {
	case stateBindingLookupKnown:
		target, ok := operation.route.targetByID(result.owner.targetID)
		if !ok || !stateBindingOwnerMatchesTarget(result.owner, operation.route, target) {
			h.RecordStateBindingMiss()
			return &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("provider state is bound to an unavailable route target")}
		}
		if err := operation.forcePinnedTargetRevision(result.owner.targetID, result.owner.targetRevision); err != nil {
			return &providerRequestError{statusCode: http.StatusBadRequest, err: err}
		}
		if bootstrapped {
			h.RecordStateBindingMiss()
		} else {
			h.RecordStateBindingHit()
		}
		return nil
	case stateBindingLookupUnknown:
		h.RecordStateBindingMiss()
		return &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("unknown provider-bound state for explicit model route")}
	default:
		h.RecordStateBindingMiss()
		return &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("conflicting provider-bound state for explicit model route")}
	}
}

// resolveOrBindConversationForRoute atomically resolves an existing conversation
// owner or claims an unknown conversation for bootstrapOwner. Competing callers
// therefore observe one exact owner instead of racing a separate lookup and bind.
func (s *stateBindingStore) resolveOrBindConversationForRoute(routeID string, token stateBindingToken, bootstrapOwner stateBindingOwner) (stateBindingLookupResult, bool, uint64) {
	if s == nil || token.stateType != stateBindingTypeConversationID || token.value == "" {
		return stateBindingLookupResult{outcome: stateBindingLookupConflict}, false, 0
	}

	key := s.bindingKey(token.stateType, token.value)
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	evictionsBefore := s.evictions
	finish := func(result stateBindingLookupResult, bootstrapped bool) (stateBindingLookupResult, bool, uint64) {
		return result, bootstrapped, s.evictions - evictionsBefore
	}

	result := s.lookupKeyLocked(key, now)
	switch result.outcome {
	case stateBindingLookupKnown:
		if routeID == "" || result.owner.routeID != routeID {
			return finish(stateBindingLookupResult{outcome: stateBindingLookupConflict}, false)
		}
		return finish(result, false)
	case stateBindingLookupConflict:
		return finish(result, false)
	case stateBindingLookupUnknown:
		if !bootstrapOwner.valid() || bootstrapOwner.routeID != routeID {
			return finish(result, false)
		}
	default:
		return finish(stateBindingLookupResult{outcome: stateBindingLookupConflict}, false)
	}

	if len(s.entries) >= s.maxEntries {
		s.pruneExpiredLocked(now)
	}
	for len(s.entries) >= s.maxEntries {
		s.removeLocked(s.recency.Back(), stateBindingRemovalCapacity)
	}
	record := &stateBindingRecord{
		key:       key,
		owner:     bootstrapOwner,
		outcome:   stateBindingLookupKnown,
		expiresAt: now.Add(s.ttl),
	}
	s.entries[key] = s.recency.PushFront(record)
	return finish(stateBindingLookupResult{outcome: stateBindingLookupKnown, owner: bootstrapOwner}, true)
}

func explicitConversationBootstrapTarget(route *modelRoute) (targetBinding, bool) {
	if route == nil {
		return targetBinding{}, false
	}

	// orderedRouteTargets applies the same provider endpoint eligibility and
	// primary_only boundary as dispatch. A priority_failover route is safe only
	// when that leaves one exact eligible target; primary_only is deterministic
	// because only its configured primary can be selected.
	targets := orderedRouteTargets(route, nil, providerEndpointResponses)
	if len(targets) != 1 {
		return targetBinding{}, false
	}
	return targets[0], true
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
		var hasPreviousResponseID bool
		if raw, exists := object["previous_response_id"]; exists {
			if raw != nil {
				value, ok := raw.(string)
				if !ok || strings.TrimSpace(value) == "" {
					return nil, fmt.Errorf("previous_response_id must be a non-empty string")
				}
				hasPreviousResponseID = true
				add(stateBindingTypeResponseID, value)
			}
		}

		conversationID, hasConversation, err := explicitResponsesRequestConversationID(object["conversation"])
		if err != nil {
			return nil, err
		}
		if hasPreviousResponseID && hasConversation {
			return nil, fmt.Errorf("previous_response_id cannot be used in conjunction with conversation")
		}
		if hasConversation {
			add(stateBindingTypeConversationID, conversationID)
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

func explicitResponsesRequestConversationID(value any) (string, bool, error) {
	if value == nil {
		return "", false, nil
	}

	var conversationID string
	switch conversation := value.(type) {
	case string:
		conversationID = conversation
	case map[string]any:
		rawID, exists := conversation["id"]
		if !exists {
			return "", false, fmt.Errorf("conversation must be a non-empty string or an object with a non-empty string id")
		}
		var ok bool
		conversationID, ok = rawID.(string)
		if !ok {
			return "", false, fmt.Errorf("conversation must be a non-empty string or an object with a non-empty string id")
		}
	default:
		return "", false, fmt.Errorf("conversation must be a non-empty string or an object with a non-empty string id")
	}

	conversationID = strings.TrimSpace(conversationID)
	if conversationID == "" {
		return "", false, fmt.Errorf("conversation must be a non-empty string or an object with a non-empty string id")
	}
	return conversationID, true, nil
}

func explicitResponsesOutputConversationID(value any) (string, bool) {
	conversation, ok := value.(map[string]any)
	if !ok {
		return "", false
	}
	conversationID, ok := conversation["id"].(string)
	if !ok {
		return "", false
	}
	conversationID = strings.TrimSpace(conversationID)
	return conversationID, conversationID != ""
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
	// A recognized prefix is not sufficient proof of proxy ownership. If the
	// checkpoint cannot be decoded, preserve it as provider state so explicit
	// routes fail closed instead of forwarding opaque content unbound.
	if !strings.HasPrefix(value, syntheticCompactionPrefix) &&
		!strings.HasPrefix(value, legacySyntheticCompactionPrefix) {
		return false
	}
	_, ok := extractSyntheticOrLegacyCompactionSummary(value)
	return ok
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
		return nil, fmt.Errorf("responses output root must be a JSON object")
	}
	visitItem := func(stateType stateBindingType, value string) {
		add(stateType, value)
	}
	visitResponse := func(response map[string]any) error {
		if token, ok := response["id"].(string); ok {
			add(stateBindingTypeResponseID, token)
		}
		conversationID, hasConversation := explicitResponsesOutputConversationID(response["conversation"])
		if hasConversation {
			add(stateBindingTypeConversationID, conversationID)
		}
		return visitExplicitResponsesItems(response["output"], false, visitItem)
	}

	// A non-streaming response is the root object. Streaming lifecycle events
	// put the response under response, while output-item events put the exposed
	// state-bearing artifact under item.
	eventType, _ := object["type"].(string)
	if strings.TrimSpace(eventType) == "" {
		if err := visitResponse(object); err != nil {
			return nil, err
		}
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
	targetRevision := info.targetRevision
	if targetRevision == "" {
		var found bool
		targetRevision, found = h.explicitRouteTargetRevision(info.routeID, info.targetID)
		if !found && h != nil && h.providersState != nil {
			return fmt.Errorf("provider state route target is unavailable")
		}
	}
	owner := stateBindingOwner{routeID: info.routeID, targetID: info.targetID, targetRevision: targetRevision}
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
