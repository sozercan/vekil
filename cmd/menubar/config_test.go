package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/proxy"
)

func TestLoadMenubarConfigMissingFile(t *testing.T) {
	configDir := stubUserConfigDir(t)

	cfg, err := loadMenubarConfig()
	if err != nil {
		t.Fatalf("loadMenubarConfig() error = %v", err)
	}
	if cfg.ProvidersConfigPath != "" {
		t.Fatalf("loadMenubarConfig() ProvidersConfigPath = %q, want empty", cfg.ProvidersConfigPath)
	}

	if _, err := os.Stat(filepath.Join(configDir, "vekil", menubarConfigFilename)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected config file to be absent, stat error = %v", err)
	}
}

func TestSaveMenubarConfigRoundTrip(t *testing.T) {
	configDir := stubUserConfigDir(t)
	wantPath := "/tmp/providers.json"

	if err := saveMenubarConfig(menubarConfig{ProvidersConfigPath: "  " + wantPath + "  "}); err != nil {
		t.Fatalf("saveMenubarConfig() error = %v", err)
	}

	cfg, err := loadMenubarConfig()
	if err != nil {
		t.Fatalf("loadMenubarConfig() error = %v", err)
	}
	if cfg.ProvidersConfigPath != wantPath {
		t.Fatalf("loadMenubarConfig() ProvidersConfigPath = %q, want %q", cfg.ProvidersConfigPath, wantPath)
	}

	body, err := os.ReadFile(filepath.Join(configDir, "vekil", menubarConfigFilename))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if len(body) == 0 || body[len(body)-1] != '\n' {
		t.Fatalf("saved config should end with newline, got %q", string(body))
	}
}

func TestSaveMenubarConfigClearsFile(t *testing.T) {
	configDir := stubUserConfigDir(t)
	configPath := filepath.Join(configDir, "vekil", menubarConfigFilename)

	if err := saveMenubarConfig(menubarConfig{ProvidersConfigPath: "/tmp/providers.json"}); err != nil {
		t.Fatalf("saveMenubarConfig(set) error = %v", err)
	}
	if err := saveMenubarConfig(menubarConfig{}); err != nil {
		t.Fatalf("saveMenubarConfig(clear) error = %v", err)
	}

	if _, err := os.Stat(configPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected config file to be removed, stat error = %v", err)
	}
}

