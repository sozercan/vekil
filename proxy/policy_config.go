package proxy

import (
	"fmt"
	"math"
	"strings"
)

const (
	maxPolicyProfiles = 128

	modelRouteExposurePublic   = "public"
	modelRouteExposureInternal = "internal"

	modelRouteInternalPurposePolicyClassifier = "policy_classifier"

	policyConfigModeOff     = "off"
	policyConfigModeObserve = "observe"
	policyConfigModeEnforce = "enforce"

	policyConfigTierLightweight = "lightweight"
	policyConfigTierPowerful    = "powerful"

	policyConfigClassifierProfileCodingAgentV1 = "coding_agent_v1"

	defaultPolicyClassifierTimeoutMS           = 3000
	defaultPolicyClassifierMaxCompletionTokens = 256
	defaultPolicyClassifierMaxRequestBytes     = 16000
	defaultPolicyClassifierRecentTurns         = 4
	defaultPolicyClassifierMaxConcurrency      = 4
	defaultPolicyClassifierObserveSampleRate   = 1.0
)

// PolicyProfileConfig declares one public semantic-routing policy. Mode and
// tier values intentionally remain strings here; runtime policy enums and
// planner state belong to the policy execution module.
type PolicyProfileConfig struct {
	ID                  string `json:"id" yaml:"id"`
	PublicID            string `json:"public_id" yaml:"public_id"`
	Name                string `json:"name,omitempty" yaml:"name,omitempty"`
	Mode                string `json:"mode,omitempty" yaml:"mode,omitempty"`
	ModelPickerEnabled  *bool  `json:"model_picker_enabled,omitempty" yaml:"model_picker_enabled,omitempty"`
	ModelPickerCategory string `json:"model_picker_category,omitempty" yaml:"model_picker_category,omitempty"`

	LightweightRoute string `json:"lightweight_route" yaml:"lightweight_route"`
	PowerfulRoute    string `json:"powerful_route" yaml:"powerful_route"`

	BaselineTier              string `json:"baseline_tier,omitempty" yaml:"baseline_tier,omitempty"`
	ClassifierUnavailableTier string `json:"classifier_unavailable_tier,omitempty" yaml:"classifier_unavailable_tier,omitempty"`
	ClassifierUncertainTier   string `json:"classifier_uncertain_tier,omitempty" yaml:"classifier_uncertain_tier,omitempty"`

	Classifier PolicyClassifierConfig `json:"classifier" yaml:"classifier"`
	DataPolicy PolicyDataPolicyConfig `json:"data_policy" yaml:"data_policy"`
}

// PolicyClassifierConfig bounds one profile's internal classifier operation.
type PolicyClassifierConfig struct {
	Route               string `json:"route" yaml:"route"`
	Profile             string `json:"profile,omitempty" yaml:"profile,omitempty"`
	TimeoutMS           int    `json:"timeout_ms,omitempty" yaml:"timeout_ms,omitempty"`
	MaxCompletionTokens int    `json:"max_completion_tokens,omitempty" yaml:"max_completion_tokens,omitempty"`
	// MaxRequestBytes caps the serialized canonical facts payload. The fixed
	// forced-tool Chat envelope has a separate implementation bound.
	MaxRequestBytes   int     `json:"max_request_bytes,omitempty" yaml:"max_request_bytes,omitempty"`
	RecentTurns       int     `json:"recent_turns,omitempty" yaml:"recent_turns,omitempty"`
	MaxConcurrency    int     `json:"max_concurrency,omitempty" yaml:"max_concurrency,omitempty"`
	ObserveSampleRate float64 `json:"observe_sample_rate,omitempty" yaml:"observe_sample_rate,omitempty"`

	timeoutMSSet           bool
	maxCompletionTokensSet bool
	maxRequestBytesSet     bool
	recentTurnsSet         bool
	maxConcurrencySet      bool
	observeSampleRateSet   bool

	timeoutMSNull           bool
	maxCompletionTokensNull bool
	maxRequestBytesNull     bool
	recentTurnsNull         bool
	maxConcurrencyNull      bool
	observeSampleRateNull   bool
}

// PolicyDataPolicyConfig records the operator acknowledgements required before
// bounded request content may be forwarded to the classifier provider.
type PolicyDataPolicyConfig struct {
	ContentForwardingAcknowledged bool `json:"content_forwarding_acknowledged" yaml:"content_forwarding_acknowledged"`
	AllowCrossTrustDomain         bool `json:"allow_cross_trust_domain,omitempty" yaml:"allow_cross_trust_domain,omitempty"`
	AllowProviderRetention        bool `json:"allow_provider_retention,omitempty" yaml:"allow_provider_retention,omitempty"`
}

