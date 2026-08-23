package auth

import "time"

// NewTestAuthenticator creates an Authenticator pre-loaded with a token for testing.
func NewTestAuthenticator(token string) *Authenticator {
	return &Authenticator{
		copilotToken: token,
		tokenExpiry:  time.Now().Add(1 * time.Hour),
	}
}

// NewTestAuthenticatorWithResponsesToken creates an Authenticator with a
// direct token plus a preloaded endpoint-compatible Responses token.
func NewTestAuthenticatorWithResponsesToken(token, responsesToken string) *Authenticator {
	return &Authenticator{
		copilotToken:         token,
		tokenExpiry:          time.Now().Add(1 * time.Hour),
		responsesSourceToken: token,
		responsesToken:       responsesToken,
		responsesTokenExpiry: time.Now().Add(1 * time.Hour),
	}
}
