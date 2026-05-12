package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os/exec"
	"strings"
)

const toolOptimizerSchemaV1 = "vekil.tool_optimizer.v1"

type execJSONToolOptimizer struct {
	id             string
	path           string
	args           []string
	maxStdoutBytes int64
	maxStderrBytes int64
}

type execJSONToolOptimizerRequest struct {
	Schema     string            `json:"schema"`
	Operation  string            `json:"operation"`
	ToolName   string            `json:"tool_name,omitempty"`
	CallID     string            `json:"call_id,omitempty"`
	Command    string            `json:"command,omitempty"`
	FilterHint string            `json:"filter_hint,omitempty"`
	Output     string            `json:"output,omitempty"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type execJSONToolOptimizerResponse struct {
	Changed bool   `json:"changed"`
	Command string `json:"command,omitempty"`
	Output  string `json:"output,omitempty"`
	Reason  string `json:"reason,omitempty"`
}

func newExecJSONToolOptimizer(cfg ToolOptimizerProviderConfig) ToolOptimizer {
	path := strings.TrimSpace(cfg.Path)
	if path == "" {
		return nil
	}
	maxStdout := int64(cfg.MaxStdoutBytes)
	if maxStdout <= 0 {
		maxStdout = 1 << 20
	}
	maxStderr := int64(cfg.MaxStderrBytes)
	if maxStderr <= 0 {
		maxStderr = 64 << 10
	}
	return &execJSONToolOptimizer{
		id:             firstNonEmpty(cfg.ID, "exec_json"),
		path:           path,
		args:           append([]string(nil), cfg.Args...),
		maxStdoutBytes: maxStdout,
		maxStderrBytes: maxStderr,
	}
}

func (e *execJSONToolOptimizer) ID() string {
	if e == nil {
		return "exec_json"
	}
	return firstNonEmpty(e.id, "exec_json")
}

func (e *execJSONToolOptimizer) RewriteCommand(ctx context.Context, req ToolCommandRewriteRequest) (ToolCommandRewriteResult, error) {
	if e == nil {
		return ToolCommandRewriteResult{}, nil
	}
	resp, err := e.call(ctx, execJSONToolOptimizerRequest{
		Schema:    toolOptimizerSchemaV1,
		Operation: "rewrite_command",
		ToolName:  req.ToolName,
		CallID:    req.CallID,
		Command:   req.Command,
		Metadata:  req.Metadata,
	})
	if err != nil || !resp.Changed {
		return ToolCommandRewriteResult{}, err
	}
	return ToolCommandRewriteResult{Changed: true, Command: resp.Command, Provider: e.ID(), Reason: resp.Reason}, nil
}

func (e *execJSONToolOptimizer) ReduceOutput(ctx context.Context, req ToolOutputReduceRequest) (ToolOutputReduceResult, error) {
	if e == nil {
		return ToolOutputReduceResult{}, nil
	}
	resp, err := e.call(ctx, execJSONToolOptimizerRequest{
		Schema:     toolOptimizerSchemaV1,
		Operation:  "reduce_output",
		ToolName:   req.ToolName,
		CallID:     req.CallID,
		Command:    req.Command,
		FilterHint: req.FilterHint,
		Output:     req.Output,
		Metadata:   req.Metadata,
	})
	if err != nil || !resp.Changed {
		return ToolOutputReduceResult{}, err
	}
	return ToolOutputReduceResult{Changed: true, Output: resp.Output, Provider: e.ID(), Reason: resp.Reason}, nil
}

func (e *execJSONToolOptimizer) call(ctx context.Context, req execJSONToolOptimizerRequest) (execJSONToolOptimizerResponse, error) {
	input, err := json.Marshal(req)
	if err != nil {
		return execJSONToolOptimizerResponse{}, err
	}
	cmd := exec.CommandContext(ctx, e.path, e.args...)
	cmd.Stdin = bytes.NewReader(input)
	var stdout, stderr limitedBuffer
	stdout.limit = e.maxStdoutBytes
	stderr.limit = e.maxStderrBytes
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return execJSONToolOptimizerResponse{}, err
	}
	var resp execJSONToolOptimizerResponse
	if err := json.Unmarshal(stdout.Bytes(), &resp); err != nil {
		return execJSONToolOptimizerResponse{}, err
	}
	return resp, nil
}

type limitedBuffer struct {
	buf   bytes.Buffer
	limit int64
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if b.limit <= 0 {
		return b.buf.Write(p)
	}
	if int64(b.buf.Len()+len(p)) > b.limit {
		remaining := int(b.limit) - b.buf.Len()
		if remaining > 0 {
			_, _ = b.buf.Write(p[:remaining])
		}
		return 0, io.ErrShortBuffer
	}
	return b.buf.Write(p)
}

func (b *limitedBuffer) Bytes() []byte {
	if b == nil {
		return nil
	}
	return b.buf.Bytes()
}
