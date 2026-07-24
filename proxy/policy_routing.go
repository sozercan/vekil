package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const policyClassifierResponseLimit = 64 << 10

type compiledPolicyProfile struct {
	config PolicyProfileConfig
	entry  *publicModelEntry

	lightweight *modelRoute
	powerful    *modelRoute
	classifier  *modelRoute

	baselineTier    policyTier
	unavailableTier policyTier
	uncertainTier   policyTier

	mode              atomic.Uint32
	preflightRequired bool
	preflightReady    atomic.Bool

	classifierAdapter policyClassifier
	classifierRuntime *policyClassifierRuntime
	admission         *policyAdmission
	breaker           *policyBreaker

	configGeneration     string
	profileGeneration    string
	classifierGeneration string
	binaryGeneration     string
}

func (p *compiledPolicyProfile) effectiveMode() policyMode {
	if p == nil {
		return policyModeOff
	}
	return policyMode(p.mode.Load())
}

func (p *compiledPolicyProfile) statsID() string {
	if p == nil {
		return ""
	}
	if p.entry != nil && strings.TrimSpace(p.entry.id) != "" {
		return p.entry.id
	}
	return strings.TrimSpace(p.config.PublicID)
}

func (p *compiledPolicyProfile) setEffectiveMode(mode policyMode) {
	if p != nil {
		p.mode.Store(uint32(mode))
	}
}

func (p *compiledPolicyProfile) routeForTier(tier policyTier) *modelRoute {
	if p == nil {
		return nil
	}
	if tier == policyTierPowerful {
		return p.powerful
	}
	return p.lightweight
}

func (p *compiledPolicyProfile) reasoningEffortForTier(tier policyTier) string {
	if p == nil || p.config.TierReasoningEffort == nil {
		return ""
	}
	if tier == policyTierPowerful {
		return p.config.TierReasoningEffort.Powerful
	}
	return p.config.TierReasoningEffort.Lightweight
}

type chatPolicyRoutingController struct {
	h *ProxyHandler

	profiles        map[string]*compiledPolicyProfile
	ordered         []*compiledPolicyProfile
	breakerProfiles map[string][]*compiledPolicyProfile
	stats           *policyStatsCollector

	active bool

	diagnosticMu sync.RWMutex
	diagnostic   string
}

func newChatPolicyRoutingController(h *ProxyHandler, cfg ProvidersConfig, global PolicyRoutingMode) (*chatPolicyRoutingController, error) {
	validated, err := validateAndNormalizeProvidersConfig(cfg)
	if err != nil {
		return nil, err
	}
	cfg = validated.config
	setup := h.providerSetup()
	cfg.PolicyProfiles = policyProfilesWithinAllowedModelScope(h, setup, cfg.PolicyProfiles)
	if len(cfg.PolicyProfiles) == 0 {
		return nil, nil
	}
	controller := &chatPolicyRoutingController{
		h:               h,
		profiles:        make(map[string]*compiledPolicyProfile, len(cfg.PolicyProfiles)),
		ordered:         make([]*compiledPolicyProfile, 0, len(cfg.PolicyProfiles)),
		breakerProfiles: make(map[string][]*compiledPolicyProfile),
		stats:           newPolicyStatsCollector(),
	}
	configGeneration := policyConfigGeneration(cfg)
	binaryGeneration := policyBinaryGeneration()
	globalMode := global.internal()
	sharedBreakers := make(map[string]*policyBreaker)
	var sharedAdmission *policyAdmission

	for index, profileCfg := range cfg.PolicyProfiles {
		entry, ok := setup.lookupPublicModelEntry(profileCfg.PublicID)
		if !ok || entry == nil || entry.kind != publicEntryPolicy {
			return nil, configPathError(fmt.Sprintf("policy_profiles[%d].public_id", index), "compiled public policy entry %q is unavailable", profileCfg.PublicID)
		}
		lightweight, ok := setup.lookupTerminalRoute(profileCfg.LightweightRoute)
		if !ok || lightweight == nil {
			return nil, configPathError(fmt.Sprintf("policy_profiles[%d].lightweight_route", index), "compiled terminal route %q is unavailable", profileCfg.LightweightRoute)
		}
		powerful, ok := setup.lookupTerminalRoute(profileCfg.PowerfulRoute)
		if !ok || powerful == nil {
			return nil, configPathError(fmt.Sprintf("policy_profiles[%d].powerful_route", index), "compiled terminal route %q is unavailable", profileCfg.PowerfulRoute)
		}
		classifierRoute, ok := setup.lookupTerminalRoute(profileCfg.Classifier.Route)
		if !ok || classifierRoute == nil {
			return nil, configPathError(fmt.Sprintf("policy_profiles[%d].classifier.route", index), "compiled classifier route %q is unavailable", profileCfg.Classifier.Route)
		}
		profileMode, err := parsePolicyMode(profileCfg.Mode)
		if err != nil {
			return nil, err
		}
		baselineTier, err := parsePolicyTier(profileCfg.BaselineTier)
		if err != nil {
			return nil, err
		}
		unavailableTier, err := parsePolicyTier(profileCfg.ClassifierUnavailableTier)
		if err != nil {
			return nil, err
		}
		uncertainTier, err := parsePolicyTier(profileCfg.ClassifierUncertainTier)
		if err != nil {
			return nil, err
		}
		if sharedAdmission == nil {
			sharedAdmission = newPolicyAdmission(policyDefaultGlobalConcurrency, profileCfg.Classifier.MaxConcurrency)
		} else {
			sharedAdmission = sharedAdmission.forProfile(profileCfg.Classifier.MaxConcurrency)
		}
		admission := sharedAdmission
		breaker := sharedBreakers[classifierRoute.public.routeID]
		if breaker == nil {
			breaker = newPolicyBreaker(nil)
			sharedBreakers[classifierRoute.public.routeID] = breaker
		}
		classifierAdapter, err := newRoutePolicyClassifier(h, classifierRoute, profileCfg, controller.stats)
		if err != nil {
			return nil, configPathError(fmt.Sprintf("policy_profiles[%d].classifier", index), "%v", err)
		}
		profile := &compiledPolicyProfile{
			config:               clonePolicyProfileConfig(profileCfg),
			entry:                clonePublicModelEntry(entry),
			lightweight:          lightweight,
			powerful:             powerful,
			classifier:           classifierRoute,
			baselineTier:         baselineTier,
			unavailableTier:      unavailableTier,
			uncertainTier:        uncertainTier,
			classifierAdapter:    classifierAdapter,
			admission:            admission,
			breaker:              breaker,
			configGeneration:     configGeneration,
			profileGeneration:    policyProfileGeneration(profileCfg, entry.contract, lightweight, powerful),
			classifierGeneration: policyClassifierGeneration(classifierRoute),
			binaryGeneration:     binaryGeneration,
		}
		profile.classifierRuntime = newPolicyClassifierRuntime(classifierAdapter, admission, breaker)
		effectiveMode := effectivePolicyMode(globalMode, profileMode)
		profile.setEffectiveMode(effectiveMode)
		profile.preflightRequired = effectiveMode != policyModeOff
		if effectiveMode != policyModeOff {
			controller.active = true
		}
		controller.profiles[profileCfg.ID] = profile
		controller.ordered = append(controller.ordered, profile)
		controller.breakerProfiles[classifierRoute.public.routeID] = append(controller.breakerProfiles[classifierRoute.public.routeID], profile)
		preflightState := policyStatsPreflightNotRequired
		if effectiveMode != policyModeOff {
			preflightState = policyStatsPreflightPending
		}
		controller.stats.setProfileState(entry.id, policyStatsProfileState{
			EffectiveMode:            effectiveMode.String(),
			PreflightState:           preflightState,
			BreakerState:             policyStatsBreakerClosed,
			ConfigGenerationHash:     configGeneration,
			ProfileGenerationHash:    profile.profileGeneration,
			ClassifierGenerationHash: profile.classifierGeneration,
			BinaryGenerationHash:     binaryGeneration,
		})
	}
	return controller, nil
}

