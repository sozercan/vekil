package proxy

import "strings"

// policyDecisionRecord is the bounded, content-free provenance retained for a
// policy decision. It deliberately excludes request text, tool arguments,
// classifier output, rationale, credentials, and physical deployment names.
type policyDecisionRecord struct {
	Category          string
	FailureCategory   string
	ActualTier        policyTier
	ShadowTier        policyTier
	ClassifierLatency int64
	MessageCount      int
	ToolCount         int
	InputBytes        int
	Truncated         bool
}

// chatOperationPlan is the sealed logical execution snapshot produced before
// the first terminal send. Candidate and metadata slices are defensively copied
// so later catalog refreshes or caller mutation cannot change the operation.
type chatOperationPlan struct {
	operationID string

	entryID  string
	publicID string
	routeID  string

	candidates                []targetBinding
	routePolicy               routePolicy
	contract                  publicModelContract
	terminalParallelToolCalls *bool
	selectedReasoningEffort   string

	policyID             string
	selectedTier         policyTier
	effectiveMode        policyMode
	configGeneration     string
	profileGeneration    string
	classifierGeneration string
	binaryGeneration     string
	decision             policyDecisionRecord

	operationRoute *modelRoute
}

type chatOperationPlanOptions struct {
	OperationID string
	EntryID     string
	PublicID    string
	RouteID     string
	Route       *modelRoute
	Contract    publicModelContract
	// SelectedReasoningEffort is the policy-owned canonical Chat value sealed
	// for the selected tier. Empty preserves ordinary non-managed behavior.
	SelectedReasoningEffort string

	PolicyID             string
	SelectedTier         policyTier
	EffectiveMode        policyMode
	ConfigGeneration     string
	ProfileGeneration    string
	ClassifierGeneration string
	BinaryGeneration     string
	Decision             policyDecisionRecord
}

func newChatOperationPlan(options chatOperationPlanOptions) chatOperationPlan {
	publicID := strings.TrimSpace(options.PublicID)
	entryID := strings.TrimSpace(options.EntryID)
	if entryID == "" {
		entryID = publicID
	}
	routeID := strings.TrimSpace(options.RouteID)
	if routeID == "" && options.Route != nil {
		routeID = strings.TrimSpace(options.Route.public.routeID)
	}
	contract := clonePublicModelContract(options.Contract)
	if contract.id == "" {
		contract.id = publicID
	}
	contract.routeID = routeID

	var terminalParallelToolCalls *bool
	if options.Route != nil {
		terminalParallelToolCalls = cloneBoolPtr(options.Route.public.policy.parallelToolCalls)
	}

	plan := chatOperationPlan{
		operationID:               strings.TrimSpace(options.OperationID),
		terminalParallelToolCalls: terminalParallelToolCalls,
		entryID:                   entryID,
		publicID:                  publicID,
		routeID:                   routeID,
		contract:                  contract,
		selectedReasoningEffort:   strings.TrimSpace(options.SelectedReasoningEffort),
		policyID:                  strings.TrimSpace(options.PolicyID),
		selectedTier:              options.SelectedTier,
		effectiveMode:             options.EffectiveMode,
		configGeneration:          strings.TrimSpace(options.ConfigGeneration),
		profileGeneration:         strings.TrimSpace(options.ProfileGeneration),
		classifierGeneration:      strings.TrimSpace(options.ClassifierGeneration),
		binaryGeneration:          strings.TrimSpace(options.BinaryGeneration),
		decision:                  options.Decision,
	}
	if options.Route != nil {
		plan.candidates = cloneTargetBindings(options.Route.targets)
		for index := range plan.candidates {
			plan.candidates[index].wirePolicy.parallelToolCalls = cloneBoolPtr(terminalParallelToolCalls)
		}
		plan.routePolicy = options.Route.policy
		plan.operationRoute = clonePolicyOperationRoute(options.Route, contract, plan.candidates)
		plan.operationRoute.policyTier = options.SelectedTier
	}
	return plan
}

func clonePublicModelContract(contract publicModelContract) publicModelContract {
	contract.endpoints = append([]string(nil), contract.endpoints...)
	contract.raw = append([]byte(nil), contract.raw...)
	contract.policy.parallelToolCalls = cloneBoolPtr(contract.policy.parallelToolCalls)
	return contract
}

func clonePolicyOperationRoute(route *modelRoute, contract publicModelContract, candidates []targetBinding) *modelRoute {
	if route == nil {
		return nil
	}
	cloned := *route
	physicalEndpoints := append([]string(nil), route.public.endpoints...)
	cloned.public = clonePublicModelContract(contract)
	// The policy contract remains the client-visible Chat identity, while the
	// sealed operation must retain the selected terminal route's physical wire
	// endpoints so a Responses-backed target can execute Chat compatibility in
	// process.
	cloned.public.endpoints = physicalEndpoints
	cloned.targets = cloneTargetBindings(candidates)
	cloned.legacy = false
	return &cloned
}

func (p chatOperationPlan) valid() bool {
	return p.operationRoute != nil && p.publicID != "" && p.routeID != "" && len(p.candidates) > 0
}

// allowsResponsesReplayPassthrough reports whether a policy-planned Chat
// continuation can safely preserve replay state owned by a downstream bridge.
// Off and observe always select the baseline tier; requiring one sealed target
// keeps that continuation pinned to the same deterministic route destination.
// Target determinism does not create replica affinity: deployments that accept
// downstream process-local replay IDs must use one bridge process or sticky
// ingress to the replay-owning bridge, as documented for replay state.
func (p chatOperationPlan) allowsResponsesReplayPassthrough() bool {
	if !p.valid() || len(p.candidates) != 1 {
		return false
	}
	return p.effectiveMode == policyModeOff || p.effectiveMode == policyModeObserve
}

func (p chatOperationPlan) routeSnapshot() *modelRoute {
	if p.operationRoute == nil {
		return nil
	}
	return clonePolicyOperationRoute(p.operationRoute, p.contract, p.operationRoute.targets)
}

func cloneTargetBindings(candidates []targetBinding) []targetBinding {
	cloned := append([]targetBinding(nil), candidates...)
	for index := range cloned {
		cloned[index].wirePolicy.parallelToolCalls = cloneBoolPtr(cloned[index].wirePolicy.parallelToolCalls)
		cloned[index].legacyOwner = cloneProviderModelForRoute(cloned[index].legacyOwner)
	}
	return cloned
}

func (p chatOperationPlan) candidateSnapshot() []targetBinding {
	if p.operationRoute != nil {
		return cloneTargetBindings(p.operationRoute.targets)
	}
	return cloneTargetBindings(p.candidates)
}
