package proxy

import (
	"net/http"
	"strings"
	"sync"
)

const providerEndpointResponsesWebSocket = "ws:/responses"

type copilotNativeWebSocketCatalog struct {
	mu        sync.RWMutex
	providers map[string]map[string]bool
}

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
		advertised := model.endpointsAdvertised && supportsEndpoint(model.supportedEndpoints, providerEndpointResponsesWebSocket)
		for _, id := range []string{model.upstreamModel, model.publicID} {
			if id = strings.TrimSpace(id); id != "" {
				entries[id] = advertised
			}
		}
	}
	c.providers[providerID] = entries
}

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

const (
	responsesNativeDeclinedModelUnknown       = "model_unknown"
	responsesNativeDeclinedCatalogUnavailable = "catalog_unavailable"
	responsesNativeDeclinedNotAdvertised      = "model_not_advertised"
	responsesNativeDeclinedConnectionMismatch = "connection_mismatch"
	responsesNativeDeclinedInputTooLarge      = "input_too_large"
	responsesNativeDeclinedTurnHeaders        = "turn_headers"
	responsesNativeDeclinedDialUnavailable    = "dial_unavailable"
)

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

func responsesNativeRequestModel(req *http.Request) string {
	if req == nil {
		return ""
	}
	metadata, _ := req.Context().Value(copilotInferenceRequestContextKey{}).(copilotInferenceRequest)
	return metadata.model
}
