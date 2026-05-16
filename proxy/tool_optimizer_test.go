package proxy

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

type contextCheckingToolOptimizer struct {
	reduceCtxErr      error
	reduceHasDeadline bool
}

func (o *contextCheckingToolOptimizer) ID() string { return "context-checking" }

func (o *contextCheckingToolOptimizer) RewriteCommand(context.Context, ToolCommandRewriteRequest) (ToolCommandRewriteResult, error) {
	return ToolCommandRewriteResult{}, nil
}

func (o *contextCheckingToolOptimizer) ReduceOutput(ctx context.Context, _ ToolOutputReduceRequest) (ToolOutputReduceResult, error) {
	o.reduceCtxErr = ctx.Err()
	_, o.reduceHasDeadline = ctx.Deadline()
	return ToolOutputReduceResult{Changed: true, Output: "reduced"}, nil
}

func TestToolOptimizerOutputReduceExplicitZeroTimeoutDisablesDeadline(t *testing.T) {
	optimizer := &contextCheckingToolOptimizer{}
	var cfg ToolOptimizersConfig
	if err := json.Unmarshal([]byte(`{
		"enabled": true,
		"output_reduce": {
			"enabled": true,
			"timeout_ms": 0,
			"min_input_bytes": 0,
			"max_input_bytes": 0
		}
	}`), &cfg); err != nil {
		t.Fatalf("unmarshal config: %v", err)
	}
	manager := NewToolOptimizerManager(cfg, []stagedToolOptimizer{{optimizer: optimizer, outputReduce: true}})

	result := manager.ReduceOutput(context.Background(), ToolOutputReduceRequest{Command: "cat big.log", Output: "large output"})
	if !result.Changed || result.Output != "reduced" {
		t.Fatalf("ReduceOutput result = %+v, want changed reduced output", result)
	}
	if optimizer.reduceCtxErr != nil {
		t.Fatalf("optimizer context error = %v, want nil", optimizer.reduceCtxErr)
	}
	if optimizer.reduceHasDeadline {
		t.Fatalf("optimizer context unexpectedly had a deadline")
	}
}

func TestLimitedBufferDiscardsAfterLimitWithoutShortWrite(t *testing.T) {
	var buf limitedBuffer
	buf.limit = 3

	n, err := buf.Write([]byte("abcde"))
	if err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if n != 5 {
		t.Fatalf("Write n = %d, want 5", n)
	}
	if got := string(buf.Bytes()); got != "abc" {
		t.Fatalf("buffer = %q, want abc", got)
	}
	if !buf.Truncated() {
		t.Fatalf("expected buffer to record truncation")
	}

	n, err = buf.Write([]byte("fgh"))
	if err != nil {
		t.Fatalf("second Write returned error: %v", err)
	}
	if n != 3 {
		t.Fatalf("second Write n = %d, want 3", n)
	}
	if got := string(buf.Bytes()); got != "abc" {
		t.Fatalf("buffer after discard = %q, want abc", got)
	}
}

func TestValidCommandReplacementAllowsInternalNewlinesAndCarriageReturns(t *testing.T) {
	if !validCommandReplacement("grep foo big.log", "printf 'foo\\nbar'\nrg foo big.log") {
		t.Fatalf("expected replacement with internal newline to be valid")
	}
	if !validCommandReplacement("grep foo big.log", "printf 'foo\\rbar'\rrg foo big.log") {
		t.Fatalf("expected replacement with internal carriage return to be valid")
	}
}

func TestValidCommandReplacementRejectsUnsafeOrNoopValues(t *testing.T) {
	tests := []struct {
		name        string
		original    string
		replacement string
	}{
		{name: "nul", original: "grep foo big.log", replacement: "rg foo\x00 big.log"},
		{name: "trim empty", original: "grep foo big.log", replacement: " \t\n"},
		{name: "unchanged", original: "grep foo big.log", replacement: "  grep foo big.log\n"},
		{name: "too large", original: "echo original", replacement: strings.Repeat("x", maxOptimizedCommandBytes+1)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if validCommandReplacement(tt.original, tt.replacement) {
				t.Fatalf("expected replacement %q to be invalid", tt.replacement)
			}
		})
	}
}

func TestValidCommandReplacementAllowsMaxSizedReplacement(t *testing.T) {
	replacement := strings.Repeat("x", maxOptimizedCommandBytes)
	if !validCommandReplacement("echo original", replacement) {
		t.Fatalf("expected %d-byte replacement to be valid", maxOptimizedCommandBytes)
	}
}
