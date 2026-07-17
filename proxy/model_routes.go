package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync/atomic"
)

type routeMode string

const (
	routeModePrimaryOnly      routeMode = "primary_only"
	routeModePriorityFailover routeMode = "priority_failover"
)

type publicModelContract struct {
	id        string
	routeID   string
	name      string
	endpoints []string
	raw       json.RawMessage
	policy    providerRequestPolicy
}

type providerRequestPolicy struct {
	parallelToolCalls      *bool
	dropSamplingParams     bool
	useMaxCompletionTokens bool
}

type targetBinding struct {
	id            string
	provider      *providerRuntime
	upstreamModel string
	wirePolicy    providerRequestPolicy
	legacyOwner   providerModel
}

type routePolicy struct {
	mode              routeMode
	maxTargetAttempts int
	maxUpstreamSends  int
	legacyRetry       bool
}

type modelRoute struct {
	public  publicModelContract
	targets []targetBinding
	policy  routePolicy
	legacy  bool
}

func (r *modelRoute) supportsEndpoint(endpoint string) bool {
	if r == nil {
		return false
	}
	return supportsEndpoint(r.public.endpoints, endpoint)
}

func (r *modelRoute) primaryTarget() (targetBinding, bool) {
	if r == nil || len(r.targets) == 0 {
		return targetBinding{}, false
	}
	return r.targets[0], true
}

func (r *modelRoute) targetByID(id string) (targetBinding, bool) {
	if r == nil {
		return targetBinding{}, false
	}
	for _, target := range r.targets {
		if target.id == id {
			return target, true
		}
	}
	return targetBinding{}, false
}

type modelRouteRegistrySnapshot struct {
	byPublicID       map[string]*modelRoute
	aliases          map[string]*modelRoute
	explicit         []*modelRoute
	legacyOrder      []*modelRoute
	legacyByProvider map[string][]*modelRoute
	strictAliases    bool
}

type modelRouteRegistry struct {
	snapshot atomic.Pointer[modelRouteRegistrySnapshot]
}

func newModelRouteRegistry(explicit []*modelRoute) (*modelRouteRegistry, error) {
	registry := &modelRouteRegistry{}
	snapshot := &modelRouteRegistrySnapshot{
		byPublicID:       make(map[string]*modelRoute, len(explicit)),
		aliases:          make(map[string]*modelRoute, len(explicit)*2),
		explicit:         append([]*modelRoute(nil), explicit...),
		legacyByProvider: make(map[string][]*modelRoute),
		strictAliases:    len(explicit) > 0,
	}
	for _, route := range explicit {
		if err := addRouteToSnapshot(snapshot, route); err != nil {
			return nil, err
		}
	}
	registry.snapshot.Store(snapshot)
	return registry, nil
}

func (r *modelRouteRegistry) setStrictAliases(strict bool) {
	if r == nil {
		return
	}
	current := r.load()
	if current == nil || current.strictAliases == strict {
		return
	}
	next := cloneRouteRegistrySnapshot(current, 0)
	next.strictAliases = strict
	r.snapshot.Store(next)
}

func (r *modelRouteRegistry) load() *modelRouteRegistrySnapshot {
	if r == nil {
		return nil
	}
	return r.snapshot.Load()
}

func (r *modelRouteRegistry) lookup(publicID string) (*modelRoute, bool) {
	snapshot := r.load()
	if snapshot == nil {
		return nil, false
	}
	publicID = strings.TrimSpace(publicID)
	if publicID == "" {
		return nil, false
	}
	route, ok := snapshot.byPublicID[publicID]
	return route, ok
}

func (r *modelRouteRegistry) lookupAlias(publicID string) (*modelRoute, bool) {
	snapshot := r.load()
	if snapshot == nil {
		return nil, false
	}
	publicID = strings.TrimSpace(publicID)
	if publicID == "" {
		return nil, false
	}
	if route, ok := snapshot.byPublicID[publicID]; ok {
		return route, true
	}
	route, ok := snapshot.aliases[publicID]
	return route, ok
}

func (r *modelRouteRegistry) explicitRoutes() []*modelRoute {
	snapshot := r.load()
	if snapshot == nil {
		return nil
	}
	return append([]*modelRoute(nil), snapshot.explicit...)
}

