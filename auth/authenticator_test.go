package auth

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
