package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type openAICodexRoundTripFunc func(*http.Request) (*http.Response, error)

func (f openAICodexRoundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func testOpenAICodexJWT(t testing.TB, claims map[string]interface{}) string {
	t.Helper()

	header, err := json.Marshal(map[string]string{"alg": "none", "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal jwt header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal jwt payload: %v", err)
	}

	return base64.RawURLEncoding.EncodeToString(header) + "." +
		base64.RawURLEncoding.EncodeToString(payload) + ".signature"
}

func testOpenAICodexTokens(t testing.TB, accessExp time.Time, accountID string, fedRAMP bool, refreshToken string) openAICodexTokenData {
	t.Helper()

	accessClaims := map[string]interface{}{"exp": accessExp.Unix()}
	idClaims := map[string]interface{}{
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id":         accountID,
			"chatgpt_account_is_fedramp": fedRAMP,
		},
	}

	return openAICodexTokenData{
		IDToken:      testOpenAICodexJWT(t, idClaims),
		AccessToken:  testOpenAICodexJWT(t, accessClaims),
		RefreshToken: refreshToken,
		AccountID:    accountID,
	}
}

func testOpenAICodexOpaqueTokens(t testing.TB, accessToken, accountID string, fedRAMP bool, refreshToken string) openAICodexTokenData {
	t.Helper()

	idClaims := map[string]interface{}{
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id":         accountID,
			"chatgpt_account_is_fedramp": fedRAMP,
		},
	}

	return openAICodexTokenData{
		IDToken:      testOpenAICodexJWT(t, idClaims),
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		AccountID:    accountID,
	}
}

func writeTestOpenAICodexAuth(t testing.TB, codexHome string, tokens openAICodexTokenData) string {
	t.Helper()
	return writeTestOpenAICodexAuthWithLastRefresh(t, codexHome, tokens, nil)
}

func writeTestOpenAICodexAuthWithLastRefresh(t testing.TB, codexHome string, tokens openAICodexTokenData, lastRefresh *time.Time) string {
	t.Helper()

	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}

	body := map[string]interface{}{
		"auth_mode": "chatgpt",
		"tokens":    tokens,
	}
	if lastRefresh != nil {
		body["last_refresh"] = lastRefresh.UTC().Format(time.RFC3339)
	}

	encoded, err := json.MarshalIndent(body, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent() error = %v", err)
	}

	authPath := filepath.Join(codexHome, "auth.json")
	if err := os.WriteFile(authPath, append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return authPath
}

func testOpenAICodexAuthInfo(t testing.TB, authPath string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(openAICodexAuthPathKey(authPath))
	if err != nil {
		t.Fatalf("stat auth.json: %v", err)
	}
	return info
}

func testOpenAICodexJournalPath(authPath string) string {
	return (&openAICodexAuth{path: authPath}).journalPath()
}

func readTestOpenAICodexJournal(t testing.TB, authPath string) openAICodexJournal {
	t.Helper()
	body, err := os.ReadFile(testOpenAICodexJournalPath(authPath))
	if err != nil {
		t.Fatalf("ReadFile() journal error = %v", err)
	}
	var journal openAICodexJournal
	if err := json.Unmarshal(body, &journal); err != nil {
		t.Fatalf("Unmarshal() journal error = %v", err)
	}
	return journal
}

func requireOpenAICodexRefresh(t testing.TB) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("Vekil-driven Codex refresh is disabled on Windows")
	}
}

func TestNewOpenAICodexAuthPreservesDefaultPathBehavior(t *testing.T) {
	t.Run("CODEX_HOME", func(t *testing.T) {
		codexHome := filepath.Join(t.TempDir(), "configured-codex-home")
		t.Setenv("CODEX_HOME", " \t"+codexHome+"\n")

		auth, err := newOpenAICodexAuth()
		if err != nil {
			t.Fatalf("newOpenAICodexAuth() error = %v", err)
		}
		if got, want := auth.path, filepath.Join(codexHome, "auth.json"); got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
	})

	t.Run("user home fallback", func(t *testing.T) {
		t.Setenv("CODEX_HOME", "")
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("os.UserHomeDir() error = %v", err)
		}

		auth, err := newOpenAICodexAuth()
		if err != nil {
			t.Fatalf("newOpenAICodexAuth() error = %v", err)
		}
		if got, want := auth.path, filepath.Join(home, ".codex", "auth.json"); got != want {
			t.Fatalf("path = %q, want %q", got, want)
		}
	})
}

func TestNewOpenAICodexAuthAtUsesExplicitPath(t *testing.T) {
	t.Setenv("CODEX_HOME", filepath.Join(t.TempDir(), "ignored-codex-home"))
	explicitPath := filepath.Join(t.TempDir(), "account", "auth.json")

	auth := newOpenAICodexAuthAt(explicitPath)
	if got := auth.path; got != explicitPath {
		t.Fatalf("path = %q, want explicit path %q", got, explicitPath)
	}
}

func TestNewOpenAICodexAuthAtKeysSharedStateByCanonicalPath(t *testing.T) {
	dir := t.TempDir()
	authPath := filepath.Join(dir, "auth.json")
	aliasPath := filepath.Join(dir, "unused") + string(os.PathSeparator) + ".." + string(os.PathSeparator) + "auth.json"
	otherPath := filepath.Join(dir, "other-auth.json")

	for _, path := range []string{authPath, aliasPath, otherPath} {
		openAICodexAuthSharedStates.Delete(openAICodexAuthPathKey(path))
	}
	t.Cleanup(func() {
		for _, path := range []string{authPath, aliasPath, otherPath} {
			openAICodexAuthSharedStates.Delete(openAICodexAuthPathKey(path))
		}
	})

	primary := newOpenAICodexAuthAt(authPath).sharedState()
	alias := newOpenAICodexAuthAt(aliasPath).sharedState()
	other := newOpenAICodexAuthAt(otherPath).sharedState()

	if primary != alias {
		t.Fatal("canonical aliases did not share OpenAI Codex auth state")
	}
	if primary == other {
		t.Fatal("distinct explicit auth paths unexpectedly shared OpenAI Codex auth state")
	}
}

func TestOpenAICodexAuthCredentialsUsesValidAuthJSON(t *testing.T) {
	t.Parallel()

	authPath := writeTestOpenAICodexAuth(
		t,
		t.TempDir(),
		testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-123", true, "refresh-token"),
	)

	credentials, err := (&openAICodexAuth{path: authPath}).credentials(context.Background(), nil)
	if err != nil {
		t.Fatalf("credentials() error = %v", err)
	}
	if credentials.accessToken == "" {
		t.Fatal("expected access token")
	}
	if credentials.accountID != "acct-123" {
		t.Fatalf("accountID = %q, want acct-123", credentials.accountID)
	}
	if !credentials.fedRAMP {
		t.Fatal("expected fedRAMP true")
	}
}