func clonePolicyProfileConfig(profile PolicyProfileConfig) PolicyProfileConfig {
	cloned := profile
	cloned.ModelPickerEnabled = cloneBoolPtr(profile.ModelPickerEnabled)
	return cloned
}

func normalizeAndValidatePolicyProfileConfig(profile *PolicyProfileConfig, path string) error {
	if profile == nil {
		return configPathError(path, "is required")
	}

	var err error
	if profile.ID, err = normalizeOperationalID(profile.ID, path+".id"); err != nil {
		return err
	}
	if profile.PublicID = strings.TrimSpace(profile.PublicID); profile.PublicID == "" {
		return configPathError(path+".public_id", "is required")
	}
	if containsControlCharacter(profile.PublicID) {
		return configPathError(path+".public_id", "must not contain control characters")
	}
	if len(profile.PublicID) > policyStatsLabelMaxLen {
		return configPathError(path+".public_id", "must be at most %d bytes", policyStatsLabelMaxLen)
	}
	if profile.Name = strings.TrimSpace(profile.Name); profile.Name == "" {
		profile.Name = profile.PublicID
	}

	profile.Mode = strings.TrimSpace(profile.Mode)
	if profile.Mode == "" {
		profile.Mode = policyConfigModeOff
	}
	switch profile.Mode {
	case policyConfigModeOff, policyConfigModeObserve, policyConfigModeEnforce:
	default:
		return configPathError(path+".mode", "unsupported policy mode %q", profile.Mode)
	}

	if profile.ModelPickerEnabled == nil {
		enabled := true
		profile.ModelPickerEnabled = &enabled
	}
	if profile.ModelPickerCategory = strings.TrimSpace(profile.ModelPickerCategory); profile.ModelPickerCategory == "" {
		profile.ModelPickerCategory = "versatile"
	}

	if profile.LightweightRoute, err = normalizeOperationalID(profile.LightweightRoute, path+".lightweight_route"); err != nil {
		return err
	}
	if profile.PowerfulRoute, err = normalizeOperationalID(profile.PowerfulRoute, path+".powerful_route"); err != nil {
		return err
	}

	profile.BaselineTier = strings.TrimSpace(profile.BaselineTier)
	if profile.BaselineTier == "" {
		profile.BaselineTier = policyConfigTierLightweight
	}
	if err := validatePolicyConfigTier(profile.BaselineTier, path+".baseline_tier"); err != nil {
		return err
	}
	profile.ClassifierUnavailableTier = strings.TrimSpace(profile.ClassifierUnavailableTier)
	if profile.ClassifierUnavailableTier == "" {
		profile.ClassifierUnavailableTier = profile.BaselineTier
	}
	if err := validatePolicyConfigTier(profile.ClassifierUnavailableTier, path+".classifier_unavailable_tier"); err != nil {
		return err
	}
	profile.ClassifierUncertainTier = strings.TrimSpace(profile.ClassifierUncertainTier)
	if profile.ClassifierUncertainTier == "" {
		profile.ClassifierUncertainTier = policyConfigTierPowerful
	}
	if err := validatePolicyConfigTier(profile.ClassifierUncertainTier, path+".classifier_uncertain_tier"); err != nil {
		return err
	}

	classifierPath := path + ".classifier"
	if profile.Classifier.Route, err = normalizeOperationalID(profile.Classifier.Route, classifierPath+".route"); err != nil {
		return err
	}
	profile.Classifier.Profile = strings.TrimSpace(profile.Classifier.Profile)
	if profile.Classifier.Profile == "" {
		profile.Classifier.Profile = policyConfigClassifierProfileCodingAgentV1
	}
	if profile.Classifier.Profile != policyConfigClassifierProfileCodingAgentV1 {
		return configPathError(classifierPath+".profile", "unsupported classifier profile %q", profile.Classifier.Profile)
	}

	nullFields := []struct {
		name   string
		isNull bool
	}{
		{"timeout_ms", profile.Classifier.timeoutMSNull},
		{"max_completion_tokens", profile.Classifier.maxCompletionTokensNull},
		{"max_request_bytes", profile.Classifier.maxRequestBytesNull},
		{"recent_turns", profile.Classifier.recentTurnsNull},
		{"max_concurrency", profile.Classifier.maxConcurrencyNull},
		{"observe_sample_rate", profile.Classifier.observeSampleRateNull},
	}
	for _, field := range nullFields {
		if field.isNull {
			return configPathError(classifierPath+"."+field.name, "must not be null")
		}
	}

	if !profile.Classifier.timeoutMSSet && profile.Classifier.TimeoutMS == 0 {
		profile.Classifier.TimeoutMS = defaultPolicyClassifierTimeoutMS
	}
	if profile.Classifier.TimeoutMS < 100 || profile.Classifier.TimeoutMS > 10000 {
		return configPathError(classifierPath+".timeout_ms", "must be between 100 and 10000")
	}
	if !profile.Classifier.maxCompletionTokensSet && profile.Classifier.MaxCompletionTokens == 0 {
		profile.Classifier.MaxCompletionTokens = defaultPolicyClassifierMaxCompletionTokens
	}
	if profile.Classifier.MaxCompletionTokens < 32 || profile.Classifier.MaxCompletionTokens > 1024 {
		return configPathError(classifierPath+".max_completion_tokens", "must be between 32 and 1024")
	}
	if !profile.Classifier.maxRequestBytesSet && profile.Classifier.MaxRequestBytes == 0 {
		profile.Classifier.MaxRequestBytes = defaultPolicyClassifierMaxRequestBytes
	}
	if profile.Classifier.MaxRequestBytes < 1024 || profile.Classifier.MaxRequestBytes > 65536 {
		return configPathError(classifierPath+".max_request_bytes", "must be between 1024 and 65536")
	}
	if !profile.Classifier.recentTurnsSet && profile.Classifier.RecentTurns == 0 {
		profile.Classifier.RecentTurns = defaultPolicyClassifierRecentTurns
	}
	if profile.Classifier.RecentTurns < 0 || profile.Classifier.RecentTurns > 8 {
		return configPathError(classifierPath+".recent_turns", "must be between 0 and 8")
	}
	if !profile.Classifier.maxConcurrencySet && profile.Classifier.MaxConcurrency == 0 {
		profile.Classifier.MaxConcurrency = defaultPolicyClassifierMaxConcurrency
	}
	if profile.Classifier.MaxConcurrency < 1 || profile.Classifier.MaxConcurrency > 32 {
		return configPathError(classifierPath+".max_concurrency", "must be between 1 and 32")
	}
	if !profile.Classifier.observeSampleRateSet && profile.Classifier.ObserveSampleRate == 0 {
		profile.Classifier.ObserveSampleRate = defaultPolicyClassifierObserveSampleRate
	}
	if math.IsNaN(profile.Classifier.ObserveSampleRate) || math.IsInf(profile.Classifier.ObserveSampleRate, 0) || profile.Classifier.ObserveSampleRate < 0 || profile.Classifier.ObserveSampleRate > 1 {
		return configPathError(classifierPath+".observe_sample_rate", "must be a finite number between 0 and 1")
	}

	return nil
}

