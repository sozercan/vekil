package proxy

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"strings"
)

func (h *ProxyHandler) applyDurableRequestStateBinding(store *stateBindingStore, operation *routeOperation, tokens []stateBindingToken) error {
	result := store.resolveForRoute(operation.route.public.routeID, operation.pinnedTarget(), tokens)
	if result.err != nil {
		return &providerRequestError{statusCode: http.StatusServiceUnavailable, err: result.err}
	}
	if result.outcome == stateBindingLookupUnknown && len(tokens) == 1 && tokens[0].stateType == stateBindingTypeConversationID {
		if target, ok := explicitConversationBootstrapTarget(operation.route); ok {
			if err := operation.forcePinnedTarget(target.id); err != nil {
				return &providerRequestError{statusCode: http.StatusBadRequest, err: errDurableStateIdentity}
			}
			// Defer the atomic claim until authentication for the actual request
			// has resolved. Config labels alone are not durable ownership proof.
			operation.mu.Lock()
			token := tokens[0]
			operation.bootstrapConversation = &token
			operation.mu.Unlock()
			h.RecordStateBindingMiss()
			return nil
		}
	}
	if result.outcome == stateBindingLookupKnown {
		targetID := store.ownerTarget(result.owner, operation.route)
		if targetID == "" {
			h.RecordStateBindingMiss()
			return &providerRequestError{statusCode: http.StatusBadRequest, err: errDurableStateIdentity}
		}
		if err := operation.forcePinnedTarget(targetID); err != nil {
			return &providerRequestError{statusCode: http.StatusBadRequest, err: errDurableStateIdentity}
		}
		operation.mu.Lock()
		defer operation.mu.Unlock()
		if operation.stateOwnerIdentity != [32]byte{} && operation.stateOwnerIdentity != result.owner.identity {
			return &providerRequestError{statusCode: http.StatusBadRequest, err: errDurableStateIdentity}
		}
		operation.stateOwnerIdentity = result.owner.identity
		h.RecordStateBindingHit()
		return nil
	}
	h.RecordStateBindingMiss()
	if result.outcome == stateBindingLookupUnknown {
		return &providerRequestError{statusCode: http.StatusBadRequest, code: "provider_state_unavailable", err: errDurableStateUnknown}
	}
	return &providerRequestError{statusCode: http.StatusBadRequest, err: errDurableStateConflict}
}

func (h *ProxyHandler) validateDurableRequestOwner(req *http.Request, route *modelRoute, target targetBinding, operation *routeOperation) ([32]byte, error) {
	if h.stateBindings == nil || h.stateBindings.durable == nil {
		return [32]byte{}, nil
	}
	d := h.stateBindings.durable
	d.mu.Lock()
	failed, closed := d.failed, d.db == nil
	d.mu.Unlock()
	if closed {
		failed = errDurableStateClosed
	}
	if failed != nil {
		return [32]byte{}, &providerRequestError{statusCode: http.StatusServiceUnavailable, err: failed}
	}
	identity, err := d.requestIdentity(req, route, target)
	if err != nil {
		return [32]byte{}, &providerRequestError{statusCode: http.StatusBadRequest, err: err}
	}
	operation.mu.Lock()
	required, conversation := operation.stateOwnerIdentity, operation.bootstrapConversation
	operation.mu.Unlock()
	if required != [32]byte{} && required != identity {
		return [32]byte{}, &providerRequestError{statusCode: http.StatusBadRequest, err: errDurableStateIdentity}
	}
	if conversation != nil {
		owner := stateBindingOwner{routeID: route.public.routeID, targetID: target.id, identity: identity}
		result := d.bind([]stateBindingToken{*conversation}, owner, true)
		if result.err != nil {
			return [32]byte{}, &providerRequestError{statusCode: http.StatusServiceUnavailable, err: result.err}
		}
		if result.outcome != stateBindingLookupKnown || result.owner != d.encodeOwner(owner) {
			return [32]byte{}, &providerRequestError{statusCode: http.StatusBadRequest, err: errDurableStateIdentity}
		}
		operation.mu.Lock()
		operation.stateOwnerIdentity = identity
		operation.mu.Unlock()
	}
	return identity, nil
}