func (r *modelRouteRegistry) replaceLegacyProvider(provider *providerRuntime, models []providerModel) error {
	if provider == nil {
		return fmt.Errorf("provider is required")
	}
	providerID := provider.id
	if r == nil {
		return nil
	}
	current := r.load()
	if current == nil {
		return fmt.Errorf("model route registry is not initialized")
	}

	next := &modelRouteRegistrySnapshot{
		byPublicID:       make(map[string]*modelRoute, len(current.byPublicID)+len(models)),
		aliases:          make(map[string]*modelRoute, len(current.aliases)+len(models)*2),
		explicit:         append([]*modelRoute(nil), current.explicit...),
		legacyByProvider: make(map[string][]*modelRoute, len(current.legacyByProvider)+1),
		strictAliases:    current.strictAliases,
	}
	for _, route := range next.explicit {
		if err := addRouteToSnapshot(next, route); err != nil {
			return err
		}
	}
	for id, routes := range current.legacyByProvider {
		if id == providerID {
			continue
		}
		cloned := append([]*modelRoute(nil), routes...)
		next.legacyByProvider[id] = cloned
		for _, route := range cloned {
			if err := addRouteToSnapshot(next, route); err != nil {
				return err
			}
			next.legacyOrder = append(next.legacyOrder, route)
		}
	}

	compiled := make([]*modelRoute, 0, len(models))
	for _, model := range models {
		route, err := compileLegacyModelRoute(model, provider)
		if err != nil {
			return err
		}
		compiled = append(compiled, route)
		if err := addRouteToSnapshot(next, route); err != nil {
			return err
		}
		next.legacyOrder = append(next.legacyOrder, route)
	}
	next.legacyByProvider[providerID] = compiled
	r.snapshot.Store(next)
	return nil
}

func (r *modelRouteRegistry) addLegacyProvider(provider *providerRuntime, models []providerModel) error {
	if r == nil || provider == nil {
		return nil
	}
	current := r.load()
	if current == nil {
		return fmt.Errorf("model route registry is not initialized")
	}
	if _, exists := current.legacyByProvider[provider.id]; exists {
		return r.replaceLegacyProvider(provider, models)
	}

	next := cloneRouteRegistrySnapshot(current, len(models))
	compiled := make([]*modelRoute, 0, len(models))
	for _, model := range models {
		route, err := compileLegacyModelRoute(model, provider)
		if err != nil {
			return err
		}
		if err := addRouteToSnapshot(next, route); err != nil {
			return err
		}
		compiled = append(compiled, route)
		next.legacyOrder = append(next.legacyOrder, route)
	}
	next.legacyByProvider[provider.id] = compiled
	r.snapshot.Store(next)
	return nil
}

func cloneRouteRegistrySnapshot(current *modelRouteRegistrySnapshot, extra int) *modelRouteRegistrySnapshot {
	next := &modelRouteRegistrySnapshot{
		byPublicID:       make(map[string]*modelRoute, len(current.byPublicID)+extra),
		aliases:          make(map[string]*modelRoute, len(current.aliases)+extra*2),
		explicit:         append([]*modelRoute(nil), current.explicit...),
		legacyOrder:      append([]*modelRoute(nil), current.legacyOrder...),
		legacyByProvider: make(map[string][]*modelRoute, len(current.legacyByProvider)+1),
		strictAliases:    current.strictAliases,
	}
	for key, route := range current.byPublicID {
		next.byPublicID[key] = route
	}
	for key, route := range current.aliases {
		next.aliases[key] = route
	}
	for providerID, routes := range current.legacyByProvider {
		next.legacyByProvider[providerID] = append([]*modelRoute(nil), routes...)
	}
	return next
}

func addRouteToSnapshot(snapshot *modelRouteRegistrySnapshot, route *modelRoute) error {
	if snapshot == nil || route == nil {
		return nil
	}
	publicID := strings.TrimSpace(route.public.id)
	if publicID == "" {
		return fmt.Errorf("model route public id is required")
	}
	if existing, ok := snapshot.byPublicID[publicID]; ok && existing != route {
		return modelRouteCollisionError(publicID, existing, route)
	}
	strictAliases := snapshot.strictAliases || len(snapshot.explicit) > 0
	for _, alias := range modelRouteAliases(publicID) {
		if existing, ok := snapshot.aliases[alias]; ok && existing != route {
			// Version-1 legacy catalogs historically allow an exact raw model and
			// its Anthropic-normalized spelling to coexist. Preserve exact-match
			// precedence only for provider-only snapshots. A version-2 snapshot
			// with explicit routes enforces the global normalized namespace during
			// dynamic refresh as well as startup validation.
			if strictAliases || !existing.legacy || !route.legacy {
				return modelRouteCollisionError(alias, existing, route)
			}
		}
		if existing, ok := snapshot.byPublicID[alias]; ok && existing != route && (strictAliases || !existing.legacy || !route.legacy) {
			return modelRouteCollisionError(alias, existing, route)
		}
	}
	snapshot.byPublicID[publicID] = route
	for _, alias := range modelRouteAliases(publicID) {
		if existing, ok := snapshot.aliases[alias]; ok && existing != route && existing.legacy && route.legacy && !strictAliases {
			continue
		}
		snapshot.aliases[alias] = route
	}
	return nil
}