func policyProfilesWithinAllowedModelScope(h *ProxyHandler, setup *providerSetup, profiles []PolicyProfileConfig) []PolicyProfileConfig {
	if h == nil || setup == nil || len(h.allowedModels) == 0 {
		return profiles
	}

	allowedPolicyIDs := make(map[string]struct{}, len(h.allowedModels))
	for model := range h.allowedModels {
		entry, ok := setup.lookupPublicModelEntry(model)
		if !ok || entry == nil || entry.kind != publicEntryPolicy {
			continue
		}
		allowedPolicyIDs[entry.policyID] = struct{}{}
	}
	if len(allowedPolicyIDs) == 0 {
		return nil
	}

	filtered := make([]PolicyProfileConfig, 0, len(allowedPolicyIDs))
	for _, profile := range profiles {
		if _, ok := allowedPolicyIDs[profile.ID]; ok {
			filtered = append(filtered, profile)
		}
	}
	return filtered
}

func (c *chatPolicyRoutingController) Active() bool { return c != nil && c.active }

func (c *chatPolicyRoutingController) ReadinessDiagnostic() string {
	if c == nil {
		return ""
	}
	c.diagnosticMu.RLock()
	defer c.diagnosticMu.RUnlock()
	return c.diagnostic
}

func (c *chatPolicyRoutingController) setDiagnostic(value string) {
	if c == nil {
		return
	}
	value = strings.TrimSpace(value)
	if len(value) > 512 {
		value = value[:512]
	}
	c.diagnosticMu.Lock()
	c.diagnostic = value
	c.diagnosticMu.Unlock()
}

