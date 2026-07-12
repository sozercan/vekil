package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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
	openAICodexRefreshTimeout       = 30 * time.Second
	openAICodexCommitTimeout        = 30 * time.Second
	openAICodexRefreshInterval      = 8 * 24 * time.Hour
	openAICodexJournalSuffix        = ".vekil-cache"
	openAICodexJournalVersion       = 1
	openAICodexLockSuffix           = ".vekil-lock"
	openAICodexJournalTempPrefix    = ".vekil-journal-"
)

type openAICodexAuth struct {
	path                 string
	goos                 string
	sharedStateKey       string
	journalPathPinned    string
	lockPathPinned       string
	beforeJournalWrite   func() error
	beforePersistWrite   func() error
	beforeRefreshPublish func()
}

type openAICodexAuthSharedState struct {
	mu      sync.Mutex
	state   *openAICodexAuthState
	refresh *openAICodexRefreshCall
}

type openAICodexRefreshCall struct {
	ctx            context.Context
	cancel         context.CancelFunc
	done           chan struct{}
	credentials    openAICodexCredentials
	err            error
	waiters        int
	completed      bool
	abandoned      bool
	sourceDigest   string
	candidateState *openAICodexAuthState
}

type openAICodexAuthoritativeState struct {
	file   *os.File
	info   os.FileInfo
	body   []byte
	state  openAICodexAuthState
	digest string
}

type openAICodexJournal struct {
	Version           int    `json:"version"`
	SourceDigest      string `json:"source_sha256"`
	SourceAuthJSON    []byte `json:"source_auth_json"`
	RefreshedAuthJSON []byte `json:"refreshed_auth_json"`
}

var openAICodexAuthSharedStates sync.Map

type openAICodexTokenData struct {
	IDToken      string `json:"id_token,omitempty"`
	AccessToken  string `json:"access_token,omitempty"`
	RefreshToken string `json:"refresh_token,omitempty"`
	AccountID    string `json:"account_id,omitempty"`
}

