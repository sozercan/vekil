package proxy

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"net/http"
	"strings"
)

const dashboardConfigCSRFHeader = "X-Vekil-CSRF"

type dashboardConfigAccessState struct {
	available bool
	writable  bool
	reason    string
	mode      string
}

// DashboardConfigCapability is safe to return even when the listener is not
// eligible to expose the provider document.
type DashboardConfigCapability struct {
	Available bool   `json:"available"`
	Writable  bool   `json:"writable"`
	Reason    string `json:"reason,omitempty"`
	Mode      string `json:"mode,omitempty"`
}

func (h *ProxyHandler) ConfigureDashboardConfigAccess(available, writable bool, reason, mode string) {
	if h == nil {
		return
	}
	if !available {
		writable = false
	}
	h.dashboardConfigAccess.Store(&dashboardConfigAccessState{
		available: available,
		writable:  writable,
		reason:    strings.TrimSpace(reason),
		mode:      strings.TrimSpace(mode),
	})
}

func (h *ProxyHandler) dashboardConfigCapability() DashboardConfigCapability {
	if h == nil {
		return DashboardConfigCapability{Reason: "configuration control is unavailable"}
	}
	state := h.dashboardConfigAccess.Load()
	if state == nil {
		return DashboardConfigCapability{Reason: "configuration control is not enabled for this server mode"}
	}
	return DashboardConfigCapability{
		Available: state.available,
		Writable:  state.writable,
		Reason:    state.reason,
		Mode:      state.mode,
	}
}

func (h *ProxyHandler) dashboardConfigCSRFToken() string {
	if h == nil {
		return ""
	}
	h.dashboardConfigCSRFOnce.Do(func() {
		var raw [32]byte
		if _, err := rand.Read(raw[:]); err == nil {
			h.dashboardConfigCSRF = base64.RawURLEncoding.EncodeToString(raw[:])
		}
	})
	return h.dashboardConfigCSRF
}

// DashboardConfigCSRFToken exposes the process-local nonce to server-owned
// request security middleware. The token is returned to browsers only from a
// no-store same-origin config response.
func (h *ProxyHandler) DashboardConfigCSRFToken() string {
	return h.dashboardConfigCSRFToken()
}

func (h *ProxyHandler) dashboardConfigCSRFMatches(r *http.Request) bool {
	if h == nil || r == nil {
		return false
	}
	expected := h.dashboardConfigCSRFToken()
	provided := strings.TrimSpace(r.Header.Get(dashboardConfigCSRFHeader))
	return expected != "" && len(expected) == len(provided) && subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) == 1
}

func (h *ProxyHandler) DashboardConfigPersistenceWritable() bool {
	return h != nil && h.dashboardConfigSource != nil && h.dashboardConfigSource.store != nil
}

func (h *ProxyHandler) DashboardConfigReadOnlyReason() string {
	if h == nil || h.dashboardConfigSource == nil {
		return ""
	}
	return strings.TrimSpace(h.dashboardConfigSource.readOnlyReason)
}
