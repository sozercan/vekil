package macosruntime

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/sozercan/vekil/internal/appcontrol"
	"github.com/sozercan/vekil/proxy"
	"gopkg.in/yaml.v3"
)

const LegacyConfigRevision = "cfg_legacy_copilot"

var initialManagedCopilotYAML = []byte("schema_version: 2\nproviders:\n  - id: copilot\n    type: copilot\n    default: true\n")

var errRemoteConfigurationNotLoaded = errors.New("remote configuration snapshot is not loaded")

// Paths contains all helper-owned persistence paths. Staged and backup files
// are transaction-private and contain only secret-free managed YAML.
type Paths struct {
	Directory string
	Managed   string
	State     string
	Journal   string
	Staged    string
	Backup    string
	Lock      string
}

// PathsInDirectory returns the canonical filenames under directory.
func PathsInDirectory(directory string) Paths {
	return Paths{
		Directory: directory,
		Managed:   filepath.Join(directory, "providers.yaml"),
		State:     filepath.Join(directory, "menubar.json"),
		Journal:   filepath.Join(directory, "managed-apply.json"),
		Staged:    filepath.Join(directory, ".providers.yaml.staged"),
		Backup:    filepath.Join(directory, ".providers.yaml.previous"),
		Lock:      filepath.Join(directory, ".runtime.lock"),
	}
}

// DefaultPaths uses the platform user configuration directory. On macOS this
// resolves to ~/Library/Application Support/vekil.
func DefaultPaths() (Paths, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return Paths{}, err
	}
	return PathsInDirectory(filepath.Join(dir, "vekil")), nil
}

// ConfigManagerOptions configures helper-owned storage.
type ConfigManagerOptions struct {
	Paths Paths
	UUID  func() string
}

// ConfigManager owns configuration selection, safe snapshot reads, revisions,
// managed-file ownership, and crash recovery metadata.
type ConfigManager struct {
	mu sync.Mutex

	paths                 Paths
	uuid                  func() string
	state                 PersistentState
	stagedSelection       *PersistentState
	selectionSwitchActive bool
	// The last snapshot represents the currently selected external runtime.
	// Prepared and rollback snapshots exist only during one selection switch.
	lastExternalSnapshot     *externalConfigurationSnapshot
	preparedExternalSnapshot *externalConfigurationSnapshot
	rollbackExternalSnapshot *externalConfigurationSnapshot
}

type externalConfigurationSnapshot struct {
	path     string
	revision string
	body     []byte
}

func loadExternalConfigurationSnapshot(ctx context.Context, source string) (proxy.ProvidersConfig, []byte, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return proxy.ProvidersConfig{}, nil, err
	}
	if proxy.IsRemoteProvidersConfigSource(source) {
		return proxy.LoadProvidersConfigSnapshot(ctx, source)
	}
	body, _, err := readSecureFile(source, MaxConfigBytes)
	if err != nil {
		return proxy.ProvidersConfig{}, nil, err
	}
	cfg, err := proxy.LoadProvidersConfigBytes(source, body)
	if err != nil {
		return proxy.ProvidersConfig{}, nil, err
	}
	return cfg, body, nil
}

func cloneExternalConfigurationSnapshot(snapshot *externalConfigurationSnapshot) *externalConfigurationSnapshot {
	if snapshot == nil {
		return nil
	}
	return &externalConfigurationSnapshot{
		path:     snapshot.path,
		revision: snapshot.revision,
		body:     append([]byte(nil), snapshot.body...),
	}
}

func (s *externalConfigurationSnapshot) matches(path, revision string) bool {
	return s != nil && s.path == strings.TrimSpace(path) && s.revision == revision
}

// NewConfigManager loads state and resolves any incomplete apply journal before
// accepting commands.
func NewConfigManager(opts ConfigManagerOptions) (*ConfigManager, error) {
	paths, err := resolvePaths(opts.Paths)
	if err != nil {
		return nil, err
	}
	if opts.UUID == nil {
		opts.UUID = uuid.NewString
	}
	if err := ensurePrivateDirectory(paths.Directory); err != nil {
		return nil, err
	}
	manager := &ConfigManager{paths: paths, uuid: opts.UUID}
	state, migrated, err := loadPersistentState(paths.State)
	if err != nil {
		return nil, err
	}
	manager.state = state
	if err := manager.recoverApplyJournalLocked(); err != nil {
		return nil, err
	}
	activationReset := manager.state.ActiveRuntimeRevision != "" || manager.state.ActiveRuntimeGeneration != 0
	manager.state.ActiveRuntimeRevision = ""
	manager.state.ActiveRuntimeGeneration = 0
	if migrated || activationReset {
		if err := manager.saveStateLocked(); err != nil {
			return nil, err
		}
	}
	return manager, nil
}

