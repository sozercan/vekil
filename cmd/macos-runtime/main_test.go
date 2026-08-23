package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/internal/macosruntime"
)

func TestParseOptionsSupportsInjectedHostAndPortZero(t *testing.T) {
	opts, err := parseOptions([]string{"--host", "127.0.0.2", "--port", "0", "--state-dir", t.TempDir(), "--parent-pid", "42"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if opts.host != "127.0.0.2" || opts.port != "0" || opts.parentPID != 42 {
		t.Fatalf("options = %+v", opts)
	}
}

func TestRunEmitsLdflagsHelloFirstAndKeepsStdoutProtocolOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS runtime state lock is unavailable on Windows")
	}

	previousBuild := buildVersion
	previousBundle := bundleBuildID
	buildVersion = "1.2.3-test"
	bundleBuildID = "456"
	t.Cleanup(func() { buildVersion = previousBuild; bundleBuildID = previousBundle })

	input := strings.NewReader("{\"v\":1,\"id\":\"req_shutdown\",\"command\":\"shutdown\",\"payload\":{}}\n")
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if code := run([]string{"--host", "127.0.0.1", "--port", "0", "--state-dir", t.TempDir(), "--log-level", "error"}, input, &stdout, &stderr); code != 0 {
		t.Fatalf("run() code = %d, stderr = %s", code, stderr.String())
	}
	scanner := bufio.NewScanner(bytes.NewReader(stdout.Bytes()))
	if !scanner.Scan() {
		t.Fatal("missing hello frame")
	}
	var hello struct {
		Event   string `json:"event"`
		Payload struct {
			ProtocolMin   int    `json:"protocol_min"`
			ProtocolMax   int    `json:"protocol_max"`
			HelperBuild   string `json:"helper_build"`
			BundleBuildID string `json:"bundle_build_id"`
			PID           int    `json:"pid"`
			HelperEpoch   string `json:"helper_epoch"`
		} `json:"payload"`
	}
	if err := json.Unmarshal(scanner.Bytes(), &hello); err != nil {
		t.Fatal(err)
	}
	if hello.Event != "hello" || hello.Payload.ProtocolMin != 1 || hello.Payload.ProtocolMax != 1 || hello.Payload.HelperBuild != "1.2.3-test" || hello.Payload.BundleBuildID != "456" || hello.Payload.PID <= 0 || hello.Payload.HelperEpoch == "" {
		t.Fatalf("hello = %+v", hello)
	}
	for scanner.Scan() {
		var envelope map[string]json.RawMessage
		if err := json.Unmarshal(scanner.Bytes(), &envelope); err != nil {
			t.Fatalf("stdout contains non-protocol line %q: %v", scanner.Text(), err)
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestRunWaitsForPreviousHelperStateOwner(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("macOS runtime state lock is unavailable on Windows")
	}

	stateDir := t.TempDir()
	lock, err := macosruntime.AcquireHelperStateLock(
		context.Background(),
		macosruntime.PathsInDirectory(stateDir),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = lock.Close() })

	done := make(chan int, 1)
	go func() {
		done <- run(
			[]string{"--host", "127.0.0.1", "--port", "0", "--state-dir", stateDir, "--log-level", "error"},
			strings.NewReader("{\"v\":1,\"id\":\"req_shutdown\",\"command\":\"shutdown\",\"payload\":{}}\n"),
			&bytes.Buffer{},
			&bytes.Buffer{},
		)
	}()

	select {
	case code := <-done:
		t.Fatalf("replacement helper returned before state ownership was released: %d", code)
	case <-time.After(100 * time.Millisecond):
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("replacement helper exit code = %d", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("replacement helper did not start after state ownership was released")
	}
}