type openAICodexAuthState struct {
	raw          map[string]json.RawMessage
	tokens       openAICodexTokenData
	lastRefresh  *time.Time
	sourceDigest string
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

func (a *openAICodexAuth) platform() string {
	if strings.TrimSpace(a.goos) != "" {
		return strings.ToLower(strings.TrimSpace(a.goos))
	}
	return runtime.GOOS
}

func openAICodexWindowsRefreshError() error {
	return fmt.Errorf("OpenAI Codex token refresh is disabled on Windows; run `codex login` to refresh credentials")
}

func closeOpenAICodexAuthoritative(authoritative *openAICodexAuthoritativeState) {
	if authoritative != nil && authoritative.file != nil {
		_ = authoritative.file.Close()
	}
}

func (a *openAICodexAuth) credentials(ctx context.Context, client *http.Client) (openAICodexCredentials, error) {
	if a == nil {
		return openAICodexCredentials{}, fmt.Errorf("OpenAI Codex auth is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	shared := a.sharedState()
	shared.mu.Lock()
	if err := ctx.Err(); err != nil {
		shared.mu.Unlock()
		return openAICodexCredentials{}, err
	}
	if refresh := shared.refresh; refresh != nil && !refresh.abandoned {
		if current, err := a.readAuthoritative(); err == nil {
			if current.digest != refresh.sourceDigest && !openAICodexNeedsRefresh(current.state.tokens.AccessToken, current.state.lastRefresh, time.Now().UTC()) {
				state := current.state
				state.sourceDigest = current.digest
				shared.state = openAICodexCloneStatePtr(&state)
				shared.mu.Unlock()
				return openAICodexCredentialsFromTokens(state.tokens), nil
			}
		}
		refresh.waiters++
		shared.mu.Unlock()
		return a.waitForRefresh(ctx, shared, refresh)
	}

	artifacts, artifactErr := a.hasJournalArtifacts()
	if artifactErr != nil {
		shared.mu.Unlock()
		return openAICodexCredentials{}, artifactErr
	}
	now := time.Now().UTC()
	state, authoritative, loadErr := a.loadState(shared, now)
	freshWithArtifacts := false
	if loadErr == nil && artifacts {
		freshWithArtifacts = a.completedJournalMatchesCurrent(authoritative.body) ||
			a.applicableJournalTargetMatchesState(authoritative.body, state) ||
			!a.hasApplicableJournalReadOnly(authoritative.body)
	}
	if loadErr == nil && !openAICodexNeedsRefresh(state.tokens.AccessToken, state.lastRefresh, now) && (!artifacts || freshWithArtifacts) {
		credentials := openAICodexCredentialsFromTokens(state.tokens)
		shared.mu.Unlock()
		closeOpenAICodexAuthoritative(authoritative)
		if artifacts {
			a.tryCleanupCompletedJournal(ctx)
		}
		return credentials, nil
	}
	closeOpenAICodexAuthoritative(authoritative)
	if a.platform() == "windows" {
		shared.mu.Unlock()
		if loadErr != nil {
			return openAICodexCredentials{}, loadErr
		}
		return openAICodexCredentials{}, openAICodexWindowsRefreshError()
	}
	if loadErr != nil && !artifacts {
		shared.mu.Unlock()
		return openAICodexCredentials{}, loadErr
	}

	callCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), openAICodexRefreshTimeout)
	refresh := &openAICodexRefreshCall{
		ctx:            callCtx,
		cancel:         cancel,
		done:           make(chan struct{}),
		waiters:        1,
		sourceDigest:   state.sourceDigest,
		candidateState: openAICodexCloneStatePtr(&state),
	}
	shared.refresh = refresh
	shared.mu.Unlock()

	go a.executeRefresh(shared, refresh, client)
	return a.waitForRefresh(ctx, shared, refresh)
}

func (a *openAICodexAuth) executeRefresh(
	shared *openAICodexAuthSharedState,
	refresh *openAICodexRefreshCall,
	client *http.Client,
) {
	credentials := openAICodexCredentials{}
	var cacheState *openAICodexAuthState
	clearCache := false

	transactionAuth := *a
	transactionAuth.path = openAICodexAuthPathKey(a.path)
	transactionAuth.journalPathPinned = transactionAuth.path + openAICodexJournalSuffix
	transactionAuth.lockPathPinned = transactionAuth.path + openAICodexLockSuffix
	refreshErr := error(nil)
	var processLock *openAICodexProcessLock
	processLock, refreshErr = acquireOpenAICodexProcessLock(refresh.ctx, transactionAuth.lockPath())
	var authInfo os.FileInfo
	if refreshErr == nil {
		authInfo, refreshErr = os.Stat(transactionAuth.path)
		if errors.Is(refreshErr, os.ErrNotExist) {
			refreshErr = nil
			authInfo = nil
		}
	}
	if refreshErr == nil {
		refreshErr = alignOpenAICodexFileOwner(processLock.file, authInfo)
	}
	if refreshErr == nil {
		credentials, cacheState, clearCache, refreshErr = transactionAuth.executeRefreshLocked(refresh.ctx, client, refresh.candidateState)
		if refreshErr == nil && openAICodexAuthPathKey(a.path) != transactionAuth.path {
			current, err := a.readAuthoritative()
			if err != nil {
				refreshErr = fmt.Errorf("configured OpenAI Codex auth path changed during refresh: %w", err)
			} else if openAICodexNeedsRefresh(current.state.tokens.AccessToken, current.state.lastRefresh, time.Now().UTC()) {
				refreshErr = fmt.Errorf("configured OpenAI Codex auth path changed during refresh and still requires refresh; retry")
			} else {
				state := current.state
				state.sourceDigest = current.digest
				credentials = openAICodexCredentialsFromTokens(state.tokens)
				cacheState = &state
			}
		}
	}
	if processLock != nil {
		// Releasing/closing the descriptor is post-commit cleanup. Never discard
		// already persisted or recovered credentials because cleanup reported an
		// error; closing the descriptor still releases the kernel lock.
		_ = processLock.release()
	}
	if a.beforeRefreshPublish != nil {
		a.beforeRefreshPublish()
	}
	if refreshErr == nil && cacheState != nil {
		if current, err := a.readAuthoritative(); err != nil {
			refreshErr = fmt.Errorf("revalidate OpenAI Codex auth after refresh: %w", err)
		} else if current.digest != cacheState.sourceDigest {
			state := current.state
			state.sourceDigest = current.digest
			if openAICodexNeedsRefresh(state.tokens.AccessToken, state.lastRefresh, time.Now().UTC()) {
				refreshErr = fmt.Errorf("OpenAI Codex auth changed after refresh and still requires refresh; retry")
			} else {
				credentials = openAICodexCredentialsFromTokens(state.tokens)
				cacheState = &state
			}
		}
	}

	shared.mu.Lock()
	if refresh.abandoned {
		credentials = openAICodexCredentials{}
		cacheState = nil
		if refreshErr == nil {
			refreshErr = context.Canceled
		}
	} else if clearCache {
		shared.state = nil
	} else if cacheState != nil {
		shared.state = openAICodexCloneStatePtr(cacheState)
	}
	refresh.credentials = credentials
	refresh.err = refreshErr
	refresh.completed = true
	if shared.refresh == refresh {
		shared.refresh = nil
	}
	close(refresh.done)
	refresh.cancel()
	shared.mu.Unlock()
}

func (a *openAICodexAuth) executeRefreshLocked(
	ctx context.Context,
	client *http.Client,
	candidateState *openAICodexAuthState,
) (openAICodexCredentials, *openAICodexAuthState, bool, error) {
	if err := a.recoverJournalLocked(); err != nil {
		return openAICodexCredentials{}, nil, false, err
	}

	readOnly, err := a.readAuthoritative()
	if err != nil {
		return openAICodexCredentials{}, nil, errors.Is(err, os.ErrNotExist), err
	}
	state := readOnly.state
	if candidateState != nil && candidateState.sourceDigest == readOnly.digest {
		state = openAICodexPreferState(readOnly.state, candidateState, time.Now().UTC())
		state.sourceDigest = readOnly.digest
	}
	now := time.Now().UTC()
	if !openAICodexNeedsRefresh(state.tokens.AccessToken, state.lastRefresh, now) {
		return openAICodexCredentialsFromTokens(state.tokens), &state, false, nil
	}
	if strings.TrimSpace(state.tokens.RefreshToken) == "" {
		return openAICodexCredentials{}, &state, false, fmt.Errorf("OpenAI Codex access token expired or stale and auth.json has no refresh_token; run `codex login`")
	}

	authoritative, err := a.openAuthoritativeWritableLocked()
	if err != nil {
		return openAICodexCredentials{}, nil, errors.Is(err, os.ErrNotExist), err
	}
	defer closeOpenAICodexAuthoritative(authoritative)
	state = authoritative.state
	if candidateState != nil && candidateState.sourceDigest == authoritative.digest {
		state = openAICodexPreferState(authoritative.state, candidateState, time.Now().UTC())
		state.sourceDigest = authoritative.digest
	}
	now = time.Now().UTC()
	if !openAICodexNeedsRefresh(state.tokens.AccessToken, state.lastRefresh, now) {
		return openAICodexCredentialsFromTokens(state.tokens), &state, false, nil
	}
	if strings.TrimSpace(state.tokens.RefreshToken) == "" {
		return openAICodexCredentials{}, &state, false, fmt.Errorf("OpenAI Codex access token expired or stale and auth.json has no refresh_token; run `codex login`")
	}
	if err := preflightOpenAICodexJournal(a.journalPath(), authoritative.info); err != nil {
		return openAICodexCredentials{}, &state, false, err
	}

	refreshed, refreshErr := requestOpenAICodexTokenRefresh(ctx, client, state.tokens.RefreshToken)
	if refreshErr != nil {
		if recovered, recoveredState, ok := a.recoverAuthoritativeAfterRefreshFailureLocked(state); ok {
			return recovered, recoveredState, false, nil
		}
		return openAICodexCredentials{}, nil, false, refreshErr
	}
	commitCtx, cancelCommit := context.WithTimeout(context.Background(), openAICodexCommitTimeout)
	defer cancelCommit()
	return a.applyRefresh(commitCtx, state, authoritative, refreshed)
}

func (a *openAICodexAuth) recoverAuthoritativeAfterRefreshFailureLocked(previous openAICodexAuthState) (openAICodexCredentials, *openAICodexAuthState, bool) {
	current, err := a.readCurrentStateWithoutJournalRecovery()
	if err != nil || current.sourceDigest == previous.sourceDigest {
		return openAICodexCredentials{}, nil, false
	}
	if openAICodexNeedsRefresh(current.tokens.AccessToken, current.lastRefresh, time.Now().UTC()) {
		return openAICodexCredentials{}, nil, false
	}
	return openAICodexCredentialsFromTokens(current.tokens), current, true
}

func (a *openAICodexAuth) waitForRefresh(
	ctx context.Context,
	shared *openAICodexAuthSharedState,
	refresh *openAICodexRefreshCall,
) (openAICodexCredentials, error) {
	completed := false
	select {
	case <-refresh.done:
		completed = true
	case <-ctx.Done():
	}

	shared.mu.Lock()
	refresh.waiters--
	credentials, err := refresh.credentials, refresh.err
	shared.mu.Unlock()

	if !completed {
		return openAICodexCredentials{}, ctx.Err()
	}
	return credentials, err
}

func (a *openAICodexAuth) applyRefresh(
	ctx context.Context,
	state openAICodexAuthState,
	authoritative *openAICodexAuthoritativeState,
	refreshed openAICodexRefreshResponse,
) (openAICodexCredentials, *openAICodexAuthState, bool, error) {
	if strings.TrimSpace(refreshed.AccessToken) != "" {
		state.tokens.AccessToken = refreshed.AccessToken
	}
	if strings.TrimSpace(refreshed.IDToken) != "" {
		state.tokens.IDToken = refreshed.IDToken
	}
	if strings.TrimSpace(refreshed.RefreshToken) != "" {
		state.tokens.RefreshToken = refreshed.RefreshToken
	}
	if strings.TrimSpace(state.tokens.AccessToken) == "" {
		return openAICodexCredentials{}, nil, false, fmt.Errorf("OpenAI Codex token refresh returned no access_token")
	}
	if authoritative == nil || authoritative.file == nil {
		return openAICodexCredentials{}, nil, false, fmt.Errorf("OpenAI Codex auth file %q is not open for a journaled refresh", a.path)
	}

	refreshedAt := time.Now().UTC()
	state = openAICodexStateWithTokens(state, state.tokens, refreshedAt)
	targetBody, err := marshalOpenAICodexAuthState(state)
	if err != nil {
		return openAICodexCredentials{}, nil, false, err
	}

	currentState, err := a.persistJournaledRefresh(ctx, authoritative, targetBody)
	if currentState != nil {
		if openAICodexNeedsRefresh(currentState.tokens.AccessToken, currentState.lastRefresh, time.Now().UTC()) {
			return openAICodexCredentials{}, currentState, false, fmt.Errorf("OpenAI Codex auth file %q changed during token refresh and still requires refresh; retry", a.path)
		}
		// The authoritative credentials are already fresh. Cleanup/directory-sync
		// errors must not make current requests fail; the remaining journal is retried
		// under the process lock on a later load.
		return openAICodexCredentialsFromTokens(currentState.tokens), currentState, false, nil
	}
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return openAICodexCredentials{}, nil, true, err
		}
		// The authority may already have rotated the refresh token. Keep the
		// successfully issued credentials in the process cache even when durable
		// journal persistence fails, so the running proxy does not immediately lose
		// the only usable token. A later request retries persistence/refresh.
		state.sourceDigest = authoritative.digest
		return openAICodexCredentialsFromTokens(state.tokens), &state, false, nil
	}

	state.sourceDigest = openAICodexSourceDigest(targetBody)
	return openAICodexCredentialsFromTokens(state.tokens), &state, false, nil
}

