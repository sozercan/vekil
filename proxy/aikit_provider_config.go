package proxy

import (
	"fmt"
	"strings"
	"time"
)

// providerTypeAIKit marks a provider whose upstream is an AIKit model container
// started by the vekil CLI. The proxy never serves it directly: callers must
// replace it with a static openai-compatible provider pointing at the running
// container before constructing a handler.
const providerTypeAIKit providerType = "aikit"

// ProviderTypeAIKit is the providers-config type for AIKit model containers.
const ProviderTypeAIKit = string(providerTypeAIKit)

// aikitValidationBaseURL stands in for a container URL while an unstarted
// aikit provider is validated.
const aikitValidationBaseURL = "http://127.0.0.1/v1"

// AIKitProviderConfig describes the AIKit model container behind a
// `type: aikit` provider.
type AIKitProviderConfig struct {
	// Model is an AIKit model reference without the aikit: prefix: a pre-made
	// name such as qwen3.8:27b, an image reference, an https .gguf URL, or
	// hf.co/org/repo@commit.
	Model string `json:"model" yaml:"model"`
	// ContextSize is an explicit context window; zero lets vekil choose.
	ContextSize int `json:"context_size,omitempty" yaml:"context_size,omitempty"`
	// Runtime selects the container engine: auto, docker, or podman.
	Runtime string `json:"runtime,omitempty" yaml:"runtime,omitempty"`
	// Backend selects the runner backend for runner references.
	Backend string `json:"backend,omitempty" yaml:"backend,omitempty"`
	// Keep leaves the container running when vekil exits.
	Keep bool `json:"keep,omitempty" yaml:"keep,omitempty"`
	// LoadTimeout bounds model loading, as a Go duration such as 10m.
	LoadTimeout string `json:"load_timeout,omitempty" yaml:"load_timeout,omitempty"`
}

var aikitProviderConfigFields = configFieldSet("model", "context_size", "runtime", "backend", "keep", "load_timeout")

// IsAIKit reports whether the provider is an AIKit provider that has not been
// started yet.
func (p ProviderConfig) IsAIKit() bool {
	return providerType(strings.TrimSpace(p.Type)) == providerTypeAIKit
}

// HasAIKitProviders reports whether any provider still needs an AIKit
// container before the proxy can serve it.
func (c ProvidersConfig) HasAIKitProviders() bool {
	for _, provider := range c.Providers {
		if provider.IsAIKit() {
			return true
		}
	}
	return false
}

// LoadTimeoutDuration parses LoadTimeout; zero means the default.
func (c AIKitProviderConfig) LoadTimeoutDuration() (time.Duration, error) {
	value := strings.TrimSpace(c.LoadTimeout)
	if value == "" {
		return 0, nil
	}
	parsed, err := time.ParseDuration(value)
	if err != nil {
		return 0, err
	}
	if parsed <= 0 {
		return 0, fmt.Errorf("must be positive")
	}
	return parsed, nil
}

func validateAIKitProvider(provider ProviderConfig, index int) error {
	path := fmt.Sprintf("providers[%d]", index)
	if provider.AIKit == nil {
		return configPathError(path+".aikit", "is required for type aikit")
	}
	block := provider.AIKit
	if strings.TrimSpace(block.Model) == "" {
		return configPathError(path+".aikit.model", "is required")
	}
	if strings.HasPrefix(strings.TrimSpace(block.Model), "aikit:") {
		return configPathError(path+".aikit.model", "must not include the aikit: prefix")
	}
	if block.ContextSize < 0 {
		return configPathError(path+".aikit.context_size", "must not be negative")
	}
	switch strings.TrimSpace(block.Runtime) {
	case "", "auto", "docker", "podman":
	default:
		return configPathError(path+".aikit.runtime", "must be auto, docker, or podman")
	}
	switch strings.TrimSpace(block.Backend) {
	case "", "llama-cpp", "vllm-cpp":
	default:
		return configPathError(path+".aikit.backend", "must be llama-cpp or vllm-cpp")
	}
	if _, err := block.LoadTimeoutDuration(); err != nil {
		return configPathError(path+".aikit.load_timeout", "%v", err)
	}
	// vekil owns the connection to the container it starts.
	forbidden := []struct {
		name string
		set  bool
	}{
		{"base_url", strings.TrimSpace(provider.BaseURL) != ""},
		{"auth_mode", strings.TrimSpace(provider.AuthMode) != ""},
		{"api_key", strings.TrimSpace(provider.APIKey) != ""},
		{"api_key_env", strings.TrimSpace(provider.APIKeyEnv) != ""},
		{"api_version", strings.TrimSpace(provider.APIVersion) != ""},
		{"token_scope", strings.TrimSpace(provider.TokenScope) != ""},
		{"auth_type", strings.TrimSpace(provider.AuthType) != ""},
		{"auth_header", strings.TrimSpace(provider.AuthHeader) != ""},
		{"auth_prefix", strings.TrimSpace(provider.AuthPrefix) != ""},
		{"extra_headers", len(provider.ExtraHeaders) > 0},
		{"chat_completions_path", strings.TrimSpace(provider.ChatCompletionsPath) != ""},
		{"responses_path", strings.TrimSpace(provider.ResponsesPath) != ""},
		{"messages_path", strings.TrimSpace(provider.MessagesPath) != ""},
		{"models_path", strings.TrimSpace(provider.ModelsPath) != ""},
		{"systemone_path", strings.TrimSpace(provider.SystemOnePath) != ""},
		{"model_discovery", strings.TrimSpace(provider.ModelDiscovery) != ""},
		{"include_models", len(provider.IncludeModels) > 0},
		{"exclude_models", len(provider.ExcludeModels) > 0},
		{"hosted_tools", len(provider.HostedTools) > 0},
	}
	for _, field := range forbidden {
		if field.set {
			return configPathError(path+"."+field.name, "is managed by vekil for type aikit")
		}
	}
	return nil
}