func (c *chatPolicyRoutingController) Initialize(ctx context.Context) error {
	if c == nil || !c.active {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	groups := make(map[string][]*compiledPolicyProfile)
	order := make([]string, 0)
	for _, profile := range c.ordered {
		if profile.effectiveMode() == policyModeOff {
			continue
		}
		routeID := profile.classifier.public.routeID
		if _, exists := groups[routeID]; !exists {
			order = append(order, routeID)
		}
		groups[routeID] = append(groups[routeID], profile)
	}
	sort.Strings(order)
	var observeFailures []string
	for _, routeID := range order {
		profiles := groups[routeID]
		if len(profiles) == 0 {
			continue
		}
		profile := profiles[0]
		preflightCtx, cancel := c.h.newPolicyClassificationContext(ctx, time.Duration(profile.config.Classifier.TimeoutMS)*time.Millisecond)
		preflightCtx = withPolicyStatsBucket(preflightCtx, policyStatsTrafficBucketPreflight)
		preflightCtx, dispatchEvidence := withPolicyClassifierDispatchEvidence(preflightCtx)
		start := time.Now()
		_, err := profile.classifierAdapter.Classify(preflightCtx, policyPreflightFacts())
		cancel()
		latency := time.Since(start)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if errors.Is(context.Cause(preflightCtx), errProxyLifecycleShutdown) {
			return errProxyLifecycleShutdown
		}
		if err == nil {
			for profileIndex, candidate := range profiles {
				candidate.preflightReady.Store(true)
				c.stats.setProfileState(candidate.statsID(), policyStatsProfileState{
					EffectiveMode:            candidate.effectiveMode().String(),
					PreflightState:           policyStatsPreflightReady,
					BreakerState:             policyStatsBreakerClosed,
					ConfigGenerationHash:     candidate.configGeneration,
					ProfileGenerationHash:    candidate.profileGeneration,
					ClassifierGenerationHash: candidate.classifierGeneration,
					BinaryGenerationHash:     candidate.binaryGeneration,
				})
				physicalSends := int64(0)
				if profileIndex == 0 && dispatchEvidence.dispatched.Load() {
					physicalSends = 1
				}
				c.stats.record(policyStatsObservation{Profile: candidate.statsID(), TrafficBucket: policyStatsTrafficBucketPreflight, ClassifierOutcome: policyStatsClassifierCompletion, ClassifierLatency: latency, PhysicalClassifierSends: physicalSends})
			}
			continue
		}

		hasEnforce := false
		for _, candidate := range profiles {
			if candidate.effectiveMode() == policyModeEnforce {
				hasEnforce = true
			}
		}
		readinessMessage := "policy classifier preflight failed"
		detailMessage := fmt.Sprintf("policy classifier preflight failed for route %q", routeID)
		failureCategory := policyFailureStatsReason(policyClassifierFailureFromError(err).Category)
		for profileIndex, candidate := range profiles {
			candidate.preflightReady.Store(false)
			if !hasEnforce {
				candidate.setEffectiveMode(policyModeOff)
			}
			c.stats.setProfileState(candidate.statsID(), policyStatsProfileState{
				EffectiveMode:            candidate.effectiveMode().String(),
				PreflightState:           policyStatsPreflightFailed,
				BreakerState:             policyStatsBreakerClosed,
				ConfigGenerationHash:     candidate.configGeneration,
				ProfileGenerationHash:    candidate.profileGeneration,
				ClassifierGenerationHash: candidate.classifierGeneration,
				BinaryGenerationHash:     candidate.binaryGeneration,
			})
			physicalSends := int64(0)
			if profileIndex == 0 && dispatchEvidence.dispatched.Load() {
				physicalSends = 1
			}
			c.stats.record(policyStatsObservation{Profile: candidate.statsID(), TrafficBucket: policyStatsTrafficBucketPreflight, ClassifierOutcome: policyStatsClassifierUnavailable, DropReason: failureCategory, ClassifierLatency: latency, PhysicalClassifierSends: physicalSends})
		}
		if hasEnforce {
			c.setDiagnostic(readinessMessage)
			return fmt.Errorf("%s: %w", detailMessage, err)
		}
		observeFailures = append(observeFailures, fmt.Sprintf("%s: %v", detailMessage, err))
	}
	if len(observeFailures) > 0 {
		c.setDiagnostic(strings.Join(observeFailures, "; "))
		return nil
	}
	allRequiredReady := true
	for _, profile := range c.ordered {
		if profile.preflightRequired && !profile.preflightReady.Load() {
			allRequiredReady = false
			break
		}
	}
	if allRequiredReady {
		c.setDiagnostic("")
	}
	return nil
}

func policyPreflightFacts() policyClassifierFacts {
	text := "Inspect one small function and report its behavior."
	return policyClassifierFacts{
		SchemaVersion: policyFactSchemaVersion,
		FirstUserTask: &policyFactMessage{Role: policyFactRoleUser, Text: text, OriginalBytes: len(text)},
		Counts:        policyFactCounts{Messages: 1, UserMessages: 1, TextMessages: 1, TaskOriginalBytes: len(text)},
	}
}

func (c *chatPolicyRoutingController) Plan(ctx context.Context, input chatPolicyInput) (chatOperationPlan, error) {
	if c == nil || c.h == nil {
		return chatOperationPlan{}, nil
	}
	input = input.normalized()
	entry, known := c.h.providerSetup().lookupPublicModelEntry(input.Model)
	if !known || entry == nil || entry.kind != publicEntryPolicy {
		return chatOperationPlan{}, nil
	}
	profile := c.profiles[entry.policyID]
	if profile == nil {
		return chatOperationPlan{}, &providerRequestError{statusCode: http.StatusInternalServerError, err: fmt.Errorf("policy profile %q is unavailable", entry.policyID)}
	}
	mode := profile.effectiveMode()
	if mode != policyModeOff && !profile.preflightReady.Load() {
		message := "policy classifier preflight has not completed"
		if !c.h.PolicyRoutingPreflightPending() {
			message = "policy classifier preflight failed"
		}
		return chatOperationPlan{}, &providerRequestError{statusCode: http.StatusServiceUnavailable, err: fmt.Errorf("%s for policy model %q", message, entry.id)}
	}
	var replayRoute *modelRoute
	var replayTier policyTier
	downstreamReplayPassthrough := false
	if chatRequestContainsResponsesReplayID(input.OriginalBody) {
		var replayErr error
		replayRoute, replayTier, replayErr = c.resolvePolicyResponsesReplayRoute(profile, input.OriginalBody)
		if replayErr != nil {
			baselineRoute := profile.routeForTier(profile.baselineTier)
			downstreamReplayPassthrough = (mode == policyModeOff || mode == policyModeObserve) &&
				baselineRoute != nil && len(baselineRoute.targets) == 1 &&
				explicitRouteHasChatBackend(baselineRoute, providerEndpointChatCompletions) &&
				(isMissingResponsesChatReplayError(replayErr) || !explicitRouteHasChatBackend(baselineRoute, providerEndpointResponses))
			if !downstreamReplayPassthrough {
				return chatOperationPlan{}, replayErr
			}
		}
	}
	if replayRoute == nil && !downstreamReplayPassthrough {
		if err := c.validateResponsesBackedPolicyRequest(profile, input.OriginalBody); err != nil {
			return chatOperationPlan{}, &providerRequestError{
				statusCode: http.StatusBadRequest,
				err:        fmt.Errorf("policy model %q request is outside its shared terminal contract: %w", entry.id, err),
			}
		}
	}
	facts, err := buildPolicyClassifierFacts(input.OriginalBody, policyFactOptions{
		RecentTurns:     profile.config.Classifier.RecentTurns,
		MaxRequestBytes: profile.config.Classifier.MaxRequestBytes,
	})
	if err != nil {
		return chatOperationPlan{}, &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("policy model %q supports text and standard function tools only: %w", entry.id, err)}
	}
	if err := validatePolicyPublicRequestContract(input.OriginalBody, profile.entry.contract, profile.config.TierReasoningEffort != nil); err != nil {
		return chatOperationPlan{}, &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("policy model %q request is outside its public contract: %w", entry.id, err)}
	}
	if err := ctx.Err(); err != nil {
		return chatOperationPlan{}, err
	}
	bucket := policyTrafficBucket(len(input.OriginalBody), facts.Counts.FunctionTools)
	if replayRoute != nil {
		c.stats.record(policyStatsObservation{Profile: profile.statsID(), TrafficBucket: bucket, Eligible: true, ActualTier: replayTier.String()})
		return c.sealRoutePlan(profile, input, facts, replayRoute, replayTier, policyDecisionRecord{Category: "replay_binding", ActualTier: replayTier}), nil
	}
	if mode == policyModeOff {
		c.stats.record(policyStatsObservation{Profile: profile.statsID(), TrafficBucket: bucket, Eligible: true, ActualTier: profile.baselineTier.String()})
		return c.sealPlan(profile, input, facts, profile.baselineTier, policyDecisionRecord{Category: "baseline", ActualTier: profile.baselineTier}), nil
	}
	if mode == policyModeObserve {
		c.launchObservation(ctx, profile, input, facts, bucket)
		return c.sealPlan(profile, input, facts, profile.baselineTier, policyDecisionRecord{Category: "observe_baseline", ActualTier: profile.baselineTier}), nil
	}
	return c.enforce(ctx, profile, input, facts, bucket)
}

