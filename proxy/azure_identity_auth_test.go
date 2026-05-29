package proxy

import (
	"context"
	"errors"
	"reflect"
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
