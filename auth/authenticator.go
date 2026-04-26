// Package auth implements GitHub OAuth device code flow authentication
// and Copilot API token management with automatic caching and refresh.
package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	githubClientID      = "Iv1.b507a08c87ecfe98"
	deviceCodeURL       = "https://github.com/login/device/code"
	accessTokenURL      = "https://github.com/login/oauth/access_token"
	copilotTokenURL     = "https://api.github.com/copilot_internal/v2/token"
	copilotUserURL      = "https://api.github.com/copilot_internal/user"
	defaultTokenDir     = "~/.config/vekil"
	githubCLITokenTTL   = 15 * time.Minute
	signedOutMarkerFile = "signed-out"
	authPreferencesFile = "auth-preferences.json"
)

var accessTokenEnvVars = []string{
	"COPILOT_GITHUB_TOKEN",
}

var (
	githubCLITokenTimeout = 5 * time.Second

	githubCLICommonPaths = []string{
		"/opt/homebrew/bin/gh",
		"/usr/local/bin/gh",
		"/usr/bin/gh",
	}

	// ErrNotAuthenticated indicates that no reusable GitHub access token was
	// available for a token refresh attempt.
	ErrNotAuthenticated = errors.New("not authenticated")

	// ErrInvalidAccessToken indicates that a stored GitHub access token exists
	// but can no longer be exchanged for a Copilot token.
	ErrInvalidAccessToken = errors.New("invalid access token")
)

// Authenticator manages GitHub OAuth and Copilot API tokens.
// It handles the device code flow, token caching to disk, and automatic
// refresh using a read-write mutex for concurrent access.
type Authenticator struct {
	tokenDir       string
	accessToken    string
	copilotToken   string
	tokenExpiry    time.Time
	mu             sync.RWMutex
	client         *http.Client
	directClient   *http.Client
	copilotBaseURL string // overridable for tests; defaults to https://api.github.com
	githubCLIPath  string // optional override for tests; defaults to gh lookup/common paths

	// DisableAutoDeviceFlow prevents refreshToken from falling through to the
	// interactive device-code flow. When true, callers (e.g. the menubar app)
	// are expected to drive the flow themselves via RequestDeviceCode /
	// PollForAuthorization.
	DisableAutoDeviceFlow bool
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

// CopilotUserResponse is the response from the Copilot user endpoint used to
// validate GitHub CLI tokens. Only the fields needed for validation are modeled.
type CopilotUserResponse struct {
	Login       string `json:"login,omitempty"`
	ChatEnabled *bool  `json:"chat_enabled,omitempty"`
}

type githubAPIErrorResponse struct {
	Message      string `json:"message,omitempty"`
	Status       string `json:"status,omitempty"`
	ErrorDetails string `json:"error_details,omitempty"`
}

// AuthPreferences stores explicit authentication preferences shared by the
// CLI and menubar app.
type AuthPreferences struct {
	GitHubCLIAutoSignIn bool `json:"github_cli_auto_sign_in,omitempty"`
}

// AuthSource identifies the source of the currently usable authentication
// state.
type AuthSource string

const (
	AuthSourceNone      AuthSource = "none"
	AuthSourceEnv       AuthSource = "env"
	AuthSourceVekil     AuthSource = "vekil"
	AuthSourceGitHubCLI AuthSource = "github-cli"
)

// AuthStatus is a fast snapshot of local authentication state. It never shells
// out to external tools such as gh.
type AuthStatus struct {
	SignedIn             bool
	Source               AuthSource
	GitHubCLIAutoSignIn  bool
	SignedOut            bool
	HasValidCopilotCache bool
	HasVekilAccessToken  bool
}

// NewAuthenticator creates an Authenticator that stores tokens in tokenDir.
// If tokenDir is empty, it defaults to ~/.config/vekil.
func NewAuthenticator(tokenDir string) (*Authenticator, error) {
	if tokenDir == "" {
		tokenDir = defaultTokenDir
	}
	if strings.HasPrefix(tokenDir, "~") {
		home, err := os.UserHomeDir()
		if err == nil {
			tokenDir = filepath.Join(home, tokenDir[1:])
		}
	}
	if err := os.MkdirAll(tokenDir, 0o700); err != nil {
		return nil, fmt.Errorf("creating token directory: %w", err)
	}

	return &Authenticator{
		tokenDir:     tokenDir,
		client:       newAuthHTTPClient(30*time.Second, true),
		directClient: newAuthHTTPClient(30*time.Second, false),
	}, nil
}

// IsSignedIn reports whether the authenticator has a usable or explicitly
// configured local authentication source.
func (a *Authenticator) IsSignedIn() bool {
	return a.Status().SignedIn
}

// Status returns a fast snapshot of local authentication state without making
// network requests or invoking external commands.
func (a *Authenticator) Status() AuthStatus {
	status := AuthStatus{
		Source:              AuthSourceNone,
		GitHubCLIAutoSignIn: a.isGitHubCLIAutoSignInEnabled(),
		SignedOut:           a.hasSignedOutMarker(),
	}

	status.HasVekilAccessToken = a.hasAccessTokenOnDisk()
	status.HasValidCopilotCache = a.hasValidCopilotTokenOnDisk()

	a.mu.RLock()
	memoryAccessToken := strings.TrimSpace(a.accessToken) != ""
	memoryCopilotToken := a.copilotToken != "" && time.Now().Before(a.tokenExpiry)
	a.mu.RUnlock()

	if token, _ := lookupAccessTokenFromEnv(); token != "" {
		status.SignedIn = true
		status.Source = AuthSourceEnv
		return status
	}

	if status.HasVekilAccessToken {
		status.SignedIn = true
		status.Source = AuthSourceVekil
		return status
	}

	if memoryAccessToken {
		status.SignedIn = true
		if status.GitHubCLIAutoSignIn && (memoryCopilotToken || status.HasValidCopilotCache) {
			status.Source = AuthSourceGitHubCLI
		} else {
			status.Source = AuthSourceVekil
		}
		return status
	}

	if memoryCopilotToken || status.HasValidCopilotCache {
		status.SignedIn = true
		if status.GitHubCLIAutoSignIn {
			status.Source = AuthSourceGitHubCLI
		} else {
			status.Source = AuthSourceVekil
		}
		return status
	}

	if status.GitHubCLIAutoSignIn && !status.SignedOut {
		status.SignedIn = true
		status.Source = AuthSourceGitHubCLI
	}

	return status
}

// hasAccessTokenOnDisk returns true when the access-token file exists and is
// non-empty. Must NOT be called with the write lock held.
func (a *Authenticator) hasAccessTokenOnDisk() bool {
	data, err := os.ReadFile(filepath.Join(a.tokenDir, "access-token"))
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(data)) != ""
}

