package macosruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/sozercan/vekil/internal/appcontrol"
	"github.com/sozercan/vekil/proxy"
)

func deterministicUUIDs(values ...string) func() string {
	index := 0
	return func() string {
		if index >= len(values) {
			return "uuid-extra"
		}
		value := values[index]
		index++
		return value
	}
}

func newManagerForTest(t *testing.T, ids ...string) *ConfigManager {
	t.Helper()
	manager, err := NewConfigManager(ConfigManagerOptions{
		Paths: PathsInDirectory(t.TempDir()),
		UUID:  deterministicUUIDs(ids...),
	})
	if err != nil {
		t.Fatalf("NewConfigManager() error = %v", err)
	}
	return manager
}

func TestConfigManagerMigratesLegacyMenubarState(t *testing.T) {
	dir := t.TempDir()
	paths := PathsInDirectory(dir)
	external := filepath.Join(dir, "external.yaml")
	externalBody := []byte("schema_version: 2\nproviders:\n  - id: local\n    type: openai-compatible\n    default: true\n    base_url: https://example.test/v1\n    auth_type: none\n    model_discovery: static\n    models:\n      - public_id: local-model\n        endpoints: [/chat/completions]\n")
	if err := os.WriteFile(external, externalBody, 0o600); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`{"providers_config_path":"` + external + `"}`)
	if err := os.WriteFile(paths.State, legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	manager, err := NewConfigManager(ConfigManagerOptions{Paths: paths, UUID: func() string { return "uuid" }})
	if err != nil {
		t.Fatal(err)
	}
	state := manager.State()
	wantRevision, _ := configRevision(externalBody)
	if state.Version != StateVersion || state.ConfigMode != ConfigModeExternal || state.SelectedPath != external || state.SelectedConfigRevision != wantRevision {
		t.Fatalf("migrated state = %+v", state)
	}
	body, err := os.ReadFile(paths.State)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), `"version": 1`) || !strings.Contains(string(body), `"providers_config_path"`) {
		t.Fatalf("rewritten state = %s", body)
	}
}

func TestConfigManagerMigratesUnavailableLegacyExternalWithoutLegacyRevision(t *testing.T) {
	dir := t.TempDir()
	paths := PathsInDirectory(dir)
	external := filepath.Join(dir, "missing.yaml")
	legacy := []byte(`{"providers_config_path":"` + external + `"}`)
	if err := os.WriteFile(paths.State, legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	manager, err := NewConfigManager(ConfigManagerOptions{Paths: paths, UUID: func() string { return "uuid" }})
	if err != nil {
		t.Fatal(err)
	}
	state := manager.State()
	if state.ConfigMode != ConfigModeExternal || state.SelectedPath != external || state.SelectedConfigRevision != "" {
		t.Fatalf("migrated state = %+v", state)
	}
}

func TestConfigManagerMigratesLegacyRemoteExternalWithoutExposingSignedURL(t *testing.T) {
	externalBody := []byte("schema_version: 2\nproviders:\n  - id: remote\n    type: openai-compatible\n    default: true\n    base_url: https://example.test/v1\n    auth_type: none\n    model_discovery: static\n    models:\n      - public_id: remote-model\n        endpoints: [/chat/completions]\n")
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write(externalBody)
	}))
	defer server.Close()

	dir := t.TempDir()
	paths := PathsInDirectory(dir)
	source := strings.Replace(server.URL, "http://", "http://signed-user:signed-password@", 1) +
		"/providers.yaml?signature=signed-query#private-fragment"
	legacy, err := json.Marshal(map[string]string{"providers_config_path": source})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.State, legacy, 0o600); err != nil {
		t.Fatal(err)
	}

	manager, err := NewConfigManager(ConfigManagerOptions{Paths: paths, UUID: func() string { return "uuid" }})
	if err != nil {
		t.Fatal(err)
	}
	wantRevision, _ := configRevision(externalBody)
	state := manager.State()
	if state.ConfigMode != ConfigModeExternal || state.SelectedPath != source || state.SelectedConfigRevision != wantRevision {
		t.Fatalf("migrated remote state = %+v", state)
	}

	description, err := manager.Describe()
	if err != nil {
		t.Fatal(err)
	}
	wantDisplay := server.URL + "/providers.yaml"
	if description.SelectedPath != wantDisplay || !description.Available || description.ErrorCode != "" {
		t.Fatalf("remote description = %+v, want display path %q", description, wantDisplay)
	}
	descriptionJSON, err := json.Marshal(description)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{"signed-user", "signed-password", "signed-query", "private-fragment"} {
		if bytes.Contains(descriptionJSON, []byte(secret)) {
			t.Fatalf("protocol description exposes %q: %s", secret, descriptionJSON)
		}
	}

	configuration, err := manager.LoadConfiguration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	loaded, ok := configuration.Value.(proxy.ProvidersConfig)
	if !ok || len(loaded.Providers) != 1 || loaded.Providers[0].ID != "remote" {
		t.Fatalf("loaded remote configuration = %#v", configuration.Value)
	}
}

