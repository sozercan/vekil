package macosruntime

import (
	"context"
	"errors"
	"sync"

	"github.com/sozercan/vekil/internal/appcontrol"
)

type coordinatedOperation struct {
	id         string
	kind       string
	ctx        context.Context
	cancel     context.CancelCauseFunc
	cancelable bool
	controller bool
	done       chan struct{}
	cleaned    bool
	complete   sync.Once
}

type operationCoordinator struct {
	mu     sync.Mutex
	active *coordinatedOperation
}

type coordinatedOperationSnapshot struct {
	id         string
	kind       string
	controller bool
}

func (c *operationCoordinator) beginController(parent context.Context, kind string, controller *appcontrol.Controller, begin func() (appcontrol.Operation, error)) (appcontrol.Operation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != nil {
		return appcontrol.Operation{}, appcontrol.ErrOperationInProgress
	}
	op, err := begin()
	if err != nil {
		return appcontrol.Operation{}, err
	}
	c.active = &coordinatedOperation{id: op.ID, kind: kind, cancelable: kind == "start", controller: true, done: make(chan struct{})}
	return op, nil
}

func (c *operationCoordinator) beginAsync(parent context.Context, kind string, cancelable bool) (*coordinatedOperation, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active != nil {
		return nil, appcontrol.ErrOperationInProgress
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancelCause(parent)
	op := &coordinatedOperation{
		id:         randomOpaqueID("op_"),
		kind:       kind,
		ctx:        ctx,
		cancel:     cancel,
		cancelable: cancelable,
		done:       make(chan struct{}),
	}
	c.active = op
	return op, nil
}

func (c *operationCoordinator) activeSnapshot() *coordinatedOperationSnapshot {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil {
		return nil
	}
	if c.active.cleaned {
		return nil
	}
	return &coordinatedOperationSnapshot{id: c.active.id, kind: c.active.kind, controller: c.active.controller}
}

func (c *operationCoordinator) finish(id string) *coordinatedOperation {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.active == nil || c.active.id != id {
		return nil
	}
	op := c.active
	op.cleaned = true
	return op
}

func (c *operationCoordinator) completeOperation(op *coordinatedOperation) {
	if op == nil {
		return
	}
	c.mu.Lock()
	if c.active == op {
		c.active = nil
	}
	c.mu.Unlock()
	op.complete.Do(func() { close(op.done) })
}

func (c *operationCoordinator) cancel(id string, controller *appcontrol.Controller) error {
	c.mu.Lock()
	active := c.active
	if active == nil || active.cleaned {
		c.mu.Unlock()
		return appcontrol.ErrOperationNotFound
	}
	if active.id != id {
		c.mu.Unlock()
		return appcontrol.ErrOperationIDMismatch
	}
	if !active.cancelable {
		c.mu.Unlock()
		return appcontrol.ErrOperationNotCancelable
	}
	if active.controller {
		c.mu.Unlock()
		return controller.CancelOperation(id)
	}
	cancel := active.cancel
	c.mu.Unlock()
	cancel(context.Canceled)
	return nil
}

func (c *operationCoordinator) cancelForShutdown(controller *appcontrol.Controller) {
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	if active == nil {
		return
	}
	if active.controller {
		_ = controller.CancelOperation(active.id)
		return
	}
	if active.cancel != nil {
		active.cancel(errShutdownRequested)
	}
}

func (c *operationCoordinator) wait(ctx context.Context) error {
	c.mu.Lock()
	active := c.active
	c.mu.Unlock()
	if active == nil {
		return nil
	}
	select {
	case <-active.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

var errOperationCompletionLost = errors.New("operation completion lost")
