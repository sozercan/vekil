package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"gopkg.in/yaml.v3"
)

type providerType string
type providerAuthMode string

const (
	providerTypeCopilot             providerType = "copilot"
	providerTypeAzureOpenAI         providerType = "azure-openai"
	providerTypeOpenAICodex         providerType = "openai-codex"
	providerTypeOpenAICompatible    providerType = "openai-compatible"
	providerTypeAnthropicCompatible providerType = "anthropic-compatible"

	providerAuthModeAPIKey        providerAuthMode = "api_key"
	providerAuthModeAzureIdentity providerAuthMode = "azure_identity"
)

type providerAuthType string

const (
	providerAuthTypeBearer       providerAuthType = "bearer"
	providerAuthTypeAPIKeyHeader providerAuthType = "api-key-header"
	providerAuthTypeNone         providerAuthType = "none"
)

type providerModelDiscovery string

const (
	providerModelDiscoveryStatic          providerModelDiscovery = "static"
	providerModelDiscoveryOpenAI          providerModelDiscovery = "openai"
	providerModelDiscoveryOllama          providerModelDiscovery = "ollama"
	providerModelDiscoveryOpenRouterTools providerModelDiscovery = "openrouter-tools"
)

const (
	providerEndpointChatCompletions = "/chat/completions"
	providerEndpointResponses       = "/responses"
	providerEndpointMessages        = "/v1/messages"
	providerEndpointMessagesCount   = "/v1/messages/count_tokens"
	providerEndpointModels          = "/models"
)

var openAICodexProviderEndpoints = []string{providerEndpointResponses}

// ProvidersConfig configures optional non-Copilot upstream providers.
// When empty, the proxy keeps its legacy zero-config Copilot behavior.
type ProvidersConfig struct {
	Providers        []ProviderConfig     `json:"providers" yaml:"providers"`
	ToolOptimizers   ToolOptimizersConfig `json:"tool_optimizers,omitempty" yaml:"tool_optimizers,omitempty"`
	SpeedTierEnabled bool                 `json:"speed_tier_enabled,omitempty" yaml:"speed_tier_enabled,omitempty"`
	// InsightModel is the public model ID the dashboard uses to generate
	// natural-language traffic insights on demand. Empty disables the feature
	// (the dashboard's "Generate insights" button is hidden). The model must be
	// one served by the configured providers.
	InsightModel string `json:"insight_model,omitempty" yaml:"insight_model,omitempty"`
}

// ProviderConfig configures one upstream provider instance.
type ProviderConfig struct {
	ID                  string                      `json:"id" yaml:"id"`
	Type                string                      `json:"type" yaml:"type"`
	Default             bool                        `json:"default,omitempty" yaml:"default,omitempty"`
	IncludeModels       []string                    `json:"include_models,omitempty" yaml:"include_models,omitempty"`
	ExcludeModels       []string                    `json:"exclude_models,omitempty" yaml:"exclude_models,omitempty"`
	BaseURL             string                      `json:"base_url,omitempty" yaml:"base_url,omitempty"`
	AuthMode            string                      `json:"auth_mode,omitempty" yaml:"auth_mode,omitempty"`
	APIKey              string                      `json:"api_key,omitempty" yaml:"api_key,omitempty"`
	APIKeyEnv           string                      `json:"api_key_env,omitempty" yaml:"api_key_env,omitempty"`
	APIVersion          string                      `json:"api_version,omitempty" yaml:"api_version,omitempty"`
	TokenScope          string                      `json:"token_scope,omitempty" yaml:"token_scope,omitempty"`
	AuthType            string                      `json:"auth_type,omitempty" yaml:"auth_type,omitempty"`
	AuthHeader          string                      `json:"auth_header,omitempty" yaml:"auth_header,omitempty"`
	AuthPrefix          string                      `json:"auth_prefix,omitempty" yaml:"auth_prefix,omitempty"`
	ExtraHeaders        map[string]string           `json:"extra_headers,omitempty" yaml:"extra_headers,omitempty"`
	ChatCompletionsPath string                      `json:"chat_completions_path,omitempty" yaml:"chat_completions_path,omitempty"`
	ResponsesPath       string                      `json:"responses_path,omitempty" yaml:"responses_path,omitempty"`
	MessagesPath        string                      `json:"messages_path,omitempty" yaml:"messages_path,omitempty"`
	ModelsPath          string                      `json:"models_path,omitempty" yaml:"models_path,omitempty"`
	ModelDiscovery      string                      `json:"model_discovery,omitempty" yaml:"model_discovery,omitempty"`
	Headers             CopilotHeaderProfilesConfig `json:"headers,omitempty" yaml:"headers,omitempty"`
	Models              []ProviderModelConfig       `json:"models,omitempty" yaml:"models,omitempty"`
}

// ProviderModelConfig maps a public model ID exposed by this proxy to the
// upstream model or deployment name used by the provider.
type ProviderModelConfig struct {
	PublicID            string           `json:"public_id" yaml:"public_id"`
	Deployment          string           `json:"deployment,omitempty" yaml:"deployment,omitempty"`
	Name                string           `json:"name,omitempty" yaml:"name,omitempty"`
	Endpoints           []string         `json:"endpoints,omitempty" yaml:"endpoints,omitempty"`
	ModelPickerEnabled  *bool            `json:"model_picker_enabled,omitempty" yaml:"model_picker_enabled,omitempty"`
	ModelPickerCategory string           `json:"model_picker_category,omitempty" yaml:"model_picker_category,omitempty"`
	ReasoningEffort     []string         `json:"reasoning_effort,omitempty" yaml:"reasoning_effort,omitempty"`
	Vision              *bool            `json:"vision,omitempty" yaml:"vision,omitempty"`
	ParallelToolCalls   *bool            `json:"parallel_tool_calls,omitempty" yaml:"parallel_tool_calls,omitempty"`
	DropSamplingParams  *bool            `json:"drop_sampling_params,omitempty" yaml:"drop_sampling_params,omitempty"`
	ContextWindow       *int64           `json:"context_window,omitempty" yaml:"context_window,omitempty"`
	SpeedTier           *SpeedTierConfig `json:"speed_tier,omitempty" yaml:"speed_tier,omitempty"`
}

type providerRuntime struct {
	id             string
	kind           providerType
	isDefault      bool
	baseURL        string
	authMode       providerAuthMode
	apiKey         string
	apiVersion     string
	tokenScope     string
	azureToken     azureTokenSource
	authType       providerAuthType
	authHeader     string
	authPrefix     string
	extraHeaders   http.Header
	paths          providerEndpointPaths
	modelDiscovery providerModelDiscovery
	includeModels  map[string]struct{}
	excludeModels  map[string]struct{}
	staticModels   map[string]providerModel
	staticConfigs  map[string]ProviderModelConfig
	staticOrder    []string
	codexAuth      *openAICodexAuth
	headerProfiles CopilotHeaderProfilesConfig
}

type providerEndpointPaths struct {
	chatCompletions string
	responses       string
	messages        string
	models          string
}

type providerModel struct {
	publicID           string
	upstreamModel      string
	providerID         string
	supportedEndpoints []string
	parallelToolCalls  *bool
	dropSamplingParams bool
	speedTier          *speedTierRule
	disabled           bool
	raw                json.RawMessage
}

type providerSetup struct {
	providers          map[string]*providerRuntime
	providerOrder      []string
	defaultProviderID  string
	modelsMu           sync.RWMutex
	models             map[string]providerModel
	speedTierEnabled   bool
	hasConfiguredState bool
}

type providerModelsFetchResult struct {
	models      []providerModel
	etag        string
	notModified bool
}

type openAICodexReasoningPreset struct {
	Effort string `json:"effort"`
}

type openAICodexModelPayload struct {
	Slug                        string                       `json:"slug"`
	DisplayName                 string                       `json:"display_name"`
	Description                 string                       `json:"description"`
	Visibility                  string                       `json:"visibility"`
	SupportedInAPI              bool                         `json:"supported_in_api"`
	Priority                    int                          `json:"priority"`
	SupportedReasoningLevels    []openAICodexReasoningPreset `json:"supported_reasoning_levels"`
	SupportsParallelToolCalls   bool                         `json:"supports_parallel_tool_calls"`
	SupportsImageDetailOriginal bool                         `json:"supports_image_detail_original"`
	SupportsReasoningSummaries  bool                         `json:"supports_reasoning_summaries"`
	SupportVerbosity            bool                         `json:"support_verbosity"`
	ContextWindow               *int64                       `json:"context_window,omitempty"`
	MaxContextWindow            *int64                       `json:"max_context_window,omitempty"`
	AutoCompactTokenLimit       *int64                       `json:"auto_compact_token_limit,omitempty"`
	EffectiveContextWindowPct   int64                        `json:"effective_context_window_percent,omitempty"`
	InputModalities             []string                     `json:"input_modalities"`
	ExperimentalSupportedTools  []string                     `json:"experimental_supported_tools"`
	BaseInstructions            string                       `json:"base_instructions"`
	ShellType                   string                       `json:"shell_type"`
	DefaultReasoningLevel       string                       `json:"default_reasoning_level"`
}

type providerRequestError struct {
	statusCode int
	err        error
}

type azureBaseURLKind int

const (
	azureBaseURLKindInvalid azureBaseURLKind = iota
	azureBaseURLKindResourceRoot
	azureBaseURLKindLegacyOpenAI
	azureBaseURLKindOpenAIV1
	azureBaseURLKindModels
)