func TestOpenAICodexNeedsRefreshUsesJWTExpiryWhenAvailable(t *testing.T) {
	t.Parallel()

	now := time.Now()
	staleRefresh := now.Add(-(openAICodexRefreshInterval + time.Hour))
	recentRefresh := now.Add(-time.Hour)

	tests := []struct {
		name        string
		accessToken string
		lastRefresh *time.Time
		want        bool
	}{
		{
			name:        "fresh jwt ignores stale last_refresh",
			accessToken: testOpenAICodexJWT(t, map[string]interface{}{"exp": now.Add(time.Hour).Unix()}),
			lastRefresh: &staleRefresh,
			want:        false,
		},
		{
			name:        "expiring jwt refreshes even with recent last_refresh",
			accessToken: testOpenAICodexJWT(t, map[string]interface{}{"exp": now.Add(openAICodexRefreshSkew / 2).Unix()}),
			lastRefresh: &recentRefresh,
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := openAICodexNeedsRefresh(tt.accessToken, tt.lastRefresh, now); got != tt.want {
				t.Fatalf("openAICodexNeedsRefresh() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenAICodexNeedsRefreshFallsBackToLastRefreshForOpaqueTokens(t *testing.T) {
	t.Parallel()

	now := time.Now()
	recentRefresh := now.Add(-time.Hour)
	staleRefresh := now.Add(-(openAICodexRefreshInterval + time.Hour))

	tests := []struct {
		name        string
		lastRefresh *time.Time
		want        bool
	}{
		{
			name:        "missing last_refresh",
			lastRefresh: nil,
			want:        true,
		},
		{
			name:        "recent last_refresh",
			lastRefresh: &recentRefresh,
			want:        false,
		},
		{
			name:        "stale last_refresh",
			lastRefresh: &staleRefresh,
			want:        true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := openAICodexNeedsRefresh("opaque-access-token", tt.lastRefresh, now); got != tt.want {
				t.Fatalf("openAICodexNeedsRefresh() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOpenAICodexAuthCredentialsRefreshesExpiredToken(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	newAccessToken := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})
	newIDToken := testOpenAICodexJWT(t, map[string]interface{}{
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id":         "acct-456",
			"chatgpt_account_is_fedramp": true,
		},
	})

	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("refresh method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("Content-Type = %q, want application/json", got)
		}
		var req map[string]string
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode refresh request: %v", err)
		}
		if req["client_id"] != openAICodexClientID {
			t.Fatalf("client_id = %q, want %q", req["client_id"], openAICodexClientID)
		}
		if req["grant_type"] != "refresh_token" {
			t.Fatalf("grant_type = %q, want refresh_token", req["grant_type"])
		}
		if req["refresh_token"] != "old-refresh" {
			t.Fatalf("refresh_token = %q, want old-refresh", req["refresh_token"])
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  newAccessToken,
			"id_token":      newIDToken,
			"refresh_token": "new-refresh",
		})
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)
	if err := os.Chmod(authPath, 0o644); err != nil {
		t.Fatalf("chmod auth.json: %v", err)
	}
	before := time.Now().Add(-time.Second)
	credentials, err := (&openAICodexAuth{path: authPath}).credentials(context.Background(), refreshServer.Client())
	if err != nil {
		t.Fatalf("credentials() error = %v", err)
	}
	after := time.Now().Add(time.Second)
	if credentials.accessToken != newAccessToken {
		t.Fatalf("accessToken was not refreshed")
	}
	if credentials.accountID != "acct-123" {
		t.Fatalf("accountID = %q, want existing account_id acct-123", credentials.accountID)
	}
	if !credentials.fedRAMP {
		t.Fatal("expected refreshed id_token fedRAMP true")
	}

	state, err := (&openAICodexAuth{path: authPath}).read()
	if err != nil {
		t.Fatalf("read() error = %v", err)
	}
	if state.tokens.AccessToken != newAccessToken || state.tokens.IDToken != newIDToken || state.tokens.RefreshToken != "new-refresh" {
		t.Fatalf("Codex-owned auth.json was not updated: %+v", state.tokens)
	}
	if state.lastRefresh == nil || state.lastRefresh.Before(before) || state.lastRefresh.After(after) {
		t.Fatalf("last_refresh = %v, want between %v and %v", state.lastRefresh, before, after)
	}
	info, err := os.Stat(authPath)
	if err != nil {
		t.Fatalf("stat refreshed auth.json: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("refreshed auth.json mode = %o, want 600", got)
	}
	if _, err := os.Stat(testOpenAICodexJournalPath(authPath)); !os.IsNotExist(err) {
		t.Fatalf("journal remains after successful refresh: %v", err)
	}
}

func TestOpenAICodexAuthCredentialsSharesRefreshAcrossInstances(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	newAccessToken := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})
	newIDToken := testOpenAICodexJWT(t, map[string]interface{}{
		"https://api.openai.com/auth": map[string]interface{}{
			"chatgpt_account_id":         "acct-123",
			"chatgpt_account_is_fedramp": true,
		},
	})

	var refreshCalls atomic.Int32
	firstRequestSeen := make(chan struct{})
	allowFirstResponse := make(chan struct{}, 1)
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		call := refreshCalls.Add(1)
		if call == 1 {
			close(firstRequestSeen)
			<-allowFirstResponse
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  newAccessToken,
			"id_token":      newIDToken,
			"refresh_token": "new-refresh",
		})
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)
	authA := &openAICodexAuth{path: authPath}
	authB := &openAICodexAuth{path: authPath}

	type result struct {
		credentials openAICodexCredentials
		err         error
	}
	results := make(chan result, 2)

	go func() {
		credentials, err := authA.credentials(context.Background(), refreshServer.Client())
		results <- result{credentials: credentials, err: err}
	}()

	select {
	case <-firstRequestSeen:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for first refresh request")
	}

	go func() {
		credentials, err := authB.credentials(context.Background(), refreshServer.Client())
		results <- result{credentials: credentials, err: err}
	}()

	time.Sleep(150 * time.Millisecond)
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refreshCalls before releasing first response = %d, want 1", got)
	}
	allowFirstResponse <- struct{}{}

	for i := 0; i < 2; i++ {
		result := <-results
		if result.err != nil {
			t.Fatalf("credentials() error = %v", result.err)
		}
		if result.credentials.accessToken != newAccessToken {
			t.Fatalf("accessToken = %q, want refreshed token", result.credentials.accessToken)
		}
	}

	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refreshCalls = %d, want 1", got)
	}

	state, err := (&openAICodexAuth{path: authPath}).read()
	if err != nil {
		t.Fatalf("read() error = %v", err)
	}
	if state.tokens.AccessToken != newAccessToken || state.tokens.IDToken != newIDToken || state.tokens.RefreshToken != "new-refresh" {
		t.Fatalf("tokens were not persisted after shared refresh: %+v", state.tokens)
	}
	if _, err := os.Stat(testOpenAICodexJournalPath(authPath)); !os.IsNotExist(err) {
		t.Fatalf("journal remains after shared refresh: %v", err)
	}
}

func TestOpenAICodexAuthRecoversPreWriteCrashJournal(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	newAccessToken := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})

	var refreshCalls atomic.Int32
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refreshCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  newAccessToken,
			"refresh_token": "rotated-refresh",
		})
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)
	crashErr := errors.New("simulated crash before auth write")
	auth := &openAICodexAuth{
		path: authPath,
		beforePersistWrite: func() error {
			return crashErr
		},
	}
	firstCredentials, err := auth.credentials(context.Background(), refreshServer.Client())
	if err != nil || firstCredentials.accessToken != newAccessToken {
		t.Fatalf("credentials() = (%q, %v), want in-memory refreshed token despite persistence failure", firstCredentials.accessToken, err)
	}
	state, err := auth.readCurrentStateWithoutJournalRecovery()
	if err != nil {
		t.Fatalf("read auth.json before recovery: %v", err)
	}
	if state.tokens.AccessToken != oldTokens.AccessToken || state.tokens.RefreshToken != oldTokens.RefreshToken {
		t.Fatalf("auth.json changed before recovery: %+v", state.tokens)
	}
	journal := readTestOpenAICodexJournal(t, authPath)
	if journal.SourceDigest != openAICodexSourceDigest(journal.SourceAuthJSON) {
		t.Fatal("journal source digest does not match source auth bytes")
	}

	openAICodexAuthSharedStates.Delete(openAICodexAuthPathKey(authPath))
	credentials, err := (&openAICodexAuth{path: authPath}).credentials(context.Background(), refreshServer.Client())
	if err != nil || credentials.accessToken != newAccessToken {
		t.Fatalf("recovered credentials = (%q, %v), want refreshed token", credentials.accessToken, err)
	}
	recoveredState, err := (&openAICodexAuth{path: authPath}).read()
	if err != nil {
		t.Fatalf("read recovered auth.json: %v", err)
	}
	if recoveredState.tokens.AccessToken != newAccessToken || recoveredState.tokens.RefreshToken != "rotated-refresh" {
		t.Fatalf("recovered auth.json tokens = %+v", recoveredState.tokens)
	}
	if _, err := os.Stat(testOpenAICodexJournalPath(authPath)); !os.IsNotExist(err) {
		t.Fatalf("journal remains after recovery: %v", err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1 before journal recovery", got)
	}
}

func TestOpenAICodexAuthCredentialsRefreshesOpaqueTokenWhenLastRefreshMissing(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexOpaqueTokens(t, "opaque-access-token", "acct-123", false, "old-refresh")
	newAccessToken := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})

	refreshCalls := 0
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token": newAccessToken,
		})
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)
	credentials, err := (&openAICodexAuth{path: authPath}).credentials(context.Background(), refreshServer.Client())
	if err != nil {
		t.Fatalf("credentials() error = %v", err)
	}
	if refreshCalls != 1 {
		t.Fatalf("refreshCalls = %d, want 1", refreshCalls)
	}
	if credentials.accessToken != newAccessToken {
		t.Fatalf("accessToken = %q, want refreshed token", credentials.accessToken)
	}

	state, err := (&openAICodexAuth{path: authPath}).read()
	if err != nil {
		t.Fatalf("read() error = %v", err)
	}
	if state.lastRefresh == nil {
		t.Fatal("expected last_refresh in auth.json after opaque-token refresh")
	}
	if state.tokens.AccessToken != newAccessToken || state.tokens.RefreshToken != oldTokens.RefreshToken {
		t.Fatalf("auth.json tokens after opaque refresh = %+v", state.tokens)
	}
	if _, err := os.Stat(testOpenAICodexJournalPath(authPath)); !os.IsNotExist(err) {
		t.Fatalf("journal remains after opaque-token refresh: %v", err)
	}
}

