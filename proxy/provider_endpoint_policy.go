package proxy

// providerEndpointPolicy keeps provider-kind endpoint decisions in one module.
// Routing code should ask providerRuntime for endpoint behavior instead of
// duplicating provider-type switch statements at each call site.
type providerEndpointPolicy struct {
	defaultPaths                 providerEndpointPaths
	staticModelEndpoints         []string
	dynamicModelEndpoints        []string
	routedRequestEndpoints       []string
	unknownModelEndpoints        []string
	discoveredModelEndpoints     []string
	allowAnyRoutedRequest        bool
	allowAnyUnknownModel         bool
	allowAnyDiscoveredModelEntry bool
}

var (
	providerEndpointsResponsesOnly = []string{providerEndpointResponses}
	providerEndpointsChatOnly      = []string{providerEndpointChatCompletions}
	providerEndpointsMessagesOnly  = []string{providerEndpointMessages}
	providerEndpointsOpenAI        = []string{providerEndpointChatCompletions, providerEndpointResponses}
)

func providerEndpointPolicyFor(kind providerType) providerEndpointPolicy {
	paths := providerEndpointPaths{
		chatCompletions: providerEndpointChatCompletions,
		responses:       providerEndpointResponses,
		messages:        providerEndpointMessages,
		models:          providerEndpointModels,
	}

	switch kind {
	case providerTypeOpenAICodex:
		paths.chatCompletions = ""
		paths.messages = ""
		return providerEndpointPolicy{
			defaultPaths:             paths,
			staticModelEndpoints:     providerEndpointsResponsesOnly,
			dynamicModelEndpoints:    providerEndpointsResponsesOnly,
			routedRequestEndpoints:   providerEndpointsResponsesOnly,
			unknownModelEndpoints:    providerEndpointsResponsesOnly,
			discoveredModelEndpoints: providerEndpointsResponsesOnly,
		}
	case providerTypeOpenAICompatible:
		return providerEndpointPolicy{
			defaultPaths:                 paths,
			staticModelEndpoints:         providerEndpointsChatOnly,
			dynamicModelEndpoints:        providerEndpointsChatOnly,
			routedRequestEndpoints:       providerEndpointsOpenAI,
			unknownModelEndpoints:        providerEndpointsChatOnly,
			allowAnyDiscoveredModelEntry: true,
		}
	case providerTypeAnthropicCompatible:
		return providerEndpointPolicy{
			defaultPaths:             paths,
			staticModelEndpoints:     providerEndpointsMessagesOnly,
			dynamicModelEndpoints:    providerEndpointsMessagesOnly,
			routedRequestEndpoints:   providerEndpointsMessagesOnly,
			unknownModelEndpoints:    providerEndpointsMessagesOnly,
			discoveredModelEndpoints: providerEndpointsMessagesOnly,
		}
	case providerTypeCopilot:
		return providerEndpointPolicy{
			defaultPaths:                 paths,
			staticModelEndpoints:         providerEndpointsOpenAI,
			allowAnyRoutedRequest:        true,
			allowAnyUnknownModel:         true,
			allowAnyDiscoveredModelEntry: true,
		}
	case providerTypeAzureOpenAI:
		return providerEndpointPolicy{
			defaultPaths:                 paths,
			staticModelEndpoints:         providerEndpointsOpenAI,
			allowAnyRoutedRequest:        true,
			allowAnyUnknownModel:         true,
			allowAnyDiscoveredModelEntry: true,
		}
	default:
		return providerEndpointPolicy{
			defaultPaths:                 paths,
			staticModelEndpoints:         providerEndpointsOpenAI,
			allowAnyRoutedRequest:        true,
			allowAnyUnknownModel:         true,
			allowAnyDiscoveredModelEntry: true,
		}
	}
}

func endpointList(endpoints ...string) []string {
	return append([]string(nil), endpoints...)
}

func (p providerEndpointPolicy) defaultEndpointPaths() providerEndpointPaths {
	return p.defaultPaths
}

func (p providerEndpointPolicy) defaultStaticEndpoints() []string {
	return endpointList(p.staticModelEndpoints...)
}

func (p providerEndpointPolicy) defaultDynamicEndpoints() []string {
	return endpointList(p.dynamicModelEndpoints...)
}

func (p providerEndpointPolicy) supportsRoutedRequest(endpoint string) bool {
	if p.allowAnyRoutedRequest {
		return true
	}
	return supportsEndpoint(p.routedRequestEndpoints, endpoint)
}

func (p providerEndpointPolicy) allowsUnknownModelEndpoint(endpoint string) bool {
	if p.allowAnyUnknownModel {
		return true
	}
	return supportsEndpoint(p.unknownModelEndpoints, endpoint)
}

func (p providerEndpointPolicy) acceptsDiscoveredModelEndpoint(endpoint string) bool {
	if p.allowAnyDiscoveredModelEntry {
		return true
	}
	return supportsEndpoint(p.discoveredModelEndpoints, endpoint)
}

func (p *providerRuntime) endpointPolicy() providerEndpointPolicy {
	if p == nil {
		return providerEndpointPolicy{}
	}
	return providerEndpointPolicyFor(p.kind)
}

func (p *providerRuntime) supportsEndpoint(endpoint string) bool {
	if p == nil {
		return false
	}
	return p.endpointPolicy().supportsRoutedRequest(endpoint)
}

func (p *providerRuntime) allowsUnknownModelEndpoint(endpoint string) bool {
	if p == nil {
		return false
	}
	if !providerUsesDynamicModels(p) || providerHasModelFilters(p) {
		return false
	}
	return p.endpointPolicy().allowsUnknownModelEndpoint(endpoint)
}

func (p *providerRuntime) acceptsDiscoveredModelEndpoint(endpoint string) bool {
	if p == nil {
		return false
	}
	return p.endpointPolicy().acceptsDiscoveredModelEndpoint(endpoint)
}

func (p *providerRuntime) defaultStaticModelEndpoints() []string {
	return p.endpointPolicy().defaultStaticEndpoints()
}

func (p *providerRuntime) defaultDynamicModelEndpoints() []string {
	return p.endpointPolicy().defaultDynamicEndpoints()
}
