package proxy

import "context"

// policyRoutingController combines the request planner with startup preflight
// and bounded readiness state. Production installs one immutable controller;
// focused tests may still inject only the planner seam.
type policyRoutingController interface {
	chatPolicyPlanner
	Active() bool
	Initialize(context.Context) error
	ReadinessDiagnostic() string
}

func (h *ProxyHandler) PolicyRoutingActive() bool {
	binding := h.policyBindingForContext(nil)
	return binding != nil && binding.controller != nil && binding.controller.Active()
}

func (h *ProxyHandler) PolicyRoutingPreflightPending() bool {
	return h != nil && h.policyPreflightPending.Load()
}

func (h *ProxyHandler) PolicyRoutingReadinessDiagnostic() string {
	return h.policyRoutingReadinessDiagnosticForContext(context.Background())
}

func (h *ProxyHandler) policyRoutingReadinessDiagnosticForContext(ctx context.Context) string {
	binding := h.policyBindingForContext(ctx)
	if binding == nil || binding.controller == nil {
		return ""
	}
	return binding.controller.ReadinessDiagnostic()
}

func (h *ProxyHandler) policyPreflightSemaphore() chan struct{} {
	h.policyPreflightPermitOnce.Do(func() {
		h.policyPreflightPermit = make(chan struct{}, 1)
		h.policyPreflightPermit <- struct{}{}
	})
	return h.policyPreflightPermit
}

func (h *ProxyHandler) beginPolicyPreflightAttempt() {
	h.policyPreflightStateMu.Lock()
	h.policyPreflightAttempts++
	h.policyPreflightPending.Store(true)
	h.policyPreflightStateMu.Unlock()
}

func (h *ProxyHandler) finishPolicyPreflightAttempt(success bool) {
	h.policyPreflightStateMu.Lock()
	if h.policyPreflightAttempts > 0 {
		h.policyPreflightAttempts--
	}
	if success && h.policyPreflightAttempts == 0 {
		h.policyPreflightPending.Store(false)
	}
	h.policyPreflightStateMu.Unlock()
}

func (h *ProxyHandler) InitializePolicyRouting(ctx context.Context) (err error) {
	if h == nil {
		return nil
	}
	binding := h.policyBindingForContext(nil)
	if binding == nil || binding.controller == nil || !binding.controller.Active() {
		h.policyPreflightPending.Store(false)
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	h.beginPolicyPreflightAttempt()
	success := false
	defer func() { h.finishPolicyPreflightAttempt(success) }()

	if err := ctx.Err(); err != nil {
		return err
	}
	permit := h.policyPreflightSemaphore()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-permit:
	}
	defer func() { permit <- struct{}{} }()

	if err := binding.controller.Initialize(ctx); err != nil {
		return err
	}
	success = true
	return nil
}