func TestOpenAICodexAuthCredentialsUsesOpaqueTokenWhenLastRefreshRecent(t *testing.T) {
	recentRefresh := time.Now().Add(-time.Hour)
	tokens := testOpenAICodexOpaqueTokens(t, "opaque-access-token", "acct-123", true, "refresh-token")
	authPath := writeTestOpenAICodexAuthWithLastRefresh(t, t.TempDir(), tokens, &recentRefresh)

	refreshCalls := 0
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls++
		t.Fatalf("unexpected refresh request for recent opaque token")
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	credentials, err := (&openAICodexAuth{path: authPath}).credentials(context.Background(), refreshServer.Client())
	if err != nil {
		t.Fatalf("credentials() error = %v", err)
	}
	if refreshCalls != 0 {
		t.Fatalf("refreshCalls = %d, want 0", refreshCalls)
	}
	if credentials.accessToken != "opaque-access-token" {
		t.Fatalf("accessToken = %q, want original opaque-access-token", credentials.accessToken)
	}
	if credentials.accountID != "acct-123" {
		t.Fatalf("accountID = %q, want acct-123", credentials.accountID)
	}
	if !credentials.fedRAMP {
		t.Fatal("expected fedRAMP true")
	}
}

func TestOpenAICodexAuthCredentialsRejectsNonChatGPTAuth(t *testing.T) {
	t.Parallel()

	codexHome := t.TempDir()
	authPath := filepath.Join(codexHome, "auth.json")
	if err := os.WriteFile(authPath, []byte(`{"auth_mode":"apikey","tokens":null}`), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	_, err := (&openAICodexAuth{path: authPath}).credentials(context.Background(), nil)
	if err == nil {
		t.Fatal("credentials() error = nil, want auth_mode error")
	}
}

func waitForOpenAICodexRefreshWaiters(t *testing.T, auth *openAICodexAuth, want int) {
	t.Helper()
	shared := auth.sharedState()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		shared.mu.Lock()
		got := 0
		if shared.refresh != nil {
			got = shared.refresh.waiters
		}
		shared.mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d OpenAI Codex refresh waiters", want)
}

func TestOpenAICodexAuthRefreshWaiterHonorsContext(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	newAccessToken := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})

	var refreshCalls atomic.Int32
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refreshCalls.Add(1)
		close(refreshStarted)
		<-releaseRefresh
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  newAccessToken,
			"refresh_token": "new-refresh",
		})
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	auth := &openAICodexAuth{path: writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)}
	leaderDone := make(chan error, 1)
	go func() {
		_, err := auth.credentials(context.Background(), refreshServer.Client())
		leaderDone <- err
	}()
	<-refreshStarted

	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := auth.credentials(ctx, refreshServer.Client())
		waiterDone <- err
	}()
	waitForOpenAICodexRefreshWaiters(t, auth, 2)
	cancel()

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("OpenAI Codex refresh waiter did not honor its context")
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls while leader blocked = %d, want 1", got)
	}

	close(releaseRefresh)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader error = %v", err)
	}
}

func TestOpenAICodexAuthSharesFailedRefresh(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")

	var refreshCalls atomic.Int32
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refreshCalls.Add(1)
		close(refreshStarted)
		<-releaseRefresh
		http.Error(w, "temporary failure", http.StatusServiceUnavailable)
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	auth := &openAICodexAuth{path: writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)}
	const callers = 8
	results := make(chan error, callers)
	go func() {
		_, err := auth.credentials(context.Background(), refreshServer.Client())
		results <- err
	}()
	<-refreshStarted
	for range callers - 1 {
		go func() {
			_, err := auth.credentials(context.Background(), refreshServer.Client())
			results <- err
		}()
	}
	waitForOpenAICodexRefreshWaiters(t, auth, callers)
	close(releaseRefresh)

	for range callers {
		err := <-results
		if err == nil || !strings.Contains(err.Error(), "HTTP 503") {
			t.Fatalf("shared refresh error = %v, want HTTP 503", err)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1 shared failed refresh", got)
	}
}

func TestOpenAICodexAuthDeletionDiscardsPendingJournal(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	newAccessToken := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})

	var refreshCalls atomic.Int32
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refreshCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  newAccessToken,
			"refresh_token": "new-refresh",
		})
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)
	crashErr := errors.New("stop after journal")
	auth := &openAICodexAuth{path: authPath, beforePersistWrite: func() error { return crashErr }}
	firstCredentials, err := auth.credentials(context.Background(), refreshServer.Client())
	if err != nil || firstCredentials.accessToken != newAccessToken {
		t.Fatalf("initial credentials() = (%q, %v), want in-memory refreshed token", firstCredentials.accessToken, err)
	}
	if _, err := os.Stat(testOpenAICodexJournalPath(authPath)); err != nil {
		t.Fatalf("expected journal before deletion: %v", err)
	}
	if err := os.Remove(authPath); err != nil {
		t.Fatalf("remove auth.json: %v", err)
	}

	openAICodexAuthSharedStates.Delete(openAICodexAuthPathKey(authPath))
	_, err = (&openAICodexAuth{path: authPath}).credentials(context.Background(), refreshServer.Client())
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credentials() after deletion error = %v, want os.ErrNotExist", err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls after deletion = %d, want 1", got)
	}
	if _, err := os.Stat(testOpenAICodexJournalPath(authPath)); !os.IsNotExist(err) {
		t.Fatalf("journal exists after auth.json deletion: %v", err)
	}
}

func TestOpenAICodexAuthDoesNotOverwriteExternalLoginDuringRefresh(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-old", false, "old-refresh")
	staleRefreshedAccess := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})
	externalTokens := testOpenAICodexTokens(t, time.Now().Add(2*time.Hour), "acct-new", true, "external-refresh")

	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(refreshStarted)
		<-releaseRefresh
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  staleRefreshedAccess,
			"refresh_token": "stale-new-refresh",
		})
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	codexHome := t.TempDir()
	authPath := writeTestOpenAICodexAuth(t, codexHome, oldTokens)
	auth := &openAICodexAuth{path: authPath}
	type result struct {
		credentials openAICodexCredentials
		err         error
	}
	resultCh := make(chan result, 1)
	go func() {
		credentials, err := auth.credentials(context.Background(), refreshServer.Client())
		resultCh <- result{credentials: credentials, err: err}
	}()
	<-refreshStarted

	replacementPath := filepath.Join(codexHome, "auth.json.login")
	replacementBody, err := json.MarshalIndent(map[string]interface{}{
		"auth_mode": "chatgpt",
		"tokens":    externalTokens,
	}, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent() error = %v", err)
	}
	if err := os.WriteFile(replacementPath, append(replacementBody, '\n'), 0o600); err != nil {
		t.Fatalf("WriteFile() replacement error = %v", err)
	}
	if err := os.Rename(replacementPath, authPath); err != nil {
		t.Fatalf("Rename() replacement error = %v", err)
	}
	close(releaseRefresh)

	got := <-resultCh
	if got.err != nil {
		t.Fatalf("credentials() error = %v", got.err)
	}
	if got.credentials.accessToken != externalTokens.AccessToken || got.credentials.accountID != "acct-new" {
		t.Fatalf("credentials = %+v, want external login", got.credentials)
	}

	state, err := auth.read()
	if err != nil {
		t.Fatalf("read() error = %v", err)
	}
	if state.tokens.AccessToken != externalTokens.AccessToken || state.tokens.RefreshToken != "external-refresh" {
		t.Fatalf("external login was overwritten: %+v", state.tokens)
	}
}

func TestOpenAICodexAuthLeaderCancellationDoesNotCancelLiveWaiter(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	newAccessToken := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})

	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	refreshCanceled := make(chan struct{})
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(refreshStarted)
		select {
		case <-releaseRefresh:
			_ = json.NewEncoder(w).Encode(map[string]string{
				"access_token":  newAccessToken,
				"refresh_token": "new-refresh",
			})
		case <-r.Context().Done():
			close(refreshCanceled)
		}
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	auth := &openAICodexAuth{path: writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)}
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := auth.credentials(leaderCtx, refreshServer.Client())
		leaderDone <- err
	}()
	<-refreshStarted

	waiterDone := make(chan struct {
		credentials openAICodexCredentials
		err         error
	}, 1)
	go func() {
		credentials, err := auth.credentials(context.Background(), refreshServer.Client())
		waiterDone <- struct {
			credentials openAICodexCredentials
			err         error
		}{credentials: credentials, err: err}
	}()
	waitForOpenAICodexRefreshWaiters(t, auth, 2)
	cancelLeader()

	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %v, want context.Canceled", err)
	}
	shared := auth.sharedState()
	shared.mu.Lock()
	activeCall := shared.refresh
	shared.mu.Unlock()
	if activeCall == nil {
		t.Fatal("shared Codex refresh disappeared while a waiter remained")
	}
	select {
	case <-activeCall.ctx.Done():
		t.Fatal("shared Codex refresh context was canceled while a waiter remained")
	default:
	}
	select {
	case <-refreshCanceled:
		t.Fatal("shared Codex refresh was canceled while a waiter remained")
	default:
	}

	close(releaseRefresh)
	waiter := <-waiterDone
	if waiter.err != nil || waiter.credentials.accessToken != newAccessToken {
		t.Fatalf("waiter result = (%q, %v), want refreshed access token", waiter.credentials.accessToken, waiter.err)
	}
}

