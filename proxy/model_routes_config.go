package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	ProvidersConfigSchemaVersion1 = 1
	ProvidersConfigSchemaVersion2 = 2

	maxExplicitModelRoutes          = 256
	maxExplicitTargetsPerRoute      = 32
	maxExplicitModelRouteTargets    = 1024
	maxModelRouteOperationalIDBytes = 128
)

// ModelRouteConfig maps one public model contract to an ordered set of
// semantically equivalent provider targets.
type ModelRouteConfig struct {
	ID                  string                   `json:"id" yaml:"id"`
	Exposure            string                   `json:"exposure,omitempty" yaml:"exposure,omitempty"`
	InternalPurpose     string                   `json:"internal_purpose,omitempty" yaml:"internal_purpose,omitempty"`
	PublicID            string                   `json:"public_id,omitempty" yaml:"public_id,omitempty"`
	Name                string                   `json:"name,omitempty" yaml:"name,omitempty"`
	Endpoints           []string                 `json:"endpoints" yaml:"endpoints"`
	ReasoningEffort     []string                 `json:"reasoning_effort,omitempty" yaml:"reasoning_effort,omitempty"`
	ParallelToolCalls   *bool                    `json:"parallel_tool_calls,omitempty" yaml:"parallel_tool_calls,omitempty"`
	Vision              *bool                    `json:"vision,omitempty" yaml:"vision,omitempty"`
	ContextWindow       *int64                   `json:"context_window,omitempty" yaml:"context_window,omitempty"`
	ModelPickerEnabled  *bool                    `json:"model_picker_enabled,omitempty" yaml:"model_picker_enabled,omitempty"`
	ModelPickerCategory string                   `json:"model_picker_category,omitempty" yaml:"model_picker_category,omitempty"`
	DropSamplingParams  *bool                    `json:"drop_sampling_params,omitempty" yaml:"drop_sampling_params,omitempty"`
	DropStopSequences   *bool                    `json:"drop_stop_sequences,omitempty" yaml:"drop_stop_sequences,omitempty"`
	Targets             []ModelRouteTargetConfig `json:"targets" yaml:"targets"`
	Routing             ModelRouteRoutingConfig  `json:"routing,omitempty" yaml:"routing,omitempty"`

	exposureSet            bool
	internalPurposeSet     bool
	publicIDSet            bool
	modelPickerEnabledSet  bool
	modelPickerCategorySet bool
}

// ModelRouteTargetConfig binds one ordered route target to a provider and its
// physical upstream model or deployment name.
type ModelRouteTargetConfig struct {
	ID                     string `json:"id" yaml:"id"`
	Provider               string `json:"provider" yaml:"provider"`
	UpstreamModel          string `json:"upstream_model" yaml:"upstream_model"`
	UseMaxCompletionTokens *bool  `json:"use_max_completion_tokens,omitempty" yaml:"use_max_completion_tokens,omitempty"`
}

// ModelRouteRoutingConfig bounds target selection and physical inference sends
// for one logical operation.
type ModelRouteRoutingConfig struct {
	Mode              string `json:"mode,omitempty" yaml:"mode,omitempty"`
	MaxTargetAttempts int    `json:"max_target_attempts,omitempty" yaml:"max_target_attempts,omitempty"`
	MaxUpstreamSends  int    `json:"max_upstream_sends,omitempty" yaml:"max_upstream_sends,omitempty"`

	modeSet              bool
	maxTargetAttemptsSet bool
	maxUpstreamSendsSet  bool
}

func (c *ModelRouteRoutingConfig) UnmarshalJSON(data []byte) error {
	type routingFields struct {
		Mode              string `json:"mode"`
		MaxTargetAttempts int    `json:"max_target_attempts"`
		MaxUpstreamSends  int    `json:"max_upstream_sends"`
	}
	var decoded routingFields
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var present map[string]json.RawMessage
	if err := json.Unmarshal(data, &present); err != nil {
		return err
	}
	*c = ModelRouteRoutingConfig{
		Mode:                 decoded.Mode,
		MaxTargetAttempts:    decoded.MaxTargetAttempts,
		MaxUpstreamSends:     decoded.MaxUpstreamSends,
		modeSet:              present["mode"] != nil,
		maxTargetAttemptsSet: present["max_target_attempts"] != nil,
		maxUpstreamSendsSet:  present["max_upstream_sends"] != nil,
	}
	return nil
}

func (c *ModelRouteRoutingConfig) UnmarshalYAML(node *yaml.Node) error {
	type routingFields struct {
		Mode              string `yaml:"mode"`
		MaxTargetAttempts int    `yaml:"max_target_attempts"`
		MaxUpstreamSends  int    `yaml:"max_upstream_sends"`
	}
	if node == nil {
		return nil
	}
	present := make(map[string]bool)
	if node.Kind == yaml.MappingNode {
		for index := 0; index+1 < len(node.Content); index += 2 {
			key := node.Content[index].Value
			switch key {
			case "mode", "max_target_attempts", "max_upstream_sends":
				present[key] = true
			default:
				return fmt.Errorf("field %s not found in type proxy.ModelRouteRoutingConfig", key)
			}
		}
	}
	var decoded routingFields
	if err := node.Decode(&decoded); err != nil {
		return err
	}
	*c = ModelRouteRoutingConfig{
		Mode:                 decoded.Mode,
		MaxTargetAttempts:    decoded.MaxTargetAttempts,
		MaxUpstreamSends:     decoded.MaxUpstreamSends,
		modeSet:              present["mode"],
		maxTargetAttemptsSet: present["max_target_attempts"],
		maxUpstreamSendsSet:  present["max_upstream_sends"],
	}
	return nil
}

type validatedProvidersConfig struct {
	config                   ProvidersConfig
	schemaVersion            int
	routeReferencedProviders map[string]struct{}
	defaultProviderOptional  bool
}

type providerConfigDescriptor struct {
	index                      int
	id                         string
	kind                       providerType
	modelDiscovery             providerModelDiscovery
	models                     []ProviderModelConfig
	legacyCatalog              bool
	trustDomain                string
	classifierNoStoreSupported *bool
	modelFilter                normalizedProviderModelFilter
}

// EffectiveSchemaVersion returns the public configuration version. Omitting
// schema_version intentionally retains version-1 provider-only semantics.
func (c ProvidersConfig) EffectiveSchemaVersion() int {
	if c.SchemaVersion == 0 {
		return ProvidersConfigSchemaVersion1
	}
	return c.SchemaVersion
}

// ValidateProvidersConfig validates a decoded provider configuration without
// contacting provider model or inference endpoints.
func ValidateProvidersConfig(cfg ProvidersConfig) error {
	validated, err := validateAndNormalizeProvidersConfig(cfg)
	if err != nil {
		return err
	}
	if len(validated.config.Providers) == 0 {
		return nil
	}

	h := &ProxyHandler{copilotURL: "https://api.githubcopilot.com"}
	_, err = h.buildConfiguredProviderSetupWithDynamicValidation(context.Background(), validated.config, false)
	return err
}

// ValidateProvidersConfigFile strictly decodes and validates a provider
// configuration without starting the server or contacting upstream endpoints.
func ValidateProvidersConfigFile(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return configPathError("providers_config", "path is required")
	}
	cfg, err := LoadProvidersConfigFile(path)
	if err != nil {
		return err
	}
	if err := ValidateProvidersConfig(cfg); err != nil {
		return fmt.Errorf("validate providers config %q: %w", path, err)
	}
	return nil
}

func providersConfigSchemaUsesStrictDecoding(version int) bool {
	if version == 0 {
		version = ProvidersConfigSchemaVersion1
	}
	return version >= ProvidersConfigSchemaVersion2
}

func providersConfigSchemaSupportsPolicyRouting(version int) bool {
	return version == ProvidersConfigSchemaVersion2
}

