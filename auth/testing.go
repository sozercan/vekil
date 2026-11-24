package auth

import "time"

func NewTestAuthenticator(token string) *Authenticator {
	return &Authenticator{
		copilotToken: token,
		tokenExpiry:  time.Now().Add(1 * time.Hour),
	}
}

func NewTestAuthenticatorWithResponsesToken(token, responsesToken string) *Authenticator {
	return &Authenticator{
		copilotToken:         token,
		tokenExpiry:          time.Now().Add(1 * time.Hour),
		responsesSourceToken: token,
		responsesToken:       responsesToken,
		responsesTokenExpiry: time.Now().Add(1 * time.Hour),
	}
}