func TestOpenAICodexAuthContinuesRefreshAfterAllWaitersLeave(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	newAccessToken := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})

	refreshStarted := make(chan struct{})
	refreshCanceled := make(chan struct{})
	releaseRefresh := make(chan struct{})
	client := &http.Client{Transport: openAICodexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		close(refreshStarted)
		select {
		case <-releaseRefresh:
			body, _ := json.Marshal(map[string]string{
				"access_token":  newAccessToken,
				"refresh_token": "rotated-refresh",
			})
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader(body)),
				Request:    req,
			}, nil
		case <-req.Context().Done():
			close(refreshCanceled)
			return nil, req.Context().Err()
		}
	})}
	t.Setenv(openAICodexRefreshURLEnv, "http://codex-refresh.test/token")

	auth := &openAICodexAuth{path: writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)}
	ctx, cancel := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := auth.credentials(ctx, client)
		leaderDone <- err
	}()
	<-refreshStarted
	cancel()
	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %v, want context.Canceled", err)
	}
	select {
	case <-refreshCanceled:
		t.Fatal("Codex refresh was canceled after its last waiter left")
	case <-time.After(50 * time.Millisecond):
	}

	waiterDone := make(chan struct {
		credentials openAICodexCredentials
		err         error
	}, 1)
	go func() {
		credentials, err := auth.credentials(context.Background(), client)
		waiterDone <- struct {
			credentials openAICodexCredentials
			err         error
		}{credentials: credentials, err: err}
	}()
	close(releaseRefresh)
	select {
	case got := <-waiterDone:
		if got.err != nil || got.credentials.accessToken != newAccessToken {
			t.Fatalf("waiter result = (%q, %v), want completed rotated refresh", got.credentials.accessToken, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("waiter did not receive the in-flight Codex refresh")
	}
}

func TestOpenAICodexAuthExternalReplacementAfterComparisonWins(t *testing.T) {
	requireOpenAICodexRefresh(t)
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps refreshed Codex credentials in memory only")
	}
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-old", false, "old-refresh")
	refreshedAccess := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})
	externalTokens := testOpenAICodexTokens(t, time.Now().Add(2*time.Hour), "acct-external", true, "external-refresh")

	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  refreshedAccess,
			"refresh_token": "refreshed-refresh",
		})
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	codexHome := t.TempDir()
	authPath := writeTestOpenAICodexAuth(t, codexHome, oldTokens)
	replacementBody, err := json.MarshalIndent(map[string]interface{}{
		"auth_mode": "chatgpt",
		"tokens":    externalTokens,
	}, "", "  ")
	if err != nil {
		t.Fatalf("MarshalIndent() error = %v", err)
	}
	hookCalled := make(chan struct{})
	auth := &openAICodexAuth{
		path: authPath,
		beforePersistWrite: func() error {
			replacementPath := filepath.Join(codexHome, "auth.json.post-compare")
			if err := os.WriteFile(replacementPath, append(replacementBody, '\n'), 0o600); err != nil {
				return err
			}
			if err := os.Rename(replacementPath, authPath); err != nil {
				return err
			}
			close(hookCalled)
			return nil
		},
	}

	credentials, err := auth.credentials(context.Background(), refreshServer.Client())
	if err != nil {
		t.Fatalf("credentials() error = %v", err)
	}
	select {
	case <-hookCalled:
	default:
		t.Fatal("post-comparison persistence hook was not called")
	}
	if credentials.accessToken != externalTokens.AccessToken || credentials.accountID != "acct-external" {
		t.Fatalf("credentials = %+v, want external replacement", credentials)
	}

	state, err := auth.read()
	if err != nil {
		t.Fatalf("read() error = %v", err)
	}
	if state.tokens.AccessToken != externalTokens.AccessToken || state.tokens.RefreshToken != externalTokens.RefreshToken {
		t.Fatalf("post-comparison external replacement was overwritten: %+v", state.tokens)
	}
	if _, err := os.Stat(testOpenAICodexJournalPath(authPath)); !os.IsNotExist(err) {
		t.Fatalf("stale journal exists after post-comparison replacement: %v", err)
	}
}

func TestOpenAICodexAuthRefreshFailureUsesNewExternalLogin(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-old", false, "old-refresh")
	externalTokens := testOpenAICodexTokens(t, time.Now().Add(2*time.Hour), "acct-external", true, "external-refresh")

	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(refreshStarted)
		<-releaseRefresh
		w.WriteHeader(http.StatusBadGateway)
		_, _ = io.WriteString(w, `{"error":"old refresh failed"}`)
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	codexHome := t.TempDir()
	authPath := writeTestOpenAICodexAuth(t, codexHome, oldTokens)
	auth := &openAICodexAuth{path: authPath}
	type result struct {
		credentials openAICodexCredentials
		err         error
	}
	leaderDone := make(chan result, 1)
	go func() {
		credentials, err := auth.credentials(context.Background(), refreshServer.Client())
		leaderDone <- result{credentials: credentials, err: err}
	}()
	<-refreshStarted

	replacementPath := filepath.Join(codexHome, "auth.json.external")
	replacementBody, err := json.MarshalIndent(map[string]interface{}{
		"auth_mode":    "chatgpt",
		"tokens":       externalTokens,
		"last_refresh": time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal external auth: %v", err)
	}
	if err := os.WriteFile(replacementPath, append(replacementBody, '\n'), 0o600); err != nil {
		t.Fatalf("write external auth: %v", err)
	}
	if err := os.Rename(replacementPath, authPath); err != nil {
		t.Fatalf("replace auth.json: %v", err)
	}

	waiterDone := make(chan result, 1)
	go func() {
		credentials, err := auth.credentials(context.Background(), refreshServer.Client())
		waiterDone <- result{credentials: credentials, err: err}
	}()
	close(releaseRefresh)

	for name, done := range map[string]<-chan result{"leader": leaderDone, "waiter": waiterDone} {
		select {
		case got := <-done:
			if got.err != nil || got.credentials.accessToken != externalTokens.AccessToken || got.credentials.accountID != "acct-external" {
				t.Fatalf("%s result = (%q, %q, %v), want external login", name, got.credentials.accessToken, got.credentials.accountID, got.err)
			}
		case <-time.After(time.Second):
			t.Fatalf("timed out waiting for %s", name)
		}
	}
}

func TestOpenAICodexAuthJournalRecoversPartialAuthWrite(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	newTokens := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-123", true, "rotated-refresh")
	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)
	auth := &openAICodexAuth{path: authPath}

	sourceBody, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read source auth: %v", err)
	}
	sourceState, err := auth.parseState(sourceBody)
	if err != nil {
		t.Fatalf("parse source auth: %v", err)
	}
	targetState := openAICodexStateWithTokens(sourceState, newTokens, time.Now().UTC())
	targetBody, err := marshalOpenAICodexAuthState(targetState)
	if err != nil {
		t.Fatalf("marshal target auth: %v", err)
	}
	if err := auth.writeJournal(openAICodexJournal{
		Version:           openAICodexJournalVersion,
		SourceDigest:      openAICodexSourceDigest(sourceBody),
		SourceAuthJSON:    sourceBody,
		RefreshedAuthJSON: targetBody,
	}, testOpenAICodexAuthInfo(t, auth.path)); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	partial := targetBody[:len(targetBody)/2]
	if err := os.WriteFile(authPath, partial, 0o600); err != nil {
		t.Fatalf("write partial auth: %v", err)
	}

	openAICodexAuthSharedStates.Delete(openAICodexAuthPathKey(authPath))
	credentials, err := auth.credentials(context.Background(), nil)
	if err != nil || credentials.accessToken != newTokens.AccessToken {
		t.Fatalf("recovered credentials = (%q, %v), want target token", credentials.accessToken, err)
	}
	state, err := auth.read()
	if err != nil {
		t.Fatalf("read recovered auth: %v", err)
	}
	if state.tokens.RefreshToken != "rotated-refresh" || state.tokens.AccessToken != newTokens.AccessToken {
		t.Fatalf("recovered tokens = %+v", state.tokens)
	}
	if _, err := os.Stat(testOpenAICodexJournalPath(authPath)); !os.IsNotExist(err) {
		t.Fatalf("journal remains after partial-write recovery: %v", err)
	}
}