func validateAndNormalizeProvidersConfig(cfg ProvidersConfig) (validatedProvidersConfig, error) {
	validated := validatedProvidersConfig{
		config:                   cloneProvidersConfigForValidation(cfg),
		schemaVersion:            cfg.EffectiveSchemaVersion(),
		routeReferencedProviders: make(map[string]struct{}),
	}

	if cfg.schemaVersionSet && cfg.SchemaVersion == 0 {
		return validatedProvidersConfig{}, configPathError("schema_version", "unsupported schema version 0; supported versions are 1 and 2")
	}

	switch validated.schemaVersion {
	case ProvidersConfigSchemaVersion1:
		if cfg.modelRoutesSet || cfg.ModelRoutes != nil {
			return validatedProvidersConfig{}, configPathError("model_routes", "requires schema_version: 2")
		}
		if err := validateSchemaV2FeatureFields(cfg, validated.schemaVersion); err != nil {
			return validatedProvidersConfig{}, err
		}
		return validated, nil
	case ProvidersConfigSchemaVersion2:
	case 0:
		// EffectiveSchemaVersion maps zero to version 1, so this is unreachable.
		return validatedProvidersConfig{}, configPathError("schema_version", "must be 1 or 2")
	default:
		return validatedProvidersConfig{}, configPathError("schema_version", "unsupported schema version %d; supported versions are 1 and 2", cfg.SchemaVersion)
	}

	if err := validateSchemaV2FeatureFields(cfg, validated.schemaVersion); err != nil {
		return validatedProvidersConfig{}, err
	}
	if len(validated.config.ModelRoutes) > maxExplicitModelRoutes {
		return validatedProvidersConfig{}, configPathError("model_routes", "contains %d routes; maximum is %d", len(validated.config.ModelRoutes), maxExplicitModelRoutes)
	}
	if len(validated.config.PolicyProfiles) > maxPolicyProfiles {
		return validatedProvidersConfig{}, configPathError("policy_profiles", "contains %d profiles; maximum is %d", len(validated.config.PolicyProfiles), maxPolicyProfiles)
	}

	if providersConfigSchemaSupportsPolicyRouting(validated.schemaVersion) {
		for providerIndex := range validated.config.Providers {
			provider := &validated.config.Providers[providerIndex]
			provider.TrustDomain = strings.TrimSpace(provider.TrustDomain)
			if containsControlCharacter(provider.TrustDomain) {
				return validatedProvidersConfig{}, configPathError(fmt.Sprintf("providers[%d].trust_domain", providerIndex), "must not contain control characters")
			}
			provider.ClassifierNoStoreSupported = cloneBoolPtr(provider.ClassifierNoStoreSupported)
		}
	}

	providers, providerOrder, defaultProviderOptional, err := validateProviderConfigDescriptors(
		validated.config.Providers,
		providersConfigSchemaSupportsPolicyRouting(validated.schemaVersion),
	)
	if err != nil {
		return validatedProvidersConfig{}, err
	}
	validated.defaultProviderOptional = defaultProviderOptional

	legacyPublicIDs := make(map[string]string)
	for _, descriptor := range providerOrder {
		modelFilter := newNormalizedProviderModelFilter(validated.config.Providers[descriptor.index])
		for modelIndex, model := range descriptor.models {
			publicID := strings.TrimSpace(model.PublicID)
			path := fmt.Sprintf("providers[%d].models[%d].public_id", descriptor.index, modelIndex)
			if publicID == "" {
				return validatedProvidersConfig{}, configPathError(path, "is required")
			}
			if !modelFilter.allows(publicID) {
				continue
			}
			for _, alias := range configuredPublicModelAliases(publicID) {
				if prior, exists := legacyPublicIDs[alias]; exists {
					return validatedProvidersConfig{}, configPathError(path, "collides with %s after model normalization as %q", prior, alias)
				}
				legacyPublicIDs[alias] = path
			}
		}
	}

	routeIDs := make(map[string]string, len(validated.config.ModelRoutes))
	routeConfigs := make(map[string]*ModelRouteConfig, len(validated.config.ModelRoutes))
	publicIDs := make(map[string]string, len(validated.config.ModelRoutes)*2+len(validated.config.PolicyProfiles)*2)
	totalTargets := 0
	for routeIndex := range validated.config.ModelRoutes {
		routePath := fmt.Sprintf("model_routes[%d]", routeIndex)
		route := &validated.config.ModelRoutes[routeIndex]
		if err := normalizeAndValidateModelRouteForSchema(route, routePath, validated.schemaVersion); err != nil {
			return validatedProvidersConfig{}, err
		}

		if prior, exists := routeIDs[route.ID]; exists {
			return validatedProvidersConfig{}, configPathError(routePath+".id", "duplicates %s", prior)
		}
		routeIDs[route.ID] = routePath + ".id"
		routeConfigs[route.ID] = route

		if modelRouteConfigIsPublic(route, validated.schemaVersion) {
			for _, alias := range configuredPublicModelAliases(route.PublicID) {
				if prior, exists := publicIDs[alias]; exists {
					return validatedProvidersConfig{}, configPathError(routePath+".public_id", "collides with %s after model normalization as %q", prior, alias)
				}
				if prior, exists := legacyPublicIDs[alias]; exists {
					return validatedProvidersConfig{}, configPathError(routePath+".public_id", "collides with %s after model normalization as %q", prior, alias)
				}
				publicIDs[alias] = routePath + ".public_id"
			}
		}

		if descriptor, exists := providers[route.ID]; exists && descriptor.legacyCatalog {
			return validatedProvidersConfig{}, configPathError(routePath+".id", "collides with legacy provider id declared at providers[%d].id", descriptor.index)
		}

		totalTargets += len(route.Targets)
		if totalTargets > maxExplicitModelRouteTargets {
			return validatedProvidersConfig{}, configPathError("model_routes", "contains more than %d total targets", maxExplicitModelRouteTargets)
		}

		var routeFamily string
		for targetIndex := range route.Targets {
			targetPath := fmt.Sprintf("%s.targets[%d]", routePath, targetIndex)
			target := &route.Targets[targetIndex]
			descriptor, exists := providers[target.Provider]
			if !exists {
				return validatedProvidersConfig{}, configPathError(targetPath+".provider", "references unknown provider %q", target.Provider)
			}
			if descriptor.kind == providerTypeCopilot {
				if !descriptor.modelFilter.allows(target.UpstreamModel) {
					return validatedProvidersConfig{}, configPathError(targetPath+".upstream_model", "model %q is excluded by provider %q include_models/exclude_models filters", target.UpstreamModel, descriptor.id)
				}
			}
			if !providerKindSupportsExplicitRoutes(descriptor.kind) {
				return validatedProvidersConfig{}, configPathError(targetPath+".provider", "provider %q has unsupported explicit-route type %q", descriptor.id, descriptor.kind)
			}
			if (descriptor.kind == providerTypeOpenAICompatible || descriptor.kind == providerTypeAnthropicCompatible) && descriptor.modelDiscovery != providerModelDiscoveryStatic {
				return validatedProvidersConfig{}, configPathError(targetPath+".provider", "provider %q uses dynamic model_discovery %q; explicit route targets must be static", descriptor.id, descriptor.modelDiscovery)
			}

			family := explicitRouteProviderFamily(descriptor.kind)
			if routeFamily == "" {
				routeFamily = family
			} else if routeFamily != family {
				return validatedProvidersConfig{}, configPathError(targetPath+".provider", "provider %q is not adapter-compatible with earlier targets in this route", descriptor.id)
			}
			for endpointIndex, endpoint := range route.Endpoints {
				if !providerKindSupportsExplicitEndpoint(descriptor.kind, endpoint) {
					return validatedProvidersConfig{}, configPathError(
						fmt.Sprintf("%s.endpoints[%d]", routePath, endpointIndex),
						"endpoint %q is not supported by target provider %q of type %q",
						endpoint,
						descriptor.id,
						descriptor.kind,
					)
				}
			}
			validated.routeReferencedProviders[descriptor.id] = struct{}{}
		}
	}

	for _, descriptor := range providerOrder {
		if !providerConfigRequiresStaticModels(descriptor) || len(descriptor.models) > 0 {
			continue
		}
		if _, referenced := validated.routeReferencedProviders[descriptor.id]; referenced {
			continue
		}
		return validatedProvidersConfig{}, configPathError(
			fmt.Sprintf("providers[%d].models", descriptor.index),
			"must configure at least one model because provider %q is not referenced by model_routes",
			descriptor.id,
		)
	}

	policyReferences := make(map[string]string, len(validated.config.PolicyProfiles)*2)
	policyIDs := make(map[string]string, len(validated.config.PolicyProfiles))
	for profileIndex := range validated.config.PolicyProfiles {
		profilePath := fmt.Sprintf("policy_profiles[%d]", profileIndex)
		profile := &validated.config.PolicyProfiles[profileIndex]
		if err := normalizeAndValidatePolicyProfileConfig(profile, profilePath); err != nil {
			return validatedProvidersConfig{}, err
		}
		if prior, exists := policyIDs[profile.ID]; exists {
			return validatedProvidersConfig{}, configPathError(profilePath+".id", "duplicates %s", prior)
		}
		policyIDs[profile.ID] = profilePath + ".id"
		policyReferences[profile.ID] = profilePath + ".id"
		policyReferences[profile.PublicID] = profilePath + ".public_id"

		for _, alias := range configuredPublicModelAliases(profile.PublicID) {
			if prior, exists := publicIDs[alias]; exists {
				return validatedProvidersConfig{}, configPathError(profilePath+".public_id", "collides with %s after model normalization as %q", prior, alias)
			}
			if prior, exists := legacyPublicIDs[alias]; exists {
				return validatedProvidersConfig{}, configPathError(profilePath+".public_id", "collides with %s after model normalization as %q", prior, alias)
			}
			publicIDs[alias] = profilePath + ".public_id"
		}
	}

	preflightContracts := make(map[string]PolicyClassifierConfig, len(validated.config.PolicyProfiles))
	preflightOwners := make(map[string]int, len(validated.config.PolicyProfiles))
	for profileIndex, profile := range validated.config.PolicyProfiles {
		if err := validatePolicyProfileConfigReferences(profile, profileIndex, routeConfigs, providers, policyReferences); err != nil {
			return validatedProvidersConfig{}, err
		}
		if previous, exists := preflightContracts[profile.Classifier.Route]; exists {
			if previous.TimeoutMS != profile.Classifier.TimeoutMS || previous.MaxCompletionTokens != profile.Classifier.MaxCompletionTokens {
				owner := preflightOwners[profile.Classifier.Route]
				return validatedProvidersConfig{}, configPathError(
					fmt.Sprintf("policy_profiles[%d].classifier", profileIndex),
					"must use the same timeout_ms and max_completion_tokens as policy_profiles[%d].classifier when sharing classifier route %q",
					owner,
					profile.Classifier.Route,
				)
			}
		} else {
			preflightContracts[profile.Classifier.Route] = profile.Classifier
			preflightOwners[profile.Classifier.Route] = profileIndex
		}
	}

	if insightModel := strings.TrimSpace(validated.config.InsightModel); insightModel != "" {
		// Insight models resolve only through the public model namespace.
		// Operational route/profile IDs are intentionally separate and may equal
		// an unrelated direct public model ID.
		for profileIndex, profile := range validated.config.PolicyProfiles {
			for _, alias := range configuredPublicModelAliases(profile.PublicID) {
				if insightModel == alias {
					return validatedProvidersConfig{}, configPathError("insight_model", "cannot reference policy profile declared at policy_profiles[%d].public_id", profileIndex)
				}
			}
		}
	}

	return validated, nil
}