func modelRouteAliases(publicID string) []string {
	return configuredPublicModelAliases(publicID)
}

func modelRouteCollisionError(alias string, existing, incoming *modelRoute) error {
	existingOwner := "unknown"
	incomingOwner := "unknown"
	if existing != nil {
		existingOwner = existing.public.routeID
	}
	if incoming != nil {
		incomingOwner = incoming.public.routeID
	}
	return fmt.Errorf("model %q is exposed by both route %q and route %q", alias, existingOwner, incomingOwner)
}

func compileLegacyModelRoute(model providerModel, provider *providerRuntime) (*modelRoute, error) {
	if provider == nil && strings.TrimSpace(model.providerID) == "" {
		return nil, fmt.Errorf("legacy model %q has no provider", model.publicID)
	}
	if provider != nil && model.providerID == "" {
		model.providerID = provider.id
	}
	routeID := strings.TrimSpace(model.providerID)
	targetID := routeID
	if targetID == "" {
		targetID = "legacy"
	}
	return &modelRoute{
		public: publicModelContract{
			id:        model.publicID,
			routeID:   routeID,
			endpoints: append([]string(nil), model.supportedEndpoints...),
			raw:       append(json.RawMessage(nil), model.raw...),
			policy: providerRequestPolicy{
				parallelToolCalls:      cloneBoolPtr(model.parallelToolCalls),
				dropSamplingParams:     model.dropSamplingParams,
				useMaxCompletionTokens: model.useMaxCompletionTokens,
			},
		},
		targets: []targetBinding{{
			id:            targetID,
			provider:      provider,
			upstreamModel: model.upstreamModel,
			wirePolicy: providerRequestPolicy{
				parallelToolCalls:      cloneBoolPtr(model.parallelToolCalls),
				dropSamplingParams:     model.dropSamplingParams,
				useMaxCompletionTokens: model.useMaxCompletionTokens,
			},
			legacyOwner: model,
		}},
		policy: routePolicy{
			mode:              routeModePrimaryOnly,
			maxTargetAttempts: 1,
			maxUpstreamSends:  1,
			legacyRetry:       true,
		},
		legacy: true,
	}, nil
}

func (ps *providerSetup) routeRegistry() *modelRouteRegistry {
	if ps == nil {
		return nil
	}
	return ps.routes
}

func (ps *providerSetup) lookupRoute(publicID string) (*modelRoute, bool) {
	registry := ps.routeRegistry()
	if registry == nil {
		return nil, false
	}
	return registry.lookup(publicID)
}

func (ps *providerSetup) lookupRouteAlias(publicID string) (*modelRoute, bool) {
	registry := ps.routeRegistry()
	if registry == nil {
		return nil, false
	}
	return registry.lookupAlias(publicID)
}

func (h *ProxyHandler) resolveModelRouteForRequest(model, endpoint string) (*modelRoute, bool) {
	setup := h.providerSetup()
	rawModel := strings.TrimSpace(model)
	if route, ok := setup.lookupRoute(rawModel); ok {
		return route, true
	}
	if route, ok := setup.lookupRouteAlias(NormalizeModelName(rawModel)); ok {
		// Explicit routes reserve their normalized public-model aliases globally,
		// so every public endpoint must resolve those aliases to the same route.
		// Legacy provider catalogs keep their historical Messages-only alias
		// fallback; chat and Responses still require an exact legacy public ID.
		if !route.legacy || endpoint == providerEndpointMessages {
			return route, true
		}
	}

	return nil, false
}

func providerModelFromRouteTarget(route *modelRoute, target targetBinding) providerModel {
	if target.legacyOwner.publicID != "" || target.legacyOwner.providerID != "" {
		owner := target.legacyOwner
		if owner.publicID == "" && route != nil {
			owner.publicID = route.public.id
		}
		if owner.providerID == "" && target.provider != nil {
			owner.providerID = target.provider.id
		}
		if owner.upstreamModel == "" {
			owner.upstreamModel = target.upstreamModel
		}
		return owner
	}
	owner := providerModel{
		publicID:               route.public.id,
		upstreamModel:          target.upstreamModel,
		supportedEndpoints:     append([]string(nil), route.public.endpoints...),
		parallelToolCalls:      cloneBoolPtr(route.public.policy.parallelToolCalls),
		dropSamplingParams:     route.public.policy.dropSamplingParams,
		useMaxCompletionTokens: target.wirePolicy.useMaxCompletionTokens,
		raw:                    append(json.RawMessage(nil), route.public.raw...),
	}
	if target.provider != nil {
		owner.providerID = target.provider.id
	}
	return owner
}