func (c *chatPolicyRoutingController) validateResponsesBackedPolicyRequest(profile *compiledPolicyProfile, body []byte) error {
	if c == nil || c.h == nil || profile == nil {
		return nil
	}
	seen := make(map[string]struct{}, 2)
	for _, route := range []*modelRoute{profile.lightweight, profile.powerful} {
		if route == nil {
			continue
		}
		routeID := strings.TrimSpace(route.public.routeID)
		if _, duplicate := seen[routeID]; duplicate {
			continue
		}
		seen[routeID] = struct{}{}
		if explicitRouteHasChatBackend(route, providerEndpointChatCompletions) || !explicitRouteHasChatBackend(route, providerEndpointResponses) {
			continue
		}
		if _, _, err := c.h.prepareExplicitResponsesChatRequest(nil, route, body, chatExecutionOptions{}); err != nil {
			return err
		}
	}
	return nil
}

func (c *chatPolicyRoutingController) resolvePolicyResponsesReplayRoute(profile *compiledPolicyProfile, body []byte) (*modelRoute, policyTier, error) {
	if c == nil || c.h == nil || profile == nil {
		return nil, policyTierUnknown, &providerRequestError{statusCode: http.StatusBadRequest, err: fmt.Errorf("policy model does not support Responses replay continuations")}
	}
	type candidate struct {
		route *modelRoute
		tier  policyTier
	}
	candidates := []candidate{{route: profile.lightweight, tier: policyTierLightweight}, {route: profile.powerful, tier: policyTierPowerful}}
	responsesCapable := false
	var missing error
	for _, candidate := range candidates {
		if candidate.route == nil || !candidate.route.supportsEndpoint(providerEndpointResponses) {
			continue
		}
		for _, target := range candidate.route.targets {
			if target.provider == nil || !target.provider.supportsEndpoint(providerEndpointResponses) {
				continue
			}
			responsesCapable = true
			upstreamModel := strings.TrimSpace(target.upstreamModel)
			if upstreamModel == "" {
				upstreamModel = profile.entry.id
			}
			_, err := translateChatRequestToResponses(body, responsesChatRequestOptions{
				UpstreamModel: upstreamModel,
				ReplayStore:   c.h.responsesChatReplayStore(),
				ReplayRoute: responsesChatReplayRoute{
					ProviderID:    target.provider.id,
					PublicModel:   profile.entry.id,
					UpstreamModel: upstreamModel,
					RouteID:       candidate.route.public.routeID,
					PolicyTier:    candidate.tier.String(),
				},
				DropSamplingParams: candidate.route.public.policy.dropSamplingParams,
			})
			if err == nil {
				return candidate.route, candidate.tier, nil
			}
			if isMissingResponsesChatReplayError(err) {
				missing = err
				continue
			}
			return nil, policyTierUnknown, err
		}
	}
	if !responsesCapable {
		return nil, policyTierUnknown, &providerRequestError{
			statusCode: http.StatusBadRequest,
			err:        fmt.Errorf("policy model %q does not support Responses replay continuations", profile.entry.id),
		}
	}
	if missing == nil {
		missing = missingResponsesChatReplayError()
	}
	return nil, policyTierUnknown, missing
}

func validatePolicyPublicRequestContract(body []byte, contract publicModelContract, policyControlsReasoning bool) error {
	root, err := decodePolicyFactObject(body, "")
	if err != nil {
		return err
	}
	if rawParallel, ok := root["parallel_tool_calls"]; ok && !policyFactJSONNull(rawParallel) {
		var requested bool
		if json.Unmarshal(rawParallel, &requested) != nil {
			return fmt.Errorf("parallel_tool_calls must be boolean")
		}
		if requested && (contract.policy.parallelToolCalls == nil || !*contract.policy.parallelToolCalls) {
			return fmt.Errorf("parallel_tool_calls is not supported")
		}
	}
	if rawEffort, ok := root["reasoning_effort"]; ok && !policyFactJSONNull(rawEffort) {
		var effort string
		if json.Unmarshal(rawEffort, &effort) != nil || strings.TrimSpace(effort) == "" {
			return fmt.Errorf("reasoning_effort must be a non-empty string")
		}
		if !policyControlsReasoning {
			return fmt.Errorf("reasoning_effort is not supported")
		}
	}
	return nil
}

