package auth

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestGetToken_ReturnsCachedToken(t *testing.T) {
	a := NewTestAuthenticator("cached-token")
	token, err := a.GetToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "cached-token" {
		t.Errorf("expected cached-token, got %q", token)
	}
}

func TestGetToken_RefreshesExpiredToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(CopilotTokenResponse{
			Token:     "refreshed-token",
			ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	// Write access token to disk so refreshToken finds it
	if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("valid-access-token"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := &Authenticator{
		copilotToken:   "expired-token",
		tokenExpiry:    time.Now().Add(-1 * time.Hour),
		client:         server.Client(),
		copilotBaseURL: server.URL,
		tokenDir:       dir,
	}

	token, err := a.GetToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "refreshed-token" {
		t.Errorf("expected refreshed-token, got %q", token)
	}
}

func TestGetToken_LoadsPersistedCopilotToken(t *testing.T) {
	dir := t.TempDir()
	data, err := json.Marshal(CopilotTokenResponse{
		Token:     "persisted-token",
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api-key.json"), data, 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	a := &Authenticator{tokenDir: dir}

	token, err := a.GetToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "persisted-token" {
		t.Errorf("expected persisted-token, got %q", token)
	}
}

func TestGetToken_EnvAccessTokenOverridesPersistedState(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "env-access-token")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token env-access-token" {
			t.Errorf("expected 'token env-access-token', got %q", got)
		}
		_ = json.NewEncoder(w).Encode(CopilotTokenResponse{
			Token:     "env-copilot-token",
			ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	data, err := json.Marshal(CopilotTokenResponse{
		Token:     "persisted-token",
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api-key.json"), data, 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	a := &Authenticator{
		tokenDir:       dir,
		accessToken:    "stale-access-token",
		copilotToken:   "stale-copilot-token",
		tokenExpiry:    time.Now().Add(1 * time.Hour),
		client:         server.Client(),
		copilotBaseURL: server.URL,
	}

	token, err := a.GetToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "env-copilot-token" {
		t.Errorf("expected env-copilot-token, got %q", token)
	}
}

func TestGetToken_EnvAccessTokenCachesInMemory(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "env-access-token")

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		_ = json.NewEncoder(w).Encode(CopilotTokenResponse{
			Token:     "env-copilot-token",
			ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
		})
	}))
	defer server.Close()

	a := &Authenticator{
		tokenDir:       t.TempDir(),
		client:         server.Client(),
		copilotBaseURL: server.URL,
	}

	for range 2 {
		token, err := a.GetToken(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if token != "env-copilot-token" {
			t.Errorf("expected env-copilot-token, got %q", token)
		}
	}
	if calls != 1 {
		t.Fatalf("expected exactly 1 token exchange, got %d", calls)
	}
}

func TestGetToken_EnvAccessTokenFallsBackToDirectBearer(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "github_pat_fine_grained")

	var exchangeCalls, userCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/copilot_internal/v2/token":
			exchangeCalls++
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"message":"Not Found","status":404}`))
		case "/copilot_internal/user":
			userCalls++
			if got := r.Header.Get("Authorization"); got != "Bearer github_pat_fine_grained" {
				t.Errorf("expected 'Bearer github_pat_fine_grained', got %q", got)
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request path %q", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	a := &Authenticator{
		tokenDir:       t.TempDir(),
		client:         server.Client(),
		copilotBaseURL: server.URL,
	}

	token, err := a.GetToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "github_pat_fine_grained" {
		t.Errorf("expected the env token used as a direct bearer, got %q", token)
	}
	if exchangeCalls != 1 || userCalls != 1 {
		t.Fatalf("exchange calls = %d, user validation calls = %d, want 1 and 1", exchangeCalls, userCalls)
	}
}

func TestGetToken_EnvAccessTokenBearerRejectionKeepsExchangeError(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "rejected-token")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"message":"Not Found"}`))
	}))
	defer server.Close()

	a := &Authenticator{
		tokenDir:       t.TempDir(),
		client:         server.Client(),
		copilotBaseURL: server.URL,
	}

	if _, err := a.GetToken(context.Background()); err == nil || !strings.Contains(err.Error(), "404") {
		t.Fatalf("expected the original exchange error, got %v", err)
	}
}

func TestUseEnvAccessTokenAsBearer_CanceledAfterValidationDoesNotCommit(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	previousExpiry := time.Now().Add(time.Hour)
	a := &Authenticator{
		copilotToken:   "previous-token",
		tokenExpiry:    previousExpiry,
		copilotBaseURL: "http://copilot.test",
		client: &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Body:       &cancelAfterJSONReadCloser{cancel: cancel},
			}, nil
		})},
	}

	if err := a.useEnvAccessTokenAsBearer(ctx, "environment-token"); !errors.Is(err, context.Canceled) {
		t.Fatalf("useEnvAccessTokenAsBearer() error = %v, want context.Canceled", err)
	}
	if a.copilotToken != "previous-token" {
		t.Fatalf("copilot token = %q, want previous-token", a.copilotToken)
	}
	if !a.tokenExpiry.Equal(previousExpiry) {
		t.Fatalf("token expiry = %v, want %v", a.tokenExpiry, previousExpiry)
	}
}

func TestGetToken_ConcurrentAccess(t *testing.T) {
	a := NewTestAuthenticator("concurrent-token")

	var wg sync.WaitGroup
	errs := make(chan error, 10)
	for range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := a.GetToken(context.Background())
			if err != nil {
				errs <- err
				return
			}
			if token != "concurrent-token" {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("concurrent GetToken error: %v", err)
	}
}

func TestGetTokenNonInteractiveReturnsWhileDeviceFlowPending(t *testing.T) {
	deviceCodeRequested := make(chan struct{})
	var closeDeviceCodeRequested sync.Once

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/device/code":
			closeDeviceCodeRequested.Do(func() { close(deviceCodeRequested) })
			_ = json.NewEncoder(w).Encode(DeviceCodeResponse{
				DeviceCode:      "dc_pending",
				UserCode:        "PEND-1234",
				VerificationURI: "https://github.com/login/device",
				ExpiresIn:       60,
				Interval:        1,
			})
		case "/login/oauth/access_token":
			_ = json.NewEncoder(w).Encode(AccessTokenResponse{Error: "authorization_pending"})
		case "/copilot_internal/v2/token":
			t.Fatalf("unexpected Copilot token exchange before authorization")
		default:
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	a := &Authenticator{
		client:         server.Client(),
		copilotBaseURL: server.URL,
		tokenDir:       t.TempDir(),
		githubCLIPath:  missingGitHubCLIPath(t),
	}
	a.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rewritten, _ := url.Parse(server.URL + req.URL.Path)
		rewritten.RawQuery = req.URL.RawQuery
		req.URL = rewritten
		return http.DefaultTransport.RoundTrip(req)
	})

	interactiveCtx, cancelInteractive := context.WithCancel(context.Background())
	defer cancelInteractive()
	interactiveDone := make(chan error, 1)
	go func() {
		_, err := a.GetToken(interactiveCtx)
		interactiveDone <- err
	}()

	select {
	case <-deviceCodeRequested:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for device-code flow to start")
	}

	nonInteractiveDone := make(chan error, 1)
	go func() {
		_, err := a.GetTokenNonInteractive(context.Background())
		nonInteractiveDone <- err
	}()

	select {
	case err := <-nonInteractiveDone:
		if !errors.Is(err, ErrNotAuthenticated) {
			t.Fatalf("GetTokenNonInteractive error = %v, want ErrNotAuthenticated", err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("GetTokenNonInteractive blocked behind the pending device-code login")
	}

	cancelInteractive()
	select {
	case err := <-interactiveDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("interactive GetToken error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for interactive GetToken to exit")
	}
}

func TestExchangeForCopilotToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "token test-access" {
			t.Errorf("expected 'token test-access', got %q", r.Header.Get("Authorization"))
		}
		_ = json.NewEncoder(w).Encode(CopilotTokenResponse{
			Token:     "copilot-tok-123",
			ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	a := &Authenticator{
		accessToken:    "test-access",
		client:         server.Client(),
		copilotBaseURL: server.URL,
		tokenDir:       dir,
	}

	err := a.exchangeForCopilotToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.copilotToken != "copilot-tok-123" {
		t.Errorf("expected copilot-tok-123, got %q", a.copilotToken)
	}
}

func TestExchangeForCopilotToken_EmptyToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(CopilotTokenResponse{
			ErrorDetails: "invalid access token",
		})
	}))
	defer server.Close()

	a := &Authenticator{
		accessToken:    "bad-token",
		client:         server.Client(),
		copilotBaseURL: server.URL,
		tokenDir:       t.TempDir(),
	}

	err := a.exchangeForCopilotToken(context.Background())
	if err == nil {
		t.Fatal("expected error for empty copilot token")
	}
}

