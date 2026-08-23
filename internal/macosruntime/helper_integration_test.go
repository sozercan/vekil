package macosruntime

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/internal/appcontrol"
	"github.com/sozercan/vekil/logger"
)

func TestHelperHelloStartPortZeroHealthAndStdinEOF(t *testing.T) {
	manager := newManagerForTest(t)
	configPath := filepath.Join(t.TempDir(), "providers.yaml")
	configBody := []byte("schema_version: 2\nproviders:\n  - id: local\n    type: openai-compatible\n    default: true\n    base_url: https://example.test/v1\n    auth_type: none\n    model_discovery: static\n    models:\n      - public_id: test-model\n        endpoints: [/chat/completions]\n")
	if err := os.WriteFile(configPath, configBody, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SelectExternal(t.Context(), configPath); err != nil {
		t.Fatal(err)
	}

	authenticator := auth.NewTestAuthenticator("test-token")
	secrets := NewSecretProjectionStore()
	factory, err := NewRuntimeFactory(RuntimeFactoryOptions{
		Authenticator: authenticator, Logger: logger.NewWithWriter(logger.LevelError, io.Discard), Host: "127.0.0.1", Port: "0", Secrets: secrets,
	})
	if err != nil {
		t.Fatal(err)
	}
	controller, err := appcontrol.New(appcontrol.Options{
		ConfigurationSource: manager, ConfigurationObserver: manager, RuntimeFactory: factory,
		Authenticator: authenticator, ReadinessChecker: appcontrol.HTTPReadinessChecker{}, StopTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	stdinReader, stdinWriter := io.Pipe()
	stdoutReader, stdoutWriter := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- RunHelper(t.Context(), HelperOptions{
			Stdin: stdinReader, Stdout: stdoutWriter, Stderr: io.Discard,
			Controller: controller, Configuration: manager, Secrets: secrets,
			ProtocolMin: 1, ProtocolMax: 1, HelperBuild: "test-helper", BundleBuildID: "test-bundle", HelperEpoch: "hep_test", PID: 4242,
			ShutdownTimeout: 2 * time.Second,
		})
	}()
	lines := make(chan []byte, 64)
	readErr := make(chan error, 1)
	go func() {
		scanner := bufio.NewScanner(stdoutReader)
		scanner.Buffer(make([]byte, 1024), MaxFrameBytes+1)
		for scanner.Scan() {
			lines <- append([]byte(nil), scanner.Bytes()...)
		}
		close(lines)
		readErr <- scanner.Err()
	}()

	first := nextProtocolLine(t, lines)
	var hello struct {
		Event   string       `json:"event"`
		Payload helloPayload `json:"payload"`
	}
	if err := json.Unmarshal(first, &hello); err != nil {
		t.Fatal(err)
	}
	if hello.Event != "hello" || hello.Payload.ProtocolMin != 1 || hello.Payload.ProtocolMax != 1 || hello.Payload.HelperBuild != "test-helper" || hello.Payload.BundleBuildID != "test-bundle" || hello.Payload.PID != 4242 || hello.Payload.HelperEpoch != "hep_test" {
		t.Fatalf("hello = %+v", hello)
	}

	writeProtocolRequest(t, stdinWriter, `{"v":1,"id":"req_start","command":"start","payload":{}}`)
	var operationID, addr string
	terminal := false
	deadline := time.After(5 * time.Second)
	for !terminal || addr == "" || operationID == "" {
		select {
		case line := <-lines:
			if line == nil {
				t.Fatal("stdout closed before startup completed")
			}
			var envelope map[string]json.RawMessage
			if err := json.Unmarshal(line, &envelope); err != nil {
				t.Fatalf("non-protocol stdout %q: %v", line, err)
			}
			var id, event string
			_ = json.Unmarshal(envelope["id"], &id)
			_ = json.Unmarshal(envelope["event"], &event)
			if id == "req_start" {
				var response struct {
					OK     bool `json:"ok"`
					Result struct {
						OperationID string `json:"operation_id"`
					} `json:"result"`
				}
				if err := json.Unmarshal(line, &response); err != nil {
					t.Fatal(err)
				}
				if !response.OK {
					t.Fatalf("start response = %s", line)
				}
				operationID = response.Result.OperationID
			}
			if event == "operation" {
				var operation struct {
					Payload operationPayload `json:"payload"`
				}
				_ = json.Unmarshal(line, &operation)
				if operation.Payload.OperationID == operationID && operation.Payload.Status == "succeeded" {
					terminal = true
				}
			}
			if event == "state" {
				var state struct {
					Payload runtimeStatePayload `json:"payload"`
				}
				_ = json.Unmarshal(line, &state)
				if state.Payload.Service == appcontrol.ServiceRunning {
					addr = state.Payload.Addr
				}
			}
		case <-deadline:
			t.Fatalf("timed out waiting for start (operation=%q terminal=%v addr=%q)", operationID, terminal, addr)
		}
	}

	resp, err := http.Get("http://" + addr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d", resp.StatusCode)
	}

	// Same request ID and logical payload must replay the original acceptance
	// without allocating another runtime generation.
	writeProtocolRequest(t, stdinWriter, `{"v":1,"id":"req_start","command":"start","payload":{}}`)
	for {
		line := nextProtocolLine(t, lines)
		var response struct {
			ID     string `json:"id"`
			OK     bool   `json:"ok"`
			Result struct {
				OperationID string `json:"operation_id"`
			} `json:"result"`
		}
		_ = json.Unmarshal(line, &response)
		if response.ID == "req_start" {
			if !response.OK || response.Result.OperationID != operationID {
				t.Fatalf("idempotent response = %s", line)
			}
			break
		}
	}
	writeProtocolRequest(t, stdinWriter, `{"v":1,"id":"req_start","command":"stop","payload":{}}`)
	for {
		line := nextProtocolLine(t, lines)
		var response struct {
			ID    string         `json:"id"`
			OK    bool           `json:"ok"`
			Error *ProtocolError `json:"error"`
		}
		_ = json.Unmarshal(line, &response)
		if response.ID == "req_start" {
			if response.OK || response.Error == nil || response.Error.Code != "request_id_conflict" {
				t.Fatalf("reuse conflict response = %s", line)
			}
			break
		}
	}

	start := time.Now()
	if err := stdinWriter.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunHelper() error = %v", err)
		}
	case <-time.After(7 * time.Second):
		t.Fatal("stdin EOF did not stop helper within seven seconds")
	}
	if elapsed := time.Since(start); elapsed > 7*time.Second {
		t.Fatalf("EOF shutdown took %s", elapsed)
	}
	_ = stdoutWriter.Close()
	_ = stdoutReader.Close()
	if err := <-readErr; err != nil && !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("stdout reader error = %v", err)
	}
	if _, err := http.Get("http://" + addr + "/healthz"); err == nil {
		t.Fatal("listener remained reachable after stdin EOF")
	}
}

