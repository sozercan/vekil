package proxy

import (
	"context"
	"time"
)

func (h *ProxyHandler) newPolicyClassificationContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if parent == nil {
		parent = context.Background()
	}
	causeCtx, cancelCause := context.WithCancelCause(parent)
	stopLifecycle := context.AfterFunc(h.lifecycleContext(), func() {
		cancelCause(errProxyLifecycleShutdown)
	})
	if h.ShuttingDown() {
		cancelCause(errProxyLifecycleShutdown)
	}
	ctx, cancelTimeout := context.WithTimeout(causeCtx, timeout)
	return ctx, func() {
		stopLifecycle()
		cancelTimeout()
		cancelCause(context.Canceled)
	}
}

func (h *ProxyHandler) newPolicyObserveContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return h.newLifecycleUpstreamContext(timeout)
}
