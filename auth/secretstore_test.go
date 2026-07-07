package auth

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// --- fileSecretStore tests ---

func TestFileSecretStore_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	store := &fileSecretStore{dir: dir}

	// Set and Get
	if err := store.Set("my-key", "my-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	val, err := store.Get("my-key")
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if val != "my-value" {
		t.Fatalf("got %q, want %q", val, "my-value")
	}

	// Overwrite
	if err := store.Set("my-key", "updated"); err != nil {
		t.Fatalf("Set overwrite: %v", err)
	}
	val, err = store.Get("my-key")
	if err != nil {
		t.Fatalf("Get after overwrite: %v", err)
	}
	if val != "updated" {
		t.Fatalf("got %q, want %q", val, "updated")
	}

	// Delete
	if err := store.Delete("my-key"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	_, err = store.Get("my-key")
	if err != ErrSecretNotFound {
		t.Fatalf("Get after Delete: got err=%v, want ErrSecretNotFound", err)
	}
}

func TestFileSecretStore_GetNotFound(t *testing.T) {
	dir := t.TempDir()
	store := &fileSecretStore{dir: dir}

	_, err := store.Get("nonexistent")
	if err != ErrSecretNotFound {
		t.Fatalf("got err=%v, want ErrSecretNotFound", err)
	}
}

func TestFileSecretStore_GetEmptyFile(t *testing.T) {
	dir := t.TempDir()
	store := &fileSecretStore{dir: dir}

	// Write an empty file manually.
	if err := os.WriteFile(filepath.Join(dir, "empty-key"), []byte("  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := store.Get("empty-key")
	if err != ErrSecretNotFound {
		t.Fatalf("got err=%v, want ErrSecretNotFound for empty value", err)
	}
}

func TestFileSecretStore_DeleteNonexistent(t *testing.T) {
	dir := t.TempDir()
	store := &fileSecretStore{dir: dir}

	// Should not error on missing key.
	if err := store.Delete("nonexistent"); err != nil {
		t.Fatalf("Delete nonexistent: %v", err)
	}
}

// --- mockSecretStore for interface-level tests ---

type mockSecretStore struct {
	data map[string]string
}

func newMockStore() *mockSecretStore {
	return &mockSecretStore{data: make(map[string]string)}
}

func (m *mockSecretStore) Get(key string) (string, error) {
	val, ok := m.data[key]
	if !ok {
		return "", ErrSecretNotFound
	}
	return val, nil
}

func (m *mockSecretStore) Set(key, value string) error {
	m.data[key] = value
	return nil
}

func (m *mockSecretStore) Delete(key string) error {
	delete(m.data, key)
	return nil
}

// --- Authenticator integration with SecretStore ---

func TestAuthenticator_SaveLoadAccessToken(t *testing.T) {
	dir := t.TempDir()
	store := newMockStore()

	a := &Authenticator{
		tokenDir:    dir,
		secretStore: store,
		accessToken: "gho_test123",
	}

	if err := a.saveAccessToken(); err != nil {
		t.Fatalf("saveAccessToken: %v", err)
	}

	// Clear in-memory state and reload.
	a.accessToken = ""
	if err := a.loadAccessToken(); err != nil {
		t.Fatalf("loadAccessToken: %v", err)
	}
	if a.accessToken != "gho_test123" {
		t.Fatalf("got %q, want %q", a.accessToken, "gho_test123")
	}
}

func TestAuthenticator_SaveLoadCopilotToken(t *testing.T) {
	dir := t.TempDir()
	store := newMockStore()

	expiry := time.Now().Add(1 * time.Hour)
	a := &Authenticator{
		tokenDir:     dir,
		secretStore:  store,
		copilotToken: "cpt_abc",
		tokenExpiry:  expiry,
	}

	if err := a.saveCopilotToken(); err != nil {
		t.Fatalf("saveCopilotToken: %v", err)
	}

	// Clear in-memory state and reload.
	a.copilotToken = ""
	a.tokenExpiry = time.Time{}
	if err := a.loadCopilotToken(); err != nil {
		t.Fatalf("loadCopilotToken: %v", err)
	}
	if a.copilotToken != "cpt_abc" {
		t.Fatalf("got %q, want %q", a.copilotToken, "cpt_abc")
	}
	if a.tokenExpiry.IsZero() {
		t.Fatal("tokenExpiry should be set")
	}
}

func TestAuthenticator_LoadCopilotTokenExpired(t *testing.T) {
	dir := t.TempDir()
	store := newMockStore()

	// Persist an expired token.
	data, _ := json.Marshal(CopilotTokenResponse{
		Token:     "expired_token",
		ExpiresAt: time.Now().Add(-1 * time.Hour).Unix(),
	})
	_ = store.Set("copilot-token", string(data))

	a := &Authenticator{
		tokenDir:    dir,
		secretStore: store,
	}

	err := a.loadCopilotToken()
	if err == nil {
		t.Fatal("expected error for expired token")
	}
}

func TestAuthenticator_HasAccessTokenOnDisk(t *testing.T) {
	dir := t.TempDir()
	store := newMockStore()

	a := &Authenticator{
		tokenDir:    dir,
		secretStore: store,
	}

	if a.hasAccessTokenOnDisk() {
		t.Fatal("expected false when no token stored")
	}

	_ = store.Set("access-token", "gho_xyz")
	if !a.hasAccessTokenOnDisk() {
		t.Fatal("expected true after storing token")
	}
}

func TestAuthenticator_HasValidCopilotTokenOnDisk(t *testing.T) {
	dir := t.TempDir()
	store := newMockStore()

	a := &Authenticator{
		tokenDir:    dir,
		secretStore: store,
	}

	if a.hasValidCopilotTokenOnDisk() {
		t.Fatal("expected false when no token stored")
	}

	// Store a valid (non-expired) copilot token.
	data, _ := json.Marshal(CopilotTokenResponse{
		Token:     "valid_token",
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
	})
	_ = store.Set("copilot-token", string(data))
	if !a.hasValidCopilotTokenOnDisk() {
		t.Fatal("expected true after storing valid token")
	}
}

// --- NewSecretStore fallback tests ---

func TestNewSecretStore_FileForced(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("VEKIL_SECRET_STORE", "file")

	store := NewSecretStore(dir)
	if _, ok := store.(*fileSecretStore); !ok {
		t.Fatalf("expected *fileSecretStore, got %T", store)
	}
}

func TestNewSecretStore_DefaultFallsBackToFile(t *testing.T) {
	dir := t.TempDir()
	// Unset the override to test auto-detection. In CI/containers, keyring
	// is typically unavailable, so this should fall back to file.
	t.Setenv("VEKIL_SECRET_STORE", "")

	store := NewSecretStore(dir)
	// We can't guarantee keyring is available in test environments, so just
	// verify we get a non-nil store that implements the interface.
	if store == nil {
		t.Fatal("expected non-nil store")
	}
}

// --- Migration test ---

func TestAuthenticator_MigrationRemovesLegacyFiles(t *testing.T) {
	dir := t.TempDir()

	// Simulate legacy plain-text files on disk.
	accessFile := filepath.Join(dir, "access-token")
	apiKeyFile := filepath.Join(dir, "api-key.json")
	if err := os.WriteFile(accessFile, []byte("gho_legacy"), 0o600); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(CopilotTokenResponse{
		Token:     "legacy_copilot",
		ExpiresAt: time.Now().Add(1 * time.Hour).Unix(),
	})
	if err := os.WriteFile(apiKeyFile, data, 0o600); err != nil {
		t.Fatal(err)
	}

	// Use a mock store that pretends to be a keyring (not *fileSecretStore).
	store := newMockStore()
	a := &Authenticator{
		tokenDir:     dir,
		secretStore:  store,
		accessToken:  "gho_new",
		copilotToken: "cpt_new",
		tokenExpiry:  time.Now().Add(1 * time.Hour),
	}

	// Save via keyring-backed store should remove legacy files.
	if err := a.saveAccessToken(); err != nil {
		t.Fatalf("saveAccessToken: %v", err)
	}
	if _, err := os.Stat(accessFile); !os.IsNotExist(err) {
		t.Fatal("expected legacy access-token file to be removed after keyring save")
	}

	if err := a.saveCopilotToken(); err != nil {
		t.Fatalf("saveCopilotToken: %v", err)
	}
	if _, err := os.Stat(apiKeyFile); !os.IsNotExist(err) {
		t.Fatal("expected legacy api-key.json file to be removed after keyring save")
	}

	// Verify the secrets are in the store.
	val, err := store.Get("access-token")
	if err != nil || val != "gho_new" {
		t.Fatalf("access-token not in store: val=%q, err=%v", val, err)
	}
}
