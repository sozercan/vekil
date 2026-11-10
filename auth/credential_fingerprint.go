package auth

import (
	"context"
	"crypto/sha256"
	"strings"
)

// Credential pairs a bearer with its issuing credential's identity. A refresh
// cannot change the source fingerprint of a credential already returned.
type Credential struct {
	Token             string
	SourceFingerprint [32]byte
}

// GetCredential returns a usable bearer and its source identity together.
func (a *Authenticator) GetCredential(ctx context.Context) (Credential, error) {
	return a.getToken(ctx, !a.DisableAutoDeviceFlow)
}

// GetResponsesCredential returns a Responses-compatible bearer with the source
// identity captured before any endpoint-specific exchange.
func (a *Authenticator) GetResponsesCredential(ctx context.Context) (Credential, error) {
	credential, err := a.GetCredential(ctx)
	if err != nil {
		return Credential{}, err
	}
	if strings.HasPrefix(strings.TrimSpace(credential.Token), "ghu_") {
		credential.Token, err = a.runSharedResponsesToken(ctx, credential.Token)
		if err != nil {
			return Credential{}, err
		}
	}
	return credential, nil
}

// credentialLocked snapshots the bearer and its provenance while mu is held.
func (a *Authenticator) credentialLocked() Credential {
	fingerprint := a.copilotSourceFingerprint
	if bearer := strings.TrimSpace(a.copilotToken); fingerprint == ([32]byte{}) && bearer != "" {
		fingerprint = sha256.Sum256([]byte(bearer))
	}
	return Credential{Token: a.copilotToken, SourceFingerprint: fingerprint}
}
