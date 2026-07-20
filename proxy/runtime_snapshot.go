package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// runtimeSnapshot is the immutable, atomically published runtime generation.
// Config-dependent request state must be obtained from one pinned snapshot for
// the lifetime of an HTTP request or WebSocket session.
type runtimeSnapshot struct {
	generation uint64
	revision   string

	config        ProvidersConfig
	managedActive bool
	providers     *providerSetup
	policy        *policyBinding
	readiness     activeRuntimeReadiness
	caches        *runtimeCaches
}

type policyBinding struct {
	planner    chatPolicyPlanner
	controller policyRoutingController
}

type activeRuntimeReadiness struct {
	policyPreflightComplete bool
	policyDiagnostic        string
}

type runtimeCaches struct {
	models     *modelsCache
	chatRoutes *chatRouteDiscoveryCache
}

type runtimeSnapshotContextKey struct{}

func newRuntimeCaches() *runtimeCaches {
	models := &modelsCache{}
	chatRoutes := newChatRouteDiscoveryCache()
	return &runtimeCaches{models: models, chatRoutes: &chatRoutes}
}

func runtimeRevisionFromConfig(cfg ProvidersConfig) string {
	// This hash is an opaque optimistic-concurrency token, not a serialization
	// format. The canonical provider-config codec replaces json.Marshal at the
	// persistence boundary; hashing the private deep clone here avoids exposing
	// secret material while keeping construction independent of the managed
	// store.
	body, err := json.Marshal(cfg)
	if err != nil {
		body = []byte(fmt.Sprintf("schema=%d providers=%d routes=%d policies=%d", cfg.EffectiveSchemaVersion(), len(cfg.Providers), len(cfg.ModelRoutes), len(cfg.PolicyProfiles)))
	}
	sum := sha256.Sum256(body)
	return "cfg_" + hex.EncodeToString(sum[:16])
}

func (h *ProxyHandler) currentRuntime() *runtimeSnapshot {
	if h == nil {
		return nil
	}
	if snapshot := h.runtime.Load(); snapshot != nil {
		return snapshot
	}
	return nil
}

func runtimeFromContext(ctx context.Context) *runtimeSnapshot {
	if ctx == nil {
		return nil
	}
	snapshot, _ := ctx.Value(runtimeSnapshotContextKey{}).(*runtimeSnapshot)
	return snapshot
}

func (h *ProxyHandler) runtimeForContext(ctx context.Context) *runtimeSnapshot {
	if snapshot := runtimeFromContext(ctx); snapshot != nil {
		return snapshot
	}
	return h.currentRuntime()
}

// PinRuntimeContext loads the current generation exactly once and stores it in
// the returned context. It is intentionally exported for the server middleware
// that admits HTTP requests.
func (h *ProxyHandler) PinRuntimeContext(ctx context.Context) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if runtimeFromContext(ctx) != nil {
		return ctx
	}
	if snapshot := h.currentRuntime(); snapshot != nil {
		return context.WithValue(ctx, runtimeSnapshotContextKey{}, snapshot)
	}
	return ctx
}

// PinRuntimeRequest returns a shallow request copy whose context owns one
// runtime generation for the complete operation.
func (h *ProxyHandler) PinRuntimeRequest(r *http.Request) *http.Request {
	if r == nil {
		return nil
	}
	return r.WithContext(h.PinRuntimeContext(r.Context()))
}

func (h *ProxyHandler) currentProvidersConfig() ProvidersConfig {
	if snapshot := h.currentRuntime(); snapshot != nil {
		return cloneProvidersConfigForValidation(snapshot.config)
	}
	if h == nil {
		return ProvidersConfig{}
	}
	return cloneProvidersConfigForValidation(h.providersConfig)
}

func (h *ProxyHandler) providersConfigForContext(ctx context.Context) ProvidersConfig {
	if snapshot := h.runtimeForContext(ctx); snapshot != nil {
		return cloneProvidersConfigForValidation(snapshot.config)
	}
	return h.currentProvidersConfig()
}

func (h *ProxyHandler) providerSetupForContext(ctx context.Context) *providerSetup {
	if snapshot := h.runtimeForContext(ctx); snapshot != nil && snapshot.providers != nil {
		return snapshot.providers
	}
	if h != nil && h.providersState != nil {
		return h.providersState
	}
	return defaultProviderSetup(h)
}

func (h *ProxyHandler) policyBindingForContext(ctx context.Context) *policyBinding {
	if snapshot := h.runtimeForContext(ctx); snapshot != nil && snapshot.policy != nil {
		return snapshot.policy
	}
	if h == nil {
		return nil
	}
	if h.chatPolicyPlanner == nil && h.policyRoutingController == nil {
		return nil
	}
	return &policyBinding{planner: h.chatPolicyPlanner, controller: h.policyRoutingController}
}

