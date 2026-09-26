package aikit

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestDecideLlamaCPPContext(t *testing.T) {
	tests := []struct {
		name      string
		plan      modelPlan
		requested int
		minimum   int
		tokens    int
		retryable bool
		env       bool
		warning   string
		wantErr   string
	}{
		{name: "default capped at 64k", plan: modelPlan{Backend: BackendLlamaCPP, TrainedContext: 262144}, minimum: AgentMinimumContextTokens, tokens: 65536, retryable: true, env: true},
		{name: "default capped at trained", plan: modelPlan{Backend: BackendLlamaCPP, TrainedContext: 57344}, minimum: AgentMinimumContextTokens, tokens: 57344, retryable: true, env: true},
		{name: "trained between floor and default rejected", plan: modelPlan{Backend: BackendLlamaCPP, ModelName: "m", TrainedContext: 40960}, minimum: AgentMinimumContextTokens, wantErr: "trained for 40960"},
		{name: "unknown trained uses default", plan: modelPlan{Backend: BackendLlamaCPP}, minimum: AgentMinimumContextTokens, tokens: 65536, retryable: true, env: true},
		{name: "trained below floor", plan: modelPlan{Backend: BackendLlamaCPP, ModelName: "phi-4", TrainedContext: 16384}, minimum: AgentMinimumContextTokens, wantErr: "trained for 16384"},
		{name: "trained below default without floor", plan: modelPlan{Backend: BackendLlamaCPP, TrainedContext: 16384}, tokens: 16384, retryable: true, env: true},
		{name: "explicit", plan: modelPlan{Backend: BackendLlamaCPP, TrainedContext: 262144}, requested: 49152, minimum: AgentMinimumContextTokens, tokens: 49152, env: true},
		{name: "explicit above trained", plan: modelPlan{Backend: BackendLlamaCPP, ModelName: "m", TrainedContext: 40960}, requested: 65536, wantErr: "exceeds m's trained context"},
		{name: "explicit below floor", plan: modelPlan{Backend: BackendLlamaCPP}, requested: 8192, minimum: AgentMinimumContextTokens, wantErr: "below the 49152"},
		{name: "pinned", plan: modelPlan{Backend: BackendLlamaCPP, PinnedContext: 65536}, minimum: AgentMinimumContextTokens, tokens: 65536, warning: "fixes context_size 65536"},
		{name: "pinned matches request", plan: modelPlan{Backend: BackendLlamaCPP, PinnedContext: 65536}, requested: 65536, tokens: 65536},
		{name: "pinned conflicts with request", plan: modelPlan{Backend: BackendLlamaCPP, PinnedContext: 65536}, requested: 32768, wantErr: "cannot apply"},
		{name: "pinned below floor", plan: modelPlan{Backend: BackendLlamaCPP, PinnedContext: 8192}, minimum: AgentMinimumContextTokens, wantErr: "rebuild the image without context_size"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			decision, err := decideContext(tt.plan, tt.requested, tt.minimum)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("error = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("decideContext: %v", err)
			}
			if decision.Tokens != tt.tokens || decision.Retryable != tt.retryable || (len(decision.Env) > 0) != tt.env {
				t.Fatalf("decision = %+v", decision)
			}
			if tt.env && decision.Env[0] != "LOCALAI_CONTEXT_SIZE="+strconv.Itoa(tt.tokens) {
				t.Fatalf("env = %v", decision.Env)
			}
			if !strings.Contains(decision.Warning, tt.warning) {
				t.Fatalf("warning = %q, want %q", decision.Warning, tt.warning)
			}
		})
	}
}

func TestDecideVLLMCPPContext(t *testing.T) {
	if _, err := decideContext(modelPlan{Backend: BackendVLLMCPP, TrainedContext: 40960}, 0, AgentMinimumContextTokens); !errors.Is(err, errVLLMCPPPoolSizing) {
		t.Fatalf("runner vllm-cpp error = %v", err)
	}
	if _, err := decideContext(modelPlan{Backend: BackendVLLMCPP, PinnedContext: 65536, PoolTokens: 8192}, 0, AgentMinimumContextTokens); err == nil || !strings.Contains(err.Error(), "raise num_blocks") {
		t.Fatalf("small pool error = %v", err)
	}
	decision, err := decideContext(modelPlan{Backend: BackendVLLMCPP, PinnedContext: 65536, PoolTokens: 65536}, 0, AgentMinimumContextTokens)
	if err != nil || decision.Tokens != 65536 || len(decision.Env) != 0 || decision.Retryable {
		t.Fatalf("sized vllm-cpp decision = %+v, %v", decision, err)
	}
	if _, err := decideContext(modelPlan{Backend: "diffusers"}, 0, 0); err == nil {
		t.Fatal("unsupported backend was accepted")
	}
}

func TestVLLMPoolTokens(t *testing.T) {
	if got := vllmPoolTokens(nil); got != 0 {
		t.Fatalf("default pool = %d", got)
	}
	if got := vllmPoolTokens([]string{"num_blocks:2049", "max_num_seqs:1"}); got != 2048*32 {
		t.Fatalf("pool = %d", got)
	}
	if got := vllmPoolTokens([]string{"block_size:16", "num_blocks:101"}); got != 1600 {
		t.Fatalf("pool = %d", got)
	}
}

func TestHalvedContext(t *testing.T) {
	steps := []int{65536}
	current := 65536
	for {
		next, ok := halvedContext(current, AgentMinimumContextTokens)
		if !ok {
			break
		}
		steps = append(steps, next)
		current = next
	}
	if len(steps) != 2 || steps[1] != AgentMinimumContextTokens {
		t.Fatalf("halving steps = %v", steps)
	}
	if next, ok := halvedContext(20000, 0); !ok || next != 10000 {
		t.Fatalf("no-floor halving = %d, %v", next, ok)
	}
	if _, ok := halvedContext(8192, 0); ok {
		t.Fatal("halving went below the serve floor")
	}
}
