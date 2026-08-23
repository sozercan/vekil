package main

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/sozercan/vekil/proxy"
)

const (
	menubarConfigFilename       = "menubar.json"
	legacyCopilotConfigRevision = "cfg_legacy_copilot"
)

var (
	userConfigDir          = os.UserConfigDir
	errMenubarConfigLoad   = errors.New("menubar config load failed")
	errProvidersConfigLoad = errors.New("providers config load failed")
)

type menubarConfig struct {
	ProvidersConfigPath    string `json:"providers_config_path,omitempty"`
	selectedConfigRevision string
	versionedState         map[string]json.RawMessage
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

	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return menubarConfig{}, fmt.Errorf("decode menubar config %q: %w", path, err)
	}
	var probe struct {
		Version             int    `json:"version"`
		ConfigMode          string `json:"config_mode"`
		SelectedPath        string `json:"selected_path"`
		SelectedRevision    string `json:"selected_config_revision"`
		ProvidersConfigPath string `json:"providers_config_path"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return menubarConfig{}, fmt.Errorf("decode menubar config %q: %w", path, err)
	}
	cfg := menubarConfig{ProvidersConfigPath: strings.TrimSpace(probe.ProvidersConfigPath)}
	if probe.Version > 0 {
		cfg.versionedState = raw
		if strings.TrimSpace(probe.ConfigMode) == "external" {
			cfg.ProvidersConfigPath = strings.TrimSpace(probe.SelectedPath)
			cfg.selectedConfigRevision = strings.TrimSpace(probe.SelectedRevision)
			if cfg.ProvidersConfigPath == "" {
				cfg.ProvidersConfigPath = strings.TrimSpace(probe.ProvidersConfigPath)
			}
		} else {
			cfg.ProvidersConfigPath = ""
		}
	}
	return cfg, nil
}

func saveMenubarConfig(cfg menubarConfig) error {
	return saveMenubarConfigWithRecovery(cfg, false)
}

func saveMenubarConfigReplacingUnreadable(cfg menubarConfig) error {
	return saveMenubarConfigWithRecovery(cfg, true)
}

func saveMenubarConfigWithRecovery(cfg menubarConfig, replaceUnreadable bool) error {
	path, err := menubarConfigPath()
	if err != nil {
		return err
	}

	cfg.ProvidersConfigPath = strings.TrimSpace(cfg.ProvidersConfigPath)
	if cfg.versionedState == nil {
		existing, err := loadMenubarConfig()
		if err != nil {
			if !replaceUnreadable {
				return fmt.Errorf("preserve existing menubar config: %w", err)
			}
			if err := ensureMenubarConfigDirectory(path); err != nil {
				return err
			}
			if err := backupUnreadableMenubarConfig(path); err != nil {
				return err
			}
		} else {
			cfg.versionedState = existing.versionedState
			if cfg.selectedConfigRevision == "" && cfg.ProvidersConfigPath == existing.ProvidersConfigPath {
				cfg.selectedConfigRevision = existing.selectedConfigRevision
			}
		}
	}
	if cfg.versionedState == nil && cfg.ProvidersConfigPath == "" {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove menubar config %q: %w", path, err)
		}
		return nil
	}

	if err := ensureMenubarConfigDirectory(path); err != nil {
		return err
	}

	var body []byte
	if cfg.versionedState != nil {
		raw := make(map[string]json.RawMessage, len(cfg.versionedState)+3)
		for key, value := range cfg.versionedState {
			raw[key] = append(json.RawMessage(nil), value...)
		}
		set := func(key string, value any) error {
			encoded, err := json.Marshal(value)
			if err != nil {
				return err
			}
			raw[key] = encoded
			return nil
		}
		if cfg.ProvidersConfigPath == "" {
			_ = set("config_mode", "legacy")
			delete(raw, "selected_path")
			delete(raw, "providers_config_path")
			_ = set("selected_config_revision", legacyCopilotConfigRevision)
		} else {
			_ = set("config_mode", "external")
			_ = set("selected_path", cfg.ProvidersConfigPath)
			_ = set("providers_config_path", cfg.ProvidersConfigPath)
			if cfg.selectedConfigRevision == "" {
				delete(raw, "selected_config_revision")
			} else {
				_ = set("selected_config_revision", cfg.selectedConfigRevision)
			}
		}
		body, err = json.MarshalIndent(raw, "", "  ")
	} else {
		body, err = json.MarshalIndent(cfg, "", "  ")
	}
	if err != nil {
		return fmt.Errorf("encode menubar config: %w", err)
	}
	body = append(body, '\n')
	return writeMenubarConfigAtomic(path, body)
}

func ensureMenubarConfigDirectory(path string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create menubar config dir: %w", err)
	}
	if err := os.Chmod(dir, 0o700); err != nil {
		return fmt.Errorf("protect menubar config dir: %w", err)
	}
	return nil
}

func backupUnreadableMenubarConfig(path string) error {
	body, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	digest := sha256.Sum256(body)
	backupPath := fmt.Sprintf("%s.unreadable-%x.bak", path, digest[:8])
	if err := writeMenubarConfigAtomic(backupPath, append([]byte(nil), body...)); err != nil {
		return fmt.Errorf("backup unreadable menubar config %q: %w", path, err)
	}
	return nil
}

func writeMenubarConfigAtomic(path string, body []byte) (returnErr error) {
	dir := filepath.Dir(path)
	file, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("create temporary menubar config for %q: %w", path, err)
	}
	tmp := file.Name()
	defer func() {
		_ = file.Close()
		_ = os.Remove(tmp)
	}()
	if err := file.Chmod(0o600); err != nil {
		return fmt.Errorf("secure temporary menubar config for %q: %w", path, err)
	}
	for len(body) > 0 {
		n, err := file.Write(body)
		if err != nil {
			return fmt.Errorf("write temporary menubar config for %q: %w", path, err)
		}
		if n == 0 {
			return fmt.Errorf("write temporary menubar config for %q: %w", path, io.ErrShortWrite)
		}
		body = body[n:]
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync temporary menubar config for %q: %w", path, err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close temporary menubar config for %q: %w", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("install menubar config %q: %w", path, err)
	}
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("open menubar config directory %q: %w", dir, err)
	}
	defer func() { _ = directory.Close() }()
	if err := directory.Sync(); err != nil && !errors.Is(err, os.ErrInvalid) {
		return fmt.Errorf("sync menubar config directory %q: %w", dir, err)
	}
	return nil
}

func loadProvidersConfigForMenubar() (menubarConfig, proxy.ProvidersConfig, error) {
	cfg, err := loadMenubarConfig()
	if err != nil {
		return menubarConfig{}, proxy.ProvidersConfig{}, fmt.Errorf("%w: %w", errMenubarConfigLoad, err)
	}

	providersCfg, err := proxy.LoadProvidersConfigFile(cfg.ProvidersConfigPath)
	if err != nil {
		return cfg, proxy.ProvidersConfig{}, fmt.Errorf("%w: %w", errProvidersConfigLoad, err)
	}

	return cfg, providersCfg, nil
}

func isMenubarConfigLoadError(err error) bool {
	return errors.Is(err, errMenubarConfigLoad)
}