func TestOpenAICodexAuthJournalCleansCompletedTransaction(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	newTokens := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-123", false, "rotated-refresh")
	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)
	auth := &openAICodexAuth{path: authPath}
	sourceBody, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read source auth: %v", err)
	}
	sourceState, err := auth.parseState(sourceBody)
	if err != nil {
		t.Fatalf("parse source auth: %v", err)
	}
	targetState := openAICodexStateWithTokens(sourceState, newTokens, time.Now().UTC())
	targetBody, err := marshalOpenAICodexAuthState(targetState)
	if err != nil {
		t.Fatalf("marshal target auth: %v", err)
	}
	if err := auth.writeJournal(openAICodexJournal{
		Version:           openAICodexJournalVersion,
		SourceDigest:      openAICodexSourceDigest(sourceBody),
		SourceAuthJSON:    sourceBody,
		RefreshedAuthJSON: targetBody,
	}, testOpenAICodexAuthInfo(t, auth.path)); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	if err := os.WriteFile(authPath, targetBody, 0o600); err != nil {
		t.Fatalf("write completed target: %v", err)
	}

	credentials, err := auth.credentials(context.Background(), nil)
	if err != nil || credentials.accessToken != newTokens.AccessToken {
		t.Fatalf("credentials = (%q, %v), want completed target", credentials.accessToken, err)
	}
	if _, err := os.Stat(testOpenAICodexJournalPath(authPath)); !os.IsNotExist(err) {
		t.Fatalf("completed journal was not cleaned: %v", err)
	}
}

func TestOpenAICodexAuthJournalExternalStateWins(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-old", false, "old-refresh")
	targetTokens := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-target", false, "target-refresh")
	externalTokens := testOpenAICodexTokens(t, time.Now().Add(2*time.Hour), "acct-external", true, "external-refresh")
	codexHome := t.TempDir()
	authPath := writeTestOpenAICodexAuth(t, codexHome, oldTokens)
	auth := &openAICodexAuth{path: authPath}
	sourceBody, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read source auth: %v", err)
	}
	sourceState, err := auth.parseState(sourceBody)
	if err != nil {
		t.Fatalf("parse source auth: %v", err)
	}
	targetState := openAICodexStateWithTokens(sourceState, targetTokens, time.Now().UTC())
	targetBody, err := marshalOpenAICodexAuthState(targetState)
	if err != nil {
		t.Fatalf("marshal target auth: %v", err)
	}
	if err := auth.writeJournal(openAICodexJournal{
		Version:           openAICodexJournalVersion,
		SourceDigest:      openAICodexSourceDigest(sourceBody),
		SourceAuthJSON:    sourceBody,
		RefreshedAuthJSON: targetBody,
	}, testOpenAICodexAuthInfo(t, auth.path)); err != nil {
		t.Fatalf("write journal: %v", err)
	}

	replacementPath := filepath.Join(codexHome, "auth.json.external")
	replacementBody, err := json.MarshalIndent(map[string]interface{}{
		"auth_mode":    "chatgpt",
		"tokens":       externalTokens,
		"last_refresh": time.Now().UTC().Format(time.RFC3339Nano),
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal external auth: %v", err)
	}
	if err := os.WriteFile(replacementPath, append(replacementBody, '\n'), 0o600); err != nil {
		t.Fatalf("write external auth: %v", err)
	}
	if err := os.Rename(replacementPath, authPath); err != nil {
		t.Fatalf("replace auth: %v", err)
	}

	credentials, err := auth.credentials(context.Background(), nil)
	if err != nil || credentials.accessToken != externalTokens.AccessToken || credentials.accountID != "acct-external" {
		t.Fatalf("credentials = (%q, %q, %v), want external auth", credentials.accessToken, credentials.accountID, err)
	}
	if _, err := os.Stat(testOpenAICodexJournalPath(authPath)); !os.IsNotExist(err) {
		t.Fatalf("journal remains after external auth won: %v", err)
	}
}

func TestOpenAICodexAuthJournalModeIs0600(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	newAccessToken := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  newAccessToken,
			"refresh_token": "new-refresh",
		})
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)
	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)
	crashErr := errors.New("stop after journal")
	auth := &openAICodexAuth{path: authPath, beforePersistWrite: func() error { return crashErr }}
	credentials, err := auth.credentials(context.Background(), refreshServer.Client())
	if err != nil || credentials.accessToken != newAccessToken {
		t.Fatalf("credentials() = (%q, %v), want in-memory refreshed token", credentials.accessToken, err)
	}
	info, err := os.Stat(testOpenAICodexJournalPath(authPath))
	if err != nil {
		t.Fatalf("stat journal: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("journal mode = %o, want 600", got)
	}
}

func TestOpenAICodexAuthRestartUsesRotatedAuthJSON(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	newAccessToken := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})
	var refreshCalls atomic.Int32
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refreshCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  newAccessToken,
			"refresh_token": "rotated-refresh",
		})
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)
	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)
	if _, err := (&openAICodexAuth{path: authPath}).credentials(context.Background(), refreshServer.Client()); err != nil {
		t.Fatalf("initial credentials() error = %v", err)
	}

	openAICodexAuthSharedStates.Delete(openAICodexAuthPathKey(authPath))
	credentials, err := (&openAICodexAuth{path: authPath}).credentials(context.Background(), refreshServer.Client())
	if err != nil || credentials.accessToken != newAccessToken {
		t.Fatalf("restart credentials = (%q, %v), want rotated auth.json", credentials.accessToken, err)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	state, err := (&openAICodexAuth{path: authPath}).read()
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if state.tokens.RefreshToken != "rotated-refresh" {
		t.Fatalf("auth.json refresh token = %q, want rotated-refresh", state.tokens.RefreshToken)
	}
}

func TestOpenAICodexAuthCorruptJournalIsDiscarded(t *testing.T) {
	requireOpenAICodexRefresh(t)
	validTokens := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-authoritative", false, "auth-refresh")
	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), validTokens)
	if err := os.WriteFile(authPath+openAICodexJournalSuffix, []byte(`{"version":`), 0o600); err != nil {
		t.Fatalf("write corrupt journal: %v", err)
	}
	credentials, err := (&openAICodexAuth{path: authPath}).credentials(context.Background(), nil)
	if err != nil || credentials.accessToken != validTokens.AccessToken {
		t.Fatalf("credentials = (%q, %v), want authoritative token", credentials.accessToken, err)
	}
	if _, err := os.Stat(testOpenAICodexJournalPath(authPath)); !os.IsNotExist(err) {
		t.Fatalf("corrupt journal was not discarded: %v", err)
	}
}

func TestOpenAICodexAuthWindowsRefreshDisabled(t *testing.T) {
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)
	var requests atomic.Int32
	client := &http.Client{Transport: openAICodexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected request")
	})}
	auth := &openAICodexAuth{path: authPath, goos: "windows"}
	_, err := auth.credentials(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "disabled on Windows") || !strings.Contains(err.Error(), "codex login") {
		t.Fatalf("credentials() error = %v, want Windows codex login guidance", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("refresh requests = %d, want 0", got)
	}
	if _, err := os.Stat(testOpenAICodexJournalPath(authPath)); !os.IsNotExist(err) {
		t.Fatalf("Windows refresh created a journal: %v", err)
	}
}

func TestOpenAICodexAuthWindowsFreshCredentialsStillWork(t *testing.T) {
	tokens := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-123", true, "refresh-token")
	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), tokens)
	credentials, err := (&openAICodexAuth{path: authPath, goos: "windows"}).credentials(context.Background(), nil)
	if err != nil || credentials.accessToken != tokens.AccessToken || credentials.accountID != "acct-123" || !credentials.fedRAMP {
		t.Fatalf("credentials = (%q, %q, %v, %v), want fresh auth.json", credentials.accessToken, credentials.accountID, credentials.fedRAMP, err)
	}
}

func TestOpenAICodexAuthFreshReadOnlyAuthJSON(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX read-only mode test")
	}
	tokens := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-123", true, "refresh-token")
	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), tokens)
	if err := os.Chmod(authPath, 0o400); err != nil {
		t.Fatalf("chmod auth.json: %v", err)
	}

	credentials, err := (&openAICodexAuth{path: authPath}).credentials(context.Background(), nil)
	if err != nil || credentials.accessToken != tokens.AccessToken || credentials.accountID != "acct-123" || !credentials.fedRAMP {
		t.Fatalf("credentials = (%q, %q, %v, %v), want fresh read-only auth", credentials.accessToken, credentials.accountID, credentials.fedRAMP, err)
	}
}

