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
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	want := sha256.Sum256([]byte("ghu_source-one"))
	a := NewTestAuthenticatorWithResponsesToken("ghu_source-one", "responses-token")
	for _, get := range []func(context.Context) (Credential, error){a.GetCredential, a.GetResponsesCredential} {
		credential, err := get(context.Background())
		if err != nil || credential.SourceFingerprint != want {
			t.Fatal("bearers for one credential have different fingerprints")
		}
	}
	first, err := a.GetResponsesCredential(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	a.responsesMu.Lock()
	a.responsesToken = "refreshed-token"
	a.responsesMu.Unlock()
	refreshed, err := a.GetResponsesCredential(context.Background())
	if err != nil || refreshed.Token != "refreshed-token" || refreshed.SourceFingerprint != first.SourceFingerprint {
		t.Fatal("service token refresh changed the source fingerprint")
	}
	a.mu.Lock()
	a.copilotToken = "ghu_source-two"
	a.copilotSourceFingerprint = sha256.Sum256([]byte("ghu_source-two"))
	a.mu.Unlock()
	current, err := a.GetCredential(context.Background())
	if err != nil || current.SourceFingerprint == first.SourceFingerprint || first.SourceFingerprint != want {
		t.Fatal("old in-flight bearer inherited another login")
	}
}

func TestCredentialCapturesSourceBeforeSharedResultIsPublished(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	for _, device := range []bool{false, true} {
		for _, nextSource := range []string{"source-one", "source-two"} {
			t.Run(fmt.Sprintf("device=%t/source=%s", device, nextSource), func(t *testing.T) {
				want := sha256.Sum256([]byte("source-one"))
				a := NewTestAuthenticator("issued-bearer")
				a.copilotSourceFingerprint = want
				rotate := func() {
					a.mu.Lock()
					a.copilotToken = "refreshed-bearer"
					a.copilotSourceFingerprint = sha256.Sum256([]byte(nextSource))
					a.mu.Unlock()
				}
				get := a.GetCredential
				if device {
					a.beforeDeviceCallFinalize = rotate
					get = a.getTokenWithDeviceFlow
				} else {
					a.beforeRefreshCallFinalize = rotate
				}
				credential, err := get(context.Background())
				if err != nil || credential.Token != "issued-bearer" || credential.SourceFingerprint != want {
					t.Fatalf("issued bearer lost its source during refresh: %v", err)
				}
				current, err := a.GetCredential(context.Background())
				if err != nil || current.Token != "refreshed-bearer" || current.SourceFingerprint != sha256.Sum256([]byte(nextSource)) {
					t.Fatalf("refreshed bearer has the wrong source: %v", err)
				}
			})
		}
	}
}