func validatePolicyConfigTier(tier, path string) error {
	switch tier {
	case policyConfigTierLightweight, policyConfigTierPowerful:
		return nil
	default:
		return configPathError(path, "unsupported policy tier %q", tier)
	}
}

func validatePolicyProfileConfigReferences(
	profile PolicyProfileConfig,
	profileIndex int,
	routes map[string]*ModelRouteConfig,
	providers map[string]providerConfigDescriptor,
	policyReferences map[string]string,
) error {
	path := fmt.Sprintf("policy_profiles[%d]", profileIndex)

	lightweight, err := resolvePolicyTerminalRoute(profile.LightweightRoute, path+".lightweight_route", routes, policyReferences)
	if err != nil {
		return err
	}
	powerful, err := resolvePolicyTerminalRoute(profile.PowerfulRoute, path+".powerful_route", routes, policyReferences)
	if err != nil {
		return err
	}
	classifier, err := resolvePolicyTerminalRoute(profile.Classifier.Route, path+".classifier.route", routes, policyReferences)
	if err != nil {
		return err
	}

	lightweightProviders, err := validatePolicyDestinationRoute(lightweight, path+".lightweight_route", providers)
	if err != nil {
		return err
	}
	powerfulProviders, err := validatePolicyDestinationRoute(powerful, path+".powerful_route", providers)
	if err != nil {
		return err
	}
	lightweightPrefersNativeChat := configRouteSupportsEndpoint(lightweight, providerEndpointChatCompletions)
	powerfulPrefersNativeChat := configRouteSupportsEndpoint(powerful, providerEndpointChatCompletions)
	if lightweightPrefersNativeChat != powerfulPrefersNativeChat {
		return configPathError(path+".powerful_route", "route %q differs from lightweight route %q in preferred Chat backend", profile.PowerfulRoute, profile.LightweightRoute)
	}
	if boolConfigValue(lightweight.DropSamplingParams) != boolConfigValue(powerful.DropSamplingParams) {
		return configPathError(path+".powerful_route", "route %q differs from lightweight route %q in drop_sampling_params public Chat semantics", profile.PowerfulRoute, profile.LightweightRoute)
	}
	if boolConfigValue(lightweight.DropStopSequences) != boolConfigValue(powerful.DropStopSequences) {
		return configPathError(path+".powerful_route", "route %q differs from lightweight route %q in drop_stop_sequences public Chat semantics", profile.PowerfulRoute, profile.LightweightRoute)
	}

	classifierProvider, err := validatePolicyClassifierRoute(classifier, path+".classifier.route", providers)
	if err != nil {
		return err
	}

	if !profile.DataPolicy.ContentForwardingAcknowledged {
		return configPathError(path+".data_policy.content_forwarding_acknowledged", "must be true because classifier facts include bounded request content")
	}
	if classifierProvider.classifierNoStoreSupported == nil || !*classifierProvider.classifierNoStoreSupported {
		if !profile.DataPolicy.AllowProviderRetention {
			return configPathError(path+".data_policy.allow_provider_retention", "must be true because classifier provider %q does not declare classifier_no_store_supported: true", classifierProvider.id)
		}
	}
	if !profile.DataPolicy.AllowCrossTrustDomain {
		for _, destination := range append(lightweightProviders, powerfulProviders...) {
			if destination.trustDomain == classifierProvider.trustDomain {
				continue
			}
			return configPathError(
				path+".data_policy.allow_cross_trust_domain",
				"must be true because classifier provider %q trust_domain %q differs from destination provider %q trust_domain %q",
				classifierProvider.id,
				classifierProvider.trustDomain,
				destination.id,
				destination.trustDomain,
			)
		}
	}

	return nil
}