func (c *chatPolicyRoutingController) enforce(ctx context.Context, profile *compiledPolicyProfile, input chatPolicyInput, facts policyClassifierFacts, bucket string) (chatOperationPlan, error) {
	classifyCtx, cancel := c.h.newPolicyClassificationContext(ctx, time.Duration(profile.config.Classifier.TimeoutMS)*time.Millisecond)
	classifyCtx = withPolicyStatsBucket(classifyCtx, bucket)
	classifyCtx, dispatchEvidence := withPolicyClassifierDispatchEvidence(classifyCtx)
	start := time.Now()
	result := profile.classifierRuntime.classify(classifyCtx, facts)
	c.refreshBreakerStats(profile.classifier.public.routeID)
	latency := time.Since(start)
	cause := context.Cause(classifyCtx)
	cancel()
	tier, mapped := mapPolicyClassifierResult(result, facts, profile.unavailableTier, profile.uncertainTier)
	observation := policyObservationForResult(profile, bucket, result, latency, dispatchEvidence.dispatched.Load())
	observation.Eligible = true
	observation.Admitted = policyResultWasAdmitted(result)
	if mapped && !errors.Is(cause, errProxyLifecycleShutdown) && (ctx == nil || ctx.Err() == nil) {
		observation.ActualTier = tier.String()
	}
	c.stats.record(observation)
	if errors.Is(cause, errProxyLifecycleShutdown) {
		return chatOperationPlan{}, errProxyLifecycleShutdown
	}
	if ctx != nil && ctx.Err() != nil {
		return chatOperationPlan{}, ctx.Err()
	}
	if !mapped {
		return chatOperationPlan{}, context.Canceled
	}
	category := "classified"
	switch result.Category {
	case policyClassifierResultUnavailable:
		category = "unavailable_fallback"
	case policyClassifierResultUncertain:
		category = "uncertain_fallback"
	}
	decision := policyDecisionRecord{
		Category:          category,
		FailureCategory:   string(result.Failure.Category),
		ActualTier:        tier,
		ClassifierLatency: latency.Milliseconds(),
		MessageCount:      facts.Counts.Messages,
		ToolCount:         facts.Counts.FunctionTools,
		InputBytes:        len(input.OriginalBody),
		Truncated:         facts.truncated(),
	}
	return c.sealPlan(profile, input, facts, tier, decision), nil
}

func (c *chatPolicyRoutingController) launchObservation(ctx context.Context, profile *compiledPolicyProfile, input chatPolicyInput, facts policyClassifierFacts, bucket string) {
	if ctx == nil || ctx.Err() != nil {
		c.stats.record(policyStatsObservation{Profile: profile.statsID(), TrafficBucket: bucket, Eligible: true, DropReason: policyStatsDropReasonCanceled, ActualTier: profile.baselineTier.String()})
		return
	}
	if !deterministicPolicySample(input.OperationID, profile.config.ID, profile.config.Classifier.ObserveSampleRate) {
		c.stats.record(policyStatsObservation{Profile: profile.statsID(), TrafficBucket: bucket, Eligible: true, DropReason: policyStatsDropReasonNotSampled, ActualTier: profile.baselineTier.String()})
		return
	}
	lease, failure := profile.admission.tryAcquire()
	if failure != policyClassifierFailureNone {
		c.stats.record(policyStatsObservation{Profile: profile.statsID(), TrafficBucket: bucket, Eligible: true, Sampled: true, DropReason: policyFailureStatsReason(failure), ActualTier: profile.baselineTier.String()})
		return
	}
	permit, ok := profile.breaker.tryAcquire()
	if !ok {
		lease.release()
		c.refreshBreakerStats(profile.classifier.public.routeID)
		c.stats.record(policyStatsObservation{Profile: profile.statsID(), TrafficBucket: bucket, Eligible: true, Sampled: true, DropReason: policyStatsDropReasonBreakerOpen, ActualTier: profile.baselineTier.String()})
		return
	}
	if !c.h.beginLifecycleWorker() {
		permit.releaseNeutral()
		lease.release()
		c.stats.record(policyStatsObservation{Profile: profile.statsID(), TrafficBucket: bucket, Eligible: true, Sampled: true, DropReason: policyStatsDropReasonCanceled, ActualTier: profile.baselineTier.String()})
		return
	}
	c.stats.record(policyStatsObservation{Profile: profile.statsID(), TrafficBucket: bucket, Eligible: true, Sampled: true, Admitted: true, ActualTier: profile.baselineTier.String()})
	go func() {
		defer c.h.endLifecycleWorker()
		defer lease.release()
		observeCtx, cancel := c.h.newPolicyObserveContext(time.Duration(profile.config.Classifier.TimeoutMS) * time.Millisecond)
		observeCtx = withPolicyStatsBucket(observeCtx, bucket)
		observeCtx, dispatchEvidence := withPolicyClassifierDispatchEvidence(observeCtx)
		start := time.Now()
		signals, err := profile.classifierAdapter.Classify(observeCtx, facts)
		latency := time.Since(start)
		cancel()
		result := newPolicyClassifierResult(signals, err)
		completePolicyBreakerPermit(permit, result, err)
		c.refreshBreakerStats(profile.classifier.public.routeID)
		shadow, mapped := mapPolicyClassifierResult(result, facts, profile.unavailableTier, profile.uncertainTier)
		observation := policyObservationForResult(profile, bucket, result, latency, dispatchEvidence.dispatched.Load())
		if mapped {
			observation.ShadowTier = shadow.String()
		}
		c.stats.record(observation)
	}()
}

func completePolicyBreakerPermit(permit *policyBreakerPermit, result policyClassifierResult, err error) {
	if permit == nil {
		return
	}
	failure := result.Failure
	switch {
	case err == nil:
		permit.recordSuccess()
	case failure.HTTPAccepted:
		permit.recordSuccess()
	case failure.AffectsBreaker:
		permit.recordFailure(failure)
	default:
		permit.releaseNeutral()
	}
}

