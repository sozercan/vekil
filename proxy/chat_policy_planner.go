package proxy

import (
	"context"
	"strings"

	"github.com/google/uuid"
)

// chatPolicyPlanner is the only policy-routing seam exposed to the OpenAI Chat
// handler. A zero/invalid plan means the public entry is a direct static model
// and existing routing should continue unchanged.
type chatPolicyPlanner interface {
	Plan(context.Context, chatPolicyInput) (chatOperationPlan, error)
}

type chatPolicyInput struct {
	OperationID  string
	Model        string
	OriginalBody []byte
	// FactSourceBytesLimit is non-zero only for a trusted canonical translation
	// whose public ingress was already checked against the default fact limit.
	FactSourceBytesLimit int
	CarriedReasoning     map[string]carriedReplay
}

func (i chatPolicyInput) normalized() chatPolicyInput {
	i.OperationID = strings.TrimSpace(i.OperationID)
	if i.OperationID == "" {
		i.OperationID = uuid.NewString()
	}
	i.Model = strings.TrimSpace(i.Model)
	return i
}

func (h *ProxyHandler) planOpenAIChatPolicy(ctx context.Context, model string, originalBody []byte) (chatOperationPlan, error) {
	return h.planOpenAIChatPolicyWithFactSourceLimit(ctx, model, originalBody, 0)
}

func (h *ProxyHandler) planOpenAIChatPolicyWithFactSourceLimit(ctx context.Context, model string, originalBody []byte, factSourceBytesLimit int) (chatOperationPlan, error) {
	return h.planOpenAIChatPolicyWithCarrier(ctx, model, originalBody, factSourceBytesLimit, nil)
}

func (h *ProxyHandler) planOpenAIChatPolicyWithCarrier(ctx context.Context, model string, originalBody []byte, factSourceBytesLimit int, carried map[string]carriedReplay) (chatOperationPlan, error) {
	if h == nil || h.chatPolicyPlanner == nil {
		return chatOperationPlan{}, nil
	}
	input := chatPolicyInput{
		Model:                model,
		OriginalBody:         originalBody,
		FactSourceBytesLimit: factSourceBytesLimit,
		CarriedReasoning:     carried,
	}
	// Policy ingress may establish a client-visible request identity before
	// translation and planning. Reuse it so sampling, route attempts, response
	// headers, and request summaries all describe the same logical operation.
	if summary := RequestSummaryFromContext(ctx); summary != nil {
		input.OperationID = summary.OperationID()
	}
	input = input.normalized()
	return h.chatPolicyPlanner.Plan(ctx, input)
}
