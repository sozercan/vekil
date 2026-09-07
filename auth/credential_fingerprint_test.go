package auth

import (
	"crypto/sha256"
	"testing"
)

func TestCredentialFingerprintPreservesRequestIdentity(t *testing.T) {
	a := &Authenticator{accessToken: "source-one", copilotToken: "chat-token", responsesSourceToken: "source-one", responsesToken: "responses-token"}
	want := sha256.Sum256([]byte("source-one"))
	for _, bearer := range []string{"source-one", "chat-token", "responses-token"} {
		if got := a.CredentialFingerprint(bearer); got != want {
			t.Fatal("bearers for one credential have different fingerprints")
		}
	}
	a.responsesToken = "refreshed-token"
	if a.CredentialFingerprint("refreshed-token") != want {
		t.Fatal("service token refresh changed the source fingerprint")
	}
	a.accessToken, a.copilotToken = "source-two", "chat-two"
	a.responsesSourceToken, a.responsesToken = "source-two", "responses-two"
	if a.CredentialFingerprint("chat-token") == a.CredentialFingerprint("chat-two") {
		t.Fatal("old in-flight bearer inherited another login")
	}
	if a.CredentialFingerprint("") != ([32]byte{}) {
		t.Fatal("empty bearer has a fingerprint")
	}
}
