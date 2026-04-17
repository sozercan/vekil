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
	"strings"
)

type providerType string

const (
	providerTypeCopilot     providerType = "copilot"
	providerTypeAzureOpenAI providerType = "azure-openai"
)

var defaultStaticProviderEndpoints = []string{"/chat/completions", "/responses"}

// ProvidersConfig configures optional non-Copilot upstream providers.
// When empty, the proxy keeps its legacy zero-config Copilot behavior.
type ProvidersConfig struct {
	Providers []ProviderConfig `json:"providers"`
}

// ProviderConfig configures one upstream provider instance.
type ProviderConfig struct {
	ID            string                `json:"id"`
	Type          string                `json:"type"`
	Default       bool                  `json:"default,omitempty"`
	ExcludeModels []string              `json:"exclude_models,omitempty"`
	BaseURL       string                `json:"base_url,omitempty"`
	APIKey        string                `json:"api_key,omitempty"`
	APIKeyEnv     string                `json:"api_key_env,omitempty"`
	APIVersion    string                `json:"api_version,omitempty"`
	Models        []ProviderModelConfig `json:"models,omitempty"`
}

// ProviderModelConfig maps a public model ID exposed by this proxy to the
// upstream model or deployment name used by the provider.
type ProviderModelConfig struct {
	PublicID            string   `json:"public_id"`
	Deployment          string   `json:"deployment,omitempty"`
	Name                string   `json:"name,omitempty"`
	Endpoints           []string `json:"endpoints,omitempty"`
	ModelPickerEnabled  *bool    `json:"model_picker_enabled,omitempty"`
	ModelPickerCategory string   `json:"model_picker_category,omitempty"`
	ReasoningEffort     []string `json:"reasoning_effort,omitempty"`
	Vision              bool     `json:"vision,omitempty"`
	ParallelToolCalls   *bool    `json:"parallel_tool_calls,omitempty"`
	ContextWindow       int64    `json:"context_window,omitempty"`
}

type providerRuntime struct {
	id            string
	kind          providerType
	isDefault     bool
	baseURL       string
	apiKey        string
	apiVersion    string
	excludeModels map[string]struct{}
	staticModels  map[string]providerModel
	staticOrder   []string
}

type providerModel struct {
	publicID           string
	upstreamModel      string
	providerID         string
	supportedEndpoints []string
	disabled           bool
	raw                json.RawMessage
}

type providerSetup struct {
	providers          map[string]*providerRuntime
	providerOrder      []string
	defaultProviderID  string
	models             map[string]providerModel
	hasConfiguredState bool
}

type providerModelsFetchResult struct {
	models      []providerModel
	etag        string
	notModified bool
}

type providerRequestError struct {
	statusCode int
	err        error
}

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
	if err := json.Unmarshal(body, &cfg); err != nil {
		return cfg, fmt.Errorf("decode providers config %q: %w", path, err)
	}
	return cfg, nil
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
	pm, ok := ps.models[strings.TrimSpace(model)]
	return pm, ok
}

func (h *ProxyHandler) initializeProviders() error {
	if len(h.providersConfig.Providers) == 0 {
		return nil
	}

	setup, err := h.buildConfiguredProviderSetup(context.Background(), h.providersConfig)
	if err != nil {
		return err
	}
	h.providersState = setup
	return nil
}