func policyObservationForResult(profile *compiledPolicyProfile, bucket string, result policyClassifierResult, latency time.Duration, dispatched bool) policyStatsObservation {
	observation := policyStatsObservation{Profile: profile.statsID(), TrafficBucket: bucket, ClassifierLatency: latency}
	if dispatched {
		observation.PhysicalClassifierSends = 1
	}
	switch result.Category {
	case policyClassifierResultClassified:
		observation.ClassifierOutcome = policyStatsClassifierCompletion
	case policyClassifierResultUncertain:
		if result.Failure.Category == policyClassifierFailureAbstained {
			observation.ClassifierOutcome = policyStatsClassifierAbstain
		} else {
			observation.ClassifierOutcome = policyStatsClassifierUncertain
		}
		observation.DropReason = policyFailureStatsReason(result.Failure.Category)
	case policyClassifierResultUnavailable:
		observation.ClassifierOutcome = policyStatsClassifierUnavailable
		observation.DropReason = policyFailureStatsReason(result.Failure.Category)
	case policyClassifierResultCanceled:
		observation.DropReason = policyStatsDropReasonCanceled
	}
	return observation
}

func policyResultWasAdmitted(result policyClassifierResult) bool {
	return result.Admitted
}

func policyFailureStatsReason(category policyClassifierFailureCategory) string {
	if category == policyClassifierFailureNone {
		return ""
	}
	return string(category)
}

func policyTrafficBucket(requestBytes, tools int) string {
	byteBucket := "bytes_gt_64k"
	switch {
	case requestBytes <= 4<<10:
		byteBucket = "bytes_le_4k"
	case requestBytes <= 16<<10:
		byteBucket = "bytes_le_16k"
	case requestBytes <= 64<<10:
		byteBucket = "bytes_le_64k"
	}
	toolBucket := "tools_65_plus"
	switch {
	case tools == 0:
		toolBucket = "tools_0"
	case tools <= 4:
		toolBucket = "tools_1_4"
	case tools <= 16:
		toolBucket = "tools_5_16"
	case tools <= 64:
		toolBucket = "tools_17_64"
	}
	return byteBucket + "/" + toolBucket
}

func (c *chatPolicyRoutingController) sealPlan(profile *compiledPolicyProfile, input chatPolicyInput, facts policyClassifierFacts, tier policyTier, decision policyDecisionRecord) chatOperationPlan {
	route := profile.routeForTier(tier)
	contract := copyPublicModelContract(profile.entry.contract)
	contract.id = profile.entry.id
	contract.routeID = route.public.routeID
	decision.MessageCount = facts.Counts.Messages
	decision.ToolCount = facts.Counts.FunctionTools
	decision.InputBytes = len(input.OriginalBody)
	decision.Truncated = decision.Truncated || facts.truncated()
	return newChatOperationPlan(chatOperationPlanOptions{
		OperationID:             input.OperationID,
		EntryID:                 profile.entry.id,
		PublicID:                profile.entry.id,
		RouteID:                 route.public.routeID,
		Route:                   route,
		Contract:                contract,
		SelectedReasoningEffort: profile.reasoningEffortForTier(tier),
		PolicyID:                profile.config.ID,
		SelectedTier:            tier,
		EffectiveMode:           profile.effectiveMode(),
		ConfigGeneration:        profile.configGeneration,
		ProfileGeneration:       profile.profileGeneration,
		ClassifierGeneration:    profile.classifierGeneration,
		BinaryGeneration:        profile.binaryGeneration,
		Decision:                decision,
	})
}

func (c *chatPolicyRoutingController) sealRoutePlan(profile *compiledPolicyProfile, input chatPolicyInput, facts policyClassifierFacts, route *modelRoute, tier policyTier, decision policyDecisionRecord) chatOperationPlan {
	contract := copyPublicModelContract(profile.entry.contract)
	contract.id = profile.entry.id
	contract.routeID = route.public.routeID
	contract.endpoints = append([]string(nil), route.public.endpoints...)
	decision.MessageCount = facts.Counts.Messages
	decision.ToolCount = facts.Counts.FunctionTools
	decision.InputBytes = len(input.OriginalBody)
	decision.Truncated = decision.Truncated || facts.truncated()
	return newChatOperationPlan(chatOperationPlanOptions{
		OperationID:          input.OperationID,
		EntryID:              profile.entry.id,
		PublicID:             profile.entry.id,
		RouteID:              route.public.routeID,
		Route:                route,
		Contract:             contract,
		PolicyID:             profile.config.ID,
		SelectedTier:         tier,
		EffectiveMode:        profile.effectiveMode(),
		ConfigGeneration:     profile.configGeneration,
		ProfileGeneration:    profile.profileGeneration,
		ClassifierGeneration: profile.classifierGeneration,
		BinaryGeneration:     profile.binaryGeneration,
		Decision:             decision,
	})
}

type policyClassifierDispatchEvidence struct {
	dispatched atomic.Bool
}

type policyClassifierDispatchContextKey struct{}

func withPolicyClassifierDispatchEvidence(ctx context.Context) (context.Context, *policyClassifierDispatchEvidence) {
	if ctx == nil {
		ctx = context.Background()
	}
	evidence := &policyClassifierDispatchEvidence{}
	return context.WithValue(ctx, policyClassifierDispatchContextKey{}, evidence), evidence
}

func markPolicyClassifierDispatched(ctx context.Context) {
	if ctx == nil {
		return
	}
	if evidence, _ := ctx.Value(policyClassifierDispatchContextKey{}).(*policyClassifierDispatchEvidence); evidence != nil {
		evidence.dispatched.Store(true)
	}
}

func (c *chatPolicyRoutingController) refreshBreakerStats(routeID string) {
	if c == nil || c.stats == nil {
		return
	}
	profiles := c.breakerProfiles[routeID]
	if len(profiles) == 0 {
		return
	}
	state := profiles[0].breaker.state()
	for _, profile := range profiles {
		c.stats.setBreakerState(profile.statsID(), state)
	}
}

type policyStatsBucketContextKey struct{}

func withPolicyStatsBucket(ctx context.Context, bucket string) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, policyStatsBucketContextKey{}, bucket)
}

func policyStatsBucketFromContext(ctx context.Context) string {
	if ctx == nil {
		return policyStatsUnspecifiedBucket
	}
	bucket, _ := ctx.Value(policyStatsBucketContextKey{}).(string)
	if strings.TrimSpace(bucket) == "" {
		return policyStatsUnspecifiedBucket
	}
	return bucket
}

