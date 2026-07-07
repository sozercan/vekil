package auth

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/zalando/go-keyring"
)

const (
	credentialStoreEnv     = "VEKIL_SECRET_STORE"
	credentialStoreFile    = "file"
	credentialStoreService = "vekil"
	accessTokenSecretName  = "github-access-token"
	copilotTokenSecretName = "copilot-token-cache"
)

type credentialStore interface {
	Get(name string) ([]byte, error)
	Set(name string, data []byte) error
	Delete(name string) error
	Exists(name string) bool
}

type fileCredentialStore struct{ dir string }

func newFileCredentialStore(dir string) credentialStore { return fileCredentialStore{dir: dir} }

func (s fileCredentialStore) path(name string) string {
	switch name {
	case accessTokenSecretName:
		name = "access-token"
	case copilotTokenSecretName:
		name = "api-key.json"
	}
	return filepath.Join(s.dir, name)
}

func (s fileCredentialStore) Get(name string) ([]byte, error) { return os.ReadFile(s.path(name)) }

func (s fileCredentialStore) Set(name string, data []byte) error {
	return atomicWriteFile(s.path(name), data, 0o600)
}

func (s fileCredentialStore) Delete(name string) error {
	if err := os.Remove(s.path(name)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func (s fileCredentialStore) Exists(name string) bool {
	data, err := s.Get(name)
	return err == nil && strings.TrimSpace(string(data)) != ""
}

type keyringCredentialStore struct{ fallback fileCredentialStore }

func newDefaultCredentialStore(tokenDir string) credentialStore {
	if strings.EqualFold(strings.TrimSpace(os.Getenv(credentialStoreEnv)), credentialStoreFile) {
		return newFileCredentialStore(tokenDir)
	}
	return keyringCredentialStore{fallback: fileCredentialStore{dir: tokenDir}}
}

func (s keyringCredentialStore) Get(name string) ([]byte, error) {
	secret, err := keyring.Get(credentialStoreService, name)
	if err == nil {
		return []byte(secret), nil
	}
	if !errors.Is(err, keyring.ErrNotFound) {
		return nil, err
	}
	legacy, legacyErr := s.fallback.Get(name)
	if legacyErr != nil {
		return nil, err
	}
	if strings.TrimSpace(string(legacy)) == "" {
		return nil, err
	}
	if setErr := s.Set(name, legacy); setErr != nil {
		return legacy, nil
	}
	_ = s.fallback.Delete(name)
	return legacy, nil
}

func (s keyringCredentialStore) Set(name string, data []byte) error {
	if err := keyring.Set(credentialStoreService, name, string(data)); err != nil {
		return fmt.Errorf("system secret store: %w", err)
	}
	_ = s.fallback.Delete(name)
	return nil
}

func (s keyringCredentialStore) Delete(name string) error {
	if err := keyring.Delete(credentialStoreService, name); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("system secret store: %w", err)
	}
	return s.fallback.Delete(name)
}

func (s keyringCredentialStore) Exists(name string) bool {
	data, err := s.Get(name)
	return err == nil && strings.TrimSpace(string(data)) != ""
}
