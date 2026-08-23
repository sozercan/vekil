package appcontrol

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

type testSource struct {
	cfg     Configuration
	err     error
	started chan struct{}
	block   bool
}

func (s *testSource) LoadConfiguration(ctx context.Context) (Configuration, error) {
	if s.started != nil {
		select {
		case <-s.started:
		default:
			close(s.started)
		}
	}
	if s.block {
		<-ctx.Done()
		return Configuration{}, ctx.Err()
	}
	return s.cfg, s.err
}

type testFactory struct {
	runtime *testRuntime
	started chan struct{}
	block   bool
}

func (f *testFactory) NewRuntime(ctx context.Context, _ Configuration) (Runtime, error) {
	if f.started != nil {
		select {
		case <-f.started:
		default:
			close(f.started)
		}
	}
	if f.block {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return f.runtime, nil
}

type testAuthenticator struct {
	started chan struct{}
	block   bool
	err     error
}

func (a *testAuthenticator) GetToken(ctx context.Context) (string, error) {
	if a.started != nil {
		select {
		case <-a.started:
		default:
			close(a.started)
		}
	}
	if a.block {
		<-ctx.Done()
		return "", ctx.Err()
	}
	return "token", a.err
}

type testReadiness struct {
	started chan struct{}
	block   bool
	err     error
}

func (r *testReadiness) CheckReadiness(ctx context.Context, _ string) error {
	if r.started != nil {
		select {
		case <-r.started:
		default:
			close(r.started)
		}
	}
	if r.block {
		<-ctx.Done()
		return ctx.Err()
	}
	return r.err
}

type testRuntime struct {
	mu sync.Mutex

	usesCopilot   bool
	addr          string
	startBlock    bool
	validateBlock bool
	policyBlock   bool

	startStarted    chan struct{}
	validateStarted chan struct{}
	policyStarted   chan struct{}
	stopStarted     chan struct{}
	releaseStop     chan struct{}
	stopErr         error
	leaveDoneOpen   bool
	done            chan error
	stopOnce        sync.Once
	authPending     []bool
}

func newTestRuntime() *testRuntime {
	return &testRuntime{addr: "127.0.0.1:12345", done: make(chan error, 1)}
}

func (r *testRuntime) Start(ctx context.Context) error {
	closeOnce(r.startStarted)
	if r.startBlock {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (r *testRuntime) Stop(ctx context.Context) error {
	closeOnce(r.stopStarted)
	if r.releaseStop != nil {
		select {
		case <-r.releaseStop:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.stopOnce.Do(func() {
		if r.leaveDoneOpen {
			return
		}
		select {
		case <-r.done:
		default:
			close(r.done)
		}
	})
	return r.stopErr
}
func (r *testRuntime) Done() <-chan error { return r.done }
func (r *testRuntime) Addr() string       { return r.addr }
func (r *testRuntime) UsesCopilot() bool  { return r.usesCopilot }
func (r *testRuntime) SetStartupAuthenticationPending(value bool) {
	r.mu.Lock()
	r.authPending = append(r.authPending, value)
	r.mu.Unlock()
}
func (r *testRuntime) ValidateDynamicProviderModels(ctx context.Context) error {
	closeOnce(r.validateStarted)
	if r.validateBlock {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}
func (r *testRuntime) InitializePolicyRouting(ctx context.Context) error {
	closeOnce(r.policyStarted)
	if r.policyBlock {
		<-ctx.Done()
		return ctx.Err()
	}
	return nil
}

func closeOnce(ch chan struct{}) {
	if ch == nil {
		return
	}
	select {
	case <-ch:
	default:
		close(ch)
	}
}

func newControllerForTest(t *testing.T, runtime *testRuntime, authn Authenticator, readiness ReadinessChecker) *Controller {
	t.Helper()
	c, err := New(Options{
		ConfigurationSource: &testSource{cfg: Configuration{Revision: "cfg_test"}},
		RuntimeFactory:      &testFactory{runtime: runtime},
		Authenticator:       authn,
		ReadinessChecker:    readiness,
		OperationID:         func() string { return "op_test" },
		StopTimeout:         time.Second,
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	return c
}

func TestControllerStartStopAndRuntimeGeneration(t *testing.T) {
	runtime := newTestRuntime()
	controller := newControllerForTest(t, runtime, nil, ReadinessCheckFunc(func(context.Context, string) error { return nil }))

	op, err := controller.Start(t.Context(), "cfg_test")
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	result, err := op.Wait(t.Context())
	if err != nil || result.Status != OperationSucceeded {
		t.Fatalf("start result = %+v, wait error = %v", result, err)
	}
	state := controller.Snapshot()
	if state.Service != ServiceRunning || state.Readiness != ReadinessReady || state.RuntimeGeneration != 1 || state.Addr != runtime.addr {
		t.Fatalf("running state = %+v", state)
	}
	if state.Operation != nil {
		t.Fatalf("operation remained active: %+v", state.Operation)
	}

	stop, err := controller.Stop(t.Context())
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	stopResult, err := stop.Wait(t.Context())
	if err != nil || stopResult.Status != OperationSucceeded {
		t.Fatalf("stop result = %+v, wait error = %v", stopResult, err)
	}
	state = controller.Snapshot()
	if state.Service != ServiceStopped || state.Readiness != ReadinessUnknown || state.RuntimeGeneration != 1 {
		t.Fatalf("stopped state = %+v", state)
	}
}

func TestControllerStartupAuthenticationGate(t *testing.T) {
	runtime := newTestRuntime()
	runtime.usesCopilot = true
	authn := &testAuthenticator{}
	controller := newControllerForTest(t, runtime, authn, ReadinessCheckFunc(func(context.Context, string) error { return nil }))
	op, err := controller.Start(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	result, _ := op.Wait(t.Context())
	if result.Status != OperationSucceeded {
		t.Fatalf("result = %+v", result)
	}
	runtime.mu.Lock()
	pending := append([]bool(nil), runtime.authPending...)
	runtime.mu.Unlock()
	if len(pending) != 2 || !pending[0] || pending[1] {
		t.Fatalf("auth pending transitions = %v, want [true false]", pending)
	}
	if got := controller.Snapshot().Auth; got != AuthSignedIn {
		t.Fatalf("Auth = %q, want signed_in", got)
	}
}

func TestControllerCancellationHoldsAdmissionUntilCleanup(t *testing.T) {
	stages := []struct {
		name       string
		setup      func(*testSource, *testFactory, *testRuntime, *testAuthenticator, *testReadiness) <-chan struct{}
		hasRuntime bool
	}{
		{"loading_configuration", func(s *testSource, _ *testFactory, _ *testRuntime, _ *testAuthenticator, _ *testReadiness) <-chan struct{} {
			s.block = true
			return s.started
		}, false},
		{"constructing_server", func(_ *testSource, f *testFactory, _ *testRuntime, _ *testAuthenticator, _ *testReadiness) <-chan struct{} {
			f.block = true
			return f.started
		}, false},
		{"listener_startup", func(_ *testSource, _ *testFactory, r *testRuntime, _ *testAuthenticator, _ *testReadiness) <-chan struct{} {
			r.startBlock = true
			return r.startStarted
		}, true},
		{"startup_authentication", func(_ *testSource, _ *testFactory, r *testRuntime, a *testAuthenticator, _ *testReadiness) <-chan struct{} {
			r.usesCopilot = true
			a.block = true
			return a.started
		}, true},
		{"dynamic_provider_model_validation", func(_ *testSource, _ *testFactory, r *testRuntime, _ *testAuthenticator, _ *testReadiness) <-chan struct{} {
			r.validateBlock = true
			return r.validateStarted
		}, true},
		{"policy_routing_preflight", func(_ *testSource, _ *testFactory, r *testRuntime, _ *testAuthenticator, _ *testReadiness) <-chan struct{} {
			r.policyBlock = true
			return r.policyStarted
		}, true},
		{"readiness_check", func(_ *testSource, _ *testFactory, _ *testRuntime, _ *testAuthenticator, ready *testReadiness) <-chan struct{} {
			ready.block = true
			return ready.started
		}, true},
	}
	for _, stage := range stages {
		stage := stage
		t.Run(stage.name, func(t *testing.T) {
			runtime := newTestRuntime()
			runtime.startStarted = make(chan struct{})
			runtime.validateStarted = make(chan struct{})
			runtime.policyStarted = make(chan struct{})
			runtime.stopStarted = make(chan struct{})
			runtime.releaseStop = make(chan struct{})
			source := &testSource{cfg: Configuration{Revision: "cfg_test"}, started: make(chan struct{})}
			factory := &testFactory{runtime: runtime, started: make(chan struct{})}
			authn := &testAuthenticator{started: make(chan struct{})}
			ready := &testReadiness{started: make(chan struct{})}
			started := stage.setup(source, factory, runtime, authn, ready)
			controller, err := New(Options{
				ConfigurationSource: source, RuntimeFactory: factory, Authenticator: authn,
				ReadinessChecker: ready, OperationID: func() string { return "op_" + stage.name }, StopTimeout: time.Second,
			})
			if err != nil {
				t.Fatal(err)
			}
			op, err := controller.Start(t.Context(), "")
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-started:
			case <-time.After(time.Second):
				t.Fatalf("stage %s did not start", stage.name)
			}
			if err := controller.CancelOperation(op.ID); err != nil {
				t.Fatal(err)
			}
			if stage.hasRuntime {
				select {
				case <-runtime.stopStarted:
				case <-time.After(time.Second):
					t.Fatal("cleanup stop did not start")
				}
				select {
				case <-op.Done():
					t.Fatal("operation completed before listener cleanup")
				default:
				}
				if _, err := controller.Start(t.Context(), ""); !errors.Is(err, ErrOperationInProgress) {
					t.Fatalf("replacement Start() error = %v, want operation in progress", err)
				}
				close(runtime.releaseStop)
			}
			result, err := op.Wait(t.Context())
			if err != nil || result.Status != OperationCanceled {
				t.Fatalf("result = %+v, wait error = %v", result, err)
			}
		})
	}
}

func TestControllerStaleCancellationCannotAffectCurrentOperation(t *testing.T) {
	runtime := newTestRuntime()
	runtime.validateBlock = true
	runtime.validateStarted = make(chan struct{})
	runtime.stopStarted = make(chan struct{})
	runtime.releaseStop = make(chan struct{})
	controller := newControllerForTest(t, runtime, nil, nil)
	op, err := controller.Start(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	<-runtime.validateStarted
	if err := controller.CancelOperation("op_stale"); !errors.Is(err, ErrOperationIDMismatch) {
		t.Fatalf("CancelOperation(stale) error = %v", err)
	}
	select {
	case <-op.Done():
		t.Fatal("stale cancellation completed current operation")
	default:
	}
	if err := controller.CancelOperation(op.ID); err != nil {
		t.Fatal(err)
	}
	<-runtime.stopStarted
	close(runtime.releaseStop)
	_, _ = op.Wait(t.Context())
}

func TestControllerUnexpectedListenerTerminationIsGenerationScoped(t *testing.T) {
	runtime := newTestRuntime()
	controller := newControllerForTest(t, runtime, nil, nil)
	op, err := controller.Start(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	result, _ := op.Wait(t.Context())
	if result.Status != OperationSucceeded {
		t.Fatalf("result = %+v", result)
	}
	runtime.done <- errors.New("listener failed")
	close(runtime.done)
	deadline := time.Now().Add(time.Second)
	for controller.Snapshot().Service != ServiceFailed {
		if time.Now().After(deadline) {
			t.Fatal("listener failure did not update state")
		}
		time.Sleep(time.Millisecond)
	}
	state := controller.Snapshot()
	if state.RuntimeGeneration != 1 || state.LastFailureCode != "listener_terminated" {
		t.Fatalf("state = %+v", state)
	}
}

func TestHTTPReadinessChecker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/readyz" {
			t.Errorf("path = %q", r.URL.Path)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	if err := (HTTPReadinessChecker{}).CheckReadiness(t.Context(), server.URL); err != nil {
		t.Fatalf("CheckReadiness() error = %v", err)
	}
}

type blockingConfigurationObserver struct {
	activated chan struct{}
	release   chan struct{}
}

func (o *blockingConfigurationObserver) RuntimeActivated(ctx context.Context, _ Configuration, _ uint64, _ string) error {
	closeOnce(o.activated)
	select {
	case <-o.release:
		return nil
	case <-ctx.Done():
		<-o.release
		return ctx.Err()
	}
}
func (o *blockingConfigurationObserver) RuntimeDeactivated(context.Context, uint64) error { return nil }

func TestControllerCancellationDuringActivationCannotCommitSuccess(t *testing.T) {
	runtime := newTestRuntime()
	observer := &blockingConfigurationObserver{activated: make(chan struct{}), release: make(chan struct{})}
	controller, err := New(Options{
		ConfigurationSource:   &testSource{cfg: Configuration{Revision: "cfg_test"}},
		ConfigurationObserver: observer,
		RuntimeFactory:        &testFactory{runtime: runtime},
		OperationID:           func() string { return "op_activation" },
		StopTimeout:           time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	operation, err := controller.Start(t.Context(), "cfg_test")
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-observer.activated:
	case <-time.After(time.Second):
		t.Fatal("activation observer did not start")
	}
	if err := controller.CancelOperation(operation.ID); err != nil {
		t.Fatal(err)
	}
	close(observer.release)
	result, err := operation.Wait(t.Context())
	if err != nil || result.Status != OperationCanceled {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	if state := controller.Snapshot(); state.Service != ServiceStopped || state.Readiness != ReadinessUnknown {
		t.Fatalf("state after cancellation = %+v", state)
	}
}

func TestUnexpectedListenerTerminationRetainsOwnershipUntilCleanup(t *testing.T) {
	runtime := newTestRuntime()
	runtime.stopStarted = make(chan struct{})
	runtime.releaseStop = make(chan struct{})
	controller := newControllerForTest(t, runtime, nil, nil)
	operation, err := controller.Start(t.Context(), "")
	if err != nil {
		t.Fatal(err)
	}
	result, _ := operation.Wait(t.Context())
	if result.Status != OperationSucceeded {
		t.Fatalf("start result = %+v", result)
	}
	runtime.done <- errors.New("listener failed")
	close(runtime.done)
	select {
	case <-runtime.stopStarted:
	case <-time.After(time.Second):
		t.Fatal("unexpected listener cleanup did not call Stop")
	}
	if _, err := controller.Start(t.Context(), ""); !errors.Is(err, ErrRuntimeCleanupPending) {
		t.Fatalf("Start during cleanup error = %v", err)
	}
	close(runtime.releaseStop)
	deadline := time.Now().Add(time.Second)
	for {
		controller.mu.Lock()
		owned := controller.runtime != nil
		controller.mu.Unlock()
		if !owned {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("runtime ownership was not released after cleanup")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestFailedStopReleasesRuntimeAndAllowsReplacement(t *testing.T) {
	runtime := newTestRuntime()
	runtime.stopErr = errors.New("graceful shutdown reported an error")
	runtime.leaveDoneOpen = true
	controller := newControllerForTest(t, runtime, nil, nil)
	start, _ := controller.Start(t.Context(), "")
	_, _ = start.Wait(t.Context())
	stop, err := controller.Stop(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	result, err := stop.Wait(t.Context())
	if err != nil || result.Status != OperationFailed {
		t.Fatalf("stop result=%+v err=%v", result, err)
	}
	controller.mu.Lock()
	owned := controller.runtime != nil
	controller.mu.Unlock()
	if owned {
		t.Fatal("failed Stop retained runtime ownership after terminal cleanup")
	}
	runtime.stopErr = nil
	restart, err := controller.Start(t.Context(), "")
	if err != nil {
		t.Fatalf("Start after failed Stop error = %v", err)
	}
	restartResult, err := restart.Wait(t.Context())
	if err != nil || restartResult.Status != OperationSucceeded {
		t.Fatalf("restart result=%+v err=%v", restartResult, err)
	}
	close(runtime.done)
}