func TestStageRemoteExternalHonorsCancellation(t *testing.T) {
	started := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
	}))
	defer server.Close()

	manager := newManagerForTest(t)
	ctx, cancel := context.WithCancel(t.Context())
	result := make(chan error, 1)
	go func() {
		result <- manager.StageExternal(ctx, server.URL+"/providers.yaml")
	}()
	<-started
	cancel()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("StageExternal() error = %v, want context cancellation", err)
	}
	if state := manager.State(); state.ConfigMode != ConfigModeLegacy || state.SelectedPath != "" {
		t.Fatalf("canceled remote selection changed state: %+v", state)
	}
}

func TestNewConfigManagerClearsStaleRuntimeActivation(t *testing.T) {
	manager := newManagerForTest(t, "owner", "copilot-uuid")
	description, err := manager.EnsureManagedConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.RuntimeActivated(
		t.Context(),
		appcontrol.Configuration{Revision: description.SelectedRevision},
		7,
		"127.0.0.1:1337",
	); err != nil {
		t.Fatal(err)
	}

	restarted, err := NewConfigManager(ConfigManagerOptions{
		Paths: manager.paths,
		UUID:  func() string { return "unused" },
	})
	if err != nil {
		t.Fatal(err)
	}
	state := restarted.State()
	if state.ActiveRuntimeRevision != "" || state.ActiveRuntimeGeneration != 0 {
		t.Fatalf("stale activation survived restart: %+v", state)
	}
	restartedDescription, err := restarted.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if restartedDescription.ActiveRevision != "" || restartedDescription.Drifted {
		t.Fatalf("restart description retained stale activation: %+v", restartedDescription)
	}
	persisted, _, err := loadPersistentState(manager.paths.State)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ActiveRuntimeRevision != "" || persisted.ActiveRuntimeGeneration != 0 {
		t.Fatalf("stale activation remained on disk: %+v", persisted)
	}
}

