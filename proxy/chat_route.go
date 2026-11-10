package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

type chatBackend uint8

const (
	chatBackendNativeChat chatBackend = iota + 1
	chatBackendResponses
)

// resolvedChatRoute is the immutable provider/model/backend snapshot used for
// one canonical Chat request. The provider runtime is configuration state and
// the owner is copied so later catalog replacement cannot change this route.
type resolvedChatRoute struct {
	provider       *providerRuntime
	owner          providerModel
	known          bool
	publicModel    string
	upstreamModel  string
	nativeEndpoint string
	backend        chatBackend
}

func chooseChatRoute(provider *providerRuntime, owner providerModel, known bool, model string) (resolvedChatRoute, error) {
	return chooseChatRouteWithOwner(provider, owner, known, model, false)
}

func chooseChatRouteFromSnapshot(provider *providerRuntime, owner providerModel, known bool, model string) (resolvedChatRoute, error) {
	return chooseChatRouteWithOwner(provider, owner, known, model, true)
}

func chooseChatRouteWithOwner(provider *providerRuntime, owner providerModel, known bool, model string, ownerSnapshot bool) (resolvedChatRoute, error) {
	model = strings.TrimSpace(model)
	if provider == nil {
		return resolvedChatRoute{}, &providerRequestError{
			statusCode: http.StatusInternalServerError,
			err:        fmt.Errorf("no provider available for endpoint %s", providerEndpointChatCompletions),
		}
	}

	if chatRouteAllowsEndpoint(provider, owner, known, providerEndpointChatCompletions) {
		return newResolvedChatRouteWithOwner(provider, owner, known, model, providerEndpointChatCompletions, chatBackendNativeChat, ownerSnapshot), nil
	}
	if chatRouteAllowsEndpoint(provider, owner, known, providerEndpointResponses) {
		return newResolvedChatRouteWithOwner(provider, owner, known, model, providerEndpointResponses, chatBackendResponses, ownerSnapshot), nil
	}

	return resolvedChatRoute{}, unsupportedChatRouteError(provider, model)
}

func chatRouteAllowsEndpoint(provider *providerRuntime, owner providerModel, known bool, endpoint string) bool {
	if provider == nil || !provider.supportsEndpoint(endpoint) {
		return false
	}
	if known {
		return providerModelSupportsEndpoint(owner, endpoint)
	}
	return provider.allowsUnknownModelEndpoint(endpoint)
}

func newResolvedChatRoute(provider *providerRuntime, owner providerModel, known bool, publicModel, endpoint string, backend chatBackend) resolvedChatRoute {
	return newResolvedChatRouteWithOwner(provider, owner, known, publicModel, endpoint, backend, false)
}

func newResolvedChatRouteWithOwner(provider *providerRuntime, owner providerModel, known bool, publicModel, endpoint string, backend chatBackend, ownerSnapshot bool) resolvedChatRoute {
	if !ownerSnapshot {
		owner = cloneProviderModelForRoute(owner)
	}
	upstreamModel := strings.TrimSpace(owner.upstreamModel)
	if upstreamModel == "" {
		upstreamModel = strings.TrimSpace(publicModel)
	}
	return resolvedChatRoute{
		provider:       provider,
		owner:          owner,
		known:          known,
		publicModel:    strings.TrimSpace(publicModel),
		upstreamModel:  upstreamModel,
		nativeEndpoint: endpoint,
		backend:        backend,
	}
}

func cloneProviderModelForRoute(model providerModel) providerModel {
	model.supportedEndpoints = append([]string(nil), model.supportedEndpoints...)
	model.parallelToolCalls = cloneBoolPtr(model.parallelToolCalls)
	if len(model.upstreamModelJSON) == 0 {
		model.upstreamModelJSON = encodeProviderModelJSON(model.upstreamModel)
	} else {
		model.upstreamModelJSON = append(json.RawMessage(nil), model.upstreamModelJSON...)
	}
	model.raw = append([]byte(nil), model.raw...)
	return model
}

func unsupportedChatRouteError(provider *providerRuntime, model string) error {
	if provider == nil {
		return &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("no provider available for endpoint %s", providerEndpointChatCompletions)}
	}
	if !provider.supportsEndpoint(providerEndpointChatCompletions) && !provider.supportsEndpoint(providerEndpointResponses) {
		return &providerRequestError{
			statusCode: http.StatusBadRequest,
			err:        fmt.Errorf("provider %q does not support %s", provider.id, providerEndpointChatCompletions),
		}
	}
	return &providerRequestError{
		statusCode: http.StatusBadRequest,
		err:        fmt.Errorf("model %q does not support %s", strings.TrimSpace(model), providerEndpointChatCompletions),
	}
}
