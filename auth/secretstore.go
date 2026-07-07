package auth

import (
	"fmt"
	"log"
	"os"
)

// SecretStore abstracts read/write/delete of secret values (tokens, keys).
// Implementations may use the OS keyring or fall back to plain files on disk.
type SecretStore interface {
	// Get retrieves a secret by key. Returns ("", ErrSecretNotFound) when the
	// key does not exist in the backing store.
	Get(key string) (string, error)

	// Set persists a secret under the given key, creating or overwriting it.
	Set(key, value string) error

	// Delete removes a secret. Returns nil if the key did not exist.
	Delete(key string) error
}

// ErrSecretNotFound is returned by SecretStore.Get when the requested key
// does not exist in the backing store.
var ErrSecretNotFound = fmt.Errorf("secret not found")

// NewSecretStore returns a SecretStore that tries the OS keyring first and
// falls back to file-based storage in tokenDir when the keyring is unavailable.
//
// The VEKIL_SECRET_STORE environment variable can force a specific backend:
//   - "file"    — always use file-based storage
//   - "keyring" — always use keyring (fail hard if unavailable)
//   - ""        — auto-detect (default)
func NewSecretStore(tokenDir string) SecretStore {
	override := os.Getenv("VEKIL_SECRET_STORE")

	switch override {
	case "file":
		return &fileSecretStore{dir: tokenDir}

	case "keyring":
		return &keyringSecretStore{}

	default:
		// Auto-detect: try the keyring, fall back to file.
		ks := &keyringSecretStore{}
		if ks.isAvailable() {
			return ks
		}
		log.Println("vekil: system keyring unavailable, using file-based secret storage")
		return &fileSecretStore{dir: tokenDir}
	}
}