func TestRuntimeActivationPersistsAcceptedExternalRevision(t *testing.T) {
	manager := newManagerForTest(t)
	path, originalRevision := writeExternalConfigForSwitchTest(t, "original-model")
	if _, err := manager.SelectExternal(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	updated := bytes.Replace(body, []byte("original-model"), []byte("updated-model"), 1)
	if err := os.WriteFile(path, updated, 0o600); err != nil {
		t.Fatal(err)
	}

	configuration, err := manager.LoadConfiguration(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if configuration.Revision == originalRevision {
		t.Fatal("external edit did not produce a new revision")
	}
	if err := manager.RuntimeActivated(t.Context(), configuration, 1, "127.0.0.1:1337"); err != nil {
		t.Fatal(err)
	}

	state := manager.State()
	if state.SelectedConfigRevision != configuration.Revision || state.ActiveRuntimeRevision != configuration.Revision {
		t.Fatalf("activated external revision was not persisted: %+v", state)
	}
	persisted, _, err := loadPersistentState(manager.paths.State)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.SelectedConfigRevision != configuration.Revision {
		t.Fatalf("persisted selected revision = %q, want %q", persisted.SelectedConfigRevision, configuration.Revision)
	}
}

func TestEnsureManagedConfigurationUsesExclusiveOwnedPrivateFile(t *testing.T) {
	manager := newManagerForTest(t, "owner-id", "provider-uuid")
	description, err := manager.EnsureManagedConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := os.ReadFile(filepath.Join("testdata", "managed-copilot.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(manager.paths.Managed)
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != string(fixture) {
		t.Fatalf("managed YAML = %q, want fixture %q", body, fixture)
	}
	if description.Mode != ConfigModeManaged || !description.ManagedOwnershipPresent || len(description.Providers) != 1 {
		t.Fatalf("description = %+v", description)
	}
	fileInfo, err := os.Stat(manager.paths.Managed)
	if err != nil {
		t.Fatal(err)
	}
	if got := fileInfo.Mode().Perm(); got != 0o600 {
		t.Fatalf("file mode = %o", got)
	}
	dirInfo, err := os.Stat(manager.paths.Directory)
	if err != nil {
		t.Fatal(err)
	}
	if got := dirInfo.Mode().Perm(); got != 0o700 {
		t.Fatalf("directory mode = %o", got)
	}

	unowned := newManagerForTest(t, "different-owner", "different-provider")
	if err := os.WriteFile(unowned.paths.Managed, []byte("do not overwrite"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := unowned.EnsureManagedConfiguration(); err == nil {
		t.Fatal("EnsureManagedConfiguration() adopted an unowned file")
	}
	preserved, _ := os.ReadFile(unowned.paths.Managed)
	if string(preserved) != "do not overwrite" {
		t.Fatalf("unowned file changed: %q", preserved)
	}
}

func TestNewConfigManagerRecoversCommittedInitialManagedCreation(t *testing.T) {
	manager := newManagerForTest(t)
	journal := initialManagedCreationJournal()
	if err := writeApplyJournal(manager.paths.Journal, journal); err != nil {
		t.Fatal(err)
	}
	journalBody, err := os.ReadFile(manager.paths.Journal)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(journalBody, initialManagedCopilotYAML) || bytes.Contains(journalBody, []byte("schema_version")) {
		t.Fatalf("initial creation journal contains raw configuration: %s", journalBody)
	}
	if err := writeExclusiveFile(manager.paths.Managed, initialManagedCopilotYAML); err != nil {
		t.Fatal(err)
	}

	recovered, err := NewConfigManager(ConfigManagerOptions{
		Paths: manager.paths,
		UUID:  deterministicUUIDs("recovered-owner", "recovered-provider"),
	})
	if err != nil {
		t.Fatal(err)
	}
	state := recovered.State()
	if state.ConfigMode != ConfigModeManaged || state.ManagedOwnershipID != "recovered-owner" ||
		state.CommittedConfigRevision != journal.NewRevision || state.CommittedSHA256 != journal.NewSHA256 ||
		len(state.Providers) != 1 || state.Providers[0].UUID != "recovered-provider" {
		t.Fatalf("recovered initial managed state = %+v", state)
	}
	if _, err := os.Stat(manager.paths.Journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("initial creation journal remains after recovery: %v", err)
	}
}

func TestNewConfigManagerPreservesCommittedInitialManagedStateWhenJournalRemains(t *testing.T) {
	manager := newManagerForTest(t, "original-owner", "original-provider")
	if _, err := manager.EnsureManagedConfiguration(); err != nil {
		t.Fatal(err)
	}
	if err := writeApplyJournal(manager.paths.Journal, initialManagedCreationJournal()); err != nil {
		t.Fatal(err)
	}

	recovered, err := NewConfigManager(ConfigManagerOptions{
		Paths: manager.paths,
		UUID:  func() string { return "unexpected-replacement" },
	})
	if err != nil {
		t.Fatal(err)
	}
	state := recovered.State()
	if state.ManagedOwnershipID != "original-owner" || len(state.Providers) != 1 || state.Providers[0].UUID != "original-provider" {
		t.Fatalf("committed managed identity changed during journal cleanup: %+v", state)
	}
	if _, err := os.Stat(manager.paths.Journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("initial creation journal remains after committed-state recovery: %v", err)
	}
}

func TestNewConfigManagerCleansIncompleteInitialManagedCreationForRetry(t *testing.T) {
	manager := newManagerForTest(t)
	if err := writeApplyJournal(manager.paths.Journal, initialManagedCreationJournal()); err != nil {
		t.Fatal(err)
	}
	if err := writeExclusiveFile(manager.paths.Managed, []byte("schema_version:")); err != nil {
		t.Fatal(err)
	}

	recovered, err := NewConfigManager(ConfigManagerOptions{
		Paths: manager.paths,
		UUID:  deterministicUUIDs("retry-owner", "retry-provider"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manager.paths.Managed); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("incomplete managed file remains after recovery: %v", err)
	}
	if _, err := os.Stat(manager.paths.Journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("initial creation journal remains after recovery: %v", err)
	}
	description, err := recovered.EnsureManagedConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	if description.Mode != ConfigModeManaged || recovered.State().ManagedOwnershipID != "retry-owner" {
		t.Fatalf("managed retry did not complete: description=%+v state=%+v", description, recovered.State())
	}
}

func TestExternalConfigurationIsReadOnceAndNeverWritten(t *testing.T) {
	manager := newManagerForTest(t)
	path := filepath.Join(t.TempDir(), "providers.yaml")
	original := []byte("schema_version: 2\nproviders:\n  - id: local\n    type: openai-compatible\n    base_url: https://example.test/v1\n    auth_type: none\n    model_discovery: static\n    models:\n      - public_id: test-model\n        endpoints: [/chat/completions]\n")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	description, err := manager.SelectExternal(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	preserved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(preserved) != string(original) {
		t.Fatal("external file was modified")
	}
	if err := manager.RuntimeActivated(context.Background(), appcontrol.Configuration{Revision: description.SelectedRevision}, 1, ""); err != nil {
		t.Fatal(err)
	}
	modified := append(append([]byte(nil), original...), []byte("# user edit\n")...)
	if err := os.WriteFile(path, modified, 0o600); err != nil {
		t.Fatal(err)
	}
	description, err = manager.Describe()
	if err != nil {
		t.Fatal(err)
	}
	if !description.Drifted || description.ActiveRevision == description.SelectedRevision {
		t.Fatalf("drift description = %+v", description)
	}
}

func TestSecureConfigReadRejectsSymlinkDirectoryAndOversize(t *testing.T) {
	dir := t.TempDir()
	regular := filepath.Join(dir, "regular.yaml")
	if err := os.WriteFile(regular, []byte("providers: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(dir, "link.yaml")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSecureFile(symlink, MaxConfigBytes); err == nil || !strings.Contains(strings.ToLower(err.Error()), "symlink") {
		t.Fatalf("symlink read error = %v", err)
	}
	if _, _, err := readSecureFile(dir, MaxConfigBytes); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("directory read error = %v", err)
	}
	oversized := filepath.Join(dir, "oversized.yaml")
	if err := os.WriteFile(oversized, make([]byte, 1025), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := readSecureFile(oversized, 1024); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("oversize read error = %v", err)
	}
}

func TestManagedProviderUUIDSurvivesRenameAndReorder(t *testing.T) {
	manager := newManagerForTest(t, "owner", "copilot-uuid", "local-uuid")
	if _, err := manager.EnsureManagedConfiguration(); err != nil {
		t.Fatal(err)
	}
	state := manager.State()
	copilotUUID := state.Providers[0].UUID
	draft := ManagedDraft{Providers: []ManagedProviderDraft{
		{Config: proxy.ProviderConfig{ID: "local", Type: "openai-compatible", BaseURL: "https://example.test/v1", AuthType: "none", ModelDiscovery: "static", Models: []proxy.ProviderModelConfig{{PublicID: "local-model", Endpoints: []string{"/chat/completions"}}}}},
		{UUID: copilotUUID, Config: proxy.ProviderConfig{ID: "github", Type: "copilot", Default: true}},
	}}
	candidate, err := manager.BuildManagedCandidate(draft, 1)
	if err != nil {
		t.Fatal(err)
	}
	if candidate.Providers[1].UUID != copilotUUID || candidate.Providers[1].ProviderID != "github" {
		t.Fatalf("renamed identity = %+v", candidate.Providers[1])
	}
	if candidate.Providers[0].UUID != "local-uuid" {
		t.Fatalf("new provider UUID = %q", candidate.Providers[0].UUID)
	}
	second, err := manager.BuildManagedCandidate(draft, 1)
	if err != nil {
		t.Fatal(err)
	}
	if string(second.Bytes) != string(candidate.Bytes) {
		t.Fatal("managed serialization is not deterministic")
	}
}

func TestManagedApplyRollbackCommitAndCrashRecovery(t *testing.T) {
	manager := newManagerForTest(t, "owner", "copilot-uuid", "local-uuid")
	initial, err := manager.EnsureManagedConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	draft := ManagedDraft{Providers: []ManagedProviderDraft{
		{UUID: manager.State().Providers[0].UUID, Config: proxy.ProviderConfig{ID: "copilot", Type: "copilot", Default: true}},
		{Config: proxy.ProviderConfig{ID: "local", Type: "openai-compatible", BaseURL: "https://example.test/v1", AuthType: "none", ModelDiscovery: "static", Models: []proxy.ProviderModelConfig{{PublicID: "local-model", Endpoints: []string{"/chat/completions"}}}}},
	}}
	candidate, err := manager.BuildManagedCandidate(draft, 1)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := manager.PrepareManagedApply("op_rollback", candidate, initial.SelectedRevision, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Install(); err != nil {
		t.Fatal(err)
	}
	if got := manager.State().CommittedConfigRevision; got != candidate.Revision {
		t.Fatalf("installed revision = %q", got)
	}
	if err := tx.Rollback("startup_failed"); err != nil {
		t.Fatal(err)
	}
	if got := manager.State().CommittedConfigRevision; got != initial.SelectedRevision {
		t.Fatalf("rollback revision = %q", got)
	}

	candidate, err = manager.BuildManagedCandidate(draft, 1)
	if err != nil {
		t.Fatal(err)
	}
	tx, err = manager.PrepareManagedApply("op_commit", candidate, initial.SelectedRevision, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Install(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(manager.paths.Journal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("journal remains: %v", err)
	}
	committed := manager.State()
	if committed.CommittedConfigRevision != candidate.Revision || committed.SecretGeneration != 1 {
		t.Fatalf("committed state = %+v", committed)
	}

	// A later uncommitted install is restored by a fresh manager before commands.
	candidate2, err := manager.BuildManagedCandidate(ManagedDraft{Providers: draft.Providers[:1]}, 2)
	if err != nil {
		t.Fatal(err)
	}
	tx, err = manager.PrepareManagedApply("op_crash", candidate2, candidate.Revision, true)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Install(); err != nil {
		t.Fatal(err)
	}
	recovered, err := NewConfigManager(ConfigManagerOptions{Paths: manager.paths, UUID: func() string { return "unused" }})
	if err != nil {
		t.Fatal(err)
	}
	if got := recovered.State().CommittedConfigRevision; got != candidate.Revision {
		t.Fatalf("recovered revision = %q, want %q", got, candidate.Revision)
	}
}

func TestManagedSecretSentinelNeverPersistsOrEntersEnvironment(t *testing.T) {
	const sentinel = "VEKIL-SECRET-SENTINEL-7d2f"
	manager := newManagerForTest(t, "owner", "copilot-uuid", "provider-uuid")
	if _, err := manager.EnsureManagedConfiguration(); err != nil {
		t.Fatal(err)
	}
	draft := ManagedDraft{Providers: []ManagedProviderDraft{{
		SecretRoles: []string{"api_key"},
		Config:      proxy.ProviderConfig{ID: "local", Type: "openai-compatible", BaseURL: "https://example.test/v1", AuthType: "bearer", ModelDiscovery: "static", Models: []proxy.ProviderModelConfig{{PublicID: "model", Endpoints: []string{"/chat/completions"}}}},
	}}}
	candidate, err := manager.BuildManagedCandidate(draft, 1)
	if err != nil {
		t.Fatal(err)
	}
	reference := candidate.Config.Providers[0].APIKeyEnv
	store := NewSecretProjectionStore()
	if err := store.Set(SecretProjection{ConfigRevision: candidate.Revision, SecretGeneration: 1, Secrets: []SecretValue{{ProviderID: "local", Reference: reference, Value: sentinel}}}); err != nil {
		t.Fatal(err)
	}
	value, ok := store.Resolver(candidate.Revision, 1).ResolveProviderSecret("local", reference)
	if !ok || value != sentinel {
		t.Fatal("resolver did not return staged secret")
	}
	if strings.Contains(strings.Join(os.Environ(), "\n"), sentinel) {
		t.Fatal("secret entered process environment")
	}
	tx, err := manager.PrepareManagedApply("op_secret", candidate, manager.State().CommittedConfigRevision, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx.Install(); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	stateJSON, _ := json.Marshal(manager.State())
	description, _ := manager.Describe()
	descriptionJSON, _ := json.Marshal(description)
	for _, body := range [][]byte{candidate.Bytes, stateJSON, descriptionJSON} {
		if strings.Contains(string(body), sentinel) {
			t.Fatal("secret leaked into serialized output")
		}
	}
	entries, err := os.ReadDir(manager.paths.Directory)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		body, err := os.ReadFile(filepath.Join(manager.paths.Directory, entry.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(body), sentinel) {
			t.Fatalf("secret leaked into %s", entry.Name())
		}
	}
}

func TestUsePreferredAppConfigurationFallsBackToLegacyUntilManagedOptIn(t *testing.T) {
	manager := newManagerForTest(t, "owner", "provider")
	path := filepath.Join(t.TempDir(), "providers.yaml")
	body := []byte("schema_version: 2\nproviders:\n  - id: local\n    type: openai-compatible\n    default: true\n    base_url: https://example.test/v1\n    auth_type: none\n    model_discovery: static\n    models:\n      - public_id: local-model\n        endpoints: [/chat/completions]\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SelectExternal(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if err := manager.UsePreferredAppConfiguration(); err != nil {
		t.Fatal(err)
	}
	if state := manager.State(); state.ConfigMode != ConfigModeLegacy || state.SelectedPath != "" || state.SelectedConfigRevision != LegacyConfigRevision {
		t.Fatalf("legacy fallback state = %+v", state)
	}

	if _, err := manager.EnsureManagedConfiguration(); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SelectExternal(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if err := manager.UsePreferredAppConfiguration(); err != nil {
		t.Fatal(err)
	}
	if state := manager.State(); state.ConfigMode != ConfigModeManaged || state.SelectedPath != manager.paths.Managed {
		t.Fatalf("managed preferred state = %+v", state)
	}
}

func TestVersionedExternalStateRemainsReadableByForwardRevertShell(t *testing.T) {
	manager := newManagerForTest(t)
	path := filepath.Join(t.TempDir(), "providers.yaml")
	body := []byte("schema_version: 2\nproviders:\n  - id: local\n    type: openai-compatible\n    default: true\n    base_url: https://example.test/v1\n    auth_type: none\n    model_discovery: static\n    models:\n      - public_id: local-model\n        endpoints: [/chat/completions]\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.SelectExternal(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	persisted, err := os.ReadFile(manager.paths.State)
	if err != nil {
		t.Fatal(err)
	}
	var legacy struct {
		ProvidersConfigPath string `json:"providers_config_path"`
	}
	if err := json.Unmarshal(persisted, &legacy); err != nil {
		t.Fatal(err)
	}
	if legacy.ProvidersConfigPath != path {
		t.Fatalf("forward-revert providers_config_path = %q, want %q\nstate=%s", legacy.ProvidersConfigPath, path, persisted)
	}
}

func TestStagedExternalSelectionIsNotPersistedBeforeCommit(t *testing.T) {
	manager := newManagerForTest(t)
	path := filepath.Join(t.TempDir(), "providers.yaml")
	body := []byte("schema_version: 2\nproviders:\n  - id: local\n    type: openai-compatible\n    default: true\n    base_url: https://example.test/v1\n    auth_type: none\n    model_discovery: static\n    models:\n      - public_id: local-model\n        endpoints: [/chat/completions]\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.StageExternal(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	if err := manager.RuntimeDeactivated(t.Context(), 1); err != nil {
		t.Fatal(err)
	}
	persisted, _, err := loadPersistentState(manager.paths.State)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ConfigMode != ConfigModeLegacy || persisted.SelectedPath != "" {
		t.Fatalf("staged selection leaked to disk: %+v", persisted)
	}
	if err := manager.CommitSelection(); err != nil {
		t.Fatal(err)
	}
	persisted, _, err = loadPersistentState(manager.paths.State)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ConfigMode != ConfigModeExternal || persisted.SelectedPath != path {
		t.Fatalf("committed selection missing: %+v", persisted)
	}
}

func TestConfigurationSelectionRejectsPendingRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "providers.yaml")
	body := []byte("schema_version: 2\nproviders:\n  - id: local\n    type: openai-compatible\n    default: true\n    base_url: https://example.test/v1\n    auth_type: none\n    model_discovery: static\n    models:\n      - public_id: local-model\n        endpoints: [/chat/completions]\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name string
		run  func(*ConfigManager) error
	}{
		{name: "stage external", run: func(manager *ConfigManager) error { return manager.StageExternal(t.Context(), path) }},
		{name: "reload external", run: func(manager *ConfigManager) error { return manager.StageReloadExternal(t.Context()) }},
		{name: "stage preferred", run: func(manager *ConfigManager) error { return manager.StagePreferredAppConfiguration() }},
		{name: "use legacy", run: func(manager *ConfigManager) error { return manager.UseLegacy() }},
		{name: "use managed", run: func(manager *ConfigManager) error { return manager.UseManaged() }},
		{name: "ensure managed", run: func(manager *ConfigManager) error { _, err := manager.EnsureManagedConfiguration(); return err }},
		{name: "commit", run: func(manager *ConfigManager) error { return manager.CommitSelection() }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			manager := newManagerForTest(t)
			manager.mu.Lock()
			manager.state.RecoveryState = string(ApplyPhaseRollbackFailed)
			manager.mu.Unlock()

			err := test.run(manager)
			if err == nil || !strings.Contains(err.Error(), "managed recovery is required") {
				t.Fatalf("selection error = %v, want recovery rejection", err)
			}
			if manager.stagedSelection != nil {
				t.Fatal("selection was staged during recovery")
			}
			state := manager.State()
			if state.ConfigMode != ConfigModeLegacy || state.SelectedPath != "" || state.RecoveryState != string(ApplyPhaseRollbackFailed) {
				t.Fatalf("state changed during recovery: %+v", state)
			}
		})
	}
}

func TestCommitSelectionRejectsRecoveryThatStartsAfterStaging(t *testing.T) {
	manager := newManagerForTest(t)
	path := filepath.Join(t.TempDir(), "providers.yaml")
	body := []byte("schema_version: 2\nproviders:\n  - id: local\n    type: openai-compatible\n    default: true\n    base_url: https://example.test/v1\n    auth_type: none\n    model_discovery: static\n    models:\n      - public_id: local-model\n        endpoints: [/chat/completions]\n")
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := manager.StageExternal(t.Context(), path); err != nil {
		t.Fatal(err)
	}
	manager.mu.Lock()
	manager.state.RecoveryState = string(ApplyPhaseRollbackFailed)
	manager.mu.Unlock()

	if err := manager.CommitSelection(); err == nil || !strings.Contains(err.Error(), "managed recovery is required") {
		t.Fatalf("CommitSelection() error = %v, want recovery rejection", err)
	}
	if manager.stagedSelection == nil {
		t.Fatal("rejected commit discarded the staged selection")
	}
	persisted, _, err := loadPersistentState(manager.paths.State)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.ConfigMode != ConfigModeLegacy || persisted.SelectedPath != "" {
		t.Fatalf("rejected selection reached disk: %+v", persisted)
	}
}