func TestLoadProvidersConfigForMenubar(t *testing.T) {
	t.Run("missing menubar file uses default config", func(t *testing.T) {
		stubUserConfigDir(t)

		cfg, providers, err := loadProvidersConfigForMenubar()
		if err != nil {
			t.Fatalf("loadProvidersConfigForMenubar() error = %v", err)
		}
		if cfg.ProvidersConfigPath != "" {
			t.Fatalf("loadProvidersConfigForMenubar() ProvidersConfigPath = %q, want empty", cfg.ProvidersConfigPath)
		}
		if len(providers.Resolved.Config.Providers) != 0 {
			t.Fatalf("loadProvidersConfigForMenubar() Providers = %v, want empty", providers.Resolved.Config.Providers)
		}
		if providers.Store == nil {
			t.Fatal("loadProvidersConfigForMenubar() Store = nil, want writable managed store")
		}
	})

	t.Run("invalid menubar config is wrapped", func(t *testing.T) {
		configDir := stubUserConfigDir(t)
		configPath := filepath.Join(configDir, "vekil", menubarConfigFilename)
		if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		if err := os.WriteFile(configPath, []byte("{"), 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}

		_, _, err := loadProvidersConfigForMenubar()
		if !errors.Is(err, errMenubarConfigLoad) {
			t.Fatalf("loadProvidersConfigForMenubar() error = %v, want wrapped menubar config error", err)
		}
	})

	t.Run("invalid providers path is wrapped", func(t *testing.T) {
		stubUserConfigDir(t)

		if err := saveMenubarConfig(menubarConfig{ProvidersConfigPath: "/tmp/missing-providers.json"}); err != nil {
			t.Fatalf("saveMenubarConfig() error = %v", err)
		}

		cfg, _, err := loadProvidersConfigForMenubar()
		if !errors.Is(err, errProvidersConfigLoad) {
			t.Fatalf("loadProvidersConfigForMenubar() error = %v, want wrapped providers config error", err)
		}
		if cfg.ProvidersConfigPath != "/tmp/missing-providers.json" {
			t.Fatalf("loadProvidersConfigForMenubar() ProvidersConfigPath = %q, want missing path", cfg.ProvidersConfigPath)
		}
	})

	t.Run("valid providers config loads successfully", func(t *testing.T) {
		stubUserConfigDir(t)

		providersPath := filepath.Join(t.TempDir(), "providers.json")
		body := []byte(`{"providers":[{"id":"custom-copilot","type":"copilot","default":true}]}`)
		if err := os.WriteFile(providersPath, body, 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		if err := saveMenubarConfig(menubarConfig{ProvidersConfigPath: providersPath}); err != nil {
			t.Fatalf("saveMenubarConfig() error = %v", err)
		}

		cfg, providers, err := loadProvidersConfigForMenubar()
		if err != nil {
			t.Fatalf("loadProvidersConfigForMenubar() error = %v", err)
		}
		if cfg.ProvidersConfigPath != providersPath {
			t.Fatalf("loadProvidersConfigForMenubar() ProvidersConfigPath = %q, want %q", cfg.ProvidersConfigPath, providersPath)
		}
		providersCfg := providers.Resolved.Config
		if len(providersCfg.Providers) != 1 || providersCfg.Providers[0].ID != "custom-copilot" {
			t.Fatalf("loadProvidersConfigForMenubar() Providers = %v, want custom Copilot provider", providersCfg.Providers)
		}
	})
}

func TestProvidersConfigErrorPresentation(t *testing.T) {
	menubarErr := errors.Join(errMenubarConfigLoad, errors.New("decode menubar config"))
	providersErr := errors.Join(errProvidersConfigLoad, errors.New("decode providers config"))

	if got := providersConfigStatusTitle(menubarErr); got != "⚠ Config unavailable" {
		t.Fatalf("providersConfigStatusTitle(menubarErr) = %q, want %q", got, "⚠ Config unavailable")
	}
	if got := providersConfigStatusTitle(providersErr); got != "⚠ Invalid providers config" {
		t.Fatalf("providersConfigStatusTitle(providersErr) = %q, want %q", got, "⚠ Invalid providers config")
	}

	if title, message := providersConfigUnavailableDialog(menubarErr); title != "Menubar Config Unavailable" || message != "Could not load the saved menubar config." {
		t.Fatalf("providersConfigUnavailableDialog(menubarErr) = (%q, %q)", title, message)
	}
	if title, message := providersConfigStartDialog(providersErr); title != "Invalid Providers Config" || message != "Could not load the selected providers config." {
		t.Fatalf("providersConfigStartDialog(providersErr) = (%q, %q)", title, message)
	}

	prevCfg := menubarCfg
	prevErr := providersConfigErr
	t.Cleanup(func() {
		menubarCfg = prevCfg
		providersConfigErr = prevErr
	})

	menubarCfg = menubarConfig{ProvidersConfigPath: "/tmp/providers.json"}
	providersConfigErr = menubarErr
	if got := providersMenuTitle(); got != "Providers: Config unavailable" {
		t.Fatalf("providersMenuTitle() with menubar error = %q, want %q", got, "Providers: Config unavailable")
	}

	providersConfigErr = providersErr
	if got := providersMenuTitle(); got != "Providers: Invalid (providers.json)" {
		t.Fatalf("providersMenuTitle() with providers error = %q, want %q", got, "Providers: Invalid (providers.json)")
	}
}

func TestAuthMenuTitle(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		status auth.AuthStatus
		want   string
	}{
		{
			name:   "not signed in",
			status: auth.AuthStatus{Source: auth.AuthSourceNone},
			want:   "GitHub Auth: Not Signed In",
		},
		{
			name:   "signed out",
			status: auth.AuthStatus{Source: auth.AuthSourceNone, SignedOut: true},
			want:   "GitHub Auth: Signed Out",
		},
		{
			name:   "environment token",
			status: auth.AuthStatus{SignedIn: true, Source: auth.AuthSourceEnv},
			want:   "GitHub Auth: Environment Token",
		},
		{
			name:   "vekil managed",
			status: auth.AuthStatus{SignedIn: true, Source: auth.AuthSourceVekil},
			want:   "GitHub Auth: Signed in with GitHub",
		},
		{
			name:   "github cli",
			status: auth.AuthStatus{SignedIn: true, Source: auth.AuthSourceGitHubCLI},
			want:   "GitHub Auth: Using GitHub CLI Account",
		},
		{
			name:   "unknown signed in source",
			status: auth.AuthStatus{SignedIn: true, Source: auth.AuthSourceNone},
			want:   "GitHub Auth: Signed In",
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := authMenuTitle(tt.status); got != tt.want {
				t.Fatalf("authMenuTitle() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestProvidersRequireGitHubAuth(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		cfg  proxy.ProvidersConfig
		err  error
		want bool
	}{
		{
			name: "default config uses copilot",
			want: true,
		},
		{
			name: "provider only config skips auth",
			cfg: proxy.ProvidersConfig{
				Providers: []proxy.ProviderConfig{
					{ID: "azure", Type: "azure-openai"},
				},
			},
			want: false,
		},
		{
			name: "invalid config does not trigger auth refresh",
			cfg: proxy.ProvidersConfig{
				Providers: []proxy.ProviderConfig{
					{ID: "azure", Type: "azure-openai"},
				},
			},
			err:  errors.New("boom"),
			want: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			if got := providersRequireGitHubAuth(tt.cfg, tt.err); got != tt.want {
				t.Fatalf("providersRequireGitHubAuth() = %v, want %v", got, tt.want)
			}
		})
	}
}

func stubUserConfigDir(t *testing.T) string {
	t.Helper()

	tmpDir := t.TempDir()
	prev := userConfigDir
	userConfigDir = func() (string, error) {
		return tmpDir, nil
	}
	t.Cleanup(func() {
		userConfigDir = prev
	})

	return tmpDir
}

func TestResolveMenubarProvidersConfigMatchesSharedResolver(t *testing.T) {
	configDir := stubUserConfigDir(t)
	bootstrapPath := filepath.Join(t.TempDir(), "providers.json")
	writeMenubarStartupBootstrap(t, bootstrapPath, "bootstrap")
	commitMenubarManagedOverride(t, bootstrapPath, configDir, "managed")

	shared, err := proxy.ResolveProvidersConfig(proxy.ProvidersConfigResolveOptions{
		BootstrapPath: bootstrapPath,
		UserConfigDir: configDir,
		Mode:          proxy.ProvidersConfigUseManaged,
	})
	if err != nil {
		t.Fatalf("ResolveProvidersConfig() error = %v", err)
	}
	menubar, err := resolveMenubarProvidersConfig(menubarConfig{ProvidersConfigPath: bootstrapPath}, proxy.ProvidersConfigUseManaged)
	if err != nil {
		t.Fatalf("resolveMenubarProvidersConfig() error = %v", err)
	}
	if menubar.Resolved.Revision != shared.Revision || menubar.Resolved.Bootstrap.Source != shared.Bootstrap.Source {
		t.Fatalf("menubar resolution = %+v, shared = %+v", menubar.Resolved, shared)
	}
	if got := menubar.Resolved.Config.Providers[0].ID; got != "managed" {
		t.Fatalf("menubar provider = %q, want managed", got)
	}
	if menubar.Store == nil {
		t.Fatal("resolveMenubarProvidersConfig() Store = nil, want writable store")
	}
}

func TestResolveMenubarProvidersConfigSourceIsolation(t *testing.T) {
	configDir := stubUserConfigDir(t)
	bootstrapDir := t.TempDir()
	pathA := filepath.Join(bootstrapDir, "a.json")
	pathB := filepath.Join(bootstrapDir, "b.json")
	writeMenubarStartupBootstrap(t, pathA, "bootstrap-a")
	writeMenubarStartupBootstrap(t, pathB, "bootstrap-b")
	commitMenubarManagedOverride(t, pathA, configDir, "managed-a")
	commitMenubarManagedOverride(t, pathB, configDir, "managed-b")

	resolvedA, err := resolveMenubarProvidersConfig(menubarConfig{ProvidersConfigPath: pathA}, proxy.ProvidersConfigUseManaged)
	if err != nil {
		t.Fatalf("resolveMenubarProvidersConfig(a) error = %v", err)
	}
	resolvedB, err := resolveMenubarProvidersConfig(menubarConfig{ProvidersConfigPath: pathB}, proxy.ProvidersConfigUseManaged)
	if err != nil {
		t.Fatalf("resolveMenubarProvidersConfig(b) error = %v", err)
	}

	if got := resolvedA.Resolved.Config.Providers[0].ID; got != "managed-a" {
		t.Fatalf("source A provider = %q, want managed-a", got)
	}
	if got := resolvedB.Resolved.Config.Providers[0].ID; got != "managed-b" {
		t.Fatalf("source B provider = %q, want managed-b", got)
	}
	if resolvedA.Resolved.Bootstrap.Source.ID == resolvedB.Resolved.Bootstrap.Source.ID {
		t.Fatalf("source identities collided: %q", resolvedA.Resolved.Bootstrap.Source.ID)
	}
	if resolvedA.Resolved.Bootstrap.Source.ManagedPath == resolvedB.Resolved.Bootstrap.Source.ManagedPath {
		t.Fatalf("managed paths collided: %q", resolvedA.Resolved.Bootstrap.Source.ManagedPath)
	}
}

func TestResolveMenubarProvidersConfigRecoveryModes(t *testing.T) {
	configDir := stubUserConfigDir(t)
	bootstrapPath := filepath.Join(t.TempDir(), "providers.json")
	selected := menubarConfig{ProvidersConfigPath: bootstrapPath}
	writeMenubarStartupBootstrap(t, bootstrapPath, "bootstrap-before")
	managedPath := commitMenubarManagedOverride(t, bootstrapPath, configDir, "managed")
	writeMenubarStartupBootstrap(t, bootstrapPath, "bootstrap-after")

	_, err := resolveMenubarProvidersConfig(selected, proxy.ProvidersConfigUseManaged)
	var configErr *proxy.ConfigError
	if !errors.As(err, &configErr) || configErr.Code != proxy.ConfigErrorManagedSourceConflict {
		t.Fatalf("use-managed conflict error = %v, want managed source conflict", err)
	}

	ignored, err := resolveMenubarProvidersConfig(selected, proxy.ProvidersConfigIgnoreManaged)
	if err != nil {
		t.Fatalf("ignore-managed error = %v", err)
	}
	if got := ignored.Resolved.Config.Providers[0].ID; got != "bootstrap-after" {
		t.Fatalf("ignore-managed provider = %q, want bootstrap-after", got)
	}
	if ignored.Store != nil || ignored.ReadOnlyReason == nil {
		t.Fatalf("ignore-managed startup = %+v, want read-only bootstrap", ignored)
	}
	if _, err := os.Stat(managedPath); err != nil {
		t.Fatalf("ignore-managed should preserve override: %v", err)
	}

	reset, err := resolveMenubarProvidersConfig(selected, proxy.ProvidersConfigResetManaged)
	if err != nil {
		t.Fatalf("reset-managed error = %v", err)
	}
	if got := reset.Resolved.Config.Providers[0].ID; got != "bootstrap-after" {
		t.Fatalf("reset-managed provider = %q, want bootstrap-after", got)
	}
	if reset.Store == nil || reset.ReadOnlyReason != nil {
		t.Fatalf("reset-managed startup = %+v, want writable bootstrap", reset)
	}
	if selected.ProvidersConfigPath != bootstrapPath {
		t.Fatalf("reset changed selected bootstrap path = %q, want %q", selected.ProvidersConfigPath, bootstrapPath)
	}
	if _, err := os.Stat(managedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("reset-managed override remains: %v", err)
	}
}

func TestResolveMenubarProvidersConfigReadOnlyFallback(t *testing.T) {
	bootstrapPath := filepath.Join(t.TempDir(), "providers.json")
	writeMenubarStartupBootstrap(t, bootstrapPath, "bootstrap")

	probeErr := errors.New("read-only filesystem")
	startup, err := resolveProvidersConfigForStartup(
		bootstrapPath,
		proxy.ProvidersConfigUseManaged,
		func() (string, error) { return t.TempDir(), nil },
		func(string) error { return probeErr },
	)
	if err != nil {
		t.Fatalf("resolveProvidersConfigForStartup() error = %v", err)
	}
	if startup.Store != nil || !errors.Is(startup.ReadOnlyReason, probeErr) {
		t.Fatalf("startup = %+v, want read-only persistence error", startup)
	}
	if got := startup.Resolved.Config.Providers[0].ID; got != "bootstrap" {
		t.Fatalf("startup provider = %q, want bootstrap", got)
	}
}

func writeMenubarStartupBootstrap(t *testing.T, path, providerID string) {
	t.Helper()
	body := []byte(`{"providers":[{"id":"` + providerID + `","type":"copilot","default":true}]}`)
	if err := os.WriteFile(path, body, 0o600); err != nil {
		t.Fatalf("write bootstrap %q: %v", path, err)
	}
}

func commitMenubarManagedOverride(t *testing.T, bootstrapPath, configDir, providerID string) string {
	t.Helper()
	bootstrap, err := proxy.LoadProvidersConfigBootstrap(bootstrapPath, configDir)
	if err != nil {
		t.Fatalf("LoadProvidersConfigBootstrap() error = %v", err)
	}
	store, err := proxy.NewManagedProvidersConfigStore(bootstrap)
	if err != nil {
		t.Fatalf("NewManagedProvidersConfigStore() error = %v", err)
	}
	managed := proxy.ProvidersConfig{Providers: []proxy.ProviderConfig{{ID: providerID, Type: "copilot", Default: true}}}
	if _, err := store.Commit(t.Context(), bootstrap.Revision, managed); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	return store.Path()
}