func resolvePaths(paths Paths) (Paths, error) {
	if strings.TrimSpace(paths.Directory) == "" {
		return DefaultPaths()
	}
	defaults := PathsInDirectory(paths.Directory)
	if paths.Managed == "" {
		paths.Managed = defaults.Managed
	}
	if paths.State == "" {
		paths.State = defaults.State
	}
	if paths.Journal == "" {
		paths.Journal = defaults.Journal
	}
	if paths.Staged == "" {
		paths.Staged = defaults.Staged
	}
	if paths.Backup == "" {
		paths.Backup = defaults.Backup
	}
	if paths.Lock == "" {
		paths.Lock = defaults.Lock
	}
	return paths, nil
}

// Paths returns the configured persistence paths.
func (m *ConfigManager) Paths() Paths { return m.paths }

// State returns a deep copy of helper-owned state.
func (m *ConfigManager) State() PersistentState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return clonePersistentState(m.state)
}

func (m *ConfigManager) recoveryRequiredLocked() error {
	if m.state.RecoveryState == "" {
		return nil
	}
	return fmt.Errorf("managed recovery is required: %s", m.state.RecoveryState)
}

// LoadConfiguration implements appcontrol.ConfigurationSource using one safe,
// immutable byte snapshot.
func (m *ConfigManager) LoadConfiguration(ctx context.Context) (appcontrol.Configuration, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return appcontrol.Configuration{}, err
	}
	m.mu.Lock()
	state := clonePersistentState(m.state)
	preparedExternal := cloneExternalConfigurationSnapshot(m.preparedExternalSnapshot)
	m.mu.Unlock()

	switch state.ConfigMode {
	case ConfigModeLegacy:
		return appcontrol.Configuration{Revision: LegacyConfigRevision, Value: proxy.ProvidersConfig{}}, nil
	case ConfigModeManaged:
		if state.ManagedOwnershipID == "" {
			return appcontrol.Configuration{}, errors.New("managed configuration ownership is missing")
		}
		body, _, err := readOwnedFile(m.paths.Managed, MaxConfigBytes)
		if err != nil {
			return appcontrol.Configuration{}, err
		}
		revision, digest := configRevision(body)
		if state.CommittedSHA256 == "" || digest != state.CommittedSHA256 || revision != state.CommittedConfigRevision {
			return appcontrol.Configuration{}, errors.New("managed configuration is drifted")
		}
		cfg, err := proxy.LoadProvidersConfigBytes(m.paths.Managed, body)
		if err != nil {
			return appcontrol.Configuration{}, err
		}
		return appcontrol.Configuration{Revision: revision, SecretGeneration: state.SecretGeneration, Value: cfg}, nil
	case ConfigModeExternal:
		path := strings.TrimSpace(state.SelectedPath)
		if path == "" {
			return appcontrol.Configuration{}, errors.New("external configuration path is missing")
		}
		var cfg proxy.ProvidersConfig
		var body []byte
		if preparedExternal.matches(path, state.SelectedConfigRevision) {
			body = append([]byte(nil), preparedExternal.body...)
			var err error
			cfg, err = proxy.LoadProvidersConfigBytes(path, body)
			if err != nil {
				return appcontrol.Configuration{}, err
			}
		} else {
			var err error
			cfg, body, err = loadExternalConfigurationSnapshot(ctx, path)
			if err != nil {
				return appcontrol.Configuration{}, err
			}
		}
		revision, _ := configRevision(body)
		if revision != state.SelectedConfigRevision {
			return appcontrol.Configuration{}, errors.New("external configuration changed; reload is required")
		}
		m.mu.Lock()
		m.lastExternalSnapshot = &externalConfigurationSnapshot{
			path:     path,
			revision: revision,
			body:     append([]byte(nil), body...),
		}
		m.mu.Unlock()
		return appcontrol.Configuration{Revision: revision, Value: cfg}, nil
	default:
		return appcontrol.Configuration{}, fmt.Errorf("unsupported config mode %q", state.ConfigMode)
	}
}

// RuntimeActivated implements appcontrol.ConfigurationObserver.
func (m *ConfigManager) RuntimeActivated(_ context.Context, cfg appcontrol.Configuration, generation uint64, _ string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.ConfigMode == ConfigModeExternal {
		m.state.SelectedConfigRevision = cfg.Revision
	}
	m.state.ActiveRuntimeRevision = cfg.Revision
	m.state.ActiveRuntimeGeneration = generation
	return m.saveStateLocked()
}

// RuntimeDeactivated implements appcontrol.ConfigurationObserver.
func (m *ConfigManager) RuntimeDeactivated(_ context.Context, generation uint64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.ActiveRuntimeGeneration != 0 && m.state.ActiveRuntimeGeneration != generation {
		return nil
	}
	m.state.ActiveRuntimeRevision = ""
	m.state.ActiveRuntimeGeneration = 0
	return m.saveStateLocked()
}