func validateSchemaV2FeatureFields(cfg ProvidersConfig, schemaVersion int) error {
	if providersConfigSchemaSupportsPolicyRouting(schemaVersion) {
		return nil
	}
	if cfg.policyProfilesSet || cfg.PolicyProfiles != nil {
		return configPathError("policy_profiles", "requires schema_version: 2")
	}
	for providerIndex, provider := range cfg.Providers {
		if provider.trustDomainSet || strings.TrimSpace(provider.TrustDomain) != "" {
			return configPathError(fmt.Sprintf("providers[%d].trust_domain", providerIndex), "requires schema_version: 2")
		}
		if provider.classifierNoStoreSupportedSet || provider.ClassifierNoStoreSupported != nil {
			return configPathError(fmt.Sprintf("providers[%d].classifier_no_store_supported", providerIndex), "requires schema_version: 2")
		}
	}
	for routeIndex, route := range cfg.ModelRoutes {
		if route.exposureSet || strings.TrimSpace(route.Exposure) != "" {
			return configPathError(fmt.Sprintf("model_routes[%d].exposure", routeIndex), "requires schema_version: 2")
		}
		if route.internalPurposeSet || strings.TrimSpace(route.InternalPurpose) != "" {
			return configPathError(fmt.Sprintf("model_routes[%d].internal_purpose", routeIndex), "requires schema_version: 2")
		}
	}
	return nil
}

func cloneProvidersConfigForValidation(cfg ProvidersConfig) ProvidersConfig {
	cloned := cfg
	if cfg.Providers != nil {
		cloned.Providers = make([]ProviderConfig, len(cfg.Providers))
		for index := range cfg.Providers {
			provider := cfg.Providers[index]
			provider.IncludeModels = append([]string(nil), cfg.Providers[index].IncludeModels...)
			provider.ExcludeModels = append([]string(nil), cfg.Providers[index].ExcludeModels...)
			provider.ClassifierNoStoreSupported = cloneBoolPtr(cfg.Providers[index].ClassifierNoStoreSupported)
			if cfg.Providers[index].ExtraHeaders != nil {
				provider.ExtraHeaders = make(map[string]string, len(cfg.Providers[index].ExtraHeaders))
				for key, value := range cfg.Providers[index].ExtraHeaders {
					provider.ExtraHeaders[key] = value
				}
			}
			if cfg.Providers[index].Models != nil {
				provider.Models = make([]ProviderModelConfig, len(cfg.Providers[index].Models))
				for modelIndex := range cfg.Providers[index].Models {
					model := cfg.Providers[index].Models[modelIndex]
					model.Endpoints = append([]string(nil), model.Endpoints...)
					model.ReasoningEffort = append([]string(nil), model.ReasoningEffort...)
					model.ModelPickerEnabled = cloneBoolPtr(model.ModelPickerEnabled)
					model.Vision = cloneBoolPtr(model.Vision)
					model.ParallelToolCalls = cloneBoolPtr(model.ParallelToolCalls)
					model.DropSamplingParams = cloneBoolPtr(model.DropSamplingParams)
					model.DropStopSequences = cloneBoolPtr(model.DropStopSequences)
					model.UseMaxCompletionTokens = cloneBoolPtr(model.UseMaxCompletionTokens)
					if model.ContextWindow != nil {
						value := *model.ContextWindow
						model.ContextWindow = &value
					}
					provider.Models[modelIndex] = model
				}
			}
			cloned.Providers[index] = provider
		}
	}
	if cfg.ModelRoutes != nil {
		cloned.ModelRoutes = make([]ModelRouteConfig, len(cfg.ModelRoutes))
		for i := range cfg.ModelRoutes {
			cloned.ModelRoutes[i] = cloneModelRouteConfig(cfg.ModelRoutes[i])
		}
	}
	if cfg.PolicyProfiles != nil {
		cloned.PolicyProfiles = make([]PolicyProfileConfig, len(cfg.PolicyProfiles))
		for index := range cfg.PolicyProfiles {
			cloned.PolicyProfiles[index] = clonePolicyProfileConfig(cfg.PolicyProfiles[index])
		}
	}
	return cloned
}

func cloneModelRouteConfig(route ModelRouteConfig) ModelRouteConfig {
	cloned := route
	cloned.Endpoints = append([]string(nil), route.Endpoints...)
	cloned.ReasoningEffort = append([]string(nil), route.ReasoningEffort...)
	cloned.Targets = append([]ModelRouteTargetConfig(nil), route.Targets...)
	cloned.ParallelToolCalls = cloneBoolPtr(route.ParallelToolCalls)
	cloned.Vision = cloneBoolPtr(route.Vision)
	cloned.ModelPickerEnabled = cloneBoolPtr(route.ModelPickerEnabled)
	cloned.DropSamplingParams = cloneBoolPtr(route.DropSamplingParams)
	cloned.DropStopSequences = cloneBoolPtr(route.DropStopSequences)
	if route.ContextWindow != nil {
		value := *route.ContextWindow
		cloned.ContextWindow = &value
	}
	for i := range cloned.Targets {
		cloned.Targets[i].UseMaxCompletionTokens = cloneBoolPtr(route.Targets[i].UseMaxCompletionTokens)
	}
	return cloned
}

func validateProviderConfigDescriptors(configured []ProviderConfig, allowRouteOnlyWithoutDefault bool) (map[string]providerConfigDescriptor, []providerConfigDescriptor, bool, error) {
	providers := make(map[string]providerConfigDescriptor, len(configured))
	ordered := make([]providerConfigDescriptor, 0, len(configured))
	defaultIndex := -1
	copilotCount := 0

	for index, provider := range configured {
		path := fmt.Sprintf("providers[%d]", index)
		id, err := normalizeOperationalID(provider.ID, path+".id")
		if err != nil {
			return nil, nil, false, err
		}
		if prior, exists := providers[id]; exists {
			return nil, nil, false, configPathError(path+".id", "duplicates providers[%d].id", prior.index)
		}

		kind := providerType(strings.TrimSpace(provider.Type))
		switch kind {
		case providerTypeCopilot, providerTypeAzureOpenAI, providerTypeOpenAICodex, providerTypeOpenAICompatible, providerTypeAnthropicCompatible:
		default:
			return nil, nil, false, configPathError(path+".type", "unsupported provider type %q", provider.Type)
		}

		discovery, err := configuredProviderModelDiscovery(kind, provider.ModelDiscovery)
		if err != nil {
			return nil, nil, false, configPathError(path+".model_discovery", "%v", err)
		}
		if err := validateProviderShellWithoutSecrets(provider, kind, path); err != nil {
			return nil, nil, false, err
		}

		descriptor := providerConfigDescriptor{
			index:                      index,
			id:                         id,
			kind:                       kind,
			modelDiscovery:             discovery,
			models:                     provider.Models,
			trustDomain:                strings.TrimSpace(provider.TrustDomain),
			classifierNoStoreSupported: cloneBoolPtr(provider.ClassifierNoStoreSupported),
			modelFilter:                newNormalizedProviderModelFilter(provider),
		}
		descriptor.legacyCatalog = providerConfigHasLegacyCatalog(descriptor, provider)
		providers[id] = descriptor
		ordered = append(ordered, descriptor)

		if kind == providerTypeCopilot {
			copilotCount++
			if copilotCount > 1 {
				return nil, nil, false, configPathError(path+".type", "multiple copilot providers are not supported")
			}
		}
		if provider.Default {
			if defaultIndex >= 0 {
				return nil, nil, false, configPathError(path+".default", "multiple default providers configured; providers[%d].default is already true", defaultIndex)
			}
			defaultIndex = index
		}
	}

	defaultOptional := false
	if len(configured) > 1 && defaultIndex < 0 && copilotCount != 1 {
		defaultOptional = allowRouteOnlyWithoutDefault
		if defaultOptional {
			for _, descriptor := range ordered {
				if descriptor.legacyCatalog {
					defaultOptional = false
					break
				}
			}
		}
		if !defaultOptional {
			return nil, nil, false, configPathError("providers", "multiple providers are configured but no default provider is selected")
		}
	}
	return providers, ordered, defaultOptional, nil
}

func validateProviderRuntimeEnvironment(cfg ProviderConfig, providerIndex int) error {
	if strings.TrimSpace(cfg.APIKey) != "" {
		return nil
	}
	envName := strings.TrimSpace(cfg.APIKeyEnv)
	if envName == "" {
		return nil
	}
	if strings.TrimSpace(os.Getenv(envName)) == "" {
		return configPathError(
			fmt.Sprintf("providers[%d].api_key_env", providerIndex),
			"environment variable %q is not set or is empty",
			envName,
		)
	}
	return nil
}

