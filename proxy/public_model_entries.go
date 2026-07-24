package proxy

import (
	"encoding/json"
	"fmt"
	"strings"
)

type publicModelEntryKind uint8

const (
	publicEntryStatic publicModelEntryKind = iota + 1
	publicEntryPolicy
)

// publicModelEntry is the only client-facing model identity compiled into the
// routing registries. Static entries bind directly to one terminal route;
// policy entries carry a normalized profile and a conservative public contract.
type publicModelEntry struct {
	id       string
	kind     publicModelEntryKind
	routeID  string
	policyID string
	contract publicModelContract
	aliases  []string

	route        *modelRoute
	catalogRoute *modelRoute
	policyConfig PolicyProfileConfig
	legacy       bool
}

type publicModelEntryRegistrySnapshot struct {
	byID          map[string]*publicModelEntry
	aliases       map[string]*publicModelEntry
	configured    []*publicModelEntry
	policyByID    map[string]*publicModelEntry
	strictAliases bool
}

// publicModelEntryRegistry is a read-only view over the atomically published
// route-registry snapshot. Terminal and public identities therefore refresh as
// one immutable generation.
type publicModelEntryRegistry struct {
	routes *modelRouteRegistry
}

func (r *publicModelEntryRegistry) load() *publicModelEntryRegistrySnapshot {
	if r == nil || r.routes == nil {
		return nil
	}
	snapshot := r.routes.load()
	if snapshot == nil {
		return nil
	}
	return snapshot.publicEntries
}

func (r *publicModelEntryRegistry) lookup(model string) (*publicModelEntry, bool) {
	snapshot := r.load()
	if snapshot == nil {
		return nil, false
	}
	model = strings.TrimSpace(model)
	if model == "" {
		return nil, false
	}
	if entry, ok := snapshot.byID[model]; ok {
		return entry, true
	}
	if entry, ok := snapshot.aliases[model]; ok {
		return entry, true
	}
	// Match direct-route resolution: request-side normalization strips dated
	// suffixes and maps separator variants before the final alias lookup. Policy
	// entries must use the same public namespace or a normalized policy alias can
	// fall through to unknown-model/default-provider routing.
	normalized := NormalizeModelName(model)
	if normalized != model {
		if entry, ok := snapshot.byID[normalized]; ok {
			return entry, true
		}
		if entry, ok := snapshot.aliases[normalized]; ok {
			return entry, true
		}
	}
	return nil, false
}

func (r *publicModelEntryRegistry) lookupExact(model string) (*publicModelEntry, bool) {
	snapshot := r.load()
	if snapshot == nil {
		return nil, false
	}
	entry, ok := snapshot.byID[strings.TrimSpace(model)]
	return entry, ok
}

func (r *publicModelEntryRegistry) lookupPolicyID(policyID string) (*publicModelEntry, bool) {
	snapshot := r.load()
	if snapshot == nil {
		return nil, false
	}
	entry, ok := snapshot.policyByID[strings.TrimSpace(policyID)]
	return entry, ok
}

func (r *publicModelEntryRegistry) configuredEntries() []*publicModelEntry {
	snapshot := r.load()
	if snapshot == nil {
		return nil
	}
	return append([]*publicModelEntry(nil), snapshot.configured...)
}

func newStaticPublicModelEntry(route *modelRoute) *publicModelEntry {
	if route == nil || !route.isPublic() {
		return nil
	}
	contract := copyPublicModelContract(route.public)
	return &publicModelEntry{
		id:           contract.id,
		kind:         publicEntryStatic,
		routeID:      contract.routeID,
		contract:     contract,
		aliases:      configuredPublicModelAliases(contract.id),
		route:        route,
		catalogRoute: route,
		legacy:       route.legacy,
	}
}