// StageExternal validates but never writes the selected user-owned file. The
// selection remains process-local until CommitSelection, so a helper crash
// before startup/readiness leaves the previously committed selection intact.
func (m *ConfigManager) StageExternal(ctx context.Context, path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("external configuration path is required")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	_, body, err := loadExternalConfigurationSnapshot(ctx, path)
	if err != nil {
		return err
	}
	revision, _ := configRevision(body)

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.recoveryRequiredLocked(); err != nil {
		return err
	}
	if m.stagedSelection != nil {
		return errors.New("a configuration selection is already staged")
	}
	previous := clonePersistentState(m.state)
	m.stagedSelection = &previous
	if !m.selectionSwitchActive {
		m.rollbackExternalSnapshot = nil
	}
	m.preparedExternalSnapshot = &externalConfigurationSnapshot{
		path:     path,
		revision: revision,
		body:     append([]byte(nil), body...),
	}
	m.state.ConfigMode = ConfigModeExternal
	m.state.SelectedPath = path
	m.state.SelectedConfigRevision = revision
	return nil
}

// SelectExternal is the stopped/offline convenience path. Helper operations
// use StageExternal and commit only after any required restart becomes ready.
func (m *ConfigManager) SelectExternal(ctx context.Context, path string) (ConfigDescription, error) {
	if err := m.StageExternal(ctx, path); err != nil {
		return ConfigDescription{}, err
	}
	if err := m.CommitSelection(); err != nil {
		return ConfigDescription{}, err
	}
	return m.Describe()
}

func (m *ConfigManager) StageReloadExternal(ctx context.Context) error {
	m.mu.Lock()
	if err := m.recoveryRequiredLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	if m.state.ConfigMode != ConfigModeExternal || strings.TrimSpace(m.state.SelectedPath) == "" {
		m.mu.Unlock()
		return errors.New("external configuration is not selected")
	}
	path := m.state.SelectedPath
	m.mu.Unlock()
	return m.StageExternal(ctx, path)
}

// ReloadExternal validates and commits the current selected path immediately.
func (m *ConfigManager) ReloadExternal(ctx context.Context) (ConfigDescription, error) {
	if err := m.StageReloadExternal(ctx); err != nil {
		return ConfigDescription{}, err
	}
	if err := m.CommitSelection(); err != nil {
		return ConfigDescription{}, err
	}
	return m.Describe()
}

// UseLegacy selects zero-config Copilot without creating managed YAML.
func (m *ConfigManager) UseLegacy() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.recoveryRequiredLocked(); err != nil {
		return err
	}
	m.state.ConfigMode = ConfigModeLegacy
	m.state.SelectedPath = ""
	m.state.SelectedConfigRevision = LegacyConfigRevision
	return m.saveStateLocked()
}

func (m *ConfigManager) StagePreferredAppConfiguration() error {
	m.mu.Lock()
	if err := m.recoveryRequiredLocked(); err != nil {
		m.mu.Unlock()
		return err
	}
	if m.stagedSelection != nil {
		m.mu.Unlock()
		return errors.New("a configuration selection is already staged")
	}
	managedOwned := m.state.ManagedOwnershipID != ""
	m.mu.Unlock()

	var revision string
	if managedOwned {
		body, _, err := readOwnedFile(m.paths.Managed, MaxConfigBytes)
		if err != nil {
			return err
		}
		var digest string
		revision, digest = configRevision(body)
		m.mu.Lock()
		if revision != m.state.CommittedConfigRevision || digest != m.state.CommittedSHA256 {
			m.mu.Unlock()
			return errors.New("managed configuration is drifted")
		}
		m.mu.Unlock()
	} else {
		revision = LegacyConfigRevision
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.recoveryRequiredLocked(); err != nil {
		return err
	}
	if m.stagedSelection != nil {
		return errors.New("a configuration selection is already staged")
	}
	previous := clonePersistentState(m.state)
	m.stagedSelection = &previous
	if managedOwned {
		m.state.ConfigMode = ConfigModeManaged
		m.state.SelectedPath = m.paths.Managed
	} else {
		m.state.ConfigMode = ConfigModeLegacy
		m.state.SelectedPath = ""
	}
	m.state.SelectedConfigRevision = revision
	return nil
}

func (m *ConfigManager) CommitSelection() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.recoveryRequiredLocked(); err != nil {
		return err
	}
	if m.stagedSelection == nil {
		if err := m.saveStateLocked(); err != nil {
			return err
		}
		m.promotePreparedExternalSnapshotLocked()
		m.preparedExternalSnapshot = nil
		m.rollbackExternalSnapshot = nil
		m.pruneLastExternalSnapshotLocked()
		return nil
	}
	previous := m.stagedSelection
	m.stagedSelection = nil
	if err := m.saveStateLocked(); err != nil {
		m.stagedSelection = previous
		return err
	}
	m.promotePreparedExternalSnapshotLocked()
	m.preparedExternalSnapshot = nil
	m.rollbackExternalSnapshot = nil
	m.pruneLastExternalSnapshotLocked()
	return nil
}

