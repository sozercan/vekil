package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"
	"testing"
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