func (a *openAICodexAuth) loadState(
	shared *openAICodexAuthSharedState,
	now time.Time,
) (openAICodexAuthState, *openAICodexAuthoritativeState, error) {
	authoritative, err := a.readAuthoritative()
	if err != nil {
		if shared.state != nil {
			if currentBody, readErr := os.ReadFile(a.path); readErr == nil {
				if targetBody := a.applicableJournalTargetReadOnly(currentBody); len(targetBody) > 0 {
					if targetState, parseErr := a.parseState(targetBody); parseErr == nil &&
						targetState.tokens.AccessToken == shared.state.tokens.AccessToken {
						cached := openAICodexCloneState(*shared.state)
						cached.sourceDigest = openAICodexSourceDigest(targetBody)
						return cached, &openAICodexAuthoritativeState{
							body:   targetBody,
							state:  cached,
							digest: cached.sourceDigest,
						}, nil
					}
				}
			}
		}
		shared.state = nil
		return openAICodexAuthState{}, nil, err
	}

	var cachedState *openAICodexAuthState
	if shared.state != nil && shared.state.sourceDigest == authoritative.digest {
		cachedState = openAICodexCloneStatePtr(shared.state)
	} else {
		shared.state = nil
	}
	state := openAICodexPreferState(authoritative.state, cachedState, now)
	state.sourceDigest = authoritative.digest
	shared.state = openAICodexCloneStatePtr(&state)
	return state, authoritative, nil
}

