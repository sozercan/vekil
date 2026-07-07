package proxy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"
)

// LocalProcessBackend executes commands as local child processes. It is the
// development/default backend for proxy-mediated code execution. It enforces the
// per-request policy: a wall-clock timeout, independent stdout/stderr byte
// caps, a required working directory boundary, and an environment allowlist so
// model-generated code never inherits the proxy's own environment or
// credentials.
//
// This backend runs commands on the same host as the proxy. Operators who need
// stronger isolation should point the feature at a container or remote backend;
// the CodeExecutionBackend interface exists precisely so that substitution does
// not touch the proxy loop.
type LocalProcessBackend struct {
	// shell is the interpreter used to run the command line. Defaults to
	// "/bin/sh" with a "-c" argument when unset.
	shell string
}

// codeExecWaitDelay bounds how long RunCommand waits for output pipes to close
// after the process is killed by a timeout. It keeps a runaway grandchild
// process from blocking the timeout indefinitely.
const codeExecWaitDelay = 2 * time.Second

// NewLocalProcessBackend constructs a LocalProcessBackend using the default
// shell.
func NewLocalProcessBackend() *LocalProcessBackend {
	return &LocalProcessBackend{shell: "/bin/sh"}
}

// Name identifies this backend in results and logs.
func (b *LocalProcessBackend) Name() string { return "local" }

func (b *LocalProcessBackend) shellPath() string {
	if strings.TrimSpace(b.shell) != "" {
		return b.shell
	}
	return "/bin/sh"
}

// RunCommand executes req.Command under the configured shell, enforcing the
// request policy. A command that runs but exits non-zero is reported through
// CodeExecResult.ExitCode; a returned error means the command could not be run
// at all (e.g. the working directory boundary is missing or invalid).
func (b *LocalProcessBackend) RunCommand(ctx context.Context, req CodeExecRequest) (CodeExecResult, error) {
	command := strings.TrimSpace(req.Command)
	if command == "" {
		return CodeExecResult{}, errors.New("code exec: empty command")
	}

	workingDir, err := validateWorkingDir(req.Policy.WorkingDir)
	if err != nil {
		return CodeExecResult{}, err
	}

	execCtx := ctx
	var cancel context.CancelFunc
	if req.Policy.Timeout > 0 {
		execCtx, cancel = context.WithTimeout(ctx, req.Policy.Timeout)
		defer cancel()
	}

	cmd := exec.CommandContext(execCtx, b.shellPath(), "-c", command)
	cmd.Dir = workingDir
	cmd.Env = filteredEnv(req.Policy.EnvAllowlist)
	// WaitDelay bounds how long Run waits for output-copy goroutines after the
	// context is cancelled. Without it, a killed shell that forked a grandchild
	// (e.g. `sleep`) leaves the child holding the stdout/stderr pipe and Wait
	// blocks until that grandchild exits — defeating the timeout. WaitDelay
	// forces the pipes closed shortly after the kill so timeouts are enforced.
	cmd.WaitDelay = codeExecWaitDelay

	var stdout, stderr limitedBuffer
	stdout.limit = int64(req.Policy.MaxOutputBytes)
	stderr.limit = int64(req.Policy.MaxOutputBytes)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	runErr := cmd.Run()
	duration := time.Since(start)

	result := CodeExecResult{
		ToolUseID:  req.ToolUseID,
		Backend:    b.Name(),
		Command:    command,
		Stdout:     string(stdout.Bytes()),
		Stderr:     string(stderr.Bytes()),
		DurationMS: duration.Milliseconds(),
		Truncated:  stdout.Truncated() || stderr.Truncated(),
	}

	// A deadline hit is reported as a timeout, not a backend failure: the model
	// still gets a structured result describing what happened.
	if execCtx.Err() == context.DeadlineExceeded {
		result.TimedOut = true
		result.ExitCode = -1
		if strings.TrimSpace(result.Stderr) == "" {
			result.Stderr = fmt.Sprintf("command timed out after %s", req.Policy.Timeout)
		}
		return result, nil
	}

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			result.ExitCode = exitErr.ExitCode()
			return result, nil
		}
		// The process could not be started (e.g. shell missing). Surface it as a
		// backend error so the loop can fail closed rather than feed a misleading
		// exit code to the model.
		return result, fmt.Errorf("code exec: run command: %w", runErr)
	}

	result.ExitCode = 0
	return result, nil
}

// validateWorkingDir enforces the working-directory boundary: the local backend
// refuses to execute without an explicit, existing directory so model-generated
// code never runs in the proxy's own working directory by default.
func validateWorkingDir(dir string) (string, error) {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return "", errors.New("code exec: working directory is required but not configured")
	}
	info, err := os.Stat(dir)
	if err != nil {
		return "", fmt.Errorf("code exec: working directory %q is not accessible: %w", dir, err)
	}
	if !info.IsDir() {
		return "", fmt.Errorf("code exec: working directory %q is not a directory", dir)
	}
	return dir, nil
}

// filteredEnv builds the child environment from only the allowlisted variable
// names present in the proxy's environment. A nil/empty allowlist yields an
// empty environment so no proxy secrets leak into executed commands.
func filteredEnv(allowlist []string) []string {
	if len(allowlist) == 0 {
		return []string{}
	}
	allowed := make(map[string]struct{}, len(allowlist))
	for _, name := range allowlist {
		name = strings.TrimSpace(name)
		if name != "" {
			allowed[name] = struct{}{}
		}
	}
	env := make([]string, 0, len(allowed))
	for _, name := range allowlistOrder(allowed) {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return env
}

// allowlistOrder returns the allowlist names in a deterministic order so the
// constructed environment is stable across runs.
func allowlistOrder(allowed map[string]struct{}) []string {
	names := make([]string, 0, len(allowed))
	for name := range allowed {
		names = append(names, name)
	}
	// Small lists; insertion sort keeps output deterministic without importing sort.
	for i := 1; i < len(names); i++ {
		for j := i; j > 0 && names[j-1] > names[j]; j-- {
			names[j-1], names[j] = names[j], names[j-1]
		}
	}
	return names
}
