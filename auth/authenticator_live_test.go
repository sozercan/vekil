package auth

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type liveCopilotAuthObservation struct {
	host       string
	path       string
	statusCode int
}

type liveCopilotAuthRecorder struct {
	base http.RoundTripper

	mu           sync.Mutex
	observations []liveCopilotAuthObservation
}

func (r *liveCopilotAuthRecorder) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := r.base.RoundTrip(req)
	if err == nil && resp != nil {
		r.mu.Lock()
		r.observations = append(r.observations, liveCopilotAuthObservation{
			host:       req.URL.Host,
			path:       req.URL.Path,
			statusCode: resp.StatusCode,
		})
		r.mu.Unlock()
	}
	return resp, err
}

func (r *liveCopilotAuthRecorder) snapshot() []liveCopilotAuthObservation {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]liveCopilotAuthObservation(nil), r.observations...)
}

func TestLiveEnvAccessTokenDirectBearer(t *testing.T) {
	if os.Getenv("LIVE_COPILOT_DIRECT_BEARER_TEST") != "1" {
		t.Skip("set LIVE_COPILOT_DIRECT_BEARER_TEST=1 to run the credentialed live check")
	}

	envToken := strings.TrimSpace(os.Getenv("COPILOT_GITHUB_TOKEN"))
	if envToken == "" {
		t.Fatal("COPILOT_GITHUB_TOKEN is required for the credentialed live check")
	}
	if !strings.HasPrefix(envToken, "github_pat_") {
		t.Fatal("COPILOT_GITHUB_TOKEN must be a fine-grained personal access token")
	}
	t.Setenv("COPILOT_GITHUB_TOKEN", envToken)

	defaultTransport, ok := http.DefaultTransport.(*http.Transport)
	if !ok {
		t.Fatal("http.DefaultTransport is not an *http.Transport")
	}
	transport := defaultTransport.Clone()
	defer transport.CloseIdleConnections()
	recorder := &liveCopilotAuthRecorder{base: transport}
	tokenDir := t.TempDir()
	a := &Authenticator{
		tokenDir: tokenDir,
		client: &http.Client{
			Transport: recorder,
			Timeout:   30 * time.Second,
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	token, err := a.GetToken(ctx)
	if err != nil {
		t.Fatalf("GetToken() live direct bearer failed: %v", err)
	}
	if token != envToken {
		t.Fatal("GetToken() returned a different token instead of the environment bearer")
	}

	cachedToken, err := a.GetToken(ctx)
	if err != nil {
		t.Fatalf("GetToken() cached direct bearer failed: %v", err)
	}
	if cachedToken != envToken {
		t.Fatal("GetToken() returned a different cached token")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.githubcopilot.com/models", nil)
	if err != nil {
		t.Fatalf("create live Copilot models request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")
	req.Header.Set("Editor-Version", "vscode/1.95.0")
	req.Header.Set("Editor-Plugin-Version", "copilot-chat/0.26.7")
	req.Header.Set("Copilot-Integration-Id", "vscode-chat")

	resp, err := a.client.Do(req)
	if err != nil {
		t.Fatalf("live Copilot models request failed: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("live Copilot models request returned status %d", resp.StatusCode)
	}

	observations := recorder.snapshot()
	if len(observations) != 1 {
		t.Fatalf("live Copilot requests = %d, want exactly one models request", len(observations))
	}
	assertLiveCopilotAuthObservation(t, observations[0], "api.githubcopilot.com", "/models", http.StatusOK)

	if a.accessToken != envToken || a.copilotToken != envToken {
		t.Fatal("authenticator did not retain the environment token as the in-memory direct bearer")
	}
	if !a.tokenExpiry.After(time.Now()) {
		t.Fatalf("direct-bearer expiry = %v, want a future refresh deadline", a.tokenExpiry)
	}
	for _, name := range []string{"access-token", "api-key.json"} {
		if _, err := os.Stat(filepath.Join(tokenDir, name)); !errors.Is(err, os.ErrNotExist) {
			if err == nil {
				t.Fatalf("live direct bearer unexpectedly persisted %s", name)
			}
			t.Fatalf("stat %s: %v", name, err)
		}
	}
}

func assertLiveCopilotAuthObservation(t *testing.T, got liveCopilotAuthObservation, wantHost, wantPath string, wantStatus int) {
	t.Helper()
	if got.host != wantHost || got.path != wantPath || got.statusCode != wantStatus {
		t.Fatalf("live auth request = host %q path %q status %d, want %s %s status %d",
			got.host, got.path, got.statusCode, wantHost, wantPath, wantStatus)
	}
}