func (h *ProxyHandler) buildConfiguredProviderSetup(ctx context.Context, cfg ProvidersConfig) (*providerSetup, error) {
	providers, providerOrder, defaultProviderID, err := h.buildProviders(cfg)
	if err != nil {
		return nil, err
	}

	setup := &providerSetup{
		providers:          providers,
		providerOrder:      providerOrder,
		defaultProviderID:  defaultProviderID,
		models:             make(map[string]providerModel),
		hasConfiguredState: true,
	}

	needsDynamicModelValidation := false
	for _, provider := range providers {
		if provider.kind == providerTypeCopilot && len(providers) > 1 {
			needsDynamicModelValidation = true
			break
		}
	}

	if !needsDynamicModelValidation {
		for _, provider := range providers {
			for publicID, model := range provider.staticModels {
				setup.models[publicID] = model
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
		if provider.kind != providerTypeCopilot {
			for publicID, model := range provider.staticModels {
				if existing, exists := setup.models[publicID]; exists {
					return nil, fmt.Errorf("model %q is exposed by both provider %q and provider %q", publicID, existing.providerID, model.providerID)
				}
				setup.models[publicID] = model
			}
			continue
		}

		result, err := h.fetchProviderModels(ctx, provider, "", "")
		if err != nil {
			return nil, fmt.Errorf("load models for provider %q: %w", provider.id, err)
		}
		for _, model := range result.models {
			if existing, exists := setup.models[model.publicID]; exists {
				if existing.providerID == model.providerID {
					continue
				}
				return nil, fmt.Errorf("model %q is exposed by both provider %q and provider %q", model.publicID, existing.providerID, model.providerID)
			}
			setup.models[model.publicID] = model
		}
	}

	return setup, nil
}

func (h *ProxyHandler) buildProviders(cfg ProvidersConfig) (map[string]*providerRuntime, []string, string, error) {
	providers := make(map[string]*providerRuntime, len(cfg.Providers))
	providerOrder := make([]string, 0, len(cfg.Providers))
	defaultProviderID := ""
	copilotProviders := 0

	for _, raw := range cfg.Providers {
		provider, err := buildProviderRuntime(raw, h.copilotURL)
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

func buildProviderRuntime(cfg ProviderConfig, defaultCopilotURL string) (*providerRuntime, error) {
	id := strings.TrimSpace(cfg.ID)
	if id == "" {
		return nil, fmt.Errorf("provider id is required")
	}

	kind := providerType(strings.TrimSpace(cfg.Type))
	switch kind {
	case providerTypeCopilot:
	case providerTypeAzureOpenAI:
	default:
		return nil, fmt.Errorf("provider %q has unsupported type %q", id, cfg.Type)
	}

	runtime := &providerRuntime{
		id:            id,
		kind:          kind,
		isDefault:     cfg.Default,
		excludeModels: make(map[string]struct{}, len(cfg.ExcludeModels)),
		staticModels:  make(map[string]providerModel, len(cfg.Models)),
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
	case providerTypeAzureOpenAI:
		baseURL := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
		if baseURL == "" {
			return nil, fmt.Errorf("provider %q must set base_url", id)
		}
		runtime.baseURL = baseURL
		runtime.apiVersion = strings.TrimSpace(cfg.APIVersion)
		runtime.apiKey = strings.TrimSpace(cfg.APIKey)
		if runtime.apiKey == "" && strings.TrimSpace(cfg.APIKeyEnv) != "" {
			runtime.apiKey = strings.TrimSpace(os.Getenv(strings.TrimSpace(cfg.APIKeyEnv)))
		}
		if runtime.apiKey == "" {
			return nil, fmt.Errorf("provider %q must set api_key or api_key_env", id)
		}
		if len(cfg.Models) == 0 {
			return nil, fmt.Errorf("provider %q must configure at least one model", id)
		}
		for _, modelCfg := range cfg.Models {
			model, err := buildStaticProviderModel(id, modelCfg)
			if err != nil {
				return nil, err
			}
			if _, excluded := runtime.excludeModels[model.publicID]; excluded {
				continue
			}
			if _, exists := runtime.staticModels[model.publicID]; exists {
				return nil, fmt.Errorf("provider %q configures model %q more than once", id, model.publicID)
			}
			runtime.staticModels[model.publicID] = model
			runtime.staticOrder = append(runtime.staticOrder, model.publicID)
		}
	}

	return runtime, nil
}

func buildStaticProviderModel(providerID string, cfg ProviderModelConfig) (providerModel, error) {
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

	endpoints := normalizeProviderEndpoints(cfg.Endpoints)
	raw, err := synthesizeProviderModelRaw(providerID, publicID, name, endpoints, cfg)
	if err != nil {
		return providerModel{}, err
	}

	return providerModel{
		publicID:           publicID,
		upstreamModel:      upstreamModel,
		providerID:         providerID,
		supportedEndpoints: endpoints,
		raw:                raw,
	}, nil
}

func normalizeProviderEndpoints(endpoints []string) []string {
	if len(endpoints) == 0 {
		return append([]string(nil), defaultStaticProviderEndpoints...)
	}

	normalized := make([]string, 0, len(endpoints))
	seen := make(map[string]struct{}, len(endpoints))
	for _, endpoint := range endpoints {
		endpoint = strings.TrimSpace(endpoint)
		if endpoint == "" {
			continue
		}
		if _, ok := seen[endpoint]; ok {
			continue
		}
		seen[endpoint] = struct{}{}
		normalized = append(normalized, endpoint)
	}
	if len(normalized) == 0 {
		return append([]string(nil), defaultStaticProviderEndpoints...)
	}
	return normalized
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
				MaxContextWindowTokens: cfg.ContextWindow,
			},
			Supports: supports{
				ParallelToolCalls: parallelToolCalls,
				ReasoningEffort:   append([]string(nil), cfg.ReasoningEffort...),
				Vision:            cfg.Vision,
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

func (h *ProxyHandler) providerRequestURL(provider *providerRuntime, path string, extraQuery string) (string, error) {
	if provider == nil {
		return "", fmt.Errorf("provider is required")
	}

	baseURL := strings.TrimRight(provider.baseURL, "/")
	fullURL := baseURL + path
	if provider.kind != providerTypeAzureOpenAI || provider.apiVersion == "" {
		return appendRawQuery(fullURL, extraQuery), nil
	}
	return appendRawQuery(fullURL, appendQuery("api-version="+url.QueryEscape(provider.apiVersion), extraQuery)), nil
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

func (h *ProxyHandler) applyProviderHeaders(req *http.Request, provider *providerRuntime) error {
	if provider == nil {
		return &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("provider is required")}
	}

	switch provider.kind {
	case providerTypeCopilot:
		token, err := h.auth.GetToken(req.Context())
		if err != nil {
			return &providerRequestError{statusCode: http.StatusInternalServerError, err: err}
		}
		h.setCopilotHeaders(req, token)
	case providerTypeAzureOpenAI:
		req.Header.Set("api-key", provider.apiKey)
		req.Header.Set("Content-Type", "application/json")
	default:
		return &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("unsupported provider type %q", provider.kind)}
	}
	return nil
}

func (h *ProxyHandler) newProviderJSONRequest(ctx context.Context, provider *providerRuntime, method, path string, body []byte, extraHeaders http.Header, extraQuery string) (*http.Request, error) {
	fullURL, err := h.providerRequestURL(provider, path, extraQuery)
	if err != nil {
		return nil, err
	}

	req, err := http.NewRequestWithContext(ctx, method, fullURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	if len(extraHeaders) > 0 {
		mergeHeaderValues(req.Header, extraHeaders)
	}
	if err := h.applyProviderHeaders(req, provider); err != nil {
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
		models := make([]providerModel, 0, len(provider.staticModels))
		for _, publicID := range provider.staticOrder {
			model, ok := provider.staticModels[publicID]
			if ok {
				models = append(models, model)
			}
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

		models, err := decodeProviderModelsFromBody(provider.id, body, provider.excludeModels)
		if err != nil {
			return providerModelsFetchResult{}, err
		}
		result.models = models
		return result, nil
	default:
		return providerModelsFetchResult{}, fmt.Errorf("unsupported provider type %q", provider.kind)
	}
}

func decodeProviderModelsFromBody(providerID string, body []byte, excluded map[string]struct{}) ([]providerModel, error) {
	var upstream struct {
		Data []json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(body, &upstream); err != nil {
		return nil, err
	}

	models := make([]providerModel, 0, len(upstream.Data))
	seen := make(map[string]struct{}, len(upstream.Data))
	for _, raw := range upstream.Data {
		var parsed struct {
			ID                 string   `json:"id"`
			SupportedEndpoints []string `json:"supported_endpoints"`
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
		if _, skip := excluded[publicID]; skip {
			continue
		}
		if _, duplicate := seen[publicID]; duplicate {
			continue
		}
		seen[publicID] = struct{}{}
		models = append(models, providerModel{
			publicID:           publicID,
			upstreamModel:      publicID,
			providerID:         providerID,
			supportedEndpoints: append([]string(nil), parsed.SupportedEndpoints...),
			disabled:           strings.EqualFold(parsed.Policy.State, "disabled"),
			raw:                raw,
		})
	}

	return models, nil
}