// UsePreferredAppConfiguration clears External Configuration without forcing
// managed-config opt-in. It returns to an owned managed file when one exists,
// otherwise it preserves the parity-release zero-config Copilot behavior.
func (m *ConfigManager) UsePreferredAppConfiguration() error {
	if err := m.StagePreferredAppConfiguration(); err != nil {
		return err
	}
	return m.CommitSelection()
}

// RestoreSelection rolls back only configuration selection fields. Runtime
// activation metadata may have changed while a stop/start attempt ran and must
// not be rewound to a stale generation.
func (m *ConfigManager) RestoreSelection(previous PersistentState) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stagedSelection = nil
	m.state.ConfigMode = previous.ConfigMode
	m.state.SelectedPath = previous.SelectedPath
	m.state.SelectedConfigRevision = previous.SelectedConfigRevision
	m.preparedExternalSnapshot = nil
	if previous.ConfigMode == ConfigModeExternal && m.rollbackExternalSnapshot.matches(
		previous.SelectedPath, previous.SelectedConfigRevision,
	) {
		m.preparedExternalSnapshot = cloneExternalConfigurationSnapshot(m.rollbackExternalSnapshot)
	}
	m.rollbackExternalSnapshot = nil
	return m.saveStateLocked()
}

func (m *ConfigManager) beginSelectionSwitch(previous PersistentState) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.selectionSwitchActive = true
	m.preparedExternalSnapshot = nil
	m.rollbackExternalSnapshot = nil
	if previous.ConfigMode == ConfigModeExternal && m.lastExternalSnapshot.matches(
		previous.SelectedPath, previous.SelectedConfigRevision,
	) {
		m.rollbackExternalSnapshot = cloneExternalConfigurationSnapshot(m.lastExternalSnapshot)
	}
}

func (m *ConfigManager) endSelectionSwitch() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.selectionSwitchActive = false
	m.preparedExternalSnapshot = nil
	m.rollbackExternalSnapshot = nil
	m.pruneLastExternalSnapshotLocked()
}

func (m *ConfigManager) pruneLastExternalSnapshotLocked() {
	if m.state.ConfigMode != ConfigModeExternal || !m.lastExternalSnapshot.matches(
		m.state.SelectedPath, m.state.SelectedConfigRevision,
	) {
		m.lastExternalSnapshot = nil
	}
}

func (m *ConfigManager) promotePreparedExternalSnapshotLocked() {
	if m.state.ConfigMode == ConfigModeExternal &&
		proxy.IsRemoteProvidersConfigSource(m.state.SelectedPath) &&
		m.preparedExternalSnapshot.matches(
			m.state.SelectedPath, m.state.SelectedConfigRevision,
		) {
		m.lastExternalSnapshot = cloneExternalConfigurationSnapshot(m.preparedExternalSnapshot)
	}
}

// UseManaged selects an already-owned, non-drifted managed file.
func (m *ConfigManager) UseManaged() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.recoveryRequiredLocked(); err != nil {
		return err
	}
	if m.state.ManagedOwnershipID == "" {
		return errors.New("managed configuration has not been created")
	}
	body, _, err := readOwnedFile(m.paths.Managed, MaxConfigBytes)
	if err != nil {
		return err
	}
	revision, digest := configRevision(body)
	if revision != m.state.CommittedConfigRevision || digest != m.state.CommittedSHA256 {
		return errors.New("managed configuration is drifted")
	}
	m.state.ConfigMode = ConfigModeManaged
	m.state.SelectedPath = m.paths.Managed
	m.state.SelectedConfigRevision = revision
	return m.saveStateLocked()
}

