package proxy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodeExecLocalBackend_SuccessfulCommand(t *testing.T) {
	backend := NewLocalProcessBackend()
	dir := t.TempDir()

	result, err := backend.RunCommand(context.Background(), CodeExecRequest{
		ToolUseID: "toolu_1",
		ToolName:  "Bash",
		Command:   "echo hello",
		Policy: CodeExecPolicy{
			Timeout:        5 * time.Second,
			MaxOutputBytes: 1024,
			WorkingDir:     dir,
		},
	})
	if err != nil {
		t.Fatalf("RunCommand error = %v", err)
	}
	if result.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", result.ExitCode)
	}
	if strings.TrimSpace(result.Stdout) != "hello" {
		t.Errorf("Stdout = %q, want \"hello\"", result.Stdout)
	}
	if result.TimedOut {
		t.Error("TimedOut = true, want false")
	}
	if result.Backend != "local" {
		t.Errorf("Backend = %q, want local", result.Backend)
	}
	if result.ToolUseID != "toolu_1" {
		t.Errorf("ToolUseID = %q, want toolu_1", result.ToolUseID)
	}
	if result.Command != "echo hello" {
		t.Errorf("Command = %q, want \"echo hello\"", result.Command)
	}
}

func TestCodeExecLocalBackend_NonZeroExit(t *testing.T) {
	backend := NewLocalProcessBackend()
	dir := t.TempDir()

	result, err := backend.RunCommand(context.Background(), CodeExecRequest{
		Command: "echo oops >&2; exit 3",
		Policy: CodeExecPolicy{
			Timeout:        5 * time.Second,
			MaxOutputBytes: 1024,
			WorkingDir:     dir,
		},
	})
	if err != nil {
		t.Fatalf("RunCommand error = %v, want nil (non-zero exit is not a backend error)", err)
	}
	if result.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want 3", result.ExitCode)
	}
	if strings.TrimSpace(result.Stderr) != "oops" {
		t.Errorf("Stderr = %q, want \"oops\"", result.Stderr)
	}
}

