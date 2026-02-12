// Package auth implements GitHub OAuth device code flow authentication
// and Copilot API token management with automatic caching and refresh.
package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	githubClientID  = "Iv1.b507a08c87ecfe98"
	deviceCodeURL   = "https://github.com/login/device/code"
	accessTokenURL  = "https://github.com/login/oauth/access_token"
	copilotTokenURL = "https://api.github.com/copilot_internal/v2/token"
	defaultTokenDir = "~/.config/copilot-proxy"
)

// Authenticator manages GitHub OAuth and Copilot API tokens.
// It handles the device code flow, token caching to disk, and automatic
// refresh using a read-write mutex for concurrent access.
type Authenticator struct {
	tokenDir     string
	accessToken  string
	copilotToken string
	tokenExpiry  time.Time
	mu           sync.RWMutex
	client       *http.Client
}

// DeviceCodeResponse is the response from GitHub's device code endpoint.
type DeviceCodeResponse struct {
	DeviceCode      string `json:"device_code"`
	UserCode        string `json:"user_code"`
	VerificationURI string `json:"verification_uri"`
	ExpiresIn       int    `json:"expires_in"`
	Interval        int    `json:"interval"`
}

// AccessTokenResponse is the response from GitHub's OAuth access token endpoint.
type AccessTokenResponse struct {
	AccessToken      string `json:"access_token"`
	TokenType        string `json:"token_type"`
	Scope            string `json:"scope"`
	Error            string `json:"error,omitempty"`
	ErrorDescription string `json:"error_description,omitempty"`
}

// CopilotTokenResponse is the response from the Copilot token exchange endpoint.
type CopilotTokenResponse struct {
	Token        string `json:"token"`
	ExpiresAt    int64  `json:"expires_at"`
	ErrorDetails string `json:"error_details,omitempty"`
}

// NewAuthenticator creates an Authenticator that stores tokens in tokenDir.
// If tokenDir is empty, it defaults to ~/.config/copilot-proxy.
func NewAuthenticator(tokenDir string) *Authenticator {
	if tokenDir == "" {
		tokenDir = defaultTokenDir
	}
	if strings.HasPrefix(tokenDir, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			tokenDir = filepath.Join(home, tokenDir[1:])
		}
	}
	_ = os.MkdirAll(tokenDir, 0o700)

	return &Authenticator{
		tokenDir: tokenDir,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// GetToken returns a valid Copilot API token, refreshing it if necessary.
// It is safe for concurrent use.
func (a *Authenticator) GetToken(ctx context.Context) (string, error) {
	a.mu.RLock()
	if a.copilotToken != "" && time.Now().Before(a.tokenExpiry) {
		token := a.copilotToken
		a.mu.RUnlock()
		return token, nil
	}
	a.mu.RUnlock()

	a.mu.Lock()
	defer a.mu.Unlock()

	// Double-check after acquiring write lock.
	if a.copilotToken != "" && time.Now().Before(a.tokenExpiry) {
		return a.copilotToken, nil
	}

	if err := a.refreshToken(ctx); err != nil {
		return "", err
	}
	return a.copilotToken, nil
}

func (a *Authenticator) refreshToken(ctx context.Context) error {
	if err := a.loadAccessToken(); err == nil {
		if err := a.exchangeForCopilotToken(ctx); err == nil {
			return nil
		}
	}
	return a.deviceCodeFlow(ctx)
}

func (a *Authenticator) deviceCodeFlow(ctx context.Context) error {
	data := url.Values{
		"client_id": {githubClientID},
		"scope":     {"read:user"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceCodeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return fmt.Errorf("creating device code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("requesting device code: %w", err)
	}
	defer resp.Body.Close()

	var dcResp DeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&dcResp); err != nil {
		return fmt.Errorf("decoding device code response: %w", err)
	}

	fmt.Fprintf(os.Stderr, "Please visit %s and enter code: %s\n", dcResp.VerificationURI, dcResp.UserCode)

	interval := time.Duration(dcResp.Interval) * time.Second
	if interval == 0 {
		interval = 5 * time.Second
	}
	deadline := time.Now().Add(time.Duration(dcResp.ExpiresIn) * time.Second)

	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(interval):
		}

		tokenData := url.Values{
			"client_id":   {githubClientID},
			"device_code": {dcResp.DeviceCode},
			"grant_type":  {"urn:ietf:params:oauth:grant-type:device_code"},
		}
		tokenReq, err := http.NewRequestWithContext(ctx, http.MethodPost, accessTokenURL, strings.NewReader(tokenData.Encode()))
		if err != nil {
			return fmt.Errorf("creating access token request: %w", err)
		}
		tokenReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		tokenReq.Header.Set("Accept", "application/json")

		tokenResp, err := a.client.Do(tokenReq)
		if err != nil {
			return fmt.Errorf("requesting access token: %w", err)
		}

		var atResp AccessTokenResponse
		respBody, _ := io.ReadAll(tokenResp.Body)
		tokenResp.Body.Close()
		if err := json.Unmarshal(respBody, &atResp); err != nil {
			return fmt.Errorf("decoding access token response: %w (body: %s)", err, string(respBody))
		}

		log.Printf("poll response: error=%q token_present=%v", atResp.Error, atResp.AccessToken != "")

		switch atResp.Error {
		case "":
			a.accessToken = atResp.AccessToken
			if err := a.saveAccessToken(); err != nil {
				return fmt.Errorf("saving access token: %w", err)
			}
			return a.exchangeForCopilotToken(ctx)
		case "authorization_pending":
			continue
		case "slow_down":
			interval += 5 * time.Second
			continue
		default:
			return fmt.Errorf("oauth error: %s - %s", atResp.Error, atResp.ErrorDescription)
		}
	}

	return fmt.Errorf("device code flow timed out")
}