func (e *providerRequestError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *providerRequestError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func LoadProvidersConfigFile(path string) (ProvidersConfig, error) {
	var cfg ProvidersConfig
	path = strings.TrimSpace(path)
	if path == "" {
		return cfg, nil
	}

	body, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read providers config %q: %w", path, err)
	}
	if err := decodeProvidersConfigFile(path, body, &cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func decodeProvidersConfigFile(path string, body []byte, cfg *ProvidersConfig) error {
	if len(bytes.TrimSpace(body)) == 0 {
		return fmt.Errorf("providers config %q is empty", path)
	}

	switch strings.ToLower(filepath.Ext(path)) {
	case ".yaml", ".yml":
		if err := yaml.Unmarshal(body, cfg); err != nil {
			return fmt.Errorf("decode providers config %q as YAML: %w", path, err)
		}
	default:
		if err := json.Unmarshal(body, cfg); err != nil {
			return fmt.Errorf("decode providers config %q as JSON: %w", path, err)
		}
	}
	return nil
}

func (c ProvidersConfig) UsesCopilot() bool {
	if len(c.Providers) == 0 {
		return true
	}
	for _, provider := range c.Providers {
		if providerType(strings.TrimSpace(provider.Type)) == providerTypeCopilot {
			return true
		}
	}
	return false
}

func defaultProviderSetup(h *ProxyHandler) *providerSetup {
	return &providerSetup{
		providers: map[string]*providerRuntime{
			"copilot": {
				id:            "copilot",
				kind:          providerTypeCopilot,
				isDefault:     true,
				baseURL:       strings.TrimRight(h.copilotURL, "/"),
				paths:         providerEndpointPolicyFor(providerTypeCopilot).defaultEndpointPaths(),
				includeModels: map[string]struct{}{},
				excludeModels: map[string]struct{}{},
				staticModels:  map[string]providerModel{},
			},
		},
		providerOrder:     []string{"copilot"},
		defaultProviderID: "copilot",
		models:            map[string]providerModel{},
	}
}

func (h *ProxyHandler) providerSetup() *providerSetup {
	if h != nil && h.providersState != nil {
		return h.providersState
	}
	return defaultProviderSetup(h)
}

func (ps *providerSetup) defaultProvider() *providerRuntime {
	if ps == nil {
		return nil
	}
	return ps.providers[ps.defaultProviderID]
}

func (ps *providerSetup) providerByID(id string) *providerRuntime {
	if ps == nil {
		return nil
	}
	return ps.providers[id]
}

func (ps *providerSetup) lookupModel(model string) (providerModel, bool) {
	if ps == nil {
		return providerModel{}, false
	}
	ps.modelsMu.RLock()
	defer ps.modelsMu.RUnlock()
	pm, ok := ps.models[strings.TrimSpace(model)]
	return pm, ok
}

func (ps *providerSetup) addProviderModels(providerID string, models []providerModel) error {
	if ps == nil {
		return nil
	}

	provider := ps.providerByID(providerID)
	models = filterProviderModels(provider, models)
	if ps.speedTierEnabled {
		if err := validateProviderSpeedTierModels(providerID, models); err != nil {
			return err
		}
	}

	ps.modelsMu.Lock()
	defer ps.modelsMu.Unlock()

	return mergeProviderModels(ps.models, models)
}

func (ps *providerSetup) addStaticProviderModels(providerID string) error {
	if ps == nil {
		return nil
	}
	return ps.addProviderModels(providerID, orderedStaticProviderModels(ps.providerByID(providerID)))
}

func (ps *providerSetup) replaceProviderModels(providerID string, models []providerModel) error {
	if ps == nil {
		return nil
	}

	provider := ps.providerByID(providerID)
	models = filterProviderModels(provider, models)
	if ps.speedTierEnabled {
		if err := validateProviderSpeedTierModels(providerID, models); err != nil {
			return err
		}
	}

	ps.modelsMu.Lock()
	defer ps.modelsMu.Unlock()

	next := make(map[string]providerModel, len(ps.models)+len(models))
	for publicID, model := range ps.models {
		if model.providerID == providerID {
			continue
		}
		next[publicID] = model
	}

	if err := mergeProviderModels(next, models); err != nil {
		return err
	}

	ps.models = next
	return nil
}

func mergeProviderModels(dst map[string]providerModel, models []providerModel) error {
	for _, model := range models {
		if existing, exists := dst[model.publicID]; exists {
			if existing.providerID == model.providerID {
				continue
			}
			return providerModelCollisionError(model.publicID, existing.providerID, model.providerID)
		}
		dst[model.publicID] = model
	}
	return nil
}

func (ps *providerSetup) modelsForProvider(providerID string) []providerModel {
	if ps == nil {
		return nil
	}
	ps.modelsMu.RLock()
	defer ps.modelsMu.RUnlock()

	models := make([]providerModel, 0)
	for _, model := range ps.models {
		if model.providerID == providerID {
			models = append(models, model)
		}
	}
	return models
}

func (h *ProxyHandler) initializeProviders() error {
	if len(h.providersConfig.Providers) == 0 {
		h.providersState = defaultProviderSetup(h)
		return nil
	}

	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(context.Background(), h.providersConfig, !h.deferDynamicProviderModelRefresh)
	if err != nil {
		return err
	}
	h.providersState = setup
	h.dynamicProviderValidationPending.Store(h.deferDynamicProviderModelRefresh && len(setup.providers) > 1 && hasDynamicProvider(setup.providers))
	return nil
}

func (h *ProxyHandler) buildConfiguredProviderSetup(ctx context.Context, cfg ProvidersConfig) (*providerSetup, error) {
	return h.buildConfiguredProviderSetupWithDynamicValidation(ctx, cfg, true)
}

func (h *ProxyHandler) buildConfiguredProviderSetupWithDynamicValidation(ctx context.Context, cfg ProvidersConfig, validateDynamicModels bool) (*providerSetup, error) {
	providers, providerOrder, defaultProviderID, err := h.buildProviders(cfg)
	if err != nil {
		return nil, err
	}

	setup := &providerSetup{
		providers:          providers,
		providerOrder:      providerOrder,
		defaultProviderID:  defaultProviderID,
		models:             make(map[string]providerModel),
		speedTierEnabled:   cfg.SpeedTierEnabled,
		hasConfiguredState: true,
	}

	needsDynamicModelValidation := hasDynamicProvider(providers) && (len(providers) > 1 || cfg.SpeedTierEnabled)

	if !needsDynamicModelValidation || !validateDynamicModels {
		for _, providerID := range providerOrder {
			if err := setup.addStaticProviderModels(providerID); err != nil {
				return nil, err
			}
		}
		return setup, nil
	}

	if len(providers) == 0 {
		return setup, nil
	}

	ctx, cancel := context.WithTimeout(ctx, modelsUpstreamTimeout)
	defer cancel()

	for _, providerID := range providerOrder {
		provider := providers[providerID]
		if !providerUsesDynamicModels(provider) {
			if err := setup.addStaticProviderModels(providerID); err != nil {
				return nil, err
			}
			continue
		}

		result, err := h.fetchProviderModels(ctx, provider, "", "")
		if err != nil {
			return nil, fmt.Errorf("load models for provider %q: %w", provider.id, err)
		}
		if err := setup.addProviderModels(providerID, result.models); err != nil {
			return nil, err
		}
	}

	return setup, nil
}

// ValidateDynamicProviderModels loads dynamic provider catalogs into an already
// initialized provider setup. It is safe to call after the HTTP listener is up:
// model-map updates are applied through providerSetup's locked replacement path.
func (h *ProxyHandler) ValidateDynamicProviderModels(ctx context.Context) error {
	setup := h.providerSetup()
	if setup == nil || !setup.hasConfiguredState || !hasDynamicProvider(setup.providers) || (len(setup.providers) <= 1 && !setup.speedTierEnabled) {
		h.dynamicProviderValidationPending.Store(false)
		return nil
	}

	ctx, cancel := context.WithTimeout(ctx, modelsUpstreamTimeout)
	defer cancel()

	for _, providerID := range setup.providerOrder {
		provider := setup.providerByID(providerID)
		if !providerUsesDynamicModels(provider) {
			continue
		}

		result, err := h.fetchProviderModels(ctx, provider, "", "")
		if err != nil {
			return fmt.Errorf("load models for provider %q: %w", provider.id, err)
		}
		if err := setup.replaceProviderModels(providerID, result.models); err != nil {
			return err
		}
	}

	h.dynamicProviderValidationPending.Store(false)
	return nil
}

func (h *ProxyHandler) buildProviders(cfg ProvidersConfig) (map[string]*providerRuntime, []string, string, error) {
	providers := make(map[string]*providerRuntime, len(cfg.Providers))
	providerOrder := make([]string, 0, len(cfg.Providers))
	defaultProviderID := ""
	copilotProviders := 0

	for _, raw := range cfg.Providers {
		provider, err := buildProviderRuntime(raw, h.copilotURL, h.azureIdentityTokenSourceFactory)
		if err != nil {
			return nil, nil, "", err
		}
		if _, exists := providers[provider.id]; exists {
			return nil, nil, "", fmt.Errorf("duplicate provider id %q", provider.id)
		}
		providers[provider.id] = provider
		providerOrder = append(providerOrder, provider.id)
		if provider.kind == providerTypeCopilot {
			copilotProviders++
			if copilotProviders > 1 {
				return nil, nil, "", fmt.Errorf("multiple copilot providers configured; only one copilot provider is supported")
			}
		}
		if provider.isDefault {
			if defaultProviderID != "" {
				return nil, nil, "", fmt.Errorf("multiple default providers configured: %q and %q", defaultProviderID, provider.id)
			}
			defaultProviderID = provider.id
		}
	}

	if len(providers) == 0 {
		return nil, nil, "", fmt.Errorf("providers config must include at least one provider when provided explicitly")
	}

	if defaultProviderID == "" {
		switch {
		case len(providers) == 1:
			for id := range providers {
				defaultProviderID = id
			}
		case copilotProviders == 1:
			for _, provider := range providers {
				if provider.kind == providerTypeCopilot {
					defaultProviderID = provider.id
					break
				}
			}
		default:
			return nil, nil, "", fmt.Errorf("multiple providers configured but no default provider selected")
		}
	}

	if defaultProvider := providers[defaultProviderID]; defaultProvider != nil {
		defaultProvider.isDefault = true
	}

	return providers, providerOrder, defaultProviderID, nil
}

func buildProviderRuntime(cfg ProviderConfig, defaultCopilotURL string, azureIdentityFactory azureIdentityTokenSourceFactory) (*providerRuntime, error) {
	id := strings.TrimSpace(cfg.ID)
	if id == "" {
		return nil, fmt.Errorf("provider id is required")
	}

	kind := providerType(strings.TrimSpace(cfg.Type))
	switch kind {
	case providerTypeCopilot, providerTypeAzureOpenAI, providerTypeOpenAICodex, providerTypeOpenAICompatible, providerTypeAnthropicCompatible:
	default:
		return nil, fmt.Errorf("provider %q has unsupported type %q", id, cfg.Type)
	}

	runtime := &providerRuntime{
		id:             id,
		kind:           kind,
		isDefault:      cfg.Default,
		paths:          providerEndpointPolicyFor(kind).defaultEndpointPaths(),
		modelDiscovery: providerModelDiscoveryStatic,
		includeModels:  make(map[string]struct{}, len(cfg.IncludeModels)),
		excludeModels:  make(map[string]struct{}, len(cfg.ExcludeModels)),
		staticModels:   make(map[string]providerModel, len(cfg.Models)),
		staticConfigs:  make(map[string]ProviderModelConfig, len(cfg.Models)),
	}

	for _, included := range cfg.IncludeModels {
		included = strings.TrimSpace(included)
		if included != "" {
			runtime.includeModels[included] = struct{}{}
		}
	}

	for _, excluded := range cfg.ExcludeModels {
		excluded = strings.TrimSpace(excluded)
		if excluded != "" {
			runtime.excludeModels[excluded] = struct{}{}
		}
	}

	switch kind {
	case providerTypeCopilot:
		runtime.baseURL = strings.TrimRight(defaultCopilotURL, "/")
		runtime.headerProfiles = cfg.Headers
	case providerTypeAzureOpenAI:
		baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
		if baseURL == "" {
			return nil, fmt.Errorf("provider %q must set base_url", id)
		}
		baseKind := classifyAzureBaseURL(baseURL)
		switch baseKind {
		case azureBaseURLKindOpenAIV1, azureBaseURLKindLegacyOpenAI, azureBaseURLKindResourceRoot:
		case azureBaseURLKindModels:
			return nil, fmt.Errorf("provider %q has unsupported Azure base_url %q: Microsoft Foundry /models inference endpoints are not supported; use the OpenAI-compatible endpoint ending in /openai/v1 instead", id, baseURL)
		default:
			return nil, fmt.Errorf("provider %q has unsupported Azure base_url %q: expected an absolute URL whose path ends in /openai/v1 or /openai, or is the Azure OpenAI resource root, with no query string or fragment", id, baseURL)
		}
		runtime.baseURL = baseURL
		runtime.apiVersion = strings.TrimSpace(cfg.APIVersion)
		if runtime.apiVersion == "" && baseKind != azureBaseURLKindOpenAIV1 {
			return nil, fmt.Errorf("provider %q api_version is required for Azure base_url %q unless the path ends in /openai/v1", id, baseURL)
		}

		authMode := providerAuthMode(strings.TrimSpace(cfg.AuthMode))
		if authMode == "" {
			authMode = providerAuthModeAPIKey
		}
		switch authMode {
		case providerAuthModeAPIKey:
			if strings.TrimSpace(cfg.TokenScope) != "" {
				return nil, fmt.Errorf("provider %q token_scope is only valid with auth_mode %q", id, providerAuthModeAzureIdentity)
			}
			runtime.authMode = providerAuthModeAPIKey
			runtime.apiKey = strings.TrimSpace(cfg.APIKey)
			if runtime.apiKey == "" && strings.TrimSpace(cfg.APIKeyEnv) != "" {
				runtime.apiKey = strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.APIKeyEnv)))
			}
			if runtime.apiKey == "" {
				return nil, fmt.Errorf("provider %q must set api_key or api_key_env", id)
			}
		case providerAuthModeAzureIdentity:
			if strings.TrimSpace(cfg.APIKey) != "" || strings.TrimSpace(cfg.APIKeyEnv) != "" {
				return nil, fmt.Errorf("provider %q auth_mode %q cannot be combined with api_key or api_key_env", id, providerAuthModeAzureIdentity)
			}
			tokenScope := strings.TrimSpace(cfg.TokenScope)
			if tokenScope == "" {
				tokenScope = defaultAzureIdentityTokenScope
			}
			if azureIdentityFactory == nil {
				azureIdentityFactory = newDefaultAzureIdentityTokenSource
			}
			tokenSource, err := azureIdentityFactory(id, tokenScope)
			if err != nil {
				return nil, err
			}
			runtime.authMode = providerAuthModeAzureIdentity
			runtime.tokenScope = tokenScope
			runtime.azureToken = tokenSource
		default:
			return nil, fmt.Errorf("provider %q has unsupported auth_mode %q", id, cfg.AuthMode)
		}
		if len(cfg.Models) == 0 {
			return nil, fmt.Errorf("provider %q must configure at least one model", id)
		}
		if err := addStaticProviderModels(runtime, cfg.Models, runtime.defaultStaticModelEndpoints()); err != nil {
			return nil, err
		}
	case providerTypeOpenAICodex:
		baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
		if baseURL == "" {
			baseURL = defaultOpenAICodexBaseURL
		}
		if err := validateGenericProviderBaseURL(id, "OpenAI Codex", baseURL); err != nil {
			return nil, err
		}
		codexAuth, err := newOpenAICodexAuth()
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", id, err)
		}
		runtime.baseURL = baseURL
		runtime.codexAuth = codexAuth
	case providerTypeOpenAICompatible, providerTypeAnthropicCompatible:
		baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
		if baseURL == "" {
			return nil, fmt.Errorf("provider %q must set base_url", id)
		}
		if err := validateGenericProviderBaseURL(id, string(kind), baseURL); err != nil {
			return nil, err
		}
		paths, err := configuredProviderEndpointPaths(kind, cfg)
		if err != nil {
			return nil, err
		}
		authType, authHeader, authPrefix, apiKey, err := configuredGenericProviderAuth(cfg)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", id, err)
		}
		extraHeaders, err := configuredProviderExtraHeaders(cfg.ExtraHeaders)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", id, err)
		}
		modelDiscovery, err := configuredProviderModelDiscovery(kind, cfg.ModelDiscovery)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", id, err)
		}

		runtime.baseURL = baseURL
		runtime.paths = paths
		runtime.authType = authType
		runtime.authHeader = authHeader
		runtime.authPrefix = authPrefix
		runtime.apiKey = apiKey
		runtime.extraHeaders = extraHeaders
		runtime.modelDiscovery = modelDiscovery

		if modelDiscovery == providerModelDiscoveryStatic && len(cfg.Models) == 0 {
			return nil, fmt.Errorf("provider %q must configure at least one model when model_discovery is static", id)
		}
		if err := addStaticProviderModels(runtime, cfg.Models, runtime.defaultStaticModelEndpoints()); err != nil {
			return nil, err
		}
	}

	return runtime, nil
}