func (a *openAICodexAuth) sharedState() *openAICodexAuthSharedState {
	key := strings.TrimSpace(a.sharedStateKey)
	if key == "" {
		key = openAICodexAuthPathKey(a.path)
	}
	if shared, ok := openAICodexAuthSharedStates.Load(key); ok {
		return shared.(*openAICodexAuthSharedState)
	}
	shared := &openAICodexAuthSharedState{}
	actual, _ := openAICodexAuthSharedStates.LoadOrStore(key, shared)
	return actual.(*openAICodexAuthSharedState)
}

func openAICodexAuthPathKey(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return ""
	}
	if abs, err := filepath.Abs(path); err == nil {
		path = abs
	}
	path = filepath.Clean(path)
	if resolved, err := filepath.EvalSymlinks(path); err == nil {
		path = resolved
	}
	return filepath.Clean(path)
}

func openAICodexPreferState(diskState openAICodexAuthState, cachedState *openAICodexAuthState, now time.Time) openAICodexAuthState {
	if cachedState == nil {
		return openAICodexCloneState(diskState)
	}

	diskNeedsRefresh := openAICodexNeedsRefresh(diskState.tokens.AccessToken, diskState.lastRefresh, now)
	cachedNeedsRefresh := openAICodexNeedsRefresh(cachedState.tokens.AccessToken, cachedState.lastRefresh, now)
	if diskNeedsRefresh && !cachedNeedsRefresh {
		return openAICodexCloneState(*cachedState)
	}
	if !diskNeedsRefresh && cachedNeedsRefresh {
		return openAICodexCloneState(diskState)
	}
	if !diskNeedsRefresh && !cachedNeedsRefresh {
		return openAICodexCloneState(diskState)
	}
	if openAICodexLastRefreshUnix(cachedState.lastRefresh) > openAICodexLastRefreshUnix(diskState.lastRefresh) {
		return openAICodexCloneState(*cachedState)
	}
	return openAICodexCloneState(diskState)
}

func openAICodexLastRefreshUnix(lastRefresh *time.Time) int64 {
	if lastRefresh == nil {
		return 0
	}
	return lastRefresh.UTC().UnixNano()
}

func openAICodexStateWithTokens(state openAICodexAuthState, tokens openAICodexTokenData, refreshedAt time.Time) openAICodexAuthState {
	updated := openAICodexCloneState(state)
	updated.tokens = tokens
	refreshedAt = refreshedAt.UTC()
	updated.lastRefresh = &refreshedAt
	if updated.raw == nil {
		updated.raw = map[string]json.RawMessage{}
	}
	if tokensRaw, err := json.Marshal(tokens); err == nil {
		updated.raw["tokens"] = tokensRaw
	}
	if lastRefreshRaw, err := json.Marshal(refreshedAt.Format(time.RFC3339)); err == nil {
		updated.raw["last_refresh"] = lastRefreshRaw
	}
	return updated
}

func openAICodexCloneStatePtr(state *openAICodexAuthState) *openAICodexAuthState {
	if state == nil {
		return nil
	}
	cloned := openAICodexCloneState(*state)
	return &cloned
}

func openAICodexCloneState(state openAICodexAuthState) openAICodexAuthState {
	cloned := openAICodexAuthState{
		raw:          openAICodexCloneRaw(state.raw),
		tokens:       state.tokens,
		sourceDigest: state.sourceDigest,
	}
	if state.lastRefresh != nil {
		refreshedAt := state.lastRefresh.UTC()
		cloned.lastRefresh = &refreshedAt
	}
	return cloned
}

func openAICodexCloneRaw(raw map[string]json.RawMessage) map[string]json.RawMessage {
	if raw == nil {
		return nil
	}
	cloned := make(map[string]json.RawMessage, len(raw))
	for key, value := range raw {
		if value == nil {
			cloned[key] = nil
			continue
		}
		copied := make(json.RawMessage, len(value))
		copy(copied, value)
		cloned[key] = copied
	}
	return cloned
}

func (a *openAICodexAuth) read() (openAICodexAuthState, error) {
	authoritative, err := a.readAuthoritative()
	if err != nil {
		return openAICodexAuthState{}, err
	}
	defer closeOpenAICodexAuthoritative(authoritative)
	return authoritative.state, nil
}