func validateProviderShellWithoutSecrets(cfg ProviderConfig, kind providerType, path string) error {
	switch kind {
	case providerTypeAzureOpenAI:
		baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
		if baseURL == "" {
			return configPathError(path+".base_url", "is required")
		}
		baseKind := classifyAzureBaseURL(baseURL)
		switch baseKind {
		case azureBaseURLKindOpenAIV1, azureBaseURLKindLegacyOpenAI, azureBaseURLKindResourceRoot:
		case azureBaseURLKindModels:
			return configPathError(path+".base_url", "Microsoft Foundry /models inference endpoints are not supported; use an OpenAI-compatible endpoint ending in /openai/v1")
		default:
			return configPathError(path+".base_url", "must be an absolute Azure OpenAI URL ending in /openai/v1 or /openai, or an Azure OpenAI resource root, with no query string or fragment")
		}
		if strings.TrimSpace(cfg.APIVersion) == "" && baseKind != azureBaseURLKindOpenAIV1 {
			return configPathError(path+".api_version", "is required unless base_url ends in /openai/v1")
		}
		authMode := providerAuthMode(strings.TrimSpace(cfg.AuthMode))
		if authMode == "" {
			authMode = providerAuthModeAPIKey
		}
		switch authMode {
		case providerAuthModeAPIKey:
			if strings.TrimSpace(cfg.TokenScope) != "" {
				return configPathError(path+".token_scope", "is only valid with auth_mode %q", providerAuthModeAzureIdentity)
			}
			if strings.TrimSpace(cfg.APIKey) == "" && strings.TrimSpace(cfg.APIKeyEnv) == "" {
				return configPathError(path+".api_key", "or %s.api_key_env is required", path)
			}
		case providerAuthModeAzureIdentity:
			if strings.TrimSpace(cfg.APIKey) != "" {
				return configPathError(path+".api_key", "cannot be combined with auth_mode %q", providerAuthModeAzureIdentity)
			}
			if strings.TrimSpace(cfg.APIKeyEnv) != "" {
				return configPathError(path+".api_key_env", "cannot be combined with auth_mode %q", providerAuthModeAzureIdentity)
			}
		default:
			return configPathError(path+".auth_mode", "unsupported auth_mode %q", cfg.AuthMode)
		}
	case providerTypeOpenAICodex:
		baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
		if baseURL != "" {
			if err := validateGenericProviderBaseURL(strings.TrimSpace(cfg.ID), "OpenAI Codex", baseURL); err != nil {
				return configPathError(path+".base_url", "%v", err)
			}
		}
	case providerTypeOpenAICompatible, providerTypeAnthropicCompatible:
		baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
		if baseURL == "" {
			return configPathError(path+".base_url", "is required")
		}
		if err := validateGenericProviderBaseURL(strings.TrimSpace(cfg.ID), string(kind), baseURL); err != nil {
			return configPathError(path+".base_url", "%v", err)
		}
		if _, err := configuredProviderEndpointPaths(kind, cfg); err != nil {
			field := providerPathValidationField(err)
			return configPathError(path+"."+field, "%v", err)
		}
		if err := validateGenericProviderAuthWithoutEnvironment(cfg); err != nil {
			return configPathError(path+"."+err.path, "%s", err.message)
		}
		if _, err := configuredProviderExtraHeaders(cfg.ExtraHeaders); err != nil {
			return configPathError(path+".extra_headers", "%v", err)
		}
	}

	for modelIndex, model := range cfg.Models {
		modelPath := fmt.Sprintf("%s.models[%d]", path, modelIndex)
		if strings.TrimSpace(model.PublicID) == "" {
			return configPathError(modelPath+".public_id", "is required")
		}
		supportsChatCompletions := len(model.Endpoints) == 0 && supportsEndpoint(
			providerEndpointPolicyFor(kind).defaultStaticEndpoints(),
			providerEndpointChatCompletions,
		)
		seenEndpoints := make(map[string]int, len(model.Endpoints))
		for endpointIndex, rawEndpoint := range model.Endpoints {
			endpoint := strings.TrimSpace(rawEndpoint)
			endpointPath := fmt.Sprintf("%s.endpoints[%d]", modelPath, endpointIndex)
			if endpoint == "" || !knownProviderEndpoint(endpoint) {
				return configPathError(endpointPath, "unsupported endpoint %q", rawEndpoint)
			}
			if prior, exists := seenEndpoints[endpoint]; exists {
				return configPathError(endpointPath, "duplicates %s.endpoints[%d]", modelPath, prior)
			}
			seenEndpoints[endpoint] = endpointIndex
			if endpoint == providerEndpointChatCompletions {
				supportsChatCompletions = true
			}
		}
		if model.DropStopSequences != nil && !supportsChatCompletions {
			return configPathError(modelPath+".drop_stop_sequences", "is only valid when the model supports %q", providerEndpointChatCompletions)
		}
		seenReasoning := make(map[string]int, len(model.ReasoningEffort))
		for effortIndex, rawEffort := range model.ReasoningEffort {
			effort := strings.TrimSpace(rawEffort)
			effortPath := fmt.Sprintf("%s.reasoning_effort[%d]", modelPath, effortIndex)
			if effort == "" {
				return configPathError(effortPath, "must not be empty")
			}
			if prior, exists := seenReasoning[effort]; exists {
				return configPathError(effortPath, "duplicates %s.reasoning_effort[%d]", modelPath, prior)
			}
			seenReasoning[effort] = effortIndex
		}
	}
	return nil
}

type providerFieldValidationError struct {
	path    string
	message string
}

func validateGenericProviderAuthWithoutEnvironment(cfg ProviderConfig) *providerFieldValidationError {
	apiKeyConfigured := strings.TrimSpace(cfg.APIKey) != "" || strings.TrimSpace(cfg.APIKeyEnv) != ""
	authType := providerAuthType(strings.TrimSpace(cfg.AuthType))
	if authType == "" {
		if apiKeyConfigured {
			authType = providerAuthTypeBearer
		} else {
			authType = providerAuthTypeNone
		}
	}

	authHeader := strings.TrimSpace(cfg.AuthHeader)
	switch authType {
	case providerAuthTypeNone:
		return nil
	case providerAuthTypeBearer:
		if !apiKeyConfigured {
			return &providerFieldValidationError{path: "api_key", message: "auth_type bearer requires api_key or api_key_env"}
		}
		if authHeader == "" {
			authHeader = "Authorization"
		}
	case providerAuthTypeAPIKeyHeader:
		if !apiKeyConfigured {
			return &providerFieldValidationError{path: "api_key", message: "auth_type api-key-header requires api_key or api_key_env"}
		}
		if authHeader == "" {
			return &providerFieldValidationError{path: "auth_header", message: "auth_type api-key-header requires auth_header"}
		}
	default:
		return &providerFieldValidationError{path: "auth_type", message: fmt.Sprintf("unsupported auth_type %q", cfg.AuthType)}
	}
	if !validProviderHeaderName(authHeader) {
		return &providerFieldValidationError{path: "auth_header", message: fmt.Sprintf("auth_header %q is invalid", authHeader)}
	}
	return nil
}

func providerPathValidationField(err error) string {
	message := err.Error()
	for _, field := range []string{"chat_completions_path", "responses_path", "messages_path", "models_path"} {
		if strings.HasPrefix(message, field+" ") || strings.HasPrefix(message, field+" must") {
			return field
		}
	}
	return "base_url"
}

type normalizedProviderModelFilter struct {
	included map[string]struct{}
	excluded map[string]struct{}
}

func newNormalizedProviderModelFilter(cfg ProviderConfig) normalizedProviderModelFilter {
	filter := normalizedProviderModelFilter{
		included: make(map[string]struct{}, len(cfg.IncludeModels)),
		excluded: make(map[string]struct{}, len(cfg.ExcludeModels)),
	}
	for _, model := range cfg.IncludeModels {
		if model = strings.TrimSpace(model); model != "" {
			filter.included[model] = struct{}{}
		}
	}
	for _, model := range cfg.ExcludeModels {
		if model = strings.TrimSpace(model); model != "" {
			filter.excluded[model] = struct{}{}
		}
	}
	return filter
}

func (f normalizedProviderModelFilter) allows(publicID string) bool {
	publicID = strings.TrimSpace(publicID)
	if publicID == "" {
		return false
	}
	if len(f.included) > 0 {
		if _, included := f.included[publicID]; !included {
			return false
		}
	}
	if _, excluded := f.excluded[publicID]; excluded {
		return false
	}
	return true
}

func providerConfigHasLegacyCatalog(provider providerConfigDescriptor, cfg ProviderConfig) bool {
	switch provider.kind {
	case providerTypeCopilot, providerTypeOpenAICodex:
		return true
	case providerTypeOpenAICompatible, providerTypeAnthropicCompatible:
		if provider.modelDiscovery != providerModelDiscoveryStatic {
			return true
		}
	case providerTypeAzureOpenAI:
	default:
		return false
	}

	filter := newNormalizedProviderModelFilter(cfg)
	for _, model := range provider.models {
		if filter.allows(model.PublicID) {
			return true
		}
	}
	return false
}

func providerConfigRequiresStaticModels(provider providerConfigDescriptor) bool {
	switch provider.kind {
	case providerTypeAzureOpenAI:
		return true
	case providerTypeOpenAICompatible, providerTypeAnthropicCompatible:
		return provider.modelDiscovery == providerModelDiscoveryStatic
	default:
		return false
	}
}

