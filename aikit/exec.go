package aikit

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Executor runs container-engine CLI commands. Tests substitute a fake.
type Executor interface {
	// LookPath resolves a command name.
	LookPath(name string) (string, error)
	// Output runs a command and returns stdout. A failing command returns an
	// error that includes trimmed stderr.
	Output(ctx context.Context, name string, args ...string) ([]byte, error)
	// Stream runs a command and returns its stdout as a stream. Closing the
	// stream stops the command.
	Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error)
	// Run runs a command with stdout and stderr connected to w.
	Run(ctx context.Context, w io.Writer, name string, args ...string) error
}

// OSExecutor runs commands with os/exec.
type OSExecutor struct {
	// Env is the command environment; nil inherits the process environment.
	Env []string
}

// LookPath implements Executor.
func (e OSExecutor) LookPath(name string) (string, error) { return exec.LookPath(name) }

// Output implements Executor.
func (e OSExecutor) Output(ctx context.Context, name string, args ...string) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = e.Env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return out, commandError(name, args, err, stderr.String())
	}
	return out, nil
}

// Stream implements Executor.
func (e OSExecutor) Stream(ctx context.Context, name string, args ...string) (io.ReadCloser, error) {
	streamCtx, cancel := context.WithCancel(ctx)
	cmd := exec.CommandContext(streamCtx, name, args...)
	cmd.Env = e.Env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		cancel()
		return nil, commandError(name, args, err, stderr.String())
	}
	return &commandStream{ReadCloser: stdout, cmd: cmd, cancel: cancel, stderr: &stderr, name: name, args: args}, nil
}

// Run implements Executor.
func (e OSExecutor) Run(ctx context.Context, w io.Writer, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Env = e.Env
	var stderr bytes.Buffer
	cmd.Stdout = w
	cmd.Stderr = io.MultiWriter(w, &stderr)
	if err := cmd.Run(); err != nil {
		return commandError(name, args, err, stderr.String())
	}
	return nil
}

type commandStream struct {
	io.ReadCloser
	cmd    *exec.Cmd
	cancel context.CancelFunc
	stderr *bytes.Buffer
	name   string
	args   []string
	closed bool
}

// Close stops the command. Stopping early is expected when a caller has read
// all it needs, so the resulting kill is not reported as an error.
func (s *commandStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.cancel()
	_ = s.ReadCloser.Close()
	_ = s.cmd.Wait()
	return nil
}

// Err reports a command failure after the stream was fully read.
func (s *commandStream) Err() error {
	if s.cmd.ProcessState != nil && !s.cmd.ProcessState.Success() {
		return commandError(s.name, s.args, errors.New(s.cmd.ProcessState.String()), s.stderr.String())
	}
	return nil
}

func commandError(name string, args []string, err error, stderr string) error {
	detail := strings.TrimSpace(stderr)
	if len(detail) > 2000 {
		detail = detail[len(detail)-2000:]
	}
	command := name
	if len(args) > 0 {
		command += " " + args[0]
	}
	if detail == "" {
		return fmt.Errorf("%s: %w", command, err)
	}
	return fmt.Errorf("%s: %w: %s", command, err, detail)
}
