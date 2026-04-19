package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

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
