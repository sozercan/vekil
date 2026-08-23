// Package appcontrol owns the platform-neutral lifecycle for an app-managed
// Vekil proxy runtime.
package appcontrol

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// ServiceState is the listener/runtime lifecycle axis.
type ServiceState string

const (
	ServiceStopped  ServiceState = "stopped"
	ServiceStarting ServiceState = "starting"
	ServiceRunning  ServiceState = "running"
	ServiceStopping ServiceState = "stopping"
	ServiceFailed   ServiceState = "failed"
)

// ReadinessState is independent from the service lifecycle.
type ReadinessState string

const (
	ReadinessUnknown  ReadinessState = "unknown"
	ReadinessChecking ReadinessState = "checking"
	ReadinessReady    ReadinessState = "ready"
	ReadinessNotReady ReadinessState = "not_ready"
	ReadinessStale    ReadinessState = "stale"
)

// AuthState describes the current app-controlled startup authentication state.
type AuthState string

const (
	AuthNotRequired AuthState = "not_required"
	AuthSignedOut   AuthState = "signed_out"
	AuthSigningIn   AuthState = "signing_in"
	AuthSignedIn    AuthState = "signed_in"
	AuthFailed      AuthState = "failed"
)

// OperationKind identifies a mutating controller operation.
type OperationKind string

const (
	OperationStart OperationKind = "start"
	OperationStop  OperationKind = "stop"
)

// OperationPhase uses the startup terminology shared with the native shell.
type OperationPhase string

const (
	PhaseLoadingConfiguration           OperationPhase = "loading_configuration"
	PhaseConstructingServer             OperationPhase = "constructing_server"
	PhaseListenerStartup                OperationPhase = "listener_startup"
	PhaseStartupAuthentication          OperationPhase = "startup_authentication"
	PhaseDynamicProviderModelValidation OperationPhase = "dynamic_provider_model_validation"
	PhasePolicyRoutingPreflight         OperationPhase = "policy_routing_preflight"
	PhaseReadinessCheck                 OperationPhase = "readiness_check"
	PhaseCleanup                        OperationPhase = "cleanup"
)

// OperationStatus is terminal operation state.
type OperationStatus string

const (
	OperationSucceeded OperationStatus = "succeeded"
	OperationCanceled  OperationStatus = "canceled"
	OperationFailed    OperationStatus = "failed"
)

// OperationState is the currently admitted mutation, if any.
type OperationState struct {
	ID    string         `json:"id"`
	Kind  OperationKind  `json:"kind"`
	Phase OperationPhase `json:"phase,omitempty"`
}

// State is a complete control-plane snapshot. Revision strictly increases for
// every published mutation. RuntimeGeneration increases only after a newly
// constructed Runtime has been allocated to a start attempt.
type State struct {
	Revision          uint64          `json:"state_revision"`
	RuntimeGeneration uint64          `json:"runtime_generation"`
	ConfigRevision    string          `json:"config_revision,omitempty"`
	SecretGeneration  uint64          `json:"secret_generation,omitempty"`
	Service           ServiceState    `json:"service"`
	Readiness         ReadinessState  `json:"readiness"`
	Auth              AuthState       `json:"auth"`
	Operation         *OperationState `json:"operation,omitempty"`
	Addr              string          `json:"addr,omitempty"`
	LastFailureCode   string          `json:"last_failure_code,omitempty"`
}

// Configuration is the exact validated snapshot used to construct a runtime.
// Value is intentionally opaque so appcontrol stays independent of proxy and
// macOS configuration packages.
type Configuration struct {
	Revision         string
	SecretGeneration uint64
	Value            any
}

// ConfigurationSource loads one exact configuration snapshot.
type ConfigurationSource interface {
	LoadConfiguration(context.Context) (Configuration, error)
}

// ConfigurationObserver records which selected configuration is active. Its
// methods must be idempotent for a runtime generation.
type ConfigurationObserver interface {
	RuntimeActivated(context.Context, Configuration, uint64, string) error
	RuntimeDeactivated(context.Context, uint64) error
}