func policyTerminalRouteReferences(profiles []PolicyProfileConfig) map[string]struct{} {
	references := make(map[string]struct{}, len(profiles)*3)
	for _, profile := range profiles {
		for _, routeID := range []string{
			profile.LightweightRoute,
			profile.PowerfulRoute,
			profile.Classifier.Route,
		} {
			routeID = strings.TrimSpace(routeID)
			if routeID != "" {
				references[routeID] = struct{}{}
			}
		}
	}
	return references
}

func resolvePolicyTerminalRoute(routeID, path string, routes map[string]*ModelRouteConfig, policyReferences map[string]string) (*ModelRouteConfig, error) {
	if route := routes[routeID]; route != nil {
		return route, nil
	}
	if referenced, exists := policyReferences[routeID]; exists {
		return nil, configPathError(path, "references policy profile %s; a terminal model route id is required", referenced)
	}
	return nil, configPathError(path, "references unknown model route %q", routeID)
}

func validatePolicyDestinationRoute(route *ModelRouteConfig, path string, providers map[string]providerConfigDescriptor) ([]providerConfigDescriptor, error) {
	if route == nil {
		return nil, configPathError(path, "references an unavailable model route")
	}
	if route.InternalPurpose != "" {
		return nil, configPathError(path, "route %q is reserved for internal purpose %q", route.ID, route.InternalPurpose)
	}
	if !configRouteSupportsEndpoint(route, providerEndpointChatCompletions) && !configRouteSupportsEndpoint(route, providerEndpointResponses) {
		return nil, configPathError(path, "route %q must expose %s or %s for Chat execution", route.ID, providerEndpointChatCompletions, providerEndpointResponses)
	}

	resolved := make([]providerConfigDescriptor, 0, len(route.Targets))
	for targetIndex, target := range route.Targets {
		descriptor, ok := providers[target.Provider]
		if !ok {
			return nil, configPathError(path, "route %q target %d references unknown provider %q", route.ID, targetIndex, target.Provider)
		}
		if descriptor.kind != providerTypeCopilot && descriptor.kind != providerTypeAzureOpenAI && descriptor.kind != providerTypeOpenAICompatible {
			return nil, configPathError(path, "route %q target provider %q does not use the supported OpenAI Chat execution family", route.ID, descriptor.id)
		}
		if descriptor.kind != providerTypeCopilot && descriptor.modelDiscovery != providerModelDiscoveryStatic {
			return nil, configPathError(path, "route %q target provider %q uses unsupported dynamic model_discovery %q", route.ID, descriptor.id, descriptor.modelDiscovery)
		}
		if descriptor.trustDomain == "" {
			return nil, configPathError(fmt.Sprintf("providers[%d].trust_domain", descriptor.index), "is required because provider %q is referenced by policy destination route %q", descriptor.id, route.ID)
		}
		resolved = append(resolved, descriptor)
	}
	return resolved, nil
}