func TestHelperMalformedDuplicateEnvelopeTerminatesWithoutResponse(t *testing.T) {
	manager := newManagerForTest(t)
	runtime := newTestRuntimeForHelper()
	controller, err := appcontrol.New(appcontrol.Options{
		ConfigurationSource: manager,
		RuntimeFactory:      runtimeFactoryFunc(func(context.Context, appcontrol.Configuration) (appcontrol.Runtime, error) { return runtime, nil }),
	})
	if err != nil {
		t.Fatal(err)
	}
	input := strings.NewReader("{\"v\":1,\"id\":\"one\",\"id\":\"two\",\"command\":\"get_state\"}\n")
	var output strings.Builder
	err = RunHelper(t.Context(), HelperOptions{Stdin: input, Stdout: &output, Stderr: io.Discard, Controller: controller, Configuration: manager, HelperEpoch: "hep"})
	if !errors.Is(err, ErrDuplicateField) {
		t.Fatalf("RunHelper() error = %v", err)
	}
	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) == 0 {
		t.Fatal("hello was not emitted")
	}
	var first map[string]any
	if json.Unmarshal([]byte(lines[0]), &first) != nil || first["event"] != "hello" {
		t.Fatalf("first frame = %q", lines[0])
	}
	for _, line := range lines {
		if strings.Contains(line, `"id":"one"`) {
			t.Fatalf("malformed request received a response: %s", line)
		}
	}
}

type runtimeFactoryFunc func(context.Context, appcontrol.Configuration) (appcontrol.Runtime, error)

func (f runtimeFactoryFunc) NewRuntime(ctx context.Context, cfg appcontrol.Configuration) (appcontrol.Runtime, error) {
	return f(ctx, cfg)
}

