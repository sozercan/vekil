package macosruntime

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/sozercan/vekil/internal/appcontrol"
)

type managedDraftPayload struct {
	ExpectedConfigRevision string       `json:"expected_config_revision,omitempty"`
	SecretGeneration       uint64       `json:"secret_generation"`
	Draft                  ManagedDraft `json:"draft"`
}

// ManagedApplyError preserves primary and rollback failures internally while
// protocol adapters expose only allowlisted codes.
type ManagedApplyError struct {
	Primary  error
	Rollback error
}

func (e *ManagedApplyError) Error() string {
	if e == nil {
		return ""
	}
	if e.Rollback != nil {
		return fmt.Sprintf("managed apply failed: %v; rollback failed: %v", e.Primary, e.Rollback)
	}
	return fmt.Sprintf("managed apply failed: %v", e.Primary)
}

func (e *ManagedApplyError) Unwrap() []error {
	if e == nil {
		return nil
	}
	var errs []error
	if e.Primary != nil {
		errs = append(errs, e.Primary)
	}
	if e.Rollback != nil {
		errs = append(errs, e.Rollback)
	}
	return errs
}

func (h *helper) validateManagedDraft(ctx context.Context, payload managedDraftPayload) error {
	candidate, err := h.opts.Configuration.BuildManagedCandidate(payload.Draft, payload.SecretGeneration)
	if err != nil {
		h.deleteUnrequiredSecretGeneration(payload.SecretGeneration)
		return err
	}
	defer h.deleteUnrequiredSecretProjection(candidate.Revision, candidate.SecretGeneration)
	return h.opts.CandidateValidator.ValidateManagedCandidate(ctx, candidate)
}

func (h *helper) applyManagedDraft(ctx context.Context, operationID string, payload managedDraftPayload) error {
	candidate, err := h.opts.Configuration.BuildManagedCandidate(payload.Draft, payload.SecretGeneration)
	if err != nil {
		h.deleteUnrequiredSecretGeneration(payload.SecretGeneration)
		return err
	}
	if err := h.opts.CandidateValidator.ValidateManagedCandidate(ctx, candidate); err != nil {
		h.deleteUnrequiredSecretProjection(candidate.Revision, candidate.SecretGeneration)
		return err
	}
	if err := ctx.Err(); err != nil {
		h.deleteUnrequiredSecretProjection(candidate.Revision, candidate.SecretGeneration)
		return err
	}

	wasRunning := h.opts.Controller.Snapshot().Service == appcontrol.ServiceRunning
	tx, err := h.opts.Configuration.PrepareManagedApply(operationID, candidate, payload.ExpectedConfigRevision, wasRunning)
	if err != nil {
		h.deleteUnrequiredSecretProjection(candidate.Revision, candidate.SecretGeneration)
		return err
	}
	rollbackStopped := func(primary error) error {
		rollbackErr := tx.Rollback(failureCode(primary))
		if rollbackErr == nil {
			h.opts.Secrets.Delete(candidate.Revision, candidate.SecretGeneration)
		}
		return &ManagedApplyError{Primary: primary, Rollback: rollbackErr}
	}

	if !wasRunning {
		if err := tx.Install(); err != nil {
			return rollbackStopped(err)
		}
		if err := h.commitManagedApply(ctx, tx); err != nil {
			return rollbackStopped(err)
		}
		h.opts.Secrets.Delete(tx.journal.OldRevision, tx.journal.OldSecretGeneration)
		return nil
	}

	if h.beforeControllerStop != nil {
		h.beforeControllerStop()
	}
	stop, err := h.opts.Controller.Stop(ctx)
	stopAdmitted := err == nil
	if stopAdmitted {
		var result appcontrol.OperationResult
		result, err = waitControllerCleanup(h.opts.Controller, stop, h.opts.ShutdownTimeout)
		if err == nil && result.Status != appcontrol.OperationSucceeded {
			err = result.Err
		}
	}
	if err != nil {
		if errors.Is(err, appcontrol.ErrNotRunning) ||
			(stopAdmitted && h.opts.Controller.Snapshot().Service != appcontrol.ServiceRunning) {
			return h.restoreManagedRunningIntent(ctx, tx, candidate, err, false)
		}
		return rollbackStopped(err)
	}
	if err := ctx.Err(); err != nil {
		return h.restoreManagedRunningIntent(ctx, tx, candidate, err, false)
	}
	if err := tx.Install(); err != nil {
		return h.restoreManagedRunningIntent(ctx, tx, candidate, err, false)
	}

	start, err := h.opts.Controller.Start(ctx, candidate.Revision)
	if err == nil {
		var result appcontrol.OperationResult
		result, err = waitControllerCleanup(h.opts.Controller, start, h.opts.ShutdownTimeout)
		if err == nil && result.Status != appcontrol.OperationSucceeded {
			err = result.Err
		}
	}
	if err != nil {
		return h.restoreManagedRunningIntent(ctx, tx, candidate, err, false)
	}

	if err := h.commitManagedApply(ctx, tx); err != nil {
		return h.restoreManagedRunningIntent(ctx, tx, candidate, err, true)
	}
	h.opts.Secrets.Delete(tx.journal.OldRevision, tx.journal.OldSecretGeneration)
	return nil
}

func (h *helper) deleteUnrequiredSecretGeneration(generation uint64) {
	description, err := h.opts.Configuration.Describe()
	if err != nil {
		return
	}
	for _, required := range description.SecretProjections {
		if required.SecretGeneration == generation {
			return
		}
	}
	h.opts.Secrets.DeleteGeneration(generation)
}