func compilePolicyPublicModelEntries(cfg ProvidersConfig) ([]*publicModelEntry, error) {
	validated, err := validateAndNormalizeProvidersConfig(cfg)
	if err != nil {
		return nil, err
	}
	if !providersConfigSchemaSupportsPolicyRouting(validated.schemaVersion) || len(validated.config.PolicyProfiles) == 0 {
		return nil, nil
	}

	routes := make(map[string]*ModelRouteConfig, len(validated.config.ModelRoutes))
	for index := range validated.config.ModelRoutes {
		route := &validated.config.ModelRoutes[index]
		routes[route.ID] = route
	}

	entries := make([]*publicModelEntry, 0, len(validated.config.PolicyProfiles))
	for profileIndex, profile := range validated.config.PolicyProfiles {
		lightweight := routes[profile.Lightweight.Route]
		powerful := routes[profile.Powerful.Route]
		contract, err := derivePolicyPublicModelContract(profile, lightweight, powerful)
		if err != nil {
			return nil, configPathError(fmt.Sprintf("policy_profiles[%d]", profileIndex), "derive public model contract: %v", err)
		}
		catalogRoute := &modelRoute{
			public:   copyPublicModelContract(contract),
			exposure: modelRouteExposurePublic,
		}
		entries = append(entries, &publicModelEntry{
			id:           profile.PublicID,
			kind:         publicEntryPolicy,
			policyID:     profile.ID,
			contract:     contract,
			aliases:      configuredPublicModelAliases(profile.PublicID),
			catalogRoute: catalogRoute,
			policyConfig: clonePolicyProfileConfig(profile),
		})
	}
	return entries, nil
}

func derivePolicyPublicModelContract(profile PolicyProfileConfig, lightweight, powerful *ModelRouteConfig) (publicModelContract, error) {
	if lightweight == nil || powerful == nil {
		return publicModelContract{}, fmt.Errorf("both terminal route configurations are required")
	}

	parallelToolCalls := boolConfigValue(lightweight.ParallelToolCalls) && boolConfigValue(powerful.ParallelToolCalls)
	vision := false
	modelCfg := ProviderModelConfig{
		PublicID:            profile.PublicID,
		Name:                profile.Name,
		Endpoints:           []string{providerEndpointChatCompletions},
		ModelPickerEnabled:  cloneBoolPtr(profile.ModelPickerEnabled),
		ModelPickerCategory: profile.ModelPickerCategory,
		Vision:              &vision,
		ParallelToolCalls:   &parallelToolCalls,
		ContextWindow:       minimumKnownPolicyContextWindow(lightweight.ContextWindow, powerful.ContextWindow),
	}
	raw, err := synthesizeProviderModelRaw("vekil-policy", profile.PublicID, profile.Name, modelCfg.Endpoints, modelCfg)
	if err != nil {
		return publicModelContract{}, err
	}

	return publicModelContract{
		id: profile.PublicID,
		// The profile id is retained as the catalog owner identity. Policy
		// execution replaces this with the selected terminal route id in the
		// sealed operation plan.
		routeID:   profile.ID,
		name:      profile.Name,
		endpoints: append([]string(nil), modelCfg.Endpoints...),
		raw:       raw,
		policy: providerRequestPolicy{
			parallelToolCalls: &parallelToolCalls,
			// Validation requires equal public request semantics today. OR keeps
			// the derived contract conservative if that invariant is ever relaxed.
			dropSamplingParams: boolConfigValue(lightweight.DropSamplingParams) || boolConfigValue(powerful.DropSamplingParams),
			dropStopSequences:  boolConfigValue(lightweight.DropStopSequences) || boolConfigValue(powerful.DropStopSequences),
		},
	}, nil
}

func minimumKnownPolicyContextWindow(lightweight, powerful *int64) *int64 {
	if lightweight == nil || powerful == nil || *lightweight <= 0 || *powerful <= 0 {
		return nil
	}
	value := *lightweight
	if *powerful < value {
		value = *powerful
	}
	return &value
}

func copyPublicModelContract(contract publicModelContract) publicModelContract {
	contract.endpoints = append([]string(nil), contract.endpoints...)
	contract.raw = append(json.RawMessage(nil), contract.raw...)
	contract.policy.parallelToolCalls = cloneBoolPtr(contract.policy.parallelToolCalls)
	return contract
}

func clonePublicModelEntry(entry *publicModelEntry) *publicModelEntry {
	if entry == nil {
		return nil
	}
	cloned := *entry
	cloned.contract = copyPublicModelContract(entry.contract)
	cloned.aliases = append([]string(nil), entry.aliases...)
	cloned.policyConfig = clonePolicyProfileConfig(entry.policyConfig)
	return &cloned
}

func publicModelEntryOwnerID(entry *publicModelEntry) string {
	if entry == nil {
		return "unknown"
	}
	if entry.kind == publicEntryPolicy {
		if value := strings.TrimSpace(entry.policyID); value != "" {
			return value
		}
	}
	if value := strings.TrimSpace(entry.routeID); value != "" {
		return value
	}
	if value := strings.TrimSpace(entry.contract.routeID); value != "" {
		return value
	}
	return "unknown"
}
