package macosruntime

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/sozercan/vekil/internal/appcontrol"
	"github.com/sozercan/vekil/proxy"
)

type candidateValidatorFunc func(context.Context, ManagedCandidate) error

func (f candidateValidatorFunc) ValidateManagedCandidate(ctx context.Context, candidate ManagedCandidate) error {
	return f(ctx, candidate)
}

type applyRuntime struct {
	startErr       error
	done           chan error
	stopOnce       sync.Once
	stopErr        error
	onStop         func()
	stop           func(context.Context) error
	validateModels func(context.Context) error
}

func newApplyRuntime(startErr error) *applyRuntime {
	return &applyRuntime{startErr: startErr, done: make(chan error, 1)}
}
func (r *applyRuntime) Start(context.Context) error { return r.startErr }
func (r *applyRuntime) Stop(ctx context.Context) error {
	r.stopOnce.Do(func() {
		if r.stop != nil {
			r.stopErr = r.stop(ctx)
		}
		if r.onStop != nil {
			r.onStop()
		}
		close(r.done)
	})
	return r.stopErr
}
func (r *applyRuntime) Done() <-chan error                   { return r.done }
func (r *applyRuntime) Addr() string                         { return "127.0.0.1:1234" }
func (r *applyRuntime) UsesCopilot() bool                    { return false }
func (r *applyRuntime) SetStartupAuthenticationPending(bool) {}
func (r *applyRuntime) ValidateDynamicProviderModels(ctx context.Context) error {
	if r.validateModels != nil {
		return r.validateModels(ctx)
	}
	return nil
}
func (r *applyRuntime) InitializePolicyRouting(context.Context) error { return nil }

type revisionRuntimeFactory struct {
	mu         sync.Mutex
	calls      map[string]int
	newRuntime func(string, int) *applyRuntime
}

func (f *revisionRuntimeFactory) NewRuntime(_ context.Context, cfg appcontrol.Configuration) (appcontrol.Runtime, error) {
	f.mu.Lock()
	if f.calls == nil {
		f.calls = map[string]int{}
	}
	f.calls[cfg.Revision]++
	call := f.calls[cfg.Revision]
	f.mu.Unlock()
	return f.newRuntime(cfg.Revision, call), nil
}

func managedApplyDraft(copilotUUID string, includeLocal bool) ManagedDraft {
	providers := []ManagedProviderDraft{{UUID: copilotUUID, Config: proxy.ProviderConfig{ID: "copilot", Type: "copilot", Default: true}}}
	if includeLocal {
		providers = append(providers, ManagedProviderDraft{Config: proxy.ProviderConfig{
			ID: "local", Type: "openai-compatible", BaseURL: "https://example.test/v1", AuthType: "none", ModelDiscovery: "static",
			Models: []proxy.ProviderModelConfig{{PublicID: "local-model", Endpoints: []string{"/chat/completions"}}},
		}})
	}
	return ManagedDraft{Providers: providers}
}

