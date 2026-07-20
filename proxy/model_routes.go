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
	revision      targetRevision
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
	public          publicModelContract
	targets         []targetBinding
	policy          routePolicy
	exposure        string
	internalPurpose string
	legacy          bool
}

func (r *modelRoute) isPublic() bool {
	if r == nil || strings.TrimSpace(r.public.id) == "" {
		return false
	}
	return r.exposure != modelRouteExposureInternal
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
	byRouteID        map[string]*modelRoute
	explicit         []*modelRoute
	legacyOrder      []*modelRoute
	legacyByProvider map[string][]*modelRoute
	strictAliases    bool
	policyEntries    []*publicModelEntry
	publicEntries    *publicModelEntryRegistrySnapshot
}

type modelRouteRegistry struct {
	snapshot      atomic.Pointer[modelRouteRegistrySnapshot]
	publicEntries *publicModelEntryRegistry
}

func newModelRouteRegistry(explicit []*modelRoute) (*modelRouteRegistry, error) {
	registry := &modelRouteRegistry{}
	registry.publicEntries = &publicModelEntryRegistry{routes: registry}
	snapshot := &modelRouteRegistrySnapshot{
		byRouteID:        make(map[string]*modelRoute, len(explicit)),
		explicit:         append([]*modelRoute(nil), explicit...),
		legacyByProvider: make(map[string][]*modelRoute),
		strictAliases:    len(explicit) > 0,
	}
	for _, route := range explicit {
		if route == nil {
			continue
		}
		ensureModelRouteTargetRevisions(route)
		routeID := strings.TrimSpace(route.public.routeID)
		if routeID == "" {
			return nil, fmt.Errorf("model route operational id is required")
		}
		if existing, ok := snapshot.byRouteID[routeID]; ok && existing != route {
			return nil, fmt.Errorf("terminal route %q is declared more than once", routeID)
		}
		snapshot.byRouteID[routeID] = route
	}
	if err := rebuildPublicModelEntrySnapshot(snapshot); err != nil {
		return nil, err
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

func (r *modelRouteRegistry) setPolicyEntries(entries []*publicModelEntry) error {
	if r == nil {
		return nil
	}
	current := r.load()
	if current == nil {
		return fmt.Errorf("model route registry is not initialized")
	}
	next := cloneRouteRegistrySnapshot(current, 0)
	next.policyEntries = make([]*publicModelEntry, 0, len(entries))
	for _, entry := range entries {
		if entry != nil {
			next.policyEntries = append(next.policyEntries, clonePublicModelEntry(entry))
		}
	}
	if err := rebuildPublicModelEntrySnapshot(next); err != nil {
		return err
	}
	r.snapshot.Store(next)
	return nil
}

func (r *modelRouteRegistry) load() *modelRouteRegistrySnapshot {
	if r == nil {
		return nil
	}
	return r.snapshot.Load()
}

func (r *modelRouteRegistry) lookup(publicID string) (*modelRoute, bool) {
	if r == nil || r.publicEntries == nil {
		return nil, false
	}
	entry, ok := r.publicEntries.lookupExact(publicID)
	if !ok || entry == nil || entry.kind != publicEntryStatic || entry.route == nil {
		return nil, false
	}
	return entry.route, true
}

func (r *modelRouteRegistry) lookupAlias(publicID string) (*modelRoute, bool) {
	if r == nil || r.publicEntries == nil {
		return nil, false
	}
	entry, ok := r.publicEntries.lookup(publicID)
	if !ok || entry == nil || entry.kind != publicEntryStatic || entry.route == nil {
		return nil, false
	}
	return entry.route, true
}

func (r *modelRouteRegistry) lookupPublicModelEntry(model string) (*publicModelEntry, bool) {
	if r == nil || r.publicEntries == nil {
		return nil, false
	}
	return r.publicEntries.lookup(model)
}

func (r *modelRouteRegistry) lookupTerminalRoute(routeID string) (*modelRoute, bool) {
	snapshot := r.load()
	if snapshot == nil {
		return nil, false
	}
	route, ok := snapshot.byRouteID[strings.TrimSpace(routeID)]
	return route, ok
}

// explicitRoutes retains the catalog-facing behavior expected by the existing
// /v1/models merger. It returns public static routes plus synthetic catalog
// routes for policy entries, never internal terminal routes.
func (r *modelRouteRegistry) explicitRoutes() []*modelRoute {
	if r == nil || r.publicEntries == nil {
		return nil
	}
	entries := r.publicEntries.configuredEntries()
	routes := make([]*modelRoute, 0, len(entries))
	for _, entry := range entries {
		if entry != nil && entry.catalogRoute != nil {
			routes = append(routes, entry.catalogRoute)
		}
	}
	return routes
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

	next := cloneRouteRegistrySnapshot(current, len(models))
	next.legacyOrder = nil
	next.legacyByProvider = make(map[string][]*modelRoute, len(current.legacyByProvider)+1)
	for id, routes := range current.legacyByProvider {
		if id == providerID {
			continue
		}
		cloned := append([]*modelRoute(nil), routes...)
		next.legacyByProvider[id] = cloned
		next.legacyOrder = append(next.legacyOrder, cloned...)
	}

	compiled, err := compileLegacyProviderRoutes(provider, models)
	if err != nil {
		return err
	}
	next.legacyByProvider[providerID] = compiled
	next.legacyOrder = append(next.legacyOrder, compiled...)
	if err := rebuildPublicModelEntrySnapshot(next); err != nil {
		return err
	}
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
	compiled, err := compileLegacyProviderRoutes(provider, models)
	if err != nil {
		return err
	}
	next.legacyByProvider[provider.id] = compiled
	next.legacyOrder = append(next.legacyOrder, compiled...)
	if err := rebuildPublicModelEntrySnapshot(next); err != nil {
		return err
	}
	r.snapshot.Store(next)
	return nil
}

func cloneRouteRegistrySnapshot(current *modelRouteRegistrySnapshot, extra int) *modelRouteRegistrySnapshot {
	next := &modelRouteRegistrySnapshot{
		byRouteID:        make(map[string]*modelRoute, len(current.byRouteID)),
		explicit:         append([]*modelRoute(nil), current.explicit...),
		legacyOrder:      append([]*modelRoute(nil), current.legacyOrder...),
		legacyByProvider: make(map[string][]*modelRoute, len(current.legacyByProvider)+1),
		strictAliases:    current.strictAliases,
		policyEntries:    append([]*publicModelEntry(nil), current.policyEntries...),
		publicEntries:    current.publicEntries,
	}
	for routeID, route := range current.byRouteID {
		next.byRouteID[routeID] = route
	}
	for providerID, routes := range current.legacyByProvider {
		next.legacyByProvider[providerID] = append([]*modelRoute(nil), routes...)
	}
	return next
}

func rebuildPublicModelEntrySnapshot(snapshot *modelRouteRegistrySnapshot) error {
	if snapshot == nil {
		return nil
	}
	public := &publicModelEntryRegistrySnapshot{
		byID:          make(map[string]*publicModelEntry, len(snapshot.explicit)+len(snapshot.policyEntries)+len(snapshot.legacyOrder)),
		aliases:       make(map[string]*publicModelEntry, (len(snapshot.explicit)+len(snapshot.policyEntries)+len(snapshot.legacyOrder))*2),
		configured:    make([]*publicModelEntry, 0, len(snapshot.explicit)+len(snapshot.policyEntries)),
		policyByID:    make(map[string]*publicModelEntry, len(snapshot.policyEntries)),
		strictAliases: snapshot.strictAliases || len(snapshot.explicit) > 0,
	}
	for _, route := range snapshot.explicit {
		entry := newStaticPublicModelEntry(route)
		if entry == nil {
			continue
		}
		if err := addPublicModelEntryToSnapshot(public, entry); err != nil {
			return err
		}
		public.configured = append(public.configured, entry)
	}
	for _, configured := range snapshot.policyEntries {
		entry := clonePublicModelEntry(configured)
		if entry == nil {
			continue
		}
		if existing, ok := public.policyByID[entry.policyID]; ok && existing != entry {
			return fmt.Errorf("policy profile %q is declared more than once", entry.policyID)
		}
		if err := addPublicModelEntryToSnapshot(public, entry); err != nil {
			return err
		}
		public.policyByID[entry.policyID] = entry
		public.configured = append(public.configured, entry)
	}
	for _, route := range snapshot.legacyOrder {
		entry := newStaticPublicModelEntry(route)
		if entry == nil {
			continue
		}
		if err := addPublicModelEntryToSnapshot(public, entry); err != nil {
			return err
		}
	}
	snapshot.publicEntries = public
	return nil
}

func addPublicModelEntryToSnapshot(snapshot *publicModelEntryRegistrySnapshot, entry *publicModelEntry) error {
	if snapshot == nil || entry == nil {
		return nil
	}
	publicID := strings.TrimSpace(entry.id)
	if publicID == "" {
		return fmt.Errorf("public model entry id is required")
	}
	if existing, ok := snapshot.byID[publicID]; ok && existing != entry {
		return publicModelEntryCollisionError(publicID, existing, entry)
	}
	strictAliases := snapshot.strictAliases
	for _, alias := range entry.aliases {
		if existing, ok := snapshot.aliases[alias]; ok && existing != entry {
			// Version-1 legacy catalogs historically allow an exact raw model and
			// its Anthropic-normalized spelling to coexist. Preserve exact-match
			// precedence only for provider-only snapshots. Version-2/3 snapshots
			// enforce the global normalized namespace during refresh as well.
			if strictAliases || !existing.legacy || !entry.legacy {
				return publicModelEntryCollisionError(alias, existing, entry)
			}
		}
		if existing, ok := snapshot.byID[alias]; ok && existing != entry && (strictAliases || !existing.legacy || !entry.legacy) {
			return publicModelEntryCollisionError(alias, existing, entry)
		}
	}
	snapshot.byID[publicID] = entry
	for _, alias := range entry.aliases {
		if existing, ok := snapshot.aliases[alias]; ok && existing != entry && existing.legacy && entry.legacy && !strictAliases {
			continue
		}
		snapshot.aliases[alias] = entry
	}
	return nil
}

func modelRouteAliases(publicID string) []string {
	return configuredPublicModelAliases(publicID)
}

func publicModelEntryCollisionError(alias string, existing, incoming *publicModelEntry) error {
	return fmt.Errorf(
		"model %q is exposed by both route %q and route %q",
		alias,
		publicModelEntryOwnerID(existing),
		publicModelEntryOwnerID(incoming),
	)
}

func modelRouteCollisionError(alias string, existing, incoming *modelRoute) error {
	return publicModelEntryCollisionError(alias, newStaticPublicModelEntry(existing), newStaticPublicModelEntry(incoming))
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
	route := &modelRoute{
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
		exposure: modelRouteExposurePublic,
		legacy:   true,
	}
	ensureModelRouteTargetRevisions(route)
	return route, nil
}

func (ps *providerSetup) routeRegistry() *modelRouteRegistry {
	if ps == nil {
		return nil
	}
	return ps.routes
}

func (ps *providerSetup) lookupRoute(publicID string) (*modelRoute, bool) {
	if ps == nil {
		return nil, false
	}
	ps.modelsMu.RLock()
	defer ps.modelsMu.RUnlock()
	registry := ps.routes
	if registry == nil {
		return nil, false
	}
	return registry.lookup(publicID)
}

func (ps *providerSetup) lookupRouteAlias(publicID string) (*modelRoute, bool) {
	if ps == nil {
		return nil, false
	}
	ps.modelsMu.RLock()
	defer ps.modelsMu.RUnlock()
	registry := ps.routes
	if registry == nil {
		return nil, false
	}
	return registry.lookupAlias(publicID)
}

// lookupPublicModelEntry resolves exact public IDs and their configured
// normalized aliases across static and policy entries.
func (ps *providerSetup) lookupPublicModelEntry(model string) (*publicModelEntry, bool) {
	if ps == nil {
		return nil, false
	}
	ps.modelsMu.RLock()
	defer ps.modelsMu.RUnlock()
	registry := ps.routes
	if registry == nil {
		return nil, false
	}
	return registry.lookupPublicModelEntry(model)
}

// lookupTerminalRoute resolves an explicit terminal route by operational ID.
// Internal routes are intentionally available here but never through the
// client-facing static route resolver.
func (ps *providerSetup) lookupTerminalRoute(routeID string) (*modelRoute, bool) {
	if ps == nil {
		return nil, false
	}
	ps.modelsMu.RLock()
	defer ps.modelsMu.RUnlock()
	registry := ps.routes
	if registry == nil {
		return nil, false
	}
	return registry.lookupTerminalRoute(routeID)
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
	parallelToolCalls := route.public.policy.parallelToolCalls
	if target.wirePolicy.parallelToolCalls != nil {
		parallelToolCalls = target.wirePolicy.parallelToolCalls
	}
	owner := providerModel{
		publicID:               route.public.id,
		upstreamModel:          target.upstreamModel,
		supportedEndpoints:     append([]string(nil), route.public.endpoints...),
		parallelToolCalls:      cloneBoolPtr(parallelToolCalls),
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
	next := cloneRouteRegistrySnapshot(current, 0)
	next.legacyOrder = nil
	next.legacyByProvider = make(map[string][]*modelRoute, len(current.legacyByProvider)+len(replacements))

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

		next.legacyOrder = append(next.legacyOrder, routes...)
		next.legacyByProvider[providerID] = routes
		seenProviders[providerID] = struct{}{}
		return nil
	}

	for _, providerID := range providerOrder {
		if err := emitProvider(providerID); err != nil {
			return err
		}
	}
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

	if err := rebuildPublicModelEntrySnapshot(next); err != nil {
		return err
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