func addStaticProviderModels(runtime *providerRuntime, models []ProviderModelConfig, defaultEndpoints []string) error {
	if runtime == nil {
		return fmt.Errorf("provider runtime is required")
	}
	for _, modelCfg := range models {
		model, err := buildStaticProviderModel(runtime.id, modelCfg, defaultEndpoints)
		if err != nil {
			return err
		}
		if !runtime.allowsModel(model.publicID) {
			continue
		}
		if _, exists := runtime.staticModels[model.publicID]; exists {
			return fmt.Errorf("provider %q configures model %q more than once", runtime.id, model.publicID)
		}
		runtime.staticModels[model.publicID] = model
		runtime.staticConfigs[model.publicID] = normalizeProviderModelConfig(modelCfg)
		runtime.staticOrder = append(runtime.staticOrder, model.publicID)
	}
	return nil
}

func validateProviderSpeedTierModels(providerID string, models []providerModel) error {
	modelByID := make(map[string]providerModel, len(models))
	for _, model := range models {
		if model.publicID != "" {
			modelByID[model.publicID] = model
		}
	}
	for _, model := range models {
		if model.speedTier == nil {
			continue
		}
		if model.speedTier.downgradeTo == "" {
			return fmt.Errorf("provider %q model %q speed_tier.downgrade_to is required", providerID, model.publicID)
		}
		switch model.speedTier.semantics {
		case speedTierSemanticsAll, speedTierSemanticsAny:
		default:
			return fmt.Errorf("provider %q model %q speed_tier.semantics must be %q or %q", providerID, model.publicID, speedTierSemanticsAll, speedTierSemanticsAny)
		}
		target, ok := modelByID[model.speedTier.downgradeTo]
		if !ok {
			return fmt.Errorf("provider %q model %q speed_tier.downgrade_to %q is not a known public_id in the same provider", providerID, model.publicID, model.speedTier.downgradeTo)
		}
		if target.disabled {
			return fmt.Errorf("provider %q model %q speed_tier.downgrade_to %q is disabled", providerID, model.publicID, target.publicID)
		}
		if target.speedTier != nil {
			return fmt.Errorf("provider %q model %q speed_tier.downgrade_to %q declares its own speed_tier; chained downgrades are not supported", providerID, model.publicID, target.publicID)
		}
		for _, endpoint := range model.supportedEndpoints {
			if !providerModelSupportsEndpoint(target, endpoint) {
				return fmt.Errorf("provider %q model %q speed_tier.downgrade_to %q does not support endpoint %s", providerID, model.publicID, target.publicID, endpoint)
			}
		}
	}
	return nil
}

