package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
)

func TestStart_ReturnsErrorWhenPortInUse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("failed to reserve port: %v", err)
	}
	defer func() {
		if err := listener.Close(); err != nil {
			t.Fatalf("failed to close listener: %v", err)
		}
	}()

	addr, ok := listener.Addr().(*net.TCPAddr)
	if !ok {
		t.Fatalf("expected TCP address, got %T", listener.Addr())
	}

	srv, err := New(auth.NewTestAuthenticator("test-token"), logger.New(logger.ParseLevel("error")), "127.0.0.1", strconv.Itoa(addr.Port))
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}
	err = srv.Start()
	if err == nil {
		t.Fatal("expected Start to fail when port is already in use")
	}
	if srv.IsRunning() {
		t.Fatal("expected server to remain stopped after listen failure")
	}
	if !strings.Contains(err.Error(), "address already in use") {
		t.Fatalf("expected address-in-use error, got %v", err)
	}
}

func TestStart_PortZeroPublishesAndLogsBoundAddress(t *testing.T) {
	var logs bytes.Buffer
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelInfo, &logs),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if got := srv.Addr(); got != "127.0.0.1:0" {
		t.Fatalf("Addr() before Start = %q, want configured address", got)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})

	boundAddr := srv.Addr()
	host, port, err := net.SplitHostPort(boundAddr)
	if err != nil {
		t.Fatalf("Addr() after Start = %q: %v", boundAddr, err)
	}
	if host != "127.0.0.1" {
		t.Fatalf("bound host = %q, want 127.0.0.1", host)
	}
	if port == "0" || port == "" {
		t.Fatalf("bound port = %q, want allocated ephemeral port", port)
	}

	resp, err := http.Get("http://" + boundAddr + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz on bound address: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz status = %d, want 200", resp.StatusCode)
	}

	var listeningEntry map[string]interface{}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var entry map[string]interface{}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode log line: %v", err)
		}
		if entry["msg"] == "vekil listening" {
			listeningEntry = entry
			break
		}
	}
	if listeningEntry == nil {
		t.Fatalf("missing listening log: %q", logs.String())
	}
	if got := listeningEntry["addr"]; got != boundAddr {
		t.Fatalf("logged addr = %#v, want %q", got, boundAddr)
	}
}

func TestAddrPreservesConfiguredFixedPort(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"43210",
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	otherBoundAddr := "127.0.0.1:54321"
	srv.boundAddr.Store(&otherBoundAddr)
	if got := srv.Addr(); got != "127.0.0.1:43210" {
		t.Fatalf("Addr() = %q, want configured fixed address", got)
	}
	if got := srv.listenerLogAddr(otherBoundAddr); got != "127.0.0.1:43210" {
		t.Fatalf("listener log addr = %q, want configured fixed address", got)
	}
}

func TestValidatePolicyRoutingListenHost(t *testing.T) {
	tests := []struct {
		name        string
		host        string
		active      bool
		allow       bool
		wantErr     bool
		wantErrText string
	}{
		{name: "inactive policy allows remote", host: "0.0.0.0", active: false},
		{name: "loopback IPv4", host: "127.0.0.2", active: true},
		{name: "loopback IPv6", host: "[::1]", active: true},
		{name: "localhost name", host: "LOCALHOST.", active: true},
		{name: "acknowledged remote", host: "0.0.0.0", active: true, allow: true},
		{name: "remote IPv4 rejected", host: "0.0.0.0", active: true, wantErr: true, wantErrText: "--policy-routing-allow-remote-single-tenant"},
		{name: "wildcard host rejected", host: "", active: true, wantErr: true, wantErrText: "non-loopback"},
		{name: "unresolved hostname rejected", host: "proxy.internal", active: true, wantErr: true, wantErrText: "non-loopback"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := validatePolicyRoutingListenHost(tc.host, tc.active, tc.allow)
			if tc.wantErr {
				if err == nil {
					t.Fatal("validatePolicyRoutingListenHost() error = nil, want rejection")
				}
				if !strings.Contains(err.Error(), tc.wantErrText) {
					t.Fatalf("validatePolicyRoutingListenHost() error = %q, want %q", err, tc.wantErrText)
				}
				return
			}
			if err != nil {
				t.Fatalf("validatePolicyRoutingListenHost() error = %v", err)
			}
		})
	}
}

type fakeStartupReadinessGate struct {
	authPending       bool
	validationPending bool
	policyPending     bool
	policyDiagnostic  string
}

func (f fakeStartupReadinessGate) StartupAuthenticationPending() bool {
	return f.authPending
}

func (f fakeStartupReadinessGate) DynamicProviderValidationPending() bool {
	return f.validationPending
}

func (f fakeStartupReadinessGate) PolicyRoutingPreflightPending() bool {
	return f.policyPending
}

func (f fakeStartupReadinessGate) PolicyRoutingReadinessDiagnostic() string {
	return f.policyDiagnostic
}

func TestProviderValidationGateIncludesPolicyPreflight(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := withProviderValidationGate(next, fakeStartupReadinessGate{
		policyPending:    true,
		policyDiagnostic: "policy classifier preflight pending",
	})

	for _, tc := range []struct {
		path       string
		wantStatus int
		wantBody   string
	}{
		{path: "/healthz", wantStatus: http.StatusNoContent},
		{path: "/readyz", wantStatus: http.StatusNoContent},
		{path: "/v1/models", wantStatus: http.StatusServiceUnavailable, wantBody: "policy classifier preflight pending"},
		{path: "/v1/chat/completions", wantStatus: http.StatusServiceUnavailable, wantBody: "policy classifier preflight pending"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			w := httptest.NewRecorder()
			handler.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tc.path, nil))
			if got := w.Code; got != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", got, tc.wantStatus, w.Body.String())
			}
			if tc.wantBody != "" && !strings.Contains(w.Body.String(), tc.wantBody) {
				t.Fatalf("body = %q, want %q", w.Body.String(), tc.wantBody)
			}
		})
	}
}

func TestProviderValidationGatePreservesStartupPrecedence(t *testing.T) {
	next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	tests := []struct {
		name string
		gate fakeStartupReadinessGate
		want string
	}{
		{
			name: "authentication before other preflight",
			gate: fakeStartupReadinessGate{authPending: true, validationPending: true, policyPending: true, policyDiagnostic: "policy pending"},
			want: "startup authentication pending",
		},
		{
			name: "dynamic validation before policy preflight",
			gate: fakeStartupReadinessGate{validationPending: true, policyPending: true, policyDiagnostic: "policy pending"},
			want: "provider model validation pending",
		},
		{
			name: "empty policy diagnostic has fallback",
			gate: fakeStartupReadinessGate{policyPending: true},
			want: "policy routing preflight pending",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			withProviderValidationGate(next, tc.gate).ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
			if got := w.Code; got != http.StatusServiceUnavailable {
				t.Fatalf("status = %d, want 503; body=%s", got, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), tc.want) {
				t.Fatalf("body = %q, want %q", w.Body.String(), tc.want)
			}
		})
	}
}