func TestSaveAndLoadAccessToken(t *testing.T) {
	dir := t.TempDir()
	a := &Authenticator{
		tokenDir:    dir,
		accessToken: "my-access-token",
	}

	if err := a.saveAccessToken(); err != nil {
		t.Fatalf("save error: %v", err)
	}

	a2 := &Authenticator{tokenDir: dir}
	if err := a2.loadAccessToken(); err != nil {
		t.Fatalf("load error: %v", err)
	}
	if a2.accessToken != "my-access-token" {
		t.Errorf("expected my-access-token, got %q", a2.accessToken)
	}
}

func TestLoadAccessToken_Missing(t *testing.T) {
	a := &Authenticator{tokenDir: t.TempDir()}
	if err := a.loadAccessToken(); err == nil {
		t.Fatal("expected error for missing access token file")
	}
}

func TestLookupAccessTokenFromEnv_UsesCopilotTokenVar(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "copilot-token")

	token, name := lookupAccessTokenFromEnv()
	if token != "copilot-token" {
		t.Fatalf("expected copilot-token, got %q", token)
	}
	if name != "COPILOT_GITHUB_TOKEN" {
		t.Fatalf("expected COPILOT_GITHUB_TOKEN, got %q", name)
	}
}

func TestSaveAndLoadCopilotToken(t *testing.T) {
	dir := t.TempDir()
	expiry := time.Now().Add(1 * time.Hour)
	a := &Authenticator{
		tokenDir:     dir,
		copilotToken: "copilot-token-abc",
		tokenExpiry:  expiry,
	}

	if err := a.saveCopilotToken(); err != nil {
		t.Fatalf("save error: %v", err)
	}

	// Verify file exists and is valid JSON
	data, err := os.ReadFile(filepath.Join(dir, "api-key.json"))
	if err != nil {
		t.Fatalf("read error: %v", err)
	}
	var resp CopilotTokenResponse
	if err := json.Unmarshal(data, &resp); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if resp.Token != "copilot-token-abc" {
		t.Errorf("expected copilot-token-abc, got %q", resp.Token)
	}

	a2 := &Authenticator{tokenDir: dir}
	if err := a2.loadCopilotToken(); err != nil {
		t.Fatalf("load error: %v", err)
	}
	if a2.copilotToken != "copilot-token-abc" {
		t.Errorf("expected copilot-token-abc, got %q", a2.copilotToken)
	}
}

func TestLoadCopilotToken_Expired(t *testing.T) {
	dir := t.TempDir()
	resp := CopilotTokenResponse{
		Token:     "expired-token",
		ExpiresAt: time.Now().Add(-1 * time.Hour).Unix(),
	}
	data, _ := json.Marshal(resp)
	if err := os.WriteFile(filepath.Join(dir, "api-key.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	a := &Authenticator{tokenDir: dir}
	if err := a.loadCopilotToken(); err == nil {
		t.Fatal("expected error for expired copilot token")
	}
}

func TestNewAuthenticator_DefaultDir(t *testing.T) {
	a, err := NewAuthenticator("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.tokenDir == "" {
		t.Fatal("expected tokenDir to be set")
	}
}

func TestNewAuthenticator_CustomDir(t *testing.T) {
	dir := t.TempDir()
	a, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.tokenDir != dir {
		t.Errorf("expected %q, got %q", dir, a.tokenDir)
	}
}

func TestIsSignedIn_NoToken(t *testing.T) {
	a := &Authenticator{tokenDir: t.TempDir()}
	if a.IsSignedIn() {
		t.Error("expected IsSignedIn() == false with no token")
	}
}

func TestIsSignedIn_WithEnvToken(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "env-access-token")

	a := &Authenticator{tokenDir: t.TempDir()}
	if !a.IsSignedIn() {
		t.Error("expected IsSignedIn() == true with env token")
	}
}

func missingGitHubCLIPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "missing-gh")
}

func TestGetToken_IgnoresGenericGitHubEnvVars(t *testing.T) {
	t.Setenv("GH_TOKEN", "generic-gh-token")
	t.Setenv("GITHUB_TOKEN", "generic-github-token")

	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		t.Fatalf("unexpected token exchange using %q", r.Header.Get("Authorization"))
	}))
	defer server.Close()

	dir := t.TempDir()
	data, err := json.Marshal(CopilotTokenResponse{
		Token:     "persisted-token",
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api-key.json"), data, 0o600); err != nil {
		t.Fatalf("write token file: %v", err)
	}

	a := &Authenticator{
		tokenDir:       dir,
		githubCLIPath:  missingGitHubCLIPath(t),
		client:         server.Client(),
		copilotBaseURL: server.URL,
	}

	token, err := a.GetToken(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "persisted-token" {
		t.Fatalf("expected persisted-token, got %q", token)
	}
	if calls != 0 {
		t.Fatalf("expected no token exchanges, got %d", calls)
	}
}

func TestIsSignedIn_WithDiskToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("ghu_xxxx"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Authenticator{tokenDir: dir}
	if !a.IsSignedIn() {
		t.Error("expected IsSignedIn() == true with disk token")
	}
}

func TestIsSignedIn_WithDiskCopilotToken(t *testing.T) {
	dir := t.TempDir()
	data, err := json.Marshal(CopilotTokenResponse{
		Token:     "persisted-token",
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api-key.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}

	a := &Authenticator{tokenDir: dir}
	if !a.IsSignedIn() {
		t.Error("expected IsSignedIn() == true with valid copilot token on disk")
	}
}

func TestSignOut_ClearsTokens(t *testing.T) {
	dir := t.TempDir()
	// Write both token files
	if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("ghu_xxxx"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api-key.json"), []byte(`{"token":"tok"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	writeAuthPreferencesForTest(t, dir, true)

	a := &Authenticator{
		tokenDir:     dir,
		accessToken:  "ghu_xxxx",
		copilotToken: "tok",
		tokenExpiry:  time.Now().Add(1 * time.Hour),
	}

	if err := a.SignOut(); err != nil {
		t.Fatalf("SignOut error: %v", err)
	}

	// Memory cleared
	if a.accessToken != "" {
		t.Errorf("accessToken not cleared: %q", a.accessToken)
	}
	if a.copilotToken != "" {
		t.Errorf("copilotToken not cleared: %q", a.copilotToken)
	}
	if !a.tokenExpiry.IsZero() {
		t.Errorf("tokenExpiry not cleared: %v", a.tokenExpiry)
	}

	// Disk cleared
	if _, err := os.Stat(filepath.Join(dir, "access-token")); !os.IsNotExist(err) {
		t.Error("access-token file still exists")
	}
	if _, err := os.Stat(filepath.Join(dir, "api-key.json")); !os.IsNotExist(err) {
		t.Error("api-key.json file still exists")
	}
	if _, err := os.Stat(filepath.Join(dir, signedOutMarkerFile)); err != nil {
		t.Fatalf("signed-out marker was not written: %v", err)
	}
	if prefs := readAuthPreferencesForTest(t, dir); prefs.GitHubCLIAutoSignIn {
		t.Fatal("expected sign out to disable GitHub CLI auto sign-in")
	}
}

func TestSignOut_Idempotent(t *testing.T) {
	dir := t.TempDir()
	a := &Authenticator{tokenDir: dir}
	// Calling SignOut when no files exist should not error
	if err := a.SignOut(); err != nil {
		t.Fatalf("SignOut on empty dir should not error: %v", err)
	}
	if err := a.SignOut(); err != nil {
		t.Fatalf("second SignOut should not error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, signedOutMarkerFile)); err != nil {
		t.Fatalf("signed-out marker was not written: %v", err)
	}
}

func TestRequestDeviceCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if ct := r.Header.Get("Content-Type"); ct != "application/x-www-form-urlencoded" {
			t.Errorf("unexpected content-type: %s", ct)
		}
		_ = json.NewEncoder(w).Encode(DeviceCodeResponse{
			DeviceCode:      "dc_test123",
			UserCode:        "ABCD-1234",
			VerificationURI: "https://github.com/login/device",
			ExpiresIn:       900,
			Interval:        5,
		})
	}))
	defer server.Close()

	// We need to override the deviceCodeURL for testing. Since it's a const,
	// we create a custom HTTP client that redirects to our test server.
	a := &Authenticator{
		client: server.Client(),
	}
	// We'll use a transport that rewrites the URL to our test server.
	a.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.URL, _ = url.Parse(server.URL + req.URL.Path)
		return http.DefaultTransport.RoundTrip(req)
	})

	dcResp, err := a.RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dcResp.UserCode != "ABCD-1234" {
		t.Errorf("expected ABCD-1234, got %q", dcResp.UserCode)
	}
	if dcResp.DeviceCode != "dc_test123" {
		t.Errorf("expected dc_test123, got %q", dcResp.DeviceCode)
	}
}