func configuredProviderEndpointPaths(kind providerType, cfg ProviderConfig) (providerEndpointPaths, error) {
	paths := providerEndpointPolicyFor(kind).defaultEndpointPaths()

	var err error
	if paths.chatCompletions, err = normalizeProviderPath(cfg.ChatCompletionsPath, paths.chatCompletions, "chat_completions_path"); err != nil {
		return providerEndpointPaths{}, err
	}
	if paths.responses, err = normalizeProviderPath(cfg.ResponsesPath, paths.responses, "responses_path"); err != nil {
		return providerEndpointPaths{}, err
	}
	if paths.messages, err = normalizeProviderPath(cfg.MessagesPath, paths.messages, "messages_path"); err != nil {
		return providerEndpointPaths{}, err
	}
	if paths.models, err = normalizeProviderPath(cfg.ModelsPath, paths.models, "models_path"); err != nil {
		return providerEndpointPaths{}, err
	}
	return paths, nil
}

func normalizeProviderPath(configured, fallback, field string) (string, error) {
	path := strings.TrimSpace(configured)
	if path == "" {
		return fallback, nil
	}
	if !strings.HasPrefix(path, "/") || strings.ContainsAny(path, "?#") {
		return "", fmt.Errorf("%s must be an absolute path with no query string or fragment", field)
	}
	return path, nil
}

func configuredGenericProviderAuth(cfg ProviderConfig) (providerAuthType, string, string, string, error) {
	apiKey := strings.TrimSpace(cfg.APIKey)
	apiKeyEnv := strings.TrimSpace(cfg.APIKeyEnv)
	if apiKey == "" && apiKeyEnv != "" {
		apiKey = strings.TrimSpace(os.Getenv(apiKeyEnv))
		if apiKey == "" {
			return "", "", "", "", fmt.Errorf("api_key_env %q is not set or is empty", apiKeyEnv)
		}
	}

	authType := providerAuthType(strings.TrimSpace(cfg.AuthType))
	if authType == "" {
		if apiKey == "" {
			authType = providerAuthTypeNone
		} else {
			authType = providerAuthTypeBearer
		}
	}

	authHeader := strings.TrimSpace(cfg.AuthHeader)
	authPrefix := strings.TrimSpace(cfg.AuthPrefix)

	switch authType {
	case providerAuthTypeNone:
		return authType, "", "", apiKey, nil
	case providerAuthTypeBearer:
		if apiKey == "" {
			return "", "", "", "", fmt.Errorf("auth_type bearer requires api_key or api_key_env")
		}
		if authHeader == "" {
			authHeader = "Authorization"
		}
		if authPrefix == "" {
			authPrefix = "Bearer"
		}
	case providerAuthTypeAPIKeyHeader:
		if apiKey == "" {
			return "", "", "", "", fmt.Errorf("auth_type api-key-header requires api_key or api_key_env")
		}
		if authHeader == "" {
			return "", "", "", "", fmt.Errorf("auth_type api-key-header requires auth_header")
		}
	default:
		return "", "", "", "", fmt.Errorf("unsupported auth_type %q", cfg.AuthType)
	}

	if !validProviderHeaderName(authHeader) {
		return "", "", "", "", fmt.Errorf("auth_header %q is invalid", authHeader)
	}
	return authType, http.CanonicalHeaderKey(authHeader), authPrefix, apiKey, nil
}

func configuredProviderExtraHeaders(configured map[string]string) (http.Header, error) {
	if len(configured) == 0 {
		return nil, nil
	}
	headers := make(http.Header, len(configured))
	for name, value := range configured {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if !validProviderHeaderName(name) {
			return nil, fmt.Errorf("extra_headers contains invalid header %q", name)
		}
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		headers.Set(name, value)
	}
	if len(headers) == 0 {
		return nil, nil
	}
	return headers, nil
}

func validProviderHeaderName(name string) bool {
	if name == "" {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z':
		case r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9':
		case strings.ContainsRune("!#$%&'*+-.^_`|~", r):
		default:
			return false
		}
	}
	return true
}

func configuredProviderModelDiscovery(kind providerType, configured string) (providerModelDiscovery, error) {
	discovery := providerModelDiscovery(strings.TrimSpace(configured))
	if discovery == "" {
		return providerModelDiscoveryStatic, nil
	}
	switch discovery {
	case providerModelDiscoveryStatic, providerModelDiscoveryOpenAI, providerModelDiscoveryOllama, providerModelDiscoveryOpenRouterTools:
	default:
		return "", fmt.Errorf("unsupported model_discovery %q", configured)
	}
	if kind == providerTypeAnthropicCompatible && discovery == providerModelDiscoveryOllama {
		return "", fmt.Errorf("model_discovery ollama is only supported for openai-compatible providers")
	}
	return discovery, nil
}

func filterProviderModels(provider *providerRuntime, models []providerModel) []providerModel {
	if provider == nil || len(models) == 0 {
		return models
	}
	if len(provider.includeModels) == 0 && len(provider.excludeModels) == 0 {
		return models
	}

	firstFiltered := -1
	for i, model := range models {
		if !provider.allowsModel(model.publicID) {
			firstFiltered = i
			break
		}
	}
	if firstFiltered == -1 {
		return models
	}

	filtered := make([]providerModel, 0, len(models)-1)
	filtered = append(filtered, models[:firstFiltered]...)
	for _, model := range models[firstFiltered+1:] {
		if provider.allowsModel(model.publicID) {
			filtered = append(filtered, model)
		}
	}
	return filtered
}

func hasDynamicProvider(providers map[string]*providerRuntime) bool {
	for _, provider := range providers {
		if providerUsesDynamicModels(provider) {
			return true
		}
	}
	return false
}

func providerUsesDynamicModels(provider *providerRuntime) bool {
	if provider == nil {
		return false
	}
	switch provider.kind {
	case providerTypeCopilot, providerTypeOpenAICodex:
		return true
	case providerTypeOpenAICompatible, providerTypeAnthropicCompatible:
		return provider.modelDiscovery != providerModelDiscoveryStatic
	default:
		return false
	}
}

func (p *providerRuntime) allowsModel(model string) bool {
	if p == nil {
		return true
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return false
	}
	if len(p.includeModels) > 0 {
		if _, included := p.includeModels[model]; !included {
			return false
		}
	}
	if _, excluded := p.excludeModels[model]; excluded {
		return false
	}
	return true
}

func (p *providerRuntime) azureAuthMode() providerAuthMode {
	if p == nil || p.authMode == "" {
		return providerAuthModeAPIKey
	}
	return p.authMode
}

func providerModelCollisionError(publicID, existingProviderID, incomingProviderID string) error {
	return fmt.Errorf(
		"model %q is exposed by both provider %q and provider %q; resolve by adding include_models to the dynamic provider or exclude_models to one provider",
		publicID,
		existingProviderID,
		incomingProviderID,
	)
}

func buildStaticProviderModel(providerID string, cfg ProviderModelConfig, defaultEndpoints []string) (providerModel, error) {
	publicID := strings.TrimSpace(cfg.PublicID)
	if publicID == "" {
		return providerModel{}, fmt.Errorf("provider %q contains a model without public_id", providerID)
	}

	upstreamModel := strings.TrimSpace(cfg.Deployment)
	if upstreamModel == "" {
		upstreamModel = publicID
	}

	name := strings.TrimSpace(cfg.Name)
	if name == "" {
		name = publicID
	}

	endpoints, err := normalizeProviderEndpoints(cfg.Endpoints, defaultEndpoints)
	if err != nil {
		return providerModel{}, fmt.Errorf("provider %q model %q: %w", providerID, publicID, err)
	}
	raw, err := synthesizeProviderModelRaw(providerID, publicID, name, endpoints, cfg)
	if err != nil {
		return providerModel{}, err
	}

	speedTier, err := normalizeSpeedTierRule(cfg.SpeedTier)
	if err != nil {
		return providerModel{}, fmt.Errorf("provider %q model %q: %w", providerID, publicID, err)
	}

	return providerModel{
		publicID:           publicID,
		upstreamModel:      upstreamModel,
		providerID:         providerID,
		supportedEndpoints: endpoints,
		parallelToolCalls:  cloneBoolPtr(cfg.ParallelToolCalls),
		dropSamplingParams: cfg.DropSamplingParams != nil && *cfg.DropSamplingParams,
		speedTier:          speedTier,
		raw:                raw,
	}, nil
}

func normalizeProviderModelConfig(cfg ProviderModelConfig) ProviderModelConfig {
	cfg.PublicID = strings.TrimSpace(cfg.PublicID)
	cfg.Deployment = strings.TrimSpace(cfg.Deployment)
	cfg.Name = strings.TrimSpace(cfg.Name)
	cfg.ModelPickerCategory = strings.TrimSpace(cfg.ModelPickerCategory)
	if cfg.Endpoints != nil {
		cfg.Endpoints = append([]string(nil), cfg.Endpoints...)
	}
	if cfg.ReasoningEffort != nil {
		cfg.ReasoningEffort = append([]string(nil), cfg.ReasoningEffort...)
	}
	if cfg.SpeedTier != nil {
		cloned := *cfg.SpeedTier
		cloned.When = normalizeSpeedTierWhen(cloned.When)
		cloned.NeverWhen = normalizeSpeedTierNeverWhen(cloned.NeverWhen)
		cfg.SpeedTier = &cloned
	}
	return cfg
}

