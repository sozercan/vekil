package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/sozercan/vekil/proxy"
)

const menubarConfigFilename = "menubar.json"

var (
	userConfigDir          = os.UserConfigDir
	errMenubarConfigLoad   = errors.New("menubar config load failed")
	errProvidersConfigLoad = errors.New("providers config load failed")
)

type menubarConfig struct {
	ProvidersConfigPath string `json:"providers_config_path,omitempty"`
}

type providersConfigStartup struct {
	Resolved       proxy.ResolvedProvidersConfig
	Store          *proxy.ManagedProvidersConfigStore
	ReadOnlyReason error
}

func menubarConfigPath() (string, error) {
	dir, err := userConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "vekil", menubarConfigFilename), nil
}

func loadMenubarConfig() (menubarConfig, error) {
	path, err := menubarConfigPath()
	if err != nil {
		return menubarConfig{}, err
	}

	body, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return menubarConfig{}, nil
		}
		return menubarConfig{}, fmt.Errorf("read menubar config %q: %w", path, err)
	}

	var cfg menubarConfig
	if err := json.Unmarshal(body, &cfg); err != nil {
		return menubarConfig{}, fmt.Errorf("decode menubar config %q: %w", path, err)
	}

	cfg.ProvidersConfigPath = strings.TrimSpace(cfg.ProvidersConfigPath)
	return cfg, nil
}

func saveMenubarConfig(cfg menubarConfig) error {
	path, err := menubarConfigPath()
	if err != nil {
		return err
	}

	cfg.ProvidersConfigPath = strings.TrimSpace(cfg.ProvidersConfigPath)
	if cfg.ProvidersConfigPath == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove menubar config %q: %w", path, err)
		}
		return nil
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("create menubar config dir: %w", err)
	}

	body, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode menubar config: %w", err)
	}

	body = append(body, '\n')
	if err := os.WriteFile(path, body, 0o644); err != nil {
		return fmt.Errorf("write menubar config %q: %w", path, err)
	}

	return nil
}

func loadProvidersConfigForMenubar() (menubarConfig, providersConfigStartup, error) {
	cfg, err := loadMenubarConfig()
	if err != nil {
		return menubarConfig{}, providersConfigStartup{}, fmt.Errorf("%w: %w", errMenubarConfigLoad, err)
	}

	providers, err := resolveMenubarProvidersConfig(cfg, proxy.ProvidersConfigUseManaged)
	if err != nil {
		return cfg, providersConfigStartup{}, fmt.Errorf("%w: %w", errProvidersConfigLoad, err)
	}

	return cfg, providers, nil
}

func resolveMenubarProvidersConfig(cfg menubarConfig, mode proxy.ProvidersConfigResolveMode) (providersConfigStartup, error) {
	return resolveProvidersConfigForStartup(cfg.ProvidersConfigPath, mode, userConfigDir, probeManagedProvidersConfigWritable)
}

func resolveProvidersConfigForStartup(
	bootstrapPath string,
	mode proxy.ProvidersConfigResolveMode,
	getUserConfigDir func() (string, error),
	probeWritable func(string) error,
) (providersConfigStartup, error) {
	if getUserConfigDir == nil {
		getUserConfigDir = os.UserConfigDir
	}
	if probeWritable == nil {
		probeWritable = probeManagedProvidersConfigWritable
	}
	if mode == "" {
		mode = proxy.ProvidersConfigUseManaged
	}

	configDir, err := getUserConfigDir()
	if err != nil {
		if mode == proxy.ProvidersConfigResetManaged {
			return providersConfigStartup{}, fmt.Errorf("reset managed providers config: resolve user config directory: %w", err)
		}
		resolved, bootstrapErr := resolveReadOnlyProvidersConfigBootstrap(bootstrapPath)
		if bootstrapErr != nil {
			return providersConfigStartup{}, bootstrapErr
		}
		return providersConfigStartup{
			Resolved:       resolved,
			ReadOnlyReason: fmt.Errorf("resolve managed providers config directory: %w", err),
		}, nil
	}

	resolved, err := proxy.ResolveProvidersConfig(proxy.ProvidersConfigResolveOptions{
		BootstrapPath: bootstrapPath,
		UserConfigDir: configDir,
		Mode:          mode,
	})
	if err != nil {
		return providersConfigStartup{}, err
	}

	startup := providersConfigStartup{Resolved: resolved}
	store, err := proxy.NewManagedProvidersConfigStore(resolved.Bootstrap)
	if err != nil {
		var configErr *proxy.ConfigError
		if errors.As(err, &configErr) && configErr.Code == proxy.ConfigErrorManagedStore {
			startup.ReadOnlyReason = err
			return startup, nil
		}
		return providersConfigStartup{}, err
	}
	if mode == proxy.ProvidersConfigIgnoreManaged {
		startup.ReadOnlyReason = errors.New("managed providers config override is ignored for this startup")
		return startup, nil
	}
	if err := probeWritable(store.Path()); err != nil {
		startup.ReadOnlyReason = fmt.Errorf("managed providers config persistence is unavailable: %w", err)
		return startup, nil
	}
	startup.Store = store
	return startup, nil
}

func resolveReadOnlyProvidersConfigBootstrap(bootstrapPath string) (proxy.ResolvedProvidersConfig, error) {
	resolved, err := proxy.ResolveProvidersConfig(proxy.ProvidersConfigResolveOptions{
		BootstrapPath: bootstrapPath,
		UserConfigDir: filepath.Join(os.TempDir(), "vekil-read-only-dashboard-config"),
		Mode:          proxy.ProvidersConfigIgnoreManaged,
	})
	if err != nil {
		return proxy.ResolvedProvidersConfig{}, err
	}
	resolved.Bootstrap.Source.ManagedPath = ""
	return resolved, nil
}

func probeManagedProvidersConfigWritable(path string) error {
	return proxy.ProbeManagedProvidersConfigWritable(path)
}

func isMenubarConfigLoadError(err error) bool {
	return errors.Is(err, errMenubarConfigLoad)
}