func TestRequestLogEnforcesSafeResponseContentTypes(t *testing.T) {
	tests := []struct {
		name            string
		method          string
		path            string
		contentType     string
		wantContentType string
	}{
		{
			name:            "API overrides upstream HTML",
			method:          http.MethodPost,
			path:            "/v1/responses",
			contentType:     "text/html; charset=utf-8",
			wantContentType: "application/json",
		},
		{
			name:            "API defaults missing type to JSON",
			method:          http.MethodPost,
			path:            "/v1/messages",
			wantContentType: "application/json",
		},
		{
			name:            "API preserves JSON",
			method:          http.MethodPost,
			path:            "/v1/chat/completions",
			contentType:     "application/json",
			wantContentType: "application/json",
		},
		{
			name:            "API preserves SSE",
			method:          http.MethodPost,
			path:            "/v1/responses",
			contentType:     "text/event-stream",
			wantContentType: "text/event-stream",
		},
		{
			name:            "API preserves plain text errors",
			method:          http.MethodGet,
			path:            "/v1/responses",
			contentType:     "text/plain; charset=utf-8",
			wantContentType: "text/plain; charset=utf-8",
		},
		{
			name:            "dashboard preserves HTML",
			method:          http.MethodGet,
			path:            "/dashboard",
			contentType:     "text/html; charset=utf-8",
			wantContentType: "text/html; charset=utf-8",
		},
		{
			name:            "dashboard preserves JavaScript",
			method:          http.MethodGet,
			path:            "/dashboard/uPlot.min.js",
			contentType:     "text/javascript; charset=utf-8",
			wantContentType: "text/javascript; charset=utf-8",
		},
		{
			name:            "dashboard preserves CSS",
			method:          http.MethodGet,
			path:            "/dashboard/uPlot.min.css",
			contentType:     "text/css; charset=utf-8",
			wantContentType: "text/css; charset=utf-8",
		},
		{
			name:            "dashboard HEAD preserves HTML",
			method:          http.MethodHead,
			path:            "/dashboard",
			contentType:     "text/html; charset=utf-8",
			wantContentType: "text/html; charset=utf-8",
		},
		{
			name:            "dashboard HEAD preserves JavaScript",
			method:          http.MethodHead,
			path:            "/dashboard/uPlot.min.js",
			contentType:     "text/javascript; charset=utf-8",
			wantContentType: "text/javascript; charset=utf-8",
		},
		{
			name:            "dashboard HEAD preserves CSS",
			method:          http.MethodHead,
			path:            "/dashboard/uPlot.min.css",
			contentType:     "text/css; charset=utf-8",
			wantContentType: "text/css; charset=utf-8",
		},
		{
			name:            "unknown dashboard asset cannot opt into HTML",
			method:          http.MethodGet,
			path:            "/dashboard/evil.html",
			contentType:     "text/html; charset=utf-8",
			wantContentType: "application/json",
		},
		{
			name:            "unknown dashboard HEAD cannot opt into HTML",
			method:          http.MethodHead,
			path:            "/dashboard/evil.html",
			contentType:     "text/html; charset=utf-8",
			wantContentType: "application/json",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			next := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if tc.contentType != "" {
					w.Header().Set("Content-Type", tc.contentType)
				}
				_, _ = io.WriteString(w, `<script>alert("xss")</script>`)
			})
			handler := withRequestLog(next, logger.NewWithWriter(logger.LevelError, io.Discard), nil, "")
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(tc.method, tc.path, nil))

			resp := recorder.Result()
			if got := resp.Header.Get("Content-Type"); got != tc.wantContentType {
				t.Fatalf("Content-Type = %q, want %q", got, tc.wantContentType)
			}
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}

func TestServerPreservesPlainTextWebSocketUpgradeError(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/responses", nil))
	resp := recorder.Result()
	if resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("status = %d, want 426; body=%s", resp.StatusCode, recorder.Body.String())
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := recorder.Body.String(); got != http.StatusText(http.StatusUpgradeRequired)+"\n" {
		t.Fatalf("body = %q, want Upgrade Required newline", got)
	}
}

func TestServerPreservesPlainTextMethodMismatch(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	recorder := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPut, "/v1/responses", nil))
	resp := recorder.Result()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want 405; body=%s", resp.StatusCode, recorder.Body.String())
	}
	if got := resp.Header.Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Fatalf("Content-Type = %q, want text/plain; charset=utf-8", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := recorder.Body.String(); got != http.StatusText(http.StatusMethodNotAllowed)+"\n" {
		t.Fatalf("body = %q, want Method Not Allowed newline", got)
	}
}

func TestServerOverridesExecutableUpstreamContentType(t *testing.T) {
	const payload = `<script>alert("upstream")</script>`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/responses" {
			t.Fatalf("upstream path = %q, want /responses", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = io.WriteString(w, payload)
	}))
	defer upstream.Close()

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
		WithProxyOptions(proxy.WithProvidersConfig(proxy.ProvidersConfig{Providers: []proxy.ProviderConfig{{
			ID:             "local",
			Type:           "openai-compatible",
			Default:        true,
			BaseURL:        upstream.URL,
			AuthType:       "none",
			ModelDiscovery: "static",
			Models: []proxy.ProviderModelConfig{{
				PublicID:  "test-model",
				Endpoints: []string{"/responses"},
			}},
		}}})),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"test-model","input":"reflect me"}`))
	req.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(recorder, req)

	resp := recorder.Result()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", resp.StatusCode, recorder.Body.String())
	}
	if got := resp.Header.Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}
	if got := recorder.Body.String(); got != payload {
		t.Fatalf("body = %q, want byte-exact upstream payload", got)
	}
}

func TestServerPreservesTrustedDashboardContentTypes(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	for _, tc := range []struct {
		path            string
		wantStatus      int
		wantContentType string
	}{
		{path: "/dashboard", wantStatus: http.StatusOK, wantContentType: "text/html; charset=utf-8"},
		{path: "/dashboard/uPlot.min.js", wantStatus: http.StatusOK, wantContentType: "text/javascript; charset=utf-8"},
		{path: "/dashboard/uPlot.min.css", wantStatus: http.StatusOK, wantContentType: "text/css; charset=utf-8"},
		{path: "/dashboard/evil.html", wantStatus: http.StatusNotFound, wantContentType: "text/plain; charset=utf-8"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			srv.httpServer.Handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tc.path, nil))
			resp := recorder.Result()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", resp.StatusCode, tc.wantStatus, recorder.Body.String())
			}
			if got := resp.Header.Get("Content-Type"); got != tc.wantContentType {
				t.Fatalf("Content-Type = %q, want %q", got, tc.wantContentType)
			}
			if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}

func TestServerBlocksNonHealthRoutesWhileStartupAuthenticationPending(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}
	srv.SetStartupAuthenticationPending(true)

	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodGet, path: "/healthz", want: http.StatusOK},
		{method: http.MethodGet, path: "/readyz", want: http.StatusServiceUnavailable},
		{method: http.MethodGet, path: "/v1/models", want: http.StatusServiceUnavailable},
		{method: http.MethodPost, path: "/v1/chat/completions", want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"model":"gpt-5.4"}`))
			w := httptest.NewRecorder()
			srv.httpServer.Handler.ServeHTTP(w, req)

			resp := w.Result()
			if resp.StatusCode != tc.want {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.want, body)
			}
			if tc.path != "/healthz" {
				body, _ := io.ReadAll(resp.Body)
				if !strings.Contains(string(body), "startup authentication pending") {
					t.Fatalf("response missing startup auth pending message: %s", body)
				}
			}
		})
	}
}

func TestServerBlocksNonHealthRoutesWhileProviderValidationPending(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer upstream.Close()

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
		WithProxyOptions(
			proxy.WithDeferredDynamicProviderModelValidation(true),
			proxy.WithProvidersConfig(proxy.ProvidersConfig{
				Providers: []proxy.ProviderConfig{
					{ID: "copilot", Type: "copilot", Default: true},
					{
						ID:       "local",
						Type:     "openai-compatible",
						BaseURL:  upstream.URL,
						AuthType: "none",
						Models: []proxy.ProviderModelConfig{{
							PublicID: "local-model",
						}},
					},
				},
			}),
		),
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}

	for _, tc := range []struct {
		method string
		path   string
		want   int
	}{
		{method: http.MethodGet, path: "/healthz", want: http.StatusOK},
		{method: http.MethodGet, path: "/readyz", want: http.StatusServiceUnavailable},
		{method: http.MethodGet, path: "/v1/models", want: http.StatusServiceUnavailable},
		{method: http.MethodPost, path: "/v1/chat/completions", want: http.StatusServiceUnavailable},
	} {
		t.Run(tc.path, func(t *testing.T) {
			req := httptest.NewRequest(tc.method, tc.path, strings.NewReader(`{"model":"gpt-5.4"}`))
			w := httptest.NewRecorder()
			srv.httpServer.Handler.ServeHTTP(w, req)

			resp := w.Result()
			if resp.StatusCode != tc.want {
				body, _ := io.ReadAll(resp.Body)
				t.Fatalf("status = %d, want %d: %s", resp.StatusCode, tc.want, body)
			}
			if tc.path != "/healthz" {
				body, _ := io.ReadAll(resp.Body)
				if !strings.Contains(string(body), "provider model validation pending") {
					t.Fatalf("response missing pending validation message: %s", body)
				}
			}
		})
	}
}

// copilotChatProxyOptionWithModelDiscovery keeps a static Chat route while
// intentionally exercising the upstream /models lifecycle in server tests.
func copilotChatProxyOptionWithModelDiscovery(baseURL string) proxy.Option {
	return proxy.WithProvidersConfig(proxy.ProvidersConfig{Providers: []proxy.ProviderConfig{{
		ID:             "test-provider",
		Type:           "openai-compatible",
		Default:        true,
		BaseURL:        baseURL,
		AuthType:       "none",
		ModelDiscovery: "openai",
		Models: []proxy.ProviderModelConfig{{
			PublicID:  "gpt-5",
			Endpoints: []string{"/chat/completions"},
		}},
	}}})
}

