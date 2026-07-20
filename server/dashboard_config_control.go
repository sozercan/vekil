package server

import (
	"crypto/subtle"
	"encoding/json"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/sozercan/vekil/proxy"
)

const (
	DashboardConfigModeCLI     = "cli"
	DashboardConfigModeMenubar = "menubar"
)

func isDashboardConfigPath(path string) bool {
	return path == "/dashboard/config" || path == "/dashboard/config.js" || path == "/dashboard/config.css" ||
		path == "/dashboard/api/v1/config" || strings.HasPrefix(path, "/dashboard/api/v1/config/")
}

func withDashboardConfigSecurity(next http.Handler, handler *proxy.ProxyHandler, enabled bool, configuredPort string) http.Handler {
	if !enabled {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isDashboardConfigPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		if !dashboardConfigHostMatchesListener(r, configuredPort) {
			writeDashboardConfigSecurityError(w, http.StatusForbidden, "dashboard config control requires a literal loopback Host with the bound port")
			return
		}
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			if !dashboardConfigSameOrigin(r) {
				writeDashboardConfigSecurityError(w, http.StatusForbidden, "cross-origin dashboard config mutation is not allowed")
				return
			}
			expected := ""
			if handler != nil {
				expected = handler.DashboardConfigCSRFToken()
			}
			provided := strings.TrimSpace(r.Header.Get("X-Vekil-CSRF"))
			if expected == "" || len(expected) != len(provided) || subtle.ConstantTimeCompare([]byte(expected), []byte(provided)) != 1 {
				writeDashboardConfigSecurityError(w, http.StatusForbidden, "invalid dashboard config CSRF token")
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func dashboardConfigHostMatchesListener(r *http.Request, configuredPort string) bool {
	if r == nil {
		return false
	}
	expectedPort := strings.TrimSpace(configuredPort)
	if conn, ok := r.Context().Value(serverConnContextKey{}).(net.Conn); ok && conn != nil {
		if _, localPort, splitErr := net.SplitHostPort(conn.LocalAddr().String()); splitErr == nil && localPort != "" {
			expectedPort = localPort
		}
	}
	if expectedPort == "" || expectedPort == "0" {
		return false
	}

	requestHost := strings.TrimSpace(r.Host)
	host, port, err := net.SplitHostPort(requestHost)
	if err != nil {
		if expectedPort != "80" {
			return false
		}
		switch {
		case strings.HasPrefix(requestHost, "[") && strings.HasSuffix(requestHost, "]"):
			host = strings.TrimSuffix(strings.TrimPrefix(requestHost, "["), "]")
		case !strings.ContainsAny(requestHost, "[]:"):
			host = requestHost
		default:
			return false
		}
		port = expectedPort
	}
	return port != "" && port == expectedPort && isLiteralLoopbackRequestHost(host)
}

func isLiteralLoopbackRequestHost(host string) bool {
	host = strings.TrimSpace(strings.TrimSuffix(strings.TrimPrefix(host, "["), "]"))
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func dashboardConfigSameOrigin(r *http.Request) bool {
	if r == nil {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(r.Header.Get("Sec-Fetch-Site"))) {
	case "", "same-origin", "none":
	default:
		return false
	}
	for _, header := range []string{"Origin", "Referer"} {
		value := strings.TrimSpace(r.Header.Get(header))
		if value == "" {
			continue
		}
		parsed, err := url.Parse(value)
		if err != nil || parsed.Host == "" || !strings.EqualFold(parsed.Host, r.Host) {
			return false
		}
	}
	return true
}

func writeDashboardConfigSecurityError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"code": "config_access_denied", "message": message}})
}
