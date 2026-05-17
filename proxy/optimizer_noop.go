package proxy

import "context"

type noopToolOptimizer struct {
	id string
}

func (n noopToolOptimizer) ID() string {
	if n.id == "" {
		return "noop"
	}
	return n.id
}

func (n noopToolOptimizer) RewriteCommand(context.Context, ToolCommandRewriteRequest) (ToolCommandRewriteResult, error) {
	return ToolCommandRewriteResult{}, nil
}

func (n noopToolOptimizer) ReduceOutput(context.Context, ToolOutputReduceRequest) (ToolOutputReduceResult, error) {
	return ToolOutputReduceResult{}, nil
}