func responsesBackedProxyOption(baseURL string) proxy.Option {
	return proxy.WithProvidersConfig(proxy.ProvidersConfig{Providers: []proxy.ProviderConfig{{
		ID:       "test-provider",
		Type:     "openai-compatible",
		Default:  true,
		BaseURL:  baseURL,
		AuthType: "none",
		Models: []proxy.ProviderModelConfig{{
			PublicID:  "gpt-5",
			Endpoints: []string{"/responses"},
		}},
	}}})
}

func TestServerInitializePolicyRoutingDelegatesWhenDisabled(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := srv.InitializePolicyRouting(context.Background()); err != nil {
		t.Fatalf("InitializePolicyRouting() error = %v", err)
	}

	var nilServer *Server
	if err := nilServer.InitializePolicyRouting(context.Background()); err != nil {
		t.Fatalf("nil Server InitializePolicyRouting() error = %v", err)
	}
}

func TestNew_ConfiguresExtendedWriteTimeout(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.ParseLevel("error")),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("failed to initialize server: %v", err)
	}

	if got, want := srv.httpServer.WriteTimeout, 65*time.Minute; got != want {
		t.Fatalf("WriteTimeout = %v, want %v", got, want)
	}
}

func TestNew_DerivesWriteTimeoutFromConfiguredProxyHandler(t *testing.T) {
	const customTimeout = 17 * time.Minute

	tests := []struct {
		name string
		opts []Option
	}{
		{
			name: "server wrapper",
			opts: []Option{WithStreamingUpstreamTimeout(customTimeout)},
		},
		{
			name: "proxy option",
			opts: []Option{WithProxyOptions(proxy.WithStreamingUpstreamTimeout(customTimeout))},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := New(
				auth.NewTestAuthenticator("test-token"),
				logger.New(logger.ParseLevel("error")),
				"127.0.0.1",
				"0",
				tc.opts...,
			)
			if err != nil {
				t.Fatalf("failed to initialize server: %v", err)
			}

			if got, want := srv.httpServer.WriteTimeout, customTimeout+5*time.Minute; got != want {
				t.Fatalf("WriteTimeout = %v, want %v", got, want)
			}
		})
	}
}

func TestRequestLogIncludesSummaryUsageAndUpstreamRequestID(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("upstream path = %q, want /chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req-upstream-123")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":12,\"completion_tokens\":3,\"total_tokens\":15}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelInfo, &logs),
		"127.0.0.1",
		"0",
		WithProxyOptions(copilotChatProxyOptionWithModelDiscovery(upstream.URL)),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}],"tools":[{"type":"function","function":{"name":"lookup","parameters":{"type":"object","properties":{}}}}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.httpServer.Handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(body))
	}

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1: %q", len(lines), logs.String())
	}
	var entry map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal log entry: %v", err)
	}
	want := map[string]interface{}{
		"msg":                 "request completed",
		"level":               "info",
		"method":              "POST",
		"path":                "/v1/chat/completions",
		"endpoint":            "openai_chat",
		"model":               "gpt-5",
		"provider":            "test-provider",
		"provider_kind":       "openai-compatible",
		"stream":              false,
		"upstream_request_id": "req-upstream-123",
	}
	for key, expected := range want {
		if got := entry[key]; got != expected {
			t.Fatalf("log[%s] = %#v, want %#v in %#v", key, got, expected, entry)
		}
	}
	for key, expected := range map[string]float64{"status": 200, "prompt_tokens": 12, "completion_tokens": 3, "total_tokens": 15} {
		if got, ok := entry[key].(float64); !ok || got != expected {
			t.Fatalf("log[%s] = %#v, want %v in %#v", key, entry[key], expected, entry)
		}
	}
}

// A non-2xx must name why: type, code, param and message on the one line.
func TestRequestLogAtWarnLevelIncludesErrorDetailOnNonSuccess(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream called at %q; the request must be rejected locally", r.URL.Path)
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelWarn, &logs),
		"127.0.0.1",
		"0",
		WithProxyOptions(copilotChatProxyOptionWithModelDiscovery(upstream.URL)),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	// A replay ID on a route that has no /responses endpoint: an invalid request
	// carrying all four of type, code, param and message.
	body := `{"model":"gpt-5","messages":[{"role":"assistant","tool_calls":[{"id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","type":"function","function":{"name":"f","arguments":"{}"}}]},{"role":"tool","tool_call_id":"call_vekil_AAAAAAAAAAAAAAAAAAAAAA","content":"ok"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want 400: %s", w.Code, w.Body.String())
	}
	entry := requestLogEntry(t, logs.String(), "request completed")
	want := map[string]any{
		"level":         "warn",
		"msg":           "request completed",
		"path":          "/v1/chat/completions",
		"model":         "gpt-5",
		"error_type":    "invalid_request_error",
		"error_code":    "responses_replay_state_missing",
		"error_param":   "messages",
		"error_message": "Responses-backed tool state is no longer available; restart the assistant tool-call turn.",
	}
	for key, expected := range want {
		if got := entry[key]; got != expected {
			t.Fatalf("log[%s] = %#v, want %#v in %#v", key, got, expected, entry)
		}
	}
	if got, ok := entry["status"].(float64); !ok || got != 400 {
		t.Fatalf("log[status] = %#v, want 400 in %#v", entry["status"], entry)
	}
}

// End to end: a relayed upstream error must not put its prose on this line.
func TestRequestLogWithholdsUpstreamAuthoredErrorMessage(t *testing.T) {
	const secret = "SSN 123-45-6789 from the user prompt"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","code":"bad_value","param":"messages","message":"Invalid value for 'messages[0].content': `+secret+`"}}`)
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelInfo, &logs),
		"127.0.0.1",
		"0",
		WithProxyOptions(responsesBackedProxyOption(upstream.URL)),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("StatusCode = %d, want 400: %s", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), secret) {
		t.Fatalf("client no longer receives the upstream message; fixture is stale: %s", w.Body.String())
	}
	if strings.Contains(logs.String(), secret) {
		t.Fatalf("upstream message leaked into the log: %s", logs.String())
	}
	entry := requestLogEntry(t, logs.String(), "request completed")
	if entry["level"] != "warn" {
		t.Fatalf("log[level] = %#v, want warn in %#v", entry["level"], entry)
	}
	if _, ok := entry["error_message"]; ok {
		t.Fatalf("log carried error_message for an upstream error: %#v", entry)
	}
}