type helperTestRuntime struct{ done chan error }

func newTestRuntimeForHelper() *helperTestRuntime {
	return &helperTestRuntime{done: make(chan error, 1)}
}
func (r *helperTestRuntime) Start(context.Context) error { return nil }
func (r *helperTestRuntime) Stop(context.Context) error {
	select {
	case <-r.done:
	default:
		close(r.done)
	}
	return nil
}
func (r *helperTestRuntime) Done() <-chan error                                  { return r.done }
func (r *helperTestRuntime) Addr() string                                        { return "127.0.0.1:0" }
func (r *helperTestRuntime) UsesCopilot() bool                                   { return false }
func (r *helperTestRuntime) SetStartupAuthenticationPending(bool)                {}
func (r *helperTestRuntime) ValidateDynamicProviderModels(context.Context) error { return nil }
func (r *helperTestRuntime) InitializePolicyRouting(context.Context) error       { return nil }

func writeProtocolRequest(t *testing.T, writer io.Writer, raw string) {
	t.Helper()
	raw = strings.ReplaceAll(raw, `\"`, `"`)
	if _, err := fmt.Fprintln(writer, raw); err != nil {
		t.Fatal(err)
	}
}

func nextProtocolLine(t *testing.T, lines <-chan []byte) []byte {
	t.Helper()
	select {
	case line := <-lines:
		if line == nil {
			t.Fatal("protocol output closed")
		}
		return line
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for protocol frame")
		return nil
	}
}

func TestHelperSecretProjectionIsWriteOnlyAndNeverObservable(t *testing.T) {
	const sentinel = "SECRET-SENTINEL-OBSERVABLE-91b7"
	manager := newManagerForTest(t)
	controller, err := appcontrol.New(appcontrol.Options{
		ConfigurationSource: manager,
		RuntimeFactory: runtimeFactoryFunc(func(context.Context, appcontrol.Configuration) (appcontrol.Runtime, error) {
			return newTestRuntimeForHelper(), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	input := strings.Join([]string{
		`{"v":1,"id":"req_secret","command":"set_secret_projection","payload":{"config_revision":"cfg_test","secret_generation":1,"secrets":[{"provider_id":"local","reference":"VEKIL_MANAGED_LOCAL_API_KEY_1","value":"` + sentinel + `"}]}}`,
		`{"v":1,"id":"req_state","command":"get_state","payload":{}}`,
		`{"v":1,"id":"req_shutdown","command":"shutdown","payload":{}}`,
	}, "\n") + "\n"
	var stdout strings.Builder
	var stderr strings.Builder
	store := NewSecretProjectionStore()
	if err := RunHelper(t.Context(), HelperOptions{
		Stdin: strings.NewReader(input), Stdout: &stdout, Stderr: &stderr,
		Controller: controller, Configuration: manager, Secrets: store, HelperEpoch: "hep_secret",
	}); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(stdout.String(), sentinel) || strings.Contains(stderr.String(), sentinel) {
		t.Fatalf("secret appeared in helper output\nstdout=%s\nstderr=%s", stdout.String(), stderr.String())
	}
	if !store.Has("cfg_test", 1) {
		t.Fatal("secret projection was not installed")
	}
}

func TestHelperParentLossStopsSession(t *testing.T) {
	manager := newManagerForTest(t)
	controller, err := appcontrol.New(appcontrol.Options{
		ConfigurationSource: manager,
		RuntimeFactory: runtimeFactoryFunc(func(context.Context, appcontrol.Configuration) (appcontrol.Runtime, error) {
			return newTestRuntimeForHelper(), nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	stdinReader, stdinWriter := io.Pipe()
	defer func() { _ = stdinWriter.Close() }()
	var output strings.Builder
	done := make(chan error, 1)
	go func() {
		done <- RunHelper(t.Context(), HelperOptions{
			Stdin: stdinReader, Stdout: &output, Stderr: io.Discard,
			Controller: controller, Configuration: manager, ParentPID: 1 << 30, ParentPoll: time.Millisecond,
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunHelper() error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("helper did not exit after parent loss")
	}
	first := strings.Split(strings.TrimSpace(output.String()), "\n")[0]
	var envelope map[string]any
	if json.Unmarshal([]byte(first), &envelope) != nil || envelope["event"] != "hello" {
		t.Fatalf("first frame = %q", first)
	}
}
