package macosruntime

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/internal/appcontrol"
)

func writeExternalConfigForSwitchTest(t *testing.T, model string) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "providers.yaml")
	body := []byte("schema_version: 2\nproviders:\n  - id: local\n    type: openai-compatible\n    default: true\n    base_url: https://example.test/v1\n    auth_type: none\n    model_discovery: static\n    models:\n      - public_id: " + model + "\n        endpoints: [/chat/completions]\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	revision, _ := configRevision(body)
	return path, revision
}

func newConfigSwitchHarness(t *testing.T, factory appcontrol.RuntimeFactory) (*ConfigManager, *appcontrol.Controller, *helper) {
	t.Helper()
	manager := newManagerForTest(t)
	controller, err := appcontrol.New(appcontrol.Options{
		ConfigurationSource: manager, ConfigurationObserver: manager, RuntimeFactory: factory,
		ReadinessChecker: appcontrol.ReadinessCheckFunc(func(context.Context, string) error { return nil }),
		StopTimeout:      time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	return manager, controller, &helper{opts: HelperOptions{
		Controller: controller, Configuration: manager, ShutdownTimeout: time.Second,
	}}
}

func startConfigSwitchHarness(t *testing.T, controller *appcontrol.Controller, revision string) {
	t.Helper()
	operation, err := controller.Start(t.Context(), revision)
	if err != nil {
		t.Fatal(err)
	}
	result, err := operation.Wait(t.Context())
	if err != nil || result.Status != appcontrol.OperationSucceeded {
		t.Fatalf("start result=%+v err=%v", result, err)
	}
}

func TestConfigSelectionWhileRunningRestartsWithSelectedRevision(t *testing.T) {
	factory := &revisionRuntimeFactory{newRuntime: func(string, int) *applyRuntime { return newApplyRuntime(nil) }}
	manager, controller, h := newConfigSwitchHarness(t, factory)
	startConfigSwitchHarness(t, controller, LegacyConfigRevision)
	path, revision := writeExternalConfigForSwitchTest(t, "selected-model")

	if err := h.switchSelectedConfiguration(t.Context(), func(ctx context.Context) error {
		_, err := manager.SelectExternal(ctx, path)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	state := controller.Snapshot()
	if state.Service != appcontrol.ServiceRunning || state.RuntimeGeneration != 2 || state.ConfigRevision != revision {
		t.Fatalf("runtime state = %+v", state)
	}
	selection := manager.State()
	if selection.ConfigMode != ConfigModeExternal || selection.SelectedPath != path || selection.SelectedConfigRevision != revision {
		t.Fatalf("selection = %+v", selection)
	}
}

func TestConfigSelectionFailureRestoresPriorSelectionAndRuntime(t *testing.T) {
	path, failedRevision := writeExternalConfigForSwitchTest(t, "failing-model")
	factory := &revisionRuntimeFactory{newRuntime: func(revision string, _ int) *applyRuntime {
		if revision == failedRevision {
			return newApplyRuntime(errors.New("candidate start failed"))
		}
		return newApplyRuntime(nil)
	}}
	manager, controller, h := newConfigSwitchHarness(t, factory)
	startConfigSwitchHarness(t, controller, LegacyConfigRevision)

	err := h.switchSelectedConfiguration(t.Context(), func(ctx context.Context) error {
		_, err := manager.SelectExternal(ctx, path)
		return err
	})
	var switchErr *ConfigSwitchError
	if !errors.As(err, &switchErr) || switchErr.Rollback != nil {
		t.Fatalf("switch error = %v", err)
	}
	state := controller.Snapshot()
	if state.Service != appcontrol.ServiceRunning || state.ConfigRevision != LegacyConfigRevision {
		t.Fatalf("restored runtime = %+v", state)
	}
	selection := manager.State()
	if selection.ConfigMode != ConfigModeLegacy || selection.SelectedPath != "" || selection.SelectedConfigRevision != LegacyConfigRevision {
		t.Fatalf("restored selection = %+v", selection)
	}
}

func TestConfigSelectionStopFailureRestoresPriorSelectionAndRuntime(t *testing.T) {
	stopErr := errors.New("candidate switch stop failed")
	factory := &revisionRuntimeFactory{newRuntime: func(revision string, call int) *applyRuntime {
		runtime := newApplyRuntime(nil)
		if revision == LegacyConfigRevision && call == 1 {
			runtime.stop = func(context.Context) error { return stopErr }
		}
		return runtime
	}}
	manager, controller, h := newConfigSwitchHarness(t, factory)
	startConfigSwitchHarness(t, controller, LegacyConfigRevision)
	path, _ := writeExternalConfigForSwitchTest(t, "selected-model")

	err := h.switchSelectedConfiguration(t.Context(), func(ctx context.Context) error {
		_, selectErr := manager.SelectExternal(ctx, path)
		return selectErr
	})
	var switchErr *ConfigSwitchError
	if !errors.As(err, &switchErr) || !errors.Is(switchErr.Primary, stopErr) || switchErr.Rollback != nil {
		t.Fatalf("switch error = %+v, want stop failure with successful restore", err)
	}
	state := controller.Snapshot()
	if state.Service != appcontrol.ServiceRunning || state.RuntimeGeneration != 2 || state.ConfigRevision != LegacyConfigRevision {
		t.Fatalf("restored runtime = %+v", state)
	}
	selection := manager.State()
	if selection.ConfigMode != ConfigModeLegacy || selection.SelectedPath != "" || selection.SelectedConfigRevision != LegacyConfigRevision {
		t.Fatalf("restored selection = %+v", selection)
	}
}

func TestInvalidExternalSelectionDoesNotStopRunningRuntime(t *testing.T) {
	factory := &revisionRuntimeFactory{newRuntime: func(string, int) *applyRuntime { return newApplyRuntime(nil) }}
	manager, controller, h := newConfigSwitchHarness(t, factory)
	startConfigSwitchHarness(t, controller, LegacyConfigRevision)

	err := h.switchSelectedConfiguration(t.Context(), func(ctx context.Context) error {
		_, err := manager.SelectExternal(ctx, filepath.Join(t.TempDir(), "missing.yaml"))
		return err
	})
	if err == nil {
		t.Fatal("selection succeeded for a missing file")
	}
	state := controller.Snapshot()
	if state.Service != appcontrol.ServiceRunning || state.RuntimeGeneration != 1 || state.ConfigRevision != LegacyConfigRevision {
		t.Fatalf("runtime changed after validation failure: %+v", state)
	}
	if selection := manager.State(); selection.ConfigMode != ConfigModeLegacy || selection.SelectedPath != "" {
		t.Fatalf("selection changed after validation failure: %+v", selection)
	}
}

func TestCanceledConfigSelectionPreservesRestoreFailure(t *testing.T) {
	factory := &revisionRuntimeFactory{newRuntime: func(string, int) *applyRuntime { return newApplyRuntime(nil) }}
	manager, controller, h := newConfigSwitchHarness(t, factory)
	startConfigSwitchHarness(t, controller, LegacyConfigRevision)
	path, _ := writeExternalConfigForSwitchTest(t, "selected-model")
	ctx, cancel := context.WithCancel(t.Context())

	err := h.switchSelectedConfiguration(ctx, func(opCtx context.Context) error {
		if err := manager.StageExternal(opCtx, path); err != nil {
			return err
		}
		if err := os.RemoveAll(manager.paths.Directory); err != nil {
			return err
		}
		if err := os.WriteFile(manager.paths.Directory, []byte("blocks state restore"), 0o600); err != nil {
			return err
		}
		cancel()
		return nil
	})
	var switchErr *ConfigSwitchError
	if !errors.As(err, &switchErr) {
		t.Fatalf("switch error = %v, want ConfigSwitchError", err)
	}
	if !errors.Is(switchErr.Primary, context.Canceled) || switchErr.Rollback == nil {
		t.Fatalf("switch error = %+v, want canceled primary and restore failure", switchErr)
	}
	if !hasRollbackFailure(err) {
		t.Fatalf("hasRollbackFailure(%v) = false", err)
	}
}

func TestCanceledStoppedConfigSelectionRestoresPriorSelection(t *testing.T) {
	factory := &revisionRuntimeFactory{newRuntime: func(string, int) *applyRuntime { return newApplyRuntime(nil) }}
	manager, _, h := newConfigSwitchHarness(t, factory)
	path, _ := writeExternalConfigForSwitchTest(t, "selected-model")
	ctx, cancel := context.WithCancel(t.Context())

	err := h.switchSelectedConfiguration(ctx, func(opCtx context.Context) error {
		if err := manager.StageExternal(opCtx, path); err != nil {
			return err
		}
		cancel()
		return nil
	})
	var switchErr *ConfigSwitchError
	if !errors.As(err, &switchErr) || !errors.Is(switchErr.Primary, context.Canceled) || switchErr.Rollback != nil {
		t.Fatalf("switch error = %+v, want canceled primary with successful restore", err)
	}
	selection := manager.State()
	if selection.ConfigMode != ConfigModeLegacy || selection.SelectedPath != "" || selection.SelectedConfigRevision != LegacyConfigRevision {
		t.Fatalf("selection after cancellation = %+v", selection)
	}
}

func TestCanceledRunningConfigSelectionAfterCandidateReadyRestoresPriorRuntime(t *testing.T) {
	factory := &revisionRuntimeFactory{newRuntime: func(string, int) *applyRuntime { return newApplyRuntime(nil) }}
	manager, controller, h := newConfigSwitchHarness(t, factory)
	startConfigSwitchHarness(t, controller, LegacyConfigRevision)
	path, _ := writeExternalConfigForSwitchTest(t, "selected-model")
	ctx, cancel := context.WithCancel(t.Context())
	h.beforeSelectionCommit = cancel

	err := h.switchSelectedConfiguration(ctx, func(opCtx context.Context) error {
		_, selectErr := manager.SelectExternal(opCtx, path)
		return selectErr
	})
	var switchErr *ConfigSwitchError
	if !errors.As(err, &switchErr) || !errors.Is(switchErr.Primary, context.Canceled) || switchErr.Rollback != nil {
		t.Fatalf("switch error = %+v, want canceled primary with successful restore", err)
	}
	state := controller.Snapshot()
	if state.Service != appcontrol.ServiceRunning || state.ConfigRevision != LegacyConfigRevision || state.RuntimeGeneration != 3 {
		t.Fatalf("restored runtime = %+v", state)
	}
	selection := manager.State()
	if selection.ConfigMode != ConfigModeLegacy || selection.SelectedPath != "" || selection.SelectedConfigRevision != LegacyConfigRevision {
		t.Fatalf("selection after cancellation = %+v", selection)
	}
}

func TestConfigSelectionWaitsForNonCancelableStopBeforeRestoringSelection(t *testing.T) {
	stopStarted := make(chan struct{})
	releaseStop := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseStop)
		}
	}()

	factory := &revisionRuntimeFactory{newRuntime: func(revision string, call int) *applyRuntime {
		runtime := newApplyRuntime(nil)
		if revision == LegacyConfigRevision && call == 1 {
			runtime.stop = func(ctx context.Context) error {
				close(stopStarted)
				select {
				case <-releaseStop:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		return runtime
	}}
	manager, controller, h := newConfigSwitchHarness(t, factory)
	h.opts.ShutdownTimeout = 250 * time.Millisecond
	startConfigSwitchHarness(t, controller, LegacyConfigRevision)
	path, revision := writeExternalConfigForSwitchTest(t, "slow-stop-model")

	done := make(chan error, 1)
	go func() {
		done <- h.switchSelectedConfiguration(t.Context(), func(ctx context.Context) error {
			_, err := manager.SelectExternal(ctx, path)
			return err
		})
	}()

	select {
	case <-stopStarted:
	case <-time.After(time.Second):
		t.Fatal("stop operation did not start")
	}
	select {
	case err := <-done:
		t.Fatalf("configuration switch returned while stop cleanup was active: %v", err)
	case <-time.After(h.opts.ShutdownTimeout + 50*time.Millisecond):
	}
	selection := manager.State()
	if selection.ConfigMode != ConfigModeExternal || selection.SelectedPath != path || selection.SelectedConfigRevision != revision {
		t.Fatalf("selection was restored while stop cleanup was active: %+v", selection)
	}
	state := controller.Snapshot()
	if state.Service != appcontrol.ServiceStopping || state.Operation == nil || state.Operation.Kind != appcontrol.OperationStop {
		t.Fatalf("controller stop was not still active: %+v", state)
	}

	close(releaseStop)
	released = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("configuration switch did not finish after stop completed")
	}
	state = controller.Snapshot()
	if state.Service != appcontrol.ServiceRunning || state.ConfigRevision != revision || state.Operation != nil {
		t.Fatalf("selected runtime = %+v", state)
	}
}

type copilotSwitchRuntime struct {
	*applyRuntime
	usesCopilot bool
}

func (r *copilotSwitchRuntime) UsesCopilot() bool { return r.usesCopilot }

type signOutAuthenticator struct {
	status       auth.AuthStatus
	tokenErr     error
	signOutErr   error
	signOutCalls int
}

func (a *signOutAuthenticator) GetToken(context.Context) (string, error) {
	return "token", a.tokenErr
}
func (a *signOutAuthenticator) Status() auth.AuthStatus { return a.status }
func (a *signOutAuthenticator) RequestDeviceCode(context.Context) (*auth.DeviceCodeResponse, error) {
	return nil, errors.New("unused")
}
func (a *signOutAuthenticator) PollForAuthorization(context.Context, *auth.DeviceCodeResponse) error {
	return errors.New("unused")
}
func (a *signOutAuthenticator) SignInWithGitHubCLI(context.Context) error {
	return errors.New("unused")
}
func (a *signOutAuthenticator) SignOut() error {
	a.signOutCalls++
	a.status = auth.AuthStatus{}
	return a.signOutErr
}

func TestSignOutStopsOnlyCopilotRuntime(t *testing.T) {
	for _, test := range []struct {
		name        string
		usesCopilot bool
		wantService appcontrol.ServiceState
	}{
		{name: "Copilot", usesCopilot: true, wantService: appcontrol.ServiceStopped},
		{name: "provider only", usesCopilot: false, wantService: appcontrol.ServiceRunning},
	} {
		t.Run(test.name, func(t *testing.T) {
			authenticator := &signOutAuthenticator{status: auth.AuthStatus{SignedIn: true, Source: auth.AuthSourceVekil}}
			manager := newManagerForTest(t)
			controller, err := appcontrol.New(appcontrol.Options{
				ConfigurationSource: manager,
				RuntimeFactory: runtimeFactoryFunc(func(context.Context, appcontrol.Configuration) (appcontrol.Runtime, error) {
					return &copilotSwitchRuntime{applyRuntime: newApplyRuntime(nil), usesCopilot: test.usesCopilot}, nil
				}),
				Authenticator:    authenticator,
				ReadinessChecker: appcontrol.ReadinessCheckFunc(func(context.Context, string) error { return nil }),
				StopTimeout:      time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			startConfigSwitchHarness(t, controller, LegacyConfigRevision)
			h := &helper{opts: HelperOptions{Controller: controller, Configuration: manager, Authenticator: authenticator, ShutdownTimeout: time.Second}}
			if err := h.signOut(t.Context()); err != nil {
				t.Fatal(err)
			}
			if got := controller.Snapshot().Service; got != test.wantService {
				t.Fatalf("service = %q, want %q", got, test.wantService)
			}
			if authenticator.signOutCalls != 1 {
				t.Fatalf("SignOut calls = %d", authenticator.signOutCalls)
			}
		})
	}
}

func TestSignOutClearsCredentialsAfterTerminalStopFailure(t *testing.T) {
	stopErr := errors.New("runtime cleanup failed")
	signOutErr := errors.New("credential cleanup failed")
	authenticator := &signOutAuthenticator{
		status:     auth.AuthStatus{SignedIn: true, Source: auth.AuthSourceVekil},
		signOutErr: signOutErr,
	}
	manager := newManagerForTest(t)
	controller, err := appcontrol.New(appcontrol.Options{
		ConfigurationSource: manager,
		RuntimeFactory: runtimeFactoryFunc(func(context.Context, appcontrol.Configuration) (appcontrol.Runtime, error) {
			runtime := newApplyRuntime(nil)
			runtime.stop = func(context.Context) error { return stopErr }
			return &copilotSwitchRuntime{applyRuntime: runtime, usesCopilot: true}, nil
		}),
		Authenticator:    authenticator,
		ReadinessChecker: appcontrol.ReadinessCheckFunc(func(context.Context, string) error { return nil }),
		StopTimeout:      time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	startConfigSwitchHarness(t, controller, LegacyConfigRevision)
	h := &helper{opts: HelperOptions{
		Controller: controller, Configuration: manager,
		Authenticator: authenticator, ShutdownTimeout: time.Second,
	}}

	err = h.signOut(t.Context())
	if !errors.Is(err, stopErr) || !errors.Is(err, signOutErr) {
		t.Fatalf("signOut() error = %v, want joined stop and credential failures", err)
	}
	if authenticator.signOutCalls != 1 || authenticator.Status().SignedIn {
		t.Fatalf("credentials were not cleared after terminal stop failure: %+v", authenticator)
	}
	if _, stopAgainErr := controller.Stop(t.Context()); !errors.Is(stopAgainErr, appcontrol.ErrNotRunning) {
		t.Fatalf("runtime remained owned after terminal stop failure: %v", stopAgainErr)
	}
}

type deviceAuthStub struct{ signOutAuthenticator }

func (a *deviceAuthStub) RequestDeviceCode(context.Context) (*auth.DeviceCodeResponse, error) {
	return &auth.DeviceCodeResponse{VerificationURI: "https://github.com/login/device", UserCode: "ABCD-EFGH", ExpiresIn: 900}, nil
}
func (a *deviceAuthStub) PollForAuthorization(context.Context, *auth.DeviceCodeResponse) error {
	return nil
}

func TestDeviceCodeEventCarriesMonotonicStateRevision(t *testing.T) {
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
	var output strings.Builder
	writer := newProtocolWriter(&output)
	h := &helper{
		epoch:  "hep_device",
		writer: writer,
		opts: HelperOptions{
			Controller: controller, Configuration: manager,
			Authenticator: &deviceAuthStub{}, ShutdownTimeout: time.Second,
		},
	}
	operation := &coordinatedOperation{id: "op_device", kind: "auth_device_start", ctx: t.Context(), done: make(chan struct{})}
	h.runDeviceAuth(operation)
	closeCtx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	if err := writer.Close(closeCtx); err != nil {
		t.Fatal(err)
	}

	found := false
	var deviceRevision, followingStateRevision uint64
	for _, line := range strings.Split(strings.TrimSpace(output.String()), "\n") {
		var envelope struct {
			Event         string            `json:"event"`
			StateRevision uint64            `json:"state_revision"`
			Payload       deviceCodePayload `json:"payload"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err != nil {
			t.Fatal(err)
		}
		if envelope.Event == "device_code" {
			found = true
			deviceRevision = envelope.StateRevision
			if envelope.StateRevision == 0 || envelope.Payload.OperationID != operation.id {
				t.Fatalf("device code event = %+v", envelope)
			}
		}
		if envelope.Event == "state" && deviceRevision != 0 && envelope.StateRevision > deviceRevision && followingStateRevision == 0 {
			followingStateRevision = envelope.StateRevision
		}
	}
	if !found {
		t.Fatal("device_code event not emitted")
	}
	if followingStateRevision <= deviceRevision {
		t.Fatalf("paired state revision = %d, device revision = %d", followingStateRevision, deviceRevision)
	}
}

func TestProtocolAuthSourceUsesStableWireTokens(t *testing.T) {
	cases := map[auth.AuthSource]string{
		auth.AuthSourceNone:      "none",
		auth.AuthSourceEnv:       "environment",
		auth.AuthSourceVekil:     "vekil",
		auth.AuthSourceGitHubCLI: "github_cli",
	}
	for source, want := range cases {
		if got := protocolAuthSource(source); got != want {
			t.Fatalf("protocolAuthSource(%q) = %q, want %q", source, got, want)
		}
	}
}

func TestStateProjectionPreservesAuthenticationFailureWithStoredCredential(t *testing.T) {
	authenticator := &signOutAuthenticator{
		status:   auth.AuthStatus{SignedIn: true, Source: auth.AuthSourceVekil},
		tokenErr: errors.New("invalid credential"),
	}
	manager := newManagerForTest(t)
	controller, err := appcontrol.New(appcontrol.Options{
		ConfigurationSource: manager,
		RuntimeFactory: runtimeFactoryFunc(func(context.Context, appcontrol.Configuration) (appcontrol.Runtime, error) {
			return &copilotSwitchRuntime{applyRuntime: newApplyRuntime(nil), usesCopilot: true}, nil
		}),
		Authenticator: authenticator,
		StopTimeout:   time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := controller.Start(t.Context(), LegacyConfigRevision)
	if err != nil {
		t.Fatal(err)
	}
	result, err := operation.Wait(t.Context())
	if err != nil || result.Status != appcontrol.OperationFailed {
		t.Fatalf("start result=%+v err=%v", result, err)
	}
	h := &helper{opts: HelperOptions{Controller: controller, Configuration: manager, Authenticator: authenticator}}
	payload := h.buildStatePayload()
	if payload.Auth != appcontrol.AuthFailed || payload.AuthSource != "vekil" {
		t.Fatalf("auth projection = state %q source %q", payload.Auth, payload.AuthSource)
	}
}