// A stream truncated after its 200 was committed is still a failure: the wire
// status stays 200, so only the summary knows the turn did not survive.
func TestRequestLogWarnsOnPostCommitStreamFailure(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"chatcmpl-1\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelInfo, &logs),
		"127.0.0.1",
		"0",
		WithProxyOptions(copilotChatProxyOptionWithModelDiscovery(upstream.URL)),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	body := `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("StatusCode = %d, want the committed 200", w.Code)
	}
	entry := requestLogEntry(t, logs.String(), "request failed after response headers were committed")
	if entry["level"] != "warn" {
		t.Fatalf("log[level] = %#v, want warn in %#v", entry["level"], entry)
	}
	if got, ok := entry["status"].(float64); !ok || got != 200 {
		t.Fatalf("log[status] = %#v, want the committed 200 in %#v", entry["status"], entry)
	}
	// The warn is only actionable with the recorded status that explains it; asserting the
	// level alone would still pass if stats_status silently stopped being emitted.
	if got, ok := entry["stats_status"].(float64); !ok || got == 200 {
		t.Fatalf("log[stats_status] = %#v, want the diverging recorded status in %#v", entry["stats_status"], entry)
	}
}

// The auth gate rejects before any handler runs; it still has to be logged.
func TestRequestLogWarnsOnInboundAuthRejection(t *testing.T) {
	var logs bytes.Buffer
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelInfo, &logs),
		"127.0.0.1",
		"0",
		WithInboundAuthToken("secret-token"),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	rec := httptest.NewRecorder()
	srv.httpServer.Handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")))

	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("StatusCode = %d, want 401", rec.Code)
	}
	entry := requestLogEntry(t, logs.String(), "request completed")
	if entry["level"] != "warn" {
		t.Fatalf("log[level] = %#v, want warn in %#v", entry["level"], entry)
	}
	if got, ok := entry["status"].(float64); !ok || got != 401 {
		t.Fatalf("log[status] = %#v, want 401 in %#v", entry["status"], entry)
	}
	if strings.Contains(logs.String(), "secret-token") {
		t.Fatalf("auth token leaked into the log: %s", logs.String())
	}
}

func requestLogEntry(t *testing.T, logs string, wantMsg string) map[string]any {
	return requestLogEntryForPath(t, logs, wantMsg, "")
}

func requestLogEntryForPath(t *testing.T, logs string, wantMsg, wantPath string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(strings.TrimSpace(logs), "\n") {
		var entry map[string]any
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if entry["msg"] == wantMsg && (wantPath == "" || entry["path"] == wantPath) {
			return entry
		}
	}
	t.Fatalf("no %q log entry for path %q in %q", wantMsg, wantPath, logs)
	return nil
}

// TestStreamingChatCompletionsPassthroughThroughServer drives a streaming
// POST /v1/chat/completions request through the full server stack (so
// withRequestLog attaches a RequestSummary and the usage callback is non-nil)
// with tool optimizers disabled. This exercises StreamOpenAIPassthroughWithFinalResponse
// on its default-config path, where onFinalResponse is nil but onUsage is not.
// Before the nil-aggregator guard this panicked on the first SSE chunk; this
// test also asserts the streamed body is forwarded verbatim with a [DONE]
// sentinel and that streaming usage is recorded in the request log.
func TestStreamingChatCompletionsPassthroughThroughServer(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			t.Fatalf("upstream path = %q, want /chat/completions", r.URL.Path)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("X-Request-Id", "req-stream-456")
		flusher, _ := w.(http.Flusher)
		writeChunk := func(s string) {
			_, _ = io.WriteString(w, s)
			if flusher != nil {
				flusher.Flush()
			}
		}
		writeChunk("data: {\"id\":\"chatcmpl-2\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":\"Hel\"}}]}\n\n")
		writeChunk("data: {\"id\":\"chatcmpl-2\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"lo\"}}]}\n\n")
		writeChunk("data: {\"id\":\"chatcmpl-2\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2,\"total_tokens\":9}}\n\n")
		writeChunk("data: [DONE]\n\n")
	}))
	defer upstream.Close()

	var logs bytes.Buffer
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelInfo, &logs),
		"127.0.0.1",
		"0",
		WithProxyOptions(copilotChatProxyOptionWithModelDiscovery(upstream.URL)),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	body := `{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	srv.httpServer.Handler.ServeHTTP(w, req)

	resp := w.Result()
	if resp.StatusCode != http.StatusOK {
		got, _ := io.ReadAll(resp.Body)
		t.Fatalf("StatusCode = %d, want 200: %s", resp.StatusCode, string(got))
	}
	if ct := resp.Header.Get("Content-Type"); !strings.Contains(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q, want text/event-stream", ct)
	}
	if got := resp.Header.Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
	}

	streamed, _ := io.ReadAll(resp.Body)
	got := string(streamed)
	// The passthrough must forward every upstream chunk verbatim, including
	// the terminal [DONE] sentinel, without truncation or panic.
	for _, want := range []string{"\"content\":\"Hel\"", "\"content\":\"lo\"", "\"finish_reason\":\"stop\"", "data: [DONE]"} {
		if !strings.Contains(got, want) {
			t.Fatalf("streamed body missing %q; got:\n%s", want, got)
		}
	}

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 1 {
		t.Fatalf("log lines = %d, want 1: %q", len(lines), logs.String())
	}
	var entry map[string]interface{}
	if err := json.Unmarshal([]byte(lines[0]), &entry); err != nil {
		t.Fatalf("unmarshal log entry: %v", err)
	}
	if got, ok := entry["stream"].(bool); !ok || !got {
		t.Fatalf("log[stream] = %#v, want true", entry["stream"])
	}
	// Usage must be observed via the streaming onUsage callback (regression:
	// the callback was previously wired but never invoked, so these were absent).
	for key, expected := range map[string]float64{"status": 200, "prompt_tokens": 7, "completion_tokens": 2, "total_tokens": 9} {
		if got, ok := entry[key].(float64); !ok || got != expected {
			t.Fatalf("log[%s] = %#v, want %v in %#v", key, entry[key], expected, entry)
		}
	}
	if got := entry["upstream_request_id"]; got != "req-stream-456" {
		t.Fatalf("log[upstream_request_id] = %#v, want req-stream-456", got)
	}
}

func TestStopCancelsHangingInferenceAndModelCatalog(t *testing.T) {
	inferenceStarted := make(chan struct{})
	modelsStarted := make(chan struct{})
	inferenceCanceled := make(chan struct{})
	modelsCanceled := make(chan struct{})
	release := make(chan struct{})
	var inferenceStartOnce sync.Once
	var modelsStartOnce sync.Once
	var inferenceCancelOnce sync.Once
	var modelsCancelOnce sync.Once

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		var started chan struct{}
		var canceled chan struct{}
		var startOnce *sync.Once
		var cancelOnce *sync.Once
		switch r.URL.Path {
		case "/chat/completions":
			started = inferenceStarted
			canceled = inferenceCanceled
			startOnce = &inferenceStartOnce
			cancelOnce = &inferenceCancelOnce
		case "/models":
			started = modelsStarted
			canceled = modelsCanceled
			startOnce = &modelsStartOnce
			cancelOnce = &modelsCancelOnce
		default:
			t.Errorf("unexpected upstream path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}

		startOnce.Do(func() { close(started) })
		select {
		case <-r.Context().Done():
			cancelOnce.Do(func() { close(canceled) })
		case <-release:
			w.Header().Set("Content-Type", "application/json")
			if r.URL.Path == "/models" {
				_, _ = io.WriteString(w, `{"object":"list","data":[]}`)
			} else {
				_, _ = io.WriteString(w, `{"id":"chatcmpl-stop","object":"chat.completion","choices":[]}`)
			}
		}
	}))

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
		WithProxyOptions(copilotChatProxyOptionWithModelDiscovery(upstream.URL)),
	)
	if err != nil {
		close(release)
		upstream.Close()
		t.Fatalf("New() error = %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		close(release)
		upstream.Close()
		t.Fatalf("net.Listen() error = %v", err)
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = srv.httpServer.Serve(listener)
	}()

	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			close(release)
			_ = srv.httpServer.Close()
			<-serveDone
			upstream.Close()
		})
	}
	t.Cleanup(cleanup)

	client := &http.Client{}
	type requestResult struct {
		name   string
		status int
		err    error
	}
	requestDone := make(chan requestResult, 2)
	go func() {
		resp, requestErr := client.Post(
			"http://"+listener.Addr().String()+"/v1/chat/completions",
			"application/json",
			strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`),
		)
		result := requestResult{name: "inference", err: requestErr}
		if requestErr == nil {
			result.status = resp.StatusCode
			_ = resp.Body.Close()
		}
		requestDone <- result
	}()
	go func() {
		resp, requestErr := client.Get("http://" + listener.Addr().String() + "/v1/models")
		result := requestResult{name: "models", err: requestErr}
		if requestErr == nil {
			result.status = resp.StatusCode
			_ = resp.Body.Close()
		}
		requestDone <- result
	}()

	for name, started := range map[string]<-chan struct{}{
		"inference": inferenceStarted,
		"models":    modelsStarted,
	} {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s upstream request", name)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	start := time.Now()
	err = srv.Stop(ctx)
	elapsed := time.Since(start)
	cancel()
	if err != nil {
		t.Fatalf("Stop() error = %v after %s; want graceful cancellation before the deadline", err, elapsed)
	}
	if elapsed >= 200*time.Millisecond {
		t.Fatalf("Stop() latency = %s, want prompt return below 200ms", elapsed)
	}
	t.Logf("Stop() canceled hanging inference and model catalog in %s", elapsed)

	for name, canceled := range map[string]<-chan struct{}{
		"inference": inferenceCanceled,
		"models":    modelsCanceled,
	} {
		select {
		case <-canceled:
		case <-time.After(200 * time.Millisecond):
			t.Fatalf("%s upstream did not observe shutdown cancellation", name)
		}
	}
	for range 2 {
		select {
		case result := <-requestDone:
			if result.err != nil {
				t.Fatalf("%s client request error = %v", result.name, result.err)
			}
			if result.status != http.StatusServiceUnavailable {
				t.Fatalf("%s shutdown status = %d, want 503", result.name, result.status)
			}
		case <-time.After(time.Second):
			t.Fatal("client request did not finish after shutdown cancellation")
		}
	}

	statsWriter := httptest.NewRecorder()
	srv.proxyHandler.HandleStatsJSON(statsWriter, httptest.NewRequest(http.MethodGet, "/stats.json", nil))
	var stats struct {
		Totals struct {
			Requests int64 `json:"requests"`
			Errors   int64 `json:"errors"`
		} `json:"totals"`
	}
	if err := json.NewDecoder(statsWriter.Result().Body).Decode(&stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.Totals.Requests != 0 || stats.Totals.Errors != 0 {
		t.Fatalf("shutdown-canceled provider stats = requests:%d errors:%d, want 0/0", stats.Totals.Requests, stats.Totals.Errors)
	}
}

func TestStopIsConcurrentIdempotentAndRejectsNewUpstreamWork(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	release := make(chan struct{})
	var upstreamCalls atomic.Int32
	var startedOnce sync.Once
	var canceledOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		upstreamCalls.Add(1)
		startedOnce.Do(func() { close(upstreamStarted) })
		select {
		case <-r.Context().Done():
			canceledOnce.Do(func() { close(upstreamCanceled) })
		case <-release:
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"id":"chatcmpl-release","object":"chat.completion","choices":[]}`)
		}
	}))

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
		WithProxyOptions(copilotChatProxyOptionWithModelDiscovery(upstream.URL)),
	)
	if err != nil {
		close(release)
		upstream.Close()
		t.Fatalf("New() error = %v", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		close(release)
		upstream.Close()
		t.Fatalf("net.Listen() error = %v", err)
	}
	serveDone := make(chan struct{})
	go func() {
		defer close(serveDone)
		_ = srv.httpServer.Serve(listener)
	}()

	var cleanupOnce sync.Once
	cleanup := func() {
		cleanupOnce.Do(func() {
			close(release)
			_ = srv.httpServer.Close()
			<-serveDone
			upstream.Close()
		})
	}
	t.Cleanup(cleanup)

	requestDone := make(chan struct{})
	go func() {
		defer close(requestDone)
		resp, requestErr := http.Post(
			"http://"+listener.Addr().String()+"/v1/chat/completions",
			"application/json",
			strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`),
		)
		if requestErr == nil {
			_ = resp.Body.Close()
		}
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream request")
	}

	const stopCalls = 8
	startStops := make(chan struct{})
	stopErrors := make(chan error, stopCalls)
	var wg sync.WaitGroup
	for range stopCalls {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-startStops
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			stopErrors <- srv.Stop(ctx)
		}()
	}
	close(startStops)
	wg.Wait()
	close(stopErrors)
	for stopErr := range stopErrors {
		if stopErr != nil {
			t.Fatalf("concurrent Stop() error = %v", stopErr)
		}
	}

	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("concurrent Stop calls did not cancel active upstream work")
	}
	select {
	case <-requestDone:
	case <-time.After(time.Second):
		t.Fatal("active request did not return after concurrent Stop calls")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := srv.Stop(ctx); err != nil {
		cancel()
		t.Fatalf("idempotent Stop() error = %v", err)
	}
	cancel()

	newReq := httptest.NewRequest(
		http.MethodPost,
		"/v1/chat/completions",
		strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"after shutdown"}]}`),
	)
	newReq.Header.Set("Content-Type", "application/json")
	newDone := make(chan struct{})
	go func() {
		defer close(newDone)
		srv.proxyHandler.HandleOpenAIChatCompletions(httptest.NewRecorder(), newReq)
	}()
	select {
	case <-newDone:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("new upstream work installed after shutdown did not fail promptly")
	}
	if got := upstreamCalls.Load(); got != 1 {
		t.Fatalf("upstream calls = %d, want 1; shutdown must reject new upstream work", got)
	}
}