func normalizeAndValidateModelRouteForSchema(route *ModelRouteConfig, path string, schemaVersion int) error {
	if route == nil {
		return configPathError(path, "is required")
	}
	var err error
	if route.ID, err = normalizeOperationalID(route.ID, path+".id"); err != nil {
		return err
	}

	isPublic := true
	if providersConfigSchemaSupportsPolicyRouting(schemaVersion) {
		route.Exposure = strings.TrimSpace(route.Exposure)
		if route.Exposure == "" {
			if route.exposureSet {
				return configPathError(path+".exposure", "must not be empty")
			}
			route.Exposure = modelRouteExposurePublic
		}
		switch route.Exposure {
		case modelRouteExposurePublic:
			isPublic = true
		case modelRouteExposureInternal:
			isPublic = false
		default:
			return configPathError(path+".exposure", "unsupported route exposure %q", route.Exposure)
		}
		route.InternalPurpose = strings.TrimSpace(route.InternalPurpose)
		if route.internalPurposeSet && route.InternalPurpose == "" {
			return configPathError(path+".internal_purpose", "must not be empty")
		}
		if route.InternalPurpose != "" && route.InternalPurpose != modelRouteInternalPurposePolicyClassifier {
			return configPathError(path+".internal_purpose", "unsupported internal purpose %q", route.InternalPurpose)
		}
		if isPublic && route.InternalPurpose != "" {
			return configPathError(path+".internal_purpose", "is only valid when exposure is %q", modelRouteExposureInternal)
		}
	}

	route.PublicID = strings.TrimSpace(route.PublicID)
	if isPublic {
		if route.PublicID == "" {
			return configPathError(path+".public_id", "is required")
		}
		if containsControlCharacter(route.PublicID) {
			return configPathError(path+".public_id", "must not contain control characters")
		}
	} else {
		if route.publicIDSet || route.PublicID != "" {
			return configPathError(path+".public_id", "must be omitted when exposure is %q", modelRouteExposureInternal)
		}
		if route.modelPickerEnabledSet || route.ModelPickerEnabled != nil {
			return configPathError(path+".model_picker_enabled", "must be omitted when exposure is %q", modelRouteExposureInternal)
		}
		if route.modelPickerCategorySet || strings.TrimSpace(route.ModelPickerCategory) != "" {
			return configPathError(path+".model_picker_category", "must be omitted when exposure is %q", modelRouteExposureInternal)
		}
	}

	if route.Name = strings.TrimSpace(route.Name); route.Name == "" {
		if isPublic {
			route.Name = route.PublicID
		} else {
			route.Name = route.ID
		}
	}
	if len(route.Endpoints) == 0 {
		return configPathError(path+".endpoints", "must contain at least one canonical endpoint")
	}
	seenEndpoints := make(map[string]int, len(route.Endpoints))
	for index, rawEndpoint := range route.Endpoints {
		endpoint := strings.TrimSpace(rawEndpoint)
		endpointPath := fmt.Sprintf("%s.endpoints[%d]", path, index)
		if endpoint == "" {
			return configPathError(endpointPath, "must not be empty")
		}
		if !knownProviderEndpoint(endpoint) {
			return configPathError(endpointPath, "unsupported canonical endpoint %q", rawEndpoint)
		}
		if prior, exists := seenEndpoints[endpoint]; exists {
			return configPathError(endpointPath, "duplicates %s.endpoints[%d]", path, prior)
		}
		seenEndpoints[endpoint] = index
		route.Endpoints[index] = endpoint
	}
	_, supportsChatCompletions := seenEndpoints[providerEndpointChatCompletions]
	seenReasoning := make(map[string]int, len(route.ReasoningEffort))
	for index, rawEffort := range route.ReasoningEffort {
		effort := strings.TrimSpace(rawEffort)
		effortPath := fmt.Sprintf("%s.reasoning_effort[%d]", path, index)
		if effort == "" {
			return configPathError(effortPath, "must not be empty")
		}
		if prior, exists := seenReasoning[effort]; exists {
			return configPathError(effortPath, "duplicates %s.reasoning_effort[%d]", path, prior)
		}
		seenReasoning[effort] = index
		route.ReasoningEffort[index] = effort
	}
	if route.ContextWindow != nil && *route.ContextWindow <= 0 {
		return configPathError(path+".context_window", "must be greater than zero")
	}
	if isPublic {
		if route.ModelPickerEnabled == nil {
			enabled := true
			route.ModelPickerEnabled = &enabled
		}
		if route.ModelPickerCategory = strings.TrimSpace(route.ModelPickerCategory); route.ModelPickerCategory == "" {
			route.ModelPickerCategory = "versatile"
		}
	} else {
		route.ModelPickerEnabled = nil
		route.ModelPickerCategory = ""
	}

	if len(route.Targets) == 0 {
		return configPathError(path+".targets", "must contain at least one target")
	}
	if len(route.Targets) > maxExplicitTargetsPerRoute {
		return configPathError(path+".targets", "contains %d targets; maximum is %d", len(route.Targets), maxExplicitTargetsPerRoute)
	}
	if route.DropStopSequences != nil && !supportsChatCompletions {
		return configPathError(path+".drop_stop_sequences", "is only valid when the route advertises %q", providerEndpointChatCompletions)
	}
	seenTargets := make(map[string]int, len(route.Targets))
	for index := range route.Targets {
		targetPath := fmt.Sprintf("%s.targets[%d]", path, index)
		target := &route.Targets[index]
		if target.ID, err = normalizeOperationalID(target.ID, targetPath+".id"); err != nil {
			return err
		}
		if prior, exists := seenTargets[target.ID]; exists {
			return configPathError(targetPath+".id", "duplicates %s.targets[%d].id", path, prior)
		}
		seenTargets[target.ID] = index

		target.Provider = strings.TrimSpace(target.Provider)
		if target.Provider == "" {
			return configPathError(targetPath+".provider", "is required")
		}
		target.UpstreamModel = strings.TrimSpace(target.UpstreamModel)
		if target.UpstreamModel == "" {
			return configPathError(targetPath+".upstream_model", "is required")
		}
		if containsControlCharacter(target.UpstreamModel) {
			return configPathError(targetPath+".upstream_model", "must not contain control characters")
		}
		if target.UseMaxCompletionTokens != nil && !supportsChatCompletions {
			return configPathError(targetPath+".use_max_completion_tokens", "is only valid when the route advertises %q", providerEndpointChatCompletions)
		}
	}

	route.Routing.Mode = strings.TrimSpace(route.Routing.Mode)
	if route.Routing.Mode == "" {
		if route.Routing.modeSet {
			return configPathError(path+".routing.mode", "must not be empty")
		}
		route.Routing.Mode = string(routeModePrimaryOnly)
	}
	switch routeMode(route.Routing.Mode) {
	case routeModePrimaryOnly, routeModePriorityFailover:
	default:
		return configPathError(path+".routing.mode", "unsupported routing mode %q", route.Routing.Mode)
	}
	if route.Routing.MaxTargetAttempts < 0 || route.Routing.maxTargetAttemptsSet && route.Routing.MaxTargetAttempts == 0 {
		return configPathError(path+".routing.max_target_attempts", "must be greater than zero")
	}
	if route.Routing.MaxTargetAttempts == 0 {
		route.Routing.MaxTargetAttempts = 1
	}
	if route.Routing.MaxUpstreamSends < 0 || route.Routing.maxUpstreamSendsSet && route.Routing.MaxUpstreamSends == 0 {
		return configPathError(path+".routing.max_upstream_sends", "must be greater than zero")
	}
	if route.Routing.MaxUpstreamSends == 0 {
		route.Routing.MaxUpstreamSends = 1
	}
	if route.Routing.MaxTargetAttempts > len(route.Targets) {
		return configPathError(path+".routing.max_target_attempts", "is %d but the route has only %d targets", route.Routing.MaxTargetAttempts, len(route.Targets))
	}
	if routeMode(route.Routing.Mode) == routeModePrimaryOnly && route.Routing.MaxTargetAttempts != 1 {
		return configPathError(path+".routing.max_target_attempts", "must be 1 when routing.mode is %q", routeModePrimaryOnly)
	}
	if route.Routing.MaxUpstreamSends < route.Routing.MaxTargetAttempts {
		return configPathError(path+".routing.max_upstream_sends", "must be at least max_target_attempts (%d)", route.Routing.MaxTargetAttempts)
	}
	return nil
}

func modelRouteConfigIsPublic(route *ModelRouteConfig, schemaVersion int) bool {
	if route == nil {
		return false
	}
	if !providersConfigSchemaSupportsPolicyRouting(schemaVersion) {
		return true
	}
	return route.Exposure != modelRouteExposureInternal
}

func normalizeOperationalID(raw, path string) (string, error) {
	value := strings.TrimSpace(raw)
	if value == "" {
		return "", configPathError(path, "is required")
	}
	if raw != value {
		return "", configPathError(path, "must not contain leading or trailing whitespace")
	}
	if len(value) > maxModelRouteOperationalIDBytes {
		return "", configPathError(path, "is %d bytes; maximum is %d", len(value), maxModelRouteOperationalIDBytes)
	}
	for index := 0; index < len(value); index++ {
		char := value[index]
		if isASCIIAlphaNumeric(char) {
			continue
		}
		if index > 0 && index < len(value)-1 && (char == '-' || char == '_' || char == '.') {
			continue
		}
		return "", configPathError(path, "must start and end with an ASCII letter or digit and contain only ASCII letters, digits, '.', '_', or '-'")
	}
	return value, nil
}

func isASCIIAlphaNumeric(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9'
}

func containsControlCharacter(value string) bool {
	for _, char := range value {
		if char < 0x20 || char == 0x7f {
			return true
		}
	}
	return false
}

