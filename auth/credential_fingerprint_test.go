package auth

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestCredentialFingerprintPreservesRequestIdentity(t *testing.T) {
	want := sha256.Sum256([]byte("source-one"))
	a := &Authenticator{accessToken: "source-one", copilotToken: "chat-token", copilotSourceFingerprint: want, responsesSourceToken: "source-one", responsesToken: "responses-token"}
	for _, bearer := range []string{"source-one", "chat-token", "responses-token"} {
		if got := a.CredentialFingerprint(bearer); got != want {
			t.Fatal("bearers for one credential have different fingerprints")
		}
	}
	a.responsesToken = "refreshed-token"
	if a.CredentialFingerprint("refreshed-token") != want {
		t.Fatal("service token refresh changed the source fingerprint")
	}
	a.accessToken, a.copilotToken = "source-two", "chat-two"
	a.copilotSourceFingerprint = sha256.Sum256([]byte("source-two"))
	a.responsesSourceToken, a.responsesToken = "source-two", "responses-two"
	if a.CredentialFingerprint("chat-token") == a.CredentialFingerprint("chat-two") {
		t.Fatal("old in-flight bearer inherited another login")
	}
	if a.CredentialFingerprint("") != ([32]byte{}) {
		t.Fatal("empty bearer has a fingerprint")
	}
}