func newApplyHarness(t *testing.T, runtimeFactory appcontrol.RuntimeFactory) (*ConfigManager, *appcontrol.Controller, *helper, string) {
	t.Helper()
	manager := newManagerForTest(t, "owner", "copilot-uuid", "local-uuid")
	description, err := manager.EnsureManagedConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	controller, err := appcontrol.New(appcontrol.Options{
		ConfigurationSource: manager, ConfigurationObserver: manager, RuntimeFactory: runtimeFactory,
		ReadinessChecker: appcontrol.ReadinessCheckFunc(func(context.Context, string) error { return nil }), StopTimeout: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	h := &helper{opts: HelperOptions{
		Controller: controller, Configuration: manager, Secrets: NewSecretProjectionStore(),
		CandidateValidator: candidateValidatorFunc(func(context.Context, ManagedCandidate) error { return nil }), ShutdownTimeout: time.Second,
	}}
	return manager, controller, h, description.SelectedRevision
}

func TestApplyManagedDraftWhileStoppedStaysStopped(t *testing.T) {
	factory := &revisionRuntimeFactory{newRuntime: func(string, int) *applyRuntime { return newApplyRuntime(nil) }}
	manager, controller, h, initialRevision := newApplyHarness(t, factory)
	payload := managedDraftPayload{ExpectedConfigRevision: initialRevision, SecretGeneration: 1, Draft: managedApplyDraft(manager.State().Providers[0].UUID, true)}
	if err := h.applyManagedDraft(t.Context(), "op_stopped", payload); err != nil {
		t.Fatal(err)
	}
	if got := controller.Snapshot().Service; got != appcontrol.ServiceStopped {
		t.Fatalf("service = %q", got)
	}
	if got := manager.State().CommittedConfigRevision; got == initialRevision {
		t.Fatal("managed revision did not advance")
	}
}

func TestValidateManagedDraftDeletesValidationOnlyProjection(t *testing.T) {
	factory := &revisionRuntimeFactory{newRuntime: func(string, int) *applyRuntime { return newApplyRuntime(nil) }}
	manager, _, h, initialRevision := newApplyHarness(t, factory)
	payload := managedDraftPayload{
		ExpectedConfigRevision: initialRevision,
		SecretGeneration:       1,
		Draft:                  managedApplyDraft(manager.State().Providers[0].UUID, true),
	}
	candidate, err := manager.BuildManagedCandidate(payload.Draft, payload.SecretGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.opts.Secrets.Set(SecretProjection{
		ConfigRevision:   candidate.Revision,
		SecretGeneration: candidate.SecretGeneration,
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.validateManagedDraft(t.Context(), payload); err != nil {
		t.Fatal(err)
	}
	if h.opts.Secrets.Has(candidate.Revision, candidate.SecretGeneration) {
		t.Fatal("validation-only secret projection was retained")
	}
}

func TestValidateManagedDraftRetainsRequiredProjection(t *testing.T) {
	factory := &revisionRuntimeFactory{newRuntime: func(string, int) *applyRuntime { return newApplyRuntime(nil) }}
	manager, _, h, initialRevision := newApplyHarness(t, factory)
	manager.mu.Lock()
	manager.state.SecretGeneration = 1
	manager.mu.Unlock()
	payload := managedDraftPayload{
		ExpectedConfigRevision: initialRevision,
		SecretGeneration:       1,
		Draft:                  managedApplyDraft(manager.State().Providers[0].UUID, false),
	}
	candidate, err := manager.BuildManagedCandidate(payload.Draft, payload.SecretGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Revision != initialRevision {
		t.Fatalf("candidate revision = %q, want %q", candidate.Revision, initialRevision)
	}
	if err := h.opts.Secrets.Set(SecretProjection{
		ConfigRevision:   candidate.Revision,
		SecretGeneration: candidate.SecretGeneration,
	}); err != nil {
		t.Fatal(err)
	}

	if err := h.validateManagedDraft(t.Context(), payload); err != nil {
		t.Fatal(err)
	}
	if !h.opts.Secrets.Has(candidate.Revision, candidate.SecretGeneration) {
		t.Fatal("required secret projection was deleted")
	}
}

func TestApplyManagedDraftDeletesProjectionRejectedBeforeTransaction(t *testing.T) {
	factory := &revisionRuntimeFactory{newRuntime: func(string, int) *applyRuntime { return newApplyRuntime(nil) }}
	manager, _, h, initialRevision := newApplyHarness(t, factory)
	payload := managedDraftPayload{
		ExpectedConfigRevision: initialRevision,
		SecretGeneration:       1,
		Draft:                  managedApplyDraft(manager.State().Providers[0].UUID, true),
	}
	candidate, err := manager.BuildManagedCandidate(payload.Draft, payload.SecretGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.opts.Secrets.Set(SecretProjection{
		ConfigRevision:   candidate.Revision,
		SecretGeneration: candidate.SecretGeneration,
	}); err != nil {
		t.Fatal(err)
	}
	h.opts.CandidateValidator = candidateValidatorFunc(func(context.Context, ManagedCandidate) error {
		return errors.New("candidate rejected")
	})

	if err := h.applyManagedDraft(t.Context(), "op_rejected", payload); err == nil {
		t.Fatal("apply unexpectedly succeeded")
	}
	if h.opts.Secrets.Has(candidate.Revision, candidate.SecretGeneration) {
		t.Fatal("rejected candidate secret projection was retained")
	}
}

func TestApplyManagedDraftWhileRunningReturnsToRunning(t *testing.T) {
	factory := &revisionRuntimeFactory{newRuntime: func(string, int) *applyRuntime { return newApplyRuntime(nil) }}
	manager, controller, h, initialRevision := newApplyHarness(t, factory)
	start, err := controller.Start(t.Context(), initialRevision)
	if err != nil {
		t.Fatal(err)
	}
	result, _ := start.Wait(t.Context())
	if result.Status != appcontrol.OperationSucceeded {
		t.Fatalf("initial start = %+v", result)
	}
	payload := managedDraftPayload{ExpectedConfigRevision: initialRevision, SecretGeneration: 1, Draft: managedApplyDraft(manager.State().Providers[0].UUID, true)}
	if err := h.applyManagedDraft(t.Context(), "op_running", payload); err != nil {
		t.Fatal(err)
	}
	state := controller.Snapshot()
	if state.Service != appcontrol.ServiceRunning || state.RuntimeGeneration != 2 {
		t.Fatalf("runtime state = %+v", state)
	}
	if state.ConfigRevision != manager.State().CommittedConfigRevision {
		t.Fatalf("active config = %q committed = %q", state.ConfigRevision, manager.State().CommittedConfigRevision)
	}
}

func TestApplyManagedDraftFailedStartRestoresPreviousRuntime(t *testing.T) {
	var candidateRevision string
	factory := &revisionRuntimeFactory{newRuntime: func(revision string, call int) *applyRuntime {
		if revision == candidateRevision {
			return newApplyRuntime(errors.New("candidate start failed"))
		}
		return newApplyRuntime(nil)
	}}
	manager, controller, h, initialRevision := newApplyHarness(t, factory)
	start, _ := controller.Start(t.Context(), initialRevision)
	_, _ = start.Wait(t.Context())
	payload := managedDraftPayload{ExpectedConfigRevision: initialRevision, SecretGeneration: 1, Draft: managedApplyDraft(manager.State().Providers[0].UUID, true)}
	candidate, err := manager.BuildManagedCandidate(payload.Draft, payload.SecretGeneration)
	if err != nil {
		t.Fatal(err)
	}
	candidateRevision = candidate.Revision
	err = h.applyManagedDraft(t.Context(), "op_restore", payload)
	var applyErr *ManagedApplyError
	if !errors.As(err, &applyErr) || applyErr.Rollback != nil {
		t.Fatalf("apply error = %v", err)
	}
	state := controller.Snapshot()
	if state.Service != appcontrol.ServiceRunning || state.ConfigRevision != initialRevision {
		t.Fatalf("restored runtime state = %+v", state)
	}
	if got := manager.State().CommittedConfigRevision; got != initialRevision {
		t.Fatalf("restored config = %q", got)
	}
	if _, statErr := os.Stat(manager.paths.Journal); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("journal remains: %v", statErr)
	}
}

func TestApplyManagedDraftRunningPersistsCandidateOnlyAfterReadiness(t *testing.T) {
	validationStarted := make(chan struct{})
	releaseValidation := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseValidation)
		}
	}()

	var candidateRevision string
	factory := &revisionRuntimeFactory{newRuntime: func(revision string, _ int) *applyRuntime {
		runtime := newApplyRuntime(nil)
		if revision == candidateRevision {
			runtime.validateModels = func(ctx context.Context) error {
				close(validationStarted)
				select {
				case <-releaseValidation:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		return runtime
	}}
	manager, controller, h, initialRevision := newApplyHarness(t, factory)
	start, _ := controller.Start(t.Context(), initialRevision)
	_, _ = start.Wait(t.Context())
	payload := managedDraftPayload{ExpectedConfigRevision: initialRevision, SecretGeneration: 1, Draft: managedApplyDraft(manager.State().Providers[0].UUID, true)}
	candidate, err := manager.BuildManagedCandidate(payload.Draft, payload.SecretGeneration)
	if err != nil {
		t.Fatal(err)
	}
	candidateRevision = candidate.Revision

	done := make(chan error, 1)
	go func() {
		done <- h.applyManagedDraft(t.Context(), "op_readiness_boundary", payload)
	}()

	select {
	case <-validationStarted:
	case <-time.After(time.Second):
		t.Fatal("candidate validation did not start")
	}
	persisted, _, err := loadPersistentState(manager.paths.State)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.CommittedConfigRevision != initialRevision || persisted.SelectedConfigRevision != initialRevision {
		t.Fatalf("candidate persisted before readiness: %+v", persisted)
	}

	close(releaseValidation)
	released = true
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("apply error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("managed apply did not finish after validation succeeded")
	}
	persisted, _, err = loadPersistentState(manager.paths.State)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.CommittedConfigRevision != candidateRevision || persisted.SelectedConfigRevision != candidateRevision {
		t.Fatalf("candidate was not persisted after readiness: %+v", persisted)
	}
	if got := manager.State().CommittedConfigRevision; got != candidateRevision {
		t.Fatalf("committed configuration = %q, want %q", got, candidateRevision)
	}
}

func TestApplyManagedDraftFailedStopRestoresPreviousRuntime(t *testing.T) {
	stopFailure := errors.New("stop failed after listener close")
	factory := &revisionRuntimeFactory{newRuntime: func(_ string, call int) *applyRuntime {
		runtime := newApplyRuntime(nil)
		if call == 1 {
			runtime.stop = func(context.Context) error { return stopFailure }
		}
		return runtime
	}}
	manager, controller, h, initialRevision := newApplyHarness(t, factory)
	start, err := controller.Start(t.Context(), initialRevision)
	if err != nil {
		t.Fatal(err)
	}
	if result, waitErr := start.Wait(t.Context()); waitErr != nil || result.Status != appcontrol.OperationSucceeded {
		t.Fatalf("initial start result=%+v err=%v", result, waitErr)
	}

	payload := managedDraftPayload{
		ExpectedConfigRevision: initialRevision,
		SecretGeneration:       1,
		Draft:                  managedApplyDraft(manager.State().Providers[0].UUID, true),
	}
	err = h.applyManagedDraft(t.Context(), "op_stop_restore", payload)
	var applyErr *ManagedApplyError
	if !errors.As(err, &applyErr) || !errors.Is(applyErr.Primary, stopFailure) || applyErr.Rollback != nil {
		t.Fatalf("apply error = %v, want stop failure with successful restore", err)
	}
	state := controller.Snapshot()
	if state.Service != appcontrol.ServiceRunning || state.ConfigRevision != initialRevision || state.RuntimeGeneration != 2 {
		t.Fatalf("restored runtime = %+v", state)
	}
	if got := manager.State().CommittedConfigRevision; got != initialRevision {
		t.Fatalf("restored configuration = %q, want %q", got, initialRevision)
	}
	if _, statErr := os.Stat(manager.paths.Journal); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("journal remains: %v", statErr)
	}
}

func TestApplyManagedDraftRollbackStartFailurePersistsRecovery(t *testing.T) {
	var candidateRevision string
	factory := &revisionRuntimeFactory{newRuntime: func(revision string, call int) *applyRuntime {
		switch {
		case revision == candidateRevision:
			return newApplyRuntime(errors.New("candidate start failed"))
		case call > 1:
			return newApplyRuntime(errors.New("rollback start failed"))
		default:
			return newApplyRuntime(nil)
		}
	}}
	manager, controller, h, initialRevision := newApplyHarness(t, factory)
	start, _ := controller.Start(t.Context(), initialRevision)
	_, _ = start.Wait(t.Context())
	payload := managedDraftPayload{ExpectedConfigRevision: initialRevision, SecretGeneration: 1, Draft: managedApplyDraft(manager.State().Providers[0].UUID, true)}
	candidate, err := manager.BuildManagedCandidate(payload.Draft, payload.SecretGeneration)
	if err != nil {
		t.Fatal(err)
	}
	candidateRevision = candidate.Revision
	err = h.applyManagedDraft(t.Context(), "op_rollback_failed", payload)
	var applyErr *ManagedApplyError
	if !errors.As(err, &applyErr) || applyErr.Rollback == nil {
		t.Fatalf("apply error = %v", err)
	}
	state := manager.State()
	if state.RecoveryState != string(ApplyPhaseRollbackFailed) || state.RecoveryPrimaryCode == "" || state.RecoveryRollbackCode == "" {
		t.Fatalf("recovery state = %+v", state)
	}
	if _, statErr := os.Stat(manager.paths.Journal); statErr != nil {
		t.Fatalf("journal not retained: %v", statErr)
	}
}

func TestApplyManagedDraftStoppedCommitFailureRestoresPreviousFile(t *testing.T) {
	factory := &revisionRuntimeFactory{newRuntime: func(string, int) *applyRuntime { return newApplyRuntime(nil) }}
	manager, controller, h, initialRevision := newApplyHarness(t, factory)
	h.managedCommit = func(*ManagedApplyTransaction) error { return errors.New("commit failed") }
	payload := managedDraftPayload{ExpectedConfigRevision: initialRevision, SecretGeneration: 1, Draft: managedApplyDraft(manager.State().Providers[0].UUID, true)}
	err := h.applyManagedDraft(t.Context(), "op_commit_stopped", payload)
	var applyErr *ManagedApplyError
	if !errors.As(err, &applyErr) || applyErr.Rollback != nil {
		t.Fatalf("apply error = %v", err)
	}
	if got := controller.Snapshot().Service; got != appcontrol.ServiceStopped {
		t.Fatalf("service = %q", got)
	}
	if got := manager.State().CommittedConfigRevision; got != initialRevision {
		t.Fatalf("committed revision = %q, want %q", got, initialRevision)
	}
	body, readErr := os.ReadFile(manager.paths.Managed)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if revision, _ := configRevision(body); revision != initialRevision {
		t.Fatalf("managed file revision = %q, want %q", revision, initialRevision)
	}
}

func TestApplyManagedDraftStoppedCancellationAfterInstallRollsBack(t *testing.T) {
	factory := &revisionRuntimeFactory{newRuntime: func(string, int) *applyRuntime { return newApplyRuntime(nil) }}
	manager, controller, h, initialRevision := newApplyHarness(t, factory)
	ctx, cancel := context.WithCancel(t.Context())
	h.beforeManagedCommit = cancel
	payload := managedDraftPayload{ExpectedConfigRevision: initialRevision, SecretGeneration: 1, Draft: managedApplyDraft(manager.State().Providers[0].UUID, true)}
	candidate, err := manager.BuildManagedCandidate(payload.Draft, payload.SecretGeneration)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.opts.Secrets.Set(SecretProjection{ConfigRevision: candidate.Revision, SecretGeneration: candidate.SecretGeneration}); err != nil {
		t.Fatal(err)
	}

	err = h.applyManagedDraft(ctx, "op_cancel_stopped", payload)
	var applyErr *ManagedApplyError
	if !errors.As(err, &applyErr) || !errors.Is(applyErr.Primary, context.Canceled) || applyErr.Rollback != nil {
		t.Fatalf("apply error = %+v, want canceled primary with successful rollback", err)
	}
	if got := controller.Snapshot().Service; got != appcontrol.ServiceStopped {
		t.Fatalf("service = %q", got)
	}
	if got := manager.State().CommittedConfigRevision; got != initialRevision {
		t.Fatalf("committed revision = %q, want %q", got, initialRevision)
	}
	if h.opts.Secrets.Has(candidate.Revision, candidate.SecretGeneration) {
		t.Fatal("canceled candidate secret projection was retained")
	}
}

func TestApplyManagedDraftRunningCommitFailureStopsCandidateAndRestoresOldRuntime(t *testing.T) {
	factory := &revisionRuntimeFactory{newRuntime: func(string, int) *applyRuntime { return newApplyRuntime(nil) }}
	manager, controller, h, initialRevision := newApplyHarness(t, factory)
	start, _ := controller.Start(t.Context(), initialRevision)
	_, _ = start.Wait(t.Context())
	h.managedCommit = func(*ManagedApplyTransaction) error { return errors.New("commit failed") }
	payload := managedDraftPayload{ExpectedConfigRevision: initialRevision, SecretGeneration: 1, Draft: managedApplyDraft(manager.State().Providers[0].UUID, true)}
	err := h.applyManagedDraft(t.Context(), "op_commit_running", payload)
	var applyErr *ManagedApplyError
	if !errors.As(err, &applyErr) || applyErr.Rollback != nil {
		t.Fatalf("apply error = %v", err)
	}
	state := controller.Snapshot()
	if state.Service != appcontrol.ServiceRunning || state.ConfigRevision != initialRevision || state.RuntimeGeneration != 3 {
		t.Fatalf("restored runtime = %+v", state)
	}
	if got := manager.State().CommittedConfigRevision; got != initialRevision {
		t.Fatalf("committed revision = %q, want %q", got, initialRevision)
	}
}

func TestApplyManagedDraftInstallFailureAfterStopRestoresRunningIntent(t *testing.T) {
	var manager *ConfigManager
	factory := &revisionRuntimeFactory{newRuntime: func(_ string, call int) *applyRuntime {
		runtime := newApplyRuntime(nil)
		if call == 1 {
			runtime.onStop = func() { _ = os.Remove(manager.paths.Staged) }
		}
		return runtime
	}}
	var controller *appcontrol.Controller
	var h *helper
	var initialRevision string
	manager, controller, h, initialRevision = newApplyHarness(t, factory)
	start, _ := controller.Start(t.Context(), initialRevision)
	_, _ = start.Wait(t.Context())
	payload := managedDraftPayload{ExpectedConfigRevision: initialRevision, SecretGeneration: 1, Draft: managedApplyDraft(manager.State().Providers[0].UUID, true)}
	err := h.applyManagedDraft(t.Context(), "op_install_failure", payload)
	var applyErr *ManagedApplyError
	if !errors.As(err, &applyErr) || applyErr.Rollback != nil {
		t.Fatalf("apply error = %v", err)
	}
	state := controller.Snapshot()
	if state.Service != appcontrol.ServiceRunning || state.ConfigRevision != initialRevision {
		t.Fatalf("restored runtime = %+v", state)
	}
}

func TestApplyManagedDraftStartTimeoutWaitsForCanceledCleanupBeforeRollback(t *testing.T) {
	validationStarted := make(chan struct{})
	cleanupStarted := make(chan struct{})
	releaseCleanup := make(chan struct{})
	released := false
	defer func() {
		if !released {
			close(releaseCleanup)
		}
	}()

	var candidateRevision string
	factory := &revisionRuntimeFactory{newRuntime: func(revision string, _ int) *applyRuntime {
		runtime := newApplyRuntime(nil)
		if revision == candidateRevision {
			runtime.validateModels = func(ctx context.Context) error {
				close(validationStarted)
				<-ctx.Done()
				return ctx.Err()
			}
			runtime.stop = func(ctx context.Context) error {
				close(cleanupStarted)
				select {
				case <-releaseCleanup:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
		}
		return runtime
	}}
	manager, controller, h, initialRevision := newApplyHarness(t, factory)
	// Keep the timeout short enough to exercise cancellation while leaving
	// rollback startup headroom under the race detector.
	h.opts.ShutdownTimeout = 500 * time.Millisecond
	start, err := controller.Start(t.Context(), initialRevision)
	if err != nil {
		t.Fatal(err)
	}
	if result, waitErr := start.Wait(t.Context()); waitErr != nil || result.Status != appcontrol.OperationSucceeded {
		t.Fatalf("initial start result=%+v err=%v", result, waitErr)
	}

	payload := managedDraftPayload{ExpectedConfigRevision: initialRevision, SecretGeneration: 1, Draft: managedApplyDraft(manager.State().Providers[0].UUID, true)}
	candidate, err := manager.BuildManagedCandidate(payload.Draft, payload.SecretGeneration)
	if err != nil {
		t.Fatal(err)
	}
	candidateRevision = candidate.Revision

	done := make(chan error, 1)
	go func() {
		done <- h.applyManagedDraft(t.Context(), "op_cleanup_wait", payload)
	}()

	select {
	case <-validationStarted:
	case <-time.After(time.Second):
		t.Fatal("candidate validation did not start")
	}
	select {
	case <-cleanupStarted:
	case <-time.After(time.Second):
		t.Fatal("timed-out candidate start was not canceled and cleaned up")
	}

	if got := manager.State().CommittedConfigRevision; got != candidateRevision {
		t.Fatalf("configuration rolled back during active cleanup: got %q, want candidate %q", got, candidateRevision)
	}
	select {
	case err := <-done:
		t.Fatalf("managed apply returned before controller cleanup completed: %v", err)
	default:
	}

	close(releaseCleanup)
	released = true
	select {
	case err := <-done:
		var applyErr *ManagedApplyError
		if !errors.As(err, &applyErr) || applyErr.Primary == nil || applyErr.Rollback != nil {
			t.Fatalf("apply error = %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("managed apply did not finish after cleanup completed")
	}
	state := controller.Snapshot()
	if state.Service != appcontrol.ServiceRunning || state.ConfigRevision != initialRevision || state.Operation != nil {
		t.Fatalf("restored runtime = %+v", state)
	}
	if got := manager.State().CommittedConfigRevision; got != initialRevision {
		t.Fatalf("restored configuration = %q, want %q", got, initialRevision)
	}
}