func validatePolicyClassifierRoute(route *ModelRouteConfig, path string, providers map[string]providerConfigDescriptor) (providerConfigDescriptor, error) {
	if route == nil {
		return providerConfigDescriptor{}, configPathError(path, "references an unavailable model route")
	}
	if route.Exposure != modelRouteExposureInternal {
		return providerConfigDescriptor{}, configPathError(path, "route %q must set exposure: internal", route.ID)
	}
	if route.InternalPurpose != modelRouteInternalPurposePolicyClassifier {
		return providerConfigDescriptor{}, configPathError(path, "route %q must set internal_purpose: %s", route.ID, modelRouteInternalPurposePolicyClassifier)
	}
	if !configRouteSupportsEndpoint(route, providerEndpointChatCompletions) && !configRouteSupportsEndpoint(route, providerEndpointResponses) {
		return providerConfigDescriptor{}, configPathError(path, "route %q must expose %s or %s for forced function-tool classification", route.ID, providerEndpointChatCompletions, providerEndpointResponses)
	}
	if len(route.Targets) != 1 {
		return providerConfigDescriptor{}, configPathError(path, "route %q must contain exactly one target", route.ID)
	}
	if route.Routing.MaxTargetAttempts != 1 {
		return providerConfigDescriptor{}, configPathError(path, "route %q must set max_target_attempts to 1", route.ID)
	}
	if route.Routing.MaxUpstreamSends != 1 {
		return providerConfigDescriptor{}, configPathError(path, "route %q must set max_upstream_sends to 1", route.ID)
	}

	descriptor, ok := providers[route.Targets[0].Provider]
	if !ok {
		return providerConfigDescriptor{}, configPathError(path, "route %q references unknown provider %q", route.ID, route.Targets[0].Provider)
	}
	if descriptor.kind != providerTypeCopilot && descriptor.kind != providerTypeAzureOpenAI && descriptor.kind != providerTypeOpenAICompatible {
		return providerConfigDescriptor{}, configPathError(path, "classifier provider %q does not support forced function-tool classification", descriptor.id)
	}
	if descriptor.kind != providerTypeCopilot && descriptor.modelDiscovery != providerModelDiscoveryStatic {
		return providerConfigDescriptor{}, configPathError(path, "classifier provider %q uses unsupported dynamic model_discovery %q", descriptor.id, descriptor.modelDiscovery)
	}
	if descriptor.trustDomain == "" {
		return providerConfigDescriptor{}, configPathError(fmt.Sprintf("providers[%d].trust_domain", descriptor.index), "is required because provider %q is referenced by classifier route %q", descriptor.id, route.ID)
	}
	return descriptor, nil
}

func configRouteSupportsEndpoint(route *ModelRouteConfig, endpoint string) bool {
	if route == nil {
		return false
	}
	for _, candidate := range route.Endpoints {
		if candidate == endpoint {
			return true
		}
	}
	return false
}

func boolConfigValue(value *bool) bool {
	return value != nil && *value
}

// SetRecentTurns preserves an explicit zero for programmatic schema-v2
// configurations. JSON/YAML decoding tracks the same presence automatically.
func (c *PolicyClassifierConfig) SetRecentTurns(value int) {
	if c == nil {
		return
	}
	c.RecentTurns = value
	c.recentTurnsSet = true
}

// SetObserveSampleRate preserves an explicit zero for programmatic schema-v2
// configurations. JSON/YAML decoding tracks the same presence automatically.
func (c *PolicyClassifierConfig) SetObserveSampleRate(value float64) {
	if c == nil {
		return
	}
	c.ObserveSampleRate = value
	c.observeSampleRateSet = true
}