func (r *modelRouteRegistry) replaceLegacyProviders(providers map[string]*providerRuntime, providerOrder []string, replacements map[string][]providerModel) error {
	if r == nil || len(replacements) == 0 {
		return nil
	}
	current := r.load()
	if current == nil {
		return fmt.Errorf("model route registry is not initialized")
	}
	next := &modelRouteRegistrySnapshot{
		byPublicID:       make(map[string]*modelRoute, len(current.byPublicID)),
		aliases:          make(map[string]*modelRoute, len(current.aliases)),
		explicit:         append([]*modelRoute(nil), current.explicit...),
		legacyByProvider: make(map[string][]*modelRoute, len(current.legacyByProvider)+len(replacements)),
		strictAliases:    current.strictAliases,
	}
	for _, route := range next.explicit {
		if err := addRouteToSnapshot(next, route); err != nil {
			return err
		}
	}

	seenProviders := make(map[string]struct{}, len(current.legacyByProvider)+len(replacements))
	emitProvider := func(providerID string) error {
		if _, emitted := seenProviders[providerID]; emitted {
			return nil
		}

		var routes []*modelRoute
		if models, replacing := replacements[providerID]; replacing {
			compiled, err := compileLegacyProviderRoutes(providers[providerID], models)
			if err != nil {
				return err
			}
			routes = compiled
		} else {
			currentRoutes, exists := current.legacyByProvider[providerID]
			if !exists {
				return nil
			}
			routes = append([]*modelRoute(nil), currentRoutes...)
		}

		for _, route := range routes {
			if err := addRouteToSnapshot(next, route); err != nil {
				return err
			}
			next.legacyOrder = append(next.legacyOrder, route)
		}
		next.legacyByProvider[providerID] = routes
		seenProviders[providerID] = struct{}{}
		return nil
	}

	// Rebuild configured providers in their original order. This matches normal
	// startup and preserves version-1's historical first-alias winner when a
	// deferred catalog introduces normalized Messages aliases. Strict version-2
	// snapshots still reject the same collision before this snapshot is stored.
	for _, providerID := range providerOrder {
		if err := emitProvider(providerID); err != nil {
			return err
		}
	}

	// Preserve the established order of unchanged providers that are not present
	// in providerOrder. Replaced providers wait for the sorted fallback below.
	for _, route := range current.legacyOrder {
		if route == nil || len(route.targets) == 0 || route.targets[0].provider == nil {
			continue
		}
		providerID := route.targets[0].provider.id
		if _, replacing := replacements[providerID]; replacing {
			continue
		}
		if err := emitProvider(providerID); err != nil {
			return err
		}
	}

	remainingReplacementIDs := make([]string, 0, len(replacements))
	for providerID := range replacements {
		if _, emitted := seenProviders[providerID]; !emitted {
			remainingReplacementIDs = append(remainingReplacementIDs, providerID)
		}
	}
	sort.Strings(remainingReplacementIDs)
	for _, providerID := range remainingReplacementIDs {
		if err := emitProvider(providerID); err != nil {
			return err
		}
	}

	// Empty legacy-provider entries do not appear in legacyOrder. Retain any
	// remaining unchanged entries deterministically so later refreshes keep the
	// same registry shape.
	remainingCurrentProviderIDs := make([]string, 0, len(current.legacyByProvider))
	for providerID := range current.legacyByProvider {
		if _, emitted := seenProviders[providerID]; !emitted {
			remainingCurrentProviderIDs = append(remainingCurrentProviderIDs, providerID)
		}
	}
	sort.Strings(remainingCurrentProviderIDs)
	for _, providerID := range remainingCurrentProviderIDs {
		if err := emitProvider(providerID); err != nil {
			return err
		}
	}

	r.snapshot.Store(next)
	return nil
}

func compileLegacyProviderRoutes(provider *providerRuntime, models []providerModel) ([]*modelRoute, error) {
	if provider == nil {
		return nil, fmt.Errorf("provider is required")
	}
	compiled := make([]*modelRoute, 0, len(models))
	for _, model := range models {
		route, err := compileLegacyModelRoute(model, provider)
		if err != nil {
			return nil, err
		}
		compiled = append(compiled, route)
	}
	return compiled, nil
}

// validateRouteAwareRequestJSON keeps strict duplicate-key rejection scoped to
// proxy-owned explicit routes; legacy and provider-owned requests retain their
// existing decoder and upstream behavior.
func (h *ProxyHandler) validateRouteAwareRequestJSON(body []byte, model, endpoint string) error {
	if h == nil {
		return nil
	}
	route, known := h.resolveModelRouteForRequest(model, endpoint)
	if !known || route == nil || route.legacy {
		return nil
	}
	if err := rejectDuplicateJSONMappingKeys(body); err != nil {
		return &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("invalid ambiguous JSON request: %w", err)}
	}
	return nil
}
