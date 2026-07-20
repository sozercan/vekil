package proxy

import (
	"context"
	"time"
)

// newPolicyClassificationContext keeps enforce/preflight work bound to both
// the caller and proxy lifecycle. Unlike ordinary terminal inference it must not
// detach from inbound cancellation because a canceled classification authorizes
// no terminal send.
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

// newPolicyObserveContext is lifecycle-rooted and detached from the completed
// client response. Callers must check the request context before worker launch.
func (h *ProxyHandler) newPolicyObserveContext(timeout time.Duration) (context.Context, context.CancelFunc) {
	return h.newLifecycleUpstreamContext(timeout)
}