func TestCodeExecLocalBackend_TimeoutEnforced(t *testing.T) {
	backend := NewLocalProcessBackend()
	dir := t.TempDir()

	start := time.Now()
	result, err := backend.RunCommand(context.Background(), CodeExecRequest{
		Command: "sleep 10",
		Policy: CodeExecPolicy{
			Timeout:        100 * time.Millisecond,
			MaxOutputBytes: 1024,
			WorkingDir:     dir,
		},
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("RunCommand error = %v, want nil (timeout is reported in result)", err)
	}
	if !result.TimedOut {
		t.Error("TimedOut = false, want true")
	}
	if elapsed > 5*time.Second {
		t.Errorf("command ran for %v, timeout was not enforced", elapsed)
	}
}

func TestCodeExecLocalBackend_OutputTruncation(t *testing.T) {
	backend := NewLocalProcessBackend()
	dir := t.TempDir()

	result, err := backend.RunCommand(context.Background(), CodeExecRequest{
		// Emit 1000 bytes but cap capture at 100.
		Command: "printf 'a%.0s' $(seq 1 1000)",
		Policy: CodeExecPolicy{
			Timeout:        5 * time.Second,
			MaxOutputBytes: 100,
			WorkingDir:     dir,
		},
	})
	if err != nil {
		t.Fatalf("RunCommand error = %v", err)
	}
	if len(result.Stdout) > 100 {
		t.Errorf("Stdout length = %d, want <= 100 (capped)", len(result.Stdout))
	}
	if !result.Truncated {
		t.Error("Truncated = false, want true when output exceeds cap")
	}
}

func TestCodeExecLocalBackend_WorkingDirRequired(t *testing.T) {
	backend := NewLocalProcessBackend()

	_, err := backend.RunCommand(context.Background(), CodeExecRequest{
		Command: "echo hi",
		Policy: CodeExecPolicy{
			Timeout:        5 * time.Second,
			MaxOutputBytes: 1024,
			// No WorkingDir: must fail closed.
		},
	})
	if err == nil {
		t.Fatal("RunCommand error = nil, want error when working directory is not configured")
	}
	if !strings.Contains(err.Error(), "working directory") {
		t.Errorf("error = %v, want working directory message", err)
	}
}

func TestCodeExecLocalBackend_WorkingDirRejectsMissing(t *testing.T) {
	backend := NewLocalProcessBackend()
	missing := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := backend.RunCommand(context.Background(), CodeExecRequest{
		Command: "echo hi",
		Policy: CodeExecPolicy{
			Timeout:        5 * time.Second,
			MaxOutputBytes: 1024,
			WorkingDir:     missing,
		},
	})
	if err == nil {
		t.Fatal("RunCommand error = nil, want error for missing working directory")
	}
}

func TestCodeExecLocalBackend_WorkingDirBoundary(t *testing.T) {
	backend := NewLocalProcessBackend()
	dir := t.TempDir()

	result, err := backend.RunCommand(context.Background(), CodeExecRequest{
		Command: "pwd",
		Policy: CodeExecPolicy{
			Timeout:        5 * time.Second,
			MaxOutputBytes: 1024,
			WorkingDir:     dir,
		},
	})
	if err != nil {
		t.Fatalf("RunCommand error = %v", err)
	}
	// macOS resolves /var -> /private/var; compare the base name to stay portable.
	if filepath.Base(strings.TrimSpace(result.Stdout)) != filepath.Base(dir) {
		t.Errorf("pwd = %q, want directory ending in %q", strings.TrimSpace(result.Stdout), filepath.Base(dir))
	}
}

func TestCodeExecLocalBackend_EnvAllowlistFilters(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VEKIL_TEST_ALLOWED", "allowed-value")
	t.Setenv("VEKIL_TEST_SECRET", "secret-value")

	backend := NewLocalProcessBackend()
	result, err := backend.RunCommand(context.Background(), CodeExecRequest{
		Command: "echo allowed=$VEKIL_TEST_ALLOWED secret=$VEKIL_TEST_SECRET",
		Policy: CodeExecPolicy{
			Timeout:        5 * time.Second,
			MaxOutputBytes: 1024,
			WorkingDir:     dir,
			EnvAllowlist:   []string{"VEKIL_TEST_ALLOWED"},
		},
	})
	if err != nil {
		t.Fatalf("RunCommand error = %v", err)
	}
	out := strings.TrimSpace(result.Stdout)
	if !strings.Contains(out, "allowed=allowed-value") {
		t.Errorf("Stdout = %q, want allowlisted var present", out)
	}
	if strings.Contains(out, "secret-value") {
		t.Errorf("Stdout = %q, leaked non-allowlisted secret var", out)
	}
}

func TestCodeExecLocalBackend_EmptyEnvByDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VEKIL_TEST_LEAK", "leak-value")

	backend := NewLocalProcessBackend()
	result, err := backend.RunCommand(context.Background(), CodeExecRequest{
		Command: "echo leak=$VEKIL_TEST_LEAK",
		Policy: CodeExecPolicy{
			Timeout:        5 * time.Second,
			MaxOutputBytes: 1024,
			WorkingDir:     dir,
			// No EnvAllowlist: nothing should be exported.
		},
	})
	if err != nil {
		t.Fatalf("RunCommand error = %v", err)
	}
	if strings.Contains(result.Stdout, "leak-value") {
		t.Errorf("Stdout = %q, leaked env var with empty allowlist", result.Stdout)
	}
}

func TestCodeExecLocalBackend_EmptyCommand(t *testing.T) {
	backend := NewLocalProcessBackend()
	_, err := backend.RunCommand(context.Background(), CodeExecRequest{
		Command: "   ",
		Policy: CodeExecPolicy{
			Timeout:    5 * time.Second,
			WorkingDir: t.TempDir(),
		},
	})
	if err == nil {
		t.Fatal("RunCommand error = nil, want error for empty command")
	}
}

func TestFilteredEnv_OnlyAllowlistedPresent(t *testing.T) {
	t.Setenv("VEKIL_FE_ONE", "1")
	t.Setenv("VEKIL_FE_TWO", "2")

	env := filteredEnv([]string{"VEKIL_FE_ONE", "VEKIL_FE_MISSING"})
	if len(env) != 1 {
		t.Fatalf("filteredEnv length = %d, want 1 (only present+allowlisted)", len(env))
	}
	if env[0] != "VEKIL_FE_ONE=1" {
		t.Errorf("filteredEnv[0] = %q, want VEKIL_FE_ONE=1", env[0])
	}
}

func TestFilteredEnv_EmptyAllowlistYieldsEmpty(t *testing.T) {
	// Ensure at least one var exists in the environment.
	if len(os.Environ()) == 0 {
		t.Skip("no environment to test against")
	}
	env := filteredEnv(nil)
	if len(env) != 0 {
		t.Errorf("filteredEnv(nil) length = %d, want 0", len(env))
	}
}
