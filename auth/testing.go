package auth

import (
	"os"
	"time"
)

// NewTestAuthenticator creates an Authenticator pre-loaded with a token for testing.
// It uses a file-backed secret store in a temporary directory so tests do not
// require a system keyring.
func NewTestAuthenticator(token string) *Authenticator {
	dir, _ := os.MkdirTemp("", "vekil-test-*")
	return &Authenticator{
		copilotToken: token,
		tokenExpiry:  time.Now().Add(1 * time.Hour),
		tokenDir:     dir,
		secretStore:  &fileSecretStore{dir: dir},
	}
}
