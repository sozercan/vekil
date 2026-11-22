package auth

import (
	"context"
	"crypto/sha256"
	"strings"
)

type Credential struct {
	Token             string
	SourceFingerprint [32]byte
}

func (a *Authenticator) GetCredential(ctx context.Context) (Credential, error) {
	return a.getToken(ctx, !a.DisableAutoDeviceFlow)
}

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

func (a *Authenticator) credentialLocked() Credential {
	fingerprint := a.copilotSourceFingerprint
	if bearer := strings.TrimSpace(a.copilotToken); fingerprint == ([32]byte{}) && bearer != "" {
		fingerprint = sha256.Sum256([]byte(bearer))
	}
	return Credential{Token: a.copilotToken, SourceFingerprint: fingerprint}
}
