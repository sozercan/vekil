package macosruntime

import (
	"context"
	"errors"
	"fmt"

	"github.com/sozercan/vekil/internal/appcontrol"
)

// ConfigSwitchError retains both the primary switch failure and a possible
// restoration failure internally. Protocol mapping remains allowlisted.
type ConfigSwitchError struct {
	Primary  error
	Rollback error
}

func (e *ConfigSwitchError) Error() string {
	if e == nil {
		return ""
	}
	if e.Rollback != nil {
		return fmt.Sprintf("configuration switch failed: %v; restore failed: %v", e.Primary, e.Rollback)
	}
	return fmt.Sprintf("configuration switch failed: %v", e.Primary)
}

func (e *ConfigSwitchError) Unwrap() []error {
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

// runConfigSelectionOperation validates and records a selection, then restores
// the prior running/stopped intent. Invalid selections never stop the current
// runtime; later failures restore the prior selected configuration.
func (h *helper) runConfigSelectionOperation(operation *coordinatedOperation, selectConfiguration func(context.Context) error) {
	h.runConfigOperation(operation, func(ctx context.Context) error {
		return h.switchSelectedConfiguration(ctx, selectConfiguration)
	})
}

func (h *helper) switchSelectedConfiguration(ctx context.Context, selectConfiguration func(context.Context) error) error {
	previous := h.opts.Configuration.State()
	h.opts.Configuration.beginSelectionSwitch(previous)
	defer h.opts.Configuration.endSelectionSwitch()
	wasRunning := h.opts.Controller.Snapshot().Service == appcontrol.ServiceRunning
	if err := selectConfiguration(ctx); err != nil {
		return err
	}
	if !wasRunning {
		if err := h.commitSelection(ctx); err != nil {
			return &ConfigSwitchError{Primary: err, Rollback: h.opts.Configuration.RestoreSelection(previous)}
		}
		return nil
	}
	if err := ctx.Err(); err != nil {
		return &ConfigSwitchError{Primary: err, Rollback: h.opts.Configuration.RestoreSelection(previous)}
	}

	stop, err := h.opts.Controller.Stop(ctx)
	if err == nil {
		var result appcontrol.OperationResult
		result, err = waitControllerCleanup(h.opts.Controller, stop, h.opts.ShutdownTimeout)
		if err == nil && result.Status != appcontrol.OperationSucceeded {
			err = result.Err
		}
	}
	if err != nil {
		primary := err
		if restoreErr := h.opts.Configuration.RestoreSelection(previous); restoreErr != nil {
			return &ConfigSwitchError{Primary: primary, Rollback: restoreErr}
		}
		if h.opts.Controller.Snapshot().Service == appcontrol.ServiceRunning {
			return &ConfigSwitchError{Primary: primary}
		}

		rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.opts.ShutdownTimeout)
		defer cancel()
		rollbackStart, rollbackErr := h.opts.Controller.Start(rollbackCtx, previous.SelectedConfigRevision)
		if rollbackErr == nil {
			var result appcontrol.OperationResult
			result, rollbackErr = waitControllerCleanup(h.opts.Controller, rollbackStart, h.opts.ShutdownTimeout)
			if rollbackErr == nil && result.Status != appcontrol.OperationSucceeded {
				rollbackErr = result.Err
			}
		}
		return &ConfigSwitchError{Primary: primary, Rollback: rollbackErr}
	}

	selected := h.opts.Configuration.State()
	start, err := h.opts.Controller.Start(ctx, selected.SelectedConfigRevision)
	if err == nil {
		var result appcontrol.OperationResult
		result, err = waitControllerCleanup(h.opts.Controller, start, h.opts.ShutdownTimeout)
		if err == nil && result.Status != appcontrol.OperationSucceeded {
			err = result.Err
		}
	}
	if err == nil {
		err = h.commitSelection(ctx)
	}
	if err == nil {
		return nil
	}

	primary := err
	if h.opts.Controller.Snapshot().Service == appcontrol.ServiceRunning {
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.opts.ShutdownTimeout)
		stop, stopErr := h.opts.Controller.Stop(stopCtx)
		if stopErr == nil {
			var result appcontrol.OperationResult
			result, stopErr = waitControllerCleanup(h.opts.Controller, stop, h.opts.ShutdownTimeout)
			if stopErr == nil && result.Status != appcontrol.OperationSucceeded {
				stopErr = result.Err
			}
		}
		cancel()
		if stopErr != nil {
			return &ConfigSwitchError{Primary: primary, Rollback: stopErr}
		}
	}
	if restoreErr := h.opts.Configuration.RestoreSelection(previous); restoreErr != nil {
		return &ConfigSwitchError{Primary: primary, Rollback: restoreErr}
	}

	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), h.opts.ShutdownTimeout)
	defer cancel()
	rollbackStart, rollbackErr := h.opts.Controller.Start(rollbackCtx, previous.SelectedConfigRevision)
	if rollbackErr == nil {
		var result appcontrol.OperationResult
		result, rollbackErr = waitControllerCleanup(h.opts.Controller, rollbackStart, h.opts.ShutdownTimeout)
		if rollbackErr == nil && result.Status != appcontrol.OperationSucceeded {
			rollbackErr = result.Err
		}
	}
	return &ConfigSwitchError{Primary: primary, Rollback: rollbackErr}
}

func (h *helper) commitSelection(ctx context.Context) error {
	if h.beforeSelectionCommit != nil {
		h.beforeSelectionCommit()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return h.opts.Configuration.CommitSelection()
}

func (h *helper) signOut(ctx context.Context) error {
	var stopErr error
	if h.opts.Controller.Snapshot().Service == appcontrol.ServiceRunning && h.opts.Controller.UsesCopilot() {
		stop, err := h.opts.Controller.Stop(ctx)
		if err != nil {
			return err
		}
		result, err := waitControllerCleanup(h.opts.Controller, stop, h.opts.ShutdownTimeout)
		if err != nil {
			return err
		}
		if result.Status != appcontrol.OperationSucceeded {
			stopErr = result.Err
			if stopErr == nil {
				stopErr = fmt.Errorf("stop operation ended with status %s", result.Status)
			}
		}
	}
	return errors.Join(stopErr, h.opts.Authenticator.SignOut())
}
