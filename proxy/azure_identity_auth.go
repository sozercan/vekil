package proxy

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
)

const (
	defaultAzureIdentityTokenScope  = "https://ai.azure.com/.default"
	azureIdentityTokenRefreshWindow = 5 * time.Minute
)

type azureTokenSource interface {
	AccessToken(context.Context) (string, error)
}

type azureIdentityTokenSourceFactory func(providerID, scope string) (azureTokenSource, error)

type azureSDKTokenSource struct {
	credential azcore.TokenCredential
	scope      string

	mu      sync.Mutex
	cached  azcore.AccessToken
	refresh *azureTokenRefresh
}

type azureTokenRefresh struct {
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	token     string
	err       error
	waiters   int
	completed bool
	abandoned bool
}

func newDefaultAzureIdentityTokenSource(providerID, scope string) (azureTokenSource, error) {
	credential, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("provider %q initialize Azure identity credential: %w", providerID, err)
	}
	return newAzureSDKTokenSource(credential, scope), nil
}

func newAzureSDKTokenSource(credential azcore.TokenCredential, scope string) *azureSDKTokenSource {
	return &azureSDKTokenSource{
		credential: credential,
		scope:      strings.TrimSpace(scope),
	}
}

func (s *azureSDKTokenSource) AccessToken(ctx context.Context) (string, error) {
	if s == nil || s.credential == nil {
		return "", fmt.Errorf("azure identity token source is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.mu.Lock()
	if strings.TrimSpace(s.cached.Token) != "" && time.Until(s.cached.ExpiresOn) > azureIdentityTokenRefreshWindow {
		token := s.cached.Token
		s.mu.Unlock()
		return token, nil
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return "", err
	}
	if refresh := s.refresh; refresh != nil && !refresh.abandoned {
		refresh.waiters++
		s.mu.Unlock()
		return s.waitForRefresh(ctx, refresh)
	}

	callCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	refresh := &azureTokenRefresh{
		ctx:     callCtx,
		cancel:  cancel,
		done:    make(chan struct{}),
		waiters: 1,
	}
	s.refresh = refresh
	s.mu.Unlock()

	go s.executeRefresh(refresh)
	return s.waitForRefresh(ctx, refresh)
}

func (s *azureSDKTokenSource) executeRefresh(refresh *azureTokenRefresh) {
	accessToken, err := s.credential.GetToken(refresh.ctx, policy.TokenRequestOptions{Scopes: []string{s.scope}})
	if err != nil {
		err = fmt.Errorf("get Azure identity token for scope %q: %w", s.scope, err)
	} else if strings.TrimSpace(accessToken.Token) == "" {
		err = fmt.Errorf("get Azure identity token for scope %q: empty token", s.scope)
	}

	s.mu.Lock()
	if refresh.abandoned {
		accessToken = azcore.AccessToken{}
		if err == nil {
			err = context.Canceled
		}
	} else if err == nil {
		s.cached = accessToken
		refresh.token = accessToken.Token
	}
	refresh.err = err
	refresh.completed = true
	if s.refresh == refresh {
		s.refresh = nil
	}
	close(refresh.done)
	refresh.cancel()
	s.mu.Unlock()
}

func (s *azureSDKTokenSource) waitForRefresh(ctx context.Context, refresh *azureTokenRefresh) (string, error) {
	completed := false
	select {
	case <-refresh.done:
		completed = true
	case <-ctx.Done():
	}

	s.mu.Lock()
	refresh.waiters--
	if !completed && refresh.waiters == 0 && !refresh.completed {
		refresh.abandoned = true
		if s.refresh == refresh {
			s.refresh = nil
		}
		refresh.cancel()
	}
	token, err := refresh.token, refresh.err
	s.mu.Unlock()

	if !completed {
		return "", ctx.Err()
	}
	return token, err
}
