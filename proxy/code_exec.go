package proxy

import (
	"context"
	"time"
)

// CodeExecutionBackend executes model-generated commands in a compute
// environment that is separate from the Vekil proxy process. It is the
// extension point that lets proxy-mediated code execution route to different
// executors (local process, container, remote sandbox, or a future HyperAgent
// adapter) without the proxy loop hard-coding any single implementation.
//
// Backends are responsible for enforcing the policy carried in each request.
// Implementations must not grant model-generated code implicit access to the
// proxy's own credentials or the full host filesystem.
type CodeExecutionBackend interface {
	// Name returns a stable identifier for the backend (e.g. "local"). It is
	// surfaced in structured results and audit logs.
	Name() string
	// RunCommand executes the request and returns a structured result. Backends
	// enforce the policy carried in the request (timeout, max output bytes,
	// working directory, environment allowlist). A returned error signals that
	// the backend could not run the command at all; a command that ran but
	// exited non-zero is reported through CodeExecResult.ExitCode, not an error.
	RunCommand(ctx context.Context, req CodeExecRequest) (CodeExecResult, error)
}

// CodeExecPolicy carries the enforced execution policy controls applied to a
// single command. The zero value applies no limits; callers derive a populated
// policy from configuration via CodeExecPolicyConfig.toPolicy.
type CodeExecPolicy struct {
	// Timeout bounds wall-clock execution time. Non-positive means no timeout.
	Timeout time.Duration
	// MaxOutputBytes caps captured stdout and stderr independently. Non-positive
	// means unlimited capture.
	MaxOutputBytes int
	// WorkingDir restricts the command's working directory. Empty means the
	// backend decides (the local backend requires an explicit directory).
	WorkingDir string
	// EnvAllowlist is the set of environment variable names passed through to the
	// executed command. A nil or empty allowlist means no environment variables
	// are exposed to model-generated code.
	EnvAllowlist []string
}

// CodeExecRequest describes a single command execution handed to a
// CodeExecutionBackend.
type CodeExecRequest struct {
	// ToolUseID is the upstream tool-call identifier this execution answers.
	ToolUseID string
	// ToolName is the owned tool that produced the command (e.g. "Bash").
	ToolName string
	// Command is the shell command line to execute.
	Command string
	// Policy is the enforced execution policy for this request.
	Policy CodeExecPolicy
}

// CodeExecArtifact describes a file or output artifact produced by an
// execution. The local process backend does not emit artifacts; the field
// exists so richer backends can report them without a shape change.
type CodeExecArtifact struct {
	Name string `json:"name"`
	Path string `json:"path,omitempty"`
	Size int64  `json:"size,omitempty"`
}

// CodeExecResult is the structured result of executing a command through a
// CodeExecutionBackend. It is serialized into the provider-specific tool result
// that is fed back to the model, so the required fields (command, exit code,
// stdout, stderr, duration, timeout status) are always present.
type CodeExecResult struct {
	ToolUseID   string             `json:"tool_use_id,omitempty"`
	Backend     string             `json:"backend,omitempty"`
	WorkspaceID string             `json:"workspace_id,omitempty"`
	Command     string             `json:"command"`
	ExitCode    int                `json:"exit_code"`
	Stdout      string             `json:"stdout"`
	Stderr      string             `json:"stderr"`
	DurationMS  int64              `json:"duration_ms"`
	TimedOut    bool               `json:"timed_out"`
	Truncated   bool               `json:"truncated,omitempty"`
	Artifacts   []CodeExecArtifact `json:"artifacts,omitempty"`
}
