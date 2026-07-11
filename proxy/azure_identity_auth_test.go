package proxy

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
)

type staticAzureTokenSource struct {
	token string
	err   error
	calls atomic.Int32
}

func (s *staticAzureTokenSource) AccessToken(context.Context) (string, error) {
	s.calls.Add(1)
	if s.err != nil {
		return "", s.err
	}
	return s.token, nil
}

func withAzureIdentityTokenSourceFactoryForTest(factory azureIdentityTokenSourceFactory) Option {
	return func(h *ProxyHandler) {
		h.azureIdentityTokenSourceFactory = factory
	}
}

type recordingAzureIdentityFactory struct {
	source azureTokenSource
	err    error

	calls      atomic.Int32
	providerID string
	scope      string
}

func (f *recordingAzureIdentityFactory) factory(providerID, scope string) (azureTokenSource, error) {
	f.calls.Add(1)
	f.providerID = providerID
	f.scope = scope
	if f.err != nil {
		return nil, f.err
	}
	return f.source, nil
}

type fakeAzureCredential struct {
	mu     sync.Mutex
	tokens []azcore.AccessToken
	err    error

	calls  int
	scopes [][]string
}

func (f *fakeAzureCredential) GetToken(_ context.Context, opts policy.TokenRequestOptions) (azcore.AccessToken, error) {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls++
	f.scopes = append(f.scopes, append([]string(nil), opts.Scopes...))
	if f.err != nil {
		return azcore.AccessToken{}, f.err
	}
	if len(f.tokens) == 0 {
		return azcore.AccessToken{}, nil
	}
	idx := f.calls - 1
	if idx >= len(f.tokens) {
		idx = len(f.tokens) - 1
	}
	return f.tokens[idx], nil
}

func TestAzureSDKTokenSourceCachesTokenUntilRefreshWindow(t *testing.T) {
	credential := &fakeAzureCredential{
		tokens: []azcore.AccessToken{{
			Token:     "cached-token",
			ExpiresOn: time.Now().Add(time.Hour),
		}},
	}
	source := newAzureSDKTokenSource(credential, "https://ai.azure.com/.default")

	first, err := source.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken() first error = %v", err)
	}
	second, err := source.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken() second error = %v", err)
	}
	if first != "cached-token" || second != "cached-token" {
		t.Fatalf("tokens = (%q, %q), want cached-token", first, second)
	}
	if credential.calls != 1 {
		t.Fatalf("credential GetToken calls = %d, want 1", credential.calls)
	}
	if !reflect.DeepEqual(credential.scopes[0], []string{"https://ai.azure.com/.default"}) {
		t.Fatalf("scopes = %v, want default scope", credential.scopes[0])
	}
}

func TestAzureSDKTokenSourceRefreshesNearExpiryToken(t *testing.T) {
	credential := &fakeAzureCredential{
		tokens: []azcore.AccessToken{
			{Token: "near-expiry", ExpiresOn: time.Now().Add(time.Minute)},
			{Token: "fresh-token", ExpiresOn: time.Now().Add(time.Hour)},
		},
	}
	source := newAzureSDKTokenSource(credential, "scope")

	first, err := source.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken() first error = %v", err)
	}
	second, err := source.AccessToken(context.Background())
	if err != nil {
		t.Fatalf("AccessToken() second error = %v", err)
	}
	if first != "near-expiry" || second != "fresh-token" {
		t.Fatalf("tokens = (%q, %q), want near-expiry then fresh-token", first, second)
	}
	if credential.calls != 2 {
		t.Fatalf("credential GetToken calls = %d, want 2", credential.calls)
	}
}

func TestAzureSDKTokenSourceErrorsOnEmptyToken(t *testing.T) {
	credential := &fakeAzureCredential{
		tokens: []azcore.AccessToken{{ExpiresOn: time.Now().Add(time.Hour)}},
	}
	source := newAzureSDKTokenSource(credential, "scope")

	_, err := source.AccessToken(context.Background())
	if err == nil {
		t.Fatal("AccessToken() error = nil, want empty token error")
	}
}

func TestAzureSDKTokenSourceReturnsCredentialError(t *testing.T) {
	credential := &fakeAzureCredential{err: errors.New("credential unavailable")}
	source := newAzureSDKTokenSource(credential, "scope")

	_, err := source.AccessToken(context.Background())
	if err == nil {
		t.Fatal("AccessToken() error = nil, want credential error")
	}
}

type blockingAzureCredential struct {
	started    chan struct{}
	release    chan struct{}
	canceled   chan struct{}
	once       sync.Once
	cancelOnce sync.Once
	calls      atomic.Int32
	token      azcore.AccessToken
	err        error
}

func (c *blockingAzureCredential) GetToken(ctx context.Context, _ policy.TokenRequestOptions) (azcore.AccessToken, error) {
	c.calls.Add(1)
	c.once.Do(func() { close(c.started) })
	select {
	case <-c.release:
		return c.token, c.err
	case <-ctx.Done():
		if c.canceled != nil {
			c.cancelOnce.Do(func() { close(c.canceled) })
		}
		return azcore.AccessToken{}, ctx.Err()
	}
}