func TestResponsesCredentialKeepsSourceDuringExchange(t *testing.T) {
	t.Setenv("COPILOT_GITHUB_TOKEN", "")
	const source = "ghu_original-source"
	for _, nextSource := range []string{source, "ghu_replacement-source"} {
		t.Run(nextSource, func(t *testing.T) {
			a := NewTestAuthenticator(source)
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				call := calls.Add(1)
				wantSource := source
				if call > 1 {
					wantSource = nextSource
				}
				if r.Header.Get("Authorization") != "token "+wantSource {
					t.Error("Responses exchange used a different source credential")
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				if call == 1 {
					a.mu.Lock()
					a.copilotToken = nextSource
					a.copilotSourceFingerprint = sha256.Sum256([]byte(nextSource))
					a.mu.Unlock()
				}
				_ = json.NewEncoder(w).Encode(CopilotTokenResponse{
					Token: fmt.Sprintf("responses-bearer-%d", call), ExpiresAt: time.Now().Add(time.Hour).Unix(),
				})
			}))
			defer server.Close()
			a.client, a.copilotBaseURL = server.Client(), server.URL
			first, err := a.GetResponsesCredential(context.Background())
			if err != nil || first.Token != "responses-bearer-1" || first.SourceFingerprint != sha256.Sum256([]byte(source)) {
				t.Fatalf("Responses bearer lost its source during exchange: %v", err)
			}
			a.responsesMu.Lock()
			a.responsesTokenExpiry = time.Time{}
			a.responsesMu.Unlock()
			current, err := a.GetResponsesCredential(context.Background())
			if err != nil || current.Token != "responses-bearer-2" || current.SourceFingerprint != sha256.Sum256([]byte(nextSource)) || calls.Load() != 2 {
				t.Fatalf("refreshed Responses bearer has the wrong source: %v", err)
			}
		})
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
			first, err := original.GetCredential(context.Background())
			if err != nil || calls.Load() != 1 {
				t.Fatalf("initial authentication: error=%v exchanges=%d", err, calls.Load())
			}
			want := sha256.Sum256([]byte(originalSource))
			if first.SourceFingerprint != want {
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
			cached, err := restarted.GetResponsesCredential(context.Background())
			if err != nil || cached.Token != first.Token || calls.Load() != 1 {
				t.Fatalf("cached startup repeated authentication: error=%v exchanges=%d", err, calls.Load())
			}
			if cached.SourceFingerprint != want {
				t.Error("cached bearer lost its issuing credential after restart")
			}
			reject.Store(tc.failRefresh)
			refreshed, err := restarted.RefreshTokenNonInteractive(context.Background())
			if tc.failRefresh {
				if err == nil || calls.Load() != 2 {
					t.Fatalf("replacement refresh: error=%v exchanges=%d", err, calls.Load())
				}
				if cached.SourceFingerprint != want {
					t.Error("failed replacement assigned its credential to the cached bearer")
				}
				return
			}
			if err != nil || refreshed == cached.Token || calls.Load() != 2 {
				t.Fatalf("service-token refresh: error=%v exchanges=%d", err, calls.Load())
			}
			if cached.SourceFingerprint != want {
				t.Error("in-flight bearer lost its issuing credential after refresh")
			}
			current, err := restarted.GetCredential(context.Background())
			if err != nil || current.Token != refreshed {
				t.Fatalf("refreshed credential: error=%v", err)
			}
			nextWant := sha256.Sum256([]byte(tc.nextSource))
			if current.SourceFingerprint != nextWant {
				t.Fatal("refreshed bearer has the wrong issuing credential")
			}
			if tc.nextSource == originalSource && current.SourceFingerprint != first.SourceFingerprint {
				t.Fatal("same-credential refresh changed session identity")
			}
			if tc.nextSource != originalSource && current.SourceFingerprint == first.SourceFingerprint {
				t.Fatal("replacement credential inherited the previous session identity")
			}
			loadedAgain := newAuth()
			last, err := loadedAgain.GetCredential(context.Background())
			if err != nil || last.Token != refreshed || calls.Load() != 2 || last.SourceFingerprint != nextWant {
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
	credential, err := a.GetCredential(context.Background())
	if err != nil || credential.Token != "legacy-cached-bearer" {
		t.Fatalf("legacy cache is no longer usable: %v", err)
	}
	if credential.SourceFingerprint != sha256.Sum256([]byte(credential.Token)) {
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
	first, err := a.GetResponsesCredential(context.Background())
	if err != nil || calls.Load() != 1 {
		t.Fatalf("CLI authentication: error=%v exchanges=%d", err, calls.Load())
	}
	want := sha256.Sum256([]byte(source))
	if first.SourceFingerprint != want {
		t.Fatal("CLI bearer lost its issuing credential")
	}
	refreshed, err := a.RefreshTokenNonInteractive(context.Background())
	if err != nil || refreshed == first.Token || calls.Load() != 2 {
		t.Fatalf("CLI refresh: error=%v exchanges=%d", err, calls.Load())
	}
	current, err := a.GetCredential(context.Background())
	if err != nil || current.Token != refreshed || current.SourceFingerprint != want || first.SourceFingerprint != want {
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
			credential, err := a.GetCredential(context.Background())
			if err != nil || credential.Token != "recovered-service-token" || calls.Load() != 1 {
				t.Fatalf("malformed cache did not reauthenticate: error=%v exchanges=%d", err, calls.Load())
			}
			if credential.SourceFingerprint != sha256.Sum256([]byte(source)) {
				t.Fatal("recovered bearer lost its issuing credential")
			}
		})
	}
}