func normalizeProviderEndpoints(endpoints []string, defaultEndpoints []string) ([]string, error) {
	if len(endpoints) == 0 {
		return append([]string(nil), defaultEndpoints...), nil
	}

	normalized := make([]string, 0, len(endpoints))
	seen := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint == "" {
			continue
		}
		if !knownProviderEndpoint(endpoint) {
			return nil, fmt.Errorf("unsupported endpoint %q", endpoint)
		}
		if _, ok := seen[endpoint]; ok {
			continue
		}
		seen[endpoint] = struct{}{}
		normalized = append(normalized, endpoint)
	}
	if len(normalized) == 0 {
		return append([]string(nil), defaultEndpoints...), nil
	}
	return normalized, nil
}

func knownProviderEndpoint(endpoint string) bool {
	switch endpoint {
	case providerEndpointChatCompletions, providerEndpointResponses, providerEndpointMessages:
		return true
	default:
		return false
	}
}

func cloneBoolPtr(value *bool) *bool {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func synthesizeProviderModelRaw(providerID, publicID, name string, endpoints []string, cfg ProviderModelConfig) (json.RawMessage, error) {
	type limits struct {
		MaxContextWindowTokens int64 `json:"max_context_window_tokens,omitempty"`
	}
	type supports struct {
		ParallelToolCalls bool     `json:"parallel_tool_calls"`
		ReasoningEffort   []string `json:"reasoning_effort,omitempty"`
		Vision            bool     `json:"vision"`
	}
	type capabilities struct {
		Limits   limits   `json:"limits,omitempty"`
		Supports supports `json:"supports,omitempty"`
	}

	modelPickerEnabled := true
	if cfg.ModelPickerEnabled != nil {
		modelPickerEnabled = *cfg.ModelPickerEnabled
	}

	parallelToolCalls := false
	if cfg.ParallelToolCalls != nil {
		parallelToolCalls = *cfg.ParallelToolCalls
	}

	vision := false
	if cfg.Vision != nil {
		vision = *cfg.Vision
	}

	contextWindow := int64(0)
	if cfg.ContextWindow != nil {
		contextWindow = *cfg.ContextWindow
	}

	category := strings.TrimSpace(cfg.ModelPickerCategory)
	if category == "" {
		category = "versatile"
	}

	payload := map[string]interface{}{
		"id":                  publicID,
		"object":              "model",
		"created":             0,
		"owned_by":            providerID,
		"name":                name,
		"supported_endpoints": endpoints,
		"capabilities": capabilities{
			Limits: limits{
				MaxContextWindowTokens: contextWindow,
			},
			Supports: supports{
				ParallelToolCalls: parallelToolCalls,
				ReasoningEffort:   append([]string(nil), cfg.ReasoningEffort...),
				Vision:            vision,
			},
		},
		"model_picker_enabled":  modelPickerEnabled,
		"model_picker_category": category,
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal model %q for provider %q: %w", publicID, providerID, err)
	}
	return raw, nil
}

func (h *ProxyHandler) resolveProviderModel(model, endpoint string) (*providerRuntime, providerModel, bool) {
	setup := h.providerSetup()
	model = strings.TrimSpace(model)
	if model != "" {
		if providerModel, ok := setup.lookupModel(model); ok {
			provider := setup.providerByID(providerModel.providerID)
			if provider != nil {
				return provider, providerModel, true
			}
		}
	}

	defaultProvider := setup.defaultProvider()
	if defaultProvider == nil {
		return nil, providerModel{}, false
	}
	return defaultProvider, providerModel{
		publicID:           model,
		upstreamModel:      model,
		providerID:         defaultProvider.id,
		supportedEndpoints: nil,
	}, false
}

func providerModelSupportsEndpoint(model providerModel, endpoint string) bool {
	if len(model.supportedEndpoints) == 0 {
		return true
	}
	return supportsEndpoint(model.supportedEndpoints, endpoint)
}

func rewriteRequestModelForProvider(body []byte, upstreamModel string) ([]byte, bool, error) {
	upstreamModel = strings.TrimSpace(upstreamModel)
	if upstreamModel == "" {
		return body, false, nil
	}

	current := extractResponsesRequestModel(body)
	if current == "" {
		var payload struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			return body, false, nil
		}
		current = strings.TrimSpace(payload.Model)
	}

	if current == "" || current == upstreamModel {
		return body, false, nil
	}
	return rewriteResponsesRequestModel(body, upstreamModel)
}

func (h *ProxyHandler) providerRequestURL(provider *providerRuntime, path string, extraQuery string, owners ...providerModel) (string, error) {
	if provider == nil {
		return "", fmt.Errorf("provider is required")
	}

	baseURL := strings.TrimRight(provider.baseURL, "/")
	upstreamPath := provider.upstreamPath(path)
	if upstreamPath == "" {
		return "", fmt.Errorf("provider %q has no upstream path configured for %s", provider.id, path)
	}

	baseKind := classifyAzureBaseURL(baseURL)
	if provider.kind == providerTypeAzureOpenAI && provider.apiVersion != "" && baseKind != azureBaseURLKindOpenAIV1 {
		if path == providerEndpointChatCompletions && !strings.HasPrefix(strings.TrimPrefix(upstreamPath, "/"), "deployments/") {
			owner := providerModel{}
			if len(owners) > 0 {
				owner = owners[0]
			}
			deployment := strings.TrimSpace(owner.upstreamModel)
			if deployment == "" {
				deployment = strings.TrimSpace(owner.publicID)
			}
			if deployment == "" {
				return "", fmt.Errorf("provider %q has no Azure deployment configured for %s", provider.id, path)
			}
			operation := strings.TrimPrefix(upstreamPath, "/")
			fullURL := azureClassicOpenAIBaseURL(baseURL, baseKind) + "/deployments/" + url.PathEscape(deployment) + "/" + operation
			return appendRawQuery(fullURL, appendQuery("api-version="+url.QueryEscape(provider.apiVersion), extraQuery)), nil
		}
		fullURL := azureClassicOpenAIBaseURL(baseURL, baseKind) + upstreamPath
		return appendRawQuery(fullURL, appendQuery("api-version="+url.QueryEscape(provider.apiVersion), extraQuery)), nil
	}

	fullURL := baseURL + upstreamPath
	if provider.kind != providerTypeAzureOpenAI || provider.apiVersion == "" || baseKind == azureBaseURLKindOpenAIV1 {
		return appendRawQuery(fullURL, extraQuery), nil
	}
	return appendRawQuery(fullURL, appendQuery("api-version="+url.QueryEscape(provider.apiVersion), extraQuery)), nil
}

func azureClassicOpenAIBaseURL(baseURL string, baseKind azureBaseURLKind) string {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseKind == azureBaseURLKindResourceRoot {
		return baseURL + "/openai"
	}
	return baseURL
}

func providerUsesAzureClassicDeploymentPath(provider *providerRuntime, endpoint string) bool {
	if provider == nil || provider.kind != providerTypeAzureOpenAI || endpoint != providerEndpointChatCompletions {
		return false
	}
	baseKind := classifyAzureBaseURL(provider.baseURL)
	return baseKind == azureBaseURLKindLegacyOpenAI || baseKind == azureBaseURLKindResourceRoot
}

func (p *providerRuntime) upstreamPath(endpoint string) string {
	if p == nil {
		return endpoint
	}
	switch strings.TrimSpace(endpoint) {
	case providerEndpointChatCompletions:
		if p.paths.chatCompletions != "" {
			return p.paths.chatCompletions
		}
	case providerEndpointResponses:
		if p.paths.responses != "" {
			return p.paths.responses
		}
	case providerEndpointMessages:
		if p.paths.messages != "" {
			return p.paths.messages
		}
	case providerEndpointMessagesCount:
		if p.paths.messages != "" {
			return strings.TrimRight(p.paths.messages, "/") + "/count_tokens"
		}
	case providerEndpointModels:
		if p.paths.models != "" {
			return p.paths.models
		}
	}
	return endpoint
}

func classifyAzureBaseURL(baseURL string) azureBaseURLKind {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if trimmed == "" {
		return azureBaseURLKindInvalid
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return azureBaseURLKindInvalid
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(trimmed, "#") {
		return azureBaseURLKindInvalid
	}

	path := strings.TrimRight(parsed.Path, "/")
	switch {
	case path == "":
		return azureBaseURLKindResourceRoot
	case strings.HasSuffix(path, "/openai/v1"):
		return azureBaseURLKindOpenAIV1
	case strings.HasSuffix(path, "/openai"):
		return azureBaseURLKindLegacyOpenAI
	case strings.HasSuffix(path, "/models"):
		return azureBaseURLKindModels
	default:
		return azureBaseURLKindInvalid
	}
}

func validateGenericProviderBaseURL(providerID, label, baseURL string) error {
	trimmed := strings.TrimRight(strings.TrimSpace(baseURL), "/")
	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("provider %q has unsupported %s base_url %q: expected an absolute URL with no query string or fragment", providerID, label, baseURL)
	}
	if parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || strings.Contains(trimmed, "#") {
		return fmt.Errorf("provider %q has unsupported %s base_url %q: expected an absolute URL with no query string or fragment", providerID, label, baseURL)
	}
	return nil
}

func appendQuery(parts ...string) string {
	combined := ""
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if combined == "" {
			combined = part
			continue
		}
		combined += "&" + part
	}
	return combined
}

func appendRawQuery(rawURL, rawQuery string) string {
	rawQuery = strings.TrimSpace(strings.TrimPrefix(rawQuery, "?"))
	if rawQuery == "" {
		return rawURL
	}
	separator := "?"
	if strings.Contains(rawURL, "?") {
		separator = "&"
	}
	return rawURL + separator + rawQuery
}

