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
	return h != nil && h.policyRoutingController != nil && h.policyRoutingController.Active()
}

func (h *ProxyHandler) PolicyRoutingPreflightPending() bool {
	return h != nil && h.policyPreflightPending.Load()
}

func (h *ProxyHandler) PolicyRoutingReadinessDiagnostic() string {
	if h == nil || h.policyRoutingController == nil {
		return ""
	}
	return h.policyRoutingController.ReadinessDiagnostic()
}

func (h *ProxyHandler) InitializePolicyRouting(ctx context.Context) error {
	if h == nil || h.policyRoutingController == nil || !h.policyRoutingController.Active() {
		if h != nil {
			h.policyPreflightPending.Store(false)
		}
		return nil
	}
	defer h.policyPreflightPending.Store(false)
	return h.policyRoutingController.Initialize(ctx)
}