func (a *openAICodexAuth) readAuthoritative() (*openAICodexAuthoritativeState, error) {
	body, err := os.ReadFile(a.path)
	if err != nil {
		return nil, fmt.Errorf("read OpenAI Codex auth file %q: %w; run `codex login` first", a.path, err)
	}
	state, err := a.parseState(body)
	if err != nil {
		return nil, err
	}
	digest := openAICodexSourceDigest(body)
	state.sourceDigest = digest
	return &openAICodexAuthoritativeState{body: body, state: state, digest: digest}, nil
}

func (a *openAICodexAuth) openAuthoritativeWritableLocked() (*openAICodexAuthoritativeState, error) {
	file, err := openOpenAICodexWritableFile(a.path)
	if err != nil {
		return nil, fmt.Errorf("open OpenAI Codex auth file %q for refresh: %w", a.path, err)
	}
	body, err := readOpenAICodexFile(file)
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("read OpenAI Codex auth file %q for refresh: %w", a.path, err)
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("stat OpenAI Codex auth file %q for refresh: %w", a.path, err)
	}
	state, err := a.parseState(body)
	if err != nil {
		_ = file.Close()
		return nil, err
	}
	digest := openAICodexSourceDigest(body)
	state.sourceDigest = digest
	return &openAICodexAuthoritativeState{
		file:   file,
		info:   info,
		body:   body,
		state:  state,
		digest: digest,
	}, nil
}

func (a *openAICodexAuth) hasJournalArtifacts() (bool, error) {
	if a.platform() == "windows" {
		return false, nil
	}
	if _, err := os.Stat(a.journalPath()); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, fmt.Errorf("stat OpenAI Codex refresh journal %q: %w", a.journalPath(), err)
	}
	entries, err := os.ReadDir(filepath.Dir(a.journalPath()))
	if err != nil {
		return false, fmt.Errorf("read OpenAI Codex auth directory for orphan journals: %w", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), openAICodexJournalTempPrefix) {
			return true, nil
		}
	}
	return false, nil
}