func (h *ProxyHandler) applyProviderHeaders(req *http.Request, provider *providerRuntime, endpoint string) error {
	if provider == nil {
		return &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("provider is required")}
	}

	switch provider.kind {
	case providerTypeCopilot:
		token, err := h.auth.GetToken(req.Context())
		if err != nil {
			return &providerRequestError{statusCode: http.StatusInternalServerError, err: err}
		}
		h.setCopilotHeadersForProvider(req, token, provider, endpoint)
	case providerTypeAzureOpenAI:
		clearCopilotHeaders(req.Header)
		mergeHeaderValues(req.Header, provider.extraHeaders)
		req.Header.Del("api-key")
		switch provider.azureAuthMode() {
		case providerAuthModeAPIKey:
			req.Header.Set("api-key", provider.apiKey)
		case providerAuthModeAzureIdentity:
			if provider.azureToken == nil {
				return &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("provider %q has no Azure identity token source configured", provider.id)}
			}
			token, err := provider.azureToken.AccessToken(req.Context())
			if err != nil {
				return &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("provider %q Azure identity auth failed: %w", provider.id, err)}
			}
			req.Header.Set("Authorization", "Bearer "+token)
		default:
			return &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("provider %q has unsupported auth mode %q", provider.id, provider.authMode)}
		}
		req.Header.Set("Content-Type", "application/json")
	case providerTypeOpenAICodex:
		clearCopilotHeaders(req.Header)
		if provider.codexAuth == nil {
			return &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("provider %q has no OpenAI Codex auth configured", provider.id)}
		}
		credentials, err := provider.codexAuth.credentials(req.Context(), h.client)
		if err != nil {
			return &providerRequestError{statusCode: http.StatusInternalServerError, err: err}
		}
		req.Header.Set("Authorization", "Bearer "+credentials.accessToken)
		if credentials.accountID != "" {
			req.Header.Set("ChatGPT-Account-ID", credentials.accountID)
		}
		if credentials.fedRAMP {
			req.Header.Set("X-OpenAI-Fedramp", "true")
		}
		req.Header.Set("Content-Type", "application/json")
	case providerTypeOpenAICompatible, providerTypeAnthropicCompatible:
		clearCopilotHeaders(req.Header)
		mergeHeaderValues(req.Header, provider.extraHeaders)
		if err := applyGenericProviderAuth(req, provider); err != nil {
			return err
		}
		if req.Method != http.MethodGet {
			req.Header.Set("Content-Type", "application/json")
		}
	default:
		return &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("unsupported provider type %q", provider.kind)}
	}
	return nil
}

func applyGenericProviderAuth(req *http.Request, provider *providerRuntime) error {
	switch provider.authType {
	case providerAuthTypeNone, "":
		return nil
	case providerAuthTypeBearer, providerAuthTypeAPIKeyHeader:
		if strings.TrimSpace(provider.apiKey) == "" {
			return &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("provider %q has no API key configured", provider.id)}
		}
		header := strings.TrimSpace(provider.authHeader)
		if header == "" {
			return &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("provider %q has no auth header configured", provider.id)}
		}
		value := provider.apiKey
		if prefix := strings.TrimSpace(provider.authPrefix); prefix != "" {
			value = prefix + " " + value
		}
		req.Header.Set(header, value)
		return nil
	default:
		return &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("unsupported auth type %q", provider.authType)}
	}
}

func (h *ProxyHandler) newProviderJSONRequest(ctx context.Context, provider *providerRuntime, method, path string, body []byte, extraHeaders http.Header, extraQuery string, owners ...providerModel) (*http.Request, error) {
	fullURL, err := h.providerRequestURL(provider, path, extraQuery, owners...)
	if err != nil {
		return nil, err
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, bodyReader)
	if err != nil {
		return nil, err
	}
	if len(extraHeaders) > 0 {
		mergeHeaderValues(req.Header, extraHeaders)
	}
	if err := h.applyProviderHeaders(req, provider, path); err != nil {
		return nil, err
	}
	return req, nil
}

func (h *ProxyHandler) fetchProviderModels(ctx context.Context, provider *providerRuntime, rawQuery, ifNoneMatch string) (providerModelsFetchResult, error) {
	if provider == nil {
		return providerModelsFetchResult{}, fmt.Errorf("provider is required")
	}

	switch provider.kind {
	case providerTypeAzureOpenAI:
		models := orderedStaticProviderModels(provider)

		// Azure /models is only a best-effort metadata overlay for the configured
		// static catalog. Routing still comes from provider.models[], and sparse
		// or failed Azure metadata probes should leave the configured model list untouched.
		resp, err := h.doWithRetry(func() (*http.Request, error) {
			return h.newProviderJSONRequest(ctx, provider, http.MethodGet, "/models", nil, nil, "")
		})
		if err != nil {
			return providerModelsFetchResult{models: models}, nil
		}
		defer drainAndClose(resp.Body)
		if resp.StatusCode != http.StatusOK {
			return providerModelsFetchResult{models: models}, nil
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return providerModelsFetchResult{models: models}, nil
		}

		overlayModels, err := decodeProviderModelsFromBody(provider, body)
		if err != nil {
			return providerModelsFetchResult{models: models}, nil
		}

		overlayByID := make(map[string]providerModel, len(overlayModels))
		for _, overlay := range overlayModels {
			overlayByID[overlay.publicID] = overlay
		}
		for i, staticModel := range models {
			cfg, ok := provider.staticConfigs[staticModel.publicID]
			if !ok {
				continue
			}
			overlay, ok := findProviderModelMetadataOverlay(cfg, overlayByID)
			if !ok {
				continue
			}
			models[i] = mergeStaticProviderMetadata(staticModel, cfg, overlay)
		}

		return providerModelsFetchResult{models: models}, nil
	case providerTypeCopilot:
		resp, err := h.doWithRetry(func() (*http.Request, error) {
			req, err := h.newProviderJSONRequest(ctx, provider, http.MethodGet, "/models", nil, nil, rawQuery)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(ifNoneMatch) != "" {
				req.Header.Set("If-None-Match", ifNoneMatch)
			}
			return req, nil
		})
		if err != nil {
			return providerModelsFetchResult{}, err
		}
		defer func() { _ = resp.Body.Close() }()

		result := providerModelsFetchResult{etag: resp.Header.Get("ETag")}
		if resp.StatusCode == http.StatusNotModified {
			result.notModified = true
			return result, nil
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return providerModelsFetchResult{}, &providerRequestError{
				statusCode: resp.StatusCode,
				err:        fmt.Errorf("unexpected /models status %d: %s", resp.StatusCode, string(body)),
			}
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return providerModelsFetchResult{}, err
		}

		models, err := decodeProviderModelsFromBody(provider, body)
		if err != nil {
			return providerModelsFetchResult{}, err
		}
		result.models = models
		return result, nil
	case providerTypeOpenAICodex:
		modelsQuery := openAICodexModelsRawQuery(rawQuery)
		resp, err := h.doWithRetry(func() (*http.Request, error) {
			req, err := h.newProviderJSONRequest(ctx, provider, http.MethodGet, "/models", nil, nil, modelsQuery)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(ifNoneMatch) != "" {
				req.Header.Set("If-None-Match", ifNoneMatch)
			}
			return req, nil
		})
		if err != nil {
			return providerModelsFetchResult{}, err
		}
		defer func() { _ = resp.Body.Close() }()

		result := providerModelsFetchResult{etag: resp.Header.Get("ETag")}
		if resp.StatusCode == http.StatusNotModified {
			result.notModified = true
			return result, nil
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return providerModelsFetchResult{}, &providerRequestError{
				statusCode: resp.StatusCode,
				err:        fmt.Errorf("unexpected /models status %d: %s", resp.StatusCode, string(body)),
			}
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return providerModelsFetchResult{}, err
		}

		models, err := decodeOpenAICodexModelsFromBody(provider, body)
		if err != nil {
			return providerModelsFetchResult{}, err
		}
		result.models = models
		return result, nil
	case providerTypeOpenAICompatible, providerTypeAnthropicCompatible:
		if provider.modelDiscovery == providerModelDiscoveryStatic {
			return providerModelsFetchResult{models: orderedStaticProviderModels(provider)}, nil
		}

		resp, err := h.doWithRetry(func() (*http.Request, error) {
			req, err := h.newProviderJSONRequest(ctx, provider, http.MethodGet, providerEndpointModels, nil, nil, rawQuery)
			if err != nil {
				return nil, err
			}
			if strings.TrimSpace(ifNoneMatch) != "" {
				req.Header.Set("If-None-Match", ifNoneMatch)
			}
			return req, nil
		})
		if err != nil {
			return providerModelsFetchResult{}, err
		}
		defer func() { _ = resp.Body.Close() }()

		result := providerModelsFetchResult{etag: resp.Header.Get("ETag")}
		if resp.StatusCode == http.StatusNotModified {
			result.notModified = true
			return result, nil
		}
		if resp.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			return providerModelsFetchResult{}, &providerRequestError{
				statusCode: resp.StatusCode,
				err:        fmt.Errorf("unexpected /models status %d: %s", resp.StatusCode, string(body)),
			}
		}

		body, err := io.ReadAll(resp.Body)
		if err != nil {
			return providerModelsFetchResult{}, err
		}

		var models []providerModel
		switch provider.modelDiscovery {
		case providerModelDiscoveryOpenAI, providerModelDiscoveryOpenRouterTools:
			models, err = decodeProviderModelsFromBody(provider, body)
		case providerModelDiscoveryOllama:
			models, err = decodeOllamaModelsFromBody(provider, body)
		default:
			err = fmt.Errorf("unsupported model discovery %q", provider.modelDiscovery)
		}
		if err != nil {
			return providerModelsFetchResult{}, err
		}
		result.models = mergeDiscoveredProviderModelsWithStaticConfig(provider, models)
		return result, nil
	default:
		return providerModelsFetchResult{}, fmt.Errorf("unsupported provider type %q", provider.kind)
	}
}