// hasValidCopilotTokenOnDisk returns true when the persisted Copilot token file
// exists, is valid JSON, and has not expired yet. Must NOT be called with the
// write lock held.
func (a *Authenticator) hasValidCopilotTokenOnDisk() bool {
	data, err := os.ReadFile(filepath.Join(a.tokenDir, "api-key.json"))
	if err != nil {
		return false
	}

	var ctResp CopilotTokenResponse
	if err := json.Unmarshal(data, &ctResp); err != nil {
		return false
	}

	return ctResp.Token != "" && time.Now().Unix() < ctResp.ExpiresAt-300
}

// GetToken returns a valid Copilot API token, refreshing it if necessary.
// It is safe for concurrent use.
func (a *Authenticator) GetToken(ctx context.Context) (string, error) {
	return a.getToken(ctx, !a.DisableAutoDeviceFlow)
}

// GetTokenNonInteractive returns a valid Copilot API token without falling
// back to the interactive device-code flow.
func (a *Authenticator) GetTokenNonInteractive(ctx context.Context) (string, error) {
	return a.getToken(ctx, false)
}

// RefreshTokenNonInteractive refreshes the Copilot API token using existing
// GitHub authentication without falling back to the interactive device-code
// flow. Unlike GetTokenNonInteractive, it bypasses any cached Copilot token so
// callers can verify that the underlying GitHub auth is still refreshable.
func (a *Authenticator) RefreshTokenNonInteractive(ctx context.Context) (string, error) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if err := a.refreshToken(ctx, false); err != nil {
		return "", err
	}
	return a.copilotToken, nil
}

// IsInteractiveLoginRequired reports whether resolving the error should fall
// back to an interactive device-code login flow.
func IsInteractiveLoginRequired(err error) bool {
	return errors.Is(err, ErrNotAuthenticated) || errors.Is(err, ErrInvalidAccessToken)
}