func (a *openAICodexAuth) parseState(body []byte) (openAICodexAuthState, error) {
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

func marshalOpenAICodexAuthState(state openAICodexAuthState) ([]byte, error) {
	body, err := json.MarshalIndent(state.raw, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal refreshed OpenAI Codex auth file: %w", err)
	}
	return append(body, '\n'), nil
}

func openAICodexSourceDigest(body []byte) string {
	sum := sha256.Sum256(body)
	return fmt.Sprintf("%x", sum[:])
}

func (a *openAICodexAuth) journalPath() string {
	if strings.TrimSpace(a.journalPathPinned) != "" {
		return a.journalPathPinned
	}
	return openAICodexAuthPathKey(a.path) + openAICodexJournalSuffix
}

func (a *openAICodexAuth) lockPath() string {
	if strings.TrimSpace(a.lockPathPinned) != "" {
		return a.lockPathPinned
	}
	return openAICodexAuthPathKey(a.path) + openAICodexLockSuffix
}

func (a *openAICodexAuth) hasApplicableJournalReadOnly(currentBody []byte) bool {
	return len(a.applicableJournalTargetReadOnly(currentBody)) > 0
}

func (a *openAICodexAuth) applicableJournalTargetMatchesState(currentBody []byte, state openAICodexAuthState) bool {
	targetBody := a.applicableJournalTargetReadOnly(currentBody)
	if len(targetBody) == 0 {
		return false
	}
	target, err := a.parseState(targetBody)
	return err == nil && target.tokens.AccessToken == state.tokens.AccessToken && target.tokens.RefreshToken == state.tokens.RefreshToken
}

func (a *openAICodexAuth) applicableJournalTargetReadOnly(currentBody []byte) []byte {
	paths := []string{a.journalPath()}
	entries, err := os.ReadDir(filepath.Dir(a.journalPath()))
	if err == nil {
		for _, entry := range entries {
			if strings.HasPrefix(entry.Name(), openAICodexJournalTempPrefix) {
				paths = append(paths, filepath.Join(filepath.Dir(a.journalPath()), entry.Name()))
			}
		}
	}
	for _, path := range paths {
		candidate, err := a.readJournalCandidate(path)
		if err == nil && candidate != nil && openAICodexJournalApplies(candidate.journal, currentBody) {
			return append([]byte(nil), candidate.journal.RefreshedAuthJSON...)
		}
	}
	return nil
}

func (a *openAICodexAuth) completedJournalMatchesCurrent(currentBody []byte) bool {
	candidate, err := a.readJournalCandidate(a.journalPath())
	if err != nil || candidate == nil {
		return false
	}
	if !bytes.Equal(currentBody, candidate.journal.RefreshedAuthJSON) {
		return false
	}
	return true
}

func (a *openAICodexAuth) tryCleanupCompletedJournal(ctx context.Context) {
	if a.platform() == "windows" {
		return
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 100*time.Millisecond)
	defer cancel()
	transactionAuth := *a
	transactionAuth.path = openAICodexAuthPathKey(a.path)
	transactionAuth.journalPathPinned = transactionAuth.path + openAICodexJournalSuffix
	transactionAuth.lockPathPinned = transactionAuth.path + openAICodexLockSuffix
	lock, err := acquireOpenAICodexProcessLock(cleanupCtx, transactionAuth.lockPath())
	if err != nil {
		return
	}
	defer func() { _ = lock.release() }()
	if authInfo, err := os.Stat(transactionAuth.path); err != nil || alignOpenAICodexFileOwner(lock.file, authInfo) != nil {
		return
	}
	if err := transactionAuth.prepareJournalLocked(); err != nil {
		return
	}
	_ = transactionAuth.recoverJournalLocked()
}

func (a *openAICodexAuth) persistJournaledRefresh(
	ctx context.Context,
	authoritative *openAICodexAuthoritativeState,
	targetBody []byte,
) (*openAICodexAuthState, error) {
	if a.platform() == "windows" {
		return nil, openAICodexWindowsRefreshError()
	}
	if authoritative == nil || authoritative.file == nil {
		return nil, fmt.Errorf("OpenAI Codex auth file %q is not open for a journaled refresh", a.path)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := a.parseState(targetBody); err != nil {
		return nil, fmt.Errorf("validate refreshed OpenAI Codex auth file: %w", err)
	}

	journal := openAICodexJournal{
		Version:           openAICodexJournalVersion,
		SourceDigest:      authoritative.digest,
		SourceAuthJSON:    append([]byte(nil), authoritative.body...),
		RefreshedAuthJSON: append([]byte(nil), targetBody...),
	}
	if a.beforeJournalWrite != nil {
		if err := a.beforeJournalWrite(); err != nil {
			return nil, err
		}
	}
	if err := a.writeJournal(journal, authoritative.info); err != nil {
		return nil, err
	}
	if a.beforePersistWrite != nil {
		if err := a.beforePersistWrite(); err != nil {
			// The durable journal intentionally remains for recovery on the next load.
			return nil, err
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	currentBody, current, err := a.recheckAuthoritativeDescriptor(authoritative)
	if err != nil {
		return nil, err
	}
	if current != nil {
		if err := a.removeJournalDurable(); err != nil {
			return current, err
		}
		return current, nil
	}
	if openAICodexSourceDigest(currentBody) != authoritative.digest || !bytes.Equal(currentBody, authoritative.body) {
		state, parseErr := a.parseState(currentBody)
		if parseErr != nil {
			return nil, parseErr
		}
		state.sourceDigest = openAICodexSourceDigest(currentBody)
		if err := a.removeJournalDurable(); err != nil {
			return &state, err
		}
		return &state, nil
	}

	if err := authoritative.file.Chmod(0o600); err != nil {
		return nil, fmt.Errorf("chmod refreshed OpenAI Codex auth file %q: %w", a.path, err)
	}
	if err := writeOpenAICodexTransactionFile(authoritative.file, targetBody); err != nil {
		// Leave the journal in place; the next load recognizes and completes a
		// source, partial-target, or full-target transaction state.
		return nil, fmt.Errorf("write refreshed OpenAI Codex auth file %q: %w", a.path, err)
	}
	same, err := a.authoritativeDescriptorMatchesPath(authoritative)
	if err != nil {
		return nil, err
	}
	if !same {
		if err := a.removeJournalDurable(); err != nil {
			return nil, err
		}
		return a.readCurrentStateWithoutJournalRecovery()
	}

	targetState, err := a.parseState(targetBody)
	if err != nil {
		return nil, err
	}
	targetState.sourceDigest = openAICodexSourceDigest(targetBody)
	if err := a.removeJournalDurable(); err != nil {
		return &targetState, err
	}
	return nil, nil
}

func (a *openAICodexAuth) recheckAuthoritativeDescriptor(
	authoritative *openAICodexAuthoritativeState,
) ([]byte, *openAICodexAuthState, error) {
	same, err := a.authoritativeDescriptorMatchesPath(authoritative)
	if err != nil {
		return nil, nil, err
	}
	if !same {
		state, err := a.readCurrentStateWithoutJournalRecovery()
		return nil, state, err
	}
	body, err := readOpenAICodexFile(authoritative.file)
	if err != nil {
		return nil, nil, fmt.Errorf("reread OpenAI Codex auth file %q: %w", a.path, err)
	}
	return body, nil, nil
}

func (a *openAICodexAuth) authoritativeDescriptorMatchesPath(authoritative *openAICodexAuthoritativeState) (bool, error) {
	pathInfo, err := os.Stat(a.path)
	if err != nil {
		return false, fmt.Errorf("stat OpenAI Codex auth file %q: %w", a.path, err)
	}
	fileInfo, err := authoritative.file.Stat()
	if err != nil {
		return false, fmt.Errorf("stat open OpenAI Codex auth file %q: %w", a.path, err)
	}
	return os.SameFile(authoritative.info, pathInfo) && os.SameFile(fileInfo, pathInfo), nil
}

func (a *openAICodexAuth) readCurrentStateWithoutJournalRecovery() (*openAICodexAuthState, error) {
	body, err := os.ReadFile(a.path)
	if err != nil {
		return nil, fmt.Errorf("read OpenAI Codex auth file %q: %w; run `codex login` first", a.path, err)
	}
	state, err := a.parseState(body)
	if err != nil {
		return nil, err
	}
	state.sourceDigest = openAICodexSourceDigest(body)
	return &state, nil
}

func readOpenAICodexFile(file *os.File) ([]byte, error) {
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	return io.ReadAll(file)
}

func writeOpenAICodexTransactionFile(file *os.File, body []byte) error {
	if err := file.Truncate(0); err != nil {
		return err
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	for len(body) > 0 {
		written, err := file.Write(body)
		if err != nil {
			return err
		}
		if written == 0 {
			return io.ErrShortWrite
		}
		body = body[written:]
	}
	return file.Sync()
}

func (a *openAICodexAuth) writeJournal(journal openAICodexJournal, owner os.FileInfo) error {
	body, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal OpenAI Codex refresh journal: %w", err)
	}
	body = append(body, '\n')
	return atomicWriteOpenAICodexJournal(a.journalPath(), body, owner)
}

func atomicWriteOpenAICodexJournal(path string, body []byte, owner os.FileInfo) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, openAICodexJournalTempPrefix+"*")
	if err != nil {
		return fmt.Errorf("create temporary OpenAI Codex refresh journal: %w", err)
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()

	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("chmod temporary OpenAI Codex refresh journal: %w", err)
	}
	if err := alignOpenAICodexFileOwner(tmp, owner); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("align temporary OpenAI Codex refresh journal ownership: %w", err)
	}
	written, err := tmp.Write(body)
	if err != nil {
		_ = tmp.Close()
		return fmt.Errorf("write temporary OpenAI Codex refresh journal: %w", err)
	}
	if written != len(body) {
		_ = tmp.Close()
		return fmt.Errorf("write temporary OpenAI Codex refresh journal: %w", io.ErrShortWrite)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("sync temporary OpenAI Codex refresh journal: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close temporary OpenAI Codex refresh journal: %w", err)
	}
	if err := os.Rename(tmpPath, path); err != nil {
		return fmt.Errorf("replace OpenAI Codex refresh journal %q: %w", path, err)
	}
	removeTemp = false
	return syncOpenAICodexDirectory(dir)
}

func preflightOpenAICodexJournal(journalPath string, owner os.FileInfo) error {
	probePath := journalPath + ".probe"
	if err := atomicWriteOpenAICodexJournal(probePath, []byte("{}\n"), owner); err != nil {
		return fmt.Errorf("preflight OpenAI Codex refresh journal: %w", err)
	}
	return removeOpenAICodexJournalPathsDurable(filepath.Dir(probePath), []string{probePath})
}

type openAICodexJournalCandidate struct {
	path    string
	journal openAICodexJournal
	modTime time.Time
}

func (a *openAICodexAuth) prepareJournalLocked() error {
	dir := filepath.Dir(a.journalPath())
	entries, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("read OpenAI Codex auth directory for journal recovery: %w", err)
	}
	paths := make([]string, 0, len(entries)+1)
	if _, err := os.Lstat(a.journalPath()); err == nil {
		paths = append(paths, a.journalPath())
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat OpenAI Codex refresh journal %q: %w", a.journalPath(), err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), openAICodexJournalTempPrefix) {
			paths = append(paths, filepath.Join(dir, entry.Name()))
		}
	}
	if len(paths) == 0 {
		return nil
	}

	currentBody, readErr := os.ReadFile(a.path)
	if readErr != nil && !errors.Is(readErr, os.ErrNotExist) {
		return fmt.Errorf("read OpenAI Codex auth file %q for journal selection: %w", a.path, readErr)
	}
	var chosen *openAICodexJournalCandidate
	removePaths := make([]string, 0, len(paths))
	for _, path := range paths {
		candidate, err := a.readJournalCandidate(path)
		if err != nil {
			return err
		}
		if candidate == nil || readErr != nil || !openAICodexJournalApplies(candidate.journal, currentBody) {
			removePaths = append(removePaths, path)
			continue
		}
		if chosen == nil || candidate.modTime.After(chosen.modTime) {
			if chosen != nil {
				removePaths = append(removePaths, chosen.path)
			}
			chosen = candidate
		} else {
			removePaths = append(removePaths, path)
		}
	}

	if chosen == nil {
		return removeOpenAICodexJournalPathsDurable(dir, removePaths)
	}
	filtered := removePaths[:0]
	for _, path := range removePaths {
		if path != chosen.path {
			filtered = append(filtered, path)
		}
	}
	if chosen.path != a.journalPath() {
		if _, err := os.Lstat(a.journalPath()); err == nil {
			filtered = append(filtered, a.journalPath())
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("stat existing OpenAI Codex refresh journal %q: %w", a.journalPath(), err)
		}
	}
	if err := removeOpenAICodexJournalPathsDurable(dir, filtered); err != nil {
		return err
	}
	if chosen.path == a.journalPath() {
		return nil
	}
	if err := os.Rename(chosen.path, a.journalPath()); err != nil {
		return fmt.Errorf("adopt orphan OpenAI Codex refresh journal %q: %w", chosen.path, err)
	}
	return syncOpenAICodexDirectory(dir)
}