func orderedStaticProviderModels(provider *providerRuntime) []providerModel {
	if provider == nil {
		return nil
	}
	models := make([]providerModel, 0, len(provider.staticModels))
	for _, publicID := range provider.staticOrder {
		model, ok := provider.staticModels[publicID]
		if ok {
			models = append(models, model)
		}
	}
	return models
}

func findProviderModelMetadataOverlay(cfg ProviderModelConfig, overlayByID map[string]providerModel) (providerModel, bool) {
	publicID := strings.TrimSpace(cfg.PublicID)
	if publicID != "" {
		if overlay, ok := overlayByID[publicID]; ok {
			return overlay, true
		}
	}

	deployment := strings.TrimSpace(cfg.Deployment)
	if deployment != "" && deployment != publicID {
		if overlay, ok := overlayByID[deployment]; ok {
			return overlay, true
		}
	}

	return providerModel{}, false
}

func mergeStaticProviderMetadata(static providerModel, cfg ProviderModelConfig, overlay providerModel) providerModel {
	mergedRaw, err := mergeProviderModelMetadataOverlayRaw(static.raw, overlay.raw, cfg)
	if err != nil {
		return static
	}
	static.raw = mergedRaw
	return static
}

// mergeProviderModelMetadataOverlayRaw opportunistically copies provider metadata
// that already exists in the Azure /models overlay payload. It does not rewrite
// configured public IDs or endpoint allowlists, and it does not synthesize
// Codex-facing fields that an upstream provider omitted.
func mergeProviderModelMetadataOverlayRaw(baseRaw, overlayRaw json.RawMessage, cfg ProviderModelConfig) (json.RawMessage, error) {
	if len(baseRaw) == 0 || len(overlayRaw) == 0 {
		return append(json.RawMessage(nil), baseRaw...), nil
	}

	base, err := decodeRawJSONObject(baseRaw)
	if err != nil {
		return nil, err
	}
	overlay, err := decodeRawJSONObject(overlayRaw)
	if err != nil {
		return nil, err
	}

	for key, value := range overlay {
		if _, exists := base[key]; !exists {
			base[key] = append(json.RawMessage(nil), value...)
		}
	}

	if strings.TrimSpace(cfg.Name) == "" {
		copyRawField(base, overlay, "name")
	}
	if cfg.ModelPickerEnabled == nil {
		copyRawField(base, overlay, "model_picker_enabled")
	}
	if strings.TrimSpace(cfg.ModelPickerCategory) == "" {
		copyRawField(base, overlay, "model_picker_category")
	}

	if err := mergeProviderModelCapabilitiesOverlay(base, overlay, cfg); err != nil {
		return nil, err
	}

	merged, err := json.Marshal(base)
	if err != nil {
		return nil, err
	}
	return merged, nil
}

func mergeProviderModelCapabilitiesOverlay(base, overlay map[string]json.RawMessage, cfg ProviderModelConfig) error {
	baseCaps, err := decodeOptionalRawJSONObject(base["capabilities"])
	if err != nil {
		return err
	}
	overlayCaps, err := decodeOptionalRawJSONObject(overlay["capabilities"])
	if err != nil {
		return err
	}

	baseSupports, err := decodeOptionalRawJSONObject(baseCaps["supports"])
	if err != nil {
		return err
	}
	overlaySupports, err := decodeOptionalRawJSONObject(overlayCaps["supports"])
	if err != nil {
		return err
	}

	if cfg.ReasoningEffort == nil {
		copyRawField(baseSupports, overlaySupports, "reasoning_effort")
	}
	if cfg.ParallelToolCalls == nil {
		copyRawField(baseSupports, overlaySupports, "parallel_tool_calls")
	}
	if cfg.Vision == nil {
		copyRawField(baseSupports, overlaySupports, "vision")
	}

	if len(baseSupports) > 0 {
		encoded, err := json.Marshal(baseSupports)
		if err != nil {
			return err
		}
		baseCaps["supports"] = encoded
	}

	baseLimits, err := decodeOptionalRawJSONObject(baseCaps["limits"])
	if err != nil {
		return err
	}
	overlayLimits, err := decodeOptionalRawJSONObject(overlayCaps["limits"])
	if err != nil {
		return err
	}

	if cfg.ContextWindow == nil {
		copyRawField(baseLimits, overlayLimits, "max_context_window_tokens")
	}

	if len(baseLimits) > 0 {
		encoded, err := json.Marshal(baseLimits)
		if err != nil {
			return err
		}
		baseCaps["limits"] = encoded
	}

	if len(baseCaps) > 0 {
		encoded, err := json.Marshal(baseCaps)
		if err != nil {
			return err
		}
		base["capabilities"] = encoded
	}

	return nil
}

func decodeRawJSONObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return nil, err
	}
	if payload == nil {
		payload = map[string]json.RawMessage{}
	}
	return payload, nil
}

func decodeOptionalRawJSONObject(raw json.RawMessage) (map[string]json.RawMessage, error) {
	if len(raw) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	return decodeRawJSONObject(raw)
}

func copyRawField(dst, src map[string]json.RawMessage, field string) {
	if dst == nil || src == nil {
		return
	}
	value, ok := src[field]
	if !ok {
		return
	}
	dst[field] = append(json.RawMessage(nil), value...)
}

func decodeProviderModelsFromBody(provider *providerRuntime, body []byte) ([]providerModel, error) {
	if provider == nil {
		return nil, fmt.Errorf("provider is required")
	}

	var upstream struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &upstream); err != nil {
		return nil, err
	}

	models := make([]providerModel, 0, len(upstream.Data))
	indexByID := make(map[string]int, len(upstream.Data))
	for _, raw := range upstream.Data {
		var parsed struct {
			ID                 string   `json:"id"`
			Name               string   `json:"name"`
			SupportedEndpoints []string `json:"supported_endpoints"`
			SupportedParams    []string `json:"supported_parameters"`
			ContextLength      int64    `json:"context_length"`
			Policy             struct {
				State string `json:"state"`
			} `json:"policy"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			continue
		}
		publicID := strings.TrimSpace(parsed.ID)
		if publicID == "" {
			continue
		}
		if !provider.allowsModel(publicID) {
			continue
		}
		if provider.modelDiscovery == providerModelDiscoveryOpenRouterTools && !openRouterModelSupportsTools(parsed.SupportedParams) {
			continue
		}

		supportedEndpoints := normalizeDynamicProviderEndpoints(provider, parsed.SupportedEndpoints)
		if len(supportedEndpoints) == 0 {
			supportedEndpoints = provider.defaultDynamicModelEndpoints()
		}
		disabled := strings.EqualFold(parsed.Policy.State, "disabled")
		if index, duplicate := indexByID[publicID]; duplicate {
			merged := models[index]
			merged.supportedEndpoints = mergeDynamicProviderEndpoints(merged.supportedEndpoints, supportedEndpoints)
			merged.disabled = merged.disabled && disabled
			baseRaw := merged.raw
			if merged.disabled != models[index].disabled && !merged.disabled {
				baseRaw = raw
			}
			merged.raw = mergeProviderModelRaw(baseRaw, merged.supportedEndpoints)
			models[index] = merged
			continue
		}

		indexByID[publicID] = len(models)
		models = append(models, providerModel{
			publicID:           publicID,
			upstreamModel:      publicID,
			providerID:         provider.id,
			supportedEndpoints: supportedEndpoints,
			parallelToolCalls:  providerModelParallelToolCallsFromRaw(raw),
			disabled:           disabled,
			raw:                mergeProviderModelRaw(raw, supportedEndpoints),
		})
	}

	return models, nil
}

func providerModelParallelToolCallsFromRaw(raw json.RawMessage) *bool {
	var parsed struct {
		Capabilities struct {
			Supports struct {
				ParallelToolCalls *bool `json:"parallel_tool_calls"`
			} `json:"supports"`
		} `json:"capabilities"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return nil
	}
	return cloneBoolPtr(parsed.Capabilities.Supports.ParallelToolCalls)
}

func openRouterModelSupportsTools(supportedParams []string) bool {
	for _, param := range supportedParams {
		switch strings.TrimSpace(param) {
		case "tools", "tool_choice":
			return true
		}
	}
	return false
}

func mergeDiscoveredProviderModelsWithStaticConfig(provider *providerRuntime, discovered []providerModel) []providerModel {
	if provider == nil || len(discovered) == 0 || len(provider.staticConfigs) == 0 {
		return discovered
	}

	merged := make([]providerModel, 0, len(discovered))
	for _, model := range discovered {
		configs := staticConfigsForDiscoveredProviderModel(provider, model)
		if len(configs) == 0 {
			merged = append(merged, model)
			continue
		}

		for _, cfg := range configs {
			staticModel, err := buildStaticProviderModel(provider.id, cfg, provider.defaultStaticModelEndpoints())
			if err != nil {
				continue
			}
			staticModel.disabled = model.disabled
			staticModel.raw, err = mergeProviderModelMetadataOverlayRaw(staticModel.raw, model.raw, cfg)
			if err != nil {
				staticModel.raw = mergeProviderModelRaw(staticModel.raw, staticModel.supportedEndpoints)
			}
			merged = append(merged, staticModel)
		}
	}
	return merged
}

func staticConfigsForDiscoveredProviderModel(provider *providerRuntime, model providerModel) []ProviderModelConfig {
	if provider == nil || len(provider.staticOrder) == 0 {
		return nil
	}

	configs := make([]ProviderModelConfig, 0, 1)
	for _, publicID := range provider.staticOrder {
		cfg, ok := provider.staticConfigs[publicID]
		if !ok {
			continue
		}
		if publicID == model.publicID || strings.TrimSpace(cfg.Deployment) == model.publicID {
			configs = append(configs, cfg)
		}
	}
	return configs
}

