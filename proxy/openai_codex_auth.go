package proxy

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	defaultOpenAICodexBaseURL       = "https://chatgpt.com/backend-api/codex"
	defaultOpenAICodexClientVersion = "1.0.0"
	openAICodexAuthMode             = "chatgpt"
	openAICodexClientID             = "app_EMoamEEZ73f0CkXaXp7hrann"
	openAICodexRefreshURL           = "https://auth.openai.com/oauth/token"
	openAICodexRefreshURLEnv        = "CODEX_REFRESH_TOKEN_URL_OVERRIDE"
	openAICodexRefreshSkew          = 30 * time.Second
	openAICodexRefreshInterval      = 8 * 24 * time.Hour
)

type openAICodexAuth struct {
	path string
	mu   sync.Mutex
}

type openAICodexTokenData struct {
	IDToken      string `json:"id_token,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
}

type openAICodexAuthState struct {
	raw         map[string]json.RawMessage
	tokens      openAICodexTokenData
	lastRefresh *time.Time
}

type openAICodexCredentials struct {
	accessToken string
	accountID   string
	fedRAMP     bool
}

type openAICodexRefreshResponse struct {
	IDToken      string `json:"id_token"`
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
}

func newOpenAICodexAuth() (*openAICodexAuth, error) {
	codexHome := strings.TrimSpace(os.Getenv("CODEX_HOME"))
	if codexHome == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("resolve CODEX_HOME: %w", err)
		}
		codexHome = filepath.Join(home, ".codex")
	}
	return &openAICodexAuth{path: filepath.Join(codexHome, "auth.json")}, nil
}

func (a *openAICodexAuth) credentials(ctx context.Context, client *http.Client) (openAICodexCredentials, error) {
	if a == nil {
		return openAICodexCredentials{}, fmt.Errorf("OpenAI Codex auth is not configured")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	state, err := a.read()
	if err != nil {
		return openAICodexCredentials{}, err
	}
	tokens := state.tokens
	if !openAICodexNeedsRefresh(tokens.AccessToken, state.lastRefresh, time.Now()) {
		return openAICodexCredentialsFromTokens(tokens), nil
	}
	if strings.TrimSpace(tokens.RefreshToken) == "" {
		return openAICodexCredentials{}, fmt.Errorf("OpenAI Codex access token expired or stale and auth.json has no refresh_token; run `codex login`")
	}

	refreshed, err := requestOpenAICodexTokenRefresh(ctx, client, tokens.RefreshToken)
	if err != nil {
		return openAICodexCredentials{}, err
	}
	if strings.TrimSpace(refreshed.AccessToken) != "" {
		tokens.AccessToken = refreshed.AccessToken
	}
	if strings.TrimSpace(refreshed.IDToken) != "" {
		tokens.IDToken = refreshed.IDToken
	}
	if strings.TrimSpace(refreshed.RefreshToken) != "" {
		tokens.RefreshToken = refreshed.RefreshToken
	}
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return openAICodexCredentials{}, fmt.Errorf("OpenAI Codex token refresh returned no access_token")
	}

	if err := a.write(state.raw, tokens); err != nil {
		return openAICodexCredentials{}, err
	}
	return openAICodexCredentialsFromTokens(tokens), nil
}

func (a *openAICodexAuth) read() (openAICodexAuthState, error) {
	body, err := os.ReadFile(a.path)
	if err != nil {
		return openAICodexAuthState{}, fmt.Errorf("read OpenAI Codex auth file %q: %w; run `codex login` first", a.path, err)
	}

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return openAICodexAuthState{}, fmt.Errorf("decode OpenAI Codex auth file %q: %w", a.path, err)
	}

	var authMode string
	if err := json.Unmarshal(raw["auth_mode"], &authMode); err != nil || authMode != openAICodexAuthMode {
		return openAICodexAuthState{}, fmt.Errorf("OpenAI Codex auth file %q must use auth_mode %q; run `codex login` with ChatGPT auth", a.path, openAICodexAuthMode)
	}

	var tokens openAICodexTokenData
	if len(raw["tokens"]) == 0 || string(raw["tokens"]) == "null" {
		return openAICodexAuthState{}, fmt.Errorf("OpenAI Codex auth file %q has no ChatGPT tokens; run `codex login`", a.path)
	}
	if err := json.Unmarshal(raw["tokens"], &tokens); err != nil {
		return openAICodexAuthState{}, fmt.Errorf("decode OpenAI Codex tokens in %q: %w", a.path, err)
	}
	if strings.TrimSpace(tokens.AccessToken) == "" {
		return openAICodexAuthState{}, fmt.Errorf("OpenAI Codex auth file %q has no access_token; run `codex login`", a.path)
	}

	lastRefresh := parseOpenAICodexLastRefresh(raw["last_refresh"])

	return openAICodexAuthState{raw: raw, tokens: tokens, lastRefresh: lastRefresh}, nil
}

func (a *openAICodexAuth) write(raw map[string]json.RawMessage, tokens openAICodexTokenData) error {
	tokensRaw, err := json.Marshal(tokens)
	if err != nil {
		return fmt.Errorf("marshal refreshed OpenAI Codex tokens: %w", err)
	}
	lastRefreshRaw, err := json.Marshal(time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("marshal OpenAI Codex last_refresh: %w", err)
	}
	raw["tokens"] = tokensRaw
	raw["last_refresh"] = lastRefreshRaw

	body, err := json.MarshalIndent(raw, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal OpenAI Codex auth file %q: %w", a.path, err)
	}
	body = append(body, '\n')

	tmpPath := a.path + ".tmp"
	if err := os.WriteFile(tmpPath, body, 0o600); err != nil {
		return fmt.Errorf("write temporary OpenAI Codex auth file %q: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, a.path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("replace OpenAI Codex auth file %q: %w", a.path, err)
	}
	if err := os.Chmod(a.path, 0o600); err != nil {
		return fmt.Errorf("chmod OpenAI Codex auth file %q: %w", a.path, err)
	}
	return nil
}

func openAICodexCredentialsFromTokens(tokens openAICodexTokenData) openAICodexCredentials {
	accountID := strings.TrimSpace(tokens.AccountID)
	idClaims := openAICodexJWTClaims(tokens.IDToken)
	if accountID == "" {
		accountID = idClaims.chatGPTAccountID
	}
	return openAICodexCredentials{
		accessToken: strings.TrimSpace(tokens.AccessToken),
		accountID:   accountID,
		fedRAMP:     idClaims.fedRAMP,
	}
}

func requestOpenAICodexTokenRefresh(ctx context.Context, client *http.Client, refreshToken string) (openAICodexRefreshResponse, error) {
	if client == nil {
		client = http.DefaultClient
	}

	refreshURL := strings.TrimSpace(os.Getenv(openAICodexRefreshURLEnv))
	if refreshURL == "" {
		refreshURL = openAICodexRefreshURL
	}

	body, err := json.Marshal(map[string]string{
		"client_id":     openAICodexClientID,
		"grant_type":    "refresh_token",
		"refresh_token": refreshToken,
	})
	if err != nil {
		return openAICodexRefreshResponse{}, err
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, refreshURL, bytes.NewReader(body))
	if err != nil {
		return openAICodexRefreshResponse{}, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return openAICodexRefreshResponse{}, fmt.Errorf("OpenAI Codex token refresh failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return openAICodexRefreshResponse{}, fmt.Errorf("read OpenAI Codex token refresh response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return openAICodexRefreshResponse{}, fmt.Errorf("OpenAI Codex token refresh failed (HTTP %d): %s", resp.StatusCode, strings.TrimSpace(string(respBody)))
	}

	var refreshed openAICodexRefreshResponse
	if err := json.Unmarshal(respBody, &refreshed); err != nil {
		return openAICodexRefreshResponse{}, fmt.Errorf("decode OpenAI Codex token refresh response: %w", err)
	}
	return refreshed, nil
}

func openAICodexNeedsRefresh(accessToken string, lastRefresh *time.Time, now time.Time) bool {
	if openAICodexJWTExpiresSoon(accessToken, now, openAICodexRefreshSkew) {
		return true
	}
	if _, ok := openAICodexJWTExpiration(accessToken); ok {
		return false
	}
	if lastRefresh == nil || lastRefresh.IsZero() {
		return true
	}
	return !now.Before(lastRefresh.Add(openAICodexRefreshInterval))
}

func parseOpenAICodexLastRefresh(raw json.RawMessage) *time.Time {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil
	}

	var value string
	if err := json.Unmarshal(raw, &value); err == nil {
		value = strings.TrimSpace(value)
		if value == "" {
			return nil
		}
		for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
			if parsed, err := time.Parse(layout, value); err == nil {
				parsed = parsed.UTC()
				return &parsed
			}
		}
		return nil
	}

	var unixSeconds int64
	if err := json.Unmarshal(raw, &unixSeconds); err == nil {
		parsed := time.Unix(unixSeconds, 0).UTC()
		return &parsed
	}

	return nil
}

func openAICodexJWTExpiresSoon(token string, now time.Time, skew time.Duration) bool {
	exp, ok := openAICodexJWTExpiration(token)
	if !ok {
		return false
	}
	return !now.Before(exp.Add(-skew))
}

func openAICodexJWTExpiration(token string) (time.Time, bool) {
	claims := openAICodexJWTClaims(token)
	if claims.exp == 0 {
		return time.Time{}, false
	}
	exp := time.Unix(claims.exp, 0)
	return exp, true
}

type openAICodexClaims struct {
	exp              int64
	chatGPTAccountID string
	fedRAMP          bool
}

func openAICodexJWTClaims(token string) openAICodexClaims {
	payload, ok := decodeOpenAICodexJWTPayload(token)
	if !ok {
		return openAICodexClaims{}
	}

	var claims struct {
		Exp  int64 `json:"exp"`
		Auth struct {
			ChatGPTAccountID      string `json:"chatgpt_account_id"`
			ChatGPTAccountFedRAMP bool   `json:"chatgpt_account_is_fedramp"`
		} `json:"https://api.openai.com/auth"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil {
		return openAICodexClaims{}
	}
	return openAICodexClaims{
		exp:              claims.Exp,
		chatGPTAccountID: strings.TrimSpace(claims.Auth.ChatGPTAccountID),
		fedRAMP:          claims.Auth.ChatGPTAccountFedRAMP,
	}
}

func decodeOpenAICodexJWTPayload(token string) ([]byte, bool) {
	parts := strings.Split(token, ".")
	if len(parts) < 2 || parts[1] == "" {
		return nil, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err == nil {
		return payload, true
	}
	payload, err = base64.URLEncoding.DecodeString(parts[1])
	if err == nil {
		return payload, true
	}
	return nil, false
}
