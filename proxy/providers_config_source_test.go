package proxy

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestLoadProvidersConfigBootstrapIdentityAndManagedPath(t *testing.T) {
	t.Parallel()

	userConfigDir := t.TempDir()
	implicit, err := LoadProvidersConfigBootstrap("", userConfigDir)
	if err != nil {
		t.Fatalf("LoadProvidersConfigBootstrap(implicit) error = %v", err)
	}
	if implicit.Source.Kind != ProvidersConfigSourceImplicitCopilot || implicit.Source.ID != "implicit-copilot" || implicit.Source.BootstrapPath != "" {
		t.Fatalf("implicit source = %+v", implicit.Source)
	}
	wantImplicitPath := filepath.Join(userConfigDir, "vekil", "dashboard-config", "implicit-copilot.json")
	if implicit.Source.ManagedPath != wantImplicitPath {
		t.Fatalf("implicit managed path = %q, want %q", implicit.Source.ManagedPath, wantImplicitPath)
	}
	if !strings.HasPrefix(implicit.Source.BootstrapDigest, "sha256:") || !strings.HasPrefix(implicit.Revision, "cfg_") {
		t.Fatalf("implicit digest/revision = %q %q", implicit.Source.BootstrapDigest, implicit.Revision)
	}

	bootstrapPath := filepath.Join(t.TempDir(), "nested", "..", "providers.json")
	if err := os.MkdirAll(filepath.Dir(filepath.Clean(bootstrapPath)), 0o755); err != nil {
		t.Fatal(err)
	}
	bootstrapPath = filepath.Clean(bootstrapPath)
	if err := os.WriteFile(bootstrapPath, []byte(`{"providers":[{"id":"copilot","type":"copilot","default":true}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	fileBootstrap, err := LoadProvidersConfigBootstrap(bootstrapPath, userConfigDir)
	if err != nil {
		t.Fatalf("LoadProvidersConfigBootstrap(file) error = %v", err)
	}
	absolutePath, _ := filepath.Abs(bootstrapPath)
	absolutePath = filepath.Clean(absolutePath)
	if fileBootstrap.Source.Kind != ProvidersConfigSourceFile || fileBootstrap.Source.ID != "file:"+absolutePath || fileBootstrap.Source.BootstrapPath != absolutePath {
		t.Fatalf("file source = %+v", fileBootstrap.Source)
	}
	if fileBootstrap.Source.ManagedPath == implicit.Source.ManagedPath || filepath.Base(fileBootstrap.Source.ManagedPath) == "implicit-copilot.json" {
		t.Fatalf("file source not isolated: %q", fileBootstrap.Source.ManagedPath)
	}
}

func TestManagedProvidersConfigStoreCommitResolvePermissionsAndBootstrapUnchanged(t *testing.T) {
	userConfigDir := t.TempDir()
	bootstrapDir := t.TempDir()
	bootstrapPath := filepath.Join(bootstrapDir, "providers.yaml")
	bootstrapBody := []byte("providers:\n  - id: copilot\n    type: copilot\n    default: true\n")
	if err := os.WriteFile(bootstrapPath, bootstrapBody, 0o644); err != nil {
		t.Fatal(err)
	}

	bootstrap, err := LoadProvidersConfigBootstrap(bootstrapPath, userConfigDir)
	if err != nil {
		t.Fatalf("LoadProvidersConfigBootstrap() error = %v", err)
	}
	store, err := NewManagedProvidersConfigStore(bootstrap)
	if err != nil {
		t.Fatalf("NewManagedProvidersConfigStore() error = %v", err)
	}
	managed := bootstrap.Config
	managed.Providers[0].Headers.Default.UserAgent = "managed-user-agent"
	commit, err := store.Commit(context.Background(), bootstrap.Revision, managed)
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if commit.Envelope.Revision == bootstrap.Revision || commit.Path != bootstrap.Source.ManagedPath {
		t.Fatalf("commit = %+v, bootstrap revision %q", commit, bootstrap.Revision)
	}
	if commit.DurabilityWarning != nil {
		t.Fatalf("Commit() durability warning = %v", commit.DurabilityWarning)
	}

	resolved, err := ResolveProvidersConfig(ProvidersConfigResolveOptions{BootstrapPath: bootstrapPath, UserConfigDir: userConfigDir})
	if err != nil {
		t.Fatalf("ResolveProvidersConfig() error = %v", err)
	}
	if !resolved.Managed || resolved.Revision != commit.Envelope.Revision || resolved.Config.Providers[0].Headers.Default.UserAgent != "managed-user-agent" {
		t.Fatalf("resolved config = %+v", resolved)
	}

	gotBootstrapBody, err := os.ReadFile(bootstrapPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotBootstrapBody) != string(bootstrapBody) {
		t.Fatalf("bootstrap changed:\nwant %q\n got %q", bootstrapBody, gotBootstrapBody)
	}

	if runtime.GOOS != "windows" {
		dirInfo, err := os.Stat(filepath.Dir(store.Path()))
		if err != nil {
			t.Fatal(err)
		}
		if got := dirInfo.Mode().Perm(); got != 0o700 {
			t.Fatalf("managed directory mode = %o, want 700", got)
		}
		fileInfo, err := os.Stat(store.Path())
		if err != nil {
			t.Fatal(err)
		}
		if got := fileInfo.Mode().Perm(); got != 0o600 {
			t.Fatalf("managed file mode = %o, want 600", got)
		}
	}
}

func TestManagedProvidersConfigSourceIsolationAndConflictRecovery(t *testing.T) {
	userConfigDir := t.TempDir()
	bootstrapDir := t.TempDir()
	pathA := filepath.Join(bootstrapDir, "a.json")
	pathB := filepath.Join(bootstrapDir, "b.json")
	bodyA := []byte(`{"providers":[{"id":"copilot-a","type":"copilot","default":true}]}`)
	bodyB := []byte(`{"providers":[{"id":"copilot-b","type":"copilot","default":true}]}`)
	if err := os.WriteFile(pathA, bodyA, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, bodyB, 0o644); err != nil {
		t.Fatal(err)
	}

	bootstrapA, err := LoadProvidersConfigBootstrap(pathA, userConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapB, err := LoadProvidersConfigBootstrap(pathB, userConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if bootstrapA.Source.ManagedPath == bootstrapB.Source.ManagedPath {
		t.Fatalf("distinct sources share managed path %q", bootstrapA.Source.ManagedPath)
	}
	storeA, err := NewManagedProvidersConfigStore(bootstrapA)
	if err != nil {
		t.Fatal(err)
	}
	managedA := bootstrapA.Config
	managedA.Providers[0].Headers.Responses.OpenAIIntent = "managed-a"
	commit, err := storeA.Commit(context.Background(), bootstrapA.Revision, managedA)
	if err != nil {
		t.Fatalf("Commit(A) error = %v", err)
	}

	resolvedA, err := ResolveProvidersConfig(ProvidersConfigResolveOptions{BootstrapPath: pathA, UserConfigDir: userConfigDir})
	if err != nil || !resolvedA.Managed || resolvedA.Revision != commit.Envelope.Revision {
		t.Fatalf("resolve A = %+v, %v", resolvedA, err)
	}
	resolvedB, err := ResolveProvidersConfig(ProvidersConfigResolveOptions{BootstrapPath: pathB, UserConfigDir: userConfigDir})
	if err != nil {
		t.Fatalf("Resolve(B) error = %v", err)
	}
	if resolvedB.Managed || resolvedB.Config.Providers[0].ID != "copilot-b" {
		t.Fatalf("source B consumed source A managed override: %+v", resolvedB)
	}

	// Raw whitespace and formatting do not alter the canonical secretful digest.
	formattedA := []byte("{\n  \"providers\": [ { \"default\": true, \"type\": \"copilot\", \"id\": \"copilot-a\" } ]\n}\n")
	if err := os.WriteFile(pathA, formattedA, 0o644); err != nil {
		t.Fatal(err)
	}
	formattedResolved, err := ResolveProvidersConfig(ProvidersConfigResolveOptions{BootstrapPath: pathA, UserConfigDir: userConfigDir})
	if err != nil || !formattedResolved.Managed || formattedResolved.Revision != commit.Envelope.Revision {
		t.Fatalf("format-only bootstrap rewrite changed resolution: %+v, %v", formattedResolved, err)
	}

	changedA := []byte(`{
		"providers": [{
			"id":"copilot-a","type":"copilot","default":true,
			"headers":{"default":{"user_agent":"bootstrap-changed"}}
		}]
	}`)
	if err := os.WriteFile(pathA, changedA, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = ResolveProvidersConfig(ProvidersConfigResolveOptions{BootstrapPath: pathA, UserConfigDir: userConfigDir})
	assertConfigError(t, err, ConfigErrorManagedSourceConflict, "/source/bootstrap_digest")

	ignored, err := ResolveProvidersConfig(ProvidersConfigResolveOptions{
		BootstrapPath: pathA,
		UserConfigDir: userConfigDir,
		Mode:          ProvidersConfigIgnoreManaged,
	})
	if err != nil {
		t.Fatalf("ignore-managed resolve error = %v", err)
	}
	if ignored.Managed || ignored.Config.Providers[0].Headers.Default.UserAgent != "bootstrap-changed" {
		t.Fatalf("ignore-managed result = %+v", ignored)
	}
	if _, err := os.Stat(bootstrapA.Source.ManagedPath); err != nil {
		t.Fatalf("ignore-managed removed managed file: %v", err)
	}

	reset, err := ResolveProvidersConfig(ProvidersConfigResolveOptions{
		BootstrapPath: pathA,
		UserConfigDir: userConfigDir,
		Mode:          ProvidersConfigResetManaged,
	})
	if err != nil {
		t.Fatalf("reset-managed resolve error = %v", err)
	}
	if reset.Managed || reset.Config.Providers[0].Headers.Default.UserAgent != "bootstrap-changed" {
		t.Fatalf("reset-managed result = %+v", reset)
	}
	if _, err := os.Stat(bootstrapA.Source.ManagedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed file still exists after reset: %v", err)
	}

	afterReset, err := ResolveProvidersConfig(ProvidersConfigResolveOptions{BootstrapPath: pathA, UserConfigDir: userConfigDir})
	if err != nil || afterReset.Managed || afterReset.Revision != reset.Revision {
		t.Fatalf("resolve after reset = %+v, %v", afterReset, err)
	}
}

func TestManagedProvidersConfigIgnoreAndResetRecoverMalformedEnvelope(t *testing.T) {
	userConfigDir := t.TempDir()
	bootstrap, err := LoadProvidersConfigBootstrap("", userConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(bootstrap.Source.ManagedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bootstrap.Source.ManagedPath, []byte(`{"managed_schema_version":1,"broken":`), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ResolveProvidersConfig(ProvidersConfigResolveOptions{UserConfigDir: userConfigDir}); err == nil {
		t.Fatal("normal resolve error = nil, want malformed managed envelope error")
	}
	ignored, err := ResolveProvidersConfig(ProvidersConfigResolveOptions{UserConfigDir: userConfigDir, Mode: ProvidersConfigIgnoreManaged})
	if err != nil || ignored.Managed {
		t.Fatalf("ignore-managed = %+v, %v", ignored, err)
	}
	reset, err := ResolveProvidersConfig(ProvidersConfigResolveOptions{UserConfigDir: userConfigDir, Mode: ProvidersConfigResetManaged})
	if err != nil || reset.Managed {
		t.Fatalf("reset-managed = %+v, %v", reset, err)
	}
	if _, err := os.Stat(bootstrap.Source.ManagedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("malformed managed file remains after reset: %v", err)
	}
}

func TestManagedProvidersConfigStoreRejectsStaleRevision(t *testing.T) {
	t.Parallel()

	userConfigDir := t.TempDir()
	bootstrap, err := LoadProvidersConfigBootstrap("", userConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewManagedProvidersConfigStore(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	firstConfig := ProvidersConfig{Providers: []ProviderConfig{{ID: "copilot", Type: "copilot", Default: true}}}
	first, err := store.Commit(context.Background(), bootstrap.Revision, firstConfig)
	if err != nil {
		t.Fatalf("first Commit() error = %v", err)
	}
	secondConfig := firstConfig
	secondConfig.Providers[0].Headers.Default.UserAgent = "stale-overwrite"
	_, err = store.Commit(context.Background(), bootstrap.Revision, secondConfig)
	assertConfigError(t, err, ConfigErrorRevisionMismatch, "/revision")

	resolved, err := ResolveProvidersConfig(ProvidersConfigResolveOptions{UserConfigDir: userConfigDir})
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Revision != first.Envelope.Revision || resolved.Config.Providers[0].Headers.Default.UserAgent != "" {
		t.Fatalf("stale commit changed managed state: %+v", resolved)
	}
}

func TestManagedProvidersConfigStoreResetUsesCurrentRevision(t *testing.T) {
	t.Parallel()

	userConfigDir := t.TempDir()
	bootstrap, err := LoadProvidersConfigBootstrap("", userConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewManagedProvidersConfigStore(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	commit, err := store.Commit(context.Background(), bootstrap.Revision, ProvidersConfig{Providers: []ProviderConfig{{ID: "copilot", Type: "copilot", Default: true}}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Reset(context.Background(), bootstrap.Revision)
	assertConfigError(t, err, ConfigErrorRevisionMismatch, "/revision")

	reset, err := store.Reset(context.Background(), commit.Envelope.Revision)
	if err != nil {
		t.Fatalf("Reset() error = %v", err)
	}
	if reset.Revision != bootstrap.Revision {
		t.Fatalf("reset revision = %q, want %q", reset.Revision, bootstrap.Revision)
	}
	if _, err := os.Stat(store.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("managed file remains after Reset(): %v", err)
	}
}

func TestManagedProvidersConfigStoreRejectsBootstrapAlias(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("hard-link permission behavior is platform-specific")
	}

	userConfigDir := t.TempDir()
	bootstrapPath := filepath.Join(t.TempDir(), "providers.json")
	bootstrapBody := []byte(`{"providers":[{"id":"copilot","type":"copilot","default":true}]}`)
	if err := os.WriteFile(bootstrapPath, bootstrapBody, 0o644); err != nil {
		t.Fatal(err)
	}
	bootstrap, err := LoadProvidersConfigBootstrap(bootstrapPath, userConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(bootstrap.Source.ManagedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Link(bootstrapPath, bootstrap.Source.ManagedPath); err != nil {
		t.Skipf("hard links unavailable: %v", err)
	}
	store, err := NewManagedProvidersConfigStore(bootstrap)
	if err != nil {
		t.Fatal(err)
	}
	_, err = store.Commit(context.Background(), bootstrap.Revision, bootstrap.Config)
	assertConfigError(t, err, ConfigErrorManagedSourceConflict, "/source/id")
	got, err := os.ReadFile(bootstrapPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(bootstrapBody) {
		t.Fatalf("bootstrap changed through managed alias: %q", got)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(store.Path()), ".providers-config-*.tmp"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("temporary files remain after alias failure: %v", matches)
	}
}

func TestManagedProvidersConfigResolverRejectsMismatchedSourceEnvelope(t *testing.T) {
	t.Parallel()

	userConfigDir := t.TempDir()
	bootstrapDir := t.TempDir()
	pathA := filepath.Join(bootstrapDir, "a.json")
	pathB := filepath.Join(bootstrapDir, "b.json")
	body := []byte(`{"providers":[{"id":"copilot","type":"copilot","default":true}]}`)
	if err := os.WriteFile(pathA, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(pathB, body, 0o644); err != nil {
		t.Fatal(err)
	}
	bootstrapA, err := LoadProvidersConfigBootstrap(pathA, userConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	bootstrapB, err := LoadProvidersConfigBootstrap(pathB, userConfigDir)
	if err != nil {
		t.Fatal(err)
	}
	envelope, err := NewManagedProvidersConfigEnvelope(bootstrapB.Source, bootstrapA.Config)
	if err != nil {
		t.Fatal(err)
	}
	envelopeBody, err := EncodeManagedProvidersConfigJSON(envelope)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(bootstrapA.Source.ManagedPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bootstrapA.Source.ManagedPath, envelopeBody, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = ResolveProvidersConfig(ProvidersConfigResolveOptions{BootstrapPath: pathA, UserConfigDir: userConfigDir})
	assertConfigError(t, err, ConfigErrorManagedSourceConflict, "/source/id")
}

func assertConfigError(t *testing.T, err error, code ConfigErrorCode, pointer string) {
	t.Helper()
	var typed *ConfigError
	if !errors.As(err, &typed) {
		t.Fatalf("error = %v, want *ConfigError", err)
	}
	if typed.Code != code || typed.Pointer != pointer {
		t.Fatalf("error = code %q pointer %q, want %q %q (error %v)", typed.Code, typed.Pointer, code, pointer, err)
	}
}

func TestProbeManagedProvidersConfigWritableRejectsSymlinkDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on many Windows builders")
	}
	root := t.TempDir()
	realDir := filepath.Join(root, "real")
	if err := os.MkdirAll(realDir, 0o700); err != nil {
		t.Fatal(err)
	}
	linkDir := filepath.Join(root, "managed-link")
	if err := os.Symlink(realDir, linkDir); err != nil {
		t.Fatal(err)
	}
	if err := ProbeManagedProvidersConfigWritable(filepath.Join(linkDir, "config.json")); err == nil {
		t.Fatal("ProbeManagedProvidersConfigWritable accepted a symlink directory")
	}
}

func TestManagedProvidersConfigStoreCancellationAtIrreversibleFence(t *testing.T) {
	t.Run("commit leaves destination unchanged", func(t *testing.T) {
		bootstrap, err := LoadProvidersConfigBootstrap("", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		store, err := NewManagedProvidersConfigStore(bootstrap)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		store.beforeIrreversible = cancel
		_, err = store.Commit(ctx, bootstrap.Revision, ProvidersConfig{Providers: []ProviderConfig{{ID: "copilot", Type: "copilot", Default: true}}})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Commit() error = %v, want context.Canceled", err)
		}
		if _, err := os.Stat(store.Path()); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("canceled commit replaced destination: %v", err)
		}
		matches, err := filepath.Glob(filepath.Join(filepath.Dir(store.Path()), ".providers-config-*.tmp"))
		if err != nil {
			t.Fatal(err)
		}
		if len(matches) != 0 {
			t.Fatalf("canceled commit left temporary files: %v", matches)
		}
	})

	t.Run("reset preserves existing override", func(t *testing.T) {
		bootstrap, err := LoadProvidersConfigBootstrap("", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		store, err := NewManagedProvidersConfigStore(bootstrap)
		if err != nil {
			t.Fatal(err)
		}
		managed := ProvidersConfig{Providers: []ProviderConfig{{ID: "copilot", Type: "copilot", Default: true}}}
		commit, err := store.Commit(context.Background(), bootstrap.Revision, managed)
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithCancel(context.Background())
		store.beforeIrreversible = cancel
		_, err = store.Reset(ctx, commit.Envelope.Revision)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Reset() error = %v, want context.Canceled", err)
		}
		resolved, err := ResolveProvidersConfig(ProvidersConfigResolveOptions{UserConfigDir: filepath.Dir(filepath.Dir(filepath.Dir(store.Path()))), Mode: ProvidersConfigUseManaged})
		if err != nil {
			t.Fatal(err)
		}
		if !resolved.Managed || resolved.Revision != commit.Envelope.Revision {
			t.Fatalf("canceled reset changed managed state: %+v", resolved)
		}
	})
}