func (h *ProxyHandler) modelsCacheForContext(ctx context.Context) *modelsCache {
	if snapshot := h.runtimeForContext(ctx); snapshot != nil && snapshot.caches != nil && snapshot.caches.models != nil {
		return snapshot.caches.models
	}
	if h == nil {
		return nil
	}
	return &h.models
}

func (h *ProxyHandler) chatRouteCacheForContext(ctx context.Context) *chatRouteDiscoveryCache {
	if snapshot := h.runtimeForContext(ctx); snapshot != nil && snapshot.caches != nil && snapshot.caches.chatRoutes != nil {
		return snapshot.caches.chatRoutes
	}
	if h == nil {
		return nil
	}
	return &h.chatRoutes
}

func (h *ProxyHandler) publishRuntime(snapshot *runtimeSnapshot) {
	if h == nil || snapshot == nil {
		return
	}
	h.runtime.Store(snapshot)
}

func (h *ProxyHandler) runtimeGeneration() uint64 {
	if snapshot := h.currentRuntime(); snapshot != nil {
		return snapshot.generation
	}
	return 0
}

func (h *ProxyHandler) runtimeRevision() string {
	if snapshot := h.currentRuntime(); snapshot != nil {
		return snapshot.revision
	}
	return ""
}

func (h *ProxyHandler) buildRuntimeSnapshot(ctx context.Context, cfg ProvidersConfig, generation uint64, revision string, validateDynamicModels bool) (*runtimeSnapshot, error) {
	if h == nil {
		return nil, fmt.Errorf("proxy handler is required")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	validated, err := validateAndNormalizeProvidersConfig(cfg)
	if err != nil {
		return nil, err
	}
	cfg = validated.config
	setup, err := h.buildConfiguredProviderSetupWithDynamicValidation(ctx, cfg, false)
	if err != nil {
		return nil, err
	}
	h.reuseCompatibleProviderAuthResources(setup)
	if validateDynamicModels {
		if err := h.discoverCandidateProviderModels(ctx, setup); err != nil {
			return nil, err
		}
	}
	controller, err := newChatPolicyRoutingControllerForSetup(h, cfg, h.policyRoutingMode, setup)
	if err != nil {
		return nil, err
	}
	binding := &policyBinding{}
	readiness := activeRuntimeReadiness{policyPreflightComplete: true}
	if controller != nil {
		binding.planner = controller
		binding.controller = controller
		if controller.Active() {
			readiness.policyPreflightComplete = false
		}
	}
	if revision == "" {
		revision = runtimeRevisionFromConfig(cfg)
	}
	managedActive := false
	if current := h.currentRuntime(); current != nil {
		managedActive = current.managedActive
	}
	return &runtimeSnapshot{
		generation:    generation,
		revision:      revision,
		config:        cloneProvidersConfigForValidation(cfg),
		managedActive: managedActive,
		providers:     setup,
		policy:        binding,
		readiness:     readiness,
		caches:        newRuntimeCaches(),
	}, nil
}

func (h *ProxyHandler) discoverCandidateProviderModels(ctx context.Context, setup *providerSetup) error {
	if setup == nil || !setup.hasConfiguredState {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, modelsUpstreamTimeout)
	defer cancel()
	replacements := make(map[string][]providerModel)
	for _, providerID := range setup.providerOrder {
		provider := setup.providerByID(providerID)
		if !providerUsesDynamicModels(provider) {
			continue
		}
		result, err := h.fetchProviderModels(ctx, provider, "", "")
		if err != nil {
			return fmt.Errorf("load models for provider %q: %w", provider.id, err)
		}
		if !result.notModified {
			replacements[providerID] = filterProviderModels(provider, result.models)
		}
	}
	return setup.replaceProviderModelsBatch(replacements)
}

func (s *runtimeSnapshot) preflight(ctx context.Context) error {
	if s == nil || s.policy == nil || s.policy.controller == nil || !s.policy.controller.Active() {
		if s != nil {
			s.readiness.policyPreflightComplete = true
			s.readiness.policyDiagnostic = ""
		}
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := s.policy.controller.Initialize(ctx); err != nil {
		s.readiness.policyDiagnostic = s.policy.controller.ReadinessDiagnostic()
		return err
	}
	s.readiness.policyPreflightComplete = true
	s.readiness.policyDiagnostic = s.policy.controller.ReadinessDiagnostic()
	return nil
}

func resolveModelRouteForSetup(setup *providerSetup, model, endpoint string) (*modelRoute, bool) {
	if setup == nil {
		return nil, false
	}
	rawModel := strings.TrimSpace(model)
	if route, ok := setup.lookupRoute(rawModel); ok {
		return route, true
	}
	if route, ok := setup.lookupRouteAlias(NormalizeModelName(rawModel)); ok {
		if !route.legacy || endpoint == providerEndpointMessages {
			return route, true
		}
	}
	return nil, false
}

func (h *ProxyHandler) modelAllowedForContext(ctx context.Context, model, endpoint string) bool {
	if h == nil || len(h.allowedModels) == 0 {
		return true
	}
	model = strings.TrimSpace(model)
	if _, ok := h.allowedModels[model]; ok {
		return true
	}
	if endpoint != providerEndpointMessages {
		return false
	}
	setup := h.providerSetupForContext(ctx)
	if _, rawKnown := setup.lookupModel(model); rawKnown {
		return false
	}
	normalizedModel := NormalizeModelName(model)
	if normalizedModel == model {
		return false
	}
	_, ok := h.allowedModels[normalizedModel]
	return ok
}

func (h *ProxyHandler) resolveProviderModelForContext(ctx context.Context, model, endpoint string) (*providerRuntime, providerModel, bool) {
	model = strings.TrimSpace(model)
	if !h.modelAllowedForContext(ctx, model, endpoint) {
		return nil, providerModel{publicID: model, upstreamModel: model}, false
	}
	setup := h.providerSetupForContext(ctx)
	if route, known := resolveModelRouteForSetup(setup, model, endpoint); known && route != nil {
		target, ok := route.primaryTarget()
		if !ok || target.provider == nil {
			return nil, providerModel{}, true
		}
		return target.provider, providerModelFromRouteTarget(route, target), true
	}
	if provider, owner, reserved := setup.resolveReservedModelIdentity(model); reserved {
		return provider, owner, true
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

func inheritRuntimeContext(dst, src context.Context) context.Context {
	if dst == nil {
		dst = context.Background()
	}
	if snapshot := runtimeFromContext(src); snapshot != nil {
		return context.WithValue(dst, runtimeSnapshotContextKey{}, snapshot)
	}
	return dst
}

func (h *ProxyHandler) providerWithinAllowedModelScopeForSetup(setup *providerSetup, provider *providerRuntime) bool {
	if provider == nil {
		return false
	}
	if len(h.allowedModels) == 0 {
		return true
	}
	for model := range h.allowedModels {
		if entry, ok := setup.lookupPublicModelEntry(model); ok && entry != nil && entry.kind == publicEntryPolicy {
			continue
		}
		if route, ok := setup.lookupRoute(model); ok && route != nil && !route.legacy {
			for _, target := range route.targets {
				if target.provider != nil && target.provider.id == provider.id {
					return true
				}
			}
			continue
		}
		if owner, ok := setup.lookupModel(model); ok {
			if owner.providerID == provider.id {
				return true
			}
			continue
		}
		if providerCanExposeModel(provider, model) {
			return true
		}
	}
	return false
}

func (h *ProxyHandler) validateRouteAwareRequestJSONForContext(ctx context.Context, body []byte, model, endpoint string) error {
	if h == nil {
		return nil
	}
	route, known := resolveModelRouteForSetup(h.providerSetupForContext(ctx), model, endpoint)
	if !known || route == nil || route.legacy {
		return nil
	}
	if err := rejectDuplicateJSONMappingKeys(body); err != nil {
		return &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("invalid ambiguous JSON request: %w", err)}
	}
	return nil
}

func (h *ProxyHandler) reuseCompatibleProviderAuthResources(candidate *providerSetup) {
	if h == nil || candidate == nil {
		return
	}
	current := h.currentRuntime()
	if current == nil || current.providers == nil {
		return
	}
	for id, next := range candidate.providers {
		prior := current.providers.providerByID(id)
		if !providerAuthResourcesCompatible(prior, next) {
			continue
		}
		switch next.kind {
		case providerTypeAzureOpenAI:
			if next.azureAuthMode() == providerAuthModeAzureIdentity {
				next.azureToken = prior.azureToken
			}
		case providerTypeOpenAICodex:
			next.codexAuth = prior.codexAuth
		}
	}
}

func providerAuthResourcesCompatible(prior, next *providerRuntime) bool {
	if prior == nil || next == nil || prior.kind != next.kind {
		return false
	}
	if normalizeTargetRevisionDestination(prior.baseURL) != normalizeTargetRevisionDestination(next.baseURL) {
		return false
	}
	switch next.kind {
	case providerTypeAzureOpenAI:
		return prior.azureAuthMode() == providerAuthModeAzureIdentity &&
			next.azureAuthMode() == providerAuthModeAzureIdentity &&
			strings.TrimSpace(prior.tokenScope) == strings.TrimSpace(next.tokenScope) &&
			prior.azureToken != nil
	case providerTypeOpenAICodex:
		return prior.codexAuth != nil && next.codexAuth != nil &&
			normalizeTargetRevisionFileSource(prior.codexAuth.path) == normalizeTargetRevisionFileSource(next.codexAuth.path)
	default:
		return false
	}
}
