package appcontrol

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"sync"
	"time"
)

const defaultStopTimeout = 10 * time.Second

// Controller serializes proxy mutations while keeping reads and named
// cancellation responsive.
type Controller struct {
	mu sync.Mutex

	source      ConfigurationSource
	observer    ConfigurationObserver
	factory     RuntimeFactory
	auth        Authenticator
	readiness   ReadinessChecker
	operationID func() string
	stopTimeout time.Duration

	rootCtx    context.Context
	rootCancel context.CancelCauseFunc
	closed     bool

	state   State
	runtime Runtime
	active  *activeOperation
	updates chan State
}

// New creates an idle, stopped controller.
func New(opts Options) (*Controller, error) {
	if opts.ConfigurationSource == nil {
		return nil, errors.New("configuration source is required")
	}
	if opts.RuntimeFactory == nil {
		return nil, errors.New("runtime factory is required")
	}
	if opts.OperationID == nil {
		opts.OperationID = randomOperationID
	}
	if opts.StopTimeout <= 0 {
		opts.StopTimeout = defaultStopTimeout
	}
	ctx, cancel := context.WithCancelCause(context.Background())
	c := &Controller{
		source:      opts.ConfigurationSource,
		observer:    opts.ConfigurationObserver,
		factory:     opts.RuntimeFactory,
		auth:        opts.Authenticator,
		readiness:   opts.ReadinessChecker,
		operationID: opts.OperationID,
		stopTimeout: opts.StopTimeout,
		rootCtx:     ctx,
		rootCancel:  cancel,
		updates:     make(chan State, 1),
		state: State{
			Service:   ServiceStopped,
			Readiness: ReadinessUnknown,
			Auth:      AuthSignedOut,
		},
	}
	c.publishLocked()
	return c, nil
}

// Snapshot returns a copy of current state.
func (c *Controller) Snapshot() State {
	c.mu.Lock()
	defer c.mu.Unlock()
	return cloneState(c.state)
}

// UsesCopilot reports whether the currently allocated runtime requires GitHub
// Copilot authentication. It is a read-only lifecycle query used by app-owned
// sign-out orchestration.
func (c *Controller) UsesCopilot() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.runtime != nil && c.runtime.UsesCopilot()
}

// Updates yields coalesced complete state snapshots. It has one intended
// consumer; callers needing a point-in-time view should use Snapshot.
func (c *Controller) Updates() <-chan State { return c.updates }

// Start admits one asynchronous startup operation.
func (c *Controller) Start(parent context.Context, expectedConfigRevision string) (Operation, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Operation{}, ErrClosed
	}
	if c.active != nil {
		c.mu.Unlock()
		return Operation{}, ErrOperationInProgress
	}
	if c.runtime != nil {
		err := ErrRuntimeCleanupPending
		if c.state.Service == ServiceRunning {
			err = ErrAlreadyRunning
		}
		c.mu.Unlock()
		return Operation{}, err
	}
	op := c.newOperationLocked(parent, OperationStart, true)
	c.state.Service = ServiceStarting
	c.state.Readiness = ReadinessChecking
	c.state.LastFailureCode = ""
	c.setOperationPhaseLocked(op, PhaseLoadingConfiguration)
	c.mu.Unlock()

	go c.runStart(op, expectedConfigRevision)
	return Operation{ID: op.id, Kind: op.kind, completion: op.completion}, nil
}

// Stop admits one asynchronous graceful stop operation.
func (c *Controller) Stop(parent context.Context) (Operation, error) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return Operation{}, ErrClosed
	}
	if c.active != nil {
		c.mu.Unlock()
		return Operation{}, ErrOperationInProgress
	}
	if c.runtime == nil {
		c.mu.Unlock()
		return Operation{}, ErrNotRunning
	}
	op := c.newOperationLocked(parent, OperationStop, false)
	c.state.Service = ServiceStopping
	c.state.Readiness = ReadinessUnknown
	c.publishLocked()
	c.mu.Unlock()

	go c.runStop(op)
	return Operation{ID: op.id, Kind: op.kind, completion: op.completion}, nil
}

// CancelOperation cancels only the named active operation. Admission remains
// occupied until cleanup and the terminal result complete.
func (c *Controller) CancelOperation(operationID string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		return ErrOperationNotFound
	}
	if c.active.id != operationID {
		return ErrOperationIDMismatch
	}
	if !c.active.cancelable {
		return ErrOperationNotCancelable
	}
	c.active.cancel(context.Canceled)
	return nil
}

