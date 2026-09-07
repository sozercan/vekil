package proxy

import "time"

const policyDecisionEvidenceLimit = 256

// Evidence supports offline comparisons without retaining classifier text or
// physical model identities. Signals have a fixed enum and numeric schema.
type policyDecisionEvidence struct {
	T                   int64                               `json:"t"`
	OperationID         string                              `json:"operation_id,omitempty"`
	Profile             string                              `json:"profile"`
	Mode                string                              `json:"mode"`
	Category            string                              `json:"category"`
	ActualTier          string                              `json:"actual_tier"`
	ShadowTier          string                              `json:"shadow_tier,omitempty"`
	MappingReason       string                              `json:"mapping_reason"`
	FailureCategory     string                              `json:"failure_category,omitempty"`
	Signals             *policyClassifierSignals            `json:"signals,omitempty"`
	MessageCount        int                                 `json:"message_count"`
	ToolCount           int                                 `json:"tool_count"`
	InputBytes          int                                 `json:"input_bytes"`
	Truncated           bool                                `json:"truncated"`
	ClassifierLatencyMS int64                               `json:"classifier_latency_ms"`
	ReasoningEffort     string                              `json:"reasoning_effort,omitempty"`
	Generations         policyStatsGenerationHashesSnapshot `json:"generation_hashes"`
}

func policyResultEvidence(result policyClassifierResult, facts policyClassifierFacts) (string, policyClassifierSignals, bool) {
	switch result.Category {
	case policyClassifierResultUnavailable:
		return "unavailable_fallback", policyClassifierSignals{}, false
	case policyClassifierResultCanceled:
		return "canceled", policyClassifierSignals{}, false
	case policyClassifierResultClassified:
		if validatePolicyClassifierSignals(result.Signals) == nil && !result.Signals.Abstain {
			_, reason := mapPolicySignalsWithReason(result.Signals, facts)
			return reason, result.Signals, true
		}
	}
	return "uncertain_fallback", policyClassifierSignals{}, false
}

func (c *chatPolicyRoutingController) recordDecisionEvidence(profile *compiledPolicyProfile, input chatPolicyInput, decision policyDecisionRecord) {
	if c == nil || c.stats == nil || profile == nil {
		return
	}
	row := policyDecisionEvidence{
		T: time.Now().Unix(), OperationID: normalizePolicyStatsProfileLabel(input.OperationID, ""),
		Profile: normalizePolicyStatsProfileLabel(profile.entry.id, policyStatsUnknownProfile),
		Mode:    profile.effectiveMode().String(), Category: policyEvidenceCategory(decision.Category),
		ActualTier: decision.ActualTier.String(), MappingReason: policyEvidenceReason(decision.MappingReason),
		MessageCount: min(max(decision.MessageCount, 0), 100000), ToolCount: min(max(decision.ToolCount, 0), 100000),
		InputBytes: min(max(decision.InputBytes, 0), maxLargeRequestBodySize), Truncated: decision.Truncated,
		ClassifierLatencyMS: min(max(decision.ClassifierLatency, 0), policyStatsMaxClassifierLatencyMs),
		ReasoningEffort:     policyEvidenceEffort(profile.reasoningEffortForTier(decision.ActualTier)),
		Generations: policyStatsGenerationHashesSnapshot{
			Config:     normalizePolicyStatsGenerationHash(profile.configGeneration),
			Profile:    normalizePolicyStatsGenerationHash(profile.profileGeneration),
			Classifier: normalizePolicyStatsGenerationHash(profile.classifierGeneration),
			Binary:     normalizePolicyStatsGenerationHash(profile.binaryGeneration),
		},
	}
	if decision.ShadowTier != policyTierUnknown {
		row.ShadowTier = decision.ShadowTier.String()
	}
	if row.Category == "replay_pinned" {
		row.MappingReason = "replay_pinned"
	}
	if decision.FailureCategory != "" {
		row.FailureCategory = policyStatsDropReasonLabel(normalizePolicyStatsDropReason(decision.FailureCategory))
	}
	if decision.HasSignals && validatePolicyClassifierSignals(decision.Signals) == nil {
		signals := decision.Signals
		row.Signals = &signals
	}
	c.stats.recordDecisionEvidence(row)
}

func policyEvidenceCategory(value string) string {
	switch value {
	case "replay_binding":
		return "replay_pinned"
	case "baseline", "classified", "unavailable_fallback", "uncertain_fallback", "shadow", "replay_pinned":
		return value
	default:
		return "baseline"
	}
}

func policyEvidenceReason(value string) string {
	switch value {
	case "uncertain_signals", "complex_turn", "broad_scope", "high_risk", "multiple_modifications", "codebase_context", "truncated_context", "bounded_task", "unavailable_fallback", "uncertain_fallback", "canceled":
		return value
	default:
		return "configured_route"
	}
}

func policyEvidenceEffort(value string) string {
	switch value {
	case "none", "minimal", "low", "medium", "high", "xhigh", "max":
		return value
	default:
		return ""
	}
}

func (c *policyStatsCollector) recordDecisionEvidence(row policyDecisionEvidence) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.decisions == nil {
		c.decisions = make([]policyDecisionEvidence, policyDecisionEvidenceLimit)
	}
	c.decisions[c.decisionNext] = row
	c.decisionNext = (c.decisionNext + 1) % len(c.decisions)
	c.decisionSize = min(c.decisionSize+1, len(c.decisions))
}

func (c *policyStatsCollector) recentDecisionsLocked() []policyDecisionEvidence {
	rows := make([]policyDecisionEvidence, 0, c.decisionSize)
	for index := 0; index < c.decisionSize; index++ {
		row := c.decisions[(c.decisionNext-1-index+len(c.decisions))%len(c.decisions)]
		if row.Signals != nil {
			signals := *row.Signals
			row.Signals = &signals
		}
		rows = append(rows, row)
	}
	return rows
}