func TestOpenAICodexAuthStaleReadOnlyAuthJSONDoesNotRefresh(t *testing.T) {
	requireOpenAICodexRefresh(t)
	if openAICodexRunningAsRoot() {
		t.Skip("root can open mode-0400 files read/write")
	}
	tokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "refresh-token")
	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), tokens)
	if err := os.Chmod(authPath, 0o400); err != nil {
		t.Fatalf("chmod auth.json: %v", err)
	}
	var requests atomic.Int32
	client := &http.Client{Transport: openAICodexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		requests.Add(1)
		return nil, errors.New("unexpected refresh")
	})}

	_, err := (&openAICodexAuth{path: authPath}).credentials(context.Background(), client)
	if err == nil || !strings.Contains(err.Error(), "open OpenAI Codex auth file") {
		t.Fatalf("credentials() error = %v, want writable-descriptor error", err)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("refresh requests = %d, want 0", got)
	}
}

func TestOpenAICodexAuthInterprocessLockSharesOneAuthorityCall(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	newAccessToken := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})
	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)

	var refreshCalls atomic.Int32
	refreshStarted := make(chan struct{})
	releaseRefresh := make(chan struct{})
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if refreshCalls.Add(1) == 1 {
			close(refreshStarted)
			<-releaseRefresh
		}
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  newAccessToken,
			"refresh_token": "rotated-refresh",
		})
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	authA := &openAICodexAuth{path: authPath, sharedStateKey: "lock-test-a"}
	authB := &openAICodexAuth{path: authPath, sharedStateKey: "lock-test-b"}
	type result struct {
		credentials openAICodexCredentials
		err         error
	}
	results := make(chan result, 2)
	go func() {
		credentials, err := authA.credentials(context.Background(), refreshServer.Client())
		results <- result{credentials: credentials, err: err}
	}()
	<-refreshStarted
	go func() {
		credentials, err := authB.credentials(context.Background(), refreshServer.Client())
		results <- result{credentials: credentials, err: err}
	}()

	time.Sleep(50 * time.Millisecond)
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls while first transaction held lock = %d, want 1", got)
	}
	close(releaseRefresh)
	for range 2 {
		got := <-results
		if got.err != nil || got.credentials.accessToken != newAccessToken {
			t.Fatalf("credentials = (%q, %v), want shared refreshed token", got.credentials.accessToken, got.err)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
	state, err := (&openAICodexAuth{path: authPath}).read()
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	if state.tokens.AccessToken != newAccessToken || state.tokens.RefreshToken != "rotated-refresh" {
		t.Fatalf("auth.json tokens = %+v", state.tokens)
	}
	if _, err := os.Stat(testOpenAICodexJournalPath(authPath)); !os.IsNotExist(err) {
		t.Fatalf("journal remains after serialized refresh: %v", err)
	}
}

func TestOpenAICodexAuthInterprocessLockPreventsInterleavedJournalWrites(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	newAccessToken := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})
	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)

	journalReady := make(chan struct{})
	releaseJournal := make(chan struct{})
	var hookCalls atomic.Int32
	var refreshCalls atomic.Int32
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		refreshCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  newAccessToken,
			"refresh_token": "rotated-refresh",
		})
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	authA := &openAICodexAuth{
		path:           authPath,
		sharedStateKey: "journal-lock-a",
		beforePersistWrite: func() error {
			if hookCalls.Add(1) == 1 {
				close(journalReady)
				<-releaseJournal
			}
			return nil
		},
	}
	authB := &openAICodexAuth{path: authPath, sharedStateKey: "journal-lock-b"}
	errs := make(chan error, 2)
	go func() {
		_, err := authA.credentials(context.Background(), refreshServer.Client())
		errs <- err
	}()
	<-journalReady
	go func() {
		_, err := authB.credentials(context.Background(), refreshServer.Client())
		errs <- err
	}()

	time.Sleep(50 * time.Millisecond)
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls during journal window = %d, want 1", got)
	}
	journal := readTestOpenAICodexJournal(t, authPath)
	if journal.SourceDigest != openAICodexSourceDigest(journal.SourceAuthJSON) {
		t.Fatal("journal was interleaved or corrupted")
	}
	close(releaseJournal)
	for range 2 {
		if err := <-errs; err != nil {
			t.Fatalf("credentials() error = %v", err)
		}
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refresh calls = %d, want 1", got)
	}
}

func writeTestOpenAICodexOrphanJournal(t testing.TB, dir string, journal openAICodexJournal, modTime time.Time) string {
	t.Helper()
	body, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		t.Fatalf("marshal orphan journal: %v", err)
	}
	file, err := os.CreateTemp(dir, openAICodexJournalTempPrefix+"*")
	if err != nil {
		t.Fatalf("create orphan journal: %v", err)
	}
	path := file.Name()
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		t.Fatalf("chmod orphan journal: %v", err)
	}
	if _, err := file.Write(append(body, '\n')); err != nil {
		_ = file.Close()
		t.Fatalf("write orphan journal: %v", err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		t.Fatalf("sync orphan journal: %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("close orphan journal: %v", err)
	}
	if err := os.Chtimes(path, modTime, modTime); err != nil {
		t.Fatalf("set orphan journal time: %v", err)
	}
	return path
}

func TestOpenAICodexAuthAdoptsNewestValidOrphanJournal(t *testing.T) {
	requireOpenAICodexRefresh(t)
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	olderTokens := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-older", false, "older-refresh")
	newerTokens := testOpenAICodexTokens(t, time.Now().Add(2*time.Hour), "acct-newer", true, "newer-refresh")
	dir := t.TempDir()
	authPath := writeTestOpenAICodexAuth(t, dir, oldTokens)
	auth := &openAICodexAuth{path: authPath}
	sourceBody, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read source auth: %v", err)
	}
	sourceState, err := auth.parseState(sourceBody)
	if err != nil {
		t.Fatalf("parse source auth: %v", err)
	}
	makeJournal := func(tokens openAICodexTokenData) openAICodexJournal {
		targetState := openAICodexStateWithTokens(sourceState, tokens, time.Now().UTC())
		targetBody, err := marshalOpenAICodexAuthState(targetState)
		if err != nil {
			t.Fatalf("marshal target auth: %v", err)
		}
		return openAICodexJournal{
			Version:           openAICodexJournalVersion,
			SourceDigest:      openAICodexSourceDigest(sourceBody),
			SourceAuthJSON:    sourceBody,
			RefreshedAuthJSON: targetBody,
		}
	}
	olderPath := writeTestOpenAICodexOrphanJournal(t, dir, makeJournal(olderTokens), time.Now().Add(-time.Minute))
	newerPath := writeTestOpenAICodexOrphanJournal(t, dir, makeJournal(newerTokens), time.Now())

	credentials, err := auth.credentials(context.Background(), nil)
	if err != nil || credentials.accessToken != newerTokens.AccessToken || credentials.accountID != "acct-newer" {
		t.Fatalf("credentials = (%q, %q, %v), want newest orphan", credentials.accessToken, credentials.accountID, err)
	}
	for _, path := range []string{olderPath, newerPath, testOpenAICodexJournalPath(authPath)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("journal artifact %q remains: %v", path, err)
		}
	}
	state, err := auth.read()
	if err != nil {
		t.Fatalf("read recovered auth: %v", err)
	}
	if state.tokens.RefreshToken != "newer-refresh" {
		t.Fatalf("recovered refresh token = %q, want newer-refresh", state.tokens.RefreshToken)
	}
}

func TestOpenAICodexAuthCleansInvalidAndStaleOrphanJournals(t *testing.T) {
	requireOpenAICodexRefresh(t)
	validTokens := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-current", false, "current-refresh")
	dir := t.TempDir()
	authPath := writeTestOpenAICodexAuth(t, dir, validTokens)

	invalid, err := os.CreateTemp(dir, openAICodexJournalTempPrefix+"*")
	if err != nil {
		t.Fatalf("create invalid orphan: %v", err)
	}
	invalidPath := invalid.Name()
	if err := invalid.Chmod(0o600); err != nil {
		t.Fatalf("chmod invalid orphan: %v", err)
	}
	_, _ = invalid.WriteString(`{"version":`)
	_ = invalid.Close()

	otherDir := t.TempDir()
	otherPath := writeTestOpenAICodexAuth(t, otherDir, testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-other", false, "other-refresh"))
	otherBody, err := os.ReadFile(otherPath)
	if err != nil {
		t.Fatalf("read stale source: %v", err)
	}
	otherAuth := &openAICodexAuth{path: otherPath}
	otherState, err := otherAuth.parseState(otherBody)
	if err != nil {
		t.Fatalf("parse stale source: %v", err)
	}
	staleTarget, err := marshalOpenAICodexAuthState(openAICodexStateWithTokens(otherState, validTokens, time.Now().UTC()))
	if err != nil {
		t.Fatalf("marshal stale target: %v", err)
	}
	stalePath := writeTestOpenAICodexOrphanJournal(t, dir, openAICodexJournal{
		Version:           openAICodexJournalVersion,
		SourceDigest:      openAICodexSourceDigest(otherBody),
		SourceAuthJSON:    otherBody,
		RefreshedAuthJSON: staleTarget,
	}, time.Now())

	credentials, err := (&openAICodexAuth{path: authPath}).credentials(context.Background(), nil)
	if err != nil || credentials.accessToken != validTokens.AccessToken {
		t.Fatalf("credentials = (%q, %v), want current auth", credentials.accessToken, err)
	}
	for _, path := range []string{invalidPath, stalePath} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("orphan %q was not removed: %v", path, err)
		}
	}
}