func decodeOllamaModelsFromBody(provider *providerRuntime, body []byte) ([]providerModel, error) {
	if provider == nil {
		return nil, fmt.Errorf("provider is required")
	}

	var upstream struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &upstream); err != nil {
		return nil, err
	}

	defaultEndpoints := provider.defaultDynamicModelEndpoints()
	models := make([]providerModel, 0, len(upstream.Models))
	seen := make(map[string]struct{}, len(upstream.Models))
	for _, raw := range upstream.Models {
		var parsed struct {
			Name  string `json:"name"`
			Model string `json:"model"`
		}
		if err := json.Unmarshal(raw, &parsed); err != nil {
			continue
		}
		publicID := strings.TrimSpace(parsed.Name)
		if publicID == "" {
			publicID = strings.TrimSpace(parsed.Model)
		}
		if publicID == "" {
			continue
		}
		if _, duplicate := seen[publicID]; duplicate {
			continue
		}
		if !provider.allowsModel(publicID) {
			continue
		}

		cfg := ProviderModelConfig{
			PublicID:  publicID,
			Name:      publicID,
			Endpoints: defaultEndpoints,
		}
		modelRaw, err := synthesizeProviderModelRaw(provider.id, publicID, publicID, defaultEndpoints, cfg)
		if err != nil {
			return nil, err
		}

		seen[publicID] = struct{}{}
		models = append(models, providerModel{
			publicID:           publicID,
			upstreamModel:      publicID,
			providerID:         provider.id,
			supportedEndpoints: append([]string(nil), defaultEndpoints...),
			raw:                modelRaw,
		})
	}

	return models, nil
}

func decodeOpenAICodexModelsFromBody(provider *providerRuntime, body []byte) ([]providerModel, error) {
	if provider == nil {
		return nil, fmt.Errorf("provider is required")
	}

	var upstream struct {
		Models []json.RawMessage `json:"models"`
	}
	if err := json.Unmarshal(body, &upstream); err != nil {
		return nil, err
	}

	models := make([]providerModel, 0, len(upstream.Models))
	seen := make(map[string]struct{}, len(upstream.Models))
	for _, raw := range upstream.Models {
		var parsed openAICodexModelPayload
		if err := json.Unmarshal(raw, &parsed); err != nil {
			continue
		}

		publicID := strings.TrimSpace(parsed.Slug)
		if publicID == "" {
			continue
		}
		if _, duplicate := seen[publicID]; duplicate {
			continue
		}
		visibilityList := strings.EqualFold(strings.TrimSpace(parsed.Visibility), "list")
		if !parsed.SupportedInAPI || !visibilityList {
			continue
		}
		if !provider.allowsModel(publicID) {
			continue
		}

		modelRaw, err := synthesizeOpenAICodexModelRaw(provider.id, parsed)
		if err != nil {
			return nil, err
		}

		seen[publicID] = struct{}{}
		models = append(models, providerModel{
			publicID:           publicID,
			upstreamModel:      publicID,
			providerID:         provider.id,
			supportedEndpoints: append([]string(nil), openAICodexProviderEndpoints...),
			parallelToolCalls:  cloneBoolPtr(&parsed.SupportsParallelToolCalls),
			raw:                modelRaw,
		})
	}

	return models, nil
}

func synthesizeOpenAICodexModelRaw(providerID string, parsed openAICodexModelPayload) (json.RawMessage, error) {
	reasoningEffort := make([]string, 0, len(parsed.SupportedReasoningLevels))
	for _, level := range parsed.SupportedReasoningLevels {
		effort := strings.TrimSpace(level.Effort)
		if effort != "" {
			reasoningEffort = append(reasoningEffort, effort)
		}
	}

	name := strings.TrimSpace(parsed.DisplayName)
	if name == "" {
		name = strings.TrimSpace(parsed.Slug)
	}

	vision := parsed.SupportsImageDetailOriginal
	for _, modality := range parsed.InputModalities {
		if strings.EqualFold(strings.TrimSpace(modality), "image") {
			vision = true
			break
		}
	}

	modelPickerCategory := "versatile"
	switch {
	case parsed.Priority <= 0:
		modelPickerCategory = "powerful"
	case parsed.Priority >= 8:
		modelPickerCategory = "lightweight"
	}

	var maxContextWindowTokens int64
	switch {
	case parsed.MaxContextWindow != nil && *parsed.MaxContextWindow > 0:
		maxContextWindowTokens = *parsed.MaxContextWindow
	case parsed.ContextWindow != nil && *parsed.ContextWindow > 0:
		maxContextWindowTokens = *parsed.ContextWindow
	}

	payload := map[string]interface{}{
		"id":                    parsed.Slug,
		"object":                "model",
		"created":               0,
		"owned_by":              providerID,
		"name":                  name,
		"supported_endpoints":   openAICodexProviderEndpoints,
		"model_picker_enabled":  parsed.SupportedInAPI && strings.EqualFold(strings.TrimSpace(parsed.Visibility), "list"),
		"model_picker_category": modelPickerCategory,
		"capabilities": map[string]interface{}{
			"limits": map[string]interface{}{
				"max_context_window_tokens": maxContextWindowTokens,
			},
			"supports": map[string]interface{}{
				"parallel_tool_calls": parsed.SupportsParallelToolCalls,
				"reasoning_effort":    reasoningEffort,
				"vision":              vision,
			},
		},
		"slug":                           parsed.Slug,
		"display_name":                   name,
		"description":                    parsed.Description,
		"visibility":                     parsed.Visibility,
		"supported_in_api":               parsed.SupportedInAPI,
		"priority":                       parsed.Priority,
		"supports_reasoning_summaries":   parsed.SupportsReasoningSummaries,
		"support_verbosity":              parsed.SupportVerbosity,
		"supports_parallel_tool_calls":   parsed.SupportsParallelToolCalls,
		"supports_image_detail_original": parsed.SupportsImageDetailOriginal,
		"input_modalities":               parsed.InputModalities,
		"experimental_supported_tools":   parsed.ExperimentalSupportedTools,
		"base_instructions":              parsed.BaseInstructions,
		"shell_type":                     parsed.ShellType,
		"default_reasoning_level":        strings.TrimSpace(parsed.DefaultReasoningLevel),
	}
	if parsed.ContextWindow != nil {
		payload["context_window"] = *parsed.ContextWindow
	}
	if parsed.MaxContextWindow != nil {
		payload["max_context_window"] = *parsed.MaxContextWindow
	}
	if parsed.AutoCompactTokenLimit != nil {
		payload["auto_compact_token_limit"] = *parsed.AutoCompactTokenLimit
	}
	if parsed.EffectiveContextWindowPct > 0 {
		payload["effective_context_window_percent"] = parsed.EffectiveContextWindowPct
	}

	raw, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal OpenAI Codex model %q for provider %q: %w", parsed.Slug, providerID, err)
	}
	return raw, nil
}

func openAICodexModelsRawQuery(rawQuery string) string {
	rawQuery = strings.TrimSpace(strings.TrimPrefix(rawQuery, "?"))
	if rawQueryHasParam(rawQuery, "client_version") {
		return rawQuery
	}
	return appendQuery(rawQuery, "client_version="+url.QueryEscape(defaultOpenAICodexClientVersion))
}

func rawQueryHasParam(rawQuery, name string) bool {
	rawQuery = strings.TrimSpace(strings.TrimPrefix(rawQuery, "?"))
	if rawQuery == "" {
		return false
	}
	values, err := url.ParseQuery(rawQuery)
	if err != nil {
		return strings.Contains(rawQuery, url.QueryEscape(name)+"=") || strings.Contains(rawQuery, name+"=")
	}
	_, ok := values[name]
	return ok
}

func normalizeDynamicProviderEndpoints(provider *providerRuntime, endpoints []string) []string {
	if len(endpoints) == 0 {
		return nil
	}

	normalized := make([]string, 0, len(endpoints))
	seen := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint == "" {
			continue
		}
		if provider != nil && !provider.acceptsDiscoveredModelEndpoint(endpoint) {
			continue
		}
		if _, exists := seen[endpoint]; exists {
			continue
		}
		seen[endpoint] = struct{}{}
		normalized = append(normalized, endpoint)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}

func mergeDynamicProviderEndpoints(existing, incoming []string) []string {
	if len(existing) == 0 || len(incoming) == 0 {
		return nil
	}

	merged := append([]string(nil), existing...)
	seen := make(map[string]struct{}, len(existing)+len(incoming))
	for _, endpoint := range existing {
		seen[endpoint] = struct{}{}
	}
	for _, endpoint := range incoming {
		if _, exists := seen[endpoint]; exists {
			continue
		}
		seen[endpoint] = struct{}{}
		merged = append(merged, endpoint)
	}
	return merged
}

func mergeProviderModelRaw(raw json.RawMessage, supportedEndpoints []string) json.RawMessage {
	if len(raw) == 0 {
		return nil
	}

	var payload map[string]json.RawMessage
	if err := json.Unmarshal(raw, &payload); err != nil {
		return append(json.RawMessage(nil), raw...)
	}

	if len(supportedEndpoints) == 0 {
		delete(payload, "supported_endpoints")
	} else if encoded, err := json.Marshal(supportedEndpoints); err == nil {
		payload["supported_endpoints"] = encoded
	}

	merged, err := json.Marshal(payload)
	if err != nil {
		return append(json.RawMessage(nil), raw...)
	}
	return merged
}

func (h *ProxyHandler) speedTierRoutingHeaderNames() []string {
	setup := h.providerSetup()
	if setup == nil {
		return nil
	}
	setup.modelsMu.RLock()
	defer setup.modelsMu.RUnlock()
	seen := map[string]struct{}{}
	var names []string
	for _, model := range setup.models {
		if model.speedTier == nil {
			continue
		}
		name := http.CanonicalHeaderKey(strings.TrimSpace(model.speedTier.neverWhen.HasHeader))
		if name == "" {
			continue
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	return names
}
