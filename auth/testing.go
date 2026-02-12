package auth

import "time"

// NewTestAuthenticator creates an Authenticator pre-loaded with a token for testing.
func NewTestAuthenticator(token string) *Authenticator {
	return &Authenticator{
		copilotToken: token,
		tokenExpiry:  time.Now().Add(1 * time.Hour),
	}
}