func TestOpenAICodexProcessLockHonorsContext(t *testing.T) {
	requireOpenAICodexRefresh(t)
	path := filepath.Join(t.TempDir(), "auth.json"+openAICodexLockSuffix)
	first, err := acquireOpenAICodexProcessLock(context.Background(), path)
	if err != nil {
		t.Fatalf("acquire first lock: %v", err)
	}
	defer func() { _ = first.release() }()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err = acquireOpenAICodexProcessLock(ctx, path)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("second lock error = %v, want context.DeadlineExceeded", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat lock file: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("lock mode = %o, want 600", got)
	}
}

func TestOpenAICodexAuthFreshAuthIgnoresOrphanCleanupFailure(t *testing.T) {
	requireOpenAICodexRefresh(t)
	tokens := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-123", false, "refresh-token")
	dir := t.TempDir()
	authPath := writeTestOpenAICodexAuth(t, dir, tokens)
	orphanDir := filepath.Join(dir, openAICodexJournalTempPrefix+"not-removable")
	if err := os.Mkdir(orphanDir, 0o700); err != nil {
		t.Fatalf("mkdir orphan artifact: %v", err)
	}
	if err := os.WriteFile(filepath.Join(orphanDir, "child"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write orphan child: %v", err)
	}

	credentials, err := (&openAICodexAuth{path: authPath}).credentials(context.Background(), nil)
	if err != nil || credentials.accessToken != tokens.AccessToken {
		t.Fatalf("credentials = (%q, %v), want fresh auth despite cleanup failure", credentials.accessToken, err)
	}
	if _, err := os.Stat(orphanDir); err != nil {
		t.Fatalf("orphan artifact unexpectedly removed: %v", err)
	}
}

func TestOpenAICodexAuthFreshCompletedJournalWorksReadOnly(t *testing.T) {
	if runtime.GOOS == "windows" || openAICodexRunningAsRoot() {
		t.Skip("POSIX non-root read-only journal test")
	}
	freshTokens := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-fresh", false, "fresh-refresh")
	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), freshTokens)
	targetBody, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read auth.json: %v", err)
	}
	sourceTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-fresh", false, "old-refresh")
	sourceState := openAICodexStateWithTokens(openAICodexAuthState{raw: map[string]json.RawMessage{"auth_mode": json.RawMessage(`"chatgpt"`)}}, sourceTokens, time.Now().Add(-time.Hour))
	sourceBody, err := marshalOpenAICodexAuthState(sourceState)
	if err != nil {
		t.Fatalf("marshal source auth: %v", err)
	}
	auth := &openAICodexAuth{path: authPath}
	if err := auth.writeJournal(openAICodexJournal{
		Version:           openAICodexJournalVersion,
		SourceDigest:      openAICodexSourceDigest(sourceBody),
		SourceAuthJSON:    sourceBody,
		RefreshedAuthJSON: targetBody,
	}, testOpenAICodexAuthInfo(t, auth.path)); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	if err := os.Chmod(authPath, 0o400); err != nil {
		t.Fatalf("chmod auth.json: %v", err)
	}
	if err := os.Chmod(auth.journalPath(), 0o400); err != nil {
		t.Fatalf("chmod journal: %v", err)
	}

	credentials, err := auth.credentials(context.Background(), nil)
	if err != nil || credentials.accessToken != freshTokens.AccessToken {
		t.Fatalf("credentials = (%q, %v), want fresh read-only auth", credentials.accessToken, err)
	}
}

func TestOpenAICodexAuthJournalPathUsesCanonicalAuthPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink behavior differs on Windows")
	}
	realHome := t.TempDir()
	authPath := writeTestOpenAICodexAuth(t, realHome, testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct", false, "refresh"))
	aliasDir := t.TempDir()
	aliasPath := filepath.Join(aliasDir, "auth.json")
	if err := os.Symlink(authPath, aliasPath); err != nil {
		t.Fatalf("symlink auth.json: %v", err)
	}
	if got, want := (&openAICodexAuth{path: aliasPath}).journalPath(), openAICodexAuthPathKey(authPath)+openAICodexJournalSuffix; got != want {
		t.Fatalf("journal path = %q, want canonical %q", got, want)
	}
}

func TestOpenAICodexProcessLockRejectsSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix no-follow lock test")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target")
	if err := os.WriteFile(target, []byte("do not chmod"), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	lockPath := filepath.Join(dir, "auth.json"+openAICodexLockSuffix)
	if err := os.Symlink(target, lockPath); err != nil {
		t.Fatalf("symlink lock: %v", err)
	}
	if lock, err := acquireOpenAICodexProcessLock(context.Background(), lockPath); err == nil {
		_ = lock.release()
		t.Fatal("symlinked lock unexpectedly opened")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("symlink target mode = %o, want unchanged 644", got)
	}
}

func TestOpenAICodexWritableAuthRejectsSymlinkReplacement(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Unix no-follow auth test")
	}
	dir := t.TempDir()
	target := filepath.Join(dir, "target-auth.json")
	if err := os.WriteFile(target, []byte(`{"auth_mode":"chatgpt"}`), 0o644); err != nil {
		t.Fatalf("write target: %v", err)
	}
	alias := filepath.Join(dir, "auth.json")
	if err := os.Symlink(target, alias); err != nil {
		t.Fatalf("symlink auth: %v", err)
	}
	if file, err := openOpenAICodexWritableFile(alias); err == nil {
		_ = file.Close()
		t.Fatal("symlinked auth path unexpectedly opened writable")
	}
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("stat target: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o644 {
		t.Fatalf("symlink target mode = %o, want unchanged 644", got)
	}
}

