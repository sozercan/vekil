//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris || windows

package proxy

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	optimizerHelperEnv            = "VEKIL_OPTIMIZER_HELPER"
	optimizerHelperRoleEnv        = "VEKIL_OPTIMIZER_HELPER_ROLE"
	optimizerHelperModeEnv        = "VEKIL_OPTIMIZER_HELPER_MODE"
	optimizerHelperKindEnv        = "VEKIL_OPTIMIZER_HELPER_KIND"
	optimizerHelperPIDEnv         = "VEKIL_OPTIMIZER_HELPER_PID_FILE"
	optimizerHelperReadyEnv       = "VEKIL_OPTIMIZER_HELPER_READY_FILE"
	optimizerHelperReleaseEnv     = "VEKIL_OPTIMIZER_HELPER_RELEASE_FILE"
	optimizerHelperLateDelayMSEnv = "VEKIL_OPTIMIZER_HELPER_LATE_DELAY_MS"
)

func TestMain(m *testing.M) {
	if os.Getenv(optimizerHelperEnv) == "1" {
		os.Exit(runOptimizerProcessHelper())
	}
	os.Exit(m.Run())
}

func runOptimizerProcessHelper() int {
	if os.Getenv(optimizerHelperRoleEnv) == "descendant" {
		return runOptimizerProcessDescendant()
	}
	return runOptimizerProcessParent()
}

func runOptimizerProcessParent() int {
	mode := os.Getenv(optimizerHelperModeEnv)
	child := exec.Command(os.Args[0])
	child.Env = append(os.Environ(), optimizerHelperRoleEnv+"=descendant")

	var redirected *os.File
	if mode == "redirected" {
		var err error
		redirected, err = os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			return 11
		}
		defer func() { _ = redirected.Close() }()
		child.Stdout = redirected
		child.Stderr = redirected
	} else {
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
	}

	if err := child.Start(); err != nil {
		return 12
	}
	if pidFile := os.Getenv(optimizerHelperPIDEnv); pidFile != "" {
		if err := os.WriteFile(pidFile, []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = child.Process.Kill()
			return 13
		}
	}

	if mode == "redirected" {
		_, _ = io.WriteString(os.Stdout, optimizerHelperSuccessfulOutput())
		return 0
	}
	if mode == "late-inherited" && !waitForOptimizerHelperFile(os.Getenv(optimizerHelperReleaseEnv), 10*time.Second) {
		return 14
	}
	return 0
}

func runOptimizerProcessDescendant() int {
	if os.Getenv(optimizerHelperModeEnv) == "late-inherited" {
		if readyFile := os.Getenv(optimizerHelperReadyEnv); readyFile != "" {
			if err := os.WriteFile(readyFile, []byte("ready"), 0o600); err != nil {
				return 21
			}
		}
		if !waitForOptimizerHelperFile(os.Getenv(optimizerHelperReleaseEnv), 10*time.Second) {
			return 22
		}
		delay := 150 * time.Millisecond
		if rawDelay := os.Getenv(optimizerHelperLateDelayMSEnv); rawDelay != "" {
			delayMS, err := strconv.Atoi(rawDelay)
			if err != nil || delayMS < 0 {
				return 23
			}
			delay = time.Duration(delayMS) * time.Millisecond
		}
		time.Sleep(delay)
		_, _ = io.WriteString(os.Stdout, optimizerHelperSuccessfulOutput())
		_, _ = io.WriteString(os.Stderr, "optimizer descendant still owns stderr\n")
	}
	time.Sleep(time.Second)
	return 0
}

