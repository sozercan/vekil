package server

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
)

type dashboardConfigTestConn struct{ local net.Addr }

func (c dashboardConfigTestConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c dashboardConfigTestConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c dashboardConfigTestConn) Close() error                     { return nil }
func (c dashboardConfigTestConn) LocalAddr() net.Addr              { return c.local }
func (c dashboardConfigTestConn) RemoteAddr() net.Addr             { return &net.TCPAddr{} }
func (c dashboardConfigTestConn) SetDeadline(time.Time) error      { return nil }
func (c dashboardConfigTestConn) SetReadDeadline(time.Time) error  { return nil }
func (c dashboardConfigTestConn) SetWriteDeadline(time.Time) error { return nil }

func TestDashboardConfigSecurity(t *testing.T) {
	h, err := proxy.NewProxyHandler(auth.NewTestAuthenticator("token"), logger.New(logger.LevelError))
	if err != nil {
		t.Fatal(err)
	}
	token := h.DashboardConfigCSRFToken()
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	secured := withDashboardConfigSecurity(next, h, true, "1337")

	tests := []struct {
		name       string
		host       string
		local      string
		method     string
		fetchSite  string
		origin     string
		token      string
		path       string
		wantStatus int
	}{
		{name: "valid localhost", host: "localhost:1337", local: "127.0.0.1:1337", method: http.MethodPost, fetchSite: "same-origin", origin: "http://localhost:1337", token: token, path: "/dashboard/api/v1/config/validate", wantStatus: http.StatusNoContent},
		{name: "valid YAML import", host: "localhost:1337", local: "127.0.0.1:1337", method: http.MethodPost, fetchSite: "same-origin", origin: "http://localhost:1337", token: token, path: "/dashboard/api/v1/config/import", wantStatus: http.StatusNoContent},
		{name: "valid ipv4", host: "127.0.0.2:1337", local: "127.0.0.1:1337", method: http.MethodGet, path: "/dashboard/api/v1/config", wantStatus: http.StatusNoContent},
		{name: "valid ipv6", host: "[::1]:1337", local: "[::1]:1337", method: http.MethodGet, path: "/dashboard/config", wantStatus: http.StatusNoContent},
		{name: "dns rebinding host", host: "proxy.example:1337", local: "127.0.0.1:1337", method: http.MethodGet, path: "/dashboard/api/v1/config", wantStatus: http.StatusForbidden},
		{name: "wrong port", host: "localhost:7331", local: "127.0.0.1:1337", method: http.MethodGet, path: "/dashboard/api/v1/config", wantStatus: http.StatusForbidden},
		{name: "cross site", host: "localhost:1337", local: "127.0.0.1:1337", method: http.MethodPost, fetchSite: "cross-site", token: token, path: "/dashboard/api/v1/config/applies", wantStatus: http.StatusForbidden},
		{name: "missing csrf", host: "localhost:1337", local: "127.0.0.1:1337", method: http.MethodDelete, fetchSite: "same-origin", origin: "http://localhost:1337", path: "/dashboard/api/v1/config/managed", wantStatus: http.StatusForbidden},
		{name: "YAML import missing csrf", host: "localhost:1337", local: "127.0.0.1:1337", method: http.MethodPost, fetchSite: "same-origin", origin: "http://localhost:1337", path: "/dashboard/api/v1/config/import", wantStatus: http.StatusForbidden},
		{name: "unrelated path bypass", host: "proxy.example:9000", local: "127.0.0.1:1337", method: http.MethodPost, path: "/v1/responses", wantStatus: http.StatusNoContent},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, "http://"+tc.host+tc.path, nil)
			r.Host = tc.host
			r.Header.Set("Sec-Fetch-Site", tc.fetchSite)
			if tc.origin != "" {
				r.Header.Set("Origin", tc.origin)
			}
			if tc.token != "" {
				r.Header.Set("X-Vekil-CSRF", tc.token)
			}
			addr, err := net.ResolveTCPAddr("tcp", tc.local)
			if err != nil {
				t.Fatal(err)
			}
			r = r.WithContext(context.WithValue(r.Context(), serverConnContextKey{}, dashboardConfigTestConn{local: addr}))
			w := httptest.NewRecorder()
			secured.ServeHTTP(w, r)
			if w.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.wantStatus, w.Body.String())
			}
		})
	}
}

