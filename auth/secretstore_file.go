package auth

import (
	"os"
	"path/filepath"
	"strings"
)

// legacyFileNames maps secret store keys to the legacy filenames used before
// the SecretStore abstraction was introduced. When Get cannot find the
// canonical key file, it checks the legacy name for migration purposes.
var legacyFileNames = map[string]string{
	"copilot-token": "api-key.json",
}

// fileSecretStore implements SecretStore using plain files in a directory.
// Each key maps to a file named after the key. This is the fallback when the
// system keyring is unavailable.
type fileSecretStore struct {
	dir string
}

func (s *fileSecretStore) path(key string) string {
	return filepath.Join(s.dir, key)
}

func (s *fileSecretStore) Get(key string) (string, error) {
	data, err := os.ReadFile(s.path(key))
	if err != nil {
		if os.IsNotExist(err) {
			// Try legacy filename for backward compatibility.
			if legacy, ok := legacyFileNames[key]; ok {
				data, err = os.ReadFile(filepath.Join(s.dir, legacy))
				if err != nil {
					if os.IsNotExist(err) {
						return "", ErrSecretNotFound
					}
					return "", err
				}
				val := strings.TrimSpace(string(data))
				if val == "" {
					return "", ErrSecretNotFound
				}
				return val, nil
			}
			return "", ErrSecretNotFound
		}
		return "", err
	}
	val := strings.TrimSpace(string(data))
	if val == "" {
		return "", ErrSecretNotFound
	}
	return val, nil
}

func (s *fileSecretStore) Set(key, value string) error {
	return atomicWriteFile(s.path(key), []byte(value), 0o600)
}

func (s *fileSecretStore) Delete(key string) error {
	err := os.Remove(s.path(key))
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}