func waitForOptimizerHelperFile(path string, timeout time.Duration) bool {
	if path == "" {
		return true
	}
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return true
		} else if !os.IsNotExist(err) {
			return false
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func optimizerHelperSuccessfulOutput() string {
	if os.Getenv(optimizerHelperKindEnv) == "exec_json" {
		return `{"changed":true,"command":"optimized command"}`
	}
	return "optimized command\n"
}

func TestOptimizerAdaptersRejectLateDescendantOutputWithinDeadline(t *testing.T) {
	const (
		timeoutMS        = 2000
		releaseLead      = 50 * time.Millisecond
		lateOutputDelay  = 150 * time.Millisecond
		startupGrace     = 1500 * time.Millisecond
		returnGrace      = 300 * time.Millisecond
		minimumRunWindow = 1700 * time.Millisecond
	)

	for _, kind := range []string{"exec_json", "rtk_cli"} {
		t.Run(kind, func(t *testing.T) {
			tempDir := t.TempDir()
			pidFile := filepath.Join(tempDir, "descendant.pid")
			readyFile := filepath.Join(tempDir, "descendant.ready")
			releaseFile := filepath.Join(tempDir, "release")
			t.Setenv(optimizerHelperEnv, "1")
			t.Setenv(optimizerHelperRoleEnv, "parent")
			t.Setenv(optimizerHelperModeEnv, "late-inherited")
			t.Setenv(optimizerHelperKindEnv, kind)
			t.Setenv(optimizerHelperPIDEnv, pidFile)
			t.Setenv(optimizerHelperReadyEnv, readyFile)
			t.Setenv(optimizerHelperReleaseEnv, releaseFile)
			t.Setenv(optimizerHelperLateDelayMSEnv, strconv.Itoa(int(lateOutputDelay/time.Millisecond)))

			providerStarted := make(chan time.Time, 1)
			manager := optimizerProcessTestManagerWithStartSignal(kind, timeoutMS, providerStarted)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			type rewriteOutcome struct {
				result  ToolCommandRewriteResult
				elapsed time.Duration
			}
			outcomeCh := make(chan rewriteOutcome, 1)
			finished := make(chan struct{})
			go func() {
				defer close(finished)
				started := time.Now()
				result := manager.RewriteCommand(ctx, ToolCommandRewriteRequest{
					ToolName: "shell_command",
					CallID:   "call-late-output",
					Command:  "original command",
				})
				outcomeCh <- rewriteOutcome{result: result, elapsed: time.Since(started)}
			}()
			defer func() {
				cancel()
				select {
				case <-finished:
				case <-time.After(time.Second):
				}
			}()

			var providerStartedAt time.Time
			select {
			case providerStartedAt = <-providerStarted:
			case <-time.After(time.Second):
				t.Fatal("optimizer provider did not start")
			}
			pid := readOptimizerHelperPIDWithin(t, pidFile, startupGrace)
			waitForOptimizerHelperFileWithin(t, readyFile, startupGrace)

			releaseAt := providerStartedAt.Add(time.Duration(timeoutMS)*time.Millisecond - releaseLead)
			if remaining := time.Until(releaseAt); remaining > 0 {
				time.Sleep(remaining)
			}
			if time.Now().After(providerStartedAt.Add(time.Duration(timeoutMS) * time.Millisecond)) {
				t.Fatal("descendant startup handshake missed the test deadline")
			}
			if err := os.WriteFile(releaseFile, []byte("release"), 0o600); err != nil {
				t.Fatalf("release optimizer helper: %v", err)
			}

			var outcome rewriteOutcome
			select {
			case outcome = <-outcomeCh:
			case <-time.After(time.Until(providerStartedAt.Add(time.Duration(timeoutMS)*time.Millisecond + returnGrace))):
				t.Fatal("optimizer did not return within timeout plus cleanup grace")
			}

			t.Logf("%dms timeout returned in %v", timeoutMS, outcome.elapsed)
			if outcome.result.Changed {
				t.Fatalf("late optimizer result was accepted: %+v", outcome.result)
			}
			if outcome.elapsed < minimumRunWindow || outcome.elapsed > time.Duration(timeoutMS)*time.Millisecond+returnGrace {
				t.Fatalf("optimizer returned after %v, want a tightly bounded deadline window", outcome.elapsed)
			}
			waitForOptimizerProcessExit(t, pid, 750*time.Millisecond)
		})
	}
}

func TestOptimizerAdaptersCleanRedirectedDescendantsAfterSuccess(t *testing.T) {
	for _, kind := range []string{"exec_json", "rtk_cli"} {
		t.Run(kind, func(t *testing.T) {
			pidFile := filepath.Join(t.TempDir(), "descendant.pid")
			t.Setenv(optimizerHelperEnv, "1")
			t.Setenv(optimizerHelperRoleEnv, "parent")
			t.Setenv(optimizerHelperModeEnv, "redirected")
			t.Setenv(optimizerHelperKindEnv, kind)
			t.Setenv(optimizerHelperPIDEnv, pidFile)

			manager := optimizerProcessTestManager(kind, 3000)
			result := manager.RewriteCommand(context.Background(), ToolCommandRewriteRequest{
				ToolName: "shell_command",
				CallID:   "call-redirected-output",
				Command:  "original command",
			})
			if !result.Changed || strings.TrimSpace(result.Command) != "optimized command" {
				t.Fatalf("optimizer result = %+v, want successful rewrite", result)
			}
			pid := readOptimizerHelperPID(t, pidFile)
			waitForOptimizerProcessExit(t, pid, 750*time.Millisecond)
		})
	}
}

func optimizerProcessTestManager(kind string, timeoutMS int) *ToolOptimizerManager {
	return optimizerProcessTestManagerWithStartSignal(kind, timeoutMS, nil)
}

func optimizerProcessTestManagerWithStartSignal(kind string, timeoutMS int, started chan<- time.Time) *ToolOptimizerManager {
	cfg := ToolOptimizersConfig{
		Enabled: true,
		CommandRewrite: ToolOptimizerRewriteConfig{
			Enabled:   true,
			TimeoutMS: timeoutMS,
		},
	}
	providerCfg := ToolOptimizerProviderConfig{Path: os.Args[0]}
	var optimizer ToolOptimizer
	switch kind {
	case "exec_json":
		optimizer = newExecJSONToolOptimizer(providerCfg)
	case "rtk_cli":
		optimizer = newRTKToolOptimizer(providerCfg)
	default:
		panic(fmt.Sprintf("unknown optimizer kind %q", kind))
	}
	if started != nil {
		optimizer = &signalingToolOptimizer{ToolOptimizer: optimizer, started: started}
	}
	return NewToolOptimizerManager(cfg, []stagedToolOptimizer{{
		optimizer:      optimizer,
		commandRewrite: true,
	}})
}

type signalingToolOptimizer struct {
	ToolOptimizer
	started chan<- time.Time
	once    sync.Once
}

func (o *signalingToolOptimizer) RewriteCommand(ctx context.Context, req ToolCommandRewriteRequest) (ToolCommandRewriteResult, error) {
	o.once.Do(func() {
		if o.started != nil {
			o.started <- time.Now()
		}
	})
	return o.ToolOptimizer.RewriteCommand(ctx, req)
}

func (*signalingToolOptimizer) optimizerUsesExternalProcess() {}

func readOptimizerHelperPID(t *testing.T, path string) int {
	t.Helper()
	return readOptimizerHelperPIDWithin(t, path, 500*time.Millisecond)
}

func readOptimizerHelperPIDWithin(t *testing.T, path string, timeout time.Duration) int {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
			if err != nil {
				t.Fatalf("parse descendant pid %q: %v", data, err)
			}
			return pid
		}
		if !os.IsNotExist(err) {
			t.Fatalf("read descendant pid: %v", err)
		}
		if time.Now().After(deadline) {
			t.Fatalf("descendant pid file %s was not created within %v", path, timeout)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func waitForOptimizerHelperFileWithin(t *testing.T, path string, timeout time.Duration) {
	t.Helper()
	if !waitForOptimizerHelperFile(path, timeout) {
		t.Fatalf("optimizer helper file %s was not created within %v", path, timeout)
	}
}

func waitForOptimizerProcessExit(t *testing.T, pid int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for optimizerTestProcessAlive(pid) {
		if time.Now().After(deadline) {
			t.Fatalf("optimizer descendant pid %d remained alive after %v", pid, timeout)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