func (a *openAICodexAuth) readJournalCandidate(path string) (*openAICodexJournalCandidate, error) {
	info, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, fmt.Errorf("stat OpenAI Codex journal candidate %q: %w", path, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		return nil, nil
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read OpenAI Codex journal candidate %q: %w", path, err)
	}
	var journal openAICodexJournal
	if err := json.Unmarshal(body, &journal); err != nil ||
		journal.Version != openAICodexJournalVersion ||
		journal.SourceDigest != openAICodexSourceDigest(journal.SourceAuthJSON) {
		return nil, nil
	}
	if _, err := a.parseState(journal.SourceAuthJSON); err != nil {
		return nil, nil
	}
	if _, err := a.parseState(journal.RefreshedAuthJSON); err != nil {
		return nil, nil
	}
	return &openAICodexJournalCandidate{path: path, journal: journal, modTime: info.ModTime()}, nil
}

func openAICodexJournalApplies(journal openAICodexJournal, currentBody []byte) bool {
	if journal.SourceDigest == openAICodexSourceDigest(currentBody) && bytes.Equal(currentBody, journal.SourceAuthJSON) {
		return true
	}
	if openAICodexSourceDigest(currentBody) == openAICodexSourceDigest(journal.RefreshedAuthJSON) && bytes.Equal(currentBody, journal.RefreshedAuthJSON) {
		return true
	}
	return len(currentBody) < len(journal.RefreshedAuthJSON) && bytes.Equal(currentBody, journal.RefreshedAuthJSON[:len(currentBody)])
}