func TestOpenAIStreamPreFirstEventShutdownReturns503OverNetHTTP(t *testing.T) {
	upstreamStarted := make(chan struct{})
	upstreamCanceled := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		if flusher, ok := w.(http.Flusher); ok {
			flusher.Flush()
		}
		close(upstreamStarted)
		<-r.Context().Done()
		close(upstreamCanceled)
	}))
	defer upstream.Close()

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
		WithProxyOptions(copilotChatProxyOptionWithModelDiscovery(upstream.URL)),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	testServer := httptest.NewServer(srv.httpServer.Handler)
	defer testServer.Close()

	type requestResult struct {
		status int
		body   []byte
		err    error
	}
	resultCh := make(chan requestResult, 1)
	go func() {
		resp, requestErr := http.Post(
			testServer.URL+"/v1/chat/completions",
			"application/json",
			strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
		)
		result := requestResult{err: requestErr}
		if requestErr == nil {
			result.status = resp.StatusCode
			result.body, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
		}
		resultCh <- result
	}()

	select {
	case <-upstreamStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream response headers")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := srv.Stop(ctx); err != nil {
		cancel()
		t.Fatalf("Stop() error = %v", err)
	}
	cancel()
	select {
	case <-upstreamCanceled:
	case <-time.After(time.Second):
		t.Fatal("upstream did not observe lifecycle cancellation")
	}
	select {
	case result := <-resultCh:
		if result.err != nil {
			t.Fatalf("client request error = %v", result.err)
		}
		if result.status != http.StatusServiceUnavailable {
			t.Fatalf("status = %d, want 503: %s", result.status, result.body)
		}
		if !bytes.Contains(result.body, []byte(`"type":"service_unavailable"`)) {
			t.Fatalf("shutdown response missing service_unavailable: %s", result.body)
		}
	case <-time.After(time.Second):
		t.Fatal("client request did not receive shutdown response")
	}
}

func TestHTTPStreamShutdownAccounting(t *testing.T) {
	tests := []struct {
		name         string
		upstreamBody string
		cancel       bool
		wantRequests int64
		wantErrors   int64
	}{
		{
			name:         "lifecycle transport cancellation is suppressed",
			upstreamBody: "data: {\"id\":\"chatcmpl-stop\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n",
			cancel:       true,
		},
		{
			name:         "semantic provider failure remains accounted",
			upstreamBody: "event: error\ndata: {\"error\":{\"type\":\"rate_limit_error\",\"code\":\"too_many_requests\",\"message\":\"slow down\"}}\n\n",
			wantRequests: 1,
			wantErrors:   1,
		},
		{
			name:         "completed provider turn remains accounted",
			upstreamBody: "data: {\"id\":\"chatcmpl-done\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":\"gpt-5\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"done\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n",
			wantRequests: 1,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			upstreamStarted := make(chan struct{})
			upstreamCanceled := make(chan struct{})
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, tt.upstreamBody)
				if flusher, ok := w.(http.Flusher); ok {
					flusher.Flush()
				}
				close(upstreamStarted)
				if tt.cancel {
					<-r.Context().Done()
					close(upstreamCanceled)
				}
			}))
			defer upstream.Close()

			srv, err := New(
				auth.NewTestAuthenticator("test-token"),
				logger.NewWithWriter(logger.LevelError, io.Discard),
				"127.0.0.1",
				"0",
				WithProxyOptions(copilotChatProxyOptionWithModelDiscovery(upstream.URL)),
			)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			testServer := httptest.NewServer(srv.httpServer.Handler)
			defer testServer.Close()

			resp, err := http.Post(
				testServer.URL+"/v1/chat/completions",
				"application/json",
				strings.NewReader(`{"model":"gpt-5","stream":true,"messages":[{"role":"user","content":"hello"}]}`),
			)
			if err != nil {
				t.Fatalf("POST chat stream: %v", err)
			}
			if resp.StatusCode != http.StatusOK {
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				t.Fatalf("stream status = %d, want 200: %s", resp.StatusCode, body)
			}
			select {
			case <-upstreamStarted:
			case <-time.After(time.Second):
				t.Fatal("timed out waiting for upstream stream")
			}

			if tt.cancel {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				if err := srv.Stop(ctx); err != nil {
					cancel()
					t.Fatalf("Stop() error = %v", err)
				}
				cancel()
				select {
				case <-upstreamCanceled:
				case <-time.After(time.Second):
					t.Fatal("stream upstream did not observe lifecycle cancellation")
				}
			}
			_, _ = io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if !tt.cancel {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				if err := srv.Stop(ctx); err != nil {
					cancel()
					t.Fatalf("Stop() error = %v", err)
				}
				cancel()
			}

			statsWriter := httptest.NewRecorder()
			srv.proxyHandler.HandleStatsJSON(statsWriter, httptest.NewRequest(http.MethodGet, "/stats.json", nil))
			var stats struct {
				Totals struct {
					Requests int64 `json:"requests"`
					Errors   int64 `json:"errors"`
				} `json:"totals"`
			}
			if err := json.NewDecoder(statsWriter.Result().Body).Decode(&stats); err != nil {
				t.Fatalf("decode stats: %v", err)
			}
			if stats.Totals.Requests != tt.wantRequests || stats.Totals.Errors != tt.wantErrors {
				t.Fatalf("provider stats = requests:%d errors:%d, want %d/%d", stats.Totals.Requests, stats.Totals.Errors, tt.wantRequests, tt.wantErrors)
			}
		})
	}
}