func TestCredentialFingerprintCachedTokenLifecycle(t *testing.T) {
	const originalSource = "legacy-source-one"
	for _, tc := range []struct {
		name, nextSource string
		failRefresh      bool
	}{
		{name: "same credential", nextSource: originalSource},
		{name: "replacement credential", nextSource: "legacy-source-two"},
		{name: "failed replacement", nextSource: "legacy-source-two", failRefresh: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("COPILOT_GITHUB_TOKEN", "")
			var calls atomic.Int32
			var reject atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				wantSource := originalSource
				if call > 1 {
					wantSource = tc.nextSource
				}
				if r.URL.Path != "/copilot_internal/v2/token" || r.Header.Get("Authorization") != "token "+wantSource {
					t.Errorf("unexpected credential exchange: %s", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				if reject.Load() {
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"error_details":"invalid access token"}`))
					return
				}
				_ = json.NewEncoder(w).Encode(CopilotTokenResponse{Token: fmt.Sprintf("service-token-%d", call), ExpiresAt: time.Now().Add(time.Hour).Unix()})
			}))
			defer server.Close()
			dir := t.TempDir()
			writeSource := func(source string) {
				t.Helper()
				if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte(source), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			newAuth := func() *Authenticator {
				t.Helper()
				a, err := NewAuthenticator(dir)
				if err != nil {
					t.Fatal(err)
				}
				a.client, a.directClient, a.copilotBaseURL = server.Client(), server.Client(), server.URL
				return a
			}
			writeSource(originalSource)
			original := newAuth()
			first, err := original.GetTokenNonInteractive(context.Background())
			if err != nil || calls.Load() != 1 {
				t.Fatalf("initial authentication: error=%v exchanges=%d", err, calls.Load())
			}
			want := sha256.Sum256([]byte(originalSource))
			if original.CredentialFingerprint(first) != want {
				t.Fatal("initial bearer lost its issuing credential")
			}
			cache, err := os.ReadFile(filepath.Join(dir, "api-key.json"))
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(cache), originalSource) {
				t.Fatal("bearer cache retained another copy of the source credential")
			}
			writeSource(tc.nextSource)
			restarted := newAuth()
			cached, err := restarted.GetResponsesToken(context.Background())
			if err != nil || cached != first || calls.Load() != 1 {
				t.Fatalf("cached startup repeated authentication: error=%v exchanges=%d", err, calls.Load())
			}
			if restarted.CredentialFingerprint(cached) != want {
				t.Error("cached bearer lost its issuing credential after restart")
			}
			reject.Store(tc.failRefresh)
			refreshed, err := restarted.RefreshTokenNonInteractive(context.Background())
			if tc.failRefresh {
				if err == nil || calls.Load() != 2 {
					t.Fatalf("replacement refresh: error=%v exchanges=%d", err, calls.Load())
				}
				if restarted.CredentialFingerprint(cached) != want {
					t.Error("failed replacement assigned its credential to the cached bearer")
				}
				return
			}
			if err != nil || refreshed == cached || calls.Load() != 2 {
				t.Fatalf("service-token refresh: error=%v exchanges=%d", err, calls.Load())
			}
			nextWant := sha256.Sum256([]byte(tc.nextSource))
			if restarted.CredentialFingerprint(refreshed) != nextWant {
				t.Fatal("refreshed bearer has the wrong issuing credential")
			}
			if tc.nextSource == originalSource && restarted.CredentialFingerprint(refreshed) != original.CredentialFingerprint(first) {
				t.Fatal("same-credential refresh changed session identity")
			}
			if tc.nextSource != originalSource && restarted.CredentialFingerprint(refreshed) == original.CredentialFingerprint(first) {
				t.Fatal("replacement credential inherited the previous session identity")
			}
			loadedAgain := newAuth()
			last, err := loadedAgain.GetTokenNonInteractive(context.Background())
			if err != nil || last != refreshed || calls.Load() != 2 || loadedAgain.CredentialFingerprint(last) != nextWant {
				t.Fatalf("refreshed cache lost its identity: error=%v exchanges=%d", err, calls.Load())
			}
		})
	}
}

func TestCredentialFingerprintLegacyCacheDoesNotAssumeCurrentSource(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	dir := t.TempDir()
	data, err := json.Marshal(CopilotTokenResponse{Token: "legacy-cached-bearer", ExpiresAt: time.Now().Add(time.Hour).Unix()})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "api-key.json"), data, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte("different-disk-source"), 0o600); err != nil {
		t.Fatal(err)
	}
	a := &Authenticator{tokenDir: dir, accessToken: "different-memory-source"}
	token, err := a.GetTokenNonInteractive(context.Background())
	if err != nil || token != "legacy-cached-bearer" {
		t.Fatalf("legacy cache is no longer usable: %v", err)
	}
	if a.CredentialFingerprint(token) != sha256.Sum256([]byte(token)) {
		t.Fatal("legacy cache assumed an unverified source credential")
	}
}

func TestCredentialFingerprintGitHubCLIRefreshWithoutPersistence(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake gh shell script test is Unix-only")
	}
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	const source = "legacy-gh-cli-source"
	ghPath := filepath.Join(t.TempDir(), "gh")
	if err := os.WriteFile(ghPath, []byte("#!/bin/sh\nprintf '"+source+"\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/copilot_internal/v2/token" || r.Header.Get("Authorization") != "token "+source {
			t.Errorf("unexpected credential exchange: %s", r.URL.Path)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_ = json.NewEncoder(w).Encode(CopilotTokenResponse{
			Token: fmt.Sprintf("cli-service-token-%d", calls.Add(1)), ExpiresAt: time.Now().Add(time.Hour).Unix(),
		})
	}))
	defer server.Close()
	dir := t.TempDir()
	a, err := NewAuthenticator(dir)
	if err != nil {
		t.Fatal(err)
	}
	a.githubCLIPath, a.client, a.directClient, a.copilotBaseURL = ghPath, server.Client(), server.Client(), server.URL
	if err := a.SignInWithGitHubCLI(context.Background()); err != nil {
		t.Fatal(err)
	}
	first, err := a.GetResponsesToken(context.Background())
	if err != nil || calls.Load() != 1 {
		t.Fatalf("CLI authentication: error=%v exchanges=%d", err, calls.Load())
	}
	want := sha256.Sum256([]byte(source))
	if a.CredentialFingerprint(first) != want {
		t.Fatal("CLI bearer lost its issuing credential")
	}
	refreshed, err := a.RefreshTokenNonInteractive(context.Background())
	if err != nil || refreshed == first || calls.Load() != 2 {
		t.Fatalf("CLI refresh: error=%v exchanges=%d", err, calls.Load())
	}
	if a.CredentialFingerprint(refreshed) != want {
		t.Fatal("CLI refresh changed session identity")
	}
	for _, filename := range []string{"access-token", "api-key.json"} {
		if _, err := os.Stat(filepath.Join(dir, filename)); !os.IsNotExist(err) {
			t.Fatalf("CLI authentication persisted %s: %v", filename, err)
		}
	}
}

func TestCredentialFingerprintMalformedCacheRefreshes(t *testing.T) {
	for _, malformed := range []string{"not-hex", strings.Repeat("01", 31), strings.Repeat("01", 33), strings.Repeat("00", 32)} {
		t.Run(malformed, func(t *testing.T) {
			t.Setenv("COPILOT_GITHUB_TOKEN", "")
			const source = "legacy-cache-recovery-source"
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				if r.URL.Path != "/copilot_internal/v2/token" || r.Header.Get("Authorization") != "token "+source {
					t.Errorf("unexpected credential exchange: %s", r.URL.Path)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				_ = json.NewEncoder(w).Encode(CopilotTokenResponse{Token: "recovered-service-token", ExpiresAt: time.Now().Add(time.Hour).Unix()})
			}))
			defer server.Close()
			dir := t.TempDir()
			cache, err := json.Marshal(map[string]any{
				"token": "unverified-cached-token", "expires_at": time.Now().Add(time.Hour).Unix(), "source_fingerprint": malformed,
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "api-key.json"), cache, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "access-token"), []byte(source), 0o600); err != nil {
				t.Fatal(err)
			}
			a, err := NewAuthenticator(dir)
			if err != nil {
				t.Fatal(err)
			}
			a.client, a.directClient, a.copilotBaseURL = server.Client(), server.Client(), server.URL
			token, err := a.GetTokenNonInteractive(context.Background())
			if err != nil || token != "recovered-service-token" || calls.Load() != 1 {
				t.Fatalf("malformed cache did not reauthenticate: error=%v exchanges=%d", err, calls.Load())
			}
			if a.CredentialFingerprint(token) != sha256.Sum256([]byte(source)) {
				t.Fatal("recovered bearer lost its issuing credential")
			}
		})
	}
}
