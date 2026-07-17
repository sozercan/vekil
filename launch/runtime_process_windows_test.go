//go:build windows

package launch

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"golang.org/x/sys/windows"
)

func TestWindowsProcessControllerResumesAndTerminatesDescendants(t *testing.T) {
	tmp := t.TempDir()
	startedPath := tmp + `\started`
	pidPath := tmp + `\grandchild.pid`
	binary, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable(): %v", err)
	}
	cmd := exec.Command(binary, "-test.run=TestWindowsLaunchControllerHelper")
	cmd.Env = append(os.Environ(),
		"GO_WANT_WINDOWS_LAUNCH_HELPER=1",
		"WINDOWS_LAUNCH_HELPER_STARTED="+startedPath,
		"WINDOWS_LAUNCH_GRANDCHILD_PID="+pidPath,
	)
	cmd.Stdout = &bytes.Buffer{}
	cmd.Stderr = &bytes.Buffer{}

	controller, err := newProcessController(cmd, nil)
	if err != nil {
		t.Fatalf("newProcessController() error = %v", err)
	}
	defer func() { _ = controller.close() }()
	if err := cmd.Start(); err != nil {
		t.Fatalf("start suspended helper: %v", err)
	}
	if err := controller.afterStart(); err != nil {
		_ = controller.kill()
		_ = controller.wait()
		t.Fatalf("assign and resume helper: %v", err)
	}
	waitForWindowsTestFile(t, startedPath)
	waitForWindowsTestFile(t, pidPath)

	waitCh := make(chan commandOutcome, 1)
	go func() { waitCh <- controller.wait() }()
	if err := controller.close(); err != nil {
		t.Fatalf("close launcher Job Object: %v", err)
	}
	select {
	case <-waitCh:
	case <-time.After(5 * time.Second):
		_ = controller.kill()
		t.Fatal("helper did not exit after Job Object cleanup")
	}
	assertWindowsProcessGone(t, pidPath)
}

func TestWindowsLaunchControllerHelper(t *testing.T) {
	if os.Getenv("GO_WANT_WINDOWS_LAUNCH_HELPER") != "1" {
		return
	}
	if err := os.WriteFile(os.Getenv("WINDOWS_LAUNCH_HELPER_STARTED"), []byte("started"), 0o600); err != nil {
		os.Exit(98)
	}
	grandchild := exec.Command(os.Args[0], "-test.run=TestWindowsLaunchGrandchildProcess")
	grandchild.Env = append(os.Environ(), "GO_WANT_WINDOWS_LAUNCH_GRANDCHILD=1")
	if err := grandchild.Start(); err != nil {
		os.Exit(97)
	}
	waitForWindowsHelperFile(os.Getenv("WINDOWS_LAUNCH_GRANDCHILD_PID"))
	time.Sleep(30 * time.Second)
}

func TestWindowsLaunchGrandchildProcess(t *testing.T) {
	if os.Getenv("GO_WANT_WINDOWS_LAUNCH_GRANDCHILD") != "1" {
		return
	}
	if err := os.WriteFile(
		os.Getenv("WINDOWS_LAUNCH_GRANDCHILD_PID"),
		[]byte(strconv.Itoa(os.Getpid())),
		0o600,
	); err != nil {
		os.Exit(96)
	}
	time.Sleep(30 * time.Second)
}

func waitForWindowsTestFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}

func waitForWindowsHelperFile(path string) {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	os.Exit(95)
}

func assertWindowsProcessGone(t *testing.T, pidPath string) {
	t.Helper()
	body, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("read grandchild pid: %v", err)
	}
	pid, err := strconv.Atoi(string(body))
	if err != nil {
		t.Fatalf("parse grandchild pid: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		handle, openErr := windows.OpenProcess(windows.SYNCHRONIZE, false, uint32(pid))
		if errors.Is(openErr, windows.ERROR_INVALID_PARAMETER) {
			return
		}
		if openErr != nil {
			t.Fatalf("open grandchild process %d: %v", pid, openErr)
		}
		status, waitErr := windows.WaitForSingleObject(handle, 0)
		_ = windows.CloseHandle(handle)
		if waitErr == nil && status == windows.WAIT_OBJECT_0 {
			return
		}
		if waitErr != nil {
			t.Fatalf("query grandchild process %d: %v", pid, waitErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("grandchild process %d survived launcher cleanup", pid)
}