func (a *Authenticator) exchangeForCopilotToken(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, copilotTokenURL, nil)
	if err != nil {
		return fmt.Errorf("creating copilot token request: %w", err)
	}
	req.Header.Set("Authorization", "token "+a.accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")

	resp, err := a.client.Do(req)
	if err != nil {
		return fmt.Errorf("requesting copilot token: %w", err)
	}
	defer resp.Body.Close()

	var ctResp CopilotTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&ctResp); err != nil {
		return fmt.Errorf("decoding copilot token response: %w", err)
	}

	if ctResp.Token == "" {
		return fmt.Errorf("empty copilot token: %s", ctResp.ErrorDetails)
	}

	a.copilotToken = ctResp.Token
	a.tokenExpiry = time.Unix(ctResp.ExpiresAt-300, 0)

	if err := a.saveCopilotToken(); err != nil {
		return fmt.Errorf("saving copilot token: %w", err)
	}
	return nil
}

func (a *Authenticator) loadAccessToken() error {
	data, err := os.ReadFile(filepath.Join(a.tokenDir, "access-token"))
	if err != nil {
		return err
	}
	a.accessToken = strings.TrimSpace(string(data))
	return nil
}

func (a *Authenticator) saveAccessToken() error {
	return os.WriteFile(filepath.Join(a.tokenDir, "access-token"), []byte(a.accessToken), 0o600)
}

func (a *Authenticator) loadCopilotToken() error {
	data, err := os.ReadFile(filepath.Join(a.tokenDir, "api-key.json"))
	if err != nil {
		return err
	}
	var ctResp CopilotTokenResponse
	if err := json.Unmarshal(data, &ctResp); err != nil {
		return err
	}
	if time.Now().Unix() >= ctResp.ExpiresAt-300 {
		return fmt.Errorf("copilot token expired")
	}
	a.copilotToken = ctResp.Token
	a.tokenExpiry = time.Unix(ctResp.ExpiresAt-300, 0)
	return nil
}

func (a *Authenticator) saveCopilotToken() error {
	data, err := json.Marshal(CopilotTokenResponse{
		Token:     a.copilotToken,
		ExpiresAt: a.tokenExpiry.Add(300 * time.Second).Unix(),
	})
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(a.tokenDir, "api-key.json"), data, 0o600)
}