func configuredPublicModelAliases(publicID string) []string {
	publicID = strings.TrimSpace(publicID)
	if publicID == "" {
		return nil
	}
	aliases := []string{publicID}
	seen := map[string]struct{}{publicID: {}}
	add := func(alias string) {
		alias = strings.TrimSpace(alias)
		if alias == "" {
			return
		}
		if _, ok := seen[alias]; ok {
			return
		}
		seen[alias] = struct{}{}
		aliases = append(aliases, alias)
	}
	add(NormalizeModelName(publicID))
	add(normalizeGeminiModelName(publicID))
	for alias, canonical := range geminiModelAliases {
		if normalizeGeminiModelName(canonical) == publicID {
			add(alias)
		}
	}
	return aliases
}

func providerKindSupportsExplicitRoutes(kind providerType) bool {
	switch kind {
	case providerTypeCopilot, providerTypeAzureOpenAI, providerTypeOpenAICompatible, providerTypeAnthropicCompatible:
		return true
	default:
		return false
	}
}

func explicitRouteProviderFamily(kind providerType) string {
	switch kind {
	case providerTypeAnthropicCompatible:
		return "anthropic"
	case providerTypeCopilot, providerTypeAzureOpenAI, providerTypeOpenAICompatible:
		return "openai"
	default:
		return ""
	}
}

func providerKindSupportsExplicitEndpoint(kind providerType, endpoint string) bool {
	switch kind {
	case providerTypeCopilot, providerTypeAzureOpenAI, providerTypeOpenAICompatible:
		return endpoint == providerEndpointResponses || endpoint == providerEndpointChatCompletions
	case providerTypeAnthropicCompatible:
		return endpoint == providerEndpointMessages
	default:
		return false
	}
}

func configPathError(path, format string, args ...interface{}) error {
	message := fmt.Sprintf(format, args...)
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("%s", message)
	}
	return fmt.Errorf("%s: %s", path, message)
}

func compileExplicitModelRoutes(cfg ProvidersConfig, providers map[string]*providerRuntime) ([]*modelRoute, error) {
	validated, err := validateAndNormalizeProvidersConfig(cfg)
	if err != nil {
		return nil, err
	}
	if validated.schemaVersion != ProvidersConfigSchemaVersion2 || len(validated.config.ModelRoutes) == 0 {
		return nil, nil
	}

	routes := make([]*modelRoute, 0, len(validated.config.ModelRoutes))
	for routeIndex, routeCfg := range validated.config.ModelRoutes {
		var raw json.RawMessage
		if modelRouteConfigIsPublic(&routeCfg, validated.schemaVersion) {
			modelCfg := ProviderModelConfig{
				PublicID:            routeCfg.PublicID,
				Name:                routeCfg.Name,
				Endpoints:           append([]string(nil), routeCfg.Endpoints...),
				ModelPickerEnabled:  cloneBoolPtr(routeCfg.ModelPickerEnabled),
				ModelPickerCategory: routeCfg.ModelPickerCategory,
				ReasoningEffort:     append([]string(nil), routeCfg.ReasoningEffort...),
				Vision:              cloneBoolPtr(routeCfg.Vision),
				ParallelToolCalls:   cloneBoolPtr(routeCfg.ParallelToolCalls),
				ContextWindow:       routeCfg.ContextWindow,
			}
			raw, err = synthesizeProviderModelRaw(routeCfg.ID, routeCfg.PublicID, routeCfg.Name, routeCfg.Endpoints, modelCfg)
			if err != nil {
				return nil, configPathError(fmt.Sprintf("model_routes[%d]", routeIndex), "synthesize catalog metadata: %v", err)
			}
		}

		targets := make([]targetBinding, 0, len(routeCfg.Targets))
		for targetIndex, targetCfg := range routeCfg.Targets {
			provider := providers[targetCfg.Provider]
			if provider == nil {
				return nil, configPathError(fmt.Sprintf("model_routes[%d].targets[%d].provider", routeIndex, targetIndex), "references unknown provider %q", targetCfg.Provider)
			}
			targets = append(targets, targetBinding{
				id:            targetCfg.ID,
				provider:      provider,
				upstreamModel: targetCfg.UpstreamModel,
				wirePolicy: providerRequestPolicy{
					useMaxCompletionTokens: targetCfg.UseMaxCompletionTokens != nil && *targetCfg.UseMaxCompletionTokens,
				},
			})
		}

		exposure := modelRouteExposurePublic
		if providersConfigSchemaSupportsPolicyRouting(validated.schemaVersion) {
			exposure = routeCfg.Exposure
		}
		routes = append(routes, &modelRoute{
			public: publicModelContract{
				id:        routeCfg.PublicID,
				routeID:   routeCfg.ID,
				name:      routeCfg.Name,
				endpoints: append([]string(nil), routeCfg.Endpoints...),
				raw:       raw,
				policy: providerRequestPolicy{
					parallelToolCalls:  cloneBoolPtr(routeCfg.ParallelToolCalls),
					dropSamplingParams: routeCfg.DropSamplingParams != nil && *routeCfg.DropSamplingParams,
					dropStopSequences:  routeCfg.DropStopSequences != nil && *routeCfg.DropStopSequences,
				},
			},
			targets: targets,
			policy: routePolicy{
				mode:              routeMode(routeCfg.Routing.Mode),
				maxTargetAttempts: routeCfg.Routing.MaxTargetAttempts,
				maxUpstreamSends:  routeCfg.Routing.MaxUpstreamSends,
			},
			exposure:        exposure,
			internalPurpose: routeCfg.InternalPurpose,
		})
	}
	return routes, nil
}

var topLevelProviderConfigFields = configFieldSet(
	"schema_version", "providers", "model_routes", "policy_profiles", "tool_optimizers", "insight_model",
)

var providerConfigFields = configFieldSet(
	"id", "type", "default", "include_models", "exclude_models", "base_url", "auth_mode",
	"api_key", "api_key_env", "api_version", "token_scope", "auth_type", "auth_header",
	"auth_prefix", "extra_headers", "chat_completions_path", "responses_path", "messages_path",
	"models_path", "model_discovery", "trust_domain", "classifier_no_store_supported", "headers", "models",
)

var providerModelConfigFields = configFieldSet(
	"public_id", "deployment", "name", "endpoints", "model_picker_enabled", "model_picker_category",
	"reasoning_effort", "vision", "parallel_tool_calls", "drop_sampling_params", "drop_stop_sequences",
	"use_max_completion_tokens", "context_window",
)

var modelRouteConfigFields = configFieldSet(
	"id", "exposure", "internal_purpose", "public_id", "name", "endpoints", "reasoning_effort", "parallel_tool_calls", "vision",
	"context_window", "model_picker_enabled", "model_picker_category", "drop_sampling_params", "drop_stop_sequences",
	"targets", "routing",
)

var modelRouteTargetConfigFields = configFieldSet(
	"id", "provider", "upstream_model", "use_max_completion_tokens",
)

var modelRouteRoutingConfigFields = configFieldSet(
	"mode", "max_target_attempts", "max_upstream_sends",
)

var policyProfileConfigFields = configFieldSet(
	"id", "public_id", "name", "mode", "model_picker_enabled", "model_picker_category",
	"lightweight", "powerful", "baseline_tier", "classifier_unavailable_tier",
	"classifier_uncertain_tier", "classifier", "data_policy",
)

var policyTierConfigFields = configFieldSet(
	"route", "reasoning_effort",
)

var policyClassifierConfigFields = configFieldSet(
	"route", "profile", "timeout_ms", "max_completion_tokens", "max_request_bytes",
	"recent_turns", "max_concurrency", "observe_sample_rate",
)

var policyDataPolicyConfigFields = configFieldSet(
	"content_forwarding_acknowledged", "allow_cross_trust_domain", "allow_provider_retention",
)

func configFieldSet(fields ...string) map[string]struct{} {
	set := make(map[string]struct{}, len(fields))
	for _, field := range fields {
		set[field] = struct{}{}
	}
	return set
}