func TestDashboardConfigHostMatchesListener(t *testing.T) {
	tests := []struct {
		name           string
		host           string
		configuredPort string
		local          string
		want           bool
	}{
		{name: "localhost omitted port at configured HTTP default", host: "localhost", configuredPort: "80", want: true},
		{name: "localhost root dot omitted port at configured HTTP default", host: "localhost.", configuredPort: "80", want: true},
		{name: "localhost root dot explicit port", host: "localhost.:1337", configuredPort: "1337", want: true},
		{name: "ipv4 omitted port at configured HTTP default", host: "127.0.0.2", configuredPort: "80", want: true},
		{name: "ipv6 omitted port at configured HTTP default", host: "[::1]", configuredPort: "80", want: true},
		{name: "localhost omitted port rejected at non-default", host: "localhost", configuredPort: "1337", want: false},
		{name: "ipv4 omitted port rejected at non-default", host: "127.0.0.2", configuredPort: "1337", want: false},
		{name: "ipv6 omitted port rejected at non-default", host: "[::1]", configuredPort: "1337", want: false},
		{name: "actual HTTP default overrides configured port", host: "localhost", configuredPort: "1337", local: "127.0.0.1:80", want: true},
		{name: "actual non-default port rejects omitted port", host: "localhost", configuredPort: "80", local: "127.0.0.1:1337", want: false},
		{name: "explicit non-default port still matches exactly", host: "[::1]:1337", configuredPort: "1337", want: true},
		{name: "explicit non-default port mismatch", host: "127.0.0.1:7331", configuredPort: "1337", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "http://example.test/dashboard/config", nil)
			r.Host = tc.host
			if tc.local != "" {
				addr, err := net.ResolveTCPAddr("tcp", tc.local)
				if err != nil {
					t.Fatal(err)
				}
				r = r.WithContext(context.WithValue(r.Context(), serverConnContextKey{}, dashboardConfigTestConn{local: addr}))
			}
			if got := dashboardConfigHostMatchesListener(r, tc.configuredPort); got != tc.want {
				t.Fatalf("dashboardConfigHostMatchesListener() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDashboardConfigCapabilityByServerMode(t *testing.T) {
	root := t.TempDir()
	resolved, err := proxy.ResolveProvidersConfig(proxy.ProvidersConfigResolveOptions{UserConfigDir: root, Mode: proxy.ProvidersConfigUseManaged})
	if err != nil {
		t.Fatal(err)
	}
	store, err := proxy.NewManagedProvidersConfigStore(resolved.Bootstrap)
	if err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name          string
		host          string
		options       []Option
		requestHost   string
		wantAvailable bool
		wantConfig    bool
	}{
		{
			name:          "long lived loopback exposes config",
			host:          "127.0.0.1",
			options:       []Option{WithDashboardConfigControl(DashboardConfigModeCLI), WithProxyOptions(proxy.WithProvidersConfig(resolved.Config), proxy.WithDashboardConfigSource(resolved, store))},
			requestHost:   "localhost:1337",
			wantAvailable: true,
			wantConfig:    true,
		},
		{
			name:          "managed launch default is capability only",
			host:          "127.0.0.1",
			options:       []Option{WithProxyOptions(proxy.WithProvidersConfig(resolved.Config))},
			requestHost:   "localhost:1337",
			wantAvailable: false,
			wantConfig:    false,
		},
		{
			name:          "non loopback is capability only",
			host:          "0.0.0.0",
			options:       []Option{WithDashboardConfigControl(DashboardConfigModeCLI), WithProxyOptions(proxy.WithProvidersConfig(resolved.Config), proxy.WithDashboardConfigSource(resolved, store))},
			requestHost:   "example.test:1337",
			wantAvailable: false,
			wantConfig:    false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := New(auth.NewTestAuthenticator("token"), logger.New(logger.LevelError), tc.host, "1337", tc.options...)
			if err != nil {
				t.Fatal(err)
			}
			r := httptest.NewRequest(http.MethodGet, "http://"+tc.requestHost+"/dashboard/api/v1/config", nil)
			r.Host = tc.requestHost
			addr, _ := net.ResolveTCPAddr("tcp", "127.0.0.1:1337")
			r = r.WithContext(context.WithValue(r.Context(), serverConnContextKey{}, dashboardConfigTestConn{local: addr}))
			w := httptest.NewRecorder()
			srv.httpServer.Handler.ServeHTTP(w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", w.Code, w.Body.String())
			}
			var response map[string]any
			if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			capability, _ := response["capability"].(map[string]any)
			if got, _ := capability["available"].(bool); got != tc.wantAvailable {
				t.Fatalf("available = %v, want %v; body=%s", got, tc.wantAvailable, w.Body.String())
			}
			_, hasConfig := response["config"]
			if hasConfig != tc.wantConfig {
				t.Fatalf("config present = %v, want %v; body=%s", hasConfig, tc.wantConfig, w.Body.String())
			}
		})
	}
}
