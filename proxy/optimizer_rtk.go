package proxy

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
)

type rtkToolOptimizer struct {
	id             string
	path           string
	maxStdoutBytes int64
	maxStderrBytes int64
}

func newRTKToolOptimizer(cfg ToolOptimizerProviderConfig) ToolOptimizer {
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		path = "rtk"
	}
	maxStdout := int64(cfg.MaxStdoutBytes)
	if maxStdout <= 0 {
		maxStdout = 1 << 20
	}
	maxStderr := int64(cfg.MaxStderrBytes)
	if maxStderr <= 0 {
		maxStderr = 64 << 10
	}
	return &rtkToolOptimizer{
		id:             firstNonEmpty(cfg.ID, "rtk"),
		path:           path,
		maxStdoutBytes: maxStdout,
		maxStderrBytes: maxStderr,
	}
}

func (r *rtkToolOptimizer) ID() string {
	if r == nil {
		return "rtk"
	}
	return firstNonEmpty(r.id, "rtk")
}

func (r *rtkToolOptimizer) RewriteCommand(ctx context.Context, req ToolCommandRewriteRequest) (ToolCommandRewriteResult, error) {
	if r == nil || strings.TrimSpace(req.Command) == "" {
		return ToolCommandRewriteResult{}, nil
	}
	cmd := exec.CommandContext(ctx, r.path, "hook", "check", "--", req.Command)
	stdout, err := r.run(cmd, "")
	if err != nil {
		return ToolCommandRewriteResult{}, err
	}
	rewritten := strings.TrimSpace(string(stdout))
	if rewritten == "" {
		return ToolCommandRewriteResult{}, nil
	}
	return ToolCommandRewriteResult{Changed: true, Command: rewritten, Provider: r.ID(), Reason: "rtk hook check"}, nil
}

func (r *rtkToolOptimizer) ReduceOutput(ctx context.Context, req ToolOutputReduceRequest) (ToolOutputReduceResult, error) {
	if r == nil || req.Output == "" {
		return ToolOutputReduceResult{}, nil
	}
	args := []string{"pipe"}
	if isSupportedRTKFilter(req.FilterHint) {
		args = append(args, "--filter", req.FilterHint)
	}
	cmd := exec.CommandContext(ctx, r.path, args...)
	stdout, err := r.run(cmd, req.Output)
	if err != nil {
		return ToolOutputReduceResult{}, err
	}
	if len(bytes.TrimSpace(stdout)) == 0 {
		return ToolOutputReduceResult{}, nil
	}
	return ToolOutputReduceResult{Changed: true, Output: string(stdout), Provider: r.ID(), Reason: "rtk pipe"}, nil
}

func (r *rtkToolOptimizer) run(cmd *exec.Cmd, stdin string) ([]byte, error) {
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr limitedBuffer
	stdout.limit = r.maxStdoutBytes
	stderr.limit = r.maxStderrBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, err
	}
	return stdout.Bytes(), nil
}

func isSupportedRTKFilter(filter string) bool {
	switch strings.TrimSpace(filter) {
	case "cargo-test", "cargo", "pytest", "go-test", "go-build", "tsc", "vitest", "grep", "rg", "find", "fd", "git-log", "git-diff", "git-status", "mypy", "ruff-check", "ruff-format", "prettier":
		return true
	default:
		return false
	}
}