func removeOpenAICodexJournalPathsDurable(dir string, paths []string) error {
	if len(paths) == 0 {
		return nil
	}
	var errs []error
	removed := false
	seen := make(map[string]struct{}, len(paths))
	for _, path := range paths {
		if path == "" {
			continue
		}
		if _, ok := seen[path]; ok {
			continue
		}
		seen[path] = struct{}{}
		if err := os.Remove(path); err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				errs = append(errs, fmt.Errorf("remove OpenAI Codex journal artifact %q: %w", path, err))
			}
		} else {
			removed = true
		}
	}
	if removed {
		if err := syncOpenAICodexDirectory(dir); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (a *openAICodexAuth) recoverJournalLocked() error {
	if err := a.prepareJournalLocked(); err != nil {
		return err
	}
	journal, err := a.readJournal()
	if err != nil {
		return err
	}
	if journal == nil {
		return nil
	}

	if journal.Version != openAICodexJournalVersion ||
		journal.SourceDigest != openAICodexSourceDigest(journal.SourceAuthJSON) {
		return a.removeJournalDurable()
	}
	if _, err := a.parseState(journal.SourceAuthJSON); err != nil {
		return a.removeJournalDurable()
	}
	if _, err := a.parseState(journal.RefreshedAuthJSON); err != nil {
		return a.removeJournalDurable()
	}

	file, err := openOpenAICodexWritableFile(a.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return a.removeJournalDurable()
		}
		return fmt.Errorf("open OpenAI Codex auth file %q for journal recovery: %w", a.path, err)
	}
	defer func() { _ = file.Close() }()
	body, err := readOpenAICodexFile(file)
	if err != nil {
		return fmt.Errorf("read OpenAI Codex auth file %q during journal recovery: %w", a.path, err)
	}
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat OpenAI Codex auth file %q during journal recovery: %w", a.path, err)
	}
	targetDigest := openAICodexSourceDigest(journal.RefreshedAuthJSON)
	currentDigest := openAICodexSourceDigest(body)

	switch {
	case currentDigest == targetDigest && bytes.Equal(body, journal.RefreshedAuthJSON):
		if err := file.Chmod(0o600); err != nil {
			return fmt.Errorf("chmod completed OpenAI Codex auth transaction %q: %w", a.path, err)
		}
		if err := file.Sync(); err != nil {
			return fmt.Errorf("sync completed OpenAI Codex auth transaction %q: %w", a.path, err)
		}
		return a.removeJournalDurable()
	case currentDigest == journal.SourceDigest && bytes.Equal(body, journal.SourceAuthJSON):
		// Complete the durable transaction below.
	case len(body) < len(journal.RefreshedAuthJSON) && bytes.Equal(body, journal.RefreshedAuthJSON[:len(body)]):
		// Truncate-first writes leave only a prefix of the journal target.
	default:
		// The path contains unrelated external state. It always wins.
		return a.removeJournalDurable()
	}

	pathInfo, err := os.Stat(a.path)
	if err != nil {
		return fmt.Errorf("stat OpenAI Codex auth file %q during journal recovery: %w", a.path, err)
	}
	if !os.SameFile(info, pathInfo) {
		return a.removeJournalDurable()
	}
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("chmod recovered OpenAI Codex auth file %q: %w", a.path, err)
	}
	if err := writeOpenAICodexTransactionFile(file, journal.RefreshedAuthJSON); err != nil {
		return fmt.Errorf("recover OpenAI Codex auth file %q from journal: %w", a.path, err)
	}
	pathInfo, err = os.Stat(a.path)
	if err != nil {
		return fmt.Errorf("stat recovered OpenAI Codex auth file %q: %w", a.path, err)
	}
	fileInfo, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat open recovered OpenAI Codex auth file %q: %w", a.path, err)
	}
	if !os.SameFile(info, pathInfo) || !os.SameFile(fileInfo, pathInfo) {
		return a.removeJournalDurable()
	}
	return a.removeJournalDurable()
}

func (a *openAICodexAuth) readJournal() (*openAICodexJournal, error) {
	path := a.journalPath()
	candidate, err := a.readJournalCandidate(path)
	if err != nil {
		return nil, err
	}
	if candidate == nil {
		if removeErr := a.removeJournalDurable(); removeErr != nil {
			return nil, removeErr
		}
		return nil, nil
	}
	return &candidate.journal, nil
}

func (a *openAICodexAuth) removeJournalDurable() error {
	path := a.journalPath()
	if err := os.Remove(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("remove OpenAI Codex refresh journal %q: %w", path, err)
	}
	return syncOpenAICodexDirectory(filepath.Dir(path))
}

func syncOpenAICodexDirectory(dir string) error {
	dirFile, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open OpenAI Codex auth directory %q for sync: %w", dir, err)
	}
	if err := dirFile.Sync(); err != nil {
		_ = dirFile.Close()
		return fmt.Errorf("sync OpenAI Codex auth directory %q: %w", dir, err)
	}
	if err := dirFile.Close(); err != nil {
		return fmt.Errorf("close OpenAI Codex auth directory %q after sync: %w", dir, err)
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
