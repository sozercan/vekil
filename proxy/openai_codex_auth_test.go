package proxy

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

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
		t.Fatalf("tokens were not persisted after refresh: %+v", state.tokens)
	}
	if state.lastRefresh == nil {
		t.Fatal("expected last_refresh to be persisted after refresh")
	}
	if state.lastRefresh.Before(before) || state.lastRefresh.After(after) {
		t.Fatalf("last_refresh = %v, want between %v and %v", state.lastRefresh, before, after)
	}
}

func TestOpenAICodexAuthCredentialsSharesRefreshAcrossInstances(t *testing.T) {
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
}

func TestOpenAICodexAuthCredentialsUsesCachedRefreshWhenPersistFails(t *testing.T) {
	oldTokens := testOpenAICodexTokens(t, time.Now().Add(-time.Hour), "acct-123", false, "old-refresh")
	newAccessToken := testOpenAICodexJWT(t, map[string]interface{}{"exp": time.Now().Add(time.Hour).Unix()})

	var refreshCalls atomic.Int32
	refreshServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		refreshCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"access_token":  newAccessToken,
			"refresh_token": "new-refresh",
		})
	}))
	defer refreshServer.Close()
	t.Setenv(openAICodexRefreshURLEnv, refreshServer.URL)

	authPath := writeTestOpenAICodexAuth(t, t.TempDir(), oldTokens)
	if err := os.Mkdir(authPath+".tmp", 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}

	authA := &openAICodexAuth{path: authPath}
	credentialsA, err := authA.credentials(context.Background(), refreshServer.Client())
	if err != nil {
		t.Fatalf("credentials() error = %v", err)
	}
	if credentialsA.accessToken != newAccessToken {
		t.Fatalf("accessToken = %q, want refreshed token", credentialsA.accessToken)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refreshCalls after first refresh = %d, want 1", got)
	}

	state, err := (&openAICodexAuth{path: authPath}).read()
	if err != nil {
		t.Fatalf("read() error = %v", err)
	}
	if state.tokens.AccessToken != oldTokens.AccessToken || state.tokens.RefreshToken != oldTokens.RefreshToken {
		t.Fatalf("expected auth.json to remain stale after persistence failure: %+v", state.tokens)
	}
	if state.lastRefresh != nil {
		t.Fatalf("lastRefresh = %v, want nil after persistence failure", state.lastRefresh)
	}

	authB := &openAICodexAuth{path: authPath}
	credentialsB, err := authB.credentials(context.Background(), refreshServer.Client())
	if err != nil {
		t.Fatalf("second credentials() error = %v", err)
	}
	if credentialsB.accessToken != newAccessToken {
		t.Fatalf("second accessToken = %q, want refreshed token", credentialsB.accessToken)
	}
	if got := refreshCalls.Load(); got != 1 {
		t.Fatalf("refreshCalls after cached lookup = %d, want 1", got)
	}
}

func TestOpenAICodexAuthCredentialsRefreshesOpaqueTokenWhenLastRefreshMissing(t *testing.T) {
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
		t.Fatal("expected last_refresh to be written after opaque-token refresh")
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
