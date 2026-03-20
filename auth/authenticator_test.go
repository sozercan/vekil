package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
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
	a := NewAuthenticator("")
	if a.tokenDir == "" {
		t.Fatal("expected tokenDir to be set")
	}
}

func TestNewAuthenticator_CustomDir(t *testing.T) {
	dir := t.TempDir()
	a := NewAuthenticator(dir)
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
}

func TestSignOut_Idempotent(t *testing.T) {
	a := &Authenticator{tokenDir: t.TempDir()}
	// Calling SignOut when no files exist should not error
	if err := a.SignOut(); err != nil {
		t.Fatalf("SignOut on empty dir should not error: %v", err)
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

// roundTripFunc adapts a function to http.RoundTripper.
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func TestPollForAuthorization_Success(t *testing.T) {
	call := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/login/oauth/access_token":
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
		case r.URL.Path == "/copilot_internal/v2/token":
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
		DisableAutoDeviceFlow: true,
	}

	err := a.refreshToken(context.Background(), false)
	if err == nil {
		t.Fatal("expected error when DisableAutoDeviceFlow is true")
	}
	if err.Error() != "not authenticated" {
		t.Errorf("expected 'not authenticated', got %q", err.Error())
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
