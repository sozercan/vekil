package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
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
	info, err := os.Stat(filepath.Join(configDir, "vekil", menubarConfigFilename))
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("saved config permissions = %#o, want 0600", got)
	}
}

func TestSaveMenubarConfigSecuresExistingFile(t *testing.T) {
	configDir := stubUserConfigDir(t)
	configPath := filepath.Join(configDir, "vekil", menubarConfigFilename)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(configPath, []byte("{\"providers_config_path\":\"/tmp/old.yaml\"}\n"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.Chmod(configPath, 0o644); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	before, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("Stat(before) error = %v", err)
	}

	wantPath := "https://config.example/providers.yaml?token=secret"
	if err := saveMenubarConfig(menubarConfig{ProvidersConfigPath: wantPath}); err != nil {
		t.Fatalf("saveMenubarConfig() error = %v", err)
	}

	info, err := os.Stat(configPath)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("saved config permissions = %#o, want 0600", got)
	}
	if runtime.GOOS != "windows" && os.SameFile(before, info) {
		t.Fatal("saveMenubarConfig() rewrote the existing inode instead of replacing it atomically")
	}
	cfg, err := loadMenubarConfig()
	if err != nil {
		t.Fatalf("loadMenubarConfig() error = %v", err)
	}
	if cfg.ProvidersConfigPath != wantPath {
		t.Fatalf("loadMenubarConfig() ProvidersConfigPath = %q, want %q", cfg.ProvidersConfigPath, wantPath)
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

		cfg, providersCfg, err := loadProvidersConfigForMenubar()
		if err != nil {
			t.Fatalf("loadProvidersConfigForMenubar() error = %v", err)
		}
		if cfg.ProvidersConfigPath != "" {
			t.Fatalf("loadProvidersConfigForMenubar() ProvidersConfigPath = %q, want empty", cfg.ProvidersConfigPath)
		}
		if len(providersCfg.Providers) != 0 {
			t.Fatalf("loadProvidersConfigForMenubar() Providers = %v, want empty", providersCfg.Providers)
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
		body := []byte(`{"providers":[{"id":"azure","type":"azure-openai"}]}`)
		if err := os.WriteFile(providersPath, body, 0o644); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
		if err := saveMenubarConfig(menubarConfig{ProvidersConfigPath: providersPath}); err != nil {
			t.Fatalf("saveMenubarConfig() error = %v", err)
		}

		cfg, providersCfg, err := loadProvidersConfigForMenubar()
		if err != nil {
			t.Fatalf("loadProvidersConfigForMenubar() error = %v", err)
		}
		if cfg.ProvidersConfigPath != providersPath {
			t.Fatalf("loadProvidersConfigForMenubar() ProvidersConfigPath = %q, want %q", cfg.ProvidersConfigPath, providersPath)
		}
		if len(providersCfg.Providers) != 1 || providersCfg.Providers[0].ID != "azure" {
			t.Fatalf("loadProvidersConfigForMenubar() Providers = %v, want azure provider", providersCfg.Providers)
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

	menubarCfg = menubarConfig{ProvidersConfigPath: "https://source-user:source-password@example.com/providers.yaml?signature=signed-secret#fragment-secret"}
	providersConfigErr = nil
	if got := providersMenuTitle(); got != "Providers: providers.yaml" {
		t.Fatalf("providersMenuTitle() with remote source = %q, want %q", got, "Providers: providers.yaml")
	}
	providersConfigErr = providersErr
	if got := providersMenuTitle(); got != "Providers: Invalid (providers.yaml)" {
		t.Fatalf("providersMenuTitle() with invalid remote source = %q, want %q", got, "Providers: Invalid (providers.yaml)")
	}
}

func TestLogProvidersConfigLoadErrorRedactsRemoteSource(t *testing.T) {
	var output bytes.Buffer
	previousLog := log
	previousCfg := menubarCfg
	t.Cleanup(func() {
		log = previousLog
		menubarCfg = previousCfg
	})

	log = logger.NewWithWriter(logger.LevelError, &output)
	menubarCfg = menubarConfig{ProvidersConfigPath: "https://source-user:source-password@example.com/providers.yaml?signature=signed-secret#fragment-secret"}
	logProvidersConfigLoadError(errors.Join(errProvidersConfigLoad, errors.New("load failed")))

	var entry map[string]any
	if err := json.Unmarshal(output.Bytes(), &entry); err != nil {
		t.Fatalf("Unmarshal(log) error = %v; output=%q", err, output.String())
	}
	if got := entry["path"]; got != "https://example.com/providers.yaml" {
		t.Fatalf("logged providers config path = %v, want sanitized source", got)
	}
	for _, secret := range []string{"source-user", "source-password", "signed-secret", "fragment-secret"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("providers config log exposes %q: %s", secret, output.String())
		}
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

func TestVersionedNativeStateIsPreservedByForwardRevertEdits(t *testing.T) {
	configDir := stubUserConfigDir(t)
	configPath := filepath.Join(configDir, "vekil", menubarConfigFilename)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	initial := `{
  "version": 1,
  "config_mode": "managed",
  "managed_ownership_id": "owner-123",
  "committed_config_revision": "cfg_managed",
  "secret_generation": 7,
  "providers": [{"uuid":"provider-uuid","provider_id":"copilot"}]
}`
	if err := os.WriteFile(configPath, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := saveMenubarConfig(menubarConfig{
		ProvidersConfigPath:    "/tmp/external.yaml",
		selectedConfigRevision: "cfg_external",
	}); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var state map[string]any
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	if state["managed_ownership_id"] != "owner-123" || state["secret_generation"] != float64(7) || state["config_mode"] != "external" || state["selected_path"] != "/tmp/external.yaml" || state["selected_config_revision"] != "cfg_external" {
		t.Fatalf("versioned state was not preserved: %s", body)
	}

	if err := saveMenubarConfig(menubarConfig{}); err != nil {
		t.Fatal(err)
	}
	body, err = os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(body, &state); err != nil {
		t.Fatal(err)
	}
	if state["managed_ownership_id"] != "owner-123" || state["config_mode"] != "legacy" {
		t.Fatalf("clear destroyed native ownership state: %s", body)
	}
}

func TestSaveMenubarConfigReplacingUnreadablePreservesBackup(t *testing.T) {
	configDir := stubUserConfigDir(t)
	configPath := filepath.Join(configDir, "vekil", menubarConfigFilename)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	unreadable := []byte("{\n")
	if err := os.WriteFile(configPath, unreadable, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := saveMenubarConfigReplacingUnreadable(menubarConfig{
		ProvidersConfigPath:    "/tmp/recovered.yaml",
		selectedConfigRevision: "cfg_recovered",
	}); err != nil {
		t.Fatal(err)
	}

	cfg, err := loadMenubarConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProvidersConfigPath != "/tmp/recovered.yaml" {
		t.Fatalf("recovered path = %q", cfg.ProvidersConfigPath)
	}

	backups, err := filepath.Glob(configPath + ".unreadable-*.bak")
	if err != nil {
		t.Fatal(err)
	}
	if len(backups) != 1 {
		t.Fatalf("unreadable backups = %v, want one", backups)
	}
	backup, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(backup, unreadable) {
		t.Fatalf("backup = %q, want %q", backup, unreadable)
	}
}

func TestSaveMenubarConfigReplacingUnreadablePreservesStateOnInitialReadFailure(t *testing.T) {
	configDir := stubUserConfigDir(t)
	configPath := filepath.Join(configDir, "vekil", menubarConfigFilename)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("{\"version\":1,\"config_mode\":\"managed\",\"managed_ownership_id\":\"owner-123\"}\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	previousRead := readMenubarConfigFile
	readMenubarConfigFile = func(string) ([]byte, error) {
		return nil, os.ErrPermission
	}
	t.Cleanup(func() { readMenubarConfigFile = previousRead })

	err := saveMenubarConfigReplacingUnreadable(menubarConfig{
		ProvidersConfigPath:    "/tmp/recovered.yaml",
		selectedConfigRevision: "cfg_recovered",
	})
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("saveMenubarConfigReplacingUnreadable() error = %v, want permission error", err)
	}
	body, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(body, original) {
		t.Fatalf("config after failed recovery = %q, want %q", body, original)
	}
}

func TestSaveMenubarConfigReplacingUnreadablePreservesStateOnBackupReadFailure(t *testing.T) {
	configDir := stubUserConfigDir(t)
	configPath := filepath.Join(configDir, "vekil", menubarConfigFilename)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	original := []byte("{\n")
	if err := os.WriteFile(configPath, original, 0o600); err != nil {
		t.Fatal(err)
	}

	previousRead := readMenubarConfigFile
	readCount := 0
	readMenubarConfigFile = func(string) ([]byte, error) {
		readCount++
		if readCount == 1 {
			return append([]byte(nil), original...), nil
		}
		return nil, os.ErrPermission
	}
	t.Cleanup(func() { readMenubarConfigFile = previousRead })

	err := saveMenubarConfigReplacingUnreadable(menubarConfig{
		ProvidersConfigPath:    "/tmp/recovered.yaml",
		selectedConfigRevision: "cfg_recovered",
	})
	if !errors.Is(err, os.ErrPermission) {
		t.Fatalf("saveMenubarConfigReplacingUnreadable() error = %v, want permission error", err)
	}
	body, readErr := os.ReadFile(configPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !bytes.Equal(body, original) {
		t.Fatalf("config after failed backup = %q, want %q", body, original)
	}
}
