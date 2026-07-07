package auth

import (
	"errors"

	"github.com/zalando/go-keyring"
)

const keyringService = "vekil"

// keyringSecretStore implements SecretStore using the OS keyring via
// github.com/zalando/go-keyring. On macOS it uses the Keychain, on Linux
// the Secret Service (D-Bus), and on Windows the Credential Manager.
type keyringSecretStore struct{}

// isAvailable probes whether the system keyring is usable by performing a
// write/delete cycle with a disposable test key.
func (s *keyringSecretStore) isAvailable() bool {
	const probeKey = "_vekil_keyring_probe"
	if err := keyring.Set(keyringService, probeKey, "probe"); err != nil {
		return false
	}
	_ = keyring.Delete(keyringService, probeKey)
	return true
}

func (s *keyringSecretStore) Get(key string) (string, error) {
	val, err := keyring.Get(keyringService, key)
	if err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return "", ErrSecretNotFound
		}
		return "", err
	}
	return val, nil
}

func (s *keyringSecretStore) Set(key, value string) error {
	return keyring.Set(keyringService, key, value)
}

func (s *keyringSecretStore) Delete(key string) error {
	err := keyring.Delete(keyringService, key)
	if err != nil && errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	return err
}
