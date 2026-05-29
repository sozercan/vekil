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

	mu     sync.Mutex
	cached azcore.AccessToken
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

	s.mu.Lock()
	defer s.mu.Unlock()

	if strings.TrimSpace(s.cached.Token) != "" && time.Until(s.cached.ExpiresOn) > azureIdentityTokenRefreshWindow {
		return s.cached.Token, nil
	}

	accessToken, err := s.credential.GetToken(ctx, policy.TokenRequestOptions{Scopes: []string{s.scope}})
	if err != nil {
		return "", fmt.Errorf("get Azure identity token for scope %q: %w", s.scope, err)
	}
	if strings.TrimSpace(accessToken.Token) == "" {
		return "", fmt.Errorf("get Azure identity token for scope %q: empty token", s.scope)
	}
	s.cached = accessToken
	return accessToken.Token, nil
}