// EnsureManagedConfiguration performs explicit opt-in using exclusive creation.
// A pre-existing unowned file is never adopted or overwritten.
func (m *ConfigManager) EnsureManagedConfiguration() (ConfigDescription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.recoveryRequiredLocked(); err != nil {
		return ConfigDescription{}, err
	}
	if err := m.recoverInitialManagedCreationIfPresentLocked(); err != nil {
		return ConfigDescription{}, err
	}

	if m.state.ManagedOwnershipID != "" {
		body, _, err := readOwnedFile(m.paths.Managed, MaxConfigBytes)
		if err != nil {
			return ConfigDescription{}, err
		}
		revision, digest := configRevision(body)
		if revision != m.state.CommittedConfigRevision || digest != m.state.CommittedSHA256 {
			return ConfigDescription{}, errors.New("managed configuration is drifted")
		}
		m.state.ConfigMode = ConfigModeManaged
		m.state.SelectedPath = m.paths.Managed
		m.state.SelectedConfigRevision = revision
		if err := m.saveStateLocked(); err != nil {
			return ConfigDescription{}, err
		}
		return m.describeLocked(body, nil), nil
	}

	if _, err := os.Lstat(m.paths.Managed); err == nil {
		return ConfigDescription{}, errors.New("managed configuration path already exists without matching ownership state")
	} else if !errors.Is(err, os.ErrNotExist) {
		return ConfigDescription{}, fmt.Errorf("inspect managed configuration path: %w", err)
	}
	if _, err := proxy.LoadProvidersConfigBytes(m.paths.Managed, initialManagedCopilotYAML); err != nil {
		return ConfigDescription{}, fmt.Errorf("validate initial managed configuration: %w", err)
	}
	if _, err := os.Lstat(m.paths.Journal); err == nil {
		return ConfigDescription{}, errors.New("managed apply journal already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return ConfigDescription{}, fmt.Errorf("inspect managed apply journal: %w", err)
	}
	journal := initialManagedCreationJournal()
	if err := writeApplyJournal(m.paths.Journal, journal); err != nil {
		return ConfigDescription{}, err
	}
	if err := writeExclusiveFile(m.paths.Managed, initialManagedCopilotYAML); err != nil {
		return ConfigDescription{}, errors.Join(err, removeUncommittedInitialManagedCreation(m.paths))
	}
	revision, digest := journal.NewRevision, journal.NewSHA256
	m.state.ConfigMode = ConfigModeManaged
	m.state.SelectedPath = m.paths.Managed
	m.state.SelectedConfigRevision = revision
	m.state.ManagedOwnershipID = m.uuid()
	m.state.CommittedConfigRevision = revision
	m.state.CommittedSHA256 = digest
	m.state.Providers = []ProviderIdentity{{UUID: m.uuid(), ProviderID: "copilot"}}
	if err := m.saveStateLocked(); err != nil {
		// The atomic state write may already have renamed the new file before a
		// directory sync error. Leave both durable artifacts in place so startup
		// recovery can reconcile either outcome without losing ownership.
		return ConfigDescription{}, err
	}
	if err := removePrivateFile(m.paths.Journal); err != nil {
		return ConfigDescription{}, err
	}
	return m.describeLocked(initialManagedCopilotYAML, nil), nil
}

// BuildManagedCandidate serializes a typed, provider-only draft into stable,
// secret-free YAML and validates the exact bytes.
func (m *ConfigManager) BuildManagedCandidate(draft ManagedDraft, secretGeneration uint64) (ManagedCandidate, error) {
	m.mu.Lock()
	existing := append([]ProviderIdentity(nil), m.state.Providers...)
	m.mu.Unlock()
	if secretGeneration == 0 {
		return ManagedCandidate{}, errors.New("secret generation must be greater than zero")
	}
	providers, identities, err := reconcileManagedProviders(existing, draft.Providers, secretGeneration, m.uuid)
	if err != nil {
		return ManagedCandidate{}, err
	}
	cfg := proxy.ProvidersConfig{SchemaVersion: proxy.ProvidersConfigSchemaVersion2, Providers: providers}
	body, err := marshalManagedProviders(cfg)
	if err != nil {
		return ManagedCandidate{}, err
	}
	validated, err := proxy.LoadProvidersConfigBytes(m.paths.Managed, body)
	if err != nil {
		return ManagedCandidate{}, err
	}
	revision, digest := configRevision(body)
	return ManagedCandidate{Bytes: body, Config: validated, Revision: revision, SHA256: digest, SecretGeneration: secretGeneration, Providers: identities}, nil
}

// Describe returns an allowlisted summary. Raw config and credentials never
// cross this boundary.
func (m *ConfigManager) Describe() (ConfigDescription, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var body []byte
	var readErr error
	switch m.state.ConfigMode {
	case ConfigModeLegacy:
		return m.describeLocked(nil, nil), nil
	case ConfigModeManaged:
		body, _, readErr = readOwnedFile(m.paths.Managed, MaxConfigBytes)
	case ConfigModeExternal:
		if m.lastExternalSnapshot.matches(m.state.SelectedPath, m.state.SelectedConfigRevision) {
			body = append([]byte(nil), m.lastExternalSnapshot.body...)
		} else if proxy.IsRemoteProvidersConfigSource(m.state.SelectedPath) {
			readErr = errRemoteConfigurationNotLoaded
		} else {
			_, body, readErr = loadExternalConfigurationSnapshot(context.Background(), m.state.SelectedPath)
		}
	default:
		readErr = fmt.Errorf("unsupported config mode %q", m.state.ConfigMode)
	}
	return m.describeLocked(body, readErr), nil
}

func (m *ConfigManager) describeLocked(body []byte, readErr error) ConfigDescription {
	description := ConfigDescription{
		Mode:                    m.state.ConfigMode,
		SelectedPath:            proxy.ProvidersConfigSourceDisplay(m.state.SelectedPath),
		ActiveRevision:          m.state.ActiveRuntimeRevision,
		ManagedOwnershipPresent: m.state.ManagedOwnershipID != "",
		SecretGeneration:        m.state.SecretGeneration,
		SecretProjections:       m.secretProjectionRequirementsLocked(),
	}
	if m.state.ConfigMode == ConfigModeExternal {
		description.SelectedRevision = m.state.SelectedConfigRevision
	}
	if m.state.ConfigMode == ConfigModeLegacy {
		description.Available = true
		description.SelectedRevision = LegacyConfigRevision
		description.Drifted = description.ActiveRevision != "" && description.ActiveRevision != LegacyConfigRevision
		return description
	}
	if readErr != nil {
		description.ErrorCode = configReadErrorCode(readErr)
		description.Drifted = description.ActiveRevision != ""
		return description
	}
	description.Available = true
	revision, digest := configRevision(body)
	description.SelectedRevision = revision
	description.Drifted = description.ActiveRevision != "" && description.ActiveRevision != revision
	if m.state.ConfigMode == ConfigModeManaged && (digest != m.state.CommittedSHA256 || revision != m.state.CommittedConfigRevision) {
		description.Drifted = true
		description.ErrorCode = "managed_drift"
	}
	if cfg, err := proxy.LoadProvidersConfigBytes(m.state.SelectedPath, body); err == nil {
		for _, provider := range cfg.Providers {
			description.Providers = append(description.Providers, ProviderSummary{
				ID:             strings.TrimSpace(provider.ID),
				Type:           strings.TrimSpace(provider.Type),
				Default:        provider.Default,
				StaticModels:   len(provider.Models),
				ModelDiscovery: strings.TrimSpace(provider.ModelDiscovery),
			})
		}
	} else if description.ErrorCode == "" {
		description.ErrorCode = "invalid_config"
	}
	return description
}

func managedSecretProjectionRequirements(state PersistentState) []SecretProjectionRequirement {
	if state.ManagedOwnershipID == "" {
		return nil
	}
	requirement, ok := managedSecretProjectionRequirement(
		state.CommittedConfigRevision,
		state.SecretGeneration,
		state.Providers,
	)
	if !ok {
		return nil
	}
	return []SecretProjectionRequirement{requirement}
}

func (m *ConfigManager) secretProjectionRequirementsLocked() []SecretProjectionRequirement {
	requirements := managedSecretProjectionRequirements(m.state)
	if m.state.RecoveryState != string(ApplyPhaseRollbackFailed) {
		return requirements
	}

	journal, err := readApplyJournal(m.paths.Journal)
	if err != nil {
		return requirements
	}
	for _, candidate := range []struct {
		revision   string
		generation uint64
		providers  []ProviderIdentity
	}{
		{journal.OldRevision, journal.OldSecretGeneration, journal.OldProviders},
		{journal.NewRevision, journal.NewSecretGeneration, journal.NewProviders},
	} {
		requirement, ok := managedSecretProjectionRequirement(
			candidate.revision,
			candidate.generation,
			candidate.providers,
		)
		if !ok {
			continue
		}
		duplicate := false
		for _, existing := range requirements {
			if existing.ConfigRevision == requirement.ConfigRevision &&
				existing.SecretGeneration == requirement.SecretGeneration {
				duplicate = true
				break
			}
		}
		if !duplicate {
			requirements = append(requirements, requirement)
		}
	}
	sort.Slice(requirements, func(i, j int) bool {
		if requirements[i].ConfigRevision != requirements[j].ConfigRevision {
			return requirements[i].ConfigRevision < requirements[j].ConfigRevision
		}
		return requirements[i].SecretGeneration < requirements[j].SecretGeneration
	})
	return requirements
}

func managedSecretProjectionRequirement(
	revision string,
	generation uint64,
	providers []ProviderIdentity,
) (SecretProjectionRequirement, bool) {
	revision = strings.TrimSpace(revision)
	if revision == "" || generation == 0 {
		return SecretProjectionRequirement{}, false
	}

	secrets := make([]ManagedSecretRequirement, 0)
	for _, provider := range providers {
		providerID := strings.TrimSpace(provider.ProviderID)
		providerUUID := strings.TrimSpace(provider.UUID)
		for _, role := range normalizeSecretRoles(provider.SecretRoles) {
			secrets = append(secrets, ManagedSecretRequirement{
				ProviderID:   providerID,
				ProviderUUID: providerUUID,
				Role:         role,
				Reference:    ManagedSecretReference(providerUUID, role, generation),
			})
		}
	}
	sort.Slice(secrets, func(i, j int) bool {
		if secrets[i].ProviderID != secrets[j].ProviderID {
			return secrets[i].ProviderID < secrets[j].ProviderID
		}
		if secrets[i].ProviderUUID != secrets[j].ProviderUUID {
			return secrets[i].ProviderUUID < secrets[j].ProviderUUID
		}
		return secrets[i].Role < secrets[j].Role
	})

	return SecretProjectionRequirement{
		ConfigRevision:   revision,
		SecretGeneration: generation,
		Secrets:          secrets,
	}, true
}

func (m *ConfigManager) saveStateLocked() error {
	m.state.Version = StateVersion
	if m.state.ConfigMode == "" {
		m.state.ConfigMode = ConfigModeLegacy
	}
	persisted := clonePersistentState(m.state)
	if m.stagedSelection != nil {
		persisted.ConfigMode = m.stagedSelection.ConfigMode
		persisted.SelectedPath = m.stagedSelection.SelectedPath
		persisted.SelectedConfigRevision = m.stagedSelection.SelectedConfigRevision
	}
	if persisted.ConfigMode == ConfigModeExternal {
		persisted.LegacyProvidersConfigPath = persisted.SelectedPath
	} else {
		persisted.LegacyProvidersConfigPath = ""
	}
	body, err := json.MarshalIndent(persisted, "", "  ")
	if err != nil {
		return fmt.Errorf("encode helper state: %w", err)
	}
	body = append(body, '\n')
	return writeAtomicFile(m.paths.State, body)
}

func loadPersistentState(path string) (PersistentState, bool, error) {
	body, _, err := readOwnedFile(path, MaxStateBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(err.Error(), "no such file") {
			return PersistentState{Version: StateVersion, ConfigMode: ConfigModeLegacy, SelectedConfigRevision: LegacyConfigRevision}, false, nil
		}
		return PersistentState{}, false, err
	}
	var versionProbe struct {
		Version             int    `json:"version"`
		ProvidersConfigPath string `json:"providers_config_path"`
	}
	if err := json.Unmarshal(body, &versionProbe); err != nil {
		return PersistentState{}, false, fmt.Errorf("decode helper state %q: %w", path, err)
	}
	if versionProbe.Version == 0 {
		selected := strings.TrimSpace(versionProbe.ProvidersConfigPath)
		state := PersistentState{Version: StateVersion, ConfigMode: ConfigModeLegacy, SelectedConfigRevision: LegacyConfigRevision}
		if selected != "" {
			state.ConfigMode = ConfigModeExternal
			state.SelectedPath = selected
			state.SelectedConfigRevision = ""
			if !proxy.IsRemoteProvidersConfigSource(selected) {
				_, body, readErr := loadExternalConfigurationSnapshot(context.Background(), selected)
				if readErr == nil {
					state.SelectedConfigRevision, _ = configRevision(body)
				}
			}
		}
		return state, true, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var state PersistentState
	if err := decoder.Decode(&state); err != nil {
		return PersistentState{}, false, fmt.Errorf("decode helper state %q: %w", path, err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return PersistentState{}, false, fmt.Errorf("decode helper state %q: %w", path, err)
	}
	if state.Version != StateVersion {
		return PersistentState{}, false, fmt.Errorf("unsupported helper state version %d", state.Version)
	}
	if state.ConfigMode == "" {
		state.ConfigMode = ConfigModeLegacy
	}
	return state, false, nil
}

func clonePersistentState(state PersistentState) PersistentState {
	state.Providers = append([]ProviderIdentity(nil), state.Providers...)
	for i := range state.Providers {
		state.Providers[i].SecretRoles = append([]string(nil), state.Providers[i].SecretRoles...)
	}
	return state
}

func configRevision(body []byte) (string, string) {
	digest := sha256.Sum256(body)
	return proxy.ProvidersConfigRevision(body), hex.EncodeToString(digest[:])
}

func reconcileManagedProviders(existing []ProviderIdentity, drafts []ManagedProviderDraft, generation uint64, newUUID func() string) ([]proxy.ProviderConfig, []ProviderIdentity, error) {
	byUUID := make(map[string]ProviderIdentity, len(existing))
	byID := make(map[string]ProviderIdentity, len(existing))
	for _, identity := range existing {
		byUUID[identity.UUID] = identity
		byID[identity.ProviderID] = identity
	}
	seenIDs := make(map[string]struct{}, len(drafts))
	seenUUIDs := make(map[string]struct{}, len(drafts))
	providers := make([]proxy.ProviderConfig, 0, len(drafts))
	identities := make([]ProviderIdentity, 0, len(drafts))
	for index, draft := range drafts {
		cfg := draft.Config
		cfg.ID = strings.TrimSpace(cfg.ID)
		if cfg.ID == "" {
			return nil, nil, fmt.Errorf("providers[%d].id is required", index)
		}
		if _, duplicate := seenIDs[cfg.ID]; duplicate {
			return nil, nil, fmt.Errorf("providers[%d].id duplicates %q", index, cfg.ID)
		}
		seenIDs[cfg.ID] = struct{}{}
		if err := validateManagedProviderFields(cfg, index); err != nil {
			return nil, nil, err
		}

		identity := ProviderIdentity{}
		requestedUUID := strings.TrimSpace(draft.UUID)
		if requestedUUID != "" {
			var ok bool
			identity, ok = byUUID[requestedUUID]
			if !ok {
				return nil, nil, fmt.Errorf("providers[%d].uuid is not helper-owned", index)
			}
		} else if previous, ok := byID[cfg.ID]; ok {
			identity = previous
		} else {
			identity.UUID = newUUID()
		}
		if identity.UUID == "" {
			return nil, nil, fmt.Errorf("providers[%d].uuid generation failed", index)
		}
		if _, duplicate := seenUUIDs[identity.UUID]; duplicate {
			return nil, nil, fmt.Errorf("providers[%d].uuid is duplicated", index)
		}
		seenUUIDs[identity.UUID] = struct{}{}
		identity.ProviderID = cfg.ID
		identity.SecretRoles = normalizeSecretRoles(draft.SecretRoles)
		if containsString(identity.SecretRoles, "api_key") {
			cfg.APIKeyEnv = ManagedSecretReference(identity.UUID, "api_key", generation)
		}
		providers = append(providers, cfg)
		identities = append(identities, identity)
	}
	return providers, identities, nil
}

func validateManagedProviderFields(cfg proxy.ProviderConfig, index int) error {
	path := fmt.Sprintf("providers[%d]", index)
	if strings.TrimSpace(cfg.APIKey) != "" {
		return fmt.Errorf("%s.api_key must never contain a managed secret", path)
	}
	if strings.TrimSpace(cfg.APIKeyEnv) != "" {
		return fmt.Errorf("%s.api_key_env is helper-owned", path)
	}
	if len(cfg.ExtraHeaders) != 0 || !reflect.ValueOf(cfg.Headers).IsZero() {
		return fmt.Errorf("%s contains unsupported custom headers", path)
	}
	if cfg.ChatCompletionsPath != "" || cfg.ResponsesPath != "" || cfg.MessagesPath != "" || cfg.ModelsPath != "" {
		return fmt.Errorf("%s contains unsupported custom paths", path)
	}
	if cfg.TrustDomain != "" || cfg.ClassifierNoStoreSupported != nil {
		return fmt.Errorf("%s contains unsupported trust metadata", path)
	}
	return nil
}

func marshalManagedProviders(cfg proxy.ProvidersConfig) ([]byte, error) {
	var buffer bytes.Buffer
	encoder := yaml.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(cfg); err != nil {
		return nil, fmt.Errorf("encode managed providers: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("close managed providers encoder: %w", err)
	}
	return buffer.Bytes(), nil
}

// ManagedSecretReference returns the generated api_key_env reference stored in
// secret-free managed YAML.
func ManagedSecretReference(providerUUID, role string, generation uint64) string {
	normalize := func(value string) string {
		value = strings.ToUpper(value)
		var builder strings.Builder
		for _, r := range value {
			if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
				builder.WriteRune(r)
			} else {
				builder.WriteByte('_')
			}
		}
		return strings.Trim(builder.String(), "_")
	}
	return fmt.Sprintf("VEKIL_MANAGED_%s_%s_%d", normalize(providerUUID), normalize(role), generation)
}

func normalizeSecretRoles(roles []string) []string {
	set := make(map[string]struct{}, len(roles))
	for _, role := range roles {
		role = strings.TrimSpace(strings.ToLower(role))
		if role != "" {
			set[role] = struct{}{}
		}
	}
	normalized := make([]string, 0, len(set))
	for role := range set {
		normalized = append(normalized, role)
	}
	sort.Strings(normalized)
	return normalized
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func configReadErrorCode(err error) string {
	if err == nil {
		return ""
	}
	message := strings.ToLower(err.Error())
	switch {
	case errors.Is(err, os.ErrNotExist) || strings.Contains(message, "no such file"):
		return "missing_config"
	case strings.Contains(message, "symlink"):
		return "unsafe_symlink"
	case strings.Contains(message, "regular file"):
		return "unsafe_file_type"
	case strings.Contains(message, "exceeds"):
		return "config_too_large"
	default:
		return "config_unavailable"
	}
}

func randomOpaqueID(prefix string) string {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return prefix + uuid.NewString()
	}
	return prefix + base64.RawURLEncoding.EncodeToString(raw[:])
}
