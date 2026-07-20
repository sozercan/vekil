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
	binding := h.policyBindingForContext(ctx)
	if binding == nil || binding.planner == nil {
		return chatOperationPlan{}, nil
	}
	input := (chatPolicyInput{Model: model, OriginalBody: originalBody}).normalized()
	return binding.planner.Plan(ctx, input)
}
