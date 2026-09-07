package auth

import (
	"crypto/sha256"
	"strings"
)

// CredentialFingerprint identifies the source credential for an already-issued
// bearer without performing authentication or I/O. Unknown or rotated bearers
// keep their own identity so an in-flight request cannot inherit another login.
func (a *Authenticator) CredentialFingerprint(bearer string) [32]byte {
	bearer = strings.TrimSpace(bearer)
	if bearer == "" {
		return [32]byte{}
	}
	fingerprint := sha256.Sum256([]byte(bearer))
	if a != nil {
		a.mu.RLock()
		if bearer == a.copilotToken && a.copilotSourceFingerprint != ([32]byte{}) {
			fingerprint = a.copilotSourceFingerprint
		}
		a.mu.RUnlock()
		a.responsesMu.Lock()
		if bearer == a.responsesToken && a.responsesSourceToken != "" {
			fingerprint = sha256.Sum256([]byte(a.responsesSourceToken))
		}
		a.responsesMu.Unlock()
	}
	return fingerprint
}