// Shutdown bypasses mutation admission, cancels active work, waits for its
// cleanup, then stops any remaining runtime. It is idempotent.
func (c *Controller) Shutdown(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	c.mu.Lock()
	if !c.closed {
		c.closed = true
		c.rootCancel(ErrClosed)
	}
	active := c.active
	if active != nil {
		active.cancel(ErrClosed)
	}
	c.mu.Unlock()

	if active != nil {
		select {
		case <-active.completion.done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	c.mu.Lock()
	runtime := c.runtime
	generation := c.state.RuntimeGeneration
	if runtime != nil {
		c.state.Service = ServiceStopping
		c.state.Readiness = ReadinessUnknown
		c.publishLocked()
	}
	c.mu.Unlock()
	if runtime == nil {
		return nil
	}
	if err := runtime.Stop(ctx); err != nil {
		return err
	}
	c.deactivateRuntime(runtime, generation, ServiceStopped, "")
	return nil
}

func (c *Controller) newOperationLocked(parent context.Context, kind OperationKind, cancelable bool) *activeOperation {
	ctx, cancel := context.WithCancelCause(c.rootCtx)
	op := &activeOperation{
		id:         c.operationID(),
		kind:       kind,
		cancelable: cancelable,
		ctx:        ctx,
		cancel:     cancel,
		completion: &operationCompletion{done: make(chan struct{})},
	}
	if parent != nil {
		op.stopParent = context.AfterFunc(parent, func() {
			cancel(context.Cause(parent))
		})
	}
	c.active = op
	c.state.Operation = &OperationState{ID: op.id, Kind: kind}
	return op
}

func (c *Controller) runStart(op *activeOperation, expectedRevision string) {
	cfg, err := c.source.LoadConfiguration(op.ctx)
	if err != nil {
		c.finishStartFailure(op, PhaseLoadingConfiguration, nil, 0, err)
		return
	}
	if expectedRevision != "" && expectedRevision != cfg.Revision {
		c.finishStartFailure(op, PhaseLoadingConfiguration, nil, 0, fmt.Errorf("%w: expected %q, loaded %q", ErrConfigRevisionMismatch, expectedRevision, cfg.Revision))
		return
	}
	if err := op.ctx.Err(); err != nil {
		c.finishStartFailure(op, PhaseLoadingConfiguration, nil, 0, err)
		return
	}

	c.setOperationPhase(op, PhaseConstructingServer)
	runtime, err := c.factory.NewRuntime(op.ctx, cfg)
	if err != nil {
		c.finishStartFailure(op, PhaseConstructingServer, nil, 0, err)
		return
	}

	c.mu.Lock()
	if c.active != op {
		c.mu.Unlock()
		_ = c.cleanupRuntime(runtime, 0)
		return
	}
	c.state.RuntimeGeneration++
	generation := c.state.RuntimeGeneration
	c.runtime = runtime
	c.state.ConfigRevision = cfg.Revision
	c.state.SecretGeneration = cfg.SecretGeneration
	c.state.Auth = AuthNotRequired
	if runtime.UsesCopilot() {
		c.state.Auth = AuthSignedOut
		runtime.SetStartupAuthenticationPending(true)
	}
	c.publishLocked()
	c.mu.Unlock()

	if err := op.ctx.Err(); err != nil {
		c.finishStartFailure(op, PhaseConstructingServer, runtime, generation, err)
		return
	}

	c.setOperationPhase(op, PhaseListenerStartup)
	if err := runtime.Start(op.ctx); err != nil {
		c.finishStartFailure(op, PhaseListenerStartup, runtime, generation, err)
		return
	}
	go c.monitorRuntime(runtime, generation)

	if runtime.UsesCopilot() {
		c.setOperationPhase(op, PhaseStartupAuthentication)
		c.setAuth(op, AuthSigningIn)
		if c.auth == nil {
			c.finishStartFailure(op, PhaseStartupAuthentication, runtime, generation, errors.New("copilot authentication is unavailable"))
			return
		}
		if _, err := c.auth.GetToken(op.ctx); err != nil {
			c.setAuth(op, AuthFailed)
			c.finishStartFailure(op, PhaseStartupAuthentication, runtime, generation, err)
			return
		}
		runtime.SetStartupAuthenticationPending(false)
		c.setAuth(op, AuthSignedIn)
	}

	c.setOperationPhase(op, PhaseDynamicProviderModelValidation)
	if err := runtime.ValidateDynamicProviderModels(op.ctx); err != nil {
		c.finishStartFailure(op, PhaseDynamicProviderModelValidation, runtime, generation, err)
		return
	}

	c.setOperationPhase(op, PhasePolicyRoutingPreflight)
	if err := runtime.InitializePolicyRouting(op.ctx); err != nil {
		c.finishStartFailure(op, PhasePolicyRoutingPreflight, runtime, generation, err)
		return
	}

	c.setOperationPhase(op, PhaseReadinessCheck)
	if c.readiness != nil {
		if err := c.readiness.CheckReadiness(op.ctx, runtime.Addr()); err != nil {
			c.finishStartFailure(op, PhaseReadinessCheck, runtime, generation, err)
			return
		}
	}
	if err := op.ctx.Err(); err != nil {
		c.finishStartFailure(op, PhaseReadinessCheck, runtime, generation, err)
		return
	}

	if c.observer != nil {
		if err := c.observer.RuntimeActivated(op.ctx, cfg, generation, runtime.Addr()); err != nil {
			c.finishStartFailure(op, PhaseReadinessCheck, runtime, generation, err)
			return
		}
	}
	if err := op.ctx.Err(); err != nil {
		c.finishStartFailure(op, PhaseReadinessCheck, runtime, generation, err)
		return
	}

	c.mu.Lock()
	if c.active != op || c.runtime != runtime || c.state.RuntimeGeneration != generation {
		c.mu.Unlock()
		_ = c.cleanupRuntime(runtime, generation)
		return
	}
	if cause := context.Cause(op.ctx); cause != nil {
		c.mu.Unlock()
		c.finishStartFailure(op, PhaseReadinessCheck, runtime, generation, cause)
		return
	}
	c.state.Service = ServiceRunning
	c.state.Readiness = ReadinessReady
	c.state.Addr = runtime.Addr()
	c.state.LastFailureCode = ""
	c.completeOperationLocked(op, OperationSucceeded, nil)
	c.mu.Unlock()
}

func (c *Controller) runStop(op *activeOperation) {
	c.mu.Lock()
	runtime := c.runtime
	generation := c.state.RuntimeGeneration
	c.mu.Unlock()

	var err error
	if runtime == nil {
		err = ErrNotRunning
	} else {
		ctx, cancel := c.cleanupContext()
		err = runtime.Stop(ctx)
		cancel()
	}
	if err == nil && c.observer != nil {
		err = c.observer.RuntimeDeactivated(context.WithoutCancel(c.rootCtx), generation)
	}

	c.mu.Lock()
	if c.active != op {
		c.mu.Unlock()
		return
	}
	status := OperationSucceeded
	if err != nil {
		status = OperationFailed
		c.state.Service = ServiceFailed
		c.state.LastFailureCode = "stop_failed"
	} else {
		if c.runtime == runtime {
			c.runtime = nil
		}
		c.state.Service = ServiceStopped
		c.state.Readiness = ReadinessUnknown
		c.state.Addr = ""
	}
	c.completeOperationLocked(op, status, err)
	c.mu.Unlock()
}

func (c *Controller) finishStartFailure(op *activeOperation, phase OperationPhase, runtime Runtime, generation uint64, err error) {
	status := OperationFailed
	cause := context.Cause(op.ctx)
	var terminated *runtimeTerminatedError
	if errors.As(cause, &terminated) {
		err = cause
	} else if cause != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrClosed) {
		status = OperationCanceled
	}
	stageErr := &StageError{Phase: phase, Err: err}
	if runtime != nil {
		c.setOperationPhase(op, PhaseCleanup)
		runtime.SetStartupAuthenticationPending(false)
		if cleanupErr := c.cleanupRuntime(runtime, generation); cleanupErr != nil {
			stageErr.Err = errors.Join(err, fmt.Errorf("cleanup: %w", cleanupErr))
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != op {
		return
	}
	if runtime == nil || c.runtime == runtime {
		c.runtime = nil
		c.state.Addr = ""
	}
	c.state.Readiness = ReadinessUnknown
	if status == OperationCanceled {
		c.state.Service = ServiceStopped
		c.state.LastFailureCode = ""
	} else {
		c.state.Service = ServiceFailed
		c.state.LastFailureCode = failureCodeForPhase(phase)
	}
	c.completeOperationLocked(op, status, stageErr)
}

func (c *Controller) cleanupRuntime(runtime Runtime, generation uint64) error {
	if runtime == nil {
		return nil
	}
	ctx, cancel := c.cleanupContext()
	err := runtime.Stop(ctx)
	cancel()
	if c.observer != nil && generation != 0 {
		err = errors.Join(err, c.observer.RuntimeDeactivated(context.WithoutCancel(c.rootCtx), generation))
	}
	return err
}

func (c *Controller) cleanupContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(c.rootCtx), c.stopTimeout)
}

func (c *Controller) monitorRuntime(runtime Runtime, generation uint64) {
	done := runtime.Done()
	if done == nil {
		return
	}
	err, ok := <-done
	if !ok {
		err = nil
	}

	c.mu.Lock()
	if c.runtime != runtime || c.state.RuntimeGeneration != generation {
		c.mu.Unlock()
		return
	}
	if c.state.Service == ServiceStopping {
		c.mu.Unlock()
		return
	}
	if c.active != nil && c.active.kind == OperationStart {
		c.active.cancel(&runtimeTerminatedError{err: err})
		c.mu.Unlock()
		return
	}
	// Keep ownership while Stop drains proxy workers and descendants. Start()
	// rejects replacements until cleanup returns.
	c.state.Service = ServiceFailed
	c.state.Readiness = ReadinessUnknown
	c.state.Addr = ""
	c.state.LastFailureCode = "listener_terminated"
	c.publishLocked()
	c.mu.Unlock()

	cleanupErr := c.cleanupRuntime(runtime, generation)
	c.mu.Lock()
	if c.runtime == runtime && c.state.RuntimeGeneration == generation {
		c.runtime = nil
		if cleanupErr != nil {
			c.state.LastFailureCode = "listener_cleanup_failed"
		}
		c.publishLocked()
	}
	c.mu.Unlock()
	_ = err // raw listener details stay outside protocol state
}

func (c *Controller) deactivateRuntime(runtime Runtime, generation uint64, service ServiceState, failure string) {
	if c.observer != nil {
		_ = c.observer.RuntimeDeactivated(context.WithoutCancel(c.rootCtx), generation)
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.runtime != runtime || c.state.RuntimeGeneration != generation {
		return
	}
	c.runtime = nil
	c.state.Service = service
	c.state.Readiness = ReadinessUnknown
	c.state.Addr = ""
	c.state.LastFailureCode = failure
	c.publishLocked()
}

func (c *Controller) setOperationPhase(op *activeOperation, phase OperationPhase) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != op {
		return
	}
	c.setOperationPhaseLocked(op, phase)
}

func (c *Controller) setOperationPhaseLocked(op *activeOperation, phase OperationPhase) {
	if c.state.Operation == nil || c.state.Operation.ID != op.id {
		return
	}
	c.state.Operation.Phase = phase
	c.publishLocked()
}

func (c *Controller) setAuth(op *activeOperation, state AuthState) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != op {
		return
	}
	c.state.Auth = state
	c.publishLocked()
}