func TestStopAdmissionGateRejectsConcurrentRequestsWithoutUpstreamWork(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer upstream.Close()

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
		WithProxyOptions(proxy.WithCopilotBaseURL(upstream.URL)),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	stopDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		stopDone <- srv.Stop(ctx)
	}()
	deadline := time.Now().Add(time.Second)
	for !srv.proxyHandler.ShuttingDown() {
		if time.Now().After(deadline) {
			t.Fatal("Stop did not close admission")
		}
		time.Sleep(time.Millisecond)
	}

	tests := []struct {
		method string
		path   string
		body   string
		want   int
		marker string
	}{
		{method: http.MethodPost, path: "/v1/chat/completions", body: `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`, want: http.StatusServiceUnavailable, marker: `"type":"service_unavailable"`},
		{method: http.MethodPost, path: "/v1/messages", body: `{"model":"claude-sonnet-4","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`, want: http.StatusServiceUnavailable, marker: `"type":"overloaded_error"`},
		{method: http.MethodPost, path: "/v1beta/models/gemini-2.5-pro:generateContent", body: `{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}`, want: http.StatusServiceUnavailable, marker: `"status":"UNAVAILABLE"`},
		{method: http.MethodPost, path: "/v1/responses", body: `{"model":"gpt-5.4","input":"hello"}`, want: http.StatusServiceUnavailable, marker: `"type":"service_unavailable"`},
		{method: http.MethodGet, path: "/v1/models", want: http.StatusServiceUnavailable, marker: `"type":"service_unavailable"`},
		{method: http.MethodGet, path: "/readyz", want: http.StatusServiceUnavailable, marker: `"type":"service_unavailable"`},
		{method: http.MethodGet, path: "/healthz", want: http.StatusOK},
	}

	const rounds = 8
	var wg sync.WaitGroup
	errorsCh := make(chan string, len(tests)*rounds)
	for range rounds {
		for _, tt := range tests {
			tt := tt
			wg.Add(1)
			go func() {
				defer wg.Done()
				req := httptest.NewRequest(tt.method, tt.path, strings.NewReader(tt.body))
				req.Header.Set("Content-Type", "application/json")
				w := httptest.NewRecorder()
				srv.httpServer.Handler.ServeHTTP(w, req)
				resp := w.Result()
				body, _ := io.ReadAll(resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != tt.want {
					errorsCh <- fmt.Sprintf("%s %s status=%d want=%d body=%s", tt.method, tt.path, resp.StatusCode, tt.want, body)
					return
				}
				if tt.want == http.StatusServiceUnavailable && !bytes.Contains(body, []byte("server shutting down")) {
					errorsCh <- fmt.Sprintf("%s %s missing shutdown body: %s", tt.method, tt.path, body)
				}
				if tt.marker != "" && !bytes.Contains(body, []byte(tt.marker)) {
					errorsCh <- fmt.Sprintf("%s %s missing protocol marker %s: %s", tt.method, tt.path, tt.marker, body)
				}
			}()
		}
	}
	wg.Wait()
	close(errorsCh)
	for message := range errorsCh {
		t.Error(message)
	}

	if err := <-stopDone; err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("upstream calls after admission closed = %d, want 0", got)
	}

	statsWriter := httptest.NewRecorder()
	srv.proxyHandler.HandleStatsJSON(statsWriter, httptest.NewRequest(http.MethodGet, "/stats.json", nil))
	var stats struct {
		Totals struct {
			Requests int64 `json:"requests"`
			Errors   int64 `json:"errors"`
		} `json:"totals"`
	}
	if err := json.NewDecoder(statsWriter.Result().Body).Decode(&stats); err != nil {
		t.Fatalf("decode stats: %v", err)
	}
	if stats.Totals.Requests != 0 || stats.Totals.Errors != 0 {
		t.Fatalf("provider stats after local shutdown rejections = requests:%d errors:%d, want 0/0", stats.Totals.Requests, stats.Totals.Errors)
	}
}

func TestStopCancelsActiveResponsesWebSocketInference(t *testing.T) {
	started := make(chan struct{})
	canceled := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		startedOnce.Do(func() { close(started) })
		<-r.Context().Done()
		canceledOnce.Do(func() { close(canceled) })
	}))
	defer upstream.Close()

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		"127.0.0.1",
		"0",
		WithProxyOptions(proxy.WithCopilotBaseURL(upstream.URL)),
		WithResponsesWebSocketConfig(proxy.ResponsesWebSocketConfig{Enabled: true, DisableAutoCompact: true}),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	testServer := httptest.NewServer(srv.httpServer.Handler)
	defer testServer.Close()

	conn := dialServerResponsesWebSocket(t, testServer)
	defer func() { _ = conn.Close() }()
	if err := conn.WriteJSON(serverResponsesWebSocketRequest(nil)); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for active websocket inference")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case <-canceled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop did not cancel active websocket inference")
	}
}

func TestStopCancelsResponsesWebSocketAutoCompaction(t *testing.T) {
	compactionStarted := make(chan struct{})
	compactionCanceled := make(chan struct{})
	var startedOnce sync.Once
	var canceledOnce sync.Once
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read upstream body: %v", err)
			return
		}
		var body map[string]interface{}
		if err := json.Unmarshal(bodyBytes, &body); err != nil {
			t.Errorf("decode upstream body: %v", err)
			return
		}
		if instructions, _ := body["instructions"].(string); strings.Contains(instructions, "CONTEXT CHECKPOINT COMPACTION") {
			startedOnce.Do(func() { close(compactionStarted) })
			<-r.Context().Done()
			canceledOnce.Do(func() { close(compactionCanceled) })
			return
		}

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp-stop-compact\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-1\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-2\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"type\":\"message\",\"role\":\"assistant\",\"id\":\"msg-3\"}}\n\n")
		_, _ = io.WriteString(w, "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp-stop-compact\",\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"total_tokens\":0}}}\n\n")
	}))
	defer upstream.Close()

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.New(logger.LevelError),
		"127.0.0.1",
		"0",
		WithProxyOptions(proxy.WithCopilotBaseURL(upstream.URL)),
		WithResponsesWebSocketConfig(proxy.ResponsesWebSocketConfig{
			Enabled:             true,
			AutoCompactMaxItems: 2,
			AutoCompactMaxBytes: 1 << 20,
			AutoCompactKeepTail: 1,
		}),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	testServer := httptest.NewServer(srv.httpServer.Handler)
	defer testServer.Close()

	conn := dialServerResponsesWebSocket(t, testServer)
	defer func() { _ = conn.Close() }()
	if err := conn.WriteJSON(serverResponsesWebSocketRequest([]interface{}{
		map[string]interface{}{"type": "message", "role": "user", "content": []map[string]string{{"type": "input_text", "text": "compact"}}},
	})); err != nil {
		t.Fatalf("failed to write websocket request: %v", err)
	}
	for range 5 {
		if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
			t.Fatalf("set websocket read deadline: %v", err)
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Fatalf("read websocket response: %v", err)
		}
	}
	select {
	case <-compactionStarted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for websocket auto-compaction")
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case <-compactionCanceled:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("Stop did not cancel websocket auto-compaction")
	}
}

func dialServerResponsesWebSocket(t *testing.T, server *httptest.Server) *websocket.Conn {
	t.Helper()
	url := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/responses"
	conn, _, err := websocket.DefaultDialer.Dial(url, nil)
	if err != nil {
		t.Fatalf("dial websocket %s: %v", url, err)
	}
	return conn
}

func serverResponsesWebSocketRequest(input []interface{}) map[string]interface{} {
	return map[string]interface{}{
		"type":                "response.create",
		"model":               "gpt-5.4",
		"instructions":        "You are helpful",
		"input":               input,
		"tools":               []interface{}{},
		"tool_choice":         "auto",
		"parallel_tool_calls": true,
		"store":               false,
		"stream":              true,
		"include":             []string{},
	}
}