// Runtime is the app-controlled server surface.
type Runtime interface {
	Start(context.Context) error
	Stop(context.Context) error
	Done() <-chan error
	Addr() string
	UsesCopilot() bool
	SetStartupAuthenticationPending(bool)
	ValidateDynamicProviderModels(context.Context) error
	InitializePolicyRouting(context.Context) error
}

// RuntimeFactory constructs, but does not start, one runtime.
type RuntimeFactory interface {
	NewRuntime(context.Context, Configuration) (Runtime, error)
}

// Authenticator supplies startup Copilot authentication.
type Authenticator interface {
	GetToken(context.Context) (string, error)
}

// ReadinessChecker validates the public readiness gate after startup preflight.
type ReadinessChecker interface {
	CheckReadiness(context.Context, string) error
}

// ReadinessCheckFunc adapts a function to ReadinessChecker.
type ReadinessCheckFunc func(context.Context, string) error

// CheckReadiness implements ReadinessChecker.
func (f ReadinessCheckFunc) CheckReadiness(ctx context.Context, addr string) error {
	if f == nil {
		return nil
	}
	return f(ctx, addr)
}

var (
	ErrClosed                 = errors.New("app controller is shut down")
	ErrOperationInProgress    = errors.New("another operation is still active")
	ErrAlreadyRunning         = errors.New("service is already running")
	ErrRuntimeCleanupPending  = errors.New("previous runtime cleanup is still in progress")
	ErrNotRunning             = errors.New("service is not running")
	ErrOperationNotFound      = errors.New("operation is not active")
	ErrOperationIDMismatch    = errors.New("operation id does not match the active operation")
	ErrOperationNotCancelable = errors.New("operation cannot be canceled")
	ErrConfigRevisionMismatch = errors.New("configuration revision changed")
)

// StageError identifies the startup phase that failed while retaining the
// internal error for logs and tests. Protocol adapters must sanitize it.
type StageError struct {
	Phase OperationPhase
	Err   error
}

func (e *StageError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s: %v", e.Phase, e.Err)
}

func (e *StageError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Err
}

// OperationResult is delivered exactly once after all required cleanup has
// completed and the controller has released mutation admission.
type OperationResult struct {
	ID            string
	Kind          OperationKind
	Status        OperationStatus
	StateRevision uint64
	Err           error
}

// Operation is an accepted asynchronous mutation.
type Operation struct {
	ID   string
	Kind OperationKind

	completion *operationCompletion
}

// Done closes after the terminal result has been recorded. It is safe for
// multiple waiters.
func (o Operation) Done() <-chan struct{} {
	if o.completion == nil {
		closed := make(chan struct{})
		close(closed)
		return closed
	}
	return o.completion.done
}

// Result returns the terminal result after Done closes.
func (o Operation) Result() OperationResult {
	if o.completion == nil {
		return OperationResult{}
	}
	o.completion.mu.Lock()
	defer o.completion.mu.Unlock()
	return o.completion.result
}

// Wait waits for the operation or caller cancellation. Caller cancellation
// does not itself cancel the controller operation; use CancelOperation.
func (o Operation) Wait(ctx context.Context) (OperationResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	select {
	case <-o.Done():
		return o.Result(), nil
	case <-ctx.Done():
		return OperationResult{}, ctx.Err()
	}
}

type activeOperation struct {
	id         string
	kind       OperationKind
	cancelable bool
	ctx        context.Context
	cancel     context.CancelCauseFunc
	completion *operationCompletion
	stopParent func() bool
	complete   sync.Once
}

type operationCompletion struct {
	mu     sync.Mutex
	done   chan struct{}
	result OperationResult
}

// Options configures a Controller.
type Options struct {
	ConfigurationSource   ConfigurationSource
	ConfigurationObserver ConfigurationObserver
	RuntimeFactory        RuntimeFactory
	Authenticator         Authenticator
	ReadinessChecker      ReadinessChecker
	OperationID           func() string
	StopTimeout           time.Duration
}
