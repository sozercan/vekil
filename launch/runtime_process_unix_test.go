//go:build darwin || linux

package launch

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"testing"
	"time"
)

func TestRunForwardsSIGTERMAndReturns143(t *testing.T) {
	capturePath := t.TempDir() + "/child.json"
	proxy := launcherTestProxy()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	signals := make(chan os.Signal, 1)
	time.AfterFunc(150*time.Millisecond, func() { signals <- syscall.SIGTERM })
	result, err := Run(context.Background(), proxy, ClaudeAdapter{}, Options{
		Model:         "claude-public",
		LocalToken:    "test-token-placeholder",
		Binary:        binary,
		ForwardedArgs: []string{"-test.run=TestLaunchHelperProcess"},
		Environment: []string{
			"GO_WANT_LAUNCH_HELPER=1",
			"LAUNCH_HELPER_CAPTURE=" + capturePath,
			"LAUNCH_HELPER_SLEEP_MS=10000",
		},
		Signals: signals,
		Stderr:  &bytes.Buffer{},
		Stdout:  &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 143 {
		t.Fatalf("ExitCode = %d, want 143", result.ExitCode)
	}
}

func TestRunBoundsInheritedPipeWaitBeforeCleanup(t *testing.T) {
	tmp := t.TempDir()
	capturePath := tmp + "/child.json"
	pidPath := tmp + "/grandchild.pid"
	proxy := launcherTestProxy()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	started := time.Now()
	_, err = Run(context.Background(), proxy, ClaudeAdapter{}, Options{
		Model:            "claude-public",
		LocalToken:       "test-token-placeholder",
		Binary:           binary,
		ChildStopTimeout: 150 * time.Millisecond,
		ForwardedArgs:    []string{"-test.run=TestLaunchHelperProcess"},
		Environment: []string{
			"GO_WANT_LAUNCH_HELPER=1",
			"LAUNCH_HELPER_CAPTURE=" + capturePath,
			"LAUNCH_HELPER_SPAWN_GRANDCHILD=1",
			"LAUNCH_HELPER_WAIT_GRANDCHILD_READY=1",
			"LAUNCH_HELPER_GRANDCHILD_INHERIT_STDIO=1",
			"LAUNCH_GRANDCHILD_PID_FILE=" + pidPath,
		},
		Stderr: &bytes.Buffer{},
		Stdout: &bytes.Buffer{},
	})
	if !errors.Is(err, exec.ErrWaitDelay) {
		t.Fatalf("Run() error = %v, want exec.ErrWaitDelay", err)
	}
	if elapsed := time.Since(started); elapsed > launcherTestTimeout(2*time.Second) {
		t.Fatalf("inherited pipe cleanup took %s", elapsed)
	}
	assertProcessGone(t, pidPath)
}

func TestRunNormalExitTerminatesRemainingGrandchild(t *testing.T) {
	tmp := t.TempDir()
	capturePath := tmp + "/child.json"
	pidPath := tmp + "/grandchild.pid"
	proxy := launcherTestProxy()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	result, err := Run(context.Background(), proxy, ClaudeAdapter{}, Options{
		Model:         "claude-public",
		LocalToken:    "test-token-placeholder",
		Binary:        binary,
		ForwardedArgs: []string{"-test.run=TestLaunchHelperProcess"},
		Environment: []string{
			"GO_WANT_LAUNCH_HELPER=1",
			"LAUNCH_HELPER_CAPTURE=" + capturePath,
			"LAUNCH_HELPER_SPAWN_GRANDCHILD=1",
			"LAUNCH_HELPER_WAIT_GRANDCHILD_READY=1",
			"LAUNCH_GRANDCHILD_PID_FILE=" + pidPath,
		},
		Stderr: &bytes.Buffer{},
		Stdout: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0", result.ExitCode)
	}
	assertProcessGone(t, pidPath)
}

func TestRunCancellationTerminatesGrandchildProcess(t *testing.T) {
	tmp := t.TempDir()
	capturePath := tmp + "/child.json"
	pidPath := tmp + "/grandchild.pid"
	proxy := launcherTestProxy()
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	grandchildReady := make(chan bool, 1)
	go func() {
		deadline := time.Now().Add(launcherTestTimeout(2 * time.Second))
		for time.Now().Before(deadline) {
			if _, err := os.Stat(pidPath); err == nil {
				grandchildReady <- true
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		grandchildReady <- false
		cancel()
	}()
	result, err := Run(ctx, proxy, ClaudeAdapter{}, Options{
		Model:         "claude-public",
		LocalToken:    "test-token-placeholder",
		Binary:        binary,
		ForwardedArgs: []string{"-test.run=TestLaunchHelperProcess"},
		Environment: []string{
			"GO_WANT_LAUNCH_HELPER=1",
			"LAUNCH_HELPER_CAPTURE=" + capturePath,
			"LAUNCH_HELPER_SLEEP_MS=10000",
			"LAUNCH_HELPER_SPAWN_GRANDCHILD=1",
			"LAUNCH_GRANDCHILD_PID_FILE=" + pidPath,
		},
		Stderr: &bytes.Buffer{},
		Stdout: &bytes.Buffer{},
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if result.ExitCode != 130 {
		t.Fatalf("ExitCode = %d, want 130", result.ExitCode)
	}
	if !<-grandchildReady {
		t.Fatal("grandchild did not become ready before cancellation deadline")
	}
	assertProcessGone(t, pidPath)
}

func assertProcessGone(t *testing.T, pidPath string) {
	t.Helper()
	body, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}
	pid, err := strconv.Atoi(string(body))
	if err != nil {
		t.Fatalf("parse grandchild pid: %v", err)
	}
	deadline := time.Now().Add(launcherTestTimeout(2 * time.Second))
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if errors.Is(err, syscall.ESRCH) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("grandchild process %d survived launcher cleanup", pid)
}

func TestLaunchGrandchildProcess(t *testing.T) {
	if os.Getenv("GO_WANT_LAUNCH_GRANDCHILD") != "1" {
		return
	}
	if err := os.WriteFile(
		os.Getenv("LAUNCH_GRANDCHILD_PID_FILE"),
		[]byte(strconv.Itoa(os.Getpid())),
		0o600,
	); err != nil {
		os.Exit(96)
	}
	time.Sleep(30 * time.Second)
	os.Exit(0)
}