// requestIdentity consumes the authenticated request, not a second credential
// lookup. Short-lived bearer refreshes must not change a stable issuer/account
// identity. Unsupported or malformed identity evidence fails before dispatch.
func (d *durableStateBindings) requestIdentity(req *http.Request, route *modelRoute, target targetBinding) ([32]byte, error) {
	if req == nil || req.URL == nil || target.provider == nil || route == nil {
		return [32]byte{}, errDurableStateIdentity
	}
	p := target.provider
	endpoint := *req.URL
	endpoint.Scheme = strings.ToLower(endpoint.Scheme)
	endpoint.Host = strings.ToLower(endpoint.Host)
	endpoint.Fragment = ""
	endpoint.RawQuery = endpoint.Query().Encode()
	// Native compact emits state for the same Responses namespace.
	if strings.HasSuffix(endpoint.Path, "/responses/compact") {
		endpoint.Path = strings.TrimSuffix(endpoint.Path, "/compact")
		endpoint.RawPath = ""
	}
	parts := []string{route.public.routeID, target.id, p.id, string(p.kind), endpoint.String(), target.upstreamModel}
	switch p.kind {
	case providerTypeCopilot:
		fingerprint, ok := req.Context().Value(copilotSourceFingerprintContextKey{}).([32]byte)
		if !ok || fingerprint == [32]byte{} {
			return [32]byte{}, errDurableStateIdentity
		}
		parts = append(parts, "copilot-source", string(fingerprint[:]), req.Header.Get("Copilot-Integration-ID"))
	case providerTypeOpenAICodex:
		claims, err := durableBearerClaims(req.Header.Get("Authorization"))
		if err != nil {
			return [32]byte{}, err
		}
		sub, issuer := claims["sub"], claims["iss"]
		if sub == "" || issuer == "" || req.Header.Get("ChatGPT-Account-ID") == "" {
			return [32]byte{}, errDurableStateIdentity
		}
		parts = append(parts, "codex-principal", issuer, sub)
	case providerTypeAzureOpenAI:
		parts = append(parts, "azure-auth", string(p.azureAuthMode()))
		if p.azureAuthMode() == providerAuthModeAzureIdentity {
			claims, err := durableBearerClaims(req.Header.Get("Authorization"))
			if err != nil || claims["iss"] == "" || claims["tid"] == "" || claims["oid"] == "" {
				return [32]byte{}, errDurableStateIdentity
			}
			parts = append(parts, "entra-principal", p.tokenScope, claims["iss"], claims["tid"], claims["oid"])
		} else {
			parts = append(parts, req.Header.Get("api-key"))
		}
	case providerTypeOpenAICompatible, providerTypeAnthropicCompatible:
		parts = append(parts, "generic-auth", string(p.authType), strings.ToLower(p.authHeader), req.Header.Get(p.authHeader))
	default:
		return [32]byte{}, errDurableStateIdentity
	}
	// Provider-owned routing/tenant headers are part of the scope. Do not
	// include client turn/session IDs or other per-request metadata.
	names := []string{"OpenAI-Organization", "OpenAI-Project", "ChatGPT-Account-ID", "X-OpenAI-Fedramp"}
	for name := range p.extraHeaders {
		rotatingBearer := p.kind == providerTypeCopilot || p.kind == providerTypeOpenAICodex || (p.kind == providerTypeAzureOpenAI && p.azureAuthMode() == providerAuthModeAzureIdentity)
		if rotatingBearer && strings.EqualFold(name, "Authorization") {
			continue
		}
		names = append(names, http.CanonicalHeaderKey(name))
	}
	sort.Strings(names)
	for _, name := range names {
		parts = append(parts, strings.ToLower(name))
		values := req.Header.Values(name)
		parts = append(parts, strconv.Itoa(len(values)))
		parts = append(parts, values...)
	}
	return d.digest("request-owner", parts...), nil
}

func durableBearerClaims(authorization string) (map[string]string, error) {
	if !strings.HasPrefix(authorization, "Bearer ") || len(authorization) > 64<<10 {
		return nil, errDurableStateIdentity
	}
	token := strings.TrimPrefix(authorization, "Bearer ")
	if len(strings.Split(token, ".")) != 3 {
		return nil, errDurableStateIdentity
	}
	payload, ok := decodeOpenAICodexJWTPayload(token)
	if !ok || rejectDuplicateJSONMappingKeys(payload) != nil {
		return nil, errDurableStateIdentity
	}
	var raw map[string]json.RawMessage
	if json.Unmarshal(payload, &raw) != nil {
		return nil, errDurableStateIdentity
	}
	claims := make(map[string]string)
	for _, name := range []string{"iss", "sub", "tid", "oid"} {
		if value, ok := raw[name]; ok {
			var text string
			if json.Unmarshal(value, &text) != nil || strings.TrimSpace(text) == "" {
				return nil, errDurableStateIdentity
			}
			claims[name] = text
		}
	}
	return claims, nil
}
