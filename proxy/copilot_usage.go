package proxy

import (
	"context"
	"encoding/json"
)

// Copilot accounting is reported separately from token usage. Retain only
// numeric totals, never token-detail model names or provider metadata.
type copilotUsageTotals struct {
	TotalNanoAIU int64 `json:"total_nano_aiu"`
	ComputeUnits int64 `json:"compute_units"`
}

func parseCopilotUsage(raw json.RawMessage) (copilotUsageTotals, bool) {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return copilotUsageTotals{}, false
	}
	var usage struct {
		TotalNanoAIU *int64 `json:"total_nano_aiu"`
		ComputeUnits *int64 `json:"compute_units"`
	}
	if json.Unmarshal(raw, &usage) != nil {
		return copilotUsageTotals{}, false
	}
	var result copilotUsageTotals
	valid := false
	if usage.TotalNanoAIU != nil && *usage.TotalNanoAIU >= 0 {
		result.TotalNanoAIU = *usage.TotalNanoAIU
		valid = true
	}
	if usage.ComputeUnits != nil && *usage.ComputeUnits >= 0 {
		result.ComputeUnits = *usage.ComputeUnits
		valid = true
	}
	return result, valid
}

func (u *copilotUsageTotals) merge(other copilotUsageTotals) {
	u.TotalNanoAIU = max(u.TotalNanoAIU, other.TotalNanoAIU)
	u.ComputeUnits = max(u.ComputeUnits, other.ComputeUnits)
}

func (u *copilotUsageTotals) add(other copilotUsageTotals) {
	u.TotalNanoAIU = policyStatsSaturatingAdd(u.TotalNanoAIU, other.TotalNanoAIU)
	u.ComputeUnits = policyStatsSaturatingAdd(u.ComputeUnits, other.ComputeUnits)
}

func observeCopilotUsage(ctx context.Context, raw json.RawMessage) {
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		if usage, ok := parseCopilotUsage(raw); ok {
			summary.mu.Lock()
			summary.copilotUsage.merge(usage)
			summary.mu.Unlock()
		}
	}
}

func observeChatCopilotUsage(ctx context.Context, body []byte) {
	if RequestSummaryFromContext(ctx) == nil {
		return
	}
	var envelope struct {
		Usage json.RawMessage `json:"copilot_usage"`
	}
	if json.Unmarshal(body, &envelope) == nil {
		observeCopilotUsage(ctx, envelope.Usage)
	}
}