func TestRequestDeviceCode_RetriesWithoutLoopbackProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://[::1]:1337")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(DeviceCodeResponse{
			DeviceCode:      "dc_retry",
			UserCode:        "RETRY-1234",
			VerificationURI: "https://github.com/login/device",
			ExpiresIn:       900,
			Interval:        5,
		})
	}))
	defer server.Close()

	proxyAttempts := 0
	directAttempts := 0

	proxyClient := &http.Client{
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			proxyAttempts++
			return nil, &url.Error{
				Op:  req.Method,
				URL: req.URL.String(),
				Err: errors.New("proxyconnect tcp: dial tcp [::1]:1337: connect: connection refused"),
			}
		}),
	}

	directClient := server.Client()
	directClient.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		directAttempts++
		req.URL, _ = url.Parse(server.URL + req.URL.Path)
		return http.DefaultTransport.RoundTrip(req)
	})

	a := &Authenticator{
		client:       proxyClient,
		directClient: directClient,
	}

	dcResp, err := a.RequestDeviceCode(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if dcResp.UserCode != "RETRY-1234" {
		t.Errorf("expected RETRY-1234, got %q", dcResp.UserCode)
	}
	if proxyAttempts != 1 {
		t.Errorf("expected 1 proxy attempt, got %d", proxyAttempts)
	}
	if directAttempts != 1 {
		t.Errorf("expected 1 direct retry, got %d", directAttempts)
	}
}

func TestRequestDeviceCode_DoesNotRetryWithoutLoopbackProxy(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy.example.com:8080")

	directAttempted := false
	a := &Authenticator{
		client: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				return nil, &url.Error{
					Op:  req.Method,
					URL: req.URL.String(),
					Err: errors.New("proxyconnect tcp: dial tcp proxy.example.com:8080: connect: connection refused"),
				}
			}),
		},
		directClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				directAttempted = true
				return nil, errors.New("direct client should not be called")
			}),
		},
	}

	_, err := a.RequestDeviceCode(context.Background())
	if err == nil {
		t.Fatal("expected proxy error")
	}
	if directAttempted {
		t.Fatal("expected no direct retry for non-loopback proxy")
	}
}

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type cancelAfterJSONReadCloser struct {
	cancel context.CancelFunc
	read   bool
}

func (r *cancelAfterJSONReadCloser) Read(p []byte) (int, error) {
	if r.read {
		return 0, io.EOF
	}
	r.read = true
	n := copy(p, `{}`)
	r.cancel()
	return n, io.EOF
}

func (*cancelAfterJSONReadCloser) Close() error { return nil }

func writeAuthPreferencesForTest(t *testing.T, dir string, enabled bool) {
	t.Helper()
	data, err := json.Marshal(AuthPreferences{GitHubCLIAutoSignIn: enabled})
	if err != nil {
		t.Fatalf("marshal auth preferences: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, authPreferencesFile), data, 0o600); err != nil {
		t.Fatalf("write auth preferences: %v", err)
	}
}

func readAuthPreferencesForTest(t *testing.T, dir string) AuthPreferences {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, authPreferencesFile))
	if err != nil {
		t.Fatalf("read auth preferences: %v", err)
	}
	var prefs AuthPreferences
	if err := json.Unmarshal(data, &prefs); err != nil {
		t.Fatalf("decode auth preferences: %v", err)
	}
	return prefs
}

func writeValidCopilotCacheForTest(t *testing.T, dir, token string) {
	t.Helper()
	data, err := json.Marshal(CopilotTokenResponse{
		Token:     token,
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal copilot token cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api-key.json"), data, 0o600); err != nil {
		t.Fatalf("write copilot token cache: %v", err)
	}
}

func TestGitHubCLIAutoSignInPreference_DefaultFalse(t *testing.T) {
	dir := t.TempDir()
	a := &Authenticator{tokenDir: dir}

	if a.GitHubCLIAutoSignInEnabled() {
		t.Fatal("expected missing auth preferences to disable GitHub CLI auto sign-in")
	}
	if _, err := os.Stat(filepath.Join(dir, authPreferencesFile)); !os.IsNotExist(err) {
		t.Fatalf("expected missing preferences file to remain missing, got err=%v", err)
	}
}

func TestGitHubCLIAutoSignInPreference_SaveLoadTrue(t *testing.T) {
	dir := t.TempDir()
	a := &Authenticator{tokenDir: dir}

	if err := a.setGitHubCLIAutoSignIn(true); err != nil {
		t.Fatalf("set GitHub CLI preference: %v", err)
	}

	reloaded := &Authenticator{tokenDir: dir}
	if !reloaded.GitHubCLIAutoSignInEnabled() {
		t.Fatal("expected saved preference to enable GitHub CLI auto sign-in")
	}
}

func TestGitHubCLIAutoSignInPreference_MalformedDisablesAutoSignIn(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, authPreferencesFile), []byte(`{not json`), 0o600); err != nil {
		t.Fatalf("write malformed preferences: %v", err)
	}

	a := &Authenticator{tokenDir: dir}
	if a.GitHubCLIAutoSignInEnabled() {
		t.Fatal("expected malformed auth preferences to disable GitHub CLI auto sign-in")
	}
}

func TestStatus_ReportsAuthSources(t *testing.T) {
	t.Run("env", func(t *testing.T) {
		t.Setenv("COPILOT_GITHUB_TOKEN", "env-token")
		t.Setenv("GITHUB_COPILOT_TOKEN", "")
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("persisted-token"), 0o600); err != nil {
			t.Fatalf("write access token: %v", err)
		}
		writeValidCopilotCacheForTest(t, dir, "cached-token")

		status := (&Authenticator{tokenDir: dir}).Status()
		if !status.SignedIn || status.Source != AuthSourceEnv {
			t.Fatalf("expected env sign-in status, got %+v", status)
		}
		if !status.HasVekilAccessToken || !status.HasValidCopilotCache {
			t.Fatalf("expected status to report local token files, got %+v", status)
		}
	})

	t.Run("vekil", func(t *testing.T) {
		t.Setenv("COPILOT_GITHUB_TOKEN", "")
		t.Setenv("GITHUB_COPILOT_TOKEN", "")
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("persisted-token"), 0o600); err != nil {
			t.Fatalf("write access token: %v", err)
		}

		status := (&Authenticator{tokenDir: dir}).Status()
		if !status.SignedIn || status.Source != AuthSourceVekil || !status.HasVekilAccessToken {
			t.Fatalf("expected Vekil sign-in status, got %+v", status)
		}
	})

	t.Run("github-cli", func(t *testing.T) {
		t.Setenv("COPILOT_GITHUB_TOKEN", "")
		t.Setenv("GITHUB_COPILOT_TOKEN", "")
		dir := t.TempDir()
		writeAuthPreferencesForTest(t, dir, true)
		writeValidCopilotCacheForTest(t, dir, "cached-token")

		status := (&Authenticator{tokenDir: dir}).Status()
		if !status.SignedIn || status.Source != AuthSourceGitHubCLI || !status.GitHubCLIAutoSignIn {
			t.Fatalf("expected GitHub CLI sign-in status, got %+v", status)
		}
	})

	t.Run("github-cli configured without cache", func(t *testing.T) {
		t.Setenv("COPILOT_GITHUB_TOKEN", "")
		t.Setenv("GITHUB_COPILOT_TOKEN", "")
		dir := t.TempDir()
		writeAuthPreferencesForTest(t, dir, true)

		status := (&Authenticator{tokenDir: dir}).Status()
		if !status.SignedIn || status.Source != AuthSourceGitHubCLI || !status.GitHubCLIAutoSignIn {
			t.Fatalf("expected configured GitHub CLI sign-in status, got %+v", status)
		}
	})

	t.Run("signed-out", func(t *testing.T) {
		t.Setenv("COPILOT_GITHUB_TOKEN", "")
		t.Setenv("GITHUB_COPILOT_TOKEN", "")
		dir := t.TempDir()
		writeAuthPreferencesForTest(t, dir, true)
		if err := os.WriteFile(filepath.Join(dir, signedOutMarkerFile), []byte("signed out\n"), 0o600); err != nil {
			t.Fatalf("write signed-out marker: %v", err)
		}

		status := (&Authenticator{tokenDir: dir}).Status()
		if status.SignedIn || status.Source != AuthSourceNone || !status.SignedOut {
			t.Fatalf("expected signed-out status, got %+v", status)
		}
	})
}