func (h *helper) deleteUnrequiredSecretProjection(revision string, generation uint64) {
	description, err := h.opts.Configuration.Describe()
	if err != nil {
		return
	}
	for _, required := range description.SecretProjections {
		if required.ConfigRevision == revision && required.SecretGeneration == generation {
			return
		}
	}
	h.opts.Secrets.Delete(revision, generation)
}

func (h *helper) commitManagedApply(ctx context.Context, tx *ManagedApplyTransaction) error {
	if h.beforeManagedCommit != nil {
		h.beforeManagedCommit()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if h.managedCommit != nil {
		return h.managedCommit(tx)
	}
	return tx.Commit()
}

func (h *helper) restoreManagedRunningIntent(ctx context.Context, tx *ManagedApplyTransaction, candidate ManagedCandidate, primary error, candidateRunning bool) error {
	if candidateRunning {
		stopCtx, stopCancel := context.WithTimeout(context.WithoutCancel(ctx), h.opts.ShutdownTimeout)
		stop, stopErr := h.opts.Controller.Stop(stopCtx)
		if stopErr == nil {
			var result appcontrol.OperationResult
			result, stopErr = waitControllerCleanup(h.opts.Controller, stop, h.opts.ShutdownTimeout)
			if stopErr == nil && result.Status != appcontrol.OperationSucceeded {
				stopErr = result.Err
			}
		}
		stopCancel()
		if stopErr != nil {
			_ = tx.MarkRollbackFailed(failureCode(primary), "candidate_stop_failed")
			return &ManagedApplyError{Primary: primary, Rollback: stopErr}
		}
	}

	if restoreErr := tx.RestoreForRuntimeRollback(failureCode(primary)); restoreErr != nil {
		_ = tx.MarkRollbackFailed(failureCode(primary), "restore_failed")
		return &ManagedApplyError{Primary: primary, Rollback: restoreErr}
	}

	rollbackCtx, rollbackCancel := context.WithTimeout(context.WithoutCancel(ctx), h.opts.ShutdownTimeout)
	defer rollbackCancel()
	rollbackStart, rollbackErr := h.opts.Controller.Start(rollbackCtx, tx.journal.OldRevision)
	if rollbackErr == nil {
		var result appcontrol.OperationResult
		result, rollbackErr = waitControllerCleanup(h.opts.Controller, rollbackStart, h.opts.ShutdownTimeout)
		if rollbackErr == nil && result.Status != appcontrol.OperationSucceeded {
			rollbackErr = result.Err
		}
	}
	if rollbackErr != nil {
		_ = tx.MarkRollbackFailed(failureCode(primary), "rollback_start_failed")
		return &ManagedApplyError{Primary: primary, Rollback: rollbackErr}
	}
	if finishErr := tx.FinishRuntimeRollback(); finishErr != nil {
		_ = tx.MarkRollbackFailed(failureCode(primary), "rollback_commit_failed")
		return &ManagedApplyError{Primary: primary, Rollback: finishErr}
	}
	h.opts.Secrets.Delete(candidate.Revision, candidate.SecretGeneration)
	return &ManagedApplyError{Primary: primary}
}
func waitControllerCleanup(controller *appcontrol.Controller, operation appcontrol.Operation, timeout time.Duration) (appcontrol.OperationResult, error) {
	if timeout <= 0 {
		timeout = defaultHelperShutdownTimeout
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	result, err := operation.Wait(ctx)
	cancel()
	if err == nil || !errors.Is(err, context.DeadlineExceeded) {
		return result, err
	}

	// A timeout only bounds the initial wait. Rollback must never mutate the
	// configuration while controller admission is still occupied. Starts are
	// cancelable, so request cancellation and then wait through runtime cleanup.
	// Stops are deliberately non-cancelable and must reach their terminal result.
	var cancelErr error
	if operation.Kind == appcontrol.OperationStart {
		cancelErr = controller.CancelOperation(operation.ID)
		if errors.Is(cancelErr, appcontrol.ErrOperationNotFound) {
			cancelErr = nil
		}
	}
	result, terminalErr := operation.Wait(context.Background())
	if cancelErr != nil || terminalErr != nil {
		return result, errors.Join(cancelErr, terminalErr)
	}
	return result, nil
}

func failureCode(err error) string {
	if err == nil {
		return "operation_failed"
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return "operation_canceled"
	}
	var stage *appcontrol.StageError
	if errors.As(err, &stage) {
		return failureCodeForManagedStage(stage.Phase)
	}
	return "operation_failed"
}

func failureCodeForManagedStage(phase appcontrol.OperationPhase) string {
	switch phase {
	case appcontrol.PhaseLoadingConfiguration:
		return "configuration_load_failed"
	case appcontrol.PhaseConstructingServer:
		return "server_construction_failed"
	case appcontrol.PhaseListenerStartup:
		return "listener_start_failed"
	case appcontrol.PhaseStartupAuthentication:
		return "authentication_failed"
	case appcontrol.PhaseDynamicProviderModelValidation:
		return "provider_validation_failed"
	case appcontrol.PhasePolicyRoutingPreflight:
		return "policy_preflight_failed"
	case appcontrol.PhaseReadinessCheck:
		return "readiness_failed"
	default:
		return "operation_failed"
	}
}
