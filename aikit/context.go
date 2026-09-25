package aikit

import (
	"errors"
	"fmt"
)

const (
	// DefaultContextTokens is the context vekil requests when none is given.
	DefaultContextTokens = 65536
	// AgentMinimumContextTokens is the smallest context a coding agent can use.
	// Claude Code 2.1's first request measured 39k tokens with Qwen's tokenizer
	// (system prompt, 59 tool schemas, and a project CLAUDE.md), so 48k leaves
	// room for a few turns before compaction.
	AgentMinimumContextTokens = 49152
	// serveMinimumContextTokens bounds automatic halving when no floor is set.
	serveMinimumContextTokens = 8192
)

// contextDecision is the context a container is started with.
type contextDecision struct {
	Tokens int
	// Env is added to the container environment.
	Env []string
	// Retryable reports whether an out-of-memory load may be retried with a
	// halved context. Explicit and image-fixed values are never shrunk.
	Retryable bool
	// Warning is printed before the container starts.
	Warning string
}

// errVLLMCPPPoolSizing explains why vllm.cpp cannot be sized from the launcher.
var errVLLMCPPPoolSizing = errors.New(
	"vllm.cpp sizes its KV cache pool with the num_blocks model option (default 256 blocks of 32 tokens = 8192 tokens), " +
		"which LocalAI reads only from the model config; LOCALAI_CONTEXT_SIZE raises the per-request limit but not the pool. " +
		"Use an image whose config sets context_size and a num_blocks option large enough for it, or use the llama-cpp backend")

// decideContext applies vekil's context rules for one backend.
func decideContext(plan modelPlan, requested, minimum int) (contextDecision, error) {
	switch plan.Backend {
	case BackendLlamaCPP:
		return decideLlamaCPPContext(plan, requested, minimum)
	case BackendVLLMCPP:
		return decideVLLMCPPContext(plan, requested, minimum)
	default:
		return contextDecision{}, fmt.Errorf("backend %q is not supported; use llama-cpp or vllm-cpp", plan.Backend)
	}
}

func decideLlamaCPPContext(plan modelPlan, requested, minimum int) (contextDecision, error) {
	if plan.PinnedContext > 0 {
		return pinnedContext(plan, requested, minimum)
	}
	if requested > 0 {
		if plan.TrainedContext > 0 && requested > plan.TrainedContext {
			return contextDecision{}, fmt.Errorf("--context-size %d exceeds %s's trained context of %d tokens", requested, plan.ModelName, plan.TrainedContext)
		}
		if minimum > 0 && requested < minimum {
			return contextDecision{}, fmt.Errorf("--context-size %d is below the %d tokens a coding agent needs", requested, minimum)
		}
		return contextDecision{Tokens: requested, Env: llamaCPPContextEnv(requested)}, nil
	}
	tokens := DefaultContextTokens
	if plan.TrainedContext > 0 && plan.TrainedContext < tokens {
		tokens = plan.TrainedContext
	}
	if minimum > 0 && tokens < minimum {
		return contextDecision{}, fmt.Errorf("%s is trained for %d tokens of context, below the %d tokens a coding agent needs", plan.ModelName, plan.TrainedContext, minimum)
	}
	return contextDecision{Tokens: tokens, Env: llamaCPPContextEnv(tokens), Retryable: true}, nil
}

func llamaCPPContextEnv(tokens int) []string {
	return []string{fmt.Sprintf("LOCALAI_CONTEXT_SIZE=%d", tokens)}
}

func pinnedContext(plan modelPlan, requested, minimum int) (contextDecision, error) {
	if requested > 0 && requested != plan.PinnedContext {
		return contextDecision{}, fmt.Errorf("the image config fixes %s at context_size %d, so --context-size %d cannot apply; rebuild the image without context_size", plan.ModelName, plan.PinnedContext, requested)
	}
	if minimum > 0 && plan.PinnedContext < minimum {
		return contextDecision{}, fmt.Errorf("the image config fixes %s at context_size %d, below the %d tokens a coding agent needs; rebuild the image without context_size", plan.ModelName, plan.PinnedContext, minimum)
	}
	decision := contextDecision{Tokens: plan.PinnedContext}
	if requested == 0 {
		decision.Warning = fmt.Sprintf("the image config fixes context_size %d; using it", plan.PinnedContext)
	}
	return decision, nil
}

func decideVLLMCPPContext(plan modelPlan, requested, minimum int) (contextDecision, error) {
	if plan.PinnedContext == 0 || plan.PoolTokens == 0 {
		return contextDecision{}, errVLLMCPPPoolSizing
	}
	if plan.PoolTokens < plan.PinnedContext {
		return contextDecision{}, fmt.Errorf("the image config sizes the vllm.cpp KV pool at %d tokens, below its context_size %d; raise num_blocks", plan.PoolTokens, plan.PinnedContext)
	}
	return pinnedContext(plan, requested, minimum)
}

// halvedContext returns the next context to try after an out-of-memory load.
func halvedContext(current, minimum int) (int, bool) {
	floor := minimum
	if floor <= 0 {
		floor = serveMinimumContextTokens
	}
	next := current / 2
	if next < floor {
		next = floor
	}
	return next, next < current
}
