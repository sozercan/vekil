package proxy

import (
	"net/http"
	"strings"
	"sync"
)

// providerEndpointResponsesWebSocket is the Copilot catalog entry for native
// Responses websocket support. It is capability metadata only: Vekil never
// routes a public request to it.
const providerEndpointResponsesWebSocket = "ws:/responses"

// copilotNativeWebSocketCatalog records which upstream Copilot models advertise
// ws:/responses. It is keyed by provider ID and upstream model, and is filled
// from every fresh catalog fetch before public hiding or allowlist filtering,
// so explicit-route targets resolve their capability even though their
// upstream models are hidden from the public catalog.
type copilotNativeWebSocketCatalog struct {
	mu        sync.RWMutex
	providers map[string]map[string]bool
}

// record stores a fetched catalog. A complete catalog replaces the provider's
// entry; a partial one (such as a query-filtered fetch) only updates the models
// it lists.
func (c *copilotNativeWebSocketCatalog) record(providerID string, models []providerModel, complete bool) {
	providerID = strings.TrimSpace(providerID)
	if c == nil || providerID == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.providers == nil {
		c.providers = make(map[string]map[string]bool)
	}
	entries := c.providers[providerID]
	if complete || entries == nil {
		entries = make(map[string]bool, len(models))
	}
	for _, model := range models {
		// Require positive advertisement: an empty endpoint list means
		// "unspecified" elsewhere, never websocket support.
		advertised := model.endpointsAdvertised && supportsEndpoint(model.supportedEndpoints, providerEndpointResponsesWebSocket)
		for _, id := range []string{model.upstreamModel, model.publicID} {
			if id = strings.TrimSpace(id); id != "" {
				entries[id] = advertised
			}
		}
	}
	c.providers[providerID] = entries
}

// lookup reports whether model advertises ws:/responses. known is false when
// no catalog has been recorded for the provider yet.
func (c *copilotNativeWebSocketCatalog) lookup(providerID, model string) (advertised, known bool) {
	if c == nil {
		return false, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	entries, known := c.providers[strings.TrimSpace(providerID)]
	if !known {
		return false, false
	}
	return entries[strings.TrimSpace(model)], true
}

// Reasons a turn that could use the native connection is sent over HTTP.
const (
	responsesNativeDeclinedModelUnknown       = "model_unknown"
	responsesNativeDeclinedCatalogUnavailable = "catalog_unavailable"
	responsesNativeDeclinedNotAdvertised      = "model_not_advertised"
	responsesNativeDeclinedConnectionMismatch = "connection_mismatch"
	responsesNativeDeclinedInputTooLarge      = "input_too_large"
	responsesNativeDeclinedTurnHeaders        = "turn_headers"
	responsesNativeDeclinedDialUnavailable    = "dial_unavailable"
)

// responsesNativeModelEligible decides whether a not-yet-connected session may
// open a native upstream websocket for this physical request. It uses the
// actual upstream model in the encoded provider body, after any route rewrite.
// A cold catalog triggers one bounded, single-flight discovery refresh; when
// capability stays unknown the session keeps the HTTP bridge.
func (h *ProxyHandler) responsesNativeModelEligible(req *http.Request, providerID string) (bool, string) {
	model := responsesNativeRequestModel(req)
	if model == "" {
		return false, responsesNativeDeclinedModelUnknown
	}
	advertised, known := h.copilotNativeWS.lookup(providerID, model)
	if !known {
		if provider := h.providerSetup().providerByID(providerID); provider != nil && providerUsesDynamicModels(provider) {
			_ = h.refreshUnknownChatRouteProvider(req.Context(), provider)
			advertised, known = h.copilotNativeWS.lookup(providerID, model)
		}
	}
	switch {
	case !known:
		return false, responsesNativeDeclinedCatalogUnavailable
	case !advertised:
		return false, responsesNativeDeclinedNotAdvertised
	}
	return true, ""
}

// responsesNativeRequestModel returns the upstream model of a prepared Copilot
// inference request, after any route or fallback rewrite.
func responsesNativeRequestModel(req *http.Request) string {
	if req == nil {
		return ""
	}
	metadata, _ := req.Context().Value(copilotInferenceRequestContextKey{}).(copilotInferenceRequest)
	return metadata.model
}