func TestStopCancelsHangingToolOptimizerAndRejectsNewOptimizerWork(t *testing.T) {
	tmpDir := t.TempDir()
	markerPath := filepath.Join(tmpDir, "optimizer-calls")
	scriptPath := filepath.Join(tmpDir, "optimizer.sh")
	script := fmt.Sprintf("#!/bin/sh\nprintf x >> %q\ncat >/dev/null\nsleep 60\n", markerPath)
	if err := os.WriteFile(scriptPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write optimizer script: %v", err)
	}

	var providers proxy.ProvidersConfig
	configJSON := fmt.Sprintf(`{
		"tool_optimizers": {
			"enabled": true,
			"tools": {"shell_function_calls": {"enabled": true, "names": ["shell_command"], "command_arg_path": "/command"}},
			"output_reduce": {"enabled": true, "timeout_ms": 0, "min_input_bytes": 1, "max_input_bytes": 100000},
			"providers": [{"id": "hang", "type": "exec_json", "path": %q, "stages": ["output_reduce"]}]
		}
	}`, scriptPath)
	if err := json.Unmarshal([]byte(configJSON), &providers); err != nil {
		t.Fatalf("decode providers config: %v", err)
	}

	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-optimizer","object":"chat.completion","choices":[]}`)
	}))
	defer upstream.Close()

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
		WithProxyOptions(proxy.WithCopilotBaseURL(upstream.URL), proxy.WithProvidersConfig(providers)),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	testServer := httptest.NewServer(srv.httpServer.Handler)
	defer testServer.Close()

	requestBody := `{
		"model":"gpt-5",
		"messages":[
			{"role":"assistant","tool_calls":[{"id":"call-1","type":"function","function":{"name":"shell_command","arguments":"{\"command\":\"echo hi\"}"}}]},
			{"role":"tool","tool_call_id":"call-1","content":"large tool output that should invoke the optimizer"}
		]
	}`
	requestDone := make(chan int, 1)
	go func() {
		resp, requestErr := http.Post(testServer.URL+"/v1/chat/completions", "application/json", strings.NewReader(requestBody))
		if requestErr != nil {
			requestDone <- 0
			return
		}
		_ = resp.Body.Close()
		requestDone <- resp.StatusCode
	}()

	deadline := time.Now().Add(time.Second)
	for {
		if data, err := os.ReadFile(markerPath); err == nil && len(data) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for optimizer process")
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	start := time.Now()
	if err := srv.Stop(ctx); err != nil {
		cancel()
		t.Fatalf("Stop() error = %v", err)
	}
	cancel()
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("Stop() took %s with timeout_ms=0 optimizer", elapsed)
	}
	select {
	case status := <-requestDone:
		if status != http.StatusServiceUnavailable {
			t.Fatalf("active optimizer request status = %d, want 503", status)
		}
	case <-time.After(time.Second):
		t.Fatal("optimizer-backed request did not return after Stop")
	}

	w := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(requestBody))
	req.Header.Set("Content-Type", "application/json")
	srv.httpServer.Handler.ServeHTTP(w, req)
	if got := w.Result().StatusCode; got != http.StatusServiceUnavailable {
		t.Fatalf("post-drain request status = %d, want 503", got)
	}
	time.Sleep(25 * time.Millisecond)
	data, err := os.ReadFile(markerPath)
	if err != nil {
		t.Fatalf("read optimizer marker: %v", err)
	}
	if len(data) != 1 {
		t.Fatalf("optimizer calls after drain = %d, want 1 total", len(data))
	}
	if got := upstreamCalls.Load(); got != 0 {
		t.Fatalf("upstream calls = %d, want 0", got)
	}
}

