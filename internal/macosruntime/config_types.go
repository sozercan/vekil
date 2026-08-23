package macosruntime

import "github.com/sozercan/vekil/proxy"

const (
	StateVersion        = 1
	ApplyJournalVersion = 1
	MaxConfigBytes      = 4 << 20
	MaxStateBytes       = 1 << 20
)

// ConfigMode identifies the selected configuration ownership model.
type ConfigMode string

const (
	ConfigModeLegacy   ConfigMode = "legacy"
	ConfigModeManaged  ConfigMode = "managed"
	ConfigModeExternal ConfigMode = "external"
)

// ProviderIdentity is helper-owned metadata and never appears in providers
// YAML. UUID remains stable across provider ID rename and reorder.
type ProviderIdentity struct {
	UUID        string   `json:"uuid"`
	ProviderID  string   `json:"provider_id"`
	SecretRoles []string `json:"secret_roles,omitempty"`
}

// PersistentState is stored in menubar.json. It contains revisions and secret
// generation identifiers, but never secret values.
type PersistentState struct {
	Version                   int                `json:"version"`
	ConfigMode                ConfigMode         `json:"config_mode"`
	SelectedPath              string             `json:"selected_path,omitempty"`
	LegacyProvidersConfigPath string             `json:"providers_config_path,omitempty"`
	SelectedConfigRevision    string             `json:"selected_config_revision,omitempty"`
	ManagedOwnershipID        string             `json:"managed_ownership_id,omitempty"`
	CommittedConfigRevision   string             `json:"committed_config_revision,omitempty"`
	CommittedSHA256           string             `json:"committed_sha256,omitempty"`
	ActiveRuntimeRevision     string             `json:"active_runtime_revision,omitempty"`
	ActiveRuntimeGeneration   uint64             `json:"active_runtime_generation,omitempty"`
	SecretGeneration          uint64             `json:"secret_generation,omitempty"`
	Providers                 []ProviderIdentity `json:"providers,omitempty"`
	RecoveryState             string             `json:"recovery_state,omitempty"`
	RecoveryPrimaryCode       string             `json:"recovery_primary_code,omitempty"`
	RecoveryRollbackCode      string             `json:"recovery_rollback_code,omitempty"`
}

// ManagedProviderDraft is the typed provider editor boundary. Config never
// contains an inline APIKey. UUID is metadata used to preserve identity.
type ManagedProviderDraft struct {
	UUID        string               `json:"uuid,omitempty"`
	SecretRoles []string             `json:"secret_roles,omitempty"`
	Config      proxy.ProviderConfig `json:"config"`
}

// ManagedDraft is deliberately limited to provider-owned configuration. The
// native guided editor does not own explicit routes, policy profiles, custom
// headers/paths, trust metadata, or tool optimizers.
type ManagedDraft struct {
	Providers []ManagedProviderDraft `json:"providers"`
}

// ManagedCandidate is deterministic, validated, secret-free YAML plus identity
// metadata ready for a transaction.
type ManagedCandidate struct {
	Bytes            []byte
	Config           proxy.ProvidersConfig
	Revision         string
	SHA256           string
	SecretGeneration uint64
	Providers        []ProviderIdentity
}

// ConfigDescription is the sanitized configuration projection returned to the
// native shell.
type ConfigDescription struct {
	Mode                    ConfigMode                    `json:"mode"`
	SelectedPath            string                        `json:"selected_path,omitempty"`
	SelectedRevision        string                        `json:"selected_revision,omitempty"`
	ActiveRevision          string                        `json:"active_revision,omitempty"`
	Available               bool                          `json:"available"`
	Drifted                 bool                          `json:"drifted"`
	ErrorCode               string                        `json:"error_code,omitempty"`
	ManagedOwnershipPresent bool                          `json:"managed_ownership_present"`
	SecretGeneration        uint64                        `json:"secret_generation,omitempty"`
	Providers               []ProviderSummary             `json:"providers,omitempty"`
	SecretProjections       []SecretProjectionRequirement `json:"secret_projections,omitempty"`
}

// SecretProjectionRequirement is the non-secret metadata Swift needs to load
// one complete Keychain generation after a helper launch. It is present for an
// owned managed configuration even while External Configuration is selected,
// so switching back to managed can start without restarting the helper.
type SecretProjectionRequirement struct {
	ConfigRevision   string                     `json:"config_revision"`
	SecretGeneration uint64                     `json:"secret_generation"`
	Secrets          []ManagedSecretRequirement `json:"secrets"`
}

// ManagedSecretRequirement maps a stable Keychain identity to the provider-
// scoped reference embedded in secret-free managed YAML.
type ManagedSecretRequirement struct {
	ProviderID   string `json:"provider_id"`
	ProviderUUID string `json:"provider_uuid"`
	Role         string `json:"role"`
	Reference    string `json:"reference"`
}

// ProviderSummary intentionally excludes credentials, URLs, headers, paths,
// and raw configuration.
type ProviderSummary struct {
	ID             string `json:"id"`
	Type           string `json:"type"`
	Default        bool   `json:"default"`
	StaticModels   int    `json:"static_models"`
	ModelDiscovery string `json:"model_discovery,omitempty"`
}

// ApplyPhase is persisted after each durable transition.
type ApplyPhase string

const (
	ApplyPhasePrepared       ApplyPhase = "prepared"
	ApplyPhaseInstalled      ApplyPhase = "installed"
	ApplyPhaseCommitted      ApplyPhase = "committed"
	ApplyPhaseRollbackFailed ApplyPhase = "rollback_failed"
)

// ApplyJournal contains no provider secret values or raw YAML.
type ApplyJournal struct {
	Version             int                `json:"version"`
	OperationID         string             `json:"operation_id"`
	Phase               ApplyPhase         `json:"phase"`
	OldRevision         string             `json:"old_revision"`
	NewRevision         string             `json:"new_revision"`
	OldSHA256           string             `json:"old_sha256"`
	NewSHA256           string             `json:"new_sha256"`
	OldSecretGeneration uint64             `json:"old_secret_generation"`
	NewSecretGeneration uint64             `json:"new_secret_generation"`
	WasRunning          bool               `json:"was_running"`
	PrimaryFailureCode  string             `json:"primary_failure_code,omitempty"`
	RollbackFailureCode string             `json:"rollback_failure_code,omitempty"`
	OldProviders        []ProviderIdentity `json:"old_providers,omitempty"`
	NewProviders        []ProviderIdentity `json:"new_providers,omitempty"`
}