func (c *Controller) completeOperationLocked(op *activeOperation, status OperationStatus, err error) {
	if c.active != op {
		return
	}
	if op.stopParent != nil {
		op.stopParent()
	}
	op.cancel(nil)
	c.active = nil
	c.state.Operation = nil
	c.publishLocked()
	result := OperationResult{
		ID:            op.id,
		Kind:          op.kind,
		Status:        status,
		StateRevision: c.state.Revision,
		Err:           err,
	}
	op.complete.Do(func() {
		op.completion.mu.Lock()
		op.completion.result = result
		op.completion.mu.Unlock()
		close(op.completion.done)
	})
}

func (c *Controller) publishLocked() {
	c.state.Revision++
	snapshot := cloneState(c.state)
	select {
	case c.updates <- snapshot:
	default:
		select {
		case <-c.updates:
		default:
		}
		c.updates <- snapshot
	}
}

func cloneState(state State) State {
	if state.Operation != nil {
		op := *state.Operation
		state.Operation = &op
	}
	return state
}

func failureCodeForPhase(phase OperationPhase) string {
	switch phase {
	case PhaseLoadingConfiguration:
		return "configuration_load_failed"
	case PhaseConstructingServer:
		return "server_construction_failed"
	case PhaseListenerStartup:
		return "listener_start_failed"
	case PhaseStartupAuthentication:
		return "authentication_failed"
	case PhaseDynamicProviderModelValidation:
		return "provider_validation_failed"
	case PhasePolicyRoutingPreflight:
		return "policy_preflight_failed"
	case PhaseReadinessCheck:
		return "readiness_failed"
	default:
		return "operation_failed"
	}
}

func randomOperationID() string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return fmt.Sprintf("op_%d", time.Now().UnixNano())
	}
	return "op_" + base64.RawURLEncoding.EncodeToString(raw[:])
}

type runtimeTerminatedError struct{ err error }

func (e *runtimeTerminatedError) Error() string {
	if e == nil || e.err == nil {
		return "runtime listener terminated during startup"
	}
	return fmt.Sprintf("runtime listener terminated during startup: %v", e.err)
}

func (e *runtimeTerminatedError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}
