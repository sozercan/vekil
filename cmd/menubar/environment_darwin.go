package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

func initializeMenubarPATH() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return initializeMenubarPATHWithContext(ctx)
}

func initializeMenubarPATHWithContext(ctx context.Context) error {
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/zsh"
	}
	shellPath, lookupErr := loginShellPATH(ctx, shell)
	if lookupErr != nil {
		shellPath = "/opt/homebrew/bin:/usr/local/bin"
	}

	paths := filepath.SplitList(os.Getenv("PATH"))
	seen := make(map[string]bool, len(paths))
	for _, path := range paths {
		seen[path] = true
	}
	for _, path := range filepath.SplitList(shellPath) {
		if filepath.IsAbs(path) && !seen[path] {
			paths = append(paths, path)
			seen[path] = true
		}
	}
	return errors.Join(lookupErr, os.Setenv("PATH", strings.Join(paths, string(os.PathListSeparator))))
}

func loginShellPATH(ctx context.Context, shell string) (string, error) {
	if !filepath.IsAbs(shell) {
		return "", fmt.Errorf("login shell must be an absolute path")
	}
	cmd := exec.CommandContext(ctx, shell, "-ilc", `/usr/bin/printf '\000'; exec /usr/bin/printenv PATH`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = 100 * time.Millisecond
	var output shellPATHOutput
	cmd.Stdout = &output
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("read login shell PATH: %w", ctx.Err())
		}
		return "", fmt.Errorf("read login shell PATH: %w", err)
	}

	marker := bytes.LastIndexByte(output.data, 0)
	if marker < 0 || !bytes.HasSuffix(output.data, []byte("\n")) {
		return "", fmt.Errorf("login shell did not return PATH")
	}
	path := string(output.data[marker+1 : len(output.data)-1])
	if path == "" {
		return "", fmt.Errorf("login shell returned an empty PATH")
	}
	return path, nil
}

type shellPATHOutput struct {
	data []byte
}

func (b *shellPATHOutput) Write(p []byte) (int, error) {
	if len(b.data)+len(p) > 64<<10 {
		return 0, fmt.Errorf("login shell output exceeds 64 KiB")
	}
	b.data = append(b.data, p...)
	return len(p), nil
}