func newRoutePolicyClassifier(h *ProxyHandler, route *modelRoute, profile PolicyProfileConfig, stats *policyStatsCollector) (policyClassifier, error) {
	if h == nil || route == nil {
		return nil, fmt.Errorf("classifier handler and route are required")
	}
	target, ok := route.primaryTarget()
	if !ok || target.provider == nil {
		return nil, fmt.Errorf("classifier route %q has no target", route.public.routeID)
	}
	endpoint := providerEndpointChatCompletions
	if !route.supportsEndpoint(endpoint) && route.supportsEndpoint(providerEndpointResponses) {
		endpoint = providerEndpointResponses
	}
	options := policyHTTPClassifierOptions{
		Model:               target.upstreamModel,
		MaxCompletionTokens: profile.Classifier.MaxCompletionTokens,
		MaxFactsBytes:       profile.Classifier.MaxRequestBytes,
		MaxResponseBytes:    policyClassifierResponseLimit,
	}
	return newPolicyHTTPClassifier(options, func(ctx context.Context, body []byte, headers http.Header) (policyClassifierHTTPResponse, error) {
		prepared, err := preparePolicyClassifierBody(body, target)
		if err != nil {
			return policyClassifierHTTPResponse{}, err
		}
		owner := providerModelFromRouteTarget(route, target)
		prepared = applyProviderModelRequestPolicy(prepared, providerEndpointChatCompletions, owner)
		if err := validatePolicyClassifierNoStore(prepared, target.provider); err != nil {
			return policyClassifierHTTPResponse{}, err
		}

		var response policyClassifierHTTPResponse
		if endpoint == providerEndpointResponses {
			response, err = h.sendPolicyClassifierOverResponses(ctx, route, target, owner, prepared, headers)
		} else {
			response, err = h.sendPolicyClassifierNativeChat(ctx, target, owner, prepared, headers)
		}
		if err != nil {
			return policyClassifierHTTPResponse{}, err
		}
		if stats != nil {
			usage := response.Usage
			if usage.isZero() {
				usage = readPolicyClassifierUsage(response.Body)
			}
			if !usage.isZero() {
				stats.record(policyStatsObservation{Profile: profile.PublicID, TrafficBucket: policyStatsBucketFromContext(ctx), ClassifierUsage: usage})
			}
		}
		return response, nil
	})
}

func (h *ProxyHandler) sendPolicyClassifierNativeChat(ctx context.Context, target targetBinding, owner providerModel, body []byte, headers http.Header) (policyClassifierHTTPResponse, error) {
	req, err := h.newProviderJSONRequest(ctx, target.provider, http.MethodPost, providerEndpointChatCompletions, body, headers, "", owner)
	if err != nil {
		return policyClassifierHTTPResponse{}, newPolicyClassifierSendError(err, true)
	}
	if err := ctx.Err(); err != nil {
		return policyClassifierHTTPResponse{}, err
	}
	observation := newRouteSendObservation(time.Now(), nil)
	markPolicyClassifierDispatched(ctx)
	resp, err := h.singleInferenceSend(req, observation)
	if err != nil {
		// Only failures proven to occur before any request bytes were written
		// may affect shared health. Delivery-ambiguous resets stay local.
		preSend := !observation.wroteHeaders.Load() && !observation.wroteRequest.Load()
		return policyClassifierHTTPResponse{}, newPolicyClassifierSendError(err, preSend)
	}
	return readPolicyClassifierHTTPResponse(resp)
}

func (h *ProxyHandler) sendPolicyClassifierOverResponses(ctx context.Context, route *modelRoute, target targetBinding, owner providerModel, chatBody []byte, headers http.Header) (policyClassifierHTTPResponse, error) {
	upstreamModel := strings.TrimSpace(target.upstreamModel)
	classifierReplay := newResponsesChatReplayStore()
	defer func() { _ = classifierReplay.Close() }()
	replayRoute := responsesChatReplayRoute{
		ProviderID:    target.provider.id,
		PublicModel:   upstreamModel,
		UpstreamModel: upstreamModel,
	}
	plan, err := translateChatRequestToResponses(chatBody, responsesChatRequestOptions{
		UpstreamModel:       upstreamModel,
		ReplayStore:         classifierReplay,
		ReplayRoute:         replayRoute,
		MinimumOutputTokens: responsesChatMinimumOutputTokens,
		DropSamplingParams:  route.public.policy.dropSamplingParams,
	})
	if err != nil {
		return policyClassifierHTTPResponse{}, err
	}
	if plan.Stream {
		return policyClassifierHTTPResponse{}, fmt.Errorf("policy classifier Responses request unexpectedly enabled streaming")
	}
	requestBody, _ := stripUnsupportedResponsesRequestFields(plan.Body, target.provider)
	requestBody, err = preparePolicyClassifierResponsesBody(requestBody, target)
	if err != nil {
		return policyClassifierHTTPResponse{}, err
	}
	if err := validatePolicyClassifierNoStore(requestBody, target.provider); err != nil {
		return policyClassifierHTTPResponse{}, err
	}
	req, err := h.newProviderJSONRequest(ctx, target.provider, http.MethodPost, providerEndpointResponses, requestBody, headers, "", owner)
	if err != nil {
		return policyClassifierHTTPResponse{}, newPolicyClassifierSendError(err, true)
	}
	if err := ctx.Err(); err != nil {
		return policyClassifierHTTPResponse{}, err
	}
	observation := newRouteSendObservation(time.Now(), nil)
	markPolicyClassifierDispatched(ctx)
	resp, err := h.singleInferenceSend(req, observation)
	if err != nil {
		preSend := !observation.wroteHeaders.Load() && !observation.wroteRequest.Load()
		return policyClassifierHTTPResponse{}, newPolicyClassifierSendError(err, preSend)
	}
	defer func() { _ = resp.Body.Close() }()
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, responsesChatMaxJSONBodyBytes+1))
	if readErr != nil && resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return policyClassifierHTTPResponse{}, newPolicyClassifierSendError(readErr, false)
	}
	if len(responseBody) > responsesChatMaxJSONBodyBytes {
		return policyClassifierHTTPResponse{}, newPolicyClassifierSendError(fmt.Errorf("policy classifier Responses body exceeds %d bytes", responsesChatMaxJSONBodyBytes), false)
	}
	classifierUsage := readPolicyClassifierUsageForEndpoint(responseBody, providerEndpointResponses)
	if resp.StatusCode != http.StatusOK {
		return policyClassifierHTTPResponse{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: responseBody, Usage: classifierUsage}, nil
	}
	converted, err := translateResponsesJSONToChat(responseBody, responsesChatResponseOptions{
		PublicModel:        upstreamModel,
		ReplayStore:        classifierReplay,
		ReplayRoute:        replayRoute,
		ReplayToolDefaults: plan.ReplayToolDefaults,
	})
	if err != nil {
		// The provider accepted and completed the HTTP request, but its terminal
		// payload could not be represented as canonical Chat. Return an accepted
		// malformed classifier envelope so policyHTTPClassifier classifies this as
		// uncertain invalid output rather than as an infrastructure outage.
		return policyClassifierHTTPResponse{
			StatusCode: http.StatusOK,
			Header:     convertedChatSafeHeaders(resp.Header),
			Body:       []byte(`{"choices":[]}`),
			Usage:      classifierUsage,
		}, nil
	}
	return policyClassifierHTTPResponse{StatusCode: http.StatusOK, Header: convertedChatSafeHeaders(resp.Header), Body: converted.Body, Usage: classifierUsage}, nil
}

