package macosruntime

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/internal/appcontrol"
)

const defaultHelperShutdownTimeout = appcontrol.DefaultStopTimeout + time.Second

// HelperAuthenticator is the authentication surface exposed by the helper.
type HelperAuthenticator interface {
	appcontrol.Authenticator
	Status() auth.AuthStatus
	RequestDeviceCode(context.Context) (*auth.DeviceCodeResponse, error)
	PollForAuthorization(context.Context, *auth.DeviceCodeResponse) error
	SignInWithGitHubCLI(context.Context) error
	SignOut() error
}

// ManagedCandidateValidator validates exact candidate bytes using the staged
// provider-scoped secret resolver and explicit dynamic discovery.
type ManagedCandidateValidator interface {
	ValidateManagedCandidate(context.Context, ManagedCandidate) error
}

// HelperOptions configures one helper process protocol session.
type HelperOptions struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer

	Controller         *appcontrol.Controller
	Configuration      *ConfigManager
	Secrets            *SecretProjectionStore
	Authenticator      HelperAuthenticator
	CandidateValidator ManagedCandidateValidator

	ProtocolMin   int
	ProtocolMax   int
	HelperBuild   string
	BundleBuildID string
	HelperEpoch   string
	PID           int
	ParentPID     int

	ShutdownTimeout time.Duration
	ParentPoll      time.Duration
}

type helloPayload struct {
	ProtocolMin   int    `json:"protocol_min"`
	ProtocolMax   int    `json:"protocol_max"`
	HelperBuild   string `json:"helper_build"`
	BundleBuildID string `json:"bundle_build_id"`
	PID           int    `json:"pid"`
	HelperEpoch   string `json:"helper_epoch"`
}

type operationPayload struct {
	OperationID string         `json:"operation_id"`
	Kind        string         `json:"kind,omitempty"`
	Status      string         `json:"status"`
	Error       *ProtocolError `json:"error,omitempty"`
}

type deviceCodePayload struct {
	OperationID     string `json:"operation_id"`
	VerificationURL string `json:"verification_url"`
	UserCode        string `json:"user_code"`
	ExpiresIn       int    `json:"expires_in"`
}

type runtimeOperationState struct {
	ID    string `json:"id"`
	Kind  string `json:"kind"`
	Phase string `json:"phase,omitempty"`
}

type runtimeStatePayload struct {
	Helper            string                    `json:"helper"`
	Service           appcontrol.ServiceState   `json:"service"`
	Readiness         appcontrol.ReadinessState `json:"readiness"`
	Auth              appcontrol.AuthState      `json:"auth"`
	AuthSource        string                    `json:"auth_source,omitempty"`
	Operation         *runtimeOperationState    `json:"operation,omitempty"`
	RuntimeGeneration uint64                    `json:"runtime_generation"`
	ConfigRevision    string                    `json:"config_revision,omitempty"`
	SecretGeneration  uint64                    `json:"secret_generation,omitempty"`
	Addr              string                    `json:"addr,omitempty"`
	LastFailureCode   string                    `json:"last_failure_code,omitempty"`
	Configuration     ConfigDescription         `json:"configuration"`
}

type helper struct {
	opts                  HelperOptions
	epoch                 string
	writer                *protocolWriter
	cache                 requestCache
	operations            operationCoordinator
	projector             stateProjector
	managedCommit         func(*ManagedApplyTransaction) error
	beforeManagedCommit   func()
	beforeSelectionCommit func()
	beforeControllerStop  func()
}

type frameResult struct {
	frame []byte
	err   error
}