func (a *Authenticator) getToken(ctx context.Context, allowDeviceFlow bool) (string, error) {
	if envToken, _ := lookupAccessTokenFromEnv(); envToken != "" {
		return a.getTokenFromEnv(ctx, envToken)
	}

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
	if err := a.loadCopilotToken(); err == nil {
		return a.copilotToken, nil
	}

	if err := a.refreshToken(ctx, allowDeviceFlow); err != nil {
		return "", err
	}
	return a.copilotToken, nil
}

func (a *Authenticator) getTokenFromEnv(ctx context.Context, envToken string) (string, error) {
	a.mu.RLock()
	if a.accessToken == envToken && a.copilotToken != "" && time.Now().Before(a.tokenExpiry) {
		token := a.copilotToken
		a.mu.RUnlock()
		return token, nil
	}
	a.mu.RUnlock()

	a.mu.Lock()
	defer a.mu.Unlock()

	// Double-check after acquiring write lock.
	if a.accessToken == envToken && a.copilotToken != "" && time.Now().Before(a.tokenExpiry) {
		return a.copilotToken, nil
	}

	// Environment-provided access tokens intentionally override any persisted
	// login state so CI or explicit shell configuration always wins.
	if a.accessToken != envToken {
		a.accessToken = envToken
		a.copilotToken = ""
		a.tokenExpiry = time.Time{}
	}

	if err := a.exchangeForCopilotToken(ctx); err != nil {
		return "", err
	}
	return a.copilotToken, nil
}

func (a *Authenticator) refreshToken(ctx context.Context, allowDeviceFlow bool) error {
	if envToken, _ := lookupAccessTokenFromEnv(); envToken != "" {
		a.accessToken = envToken
		return a.exchangeForCopilotToken(ctx)
	}

	currentAccessToken := a.accessToken
	var refreshErr error

	if currentAccessToken != "" {
		if err := a.exchangeForCopilotToken(ctx); err == nil {
			return nil
		} else {
			refreshErr = err
		}
	}

	if err := a.loadAccessToken(); err == nil {
		if a.accessToken != currentAccessToken || currentAccessToken == "" {
			if err := a.exchangeForCopilotToken(ctx); err == nil {
				return nil
			} else {
				refreshErr = err
			}
		}
	} else if refreshErr == nil && !os.IsNotExist(err) {
		refreshErr = fmt.Errorf("loading access token: %w", err)
	}

	// If there is no Vekil-managed GitHub token, or the existing token is no
	// longer usable, try borrowing the active GitHub CLI token before falling
	// back to the interactive device-code flow. Do not do this after transient
	// Copilot exchange failures, or after an explicit Vekil sign-out; those
	// should be surfaced as-is.
	if refreshErr == nil || IsInteractiveLoginRequired(refreshErr) {
		if !a.hasSignedOutMarker() && a.isGitHubCLIAutoSignInEnabled() {
			if err := a.useGitHubCLICopilotToken(ctx); err == nil {
				return nil
			} else if !errors.Is(err, ErrNotAuthenticated) || refreshErr == nil {
				refreshErr = err
			}
		}
	}

	if refreshErr == nil {
		refreshErr = ErrNotAuthenticated
	}
	if !allowDeviceFlow {
		return refreshErr
	}
	if IsInteractiveLoginRequired(refreshErr) {
		return a.deviceCodeFlow(ctx)
	}
	return refreshErr
}