func waitForAzureRefreshWaiters(t *testing.T, source *azureSDKTokenSource, want int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		source.mu.Lock()
		got := 0
		if source.refresh != nil {
			got = source.refresh.waiters
		}
		source.mu.Unlock()
		if got >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d Azure refresh waiters", want)
}

func TestAzureSDKTokenSourceRefreshWaiterHonorsContext(t *testing.T) {
	credential := &blockingAzureCredential{
		started: make(chan struct{}),
		release: make(chan struct{}),
		token: azcore.AccessToken{
			Token:     "shared-token",
			ExpiresOn: time.Now().Add(time.Hour),
		},
	}
	source := newAzureSDKTokenSource(credential, "scope")

	leaderDone := make(chan error, 1)
	go func() {
		_, err := source.AccessToken(context.Background())
		leaderDone <- err
	}()
	<-credential.started

	ctx, cancel := context.WithCancel(context.Background())
	waiterDone := make(chan error, 1)
	go func() {
		_, err := source.AccessToken(ctx)
		waiterDone <- err
	}()
	waitForAzureRefreshWaiters(t, source, 2)
	cancel()

	select {
	case err := <-waiterDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("waiter error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Azure refresh waiter did not honor its context")
	}
	if got := credential.calls.Load(); got != 1 {
		t.Fatalf("credential calls while leader blocked = %d, want 1", got)
	}

	close(credential.release)
	if err := <-leaderDone; err != nil {
		t.Fatalf("leader error = %v", err)
	}
}

func TestAzureSDKTokenSourceSharesFailedRefresh(t *testing.T) {
	credential := &blockingAzureCredential{
		started: make(chan struct{}),
		release: make(chan struct{}),
		err:     errors.New("credential unavailable"),
	}
	source := newAzureSDKTokenSource(credential, "scope")

	const callers = 8
	results := make(chan error, callers)
	go func() {
		_, err := source.AccessToken(context.Background())
		results <- err
	}()
	<-credential.started
	for range callers - 1 {
		go func() {
			_, err := source.AccessToken(context.Background())
			results <- err
		}()
	}
	waitForAzureRefreshWaiters(t, source, callers)
	close(credential.release)

	for range callers {
		err := <-results
		if err == nil || !strings.Contains(err.Error(), "credential unavailable") {
			t.Fatalf("shared refresh error = %v, want credential unavailable", err)
		}
	}
	if got := credential.calls.Load(); got != 1 {
		t.Fatalf("credential calls = %d, want 1 shared failed refresh", got)
	}
}

func TestAzureSDKTokenSourceLeaderCancellationDoesNotCancelLiveWaiter(t *testing.T) {
	credential := &blockingAzureCredential{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		canceled: make(chan struct{}),
		token: azcore.AccessToken{
			Token:     "waiter-token",
			ExpiresOn: time.Now().Add(time.Hour),
		},
	}
	source := newAzureSDKTokenSource(credential, "scope")

	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	leaderDone := make(chan error, 1)
	go func() {
		_, err := source.AccessToken(leaderCtx)
		leaderDone <- err
	}()
	<-credential.started

	waiterDone := make(chan struct {
		token string
		err   error
	}, 1)
	go func() {
		token, err := source.AccessToken(context.Background())
		waiterDone <- struct {
			token string
			err   error
		}{token: token, err: err}
	}()
	waitForAzureRefreshWaiters(t, source, 2)
	cancelLeader()

	if err := <-leaderDone; !errors.Is(err, context.Canceled) {
		t.Fatalf("leader error = %v, want context.Canceled", err)
	}
	source.mu.Lock()
	activeCall := source.refresh
	source.mu.Unlock()
	if activeCall == nil {
		t.Fatal("shared Azure refresh disappeared while a waiter remained")
	}
	select {
	case <-activeCall.ctx.Done():
		t.Fatal("shared Azure refresh context was canceled while a waiter remained")
	default:
	}
	select {
	case <-credential.canceled:
		t.Fatal("shared Azure refresh was canceled while a waiter remained")
	default:
	}

	close(credential.release)
	waiter := <-waiterDone
	if waiter.err != nil || waiter.token != "waiter-token" {
		t.Fatalf("waiter result = (%q, %v), want waiter-token", waiter.token, waiter.err)
	}
	if got := credential.calls.Load(); got != 1 {
		t.Fatalf("credential calls = %d, want 1", got)
	}
}

func TestAzureSDKTokenSourceCancelsRefreshWhenAllWaitersLeave(t *testing.T) {
	credential := &blockingAzureCredential{
		started:  make(chan struct{}),
		release:  make(chan struct{}),
		canceled: make(chan struct{}),
	}
	source := newAzureSDKTokenSource(credential, "scope")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := source.AccessToken(ctx)
		done <- err
	}()
	<-credential.started
	cancel()

	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("AccessToken() error = %v, want context.Canceled", err)
	}
	select {
	case <-credential.canceled:
	case <-time.After(time.Second):
		t.Fatal("underlying Azure refresh was not canceled after its last waiter left")
	}

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		source.mu.Lock()
		active := source.refresh != nil
		source.mu.Unlock()
		if !active {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("abandoned Azure refresh remained active")
}