func TestOpenAICodexAuthFreshReadOnlyIgnoresStaleJournalArtifact(t *testing.T) {
	if runtime.GOOS == "windows" || openAICodexRunningAsRoot() {
		t.Skip("POSIX non-root read-only artifact test")
	}
	dir := t.TempDir()
	fresh := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-fresh", false, "refresh")
	authPath := writeTestOpenAICodexAuth(t, dir, fresh)
	auth := &openAICodexAuth{path: authPath}
	if err := os.WriteFile(auth.journalPath(), []byte(`{"version":1,"source_sha256":"stale"}`), 0o600); err != nil {
		t.Fatalf("write stale journal: %v", err)
	}
	if err := os.Chmod(authPath, 0o400); err != nil {
		t.Fatalf("chmod auth.json: %v", err)
	}
	if err := os.Chmod(auth.journalPath(), 0o400); err != nil {
		t.Fatalf("chmod journal: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod auth directory: %v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()

	credentials, err := auth.credentials(context.Background(), nil)
	if err != nil || credentials.accessToken != fresh.AccessToken {
		t.Fatalf("credentials = (%q, %v), want fresh authoritative auth", credentials.accessToken, err)
	}
}

func TestOpenAICodexAuthJournalPreflightFailsBeforeAuthorityRequest(t *testing.T) {
	if runtime.GOOS == "windows" || openAICodexRunningAsRoot() {
		t.Skip("POSIX non-root journal permission test")
	}
	dir := t.TempDir()
	authPath := writeTestOpenAICodexAuth(t, dir, testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct", false, "refresh"))
	auth := &openAICodexAuth{path: authPath}
	if err := os.WriteFile(auth.lockPath(), nil, 0o600); err != nil {
		t.Fatalf("write lock file: %v", err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod auth directory: %v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()

	var calls atomic.Int32
	client := &http.Client{Transport: openAICodexRoundTripFunc(func(*http.Request) (*http.Response, error) {
		calls.Add(1)
		return nil, errors.New("authority should not be called")
	})}
	_, err := auth.credentials(context.Background(), client)
	if err == nil {
		t.Fatal("credentials() error = nil, want journal preflight failure")
	}
	if got := calls.Load(); got != 0 {
		t.Fatalf("authority calls = %d, want 0 before journal preflight succeeds", got)
	}
}

func TestOpenAICodexAuthFreshExternalLoginBypassesOldRefresh(t *testing.T) {
	old := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-old", false, "old-refresh")
	fresh := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-fresh", true, "fresh-refresh")
	started := make(chan struct{})
	release := make(chan struct{})
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	dir := t.TempDir()
	authPath := writeTestOpenAICodexAuth(t, dir, old)
	auth := &openAICodexAuth{path: authPath}
	leaderDone := make(chan error, 1)
	go func() {
		_, err := auth.credentials(context.Background(), refreshServer.Client())
		leaderDone <- err
	}()
	<-started

	replacement := filepath.Join(dir, "auth.json.external")
	body, err := json.MarshalIndent(map[string]interface{}{
		"auth_mode":    "chatgpt",
		"tokens":       fresh,
		"last_refresh": time.Now().UTC().Format(time.RFC3339),
	}, "", "  ")
	if err != nil {
		t.Fatalf("marshal external auth: %v", err)
	}
	if err := os.WriteFile(replacement, append(body, '\n'), 0o600); err != nil {
		t.Fatalf("write external auth: %v", err)
	}
	if err := os.Rename(replacement, authPath); err != nil {
		t.Fatalf("replace auth.json: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	credentials, err := auth.credentials(ctx, refreshServer.Client())
	if err != nil || credentials.accessToken != fresh.AccessToken || credentials.accountID != "acct-fresh" {
		t.Fatalf("fresh external credentials = (%q, %q, %v)", credentials.accessToken, credentials.accountID, err)
	}
	close(release)
	<-leaderDone
}

func TestOpenAICodexAuthPartialPersistenceKeepsCachedRotatedCredentials(t *testing.T) {
	if runtime.GOOS == "windows" || openAICodexRunningAsRoot() {
		t.Skip("POSIX non-root persistence fault test")
	}
	dir := t.TempDir()
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct", false, "old-refresh")
	newTokens := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct", false, "rotated-refresh")
	authPath := writeTestOpenAICodexAuth(t, dir, oldTokens)
	auth := &openAICodexAuth{path: authPath}
	sourceBody, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read source auth: %v", err)
	}
	sourceState, err := auth.parseState(sourceBody)
	if err != nil {
		t.Fatalf("parse source auth: %v", err)
	}
	targetState := openAICodexStateWithTokens(sourceState, newTokens, time.Now().UTC())
	targetBody, err := marshalOpenAICodexAuthState(targetState)
	if err != nil {
		t.Fatalf("marshal target auth: %v", err)
	}
	if err := auth.writeJournal(openAICodexJournal{
		Version:           openAICodexJournalVersion,
		SourceDigest:      openAICodexSourceDigest(sourceBody),
		SourceAuthJSON:    sourceBody,
		RefreshedAuthJSON: targetBody,
	}, testOpenAICodexAuthInfo(t, authPath)); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	if err := os.WriteFile(authPath, targetBody[:len(targetBody)/2], 0o600); err != nil {
		t.Fatalf("write partial auth: %v", err)
	}
	targetState.sourceDigest = openAICodexSourceDigest(sourceBody)
	shared := auth.sharedState()
	shared.mu.Lock()
	shared.state = openAICodexCloneStatePtr(&targetState)
	shared.mu.Unlock()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod auth directory: %v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()

	credentials, err := auth.credentials(context.Background(), nil)
	if err != nil || credentials.accessToken != newTokens.AccessToken {
		t.Fatalf("credentials = (%q, %v), want cached rotated token", credentials.accessToken, err)
	}
}

func TestOpenAICodexAuthPendingJournalServesCachedRotatedCredentials(t *testing.T) {
	if runtime.GOOS == "windows" || openAICodexRunningAsRoot() {
		t.Skip("POSIX non-root pending journal test")
	}
	dir := t.TempDir()
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct", false, "old-refresh")
	newTokens := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct", false, "rotated-refresh")
	authPath := writeTestOpenAICodexAuth(t, dir, oldTokens)
	auth := &openAICodexAuth{path: authPath}
	sourceBody, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatalf("read source auth: %v", err)
	}
	sourceState, err := auth.parseState(sourceBody)
	if err != nil {
		t.Fatalf("parse source auth: %v", err)
	}
	targetState := openAICodexStateWithTokens(sourceState, newTokens, time.Now().UTC())
	targetBody, err := marshalOpenAICodexAuthState(targetState)
	if err != nil {
		t.Fatalf("marshal target auth: %v", err)
	}
	if err := auth.writeJournal(openAICodexJournal{
		Version:           openAICodexJournalVersion,
		SourceDigest:      openAICodexSourceDigest(sourceBody),
		SourceAuthJSON:    sourceBody,
		RefreshedAuthJSON: targetBody,
	}, testOpenAICodexAuthInfo(t, authPath)); err != nil {
		t.Fatalf("write journal: %v", err)
	}
	targetState.sourceDigest = openAICodexSourceDigest(sourceBody)
	shared := auth.sharedState()
	shared.mu.Lock()
	shared.state = openAICodexCloneStatePtr(&targetState)
	shared.mu.Unlock()
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatalf("chmod auth directory: %v", err)
	}
	defer func() { _ = os.Chmod(dir, 0o700) }()

	credentials, err := auth.credentials(context.Background(), nil)
	if err != nil || credentials.accessToken != newTokens.AccessToken {
		t.Fatalf("credentials = (%q, %v), want cached rotated token", credentials.accessToken, err)
	}
}

func TestOpenAICodexAuthRetriesWithCachedRotatedTokenAfterJournalFailure(t *testing.T) {
	requireOpenAICodexRefresh(t)
	old := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct", false, "old-refresh")
	shortAccess := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(150 * time.Millisecond).Unix()})
	freshAccess := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})
	var mu sync.Mutex
	var seen []string
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]string
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode refresh request: %v", err)
		}
		mu.Lock()
		seen = append(seen, request["refresh_token"])
		call := len(seen)
		mu.Unlock()
		if call == 1 {
			_ = json.NewEncoder(w).Encode(map[string]string{"access_token": shortAccess, "refresh_token": "rotated-refresh"})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"access_token": freshAccess, "refresh_token": "final-refresh"})
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), old)
	failJournal := true
	auth := &openAICodexAuth{
		path: authPath,
		beforeJournalWrite: func() error {
			if failJournal {
				return errors.New("simulated journal creation failure")
			}
			return nil
		},
	}
	first, err := auth.credentials(context.Background(), refreshServer.Client())
	if err != nil || first.accessToken != shortAccess {
		t.Fatalf("first credentials = (%q, %v), want in-memory rotated token", first.accessToken, err)
	}
	failJournal = false
	time.Sleep(1100 * time.Millisecond)
	second, err := auth.credentials(context.Background(), refreshServer.Client())
	if err != nil || second.accessToken != freshAccess {
		t.Fatalf("second credentials = (%q, %v), want refresh via rotated token", second.accessToken, err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(seen) != 2 || seen[0] != "old-refresh" || seen[1] != "rotated-refresh" {
		t.Fatalf("refresh tokens = %v, want old then rotated", seen)
	}
}

func TestOpenAICodexAuthExternalLoginWinsBeforeRefreshPublication(t *testing.T) {
	requireOpenAICodexRefresh(t)
	old := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-old", false, "old-refresh")
	refreshed := testOpenAICodexTokens(t, time.Now().Add(time.Hour), "acct-old", false, "rotated-refresh")
	external := testOpenAICodexTokens(t, time.Now().Add(2*time.Hour), "acct-external", true, "external-refresh")
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  refreshed.AccessToken,
			"id_token":      refreshed.IDToken,
			"refresh_token": refreshed.RefreshToken,
		})
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	dir := t.TempDir()
	authPath := writeTestOpenAICodexAuth(t, dir, old)
	auth := &openAICodexAuth{path: authPath}
	auth.beforeRefreshPublish = func() {
		replacement := filepath.Join(dir, "auth.json.external")
		body, err := json.MarshalIndent(map[string]interface{}{
			"auth_mode":    "chatgpt",
			"tokens":       external,
			"last_refresh": time.Now().UTC().Format(time.RFC3339),
		}, "", "  ")
		if err != nil {
			t.Fatalf("marshal external auth: %v", err)
		}
		if err := os.WriteFile(replacement, append(body, '\n'), 0o600); err != nil {
			t.Fatalf("write external auth: %v", err)
		}
		if err := os.Rename(replacement, authPath); err != nil {
			t.Fatalf("replace auth.json: %v", err)
		}
	}

	credentials, err := auth.credentials(context.Background(), refreshServer.Client())
	if err != nil || credentials.accessToken != external.AccessToken || credentials.accountID != "acct-external" {
		t.Fatalf("credentials = (%q, %q, %v), want external login", credentials.accessToken, credentials.accountID, err)
	}
}