// RunHelper runs one JSONL helper session. Stdout is reserved exclusively for
// protocol frames; callers should route logs to opts.Stderr.
func RunHelper(parent context.Context, opts HelperOptions) error {
	if parent == nil {
		parent = context.Background()
	}
	if opts.Stdin == nil {
		opts.Stdin = os.Stdin
	}
	if opts.Stdout == nil {
		opts.Stdout = os.Stdout
	}
	if opts.Stderr == nil {
		opts.Stderr = os.Stderr
	}
	if opts.Controller == nil {
		return errors.New("controller is required")
	}
	if opts.Configuration == nil {
		return errors.New("configuration manager is required")
	}
	if opts.Secrets == nil {
		opts.Secrets = NewSecretProjectionStore()
	}
	if opts.ProtocolMin == 0 {
		opts.ProtocolMin = ProtocolMin
	}
	if opts.ProtocolMax == 0 {
		opts.ProtocolMax = ProtocolMax
	}
	if opts.ProtocolMin > opts.ProtocolMax {
		return errors.New("protocol minimum exceeds maximum")
	}
	if opts.HelperEpoch == "" {
		opts.HelperEpoch = randomOpaqueID("hep_")
	}
	if opts.PID == 0 {
		opts.PID = os.Getpid()
	}
	if opts.ShutdownTimeout <= 0 {
		opts.ShutdownTimeout = defaultHelperShutdownTimeout
	}
	if opts.ParentPoll <= 0 {
		opts.ParentPoll = time.Second
	}

	ctx, cancel := context.WithCancelCause(parent)
	defer cancel(nil)
	h := &helper{opts: opts, epoch: opts.HelperEpoch, writer: newProtocolWriter(opts.Stdout)}
	hello := eventEnvelope{Version: ProtocolMax, Event: "hello", HelperEpoch: h.epoch, Payload: helloPayload{
		ProtocolMin: opts.ProtocolMin, ProtocolMax: opts.ProtocolMax, HelperBuild: opts.HelperBuild,
		BundleBuildID: opts.BundleBuildID, PID: opts.PID, HelperEpoch: h.epoch,
	}}
	if err := h.writer.SendCritical(ctx, hello); err != nil {
		return err
	}
	if err := h.publishState(); err != nil {
		return err
	}

	pumpDone := make(chan struct{})
	go func() {
		defer close(pumpDone)
		for {
			select {
			case <-ctx.Done():
				return
			case _, ok := <-opts.Controller.Updates():
				if !ok {
					return
				}
				_ = h.publishState()
			}
		}
	}()
	if opts.ParentPID > 0 {
		go monitorParent(ctx, opts.ParentPID, opts.ParentPoll, func() { cancel(errParentExited) })
	}

	frames := make(chan frameResult, 1)
	reader := newFrameReader(opts.Stdin)
	go func() {
		for {
			frame, err := reader.ReadFrame()
			select {
			case frames <- frameResult{frame: frame, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var runErr error
	for runErr == nil {
		select {
		case <-ctx.Done():
			runErr = context.Cause(ctx)
		case result := <-frames:
			if errors.Is(result.err, io.EOF) {
				runErr = io.EOF
				break
			}
			if result.err != nil {
				runErr = result.err
				break
			}
			shutdown, err := h.handleFrame(ctx, result.frame)
			if err != nil {
				runErr = err
			} else if shutdown {
				runErr = errShutdownRequested
			}
		}
	}

	cancel(runErr)
	if closer, ok := opts.Stdin.(io.Closer); ok {
		_ = closer.Close()
	}
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), opts.ShutdownTimeout)
	h.operations.cancelForShutdown(opts.Controller)
	shutdownErr := opts.Controller.Shutdown(shutdownCtx)
	operationErr := h.operations.wait(shutdownCtx)
	shutdownCancel()
	<-pumpDone
	closeCtx, closeCancel := context.WithTimeout(context.Background(), opts.ShutdownTimeout)
	writerErr := h.writer.Close(closeCtx)
	closeCancel()

	if errors.Is(runErr, io.EOF) || errors.Is(runErr, errShutdownRequested) || errors.Is(runErr, context.Canceled) || errors.Is(runErr, errParentExited) {
		runErr = nil
	}
	return errors.Join(runErr, shutdownErr, operationErr, writerErr)
}

var (
	errShutdownRequested = errors.New("shutdown requested")
	errParentExited      = errors.New("parent process exited")
)

func (h *helper) handleFrame(ctx context.Context, frame []byte) (bool, error) {
	request, err := decodeRequestEnvelope(frame)
	if err != nil {
		return false, err
	}
	if cached, found, conflict, err := h.cache.lookup(request); err != nil {
		return false, err
	} else if found {
		if conflict {
			response := h.errorResponse(request.ID, protocolError("request_id_conflict", "This request ID was already used for a different command.", false, "retry_with_new_request_id"))
			return false, h.writer.SendCritical(ctx, response)
		}
		return false, h.writer.SendCritical(ctx, cached)
	}
	response, postSend, shutdown := h.dispatch(ctx, request)
	if err := h.cache.store(request, response); err != nil {
		return false, err
	}
	if err := h.writer.SendCritical(ctx, response); err != nil {
		return false, err
	}
	if postSend != nil {
		postSend()
	}
	return shutdown, nil
}

func (h *helper) dispatch(ctx context.Context, request requestEnvelope) (responseEnvelope, func(), bool) {
	success := func(result any) responseEnvelope {
		return responseEnvelope{Version: ProtocolMax, ID: request.ID, HelperEpoch: h.epoch, OK: true, Result: result}
	}
	failure := func(err *ProtocolError) responseEnvelope { return h.errorResponse(request.ID, err) }

	switch request.Command {
	case "get_state":
		if err := decodePayload(request.Payload, &struct{}{}); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		revision, payload := h.projector.current(h)
		return success(map[string]any{"state_revision": revision, "payload": payload}), nil, false
	case "describe_config":
		if err := decodePayload(request.Payload, &struct{}{}); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		description, _ := h.opts.Configuration.Describe()
		return success(description), nil, false
	case "start":
		var payload struct {
			ExpectedConfigRevision         string `json:"expected_config_revision"`
			AllowInteractiveAuthentication *bool  `json:"allows_interactive_authentication"`
			Reason                         string `json:"reason"`
		}
		if err := decodePayload(request.Payload, &payload); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		allowInteractiveAuthentication := true
		if payload.AllowInteractiveAuthentication != nil {
			allowInteractiveAuthentication = *payload.AllowInteractiveAuthentication
		}
		switch payload.Reason {
		case "", "userInitiated":
		case "automaticLaunch":
			if allowInteractiveAuthentication {
				return failure(invalidPayloadError()), nil, false
			}
		default:
			return failure(invalidPayloadError()), nil, false
		}
		op, err := h.operations.beginController(ctx, "start", h.opts.Controller, func() (appcontrol.Operation, error) {
			return h.opts.Controller.StartWithOptions(ctx, appcontrol.StartOptions{
				ExpectedConfigRevision:         strings.TrimSpace(payload.ExpectedConfigRevision),
				AllowInteractiveAuthentication: allowInteractiveAuthentication,
			})
		})
		if err != nil {
			return failure(mapProtocolError(err)), nil, false
		}
		return success(map[string]any{"accepted": true, "operation_id": op.ID}), func() { go h.waitControllerOperation(op) }, false
	case "stop":
		if err := decodePayload(request.Payload, &struct{}{}); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		op, err := h.operations.beginController(ctx, "stop", h.opts.Controller, func() (appcontrol.Operation, error) {
			return h.opts.Controller.Stop(ctx)
		})
		if err != nil {
			return failure(mapProtocolError(err)), nil, false
		}
		return success(map[string]any{"accepted": true, "operation_id": op.ID}), func() { go h.waitControllerOperation(op) }, false
	case "cancel_operation":
		var payload struct {
			OperationID string `json:"operation_id"`
		}
		if err := decodePayload(request.Payload, &payload); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		if err := h.operations.cancel(strings.TrimSpace(payload.OperationID), h.opts.Controller); err != nil {
			return failure(mapProtocolError(err)), nil, false
		}
		return success(map[string]any{"cancellation_requested": true, "operation_id": payload.OperationID}), nil, false
	case "set_secret_projection":
		var projection SecretProjection
		if err := decodePayload(request.Payload, &projection); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		if err := h.opts.Secrets.Set(projection); err != nil {
			return failure(protocolError("invalid_secret_projection", "The managed secret projection is invalid.", false, "open_providers")), nil, false
		}
		return success(map[string]any{"accepted": true, "config_revision": projection.ConfigRevision, "secret_generation": projection.SecretGeneration}), nil, false
	case "ensure_managed_config":
		if err := decodePayload(request.Payload, &struct{}{}); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		op, err := h.operations.beginAsync(ctx, "ensure_managed_config", true)
		if err != nil {
			return failure(mapProtocolError(err)), nil, false
		}
		return success(map[string]any{"accepted": true, "operation_id": op.id}), func() {
			_ = h.publishState()
			go h.runConfigSelectionOperation(op, func(context.Context) error {
				_, err := h.opts.Configuration.EnsureManagedConfiguration()
				return err
			})
		}, false
	case "select_external_config":
		var payload struct {
			Path string `json:"path"`
		}
		if err := decodePayload(request.Payload, &payload); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		op, err := h.operations.beginAsync(ctx, "select_external_config", true)
		if err != nil {
			return failure(mapProtocolError(err)), nil, false
		}
		return success(map[string]any{"accepted": true, "operation_id": op.id}), func() {
			_ = h.publishState()
			go h.runConfigSelectionOperation(op, func(opCtx context.Context) error {
				return h.opts.Configuration.StageExternal(opCtx, payload.Path)
			})
		}, false
	case "reload_external_config":
		if err := decodePayload(request.Payload, &struct{}{}); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		op, err := h.operations.beginAsync(ctx, "reload_external_config", true)
		if err != nil {
			return failure(mapProtocolError(err)), nil, false
		}
		return success(map[string]any{"accepted": true, "operation_id": op.id}), func() {
			_ = h.publishState()
			go h.runConfigSelectionOperation(op, func(opCtx context.Context) error {
				return h.opts.Configuration.StageReloadExternal(opCtx)
			})
		}, false
	case "use_managed_config":
		if err := decodePayload(request.Payload, &struct{}{}); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		op, err := h.operations.beginAsync(ctx, "use_managed_config", true)
		if err != nil {
			return failure(mapProtocolError(err)), nil, false
		}
		return success(map[string]any{"accepted": true, "operation_id": op.id}), func() {
			_ = h.publishState()
			go h.runConfigSelectionOperation(op, func(context.Context) error {
				return h.opts.Configuration.StagePreferredAppConfiguration()
			})
		}, false
	case "validate_managed_draft":
		var payload managedDraftPayload
		if err := decodePayload(request.Payload, &payload); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		if h.opts.CandidateValidator == nil {
			return failure(protocolError("validation_unavailable", "Managed provider validation is unavailable.", false, "restart_helper")), nil, false
		}
		op, err := h.operations.beginAsync(ctx, "validate_managed_draft", true)
		if err != nil {
			return failure(mapProtocolError(err)), nil, false
		}
		return success(map[string]any{"accepted": true, "operation_id": op.id}), func() {
			_ = h.publishState()
			go h.runConfigOperation(op, func(opCtx context.Context) error {
				return h.validateManagedDraft(opCtx, payload)
			})
		}, false
	case "apply_managed_draft":
		var payload managedDraftPayload
		if err := decodePayload(request.Payload, &payload); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		if h.opts.CandidateValidator == nil {
			return failure(protocolError("validation_unavailable", "Managed provider validation is unavailable.", false, "restart_helper")), nil, false
		}
		op, err := h.operations.beginAsync(ctx, "apply_managed_draft", true)
		if err != nil {
			return failure(mapProtocolError(err)), nil, false
		}
		return success(map[string]any{"accepted": true, "operation_id": op.id}), func() {
			_ = h.publishState()
			go h.runConfigOperation(op, func(opCtx context.Context) error {
				return h.applyManagedDraft(opCtx, op.id, payload)
			})
		}, false
	case "auth_github_cli":
		if h.opts.Authenticator == nil {
			return failure(protocolError("auth_unavailable", "GitHub authentication is unavailable.", false, "restart_helper")), nil, false
		}
		if err := decodePayload(request.Payload, &struct{}{}); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		op, err := h.operations.beginAsync(ctx, "auth_github_cli", true)
		if err != nil {
			return failure(mapProtocolError(err)), nil, false
		}
		return success(map[string]any{"accepted": true, "operation_id": op.id}), func() {
			_ = h.publishState()
			go h.runConfigOperation(op, h.opts.Authenticator.SignInWithGitHubCLI)
		}, false
	case "auth_device_start":
		if h.opts.Authenticator == nil {
			return failure(protocolError("auth_unavailable", "GitHub authentication is unavailable.", false, "restart_helper")), nil, false
		}
		if err := decodePayload(request.Payload, &struct{}{}); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		op, err := h.operations.beginAsync(ctx, "auth_device_start", true)
		if err != nil {
			return failure(mapProtocolError(err)), nil, false
		}
		return success(map[string]any{"accepted": true, "operation_id": op.id}), func() {
			_ = h.publishState()
			go h.runDeviceAuth(op)
		}, false
	case "auth_sign_out":
		if h.opts.Authenticator == nil {
			return failure(protocolError("auth_unavailable", "GitHub authentication is unavailable.", false, "restart_helper")), nil, false
		}
		if err := decodePayload(request.Payload, &struct{}{}); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		op, err := h.operations.beginAsync(ctx, "auth_sign_out", false)
		if err != nil {
			return failure(mapProtocolError(err)), nil, false
		}
		return success(map[string]any{"accepted": true, "operation_id": op.id}), func() {
			_ = h.publishState()
			go h.runConfigOperation(op, h.signOut)
		}, false
	case "shutdown":
		if err := decodePayload(request.Payload, &struct{}{}); err != nil {
			return failure(invalidPayloadError()), nil, false
		}
		return success(map[string]any{"accepted": true}), nil, true
	default:
		return failure(protocolError("unknown_command", "The helper does not recognize this command.", false, "update_app")), nil, false
	}
}

func (h *helper) waitControllerOperation(operation appcontrol.Operation) {
	result, _ := operation.Wait(context.Background())
	coordinated := h.operations.finish(operation.ID)
	var protocolErr *ProtocolError
	if result.Status == appcontrol.OperationFailed {
		protocolErr = mapProtocolError(result.Err)
	}
	h.emitTerminal(operation.ID, string(operation.Kind), string(result.Status), protocolErr)
	h.operations.completeOperation(coordinated)
}

func (h *helper) runConfigOperation(operation *coordinatedOperation, fn func(context.Context) error) {
	err := fn(operation.ctx)
	status := string(appcontrol.OperationSucceeded)
	var protocolErr *ProtocolError
	if err != nil {
		if operation.ctx.Err() != nil && !hasRollbackFailure(err) {
			status = string(appcontrol.OperationCanceled)
		} else {
			status = string(appcontrol.OperationFailed)
			protocolErr = mapProtocolError(err)
		}
	}
	coordinated := h.operations.finish(operation.id)
	h.emitTerminal(operation.id, operation.kind, status, protocolErr)
	h.operations.completeOperation(coordinated)
}

func hasRollbackFailure(err error) bool {
	var managed *ManagedApplyError
	if errors.As(err, &managed) && managed.Rollback != nil {
		return true
	}
	var selection *ConfigSwitchError
	return errors.As(err, &selection) && selection.Rollback != nil
}

func (h *helper) runDeviceAuth(operation *coordinatedOperation) {
	dc, err := h.opts.Authenticator.RequestDeviceCode(operation.ctx)
	if err == nil {
		err = h.projector.publishCriticalPair(operation.ctx, h, func(eventRevision uint64) eventEnvelope {
			return eventEnvelope{Version: ProtocolMax, Event: "device_code", HelperEpoch: h.epoch, StateRevision: eventRevision, Payload: deviceCodePayload{
				OperationID: operation.id, VerificationURL: dc.VerificationURI, UserCode: dc.UserCode, ExpiresIn: dc.ExpiresIn,
			}}
		})
	}
	if err == nil {
		err = h.opts.Authenticator.PollForAuthorization(operation.ctx, dc)
	}
	status := string(appcontrol.OperationSucceeded)
	var protocolErr *ProtocolError
	if err != nil {
		if operation.ctx.Err() != nil {
			status = string(appcontrol.OperationCanceled)
		} else {
			status = string(appcontrol.OperationFailed)
			protocolErr = mapProtocolError(err)
		}
	}
	coordinated := h.operations.finish(operation.id)
	h.emitTerminal(operation.id, operation.kind, status, protocolErr)
	h.operations.completeOperation(coordinated)
}

func (h *helper) emitTerminal(operationID, kind, status string, protocolErr *ProtocolError) {
	ctx, cancel := context.WithTimeout(context.Background(), h.opts.ShutdownTimeout)
	_ = h.projector.publishCriticalPair(ctx, h, func(eventRevision uint64) eventEnvelope {
		return eventEnvelope{Version: ProtocolMax, Event: "operation", HelperEpoch: h.epoch, StateRevision: eventRevision, Payload: operationPayload{
			OperationID: operationID, Kind: kind, Status: status, Error: protocolErr,
		}}
	})
	cancel()
}

func (h *helper) publishState() error {
	return h.projector.publishState(h)
}

func (h *helper) errorResponse(id string, err *ProtocolError) responseEnvelope {
	return responseEnvelope{Version: ProtocolMax, ID: id, HelperEpoch: h.epoch, OK: false, Error: err}
}

func protocolError(code, message string, retryable bool, recovery string) *ProtocolError {
	return &ProtocolError{Code: code, UserMessage: message, Retryable: retryable, RecoveryAction: recovery, FieldErrors: []FieldError{}}
}

func invalidPayloadError() *ProtocolError {
	return protocolError("invalid_payload", "The command payload is invalid.", false, "retry_command")
}

func mapProtocolError(err error) *ProtocolError {
	if err == nil {
		return nil
	}
	if hasRollbackFailure(err) {
		return protocolError("rollback_failed", "The previous configuration could not be fully restored.", false, "open_recovery")
	}
	switch {
	case errors.Is(err, appcontrol.ErrOperationInProgress):
		return protocolError("operation_in_progress", "Another operation is still finishing.", true, "wait")
	case errors.Is(err, appcontrol.ErrAlreadyRunning):
		return protocolError("already_running", "The proxy is already running.", false, "none")
	case errors.Is(err, appcontrol.ErrRuntimeCleanupPending):
		return protocolError("cleanup_in_progress", "The previous proxy runtime is still cleaning up.", true, "wait")
	case errors.Is(err, appcontrol.ErrNotRunning):
		return protocolError("not_running", "The proxy is not running.", false, "none")
	case errors.Is(err, appcontrol.ErrOperationIDMismatch), errors.Is(err, appcontrol.ErrOperationNotFound):
		return protocolError("stale_operation", "That operation is no longer active.", false, "refresh_state")
	case errors.Is(err, appcontrol.ErrOperationNotCancelable):
		return protocolError("operation_not_cancelable", "The active operation cannot be canceled.", false, "wait")
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return protocolError("operation_canceled", "The operation was canceled.", true, "retry_command")
	}
	var applyErr *ManagedApplyError
	if errors.As(err, &applyErr) {
		return protocolError("managed_apply_failed", "The managed configuration was not applied. The previous configuration was restored.", true, "open_providers")
	}
	var stage *appcontrol.StageError
	if errors.As(err, &stage) {
		switch stage.Phase {
		case appcontrol.PhaseLoadingConfiguration:
			return protocolError("invalid_config", "The provider configuration could not be loaded.", false, "open_providers")
		case appcontrol.PhaseConstructingServer:
			return protocolError("server_construction_failed", "Vekil could not initialize the proxy.", true, "retry_start")
		case appcontrol.PhaseListenerStartup:
			return protocolError("listener_start_failed", "Vekil could not bind the proxy listener.", true, "retry_start")
		case appcontrol.PhaseStartupAuthentication:
			return protocolError("authentication_failed", "GitHub Copilot authentication failed.", true, "open_auth")
		case appcontrol.PhaseDynamicProviderModelValidation:
			return protocolError("provider_validation_failed", "Provider model validation failed.", true, "retry_start")
		case appcontrol.PhasePolicyRoutingPreflight:
			return protocolError("policy_preflight_failed", "Policy routing preflight failed.", true, "open_providers")
		case appcontrol.PhaseReadinessCheck:
			return protocolError("readiness_failed", "The proxy did not become ready.", true, "retry_start")
		}
	}
	return protocolError("operation_failed", "The operation failed.", true, "retry_command")
}