func validateJSONConfigFieldPaths(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var document interface{}
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	root, ok := document.(map[string]interface{})
	if !ok {
		return nil
	}
	if err := validateJSONKnownFields(root, topLevelProviderConfigFields, ""); err != nil {
		return err
	}
	if providers, ok := root["providers"].([]interface{}); ok {
		for index, rawProvider := range providers {
			provider, ok := rawProvider.(map[string]interface{})
			if !ok {
				continue
			}
			path := fmt.Sprintf("providers[%d]", index)
			if err := validateJSONKnownFields(provider, providerConfigFields, path); err != nil {
				return err
			}
			if models, ok := provider["models"].([]interface{}); ok {
				for modelIndex, rawModel := range models {
					model, ok := rawModel.(map[string]interface{})
					if !ok {
						continue
					}
					if err := validateJSONKnownFields(model, providerModelConfigFields, fmt.Sprintf("%s.models[%d]", path, modelIndex)); err != nil {
						return err
					}
				}
			}
		}
	}
	if routes, ok := root["model_routes"].([]interface{}); ok {
		for index, rawRoute := range routes {
			route, ok := rawRoute.(map[string]interface{})
			if !ok {
				continue
			}
			path := fmt.Sprintf("model_routes[%d]", index)
			if err := validateJSONKnownFields(route, modelRouteConfigFields, path); err != nil {
				return err
			}
			if targets, ok := route["targets"].([]interface{}); ok {
				for targetIndex, rawTarget := range targets {
					target, ok := rawTarget.(map[string]interface{})
					if !ok {
						continue
					}
					if err := validateJSONKnownFields(target, modelRouteTargetConfigFields, fmt.Sprintf("%s.targets[%d]", path, targetIndex)); err != nil {
						return err
					}
				}
			}
			if routing, ok := route["routing"].(map[string]interface{}); ok {
				if err := validateJSONKnownFields(routing, modelRouteRoutingConfigFields, path+".routing"); err != nil {
					return err
				}
			}
		}
	}
	if profiles, ok := root["policy_profiles"].([]interface{}); ok {
		for index, rawProfile := range profiles {
			profile, ok := rawProfile.(map[string]interface{})
			if !ok {
				continue
			}
			path := fmt.Sprintf("policy_profiles[%d]", index)
			if err := validateJSONKnownFields(profile, policyProfileConfigFields, path); err != nil {
				return err
			}
			for _, tierName := range []string{"lightweight", "powerful"} {
				if tier, ok := profile[tierName].(map[string]interface{}); ok {
					if err := validateJSONKnownFields(tier, policyTierConfigFields, path+"."+tierName); err != nil {
						return err
					}
				}
			}
			if classifier, ok := profile["classifier"].(map[string]interface{}); ok {
				if err := validateJSONKnownFields(classifier, policyClassifierConfigFields, path+".classifier"); err != nil {
					return err
				}
			}
			if dataPolicy, ok := profile["data_policy"].(map[string]interface{}); ok {
				if err := validateJSONKnownFields(dataPolicy, policyDataPolicyConfigFields, path+".data_policy"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateJSONKnownFields(object map[string]interface{}, allowed map[string]struct{}, path string) error {
	fields := make([]string, 0, len(object))
	for field := range object {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	for _, field := range fields {
		if _, ok := allowed[field]; ok {
			continue
		}
		return configPathError(appendConfigObjectPath(path, field), "unknown field %q", field)
	}
	return nil
}

func validateYAMLConfigFieldPaths(body []byte) error {
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	if len(document.Content) == 0 {
		return nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil
	}
	if err := validateYAMLKnownFields(root, topLevelProviderConfigFields, ""); err != nil {
		return err
	}
	if providers := yamlMappingValue(root, "providers"); providers != nil && providers.Kind == yaml.SequenceNode {
		for index, provider := range providers.Content {
			if provider.Kind != yaml.MappingNode {
				continue
			}
			path := fmt.Sprintf("providers[%d]", index)
			if err := validateYAMLKnownFields(provider, providerConfigFields, path); err != nil {
				return err
			}
			if models := yamlMappingValue(provider, "models"); models != nil && models.Kind == yaml.SequenceNode {
				for modelIndex, model := range models.Content {
					if model.Kind != yaml.MappingNode {
						continue
					}
					if err := validateYAMLKnownFields(model, providerModelConfigFields, fmt.Sprintf("%s.models[%d]", path, modelIndex)); err != nil {
						return err
					}
				}
			}
		}
	}
	if routes := yamlMappingValue(root, "model_routes"); routes != nil && routes.Kind == yaml.SequenceNode {
		for index, route := range routes.Content {
			if route.Kind != yaml.MappingNode {
				continue
			}
			path := fmt.Sprintf("model_routes[%d]", index)
			if err := validateYAMLKnownFields(route, modelRouteConfigFields, path); err != nil {
				return err
			}
			if targets := yamlMappingValue(route, "targets"); targets != nil && targets.Kind == yaml.SequenceNode {
				for targetIndex, target := range targets.Content {
					if target.Kind != yaml.MappingNode {
						continue
					}
					if err := validateYAMLKnownFields(target, modelRouteTargetConfigFields, fmt.Sprintf("%s.targets[%d]", path, targetIndex)); err != nil {
						return err
					}
				}
			}
			if routing := yamlMappingValue(route, "routing"); routing != nil && routing.Kind == yaml.MappingNode {
				if err := validateYAMLKnownFields(routing, modelRouteRoutingConfigFields, path+".routing"); err != nil {
					return err
				}
			}
		}
	}
	if profiles := yamlMappingValue(root, "policy_profiles"); profiles != nil && profiles.Kind == yaml.SequenceNode {
		for index, profile := range profiles.Content {
			if profile.Kind != yaml.MappingNode {
				continue
			}
			path := fmt.Sprintf("policy_profiles[%d]", index)
			if err := validateYAMLKnownFields(profile, policyProfileConfigFields, path); err != nil {
				return err
			}
			for _, tierName := range []string{"lightweight", "powerful"} {
				if tier := yamlMappingValue(profile, tierName); tier != nil && tier.Kind == yaml.MappingNode {
					if err := validateYAMLKnownFields(tier, policyTierConfigFields, path+"."+tierName); err != nil {
						return err
					}
				}
			}
			if classifier := yamlMappingValue(profile, "classifier"); classifier != nil && classifier.Kind == yaml.MappingNode {
				if err := validateYAMLKnownFields(classifier, policyClassifierConfigFields, path+".classifier"); err != nil {
					return err
				}
			}
			if dataPolicy := yamlMappingValue(profile, "data_policy"); dataPolicy != nil && dataPolicy.Kind == yaml.MappingNode {
				if err := validateYAMLKnownFields(dataPolicy, policyDataPolicyConfigFields, path+".data_policy"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func validateYAMLKnownFields(node *yaml.Node, allowed map[string]struct{}, path string) error {
	for index := 0; index+1 < len(node.Content); index += 2 {
		field := node.Content[index].Value
		if _, ok := allowed[field]; ok {
			continue
		}
		return configPathError(appendConfigObjectPath(path, field), "unknown field %q", field)
	}
	return nil
}

func yamlMappingValue(node *yaml.Node, field string) *yaml.Node {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == field {
			return node.Content[index+1]
		}
	}
	return nil
}

func markJSONProvidersConfigFieldPresence(body []byte, cfg *ProvidersConfig) {
	if cfg == nil {
		return
	}
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return
	}

	var providers []map[string]json.RawMessage
	if json.Unmarshal(root["providers"], &providers) == nil {
		for index := range providers {
			if index >= len(cfg.Providers) {
				break
			}
			_, cfg.Providers[index].trustDomainSet = providers[index]["trust_domain"]
			_, cfg.Providers[index].classifierNoStoreSupportedSet = providers[index]["classifier_no_store_supported"]
		}
	}

	var routes []map[string]json.RawMessage
	if json.Unmarshal(root["model_routes"], &routes) == nil {
		for index := range routes {
			if index >= len(cfg.ModelRoutes) {
				break
			}
			_, cfg.ModelRoutes[index].exposureSet = routes[index]["exposure"]
			_, cfg.ModelRoutes[index].internalPurposeSet = routes[index]["internal_purpose"]
			_, cfg.ModelRoutes[index].publicIDSet = routes[index]["public_id"]
			_, cfg.ModelRoutes[index].modelPickerEnabledSet = routes[index]["model_picker_enabled"]
			_, cfg.ModelRoutes[index].modelPickerCategorySet = routes[index]["model_picker_category"]
		}
	}

	var profiles []map[string]json.RawMessage
	if json.Unmarshal(root["policy_profiles"], &profiles) == nil {
		for index := range profiles {
			if index >= len(cfg.PolicyProfiles) {
				break
			}
			profile := &cfg.PolicyProfiles[index]
			for _, tierField := range []struct {
				name          string
				tier          *PolicyTierConfig
				set, nullFlag *bool
			}{
				{name: "lightweight", tier: &profile.Lightweight, set: &profile.lightweightSet, nullFlag: &profile.lightweightNull},
				{name: "powerful", tier: &profile.Powerful, set: &profile.powerfulSet, nullFlag: &profile.powerfulNull},
			} {
				rawTier, tierSet := profiles[index][tierField.name]
				*tierField.set = tierSet
				*tierField.nullFlag = tierSet && bytes.Equal(bytes.TrimSpace(rawTier), []byte("null"))
				var fields map[string]json.RawMessage
				if json.Unmarshal(rawTier, &fields) == nil {
					rawEffort, effortSet := fields["reasoning_effort"]
					tierField.tier.reasoningEffortSet = effortSet
					tierField.tier.reasoningEffortNull = effortSet && bytes.Equal(bytes.TrimSpace(rawEffort), []byte("null"))
				}
			}
			var classifier map[string]json.RawMessage
			if json.Unmarshal(profiles[index]["classifier"], &classifier) != nil {
				continue
			}
			markPolicyClassifierFieldPresence(&cfg.PolicyProfiles[index].Classifier, func(field string) bool {
				_, ok := classifier[field]
				return ok
			}, func(field string) bool {
				raw, ok := classifier[field]
				return ok && bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
			})
		}
	}
}

func markYAMLProvidersConfigFieldPresence(body []byte, cfg *ProvidersConfig) {
	if cfg == nil {
		return
	}
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	var document yaml.Node
	if decoder.Decode(&document) != nil || len(document.Content) == 0 {
		return
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return
	}

	if providers := yamlMappingValue(root, "providers"); providers != nil && providers.Kind == yaml.SequenceNode {
		for index, provider := range providers.Content {
			provider = yamlDereferenceAlias(provider)
			if index >= len(cfg.Providers) || provider == nil || provider.Kind != yaml.MappingNode {
				continue
			}
			cfg.Providers[index].trustDomainSet = yamlMappingHasField(provider, "trust_domain")
			cfg.Providers[index].classifierNoStoreSupportedSet = yamlMappingHasField(provider, "classifier_no_store_supported")
		}
	}
	if routes := yamlMappingValue(root, "model_routes"); routes != nil && routes.Kind == yaml.SequenceNode {
		for index, route := range routes.Content {
			route = yamlDereferenceAlias(route)
			if index >= len(cfg.ModelRoutes) || route == nil || route.Kind != yaml.MappingNode {
				continue
			}
			cfg.ModelRoutes[index].exposureSet = yamlMappingHasField(route, "exposure")
			cfg.ModelRoutes[index].internalPurposeSet = yamlMappingHasField(route, "internal_purpose")
			cfg.ModelRoutes[index].publicIDSet = yamlMappingHasField(route, "public_id")
			cfg.ModelRoutes[index].modelPickerEnabledSet = yamlMappingHasField(route, "model_picker_enabled")
			cfg.ModelRoutes[index].modelPickerCategorySet = yamlMappingHasField(route, "model_picker_category")
		}
	}
	if profiles := yamlMappingValue(root, "policy_profiles"); profiles != nil && profiles.Kind == yaml.SequenceNode {
		for index, profile := range profiles.Content {
			profile = yamlDereferenceAlias(profile)
			if index >= len(cfg.PolicyProfiles) || profile == nil || profile.Kind != yaml.MappingNode {
				continue
			}
			profileCfg := &cfg.PolicyProfiles[index]
			for _, tierField := range []struct {
				name          string
				tier          *PolicyTierConfig
				set, nullFlag *bool
			}{
				{name: "lightweight", tier: &profileCfg.Lightweight, set: &profileCfg.lightweightSet, nullFlag: &profileCfg.lightweightNull},
				{name: "powerful", tier: &profileCfg.Powerful, set: &profileCfg.powerfulSet, nullFlag: &profileCfg.powerfulNull},
			} {
				*tierField.set = yamlMappingHasField(profile, tierField.name)
				*tierField.nullFlag = yamlMappingFieldIsNull(profile, tierField.name)
				tier := yamlDereferenceAlias(yamlMappingValue(profile, tierField.name))
				if tier != nil && tier.Kind == yaml.MappingNode {
					tierField.tier.reasoningEffortSet = yamlMappingHasField(tier, "reasoning_effort")
					tierField.tier.reasoningEffortNull = yamlMappingFieldIsNull(tier, "reasoning_effort")
				}
			}
			classifier := yamlDereferenceAlias(yamlMappingValue(profile, "classifier"))
			if classifier == nil || classifier.Kind != yaml.MappingNode {
				continue
			}
			markPolicyClassifierFieldPresence(&cfg.PolicyProfiles[index].Classifier, func(field string) bool {
				return yamlMappingHasField(classifier, field)
			}, func(field string) bool {
				return yamlMappingFieldIsNull(classifier, field)
			})
		}
	}
}

func yamlDereferenceAlias(node *yaml.Node) *yaml.Node {
	seen := make(map[*yaml.Node]struct{})
	for node != nil && node.Kind == yaml.AliasNode {
		if _, exists := seen[node]; exists || node.Alias == nil {
			return nil
		}
		seen[node] = struct{}{}
		node = node.Alias
	}
	return node
}

func markPolicyClassifierFieldPresence(classifier *PolicyClassifierConfig, has func(string) bool, isNull func(string) bool) {
	if classifier == nil || has == nil {
		return
	}
	classifier.timeoutMSSet = has("timeout_ms")
	classifier.maxCompletionTokensSet = has("max_completion_tokens")
	classifier.maxRequestBytesSet = has("max_request_bytes")
	classifier.recentTurnsSet = has("recent_turns")
	classifier.maxConcurrencySet = has("max_concurrency")
	classifier.observeSampleRateSet = has("observe_sample_rate")
	if isNull == nil {
		return
	}
	classifier.timeoutMSNull = isNull("timeout_ms")
	classifier.maxCompletionTokensNull = isNull("max_completion_tokens")
	classifier.maxRequestBytesNull = isNull("max_request_bytes")
	classifier.recentTurnsNull = isNull("recent_turns")
	classifier.maxConcurrencyNull = isNull("max_concurrency")
	classifier.observeSampleRateNull = isNull("observe_sample_rate")
}

func yamlMappingFieldIsNull(node *yaml.Node, field string) bool {
	value := yamlDereferenceAlias(yamlMappingValue(node, field))
	if value == nil {
		return false
	}
	if value.Tag == "!!null" {
		return true
	}
	trimmed := strings.TrimSpace(value.Value)
	return value.Kind == yaml.ScalarNode && (trimmed == "" || trimmed == "~" || strings.EqualFold(trimmed, "null"))
}

func yamlMappingHasField(node *yaml.Node, field string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return false
	}
	for index := 0; index+1 < len(node.Content); index += 2 {
		if node.Content[index].Value == field {
			return true
		}
	}
	return false
}

func jsonTopLevelConfigFields(body []byte) (map[string]bool, error) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	if raw == nil {
		return nil, fmt.Errorf("top-level provider configuration must be a JSON object")
	}
	present := make(map[string]bool, len(raw))
	for key := range raw {
		present[key] = true
	}
	return present, nil
}

func yamlTopLevelConfigFields(body []byte) (map[string]bool, error) {
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	present := make(map[string]bool)
	if len(document.Content) == 0 {
		return present, nil
	}
	root := document.Content[0]
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("top-level provider configuration must be a YAML mapping")
	}
	for index := 0; index+1 < len(root.Content); index += 2 {
		key := root.Content[index]
		if key.Kind == yaml.ScalarNode {
			present[key.Value] = true
		}
	}
	return present, nil
}

func rejectDuplicateJSONMappingKeys(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	if err := scanJSONValueForDuplicateKeys(decoder, ""); err != nil {
		return err
	}
	return nil
}

func scanJSONValueForDuplicateKeys(decoder *json.Decoder, path string) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, isDelimiter := token.(json.Delim)
	if !isDelimiter {
		return nil
	}

	switch delimiter {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok {
				return fmt.Errorf("%s: JSON mapping key is not a string", path)
			}
			keyPath := appendConfigObjectPath(path, key)
			if _, exists := seen[key]; exists {
				return configPathError(keyPath, "duplicate mapping key %q", key)
			}
			seen[key] = struct{}{}
			if err := scanJSONValueForDuplicateKeys(decoder, keyPath); err != nil {
				return err
			}
		}
		_, err = decoder.Token()
		return err
	case '[':
		index := 0
		for decoder.More() {
			if err := scanJSONValueForDuplicateKeys(decoder, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
			index++
		}
		_, err = decoder.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delimiter)
	}
}

func rejectDuplicateYAMLMappingKeys(body []byte, allowMergeKeys bool) error {
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	var document yaml.Node
	if err := decoder.Decode(&document); err != nil {
		return err
	}
	if len(document.Content) == 0 {
		return nil
	}
	return scanYAMLNodeForDuplicateKeys(document.Content[0], "", make(map[*yaml.Node]bool), allowMergeKeys)
}

func scanYAMLNodeForDuplicateKeys(node *yaml.Node, path string, visiting map[*yaml.Node]bool, allowMergeKeys bool) error {
	if node == nil {
		return nil
	}
	if visiting[node] {
		return configPathError(path, "recursive YAML aliases are not supported")
	}
	visiting[node] = true
	defer delete(visiting, node)

	switch node.Kind {
	case yaml.DocumentNode:
		if len(node.Content) > 0 {
			return scanYAMLNodeForDuplicateKeys(node.Content[0], path, visiting, allowMergeKeys)
		}
	case yaml.MappingNode:
		seen := make(map[string]*yaml.Node, len(node.Content)/2)
		for index := 0; index+1 < len(node.Content); index += 2 {
			keyNode := node.Content[index]
			valueNode := node.Content[index+1]
			if keyNode.Kind != yaml.ScalarNode {
				return configPathError(path, "YAML mapping keys must be strings")
			}
			key := keyNode.Value
			keyPath := appendConfigObjectPath(path, key)
			if key == "<<" {
				if allowMergeKeys {
					continue
				}
				return configPathError(keyPath, "YAML merge keys are not supported")
			}
			if keyNode.Tag != "!!str" {
				return configPathError(path, "YAML mapping keys must be strings")
			}
			if prior, exists := seen[key]; exists {
				return configPathError(keyPath, "duplicate mapping key %q (first declared at line %d)", key, prior.Line)
			}
			seen[key] = keyNode
			if err := scanYAMLNodeForDuplicateKeys(valueNode, keyPath, visiting, allowMergeKeys); err != nil {
				return err
			}
		}
	case yaml.SequenceNode:
		for index, child := range node.Content {
			if err := scanYAMLNodeForDuplicateKeys(child, fmt.Sprintf("%s[%d]", path, index), visiting, allowMergeKeys); err != nil {
				return err
			}
		}
	case yaml.AliasNode:
		return scanYAMLNodeForDuplicateKeys(node.Alias, path, visiting, allowMergeKeys)
	}
	return nil
}

func appendConfigObjectPath(path, key string) string {
	if path == "" {
		return key
	}
	if isSimpleConfigPathKey(key) {
		return path + "." + key
	}
	encoded, _ := json.Marshal(key)
	return path + "[" + string(encoded) + "]"
}

func isSimpleConfigPathKey(key string) bool {
	if key == "" {
		return false
	}
	for index := 0; index < len(key); index++ {
		char := key[index]
		if isASCIIAlphaNumeric(char) || char == '_' {
			continue
		}
		return false
	}
	return true
}
