package auth

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sync"
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
