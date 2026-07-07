package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
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

func (s keyringCredentialStore) key(name string) string {
	dir := s.fallback.dir
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	sum := sha256.Sum256([]byte(dir))
	return name + ":" + hex.EncodeToString(sum[:8])
}

func (s keyringCredentialStore) Get(name string) ([]byte, error) {
	fallback, fallbackErr := s.fallback.Get(name)
	if fallbackErr == nil && strings.TrimSpace(string(fallback)) != "" {
		if setErr := keyring.Set(credentialStoreService, s.key(name), string(fallback)); setErr == nil {
			_ = s.fallback.Delete(name)
		}
		return fallback, nil
	}

	secret, err := keyring.Get(credentialStoreService, s.key(name))
	if err == nil {
		return []byte(secret), nil
	}

	if errors.Is(err, keyring.ErrNotFound) {
		return nil, os.ErrNotExist
	}
	// If the OS keyring backend is unavailable, treat an absent fallback file as no
	// stored credential. This keeps headless/container startup on the old
	// unauthenticated path instead of failing before device/env auth can proceed.
	return nil, os.ErrNotExist
}

func (s keyringCredentialStore) Set(name string, data []byte) error {
	if err := keyring.Set(credentialStoreService, s.key(name), string(data)); err != nil {
		return s.fallback.Set(name, data)
	}
	return s.fallback.Delete(name)
}

func (s keyringCredentialStore) Delete(name string) error {
	keyringErr := keyring.Delete(credentialStoreService, s.key(name))
	fallbackErr := s.fallback.Delete(name)
	if keyringErr != nil && !errors.Is(keyringErr, keyring.ErrNotFound) {
		if fallbackErr != nil {
			return errors.Join(keyringErr, fallbackErr)
		}
		return keyringErr
	}
	return fallbackErr
}

func (s keyringCredentialStore) Exists(name string) bool {
	// Status checks must be side-effect-free: query the keyring directly without
	// legacy-file migration, and fall back to legacy file state when the keyring is
	// missing or unavailable.
	secret, err := keyring.Get(credentialStoreService, s.key(name))
	if err == nil && strings.TrimSpace(secret) != "" {
		return true
	}
	return s.fallback.Exists(name)
}