func TestSignOutWaitsForPendingDeviceAuthorization(t *testing.T) {
	a := &Authenticator{tokenDir: t.TempDir()}
	if err := a.acquireDeviceFlow(context.Background()); err != nil {
		t.Fatalf("acquire device flow: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- a.SignOut()
	}()

	select {
	case err := <-done:
		t.Fatalf("SignOut returned while device flow was still active: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	a.releaseDeviceFlow()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SignOut returned error: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for SignOut after device flow released")
	}
}

func TestPollForAuthorization_Success(t *testing.T) {
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/oauth/access_token":
			call++
			if call == 1 {
				// First call: authorization_pending
				_ = json.NewEncoder(w).Encode(AccessTokenResponse{
					Error: "authorization_pending",
				})
			} else {
				// Second call: success
				_ = json.NewEncoder(w).Encode(AccessTokenResponse{
					AccessToken: "ghu_success",
				})
			}
		case "/copilot_internal/v2/token":
			_ = json.NewEncoder(w).Encode(CopilotTokenResponse{
				Token:     "copilot-tok-poll",
				ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
			})
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, signedOutMarkerFile), []byte("signed out\n"), 0o600); err != nil {
		t.Fatalf("write signed-out marker: %v", err)
	}
	a := &Authenticator{
		client:         server.Client(),
		copilotBaseURL: server.URL,
		tokenDir:       dir,
	}
	a.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		req.URL, _ = url.Parse(server.URL + req.URL.Path)
		return http.DefaultTransport.RoundTrip(req)
	})

	dcResp := &DeviceCodeResponse{
		DeviceCode: "dc_test",
		ExpiresIn:  60,
		Interval:   1,
	}

	err := a.PollForAuthorization(context.Background(), dcResp)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.accessToken != "ghu_success" {
		t.Errorf("expected ghu_success, got %q", a.accessToken)
	}
	if a.copilotToken != "copilot-tok-poll" {
		t.Errorf("expected copilot-tok-poll, got %q", a.copilotToken)
	}

	// Verify access token was saved to disk
	data, err := os.ReadFile(filepath.Join(dir, "access-token"))
	if err != nil {
		t.Fatalf("access-token not saved: %v", err)
	}
	if string(data) != "ghu_success" {
		t.Errorf("expected ghu_success on disk, got %q", string(data))
	}
	if _, err := os.Stat(filepath.Join(dir, signedOutMarkerFile)); !os.IsNotExist(err) {
		t.Fatalf("expected signed-out marker to be cleared, got err=%v", err)
	}
	if prefs := readAuthPreferencesForTest(t, dir); prefs.GitHubCLIAutoSignIn {
		t.Fatal("expected device-code authorization to disable GitHub CLI auto sign-in")
	}
}

func TestPollForAuthorizationWaiterHonorsContextDeadline(t *testing.T) {
	a := &Authenticator{tokenDir: t.TempDir()}
	if err := a.acquireDeviceFlow(context.Background()); err != nil {
		t.Fatalf("acquire device flow: %v", err)
	}
	defer a.releaseDeviceFlow()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := a.PollForAuthorization(ctx, &DeviceCodeResponse{})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("PollForAuthorization() error = %v, want context.DeadlineExceeded", err)
	}
}

func TestPollForAuthorization_Cancelled(t *testing.T) {
	a := &Authenticator{
		client:   &http.Client{Timeout: 5 * time.Second},
		tokenDir: t.TempDir(),
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // Cancel immediately

	dcResp := &DeviceCodeResponse{
		DeviceCode: "dc_cancel",
		ExpiresIn:  60,
		Interval:   1,
	}

	err := a.PollForAuthorization(ctx, dcResp)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if err != context.Canceled {
		t.Errorf("expected context.Canceled, got %v", err)
	}
}

func TestRefreshToken_DisableAutoDeviceFlow(t *testing.T) {
	a := &Authenticator{
		tokenDir:              t.TempDir(),
		client:                &http.Client{Timeout: 5 * time.Second},
		githubCLIPath:         missingGitHubCLIPath(t),
		DisableAutoDeviceFlow: true,
	}

	err := a.refreshToken(context.Background(), false)
	if err == nil {
		t.Fatal("expected error when DisableAutoDeviceFlow is true")
	}
	if !errors.Is(err, ErrNotAuthenticated) {
		t.Errorf("expected ErrNotAuthenticated, got %v", err)
	}
}

func TestRefreshToken_UsesEnvAccessTokenWithoutSavingAccessToken(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "env-access-token")

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token env-access-token" {
			t.Errorf("expected 'token env-access-token', got %q", got)
		}
		_ = json.NewEncoder(w).Encode(CopilotTokenResponse{
			Token:     "env-copilot-token",
			ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, signedOutMarkerFile), []byte("signed out\n"), 0o600); err != nil {
		t.Fatalf("write signed-out marker: %v", err)
	}
	a := &Authenticator{
		tokenDir:       dir,
		client:         server.Client(),
		copilotBaseURL: server.URL,
	}

	if err := a.refreshToken(context.Background(), false); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.accessToken != "env-access-token" {
		t.Fatalf("expected access token to be loaded from env, got %q", a.accessToken)
	}
	if _, err := os.Stat(filepath.Join(dir, "access-token")); !os.IsNotExist(err) {
		t.Fatalf("expected no access-token file to be written, got err=%v", err)
	}
}

func TestRefreshTokenNonInteractive_MissingPreferenceSkipsGitHubCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake gh shell script test is Unix-only")
	}

	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	calledPath := filepath.Join(dir, "gh-called")
	script := `#!/bin/sh
printf 'called\n' > "$CALLED_FILE"
printf 'gh-cli-access-token\n'
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("CALLED_FILE", calledPath)

	tokenDir := t.TempDir()
	a := &Authenticator{
		tokenDir:      tokenDir,
		githubCLIPath: ghPath,
		client:        &http.Client{Timeout: 5 * time.Second},
	}

	if _, err := a.RefreshTokenNonInteractive(context.Background()); err == nil {
		t.Fatal("expected missing credentials to require explicit login")
	} else if !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("expected ErrNotAuthenticated, got %v", err)
	}
	if _, err := os.Stat(calledPath); !os.IsNotExist(err) {
		t.Fatalf("expected gh not to be invoked, got err=%v", err)
	}
}

func TestRefreshTokenNonInteractive_UsesGitHubCLITokenWithoutPersistingToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake gh shell script test is Unix-only")
	}

	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	script := `#!/bin/sh
if [ "$1" != "auth" ] || [ "$2" != "token" ] || [ "$3" != "--hostname" ] || [ "$4" != "github.com" ]; then
  exit 2
fi
if [ -n "$GH_TOKEN" ] || [ -n "$GITHUB_TOKEN" ]; then
  exit 3
fi
if [ "$GH_PROMPT_DISABLED" != "1" ]; then
  exit 4
fi
printf 'gh-cli-access-token\n'
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}

	t.Setenv("GH_TOKEN", "generic-gh-token")
	t.Setenv("GITHUB_TOKEN", "generic-github-token")

	chatEnabled := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/user" {
			t.Errorf("expected copilot user validation request, got %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer gh-cli-access-token" {
			t.Errorf("expected 'Bearer gh-cli-access-token', got %q", got)
		}
		_ = json.NewEncoder(w).Encode(CopilotUserResponse{
			Login:       "test-user",
			ChatEnabled: &chatEnabled,
		})
	}))
	defer server.Close()

	tokenDir := t.TempDir()
	writeAuthPreferencesForTest(t, tokenDir, true)
	a := &Authenticator{
		tokenDir:       tokenDir,
		githubCLIPath:  ghPath,
		client:         server.Client(),
		copilotBaseURL: server.URL,
	}

	token, err := a.RefreshTokenNonInteractive(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "gh-cli-access-token" {
		t.Fatalf("expected gh-cli-access-token, got %q", token)
	}
	if a.accessToken != "" {
		t.Fatalf("expected GitHub CLI token not to be stored as access token, got %q", a.accessToken)
	}
	if _, err := os.Stat(filepath.Join(tokenDir, "access-token")); !os.IsNotExist(err) {
		t.Fatalf("expected gh access token not to be persisted, got err=%v", err)
	}

	if _, err := os.Stat(filepath.Join(tokenDir, "api-key.json")); !os.IsNotExist(err) {
		t.Fatalf("expected gh token not to be persisted in copilot token cache, got err=%v", err)
	}
}