// RequestDeviceCode initiates the GitHub device-code flow by requesting a
// device code and user code from GitHub. The caller should present the
// UserCode and VerificationURI to the user, then call PollForAuthorization.
// No lock is required — only immutable fields (client) are accessed.
func (a *Authenticator) RequestDeviceCode(ctx context.Context) (*DeviceCodeResponse, error) {
	data := url.Values{
		"client_id": {githubClientID},
		"scope":     {"read:user"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, deviceCodeURL, strings.NewReader(data.Encode()))
	if err != nil {
		return nil, fmt.Errorf("creating device code request: %w", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")

	resp, err := a.do(req)
	if err != nil {
		return nil, fmt.Errorf("requesting device code: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var dcResp DeviceCodeResponse
	if err := json.NewDecoder(resp.Body).Decode(&dcResp); err != nil {
		return nil, fmt.Errorf("decoding device code response: %w", err)
	}
	return &dcResp, nil
}

// PollForAuthorization polls GitHub until the user authorizes the device code,
// then saves the access token and exchanges it for a Copilot API token.
// It acquires the write lock internally.
func (a *Authenticator) PollForAuthorization(ctx context.Context, dcResp *DeviceCodeResponse) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.pollForAuthorization(ctx, dcResp)
}

// pollForAuthorization is the internal implementation that must be called with
// the write lock already held (or from PollForAuthorization which acquires it).
func (a *Authenticator) pollForAuthorization(ctx context.Context, dcResp *DeviceCodeResponse) error {
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

		tokenResp, err := a.do(tokenReq)
		if err != nil {
			return fmt.Errorf("requesting access token: %w", err)
		}

		var atResp AccessTokenResponse
		respBody, _ := io.ReadAll(tokenResp.Body)
		_ = tokenResp.Body.Close()
		if err := json.Unmarshal(respBody, &atResp); err != nil {
			return fmt.Errorf("decoding access token response: %w (body: %s)", err, string(respBody))
		}

		switch atResp.Error {
		case "":
			a.accessToken = atResp.AccessToken
			if err := a.saveAccessToken(); err != nil {
				return fmt.Errorf("saving access token: %w", err)
			}
			if err := a.exchangeForCopilotToken(ctx); err != nil {
				return err
			}
			if err := a.clearSignedOutMarker(); err != nil {
				return fmt.Errorf("clearing signed-out marker: %w", err)
			}
			if err := a.setGitHubCLIAutoSignIn(false); err != nil {
				return fmt.Errorf("saving auth preferences: %w", err)
			}
			return nil
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

func (a *Authenticator) deviceCodeFlow(ctx context.Context) error {
	dcResp, err := a.RequestDeviceCode(ctx)
	if err != nil {
		return err
	}

	_, _ = fmt.Fprintf(os.Stderr, "Please visit %s and enter code: %s\n", dcResp.VerificationURI, dcResp.UserCode)

	return a.pollForAuthorization(ctx, dcResp)
}

// SignOut clears all authentication state from memory and removes persisted
// token files from disk. It is safe for concurrent use.
func (a *Authenticator) SignOut() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.accessToken = ""
	a.copilotToken = ""
	a.tokenExpiry = time.Time{}

	var errs []error
	for _, name := range []string{"access-token", "api-key.json"} {
		if err := os.Remove(filepath.Join(a.tokenDir, name)); err != nil && !os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("remove %s: %w", name, err))
		}
	}
	if err := a.setGitHubCLIAutoSignIn(false); err != nil {
		errs = append(errs, fmt.Errorf("writing auth preferences: %w", err))
	}
	if err := a.markSignedOut(); err != nil {
		errs = append(errs, fmt.Errorf("writing signed-out marker: %w", err))
	}
	if len(errs) > 0 {
		return fmt.Errorf("sign out cleanup: %w", errors.Join(errs...))
	}
	return nil
}

// SignInWithGitHubCLI explicitly signs in using the currently authenticated
// GitHub CLI account. The GitHub CLI token is kept only in memory as a
// short-lived Copilot bearer token and is never persisted by Vekil.
func (a *Authenticator) SignInWithGitHubCLI(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	previousAccessToken := a.accessToken
	previousCopilotToken := a.copilotToken
	previousTokenExpiry := a.tokenExpiry

	if err := a.useGitHubCLICopilotToken(ctx); err != nil {
		a.accessToken = previousAccessToken
		a.copilotToken = previousCopilotToken
		a.tokenExpiry = previousTokenExpiry
		return err
	}
	for _, name := range []string{"access-token", "api-key.json"} {
		if err := os.Remove(filepath.Join(a.tokenDir, name)); err != nil && !os.IsNotExist(err) {
			return fmt.Errorf("removing stale %s: %w", name, err)
		}
	}
	if err := a.clearSignedOutMarker(); err != nil {
		return fmt.Errorf("clearing signed-out marker: %w", err)
	}
	if err := a.setGitHubCLIAutoSignIn(true); err != nil {
		return fmt.Errorf("saving auth preferences: %w", err)
	}

	return nil
}

func (a *Authenticator) getCopilotTokenURL() string {
	if a.copilotBaseURL != "" {
		return a.copilotBaseURL + "/copilot_internal/v2/token"
	}
	return copilotTokenURL
}

func (a *Authenticator) getCopilotUserURL() string {
	if a.copilotBaseURL != "" {
		return a.copilotBaseURL + "/copilot_internal/user"
	}
	return copilotUserURL
}

func (a *Authenticator) useGitHubCLICopilotToken(ctx context.Context) error {
	accessToken, err := a.gitHubCLIAccessToken(ctx)
	if err != nil {
		return err
	}

	if err := a.validateGitHubCLIToken(ctx, accessToken); err != nil {
		return err
	}

	// GitHub CLI OAuth tokens are accepted directly by the Copilot API as bearer
	// tokens. Keep them in memory only: do not persist them as Vekil-managed
	// GitHub access tokens, do not write them to the Copilot token cache, and do
	// not feed them through the legacy Copilot token exchange endpoint. Some
	// GitHub CLI OAuth tokens can validate against Copilot but return 404 from
	// that exchange.
	a.accessToken = ""
	a.copilotToken = accessToken
	a.tokenExpiry = time.Now().Add(githubCLITokenTTL)
	return nil
}

func (a *Authenticator) validateGitHubCLIToken(ctx context.Context, accessToken string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.getCopilotUserURL(), nil)
	if err != nil {
		return fmt.Errorf("creating copilot user request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")

	resp, err := a.do(req)
	if err != nil {
		return fmt.Errorf("validating github cli copilot access: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		detail := readGitHubAPIErrorDetail(resp.Body)
		if detail != "" {
			detail = ": " + detail
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%w: github cli token cannot access Copilot (status %d%s)", ErrInvalidAccessToken, resp.StatusCode, detail)
		}
		return fmt.Errorf("github cli copilot validation failed with status %d%s", resp.StatusCode, detail)
	}

	var user CopilotUserResponse
	if err := json.NewDecoder(resp.Body).Decode(&user); err != nil {
		return fmt.Errorf("decoding copilot user response: %w", err)
	}
	if user.ChatEnabled != nil && !*user.ChatEnabled {
		return fmt.Errorf("%w: github cli account does not have Copilot Chat enabled", ErrInvalidAccessToken)
	}
	return nil
}

func readGitHubAPIErrorDetail(body io.Reader) string {
	data, err := io.ReadAll(io.LimitReader(body, 4096))
	if err != nil {
		return ""
	}
	text := strings.TrimSpace(string(data))
	if text == "" {
		return ""
	}

	var apiErr githubAPIErrorResponse
	if err := json.Unmarshal(data, &apiErr); err == nil {
		for _, detail := range []string{apiErr.ErrorDetails, apiErr.Message, apiErr.Status} {
			if strings.TrimSpace(detail) != "" {
				return strings.TrimSpace(detail)
			}
		}
	}
	return text
}

func (a *Authenticator) exchangeForCopilotToken(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, a.getCopilotTokenURL(), nil)
	if err != nil {
		return fmt.Errorf("creating copilot token request: %w", err)
	}
	req.Header.Set("Authorization", "token "+a.accessToken)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "GitHubCopilotChat/0.26.7")

	resp, err := a.do(req)
	if err != nil {
		return fmt.Errorf("requesting copilot token: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var ctResp CopilotTokenResponse
	if err := json.NewDecoder(resp.Body).Decode(&ctResp); err != nil {
		return fmt.Errorf("decoding copilot token response: %w", err)
	}

	if ctResp.Token == "" {
		if resp.StatusCode == http.StatusUnauthorized || isInvalidAccessTokenDetail(ctResp.ErrorDetails) {
			if strings.TrimSpace(ctResp.ErrorDetails) == "" {
				return ErrInvalidAccessToken
			}
			return fmt.Errorf("%w: %s", ErrInvalidAccessToken, ctResp.ErrorDetails)
		}
		if resp.StatusCode != http.StatusOK {
			if strings.TrimSpace(ctResp.ErrorDetails) != "" {
				return fmt.Errorf("copilot token request failed with status %d: %s", resp.StatusCode, ctResp.ErrorDetails)
			}
			return fmt.Errorf("copilot token request failed with status %d", resp.StatusCode)
		}
		if strings.TrimSpace(ctResp.ErrorDetails) == "" {
			return fmt.Errorf("empty copilot token")
		}
		return fmt.Errorf("empty copilot token: %s", ctResp.ErrorDetails)
	}

	a.copilotToken = ctResp.Token
	a.tokenExpiry = time.Unix(ctResp.ExpiresAt-300, 0)

	if err := a.saveCopilotToken(); err != nil {
		return fmt.Errorf("saving copilot token: %w", err)
	}
	return nil
}

func newAuthHTTPClient(timeout time.Duration, useProxy bool) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if !useProxy {
		transport.Proxy = nil
	}

	return &http.Client{
		Timeout:   timeout,
		Transport: transport,
	}
}

func (a *Authenticator) do(req *http.Request) (*http.Response, error) {
	client := a.client
	if client == nil {
		client = newAuthHTTPClient(30*time.Second, true)
	}

	resp, err := client.Do(req)
	if err == nil || !shouldRetryWithoutProxy(req, err) {
		return resp, err
	}

	retryReq, retryErr := cloneRequest(req)
	if retryErr != nil {
		return nil, err
	}

	directClient := a.directClient
	if directClient == nil {
		timeout := client.Timeout
		if timeout == 0 {
			timeout = 30 * time.Second
		}
		directClient = newAuthHTTPClient(timeout, false)
	}

	resp, retryErr = directClient.Do(retryReq)
	if retryErr == nil {
		return resp, nil
	}

	return nil, fmt.Errorf("%w; direct retry without loopback proxy also failed: %v", err, retryErr)
}

func cloneRequest(req *http.Request) (*http.Request, error) {
	cloned := req.Clone(req.Context())
	if req.Body == nil || req.Body == http.NoBody {
		return cloned, nil
	}
	if req.GetBody == nil {
		return nil, errors.New("request body cannot be retried")
	}

	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	cloned.Body = body
	cloned.GetBody = req.GetBody
	return cloned, nil
}

func shouldRetryWithoutProxy(req *http.Request, err error) bool {
	if req == nil || req.URL == nil || err == nil {
		return false
	}

	proxyURL := proxyURLFromEnvironment(req.URL.Scheme)
	if proxyURL == nil || !isLoopbackProxyHost(proxyURL.Hostname()) {
		return false
	}

	errText := strings.ToLower(err.Error())
	return strings.Contains(errText, "proxyconnect tcp:") && strings.Contains(errText, "connection refused")
}

func proxyURLFromEnvironment(scheme string) *url.URL {
	for _, raw := range proxyEnvCandidates(scheme) {
		if strings.TrimSpace(raw) == "" {
			continue
		}

		proxyURL, err := url.Parse(raw)
		if err == nil && proxyURL.Host != "" {
			return proxyURL
		}

		if strings.Contains(raw, "://") {
			continue
		}

		proxyURL, err = url.Parse("http://" + raw)
		if err == nil && proxyURL.Host != "" {
			return proxyURL
		}
	}

	return nil
}

func proxyEnvCandidates(scheme string) []string {
	if strings.EqualFold(scheme, "https") {
		return []string{
			os.Getenv("HTTPS_PROXY"),
			os.Getenv("https_proxy"),
			os.Getenv("HTTP_PROXY"),
			os.Getenv("http_proxy"),
			os.Getenv("ALL_PROXY"),
			os.Getenv("all_proxy"),
		}
	}

	return []string{
		os.Getenv("HTTP_PROXY"),
		os.Getenv("http_proxy"),
		os.Getenv("ALL_PROXY"),
		os.Getenv("all_proxy"),
	}
}

func isLoopbackProxyHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}

	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func (a *Authenticator) loadAccessToken() error {
	data, err := os.ReadFile(filepath.Join(a.tokenDir, "access-token"))
	if err != nil {
		return err
	}
	a.accessToken = strings.TrimSpace(string(data))
	if a.accessToken == "" {
		return ErrNotAuthenticated
	}
	return nil
}

func (a *Authenticator) signedOutMarkerPath() string {
	return filepath.Join(a.tokenDir, signedOutMarkerFile)
}

func (a *Authenticator) authPreferencesPath() string {
	return filepath.Join(a.tokenDir, authPreferencesFile)
}

func (a *Authenticator) loadAuthPreferences() (AuthPreferences, error) {
	data, err := os.ReadFile(a.authPreferencesPath())
	if err != nil {
		if os.IsNotExist(err) {
			return AuthPreferences{}, nil
		}
		return AuthPreferences{}, err
	}

	var prefs AuthPreferences
	if err := json.Unmarshal(data, &prefs); err != nil {
		return AuthPreferences{}, err
	}
	return prefs, nil
}

func (a *Authenticator) saveAuthPreferences(prefs AuthPreferences) error {
	data, err := json.MarshalIndent(prefs, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWriteFile(a.authPreferencesPath(), data, 0o600)
}

func (a *Authenticator) isGitHubCLIAutoSignInEnabled() bool {
	prefs, err := a.loadAuthPreferences()
	if err != nil {
		return false
	}
	return prefs.GitHubCLIAutoSignIn
}

func (a *Authenticator) setGitHubCLIAutoSignIn(enabled bool) error {
	prefs, err := a.loadAuthPreferences()
	if err != nil {
		prefs = AuthPreferences{}
	}
	prefs.GitHubCLIAutoSignIn = enabled
	return a.saveAuthPreferences(prefs)
}

// GitHubCLIAutoSignInEnabled reports whether the user has explicitly opted in
// to automatic GitHub CLI sign-in. Malformed preferences are treated as false.
func (a *Authenticator) GitHubCLIAutoSignInEnabled() bool {
	return a.isGitHubCLIAutoSignInEnabled()
}

func (a *Authenticator) hasSignedOutMarker() bool {
	_, err := os.Stat(a.signedOutMarkerPath())
	return err == nil || !os.IsNotExist(err)
}

func (a *Authenticator) markSignedOut() error {
	return atomicWriteFile(a.signedOutMarkerPath(), []byte("signed out\n"), 0o600)
}

func (a *Authenticator) clearSignedOutMarker() error {
	if err := os.Remove(a.signedOutMarkerPath()); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (a *Authenticator) gitHubCLIAccessToken(ctx context.Context) (string, error) {
	ghPath, err := a.gitHubCLIExecutable()
	if err != nil {
		return "", err
	}

	cmdCtx, cancel := context.WithTimeout(ctx, githubCLITokenTimeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, ghPath, "auth", "token", "--hostname", "github.com")
	cmd.Env = githubCLIEnvironment()

	output, err := cmd.Output()
	if err != nil {
		if cmdCtx.Err() != nil {
			return "", cmdCtx.Err()
		}
		return "", fmt.Errorf("%w: github cli auth token unavailable", ErrNotAuthenticated)
	}

	token := strings.TrimSpace(string(output))
	if token == "" {
		return "", fmt.Errorf("%w: github cli returned an empty token", ErrNotAuthenticated)
	}
	return token, nil
}

func (a *Authenticator) gitHubCLIExecutable() (string, error) {
	if strings.TrimSpace(a.githubCLIPath) != "" {
		return a.githubCLIPath, nil
	}

	if path, err := exec.LookPath("gh"); err == nil {
		return path, nil
	}

	for _, path := range githubCLICommonPaths {
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		if info.Mode()&0o111 != 0 {
			return path, nil
		}
	}

	return "", fmt.Errorf("%w: gh executable not found", ErrNotAuthenticated)
}

func githubCLIEnvironment() []string {
	env := os.Environ()
	filtered := make([]string, 0, len(env)+1)

	for _, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "GH_TOKEN", "GITHUB_TOKEN", "GH_PROMPT_DISABLED":
			continue
		default:
			filtered = append(filtered, kv)
		}
	}

	// Ensure gh never opens an interactive prompt when Vekil is only probing for
	// an existing CLI login.
	filtered = append(filtered, "GH_PROMPT_DISABLED=1")
	return filtered
}

func (a *Authenticator) saveAccessToken() error {
	return atomicWriteFile(filepath.Join(a.tokenDir, "access-token"), []byte(a.accessToken), 0o600)
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
	return atomicWriteFile(filepath.Join(a.tokenDir, "api-key.json"), data, 0o600)
}

func lookupAccessTokenFromEnv() (string, string) {
	for _, name := range accessTokenEnvVars {
		if token := strings.TrimSpace(os.Getenv(name)); token != "" {
			return token, name
		}
	}
	return "", ""
}

func isInvalidAccessTokenDetail(detail string) bool {
	normalized := strings.ToLower(strings.TrimSpace(detail))
	return strings.Contains(normalized, "invalid access token")
}

// atomicWriteFile writes data to a temporary file in the same directory as
// path and then renames it into place, ensuring the target file is never
// partially written.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return fmt.Errorf("creating temp file: %w", err)
	}
	tmpPath := tmp.Name()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("writing temp file: %w", err)
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("setting temp file permissions: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("closing temp file: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("renaming temp file: %w", err)
	}
	return nil
}
