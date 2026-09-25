package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestInitializeMenubarPATHFindsShellCommands(t *testing.T) {
	for _, inheritedCLI := range []bool{false, true} {
		t.Run(strconv.FormatBool(inheritedCLI), func(t *testing.T) {
			loginBin := filepath.Join(t.TempDir(), "login tools")
			interactiveBin := filepath.Join(t.TempDir(), "interactive tools")
			writeMenubarTestCommand(t, loginBin, "az")
			writeMenubarTestCommand(t, interactiveBin, "rtk")
			t.Setenv("VEKIL_TEST_LOGIN_BIN", loginBin)
			t.Setenv("VEKIL_TEST_INTERACTIVE_BIN", interactiveBin)
			t.Setenv("VEKIL_TEST_EXISTING_VALUE", "inherited")
			setMenubarTestShell(t,
				"printf 'login banner\\n'\nexport PATH=\"$VEKIL_TEST_LOGIN_BIN:/usr/bin:/bin\"\nexport VEKIL_TEST_EXISTING_VALUE=from_shell\n",
				"printf 'interactive banner\\n'\nexport PATH=\"$VEKIL_TEST_INTERACTIVE_BIN:$PATH\"\n",
			)

			inheritedPath := "/usr/bin:/bin:/usr/sbin:/sbin"
			wantAzureCLI := filepath.Join(loginBin, "az")
			if inheritedCLI {
				inheritedBin := t.TempDir()
				writeMenubarTestCommand(t, inheritedBin, "az")
				inheritedPath = inheritedBin + ":" + inheritedPath
				wantAzureCLI = filepath.Join(inheritedBin, "az")
			}
			t.Setenv("PATH", inheritedPath)

			if err := initializeMenubarPATH(); err != nil {
				t.Fatal(err)
			}
			if got, err := exec.LookPath("az"); err != nil || got != wantAzureCLI {
				t.Fatalf("Azure CLI lookup = %q, %v; want %q", got, err, wantAzureCLI)
			}
			if got, err := exec.LookPath("rtk"); err != nil || got != filepath.Join(interactiveBin, "rtk") {
				t.Fatalf("interactive shell command lookup = %q, %v", got, err)
			}
			wantPath := inheritedPath + ":" + interactiveBin + ":" + loginBin
			if got := os.Getenv("PATH"); got != wantPath {
				t.Fatalf("PATH = %q, want %q", got, wantPath)
			}
			if got := os.Getenv("VEKIL_TEST_EXISTING_VALUE"); got != "inherited" {
				t.Fatalf("imported a shell variable other than PATH: %q", got)
			}
		})
	}
}

func TestInitializeMenubarPATHDoesNotAddRelativeSearches(t *testing.T) {
	cwd := t.TempDir()
	t.Chdir(cwd)
	writeMenubarTestCommand(t, cwd, "vekil-relative-command")
	t.Setenv("PATH", "/usr/bin:/bin")
	setMenubarTestShell(t, "export PATH='.:relative::/usr/bin:/bin'\n", "")

	if err := initializeMenubarPATH(); err != nil {
		t.Fatal(err)
	}
	if _, err := exec.LookPath("vekil-relative-command"); !errors.Is(err, exec.ErrNotFound) {
		t.Fatalf("command in cwd became discoverable: %v", err)
	}
	if got := os.Getenv("PATH"); got != "/usr/bin:/bin" {
		t.Fatalf("PATH = %q, want only inherited directories", got)
	}
}

func TestInitializeMenubarPATHFallsBackOnFailure(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		shell   string
		timeout bool
	}{
		{name: "missing shell", shell: "/nonexistent-vekil-test-shell"},
		{name: "relative shell", shell: "zsh"},
		{name: "timeout", timeout: true},
		{name: "shell exits early", profile: "printf '/incorrect/path\\n'\nexit 0\n"},
		{name: "shell fails", profile: "exit 1\n"},
		{name: "empty PATH", profile: "export PATH=''\n"},
		{name: "excessive startup output", profile: "/usr/bin/head -c 70000 /dev/zero\n"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			const inheritedPath = "/usr/bin:/bin"
			t.Setenv("PATH", inheritedPath)
			setMenubarTestShell(t, tc.profile, "")
			if tc.shell != "" {
				t.Setenv("SHELL", tc.shell)
			}
			ctx := context.Background()
			if tc.timeout {
				var cancel context.CancelFunc
				ctx, cancel = context.WithDeadline(ctx, time.Unix(1, 0))
				defer cancel()
			}
			err := initializeMenubarPATHWithContext(ctx)
			if err == nil {
				t.Fatal("expected the shell lookup warning despite fallback")
			}
			if tc.timeout && !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("PATH lookup error = %v, want deadline exceeded", err)
			}
			wantPath := inheritedPath + ":/opt/homebrew/bin:/usr/local/bin"
			if got := os.Getenv("PATH"); got != wantPath {
				t.Fatalf("fallback PATH = %q, want %q", got, wantPath)
			}
		})
	}
}

func TestInitializeMenubarPATHFallbackKeepsInheritedPrecedence(t *testing.T) {
	tests := []struct {
		name      string
		inherited string
		want      string
	}{
		{
			name: "empty PATH",
			want: "/opt/homebrew/bin:/usr/local/bin",
		},
		{
			name:      "Homebrew already inherited",
			inherited: "/opt/homebrew/bin:/usr/bin:/bin",
			want:      "/opt/homebrew/bin:/usr/bin:/bin:/usr/local/bin",
		},
		{
			name:      "local bin already inherited",
			inherited: "/usr/local/bin:/usr/bin:/bin",
			want:      "/usr/local/bin:/usr/bin:/bin:/opt/homebrew/bin",
		},
		{
			name:      "both already inherited in reverse order",
			inherited: "/usr/local/bin:/usr/bin:/opt/homebrew/bin:/bin",
			want:      "/usr/local/bin:/usr/bin:/opt/homebrew/bin:/bin",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PATH", tc.inherited)
			t.Setenv("SHELL", "/nonexistent-vekil-test-shell")
			if err := initializeMenubarPATH(); err == nil {
				t.Fatal("expected the shell lookup warning despite fallback")
			}
			if got := os.Getenv("PATH"); got != tc.want {
				t.Fatalf("fallback PATH = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLoginShellPATHCancelsStartupProcesses(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "child.pid")
	t.Setenv("VEKIL_TEST_CHILD_PID_FILE", pidFile)
	setMenubarTestShell(t, "/bin/sleep 30 &\n/usr/bin/printf '%s\\n' \"$!\" > \"$VEKIL_TEST_CHILD_PID_FILE\"\nwait\n", "")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		_, err := loginShellPATH(ctx, "/bin/zsh")
		done <- err
	}()

	var childPID int
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if data, err := os.ReadFile(pidFile); err == nil {
			childPID, _ = strconv.Atoi(strings.TrimSpace(string(data)))
			if childPID > 0 {
				break
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("login shell did not start its child")
	}
	t.Cleanup(func() { _ = syscall.Kill(childPID, syscall.SIGKILL) })
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("PATH lookup error = %v, want cancellation", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("PATH lookup did not stop after cancellation")
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(childPID, 0), syscall.ESRCH) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("login shell child survived cancellation")
}

func setMenubarTestShell(t *testing.T, profile, rc string) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("SHELL", "/bin/zsh")
	t.Setenv("ZDOTDIR", dir)
	for name, contents := range map[string]string{".zprofile": profile, ".zshrc": rc} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func writeMenubarTestCommand(t *testing.T, dir, name string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
}