// aikitValidationShadow returns a copy of cfg in which each aikit provider is
// described as the static openai-compatible provider it becomes once its
// container runs, so routes, policies, and model IDs validate normally.
func aikitValidationShadow(cfg ProvidersConfig) (ProvidersConfig, []int, error) {
	shadow := cloneProvidersConfigForValidation(cfg)
	routeReferenced := map[string]bool{}
	for _, route := range cfg.ModelRoutes {
		for _, target := range route.Targets {
			routeReferenced[strings.TrimSpace(target.Provider)] = true
		}
	}
	var indexes []int
	for index := range shadow.Providers {
		provider := &shadow.Providers[index]
		if provider.AIKit != nil && !provider.IsAIKit() {
			return ProvidersConfig{}, nil, configPathError(fmt.Sprintf("providers[%d].aikit", index), "requires type aikit")
		}
		if !provider.IsAIKit() {
			continue
		}
		if err := validateAIKitProvider(*provider, index); err != nil {
			return ProvidersConfig{}, nil, err
		}
		indexes = append(indexes, index)
		provider.Type = string(providerTypeOpenAICompatible)
		provider.BaseURL = aikitValidationBaseURL
		provider.AuthType = string(providerAuthTypeNone)
		provider.AIKit = nil
		provider.UpstreamDialect = string(providerUpstreamDialectLocalAI)
		// Routes own the public contract of a route-referenced provider.
		if len(provider.Models) == 0 && !routeReferenced[strings.TrimSpace(provider.ID)] {
			provider.Models = []ProviderModelConfig{{PublicID: unusedValidationPublicID(cfg, "aikit-"+strings.TrimSpace(provider.ID))}}
		}
	}
	return shadow, indexes, nil
}

// unusedValidationPublicID returns base, or base with a numeric suffix, such
// that none of its normalized aliases matches a public ID the config declares.
// The placeholder exists only during validation and must not cause a
// collision the running config would not have.
func unusedValidationPublicID(cfg ProvidersConfig, base string) string {
	used := map[string]bool{}
	add := func(publicID string) {
		for _, alias := range configuredPublicModelAliases(publicID) {
			used[alias] = true
		}
	}
	for _, provider := range cfg.Providers {
		for _, model := range provider.Models {
			add(model.PublicID)
		}
	}
	for _, route := range cfg.ModelRoutes {
		add(route.PublicID)
	}
	for _, profile := range cfg.PolicyProfiles {
		add(profile.PublicID)
	}
	candidate := base
	for suffix := 2; ; suffix++ {
		free := true
		for _, alias := range configuredPublicModelAliases(candidate) {
			if used[alias] {
				free = false
			}
		}
		if free {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, suffix)
	}
}

func validateAndNormalizeProvidersConfigWithAIKit(cfg ProvidersConfig) (validatedProvidersConfig, error) {
	shadow, indexes, err := aikitValidationShadow(cfg)
	if err != nil {
		return validatedProvidersConfig{}, err
	}
	validated, err := validateAndNormalizeProvidersConfig(shadow)
	if err != nil {
		return validatedProvidersConfig{}, err
	}
	original := cloneProvidersConfigForValidation(cfg)
	for _, index := range indexes {
		validated.config.Providers[index] = original.Providers[index]
	}
	return validated, nil
}

func unmaterializedAIKitProviderError(id string) error {
	return fmt.Errorf("provider %q has type aikit, which the vekil CLI must start before serving (vekil serve, vekil launch, or the tray app)", id)
}

// AIKitModelEndpoints are the native endpoints an AIKit (LocalAI) model
// serves: Chat Completions and, with function-only tools, Responses.
func AIKitModelEndpoints() []string {
	return []string{providerEndpointChatCompletions, providerEndpointResponses}
}