func TestStopInterruptsAdmittedPartialRequestBody(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	stopped := false
	t.Cleanup(func() {
		if stopped {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	})

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if _, err := fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: 1000000\r\n\r\n{", srv.Addr()); err != nil {
		t.Fatalf("write partial request: %v", err)
	}

	deadline := time.Now().Add(time.Second)
	for {
		w := httptest.NewRecorder()
		srv.proxyHandler.HandleStatsJSON(w, httptest.NewRequest(http.MethodGet, "/stats.json", nil))
		var stats struct {
			Inflight int64 `json:"inflight"`
		}
		if err := json.NewDecoder(w.Result().Body).Decode(&stats); err != nil {
			t.Fatalf("decode stats: %v", err)
		}
		if stats.Inflight == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("partial request was not admitted; inflight=%d", stats.Inflight)
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	start := time.Now()
	err = srv.Stop(ctx)
	cancel()
	stopped = true
	if err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	if elapsed := time.Since(start); elapsed >= 1500*time.Millisecond {
		t.Fatalf("Stop() took %s with a partial request body", elapsed)
	}
}

func TestShutdownAdmissionRejectsBodyRequestBeforeClosingConnection(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	}()

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	srv.proxyHandler.BeginShutdown()
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`
	if _, err := fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", srv.Addr(), len(body), body); err != nil {
		t.Fatalf("write request: %v", err)
	}
	if err := conn.SetReadDeadline(time.Now().Add(time.Second)); err != nil {
		t.Fatalf("set read deadline: %v", err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(conn), &http.Request{Method: http.MethodPost})
	if err != nil {
		t.Fatalf("read shutdown response: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	responseBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503: %s", resp.StatusCode, responseBody)
	}
	if got := resp.Header.Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if !bytes.Contains(responseBody, []byte("server shutting down")) {
		t.Fatalf("shutdown detail missing: %s", responseBody)
	}
}

func TestStopClosesIdleUpstreamConnections(t *testing.T) {
	var mu sync.Mutex
	states := make(map[net.Conn]http.ConnState)
	upstream := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"chatcmpl-idle","object":"chat.completion","choices":[]}`)
	}))
	upstream.Config.ConnState = func(conn net.Conn, state http.ConnState) {
		mu.Lock()
		defer mu.Unlock()
		if state == http.StateClosed || state == http.StateHijacked {
			delete(states, conn)
			return
		}
		states[conn] = state
	}
	upstream.Start()
	defer upstream.Close()

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
		WithProxyOptions(copilotChatProxyOptionWithModelDiscovery(upstream.URL)),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	resp, err := http.Post(
		"http://"+srv.Addr()+"/v1/chat/completions",
		"application/json",
		strings.NewReader(`{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`),
	)
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status = %d, want 200", resp.StatusCode)
	}

	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		idle := len(states) == 1
		for _, state := range states {
			idle = idle && state == http.StateIdle
		}
		mu.Unlock()
		if idle {
			break
		}
		if time.Now().After(deadline) {
			mu.Lock()
			t.Fatalf("upstream connection did not become idle: %v", states)
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	if err := srv.Stop(ctx); err != nil {
		cancel()
		t.Fatalf("Stop() error = %v", err)
	}
	cancel()

	deadline = time.Now().Add(time.Second)
	for {
		mu.Lock()
		remaining := len(states)
		mu.Unlock()
		if remaining == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("idle upstream connections after Stop = %d, want 0", remaining)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestStopForceClosesNonReadingDownstreamAfterDeadline(t *testing.T) {
	started := make(chan struct{})
	var startedOnce sync.Once
	var written atomic.Int64
	chunk := bytes.Repeat([]byte("x"), 64<<10)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/octet-stream")
		w.WriteHeader(http.StatusOK)
		startedOnce.Do(func() { close(started) })
		for {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			n, err := w.Write(chunk)
			written.Add(int64(n))
			if err != nil {
				return
			}
			if flusher, ok := w.(http.Flusher); ok {
				flusher.Flush()
			}
		}
	}))
	defer upstream.Close()

	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
		WithProxyOptions(copilotChatProxyOptionWithModelDiscovery(upstream.URL)),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	conn, err := net.Dial("tcp", srv.Addr())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if tcpConn, ok := conn.(*net.TCPConn); ok {
		_ = tcpConn.SetReadBuffer(1024)
	}
	body := `{"model":"gpt-5","messages":[{"role":"user","content":"hello"}]}`
	if _, err := fmt.Fprintf(conn, "POST /v1/chat/completions HTTP/1.1\r\nHost: %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", srv.Addr(), len(body), body); err != nil {
		t.Fatalf("write request: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for upstream response")
	}
	deadline := time.Now().Add(2 * time.Second)
	for written.Load() < 9<<19 {
		if time.Now().After(deadline) {
			t.Fatalf("upstream wrote %d bytes, want enough to backpressure downstream", written.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	start := time.Now()
	stopErr := srv.Stop(ctx)
	cancel()
	if stopErr != nil && !errors.Is(stopErr, context.DeadlineExceeded) {
		t.Fatalf("Stop() error = %v, want nil or deadline exceeded", stopErr)
	}
	if elapsed := time.Since(start); elapsed >= 500*time.Millisecond {
		t.Fatalf("Stop() took %s after forced-close deadline", elapsed)
	}

	deadline = time.Now().Add(time.Second)
	for {
		w := httptest.NewRecorder()
		srv.proxyHandler.HandleStatsJSON(w, httptest.NewRequest(http.MethodGet, "/stats.json", nil))
		var stats struct {
			Inflight int64 `json:"inflight"`
		}
		if err := json.NewDecoder(w.Result().Body).Decode(&stats); err != nil {
			t.Fatalf("decode stats: %v", err)
		}
		if stats.Inflight == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("inflight after forced close = %d, want 0", stats.Inflight)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestConcurrentStopShortWaiterCannotForceCloseOwnerShutdown(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	srv.httpServer.Handler = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusNoContent)
	})
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}

	requestDone := make(chan error, 1)
	go func() {
		resp, err := http.Get("http://" + srv.Addr())
		if err == nil {
			_ = resp.Body.Close()
		}
		requestDone <- err
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for blocking handler")
	}

	ownerDone := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		ownerDone <- srv.Stop(ctx)
	}()

	deadline := time.Now().Add(time.Second)
	for srv.IsRunning() {
		if time.Now().After(deadline) {
			t.Fatal("owner Stop did not start shutdown")
		}
		time.Sleep(time.Millisecond)
	}

	shortCtx, cancelShort := context.WithTimeout(context.Background(), 30*time.Millisecond)
	shortErr := srv.Stop(shortCtx)
	cancelShort()
	if !errors.Is(shortErr, context.DeadlineExceeded) {
		t.Fatalf("short waiter Stop() error = %v, want deadline exceeded", shortErr)
	}
	select {
	case err := <-requestDone:
		t.Fatalf("short waiter force-closed owner's active request: %v", err)
	default:
	}

	close(release)
	select {
	case err := <-ownerDone:
		if err != nil {
			t.Fatalf("owner Stop() error = %v, want graceful nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("owner Stop did not complete after handler release")
	}
	select {
	case err := <-requestDone:
		if err != nil {
			t.Fatalf("graceful request error = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("request did not complete after release")
	}

	alreadyCanceled, cancelAlready := context.WithCancel(context.Background())
	cancelAlready()
	if err := srv.Stop(alreadyCanceled); err != nil {
		t.Fatalf("completed Stop with canceled waiter context = %v, want shared nil result", err)
	}
}

func TestDoneReportsNormalShutdown(t *testing.T) {
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := srv.Start(); err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := srv.Stop(ctx); err != nil {
		t.Fatalf("Stop() error = %v", err)
	}
	select {
	case err := <-srv.Done():
		if err != nil {
			t.Fatalf("Done() error = %v, want nil", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Done() did not report server completion")
	}
}

func TestInboundAuthProtectsLaunchRoutes(t *testing.T) {
	cfg := proxy.ProvidersConfig{Providers: []proxy.ProviderConfig{{
		ID:             "local",
		Type:           "openai-compatible",
		BaseURL:        "http://127.0.0.1:9/v1",
		AuthType:       "none",
		ModelDiscovery: "static",
		Models: []proxy.ProviderModelConfig{{
			PublicID:  "test-model",
			Endpoints: []string{"/chat/completions"},
		}},
	}}}
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelError, io.Discard),
		"127.0.0.1",
		"0",
		WithInboundAuthToken("session-token"),
		WithProxyOptions(proxy.WithProvidersConfig(cfg)),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	tests := []struct {
		name       string
		path       string
		headerName string
		header     string
		wantStatus int
	}{
		{name: "health exempt", path: "/healthz", wantStatus: http.StatusOK},
		{name: "missing token", path: "/v1/models", wantStatus: http.StatusUnauthorized},
		{name: "wrong token", path: "/v1/models", headerName: "Authorization", header: "Bearer wrong", wantStatus: http.StatusUnauthorized},
		{name: "bearer token", path: "/v1/models", headerName: "Authorization", header: "Bearer session-token", wantStatus: http.StatusOK},
		{name: "anthropic key", path: "/v1/models", headerName: "x-api-key", header: "session-token", wantStatus: http.StatusOK},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodGet, tc.path, nil)
			if tc.headerName != "" {
				req.Header.Set(tc.headerName, tc.header)
			}
			recorder := httptest.NewRecorder()
			srv.httpServer.Handler.ServeHTTP(recorder, req)
			if recorder.Code != tc.wantStatus {
				t.Fatalf("status = %d, want %d; body=%s", recorder.Code, tc.wantStatus, recorder.Body.String())
			}
			if got := recorder.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Fatalf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}

type blockingStatusWriter struct {
	header      http.Header
	started     chan struct{}
	release     chan struct{}
	startedOnce sync.Once
	releaseOnce sync.Once
	status      int
}

func newBlockingStatusWriter() *blockingStatusWriter {
	return &blockingStatusWriter{
		header:  make(http.Header),
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
}

func (w *blockingStatusWriter) Header() http.Header { return w.header }

func (w *blockingStatusWriter) WriteHeader(status int) {
	if w.status != 0 {
		return
	}
	w.startedOnce.Do(func() { close(w.started) })
	<-w.release
	w.status = status
}

func (w *blockingStatusWriter) Write(body []byte) (int, error) {
	if w.status == 0 {
		w.WriteHeader(http.StatusOK)
	}
	return len(body), nil
}

func (w *blockingStatusWriter) unblock() {
	w.releaseOnce.Do(func() { close(w.release) })
}

// Auth runs inside withRequestLog so the rejection is logged. It must not also be counted:
// an unauthenticated caller reached no inference handler, and letting it move request-rate,
// latency, error, or live in-flight dashboards hands a stats lever to anyone who can open a socket.
func TestInboundAuthRejectionIsLoggedButNotCounted(t *testing.T) {
	var logs bytes.Buffer
	srv, err := New(
		auth.NewTestAuthenticator("test-token"),
		logger.NewWithWriter(logger.LevelInfo, &logs),
		"127.0.0.1", "0",
		WithInboundAuthToken("secret-token"),
	)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	readStats := func() map[string]any {
		t.Helper()
		statsReq := httptest.NewRequest(http.MethodGet, "/stats.json", nil)
		statsReq.Header.Set("x-api-key", "secret-token")
		stats := httptest.NewRecorder()
		srv.httpServer.Handler.ServeHTTP(stats, statsReq)
		body := stats.Body.String()
		if stats.Code != http.StatusOK {
			t.Fatalf("stats.json = %d, want 200: %s", stats.Code, body)
		}
		var snap map[string]any
		if err := json.Unmarshal([]byte(body), &snap); err != nil {
			t.Fatalf("decode stats.json: %v (%s)", err, body)
		}
		return snap
	}

	rejected := newBlockingStatusWriter()
	t.Cleanup(rejected.unblock)
	done := make(chan struct{})
	go func() {
		defer close(done)
		srv.httpServer.Handler.ServeHTTP(rejected,
			httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("{}")))
	}()
	select {
	case <-rejected.started:
	case <-time.After(time.Second):
		t.Fatal("unauthorized response did not reach WriteHeader")
	}
	if inflight, ok := readStats()["inflight"].(float64); !ok || inflight != 0 {
		t.Fatalf("inflight during blocked unauthorized response = %v, want 0", inflight)
	}
	rejected.unblock()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("unauthorized request did not finish after response release")
	}
	if rejected.status != http.StatusUnauthorized {
		t.Fatalf("StatusCode = %d, want 401", rejected.status)
	}
	// Guard: if the rejection stopped being logged this test would pass for the wrong reason.
	if entry := requestLogEntryForPath(t, logs.String(), "request completed", "/v1/chat/completions"); entry["status"].(float64) != 401 {
		t.Fatalf("the 401 was not logged: %#v", entry)
	}

	snap := readStats()
	totals, ok := snap["totals"].(map[string]any)
	if !ok {
		t.Fatalf("stats.json has no totals object; this test would assert nothing: %#v", snap)
	}
	total, ok := totals["requests"].(float64)
	if !ok {
		t.Fatalf("totals.requests missing or not a number; this test would assert nothing: %#v", snap)
	}
	if total != 0 {
		t.Fatalf("totals.requests = %v, want 0: an unauthenticated request was counted", total)
	}
}