func TestGitHubCLIAccessToken_ReturnsCommandContextTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake gh shell script test is Unix-only")
	}

	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	script := `#!/bin/sh
exec sleep 1
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}

	previousTimeout := githubCLITokenTimeout
	githubCLITokenTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		githubCLITokenTimeout = previousTimeout
	})

	a := &Authenticator{githubCLIPath: ghPath}
	_, err := a.gitHubCLIAccessToken(context.Background())
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected context deadline exceeded, got %v", err)
	}
	if errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("expected timeout not to be classified as unauthenticated, got %v", err)
	}
}

func TestSignInWithGitHubCLI_ClearsSignedOutMarkerWritesPreferenceAndDoesNotPersistToken(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake gh shell script test is Unix-only")
	}

	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	calledPath := filepath.Join(dir, "gh-called")
	script := `#!/bin/sh
if [ "$1" != "auth" ] || [ "$2" != "token" ] || [ "$3" != "--hostname" ] || [ "$4" != "github.com" ]; then
  exit 2
fi
if [ -n "$GH_TOKEN" ] || [ -n "$GITHUB_TOKEN" ]; then
  exit 3
fi
if [ "$GH_PROMPT_DISABLED" != "1" ]; then
  exit 4
fi
printf 'called\n' > "$CALLED_FILE"
printf 'gh-cli-access-token\n'
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("CALLED_FILE", calledPath)
	t.Setenv("GH_TOKEN", "generic-gh-token")
	t.Setenv("GITHUB_TOKEN", "generic-github-token")

	chatEnabled := true
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/user" {
			t.Errorf("expected copilot user validation request, got %s", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer gh-cli-access-token" {
			t.Errorf("expected 'Bearer gh-cli-access-token', got %q", got)
		}
		_ = json.NewEncoder(w).Encode(CopilotUserResponse{
			Login:       "test-user",
			ChatEnabled: &chatEnabled,
		})
	}))
	defer server.Close()

	tokenDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tokenDir, "access-token"), []byte("stale-access-token"), 0o600); err != nil {
		t.Fatalf("write stale access token: %v", err)
	}
	staleCopilotCache, err := json.Marshal(CopilotTokenResponse{
		Token:     "stale-copilot-token",
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal stale copilot cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tokenDir, "api-key.json"), staleCopilotCache, 0o600); err != nil {
		t.Fatalf("write stale copilot cache: %v", err)
	}
	if err := os.WriteFile(filepath.Join(tokenDir, signedOutMarkerFile), []byte("signed out\n"), 0o600); err != nil {
		t.Fatalf("write signed-out marker: %v", err)
	}
	writeAuthPreferencesForTest(t, tokenDir, false)

	a := &Authenticator{
		tokenDir:       tokenDir,
		githubCLIPath:  ghPath,
		client:         server.Client(),
		copilotBaseURL: server.URL,
	}

	if err := a.SignInWithGitHubCLI(context.Background()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if _, err := os.Stat(calledPath); err != nil {
		t.Fatalf("expected gh to be invoked: %v", err)
	}
	if a.accessToken != "" {
		t.Fatalf("expected GitHub CLI token not to be stored as access token, got %q", a.accessToken)
	}
	if a.copilotToken != "gh-cli-access-token" {
		t.Fatalf("expected GitHub CLI bearer token in memory, got %q", a.copilotToken)
	}
	if _, err := os.Stat(filepath.Join(tokenDir, signedOutMarkerFile)); !os.IsNotExist(err) {
		t.Fatalf("expected signed-out marker to be cleared, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(tokenDir, "access-token")); !os.IsNotExist(err) {
		t.Fatalf("expected stale access-token file to be removed, got err=%v", err)
	}
	if prefs := readAuthPreferencesForTest(t, tokenDir); !prefs.GitHubCLIAutoSignIn {
		t.Fatal("expected explicit GitHub CLI sign-in to enable auto sign-in preference")
	}

	if _, err := os.Stat(filepath.Join(tokenDir, "api-key.json")); !os.IsNotExist(err) {
		t.Fatalf("expected stale copilot token cache to be removed, got err=%v", err)
	}
}

func TestSignInWithGitHubCLI_ValidationFailureRestoresPreviousState(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake gh shell script test is Unix-only")
	}

	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	script := `#!/bin/sh
printf 'gh-cli-access-token\n'
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/user" {
			t.Errorf("expected copilot user validation request, got %s", r.URL.Path)
		}
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(githubAPIErrorResponse{Message: "Not Found", Status: "404"})
	}))
	defer server.Close()

	tokenDir := t.TempDir()
	writeAuthPreferencesForTest(t, tokenDir, false)
	a := &Authenticator{
		tokenDir:       tokenDir,
		githubCLIPath:  ghPath,
		client:         server.Client(),
		copilotBaseURL: server.URL,
		accessToken:    "previous-access-token",
		copilotToken:   "previous-copilot-token",
		tokenExpiry:    time.Now().Add(1 * time.Hour),
	}

	err := a.SignInWithGitHubCLI(context.Background())
	if err == nil {
		t.Fatal("expected validation error")
	}
	if !errors.Is(err, ErrInvalidAccessToken) {
		t.Fatalf("expected ErrInvalidAccessToken, got %v", err)
	}
	if a.accessToken != "previous-access-token" {
		t.Fatalf("expected access token to be restored, got %q", a.accessToken)
	}
	if a.copilotToken != "previous-copilot-token" {
		t.Fatalf("expected copilot token to be restored, got %q", a.copilotToken)
	}
	if prefs := readAuthPreferencesForTest(t, tokenDir); prefs.GitHubCLIAutoSignIn {
		t.Fatal("expected failed GitHub CLI sign-in not to enable auto sign-in preference")
	}
}

func TestRefreshTokenNonInteractive_SignedOutMarkerSkipsGitHubCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake gh shell script test is Unix-only")
	}

	dir := t.TempDir()
	ghPath := filepath.Join(dir, "gh")
	calledPath := filepath.Join(dir, "gh-called")
	script := `#!/bin/sh
printf 'called\n' > "$CALLED_FILE"
exit 7
`
	if err := os.WriteFile(ghPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake gh: %v", err)
	}
	t.Setenv("CALLED_FILE", calledPath)

	tokenDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(tokenDir, signedOutMarkerFile), []byte("signed out\n"), 0o600); err != nil {
		t.Fatalf("write signed-out marker: %v", err)
	}
	writeAuthPreferencesForTest(t, tokenDir, true)
	a := &Authenticator{
		tokenDir:      tokenDir,
		githubCLIPath: ghPath,
		client:        &http.Client{Timeout: 5 * time.Second},
	}

	if _, err := a.RefreshTokenNonInteractive(context.Background()); err == nil {
		t.Fatal("expected signed-out state to require explicit login")
	} else if !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("expected ErrNotAuthenticated, got %v", err)
	}
	if _, err := os.Stat(calledPath); !os.IsNotExist(err) {
		t.Fatalf("expected gh not to be invoked, got err=%v", err)
	}
}

func TestRefreshTokenNonInteractive_UsesPersistedAccessToken(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token valid-access-token" {
			t.Errorf("expected 'token valid-access-token', got %q", got)
		}
		_ = json.NewEncoder(w).Encode(CopilotTokenResponse{
			Token:     "refreshed-copilot-token",
			ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("valid-access-token"), 0o600); err != nil {
		t.Fatalf("write access token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, signedOutMarkerFile), []byte("signed out\n"), 0o600); err != nil {
		t.Fatalf("write signed-out marker: %v", err)
	}

	a := &Authenticator{
		tokenDir:       dir,
		client:         server.Client(),
		copilotBaseURL: server.URL,
	}

	token, err := a.RefreshTokenNonInteractive(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if token != "refreshed-copilot-token" {
		t.Fatalf("expected refreshed-copilot-token, got %q", token)
	}
}

func TestRefreshTokenNonInteractive_DetectsRevokedAccessTokenDespiteCachedCopilotToken(t *testing.T) {
	var calls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if got := r.Header.Get("Authorization"); got != "token revoked-access-token" {
			t.Errorf("expected 'token revoked-access-token', got %q", got)
		}
		_ = json.NewEncoder(w).Encode(CopilotTokenResponse{
			ErrorDetails: "invalid access token",
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("revoked-access-token"), 0o600); err != nil {
		t.Fatalf("write access token: %v", err)
	}
	data, err := json.Marshal(CopilotTokenResponse{
		Token:     "persisted-copilot-token",
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api-key.json"), data, 0o600); err != nil {
		t.Fatalf("write copilot token: %v", err)
	}

	a := &Authenticator{
		tokenDir:       dir,
		client:         server.Client(),
		githubCLIPath:  missingGitHubCLIPath(t),
		copilotBaseURL: server.URL,
	}

	if _, err := a.RefreshTokenNonInteractive(context.Background()); err == nil {
		t.Fatal("expected error for revoked access token")
	} else if !errors.Is(err, ErrInvalidAccessToken) {
		t.Fatalf("expected ErrInvalidAccessToken, got %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected 1 token exchange, got %d", calls)
	}
}

func TestRefreshTokenNonInteractive_RequiresGitHubAccessToken(t *testing.T) {
	dir := t.TempDir()
	data, err := json.Marshal(CopilotTokenResponse{
		Token:     "persisted-copilot-token",
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api-key.json"), data, 0o600); err != nil {
		t.Fatalf("write copilot token: %v", err)
	}

	a := &Authenticator{
		tokenDir:      dir,
		client:        &http.Client{Timeout: 5 * time.Second},
		githubCLIPath: missingGitHubCLIPath(t),
	}

	if _, err := a.RefreshTokenNonInteractive(context.Background()); err == nil {
		t.Fatal("expected error when only copilot token is persisted")
	} else if !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("expected ErrNotAuthenticated, got %v", err)
	}
}

func TestRefreshTokenNonInteractive_PreservesTransientExchangeError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(CopilotTokenResponse{
			ErrorDetails: "temporary upstream failure",
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("valid-access-token"), 0o600); err != nil {
		t.Fatalf("write access token: %v", err)
	}

	a := &Authenticator{
		tokenDir:       dir,
		client:         server.Client(),
		copilotBaseURL: server.URL,
	}

	_, err := a.RefreshTokenNonInteractive(context.Background())
	if err == nil {
		t.Fatal("expected transient refresh error")
	}
	if errors.Is(err, ErrNotAuthenticated) || errors.Is(err, ErrInvalidAccessToken) {
		t.Fatalf("expected transient error classification, got %v", err)
	}
	if got := err.Error(); got != "copilot token request failed with status 503: temporary upstream failure" {
		t.Fatalf("unexpected error: %q", got)
	}
}

func TestRefreshToken_AutoDeviceFlowReturnsTransientRefreshError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_ = json.NewEncoder(w).Encode(CopilotTokenResponse{
			ErrorDetails: "gateway unavailable",
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("valid-access-token"), 0o600); err != nil {
		t.Fatalf("write access token: %v", err)
	}

	a := &Authenticator{
		tokenDir:       dir,
		client:         server.Client(),
		copilotBaseURL: server.URL,
	}

	err := a.refreshToken(context.Background(), true)
	if err == nil {
		t.Fatal("expected refresh error")
	}
	if IsInteractiveLoginRequired(err) {
		t.Fatalf("expected transient error to bypass device flow, got %v", err)
	}
	if got := err.Error(); got != "copilot token request failed with status 502: gateway unavailable" {
		t.Fatalf("unexpected error: %q", got)
	}
}

func waitForAuthRefreshWaiters(t *testing.T, a *Authenticator, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.refreshMu.Lock()
		got := 0
		if a.refreshCall != nil {
			got = a.refreshCall.waiters
		}
		a.refreshMu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d auth refresh waiters", want)
}

func waitForAuthDeviceWaiters(t *testing.T, a *Authenticator, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		a.deviceCallMu.Lock()
		got := 0
		if a.deviceCall != nil {
			got = a.deviceCall.waiters
		}
		a.deviceCallMu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d auth device-flow waiters", want)
}

func TestGetTokenRefreshWaiterHonorsContextDeadline(t *testing.T) {
	var calls atomic.Int32
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		close(refreshStarted)
		<-releaseRefresh
		_ = json.NewEncoder(w).Encode(CopilotTokenResponse{
			Token:     "shared-token",
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
	}))
	defer server.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("valid-access-token"), 0o600); err != nil {
		t.Fatalf("write access token: %v", err)
	}
	a := &Authenticator{
		tokenDir:       dir,
		client:         server.Client(),
		copilotBaseURL: server.URL,
	}

	leaderDone := make(chan error, 1)
	go func() {
		_, err := a.GetTokenNonInteractive(context.Background())
		leaderDone <- err
	}()
	<-refreshStarted

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	waiterDone := make(chan error, 1)
	go func() {
		_, err := a.GetTokenNonInteractive(ctx)
		waiterDone <- err
	}()
	waitForAuthRefreshWaiters(t, a, 2)

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("waiter error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("refresh waiter did not honor its context deadline")
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls while leader blocked = %d, want 1", got)
	}

	close(releaseRefresh)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader error = %v", err)
	}
}

func TestGetTokenSharesFailedRefreshAcrossWaiters(t *testing.T) {
	var calls atomic.Int32
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		close(refreshStarted)
		<-releaseRefresh
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(CopilotTokenResponse{ErrorDetails: "temporary failure"})
	}))
	defer server.Close()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("valid-access-token"), 0o600); err != nil {
		t.Fatalf("write access token: %v", err)
	}
	a := &Authenticator{
		tokenDir:       dir,
		client:         server.Client(),
		copilotBaseURL: server.URL,
	}

	const callers = 8
	results := make(chan error, callers)
	go func() {
		_, err := a.GetTokenNonInteractive(context.Background())
		results <- err
	}()
	<-refreshStarted
	for range callers - 1 {
		go func() {
			_, err := a.GetTokenNonInteractive(context.Background())
			results <- err
		}()
	}
	waitForAuthRefreshWaiters(t, a, callers)
	close(releaseRefresh)

	for range callers {
		err := <-results
		if err == nil || err.Error() != "copilot token request failed with status 503: temporary failure" {
			t.Fatalf("shared refresh error = %v", err)
		}
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1 shared failed refresh", got)
	}
}

func TestGetTokenDeviceFlowWaiterHonorsContextDeadline(t *testing.T) {
	deviceCodeStarted := make(chan struct{})
	releaseDeviceCode := make(chan struct{})
	var deviceCodeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/login/device/code":
			deviceCodeCalls.Add(1)
			close(deviceCodeStarted)
			<-releaseDeviceCode
			_ = json.NewEncoder(w).Encode(DeviceCodeResponse{
				DeviceCode:      "dc_wait",
				UserCode:        "WAIT-1234",
				VerificationURI: "https://github.com/login/device",
				ExpiresIn:       60,
				Interval:        1,
			})
		default:
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
	}))
	defer server.Close()

	a := &Authenticator{
		client:         server.Client(),
		copilotBaseURL: server.URL,
		tokenDir:       t.TempDir(),
		githubCLIPath:  missingGitHubCLIPath(t),
	}
	a.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rewritten, _ := url.Parse(server.URL + req.URL.Path)
		rewritten.RawQuery = req.URL.RawQuery
		req.URL = rewritten
		return http.DefaultTransport.RoundTrip(req)
	})

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := a.GetToken(leaderCtx)
		leaderDone <- err
	}()
	<-deviceCodeStarted

	waiterCtx, cancelWaiter := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelWaiter()
	waiterDone := make(chan error, 1)
	go func() {
		_, err := a.GetToken(waiterCtx)
		waiterDone <- err
	}()
	waitForAuthDeviceWaiters(t, a, 2)

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("device-flow waiter error = %v, want context.DeadlineExceeded", err)
		}
	case <-time.After(time.Second):
		t.Fatal("device-flow waiter did not honor its context deadline")
	}
	if got := deviceCodeCalls.Load(); got != 1 {
		t.Fatalf("device-code calls while leader blocked = %d, want 1", got)
	}

	cancelLeader()
	close(releaseDeviceCode)
	select {
	case err := <-leaderDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("leader error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for device-flow leader to exit")
	}
}

func TestRefreshTokenNonInteractiveDoesNotJoinCachedLookup(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(CopilotTokenResponse{
			Token:     "refreshed-token",
			ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
	}))
	defer server.Close()

	a := &Authenticator{
		tokenDir:       t.TempDir(),
		accessToken:    "valid-access-token",
		copilotToken:   "cached-token",
		tokenExpiry:    time.Now().Add(time.Hour),
		client:         server.Client(),
		copilotBaseURL: server.URL,
	}

	// Hold the state lock so the regular lookup publishes its in-flight call
	// before it can return the cached token.
	a.mu.Lock()
	cachedResult := make(chan struct {
		token string
		err   error
	}, 1)
	go func() {
		token, err := a.GetTokenNonInteractive(context.Background())
		cachedResult <- struct {
			token string
			err   error
		}{token: token, err: err}
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.refreshMu.Lock()
		active := a.refreshCall != nil && !a.refreshCall.force
		a.refreshMu.Unlock()
		if active {
			break
		}
		if time.Now().After(deadline) {
			a.mu.Unlock()
			t.Fatal("timed out waiting for cached lookup flight")
		}
		time.Sleep(time.Millisecond)
	}

	forcedResult := make(chan struct {
		token string
		err   error
	}, 1)
	go func() {
		token, err := a.RefreshTokenNonInteractive(context.Background())
		forcedResult <- struct {
			token string
			err   error
		}{token: token, err: err}
	}()
	waitForAuthRefreshWaiters(t, a, 2)
	a.mu.Unlock()

	cached := <-cachedResult
	if cached.err != nil || cached.token != "cached-token" {
		t.Fatalf("cached lookup = (%q, %v), want cached-token", cached.token, cached.err)
	}
	forced := <-forcedResult
	if forced.err != nil || forced.token != "refreshed-token" {
		t.Fatalf("forced refresh = (%q, %v), want refreshed-token", forced.token, forced.err)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("token exchange calls = %d, want 1 forced refresh", got)
	}
}

func TestGetTokenSharesFailedDeviceFlowAcrossWaiters(t *testing.T) {
	deviceCodeStarted := make(chan struct{})
	releaseDeviceCode := make(chan struct{})
	var deviceCodeCalls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login/device/code" {
			t.Fatalf("unexpected request path: %s", r.URL.Path)
		}
		deviceCodeCalls.Add(1)
		close(deviceCodeStarted)
		<-releaseDeviceCode
		_, _ = w.Write([]byte("{"))
	}))
	defer server.Close()

	a := &Authenticator{
		client:         server.Client(),
		copilotBaseURL: server.URL,
		tokenDir:       t.TempDir(),
		githubCLIPath:  missingGitHubCLIPath(t),
	}
	a.client.Transport = roundTripFunc(func(req *http.Request) (*http.Response, error) {
		rewritten, _ := url.Parse(server.URL + req.URL.Path)
		rewritten.RawQuery = req.URL.RawQuery
		req.URL = rewritten
		return http.DefaultTransport.RoundTrip(req)
	})

	const callers = 8
	results := make(chan error, callers)
	go func() {
		_, err := a.GetToken(context.Background())
		results <- err
	}()
	<-deviceCodeStarted
	for range callers - 1 {
		go func() {
			_, err := a.GetToken(context.Background())
			results <- err
		}()
	}
	waitForAuthDeviceWaiters(t, a, callers)
	close(releaseDeviceCode)

	var sharedError string
	for range callers {
		err := <-results
		if err == nil {
			t.Fatal("device flow error = nil")
		}
		if sharedError == "" {
			sharedError = err.Error()
		} else if err.Error() != sharedError {
			t.Fatalf("device flow errors differ: %q != %q", err.Error(), sharedError)
		}
	}
	if got := deviceCodeCalls.Load(); got != 1 {
		t.Fatalf("device-code calls = %d, want 1 shared failed flow", got)
	}
}

func copilotTokenResponseForTest(t *testing.T, token string) *http.Response {
	t.Helper()
	recorder := httptest.NewRecorder()
	if err := json.NewEncoder(recorder).Encode(CopilotTokenResponse{
		Token:     token,
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	}); err != nil {
		t.Fatalf("encode Copilot token response: %v", err)
	}
	return recorder.Result()
}

func TestGetTokenRefreshLeaderCancellationDoesNotCancelLiveWaiter(t *testing.T) {
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	refreshCanceled := make(chan struct{})
	var startOnce sync.Once
	var cancelOnce sync.Once

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("valid-access-token"), 0o600); err != nil {
		t.Fatalf("write access token: %v", err)
	}
	a := &Authenticator{
		tokenDir:       dir,
		copilotBaseURL: "http://copilot.test",
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			startOnce.Do(func() { close(refreshStarted) })
			select {
			case <-releaseRefresh:
				return copilotTokenResponseForTest(t, "waiter-token"), nil
			case <-req.Context().Done():
				cancelOnce.Do(func() { close(refreshCanceled) })
				return nil, req.Context().Err()
			}
		})},
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := a.GetTokenNonInteractive(leaderCtx)
		leaderDone <- err
	}()
	<-refreshStarted

	waiterDone := make(chan struct {
		token string
		err   error
	}, 1)
	go func() {
		token, err := a.GetTokenNonInteractive(context.Background())
		waiterDone <- struct {
			token string
			err   error
		}{token: token, err: err}
	}()
	waitForAuthRefreshWaiters(t, a, 2)
	cancelLeader()

	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %v, want context.Canceled", err)
	}
	a.refreshMu.Lock()
	activeCall := a.refreshCall
	a.refreshMu.Unlock()
	if activeCall == nil {
		t.Fatal("shared Copilot refresh disappeared while a waiter remained")
	}
	select {
	case <-activeCall.ctx.Done():
		t.Fatal("shared Copilot refresh context was canceled while a waiter remained")
	default:
	}
	select {
	case <-refreshCanceled:
		t.Fatal("shared Copilot refresh was canceled while a waiter remained")
	default:
	}

	close(releaseRefresh)
	waiter := <-waiterDone
	if waiter.err != nil || waiter.token != "waiter-token" {
		t.Fatalf("waiter result = (%q, %v), want waiter-token", waiter.token, waiter.err)
	}
}

func TestGetTokenRefreshCancelsWhenAllWaitersLeave(t *testing.T) {
	refreshStarted := make(chan struct{})
	refreshCanceled := make(chan struct{})

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("valid-access-token"), 0o600); err != nil {
		t.Fatalf("write access token: %v", err)
	}
	a := &Authenticator{
		tokenDir:       dir,
		copilotBaseURL: "http://copilot.test",
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			close(refreshStarted)
			<-req.Context().Done()
			close(refreshCanceled)
			return nil, req.Context().Err()
		})},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.GetTokenNonInteractive(ctx)
		done <- err
	}()
	<-refreshStarted
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("GetTokenNonInteractive() error = %v, want context.Canceled", err)
	}
	select {
	case <-refreshCanceled:
	case <-time.After(time.Second):
		t.Fatal("underlying Copilot refresh was not canceled after its last waiter left")
	}
}

func TestGetTokenDeviceLeaderCancellationDoesNotCancelLiveWaiter(t *testing.T) {
	dir := t.TempDir()
	data, err := json.Marshal(CopilotTokenResponse{
		Token:     "device-waiter-token",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api-key.json"), data, 0o600); err != nil {
		t.Fatalf("write token cache: %v", err)
	}
	a := &Authenticator{tokenDir: dir}
	if err := a.acquireDeviceFlow(context.Background()); err != nil {
		t.Fatalf("acquire device flow: %v", err)
	}

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := a.getTokenWithDeviceFlow(leaderCtx)
		leaderDone <- err
	}()
	waitForAuthDeviceWaiters(t, a, 1)

	waiterDone := make(chan struct {
		token string
		err   error
	}, 1)
	go func() {
		token, err := a.getTokenWithDeviceFlow(context.Background())
		waiterDone <- struct {
			token string
			err   error
		}{token: token, err: err}
	}()
	waitForAuthDeviceWaiters(t, a, 2)
	cancelLeader()

	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %v, want context.Canceled", err)
	}
	a.deviceCallMu.Lock()
	activeCall := a.deviceCall
	a.deviceCallMu.Unlock()
	if activeCall == nil {
		t.Fatal("shared device-flow call disappeared while a waiter remained")
	}
	select {
	case <-activeCall.ctx.Done():
		t.Fatal("shared device-flow context was canceled while a waiter remained")
	default:
	}
	a.releaseDeviceFlow()

	waiter := <-waiterDone
	if waiter.err != nil || waiter.token != "device-waiter-token" {
		t.Fatalf("waiter result = (%q, %v), want device-waiter-token", waiter.token, waiter.err)
	}
}

func TestGetTokenDeviceFlowCancelsWhenAllWaitersLeave(t *testing.T) {
	a := &Authenticator{tokenDir: t.TempDir()}
	if err := a.acquireDeviceFlow(context.Background()); err != nil {
		t.Fatalf("acquire device flow: %v", err)
	}
	defer a.releaseDeviceFlow()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := a.getTokenWithDeviceFlow(ctx)
		done <- err
	}()
	waitForAuthDeviceWaiters(t, a, 1)
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("getTokenWithDeviceFlow() error = %v, want context.Canceled", err)
	}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		a.deviceCallMu.Lock()
		active := a.deviceCall != nil
		a.deviceCallMu.Unlock()
		if !active {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("abandoned device-flow call remained active")
}

func TestSignOutInvalidatesInFlightNonInteractiveResults(t *testing.T) {
	refreshStarted := make(chan struct{})
	refreshCanceled := make(chan struct{})

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("valid-access-token"), 0o600); err != nil {
		t.Fatalf("write access token: %v", err)
	}
	a := &Authenticator{
		tokenDir:       dir,
		copilotBaseURL: "http://copilot.test",
		client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			close(refreshStarted)
			<-req.Context().Done()
			close(refreshCanceled)
			return nil, req.Context().Err()
		})},
	}

	results := make(chan struct {
		token string
		err   error
	}, 2)
	go func() {
		token, err := a.GetTokenNonInteractive(context.Background())
		results <- struct {
			token string
			err   error
		}{token: token, err: err}
	}()
	<-refreshStarted
	go func() {
		token, err := a.GetTokenNonInteractive(context.Background())
		results <- struct {
			token string
			err   error
		}{token: token, err: err}
	}()
	waitForAuthRefreshWaiters(t, a, 2)

	if err := a.SignOut(); err != nil {
		t.Fatalf("SignOut() error = %v", err)
	}
	select {
	case <-refreshCanceled:
	default:
		t.Fatal("SignOut did not cancel the in-flight refresh")
	}
	for range 2 {
		result := <-results
		if result.token != "" || result.err == nil {
			t.Fatalf("post-sign-out result = (%q, %v), want no token and an error", result.token, result.err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "api-key.json")); !os.IsNotExist(err) {
		t.Fatalf("api-key.json exists after SignOut: %v", err)
	}
}

func TestSignOutInvalidatesInFlightDeviceResults(t *testing.T) {
	dir := t.TempDir()
	data, err := json.Marshal(CopilotTokenResponse{
		Token:     "stale-device-token",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api-key.json"), data, 0o600); err != nil {
		t.Fatalf("write token cache: %v", err)
	}
	a := &Authenticator{tokenDir: dir}
	if err := a.acquireDeviceFlow(context.Background()); err != nil {
		t.Fatalf("acquire device flow: %v", err)
	}

	results := make(chan struct {
		token string
		err   error
	}, 2)
	for range 2 {
		go func() {
			token, err := a.getTokenWithDeviceFlow(context.Background())
			results <- struct {
				token string
				err   error
			}{token: token, err: err}
		}()
	}
	waitForAuthDeviceWaiters(t, a, 2)

	signOutDone := make(chan error, 1)
	go func() { signOutDone <- a.SignOut() }()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) && !a.signingOut.Load() {
		time.Sleep(time.Millisecond)
	}
	if !a.signingOut.Load() {
		t.Fatal("SignOut did not enter invalidation state")
	}
	a.releaseDeviceFlow()
	if err := <-signOutDone; err != nil {
		t.Fatalf("SignOut() error = %v", err)
	}

	for range 2 {
		result := <-results
		if result.token != "" || result.err == nil {
			t.Fatalf("post-sign-out device result = (%q, %v), want no token and an error", result.token, result.err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "api-key.json")); !os.IsNotExist(err) {
		t.Fatalf("api-key.json exists after SignOut: %v", err)
	}
}

func TestSignOutInvalidatesCompletedSharedResults(t *testing.T) {
	a := &Authenticator{tokenDir: t.TempDir()}
	generation := a.generation.Load()
	refresh := &authTokenCall{
		done:       make(chan struct{}),
		token:      "stale-refresh-token",
		waiters:    1,
		completed:  true,
		generation: generation,
	}
	device := &authTokenCall{
		done:       make(chan struct{}),
		token:      "stale-device-token",
		waiters:    1,
		completed:  true,
		generation: generation,
	}
	close(refresh.done)
	close(device.done)

	if err := a.SignOut(); err != nil {
		t.Fatalf("SignOut() error = %v", err)
	}
	if token, err, _ := a.waitForRefreshCall(context.Background(), refresh); token != "" || !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("completed refresh result = (%q, %v), want ErrNotAuthenticated", token, err)
	}
	if token, err := a.waitForDeviceCall(context.Background(), device); token != "" || !errors.Is(err, ErrNotAuthenticated) {
		t.Fatalf("completed device result = (%q, %v), want ErrNotAuthenticated", token, err)
	}
}

func TestSignOutDetachesRefreshBeforeAllowingFreshAuth(t *testing.T) {
	for _, tt := range []struct {
		name       string
		prepareNew func(*testing.T, string)
	}{
		{
			name: "environment",
			prepareNew: func(t *testing.T, _ string) {
				t.Setenv("COPILOT_GITHUB_TOKEN", "new-env-access-token")
			},
		},
		{
			name: "persisted access token",
			prepareNew: func(t *testing.T, dir string) {
				if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("new-disk-access-token"), 0o600); err != nil {
					t.Fatalf("write replacement access token: %v", err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("COPILOT_GITHUB_TOKEN", "")
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("old-access-token"), 0o600); err != nil {
				t.Fatalf("write old access token: %v", err)
			}

			oldRequestStarted := make(chan struct{})
			oldAtFinalize := make(chan struct{})
			releaseOldFinalize := make(chan struct{})
			var requests atomic.Int32
			var finalizeCalls atomic.Int32
			a := &Authenticator{
				tokenDir:       dir,
				copilotBaseURL: "http://copilot.test",
				client: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if requests.Add(1) == 1 {
						close(oldRequestStarted)
						<-req.Context().Done()
						return nil, req.Context().Err()
					}
					return copilotTokenResponseForTest(t, "fresh-token"), nil
				})},
				beforeRefreshCallFinalize: func() {
					if finalizeCalls.Add(1) == 1 {
						close(oldAtFinalize)
						<-releaseOldFinalize
					}
				},
			}

			oldDone := make(chan error, 1)
			go func() {
				_, err := a.GetTokenNonInteractive(context.Background())
				oldDone <- err
			}()
			<-oldRequestStarted

			signOutDone := make(chan error, 1)
			go func() { signOutDone <- a.SignOut() }()
			<-oldAtFinalize
			if err := <-signOutDone; err != nil {
				t.Fatalf("SignOut() error = %v", err)
			}

			a.refreshMu.Lock()
			oldAttached := a.refreshCall != nil
			a.refreshMu.Unlock()
			if oldAttached {
				t.Fatal("old refresh call remained attached after SignOut returned")
			}

			tt.prepareNew(t, dir)
			token, err := a.GetTokenNonInteractive(context.Background())
			if err != nil || token != "fresh-token" {
				t.Fatalf("fresh auth result = (%q, %v), want fresh-token", token, err)
			}
			if got := requests.Load(); got != 2 {
				t.Fatalf("upstream requests = %d, want a fresh second request", got)
			}

			close(releaseOldFinalize)
			if err := <-oldDone; err == nil {
				t.Fatal("old refresh returned success after SignOut")
			}
		})
	}
}

func TestSignOutDetachesDeviceCallBeforeFreshNonInteractiveAuth(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	dir := t.TempDir()
	cached, err := json.Marshal(CopilotTokenResponse{
		Token:     "old-device-token",
		ExpiresAt: time.Now().Add(time.Hour).Unix(),
	})
	if err != nil {
		t.Fatalf("marshal old device token: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api-key.json"), cached, 0o600); err != nil {
		t.Fatalf("write old device token: %v", err)
	}

	oldAtFinalize := make(chan struct{})
	releaseOldFinalize := make(chan struct{})
	var requests atomic.Int32
	var finalizeCalls atomic.Int32
	a := &Authenticator{
		tokenDir:       dir,
		copilotBaseURL: "http://copilot.test",
		client: &http.Client{Transport: roundTripFunc(func(_ *http.Request) (*http.Response, error) {
			requests.Add(1)
			return copilotTokenResponseForTest(t, "fresh-noninteractive-token"), nil
		})},
		beforeDeviceCallFinalize: func() {
			if finalizeCalls.Add(1) == 1 {
				close(oldAtFinalize)
				<-releaseOldFinalize
			}
		},
	}

	oldDone := make(chan error, 1)
	go func() {
		_, err := a.getTokenWithDeviceFlow(context.Background())
		oldDone <- err
	}()
	<-oldAtFinalize

	if err := a.SignOut(); err != nil {
		t.Fatalf("SignOut() error = %v", err)
	}
	a.deviceCallMu.Lock()
	oldAttached := a.deviceCall != nil
	a.deviceCallMu.Unlock()
	if oldAttached {
		t.Fatal("old device call remained attached after SignOut returned")
	}

	if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("new-access-token"), 0o600); err != nil {
		t.Fatalf("write new access token: %v", err)
	}
	token, err := a.GetTokenNonInteractive(context.Background())
	if err != nil || token != "fresh-noninteractive-token" {
		t.Fatalf("fresh noninteractive result = (%q, %v), want fresh-noninteractive-token", token, err)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("upstream requests = %d, want 1 fresh request", got)
	}

	close(releaseOldFinalize)
	if err := <-oldDone; err == nil {
		t.Fatal("old device call returned success after SignOut")
	}
}