func readPolicyClassifierHTTPResponse(resp *http.Response) (policyClassifierHTTPResponse, error) {
	if resp == nil {
		return policyClassifierHTTPResponse{}, newPolicyClassifierSendError(fmt.Errorf("policy classifier returned no response"), false)
	}
	defer func() { _ = resp.Body.Close() }()
	responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, policyClassifierResponseLimit+1))
	if readErr != nil && resp.StatusCode >= 200 && resp.StatusCode <= 299 {
		return policyClassifierHTTPResponse{}, newPolicyClassifierSendError(readErr, false)
	}
	return policyClassifierHTTPResponse{StatusCode: resp.StatusCode, Header: resp.Header.Clone(), Body: responseBody}, nil
}

func preparePolicyClassifierBody(body []byte, target targetBinding) ([]byte, error) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if target.provider == nil || target.provider.classifierNoStoreSupported == nil || !*target.provider.classifierNoStoreSupported {
		delete(payload, "store")
	}
	if !target.wirePolicy.useMaxCompletionTokens {
		if value, ok := payload["max_completion_tokens"]; ok {
			payload["max_tokens"] = value
			delete(payload, "max_completion_tokens")
		}
	}
	return json.Marshal(payload)
}

func preparePolicyClassifierResponsesBody(body []byte, target targetBinding) ([]byte, error) {
	if target.provider != nil && target.provider.classifierNoStoreSupported != nil && *target.provider.classifierNoStoreSupported {
		return body, nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	if _, exists := payload["store"]; !exists {
		return body, nil
	}
	delete(payload, "store")
	return json.Marshal(payload)
}

func validatePolicyClassifierNoStore(body []byte, provider *providerRuntime) error {
	if provider == nil || provider.classifierNoStoreSupported == nil || !*provider.classifierNoStoreSupported {
		return nil
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil {
		return fmt.Errorf("decode final classifier request: %w", err)
	}
	raw, ok := payload["store"]
	if !ok {
		return fmt.Errorf("classifier request lost required store: false control")
	}
	if !bytes.Equal(bytes.TrimSpace(raw), []byte("false")) {
		return fmt.Errorf("classifier request must retain store: false")
	}
	return nil
}

// PolicyStatsSnapshot returns a content-free point-in-time snapshot used by the
// main stats payload and focused tests.
func (c *chatPolicyRoutingController) PolicyStatsSnapshot() policyStatsSnapshot {
	if c == nil || c.stats == nil {
		return emptyPolicyStatsSnapshot()
	}
	for routeID := range c.breakerProfiles {
		c.refreshBreakerStats(routeID)
	}
	return c.stats.snapshot()
}

func readPolicyClassifierUsageForEndpoint(body []byte, endpoint string) policyStatsTokenUsage {
	if endpoint != providerEndpointResponses {
		return policyStatsTokenUsage{}
	}
	usage := sniffResponsesUsageBody(body)
	if usage.isZero() {
		return policyStatsTokenUsage{}
	}
	return policyStatsTokenUsage{
		InputTokens:       int64(usage.InputTokens),
		OutputTokens:      int64(usage.OutputTokens),
		TotalTokens:       int64(usage.totalTokens()),
		CachedInputTokens: int64(usage.InputTokensDetails.CachedTokens),
		ReasoningTokens:   int64(usage.OutputTokensDetails.ReasoningTokens),
	}.normalized()
}

func readPolicyClassifierUsage(body []byte) policyStatsTokenUsage {
	var envelope struct {
		Usage struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			TotalTokens         int64 `json:"total_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
			CompletionTokensDetails *struct {
				ReasoningTokens int64 `json:"reasoning_tokens"`
			} `json:"completion_tokens_details"`
		} `json:"usage"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	if decoder.Decode(&envelope) != nil {
		return policyStatsTokenUsage{}
	}
	usage := policyStatsTokenUsage{InputTokens: envelope.Usage.PromptTokens, OutputTokens: envelope.Usage.CompletionTokens, TotalTokens: envelope.Usage.TotalTokens}
	if envelope.Usage.PromptTokensDetails != nil {
		usage.CachedInputTokens = envelope.Usage.PromptTokensDetails.CachedTokens
	}
	if envelope.Usage.CompletionTokensDetails != nil {
		usage.ReasoningTokens = envelope.Usage.CompletionTokensDetails.ReasoningTokens
	}
	return usage.normalized()
}
