package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// ManagedProvidersConfigSchemaVersion is the only managed envelope schema
	// currently accepted by the resolver and store.
	ManagedProvidersConfigSchemaVersion = 1
)

// ProvidersConfigSourceKind identifies the immutable bootstrap source class.
type ProvidersConfigSourceKind string

const (
	ProvidersConfigSourceImplicitCopilot ProvidersConfigSourceKind = "implicit-copilot"
	ProvidersConfigSourceFile            ProvidersConfigSourceKind = "file"
)

// ProvidersConfigSource is the stable identity and persistence location for one
// bootstrap config. BootstrapDigest is computed from canonical secretful JSON.
type ProvidersConfigSource struct {
	Kind            ProvidersConfigSourceKind
	ID              string
	BootstrapPath   string
	BootstrapDigest string
	ManagedPath     string
}

// ProvidersConfigSourceFingerprint is persisted in managed envelopes.
type ProvidersConfigSourceFingerprint struct {
	ID              string
	BootstrapDigest string
}

// Fingerprint returns the source fields that are committed into a managed file.
func (s ProvidersConfigSource) Fingerprint() ProvidersConfigSourceFingerprint {
	return ProvidersConfigSourceFingerprint{ID: s.ID, BootstrapDigest: s.BootstrapDigest}
}

// ProvidersConfigBootstrap is the validated canonical bootstrap snapshot used
// to resolve and later commit source-scoped managed overrides.
type ProvidersConfigBootstrap struct {
	Source   ProvidersConfigSource
	Config   ProvidersConfig
	Revision string
}

// ManagedProvidersConfigEnvelope is the durable source-scoped representation.
type ManagedProvidersConfigEnvelope struct {
	ManagedSchemaVersion int
	Source               ProvidersConfigSourceFingerprint
	Revision             string
	Config               ProvidersConfig
}

// ProvidersConfigResolveMode controls pre-start managed recovery behavior.
type ProvidersConfigResolveMode string

const (
	ProvidersConfigUseManaged    ProvidersConfigResolveMode = "use-managed"
	ProvidersConfigIgnoreManaged ProvidersConfigResolveMode = "ignore-managed"
	ProvidersConfigResetManaged  ProvidersConfigResolveMode = "reset-managed"
)

// ProvidersConfigResolveOptions selects one bootstrap and managed-store root.
type ProvidersConfigResolveOptions struct {
	BootstrapPath string
	UserConfigDir string
	Mode          ProvidersConfigResolveMode
}

// ResolvedProvidersConfig is the effective startup snapshot. PersistenceWarning
// is non-fatal and is populated only when a reset committed but directory sync
// could not be confirmed.
type ResolvedProvidersConfig struct {
	Bootstrap          ProvidersConfigBootstrap
	Config             ProvidersConfig
	Revision           string
	Managed            bool
	PersistenceWarning error
}

// ManagedProvidersConfigCommitResult describes a committed managed generation.
type ManagedProvidersConfigCommitResult struct {
	Envelope          ManagedProvidersConfigEnvelope
	Path              string
	DurabilityWarning error
}

// ManagedProvidersConfigResetResult describes a reset to the immutable
// bootstrap generation.
type ManagedProvidersConfigResetResult struct {
	Config            ProvidersConfig
	Revision          string
	Path              string
	DurabilityWarning error
}

// ManagedProvidersConfigStore performs optimistic, source-fenced managed-file
// commits for one immutable bootstrap snapshot.
type ManagedProvidersConfigStore struct {
	bootstrap          ProvidersConfigBootstrap
	beforeIrreversible func()
}

// LoadProvidersConfigBootstrap resolves an implicit-Copilot or absolute-clean
// file source, validates it, and computes its source-scoped managed path.
func LoadProvidersConfigBootstrap(bootstrapPath, userConfigDir string) (ProvidersConfigBootstrap, error) {
	var bootstrap ProvidersConfigBootstrap
	cfg, kind, id, absolutePath, err := loadBootstrapProvidersConfig(bootstrapPath)
	if err != nil {
		return bootstrap, err
	}
	cfg, err = normalizeProvidersConfigForPersistence(cfg)
	if err != nil {
		return bootstrap, err
	}
	digest, err := ProvidersConfigDigest(cfg)
	if err != nil {
		return bootstrap, err
	}
	revision, err := ProvidersConfigRevision(cfg)
	if err != nil {
		return bootstrap, err
	}
	managedPath, err := ManagedProvidersConfigPath(userConfigDir, id)
	if err != nil {
		return bootstrap, err
	}
	bootstrap = ProvidersConfigBootstrap{
		Source: ProvidersConfigSource{
			Kind:            kind,
			ID:              id,
			BootstrapPath:   absolutePath,
			BootstrapDigest: digest,
			ManagedPath:     managedPath,
		},
		Config:   cfg,
		Revision: revision,
	}
	return bootstrap, nil
}

func loadBootstrapProvidersConfig(bootstrapPath string) (ProvidersConfig, ProvidersConfigSourceKind, string, string, error) {
	bootstrapPath = strings.TrimSpace(bootstrapPath)
	if bootstrapPath == "" {
		return ProvidersConfig{}, ProvidersConfigSourceImplicitCopilot, string(ProvidersConfigSourceImplicitCopilot), "", nil
	}
	absolutePath, err := filepath.Abs(bootstrapPath)
	if err != nil {
		wrapped := fmt.Errorf("resolve providers bootstrap path %q: %w", bootstrapPath, err)
		return ProvidersConfig{}, "", "", "", NewConfigError(ConfigErrorInvalidSource, "/source/id", wrapped.Error(), wrapped)
	}
	absolutePath = filepath.Clean(absolutePath)
	cfg, err := LoadProvidersConfigFile(absolutePath)
	if err != nil {
		return ProvidersConfig{}, "", "", "", NewConfigError(ConfigErrorInvalidSource, "/source/id", err.Error(), err)
	}
	return cfg, ProvidersConfigSourceFile, "file:" + absolutePath, absolutePath, nil
}

// ManagedProvidersConfigPath maps a bootstrap source ID to its isolated file
// below <UserConfigDir>/vekil/dashboard-config.
func ManagedProvidersConfigPath(userConfigDir, sourceID string) (string, error) {
	if _, _, err := parseProvidersConfigSourceID(sourceID); err != nil {
		return "", err
	}
	userConfigDir = strings.TrimSpace(userConfigDir)
	if userConfigDir == "" {
		var err error
		userConfigDir, err = os.UserConfigDir()
		if err != nil {
			wrapped := fmt.Errorf("resolve user config directory: %w", err)
			return "", NewConfigError(ConfigErrorManagedStore, "", wrapped.Error(), wrapped)
		}
	}
	absoluteDir, err := filepath.Abs(userConfigDir)
	if err != nil {
		wrapped := fmt.Errorf("resolve user config directory %q: %w", userConfigDir, err)
		return "", NewConfigError(ConfigErrorManagedStore, "", wrapped.Error(), wrapped)
	}
	name := "implicit-copilot.json"
	if sourceID != string(ProvidersConfigSourceImplicitCopilot) {
		sum := sha256.Sum256([]byte(sourceID))
		name = hex.EncodeToString(sum[:]) + ".json"
	}
	return filepath.Join(filepath.Clean(absoluteDir), "vekil", "dashboard-config", name), nil
}

func parseProvidersConfigSourceID(sourceID string) (ProvidersConfigSourceKind, string, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == string(ProvidersConfigSourceImplicitCopilot) {
		return ProvidersConfigSourceImplicitCopilot, "", nil
	}
	if !strings.HasPrefix(sourceID, "file:") {
		err := fmt.Errorf("unsupported providers config source id %q", sourceID)
		return "", "", NewConfigError(ConfigErrorInvalidSource, "/source/id", err.Error(), err)
	}
	path := strings.TrimPrefix(sourceID, "file:")
	if path == "" || !filepath.IsAbs(path) || filepath.Clean(path) != path {
		err := fmt.Errorf("providers config file source must contain an absolute clean path")
		return "", "", NewConfigError(ConfigErrorInvalidSource, "/source/id", err.Error(), err)
	}
	return ProvidersConfigSourceFile, path, nil
}

// NewManagedProvidersConfigEnvelope validates and normalizes cfg and binds it to
// source with a deterministic revision.
func NewManagedProvidersConfigEnvelope(source ProvidersConfigSource, cfg ProvidersConfig) (ManagedProvidersConfigEnvelope, error) {
	var envelope ManagedProvidersConfigEnvelope
	if err := validateProvidersConfigSourceFingerprint(source.Fingerprint()); err != nil {
		return envelope, err
	}
	normalized, err := normalizeProvidersConfigForPersistence(cfg)
	if err != nil {
		return envelope, err
	}
	revision, err := ProvidersConfigRevision(normalized)
	if err != nil {
		return envelope, err
	}
	return ManagedProvidersConfigEnvelope{
		ManagedSchemaVersion: ManagedProvidersConfigSchemaVersion,
		Source:               source.Fingerprint(),
		Revision:             revision,
		Config:               normalized,
	}, nil
}

// EncodeManagedProvidersConfigJSON returns canonical JSON for a strict schema-1
// envelope and rejects inconsistent source/revision metadata.
func EncodeManagedProvidersConfigJSON(envelope ManagedProvidersConfigEnvelope) ([]byte, error) {
	if envelope.ManagedSchemaVersion != ManagedProvidersConfigSchemaVersion {
		err := fmt.Errorf("unsupported managed schema version %d; supported version is %d", envelope.ManagedSchemaVersion, ManagedProvidersConfigSchemaVersion)
		return nil, NewConfigError(ConfigErrorUnsupportedManagedSchema, "/managed_schema_version", err.Error(), err)
	}
	if err := validateProvidersConfigSourceFingerprint(envelope.Source); err != nil {
		return nil, err
	}
	normalized, err := normalizeProvidersConfigForPersistence(envelope.Config)
	if err != nil {
		return nil, prefixConfigError(err, "/config")
	}
	revision, err := ProvidersConfigRevision(normalized)
	if err != nil {
		return nil, err
	}
	if envelope.Revision != revision {
		err := fmt.Errorf("managed revision %q does not match canonical config revision %q", envelope.Revision, revision)
		return nil, NewConfigError(ConfigErrorManagedEnvelope, "/revision", err.Error(), err)
	}
	configBody, err := EncodeProvidersConfigJSON(normalized)
	if err != nil {
		return nil, prefixConfigError(err, "/config")
	}
	body, err := json.Marshal(map[string]interface{}{
		"managed_schema_version": ManagedProvidersConfigSchemaVersion,
		"source": map[string]interface{}{
			"id":               envelope.Source.ID,
			"bootstrap_digest": envelope.Source.BootstrapDigest,
		},
		"revision": envelope.Revision,
		"config":   json.RawMessage(configBody),
	})
	if err != nil {
		return nil, NewConfigError(ConfigErrorManagedEnvelope, "", fmt.Sprintf("encode managed providers config: %v", err), err)
	}
	return body, nil
}

// DecodeManagedProvidersConfigJSON strictly decodes, validates, normalizes, and
// self-checks one managed schema-1 envelope.
func DecodeManagedProvidersConfigJSON(body []byte) (ManagedProvidersConfigEnvelope, error) {
	var envelope ManagedProvidersConfigEnvelope
	if len(bytes.TrimSpace(body)) == 0 {
		err := fmt.Errorf("managed providers config is empty")
		return envelope, NewConfigError(ConfigErrorEmpty, "", err.Error(), err)
	}
	if err := rejectDuplicateJSONMappingKeys(body); err != nil {
		return envelope, NewConfigError(
			classifyJSONDecodeError(err, ConfigErrorDuplicateField),
			legacyConfigErrorPointer(err),
			err.Error(),
			err,
		)
	}
	if err := validateManagedEnvelopeJSONFields(body); err != nil {
		return envelope, err
	}

	var wire struct {
		ManagedSchemaVersion int `json:"managed_schema_version"`
		Source               *struct {
			ID              string `json:"id"`
			BootstrapDigest string `json:"bootstrap_digest"`
		} `json:"source"`
		Revision string          `json:"revision"`
		Config   json.RawMessage `json:"config"`
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&wire); err != nil {
		return envelope, NewConfigError(classifyJSONDecodeError(err, ConfigErrorManagedEnvelope), legacyConfigErrorPointer(err), err.Error(), err)
	}
	var extra interface{}
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("more than one JSON value")
		} else {
			err = fmt.Errorf("trailing JSON value: %w", err)
		}
		return envelope, NewConfigError(ConfigErrorTrailingValue, "", err.Error(), err)
	}
	if wire.ManagedSchemaVersion != ManagedProvidersConfigSchemaVersion {
		err := fmt.Errorf("unsupported managed schema version %d; supported version is %d", wire.ManagedSchemaVersion, ManagedProvidersConfigSchemaVersion)
		return envelope, NewConfigError(ConfigErrorUnsupportedManagedSchema, "/managed_schema_version", err.Error(), err)
	}
	if wire.Source == nil {
		err := fmt.Errorf("managed source is required")
		return envelope, NewConfigError(ConfigErrorManagedEnvelope, "/source", err.Error(), err)
	}
	source := ProvidersConfigSourceFingerprint{ID: wire.Source.ID, BootstrapDigest: wire.Source.BootstrapDigest}
	if err := validateProvidersConfigSourceFingerprint(source); err != nil {
		return envelope, err
	}
	if len(bytes.TrimSpace(wire.Config)) == 0 || bytes.Equal(bytes.TrimSpace(wire.Config), []byte("null")) {
		err := fmt.Errorf("managed config is required and must not be null")
		return envelope, NewConfigError(ConfigErrorManagedEnvelope, "/config", err.Error(), err)
	}
	cfg, err := DecodeProvidersConfigJSON(wire.Config)
	if err != nil {
		return envelope, prefixConfigError(err, "/config")
	}
	cfg, err = normalizeProvidersConfigForPersistence(cfg)
	if err != nil {
		return envelope, prefixConfigError(err, "/config")
	}
	if !validSHA256Value(wire.Revision, "cfg_") {
		err := fmt.Errorf("managed revision must use cfg_ followed by a SHA-256 hex digest")
		return envelope, NewConfigError(ConfigErrorManagedEnvelope, "/revision", err.Error(), err)
	}
	revision, err := ProvidersConfigRevision(cfg)
	if err != nil {
		return envelope, err
	}
	if wire.Revision != revision {
		err := fmt.Errorf("managed revision %q does not match canonical config revision %q", wire.Revision, revision)
		return envelope, NewConfigError(ConfigErrorManagedEnvelope, "/revision", err.Error(), err)
	}
	return ManagedProvidersConfigEnvelope{
		ManagedSchemaVersion: wire.ManagedSchemaVersion,
		Source:               source,
		Revision:             wire.Revision,
		Config:               cfg,
	}, nil
}

func validateManagedEnvelopeJSONFields(body []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var document interface{}
	if err := decoder.Decode(&document); err != nil {
		return NewConfigError(ConfigErrorManagedEnvelope, "", err.Error(), err)
	}
	root, ok := document.(map[string]interface{})
	if !ok {
		err := fmt.Errorf("top-level managed provider configuration must be a JSON object")
		return NewConfigError(ConfigErrorManagedEnvelope, "", err.Error(), err)
	}
	if err := validateKnownManagedFields(root, configFieldSet("managed_schema_version", "source", "revision", "config"), ""); err != nil {
		return err
	}
	if source, ok := root["source"].(map[string]interface{}); ok {
		if err := validateKnownManagedFields(source, configFieldSet("id", "bootstrap_digest"), "source"); err != nil {
			return err
		}
	}
	return nil
}

func validateKnownManagedFields(object map[string]interface{}, allowed map[string]struct{}, path string) error {
	fields := make([]string, 0, len(object))
	for field := range object {
		fields = append(fields, field)
	}
	sort.Strings(fields)
	for _, field := range fields {
		if _, ok := allowed[field]; ok {
			continue
		}
		legacyPath := appendConfigObjectPath(path, field)
		err := configPathError(legacyPath, "unknown field %q", field)
		return NewConfigError(ConfigErrorUnknownField, ConfigPathToJSONPointer(legacyPath), err.Error(), err)
	}
	return nil
}

func validateProvidersConfigSourceFingerprint(source ProvidersConfigSourceFingerprint) error {
	if _, _, err := parseProvidersConfigSourceID(source.ID); err != nil {
		return err
	}
	if !validSHA256Value(source.BootstrapDigest, "sha256:") {
		err := fmt.Errorf("bootstrap digest must use sha256: followed by a SHA-256 hex digest")
		return NewConfigError(ConfigErrorInvalidSource, "/source/bootstrap_digest", err.Error(), err)
	}
	return nil
}

func prefixConfigError(err error, prefix string) error {
	if err == nil {
		return nil
	}
	var typed *ConfigError
	if !errors.As(err, &typed) {
		return NewConfigError(ConfigErrorInvalidConfig, normalizeJSONPointer(prefix), err.Error(), err)
	}
	copy := *typed
	copy.Pointer = prefixJSONPointer(prefix, typed.Pointer)
	copy.Err = err
	return &copy
}

func prefixJSONPointer(prefix, pointer string) string {
	prefix = normalizeJSONPointer(prefix)
	pointer = normalizeJSONPointer(pointer)
	if prefix == "" {
		return pointer
	}
	if pointer == "" {
		return prefix
	}
	return strings.TrimSuffix(prefix, "/") + pointer
}

// ResolveProvidersConfig loads one bootstrap and applies the source-matching
// managed override unless ignore/reset recovery was requested.
func ResolveProvidersConfig(options ProvidersConfigResolveOptions) (ResolvedProvidersConfig, error) {
	var resolved ResolvedProvidersConfig
	bootstrap, err := LoadProvidersConfigBootstrap(options.BootstrapPath, options.UserConfigDir)
	if err != nil {
		return resolved, err
	}
	resolved = ResolvedProvidersConfig{
		Bootstrap: bootstrap,
		Config:    bootstrap.Config,
		Revision:  bootstrap.Revision,
	}
	mode := options.Mode
	if mode == "" {
		mode = ProvidersConfigUseManaged
	}
	switch mode {
	case ProvidersConfigIgnoreManaged:
		return resolved, nil
	case ProvidersConfigResetManaged:
		warning, err := resetManagedProvidersConfigUnconditionally(context.Background(), bootstrap.Source.ManagedPath)
		if err != nil {
			return ResolvedProvidersConfig{}, err
		}
		resolved.PersistenceWarning = warning
		return resolved, nil
	case ProvidersConfigUseManaged:
	default:
		err := fmt.Errorf("unsupported providers config resolve mode %q", mode)
		return ResolvedProvidersConfig{}, NewConfigError(ConfigErrorInvalidSource, "", err.Error(), err)
	}

	envelope, found, err := readManagedProvidersConfig(bootstrap)
	if err != nil {
		return ResolvedProvidersConfig{}, err
	}
	if !found {
		return resolved, nil
	}
	resolved.Config = envelope.Config
	resolved.Revision = envelope.Revision
	resolved.Managed = true
	return resolved, nil
}

// NewManagedProvidersConfigStore binds a commit store to the exact bootstrap
// fingerprint returned by LoadProvidersConfigBootstrap or ResolveProvidersConfig.
func NewManagedProvidersConfigStore(bootstrap ProvidersConfigBootstrap) (*ManagedProvidersConfigStore, error) {
	if err := validateProvidersConfigSourceFingerprint(bootstrap.Source.Fingerprint()); err != nil {
		return nil, err
	}
	if strings.TrimSpace(bootstrap.Source.ManagedPath) == "" {
		err := fmt.Errorf("managed providers config path is required")
		return nil, NewConfigError(ConfigErrorManagedStore, "", err.Error(), err)
	}
	normalized, err := normalizeProvidersConfigForPersistence(bootstrap.Config)
	if err != nil {
		return nil, err
	}
	digest, err := ProvidersConfigDigest(normalized)
	if err != nil {
		return nil, err
	}
	if digest != bootstrap.Source.BootstrapDigest {
		err := fmt.Errorf("bootstrap config digest %q does not match source digest %q", digest, bootstrap.Source.BootstrapDigest)
		return nil, NewConfigError(ConfigErrorInvalidSource, "/source/bootstrap_digest", err.Error(), err)
	}
	revision, err := ProvidersConfigRevision(normalized)
	if err != nil {
		return nil, err
	}
	if revision != bootstrap.Revision {
		err := fmt.Errorf("bootstrap config revision %q does not match supplied revision %q", revision, bootstrap.Revision)
		return nil, NewConfigError(ConfigErrorRevisionMismatch, "/revision", err.Error(), err)
	}
	bootstrap.Config = normalized
	return &ManagedProvidersConfigStore{bootstrap: bootstrap}, nil
}

// Path returns the source-scoped managed destination.
func (s *ManagedProvidersConfigStore) Path() string {
	if s == nil {
		return ""
	}
	return s.bootstrap.Source.ManagedPath
}

// Commit validates cfg privately, verifies expectedRevision and the bootstrap
// fingerprint under a process lock, writes and reads back a synced owner-only
// temporary file, rechecks the commit fence, and atomically replaces the final
// managed file. A post-rename directory-sync failure is returned as a warning.
func (s *ManagedProvidersConfigStore) Commit(ctx context.Context, expectedRevision string, cfg ProvidersConfig) (result ManagedProvidersConfigCommitResult, err error) {
	if s == nil {
		err := fmt.Errorf("managed providers config store is nil")
		return result, NewConfigError(ConfigErrorManagedStore, "", err.Error(), err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !validSHA256Value(expectedRevision, "cfg_") {
		err := fmt.Errorf("expected revision must use cfg_ followed by a SHA-256 hex digest")
		return result, NewConfigError(ConfigErrorRevisionMismatch, "/revision", err.Error(), err)
	}
	if err := ensureManagedProvidersConfigDirectory(filepath.Dir(s.Path())); err != nil {
		return result, err
	}
	lock, err := acquireManagedProvidersConfigFileLock(ctx, s.Path()+".lock")
	if err != nil {
		return result, NewConfigError(ConfigErrorManagedStore, "", err.Error(), err)
	}
	defer func() {
		if releaseErr := lock.release(); releaseErr != nil {
			if err != nil {
				err = errors.Join(err, releaseErr)
			} else {
				result.DurabilityWarning = errors.Join(result.DurabilityWarning, releaseErr)
			}
		}
	}()

	if err := s.recheckBootstrapLocked(); err != nil {
		return result, err
	}
	current, err := s.currentLocked()
	if err != nil {
		return result, err
	}
	if current.revision != expectedRevision {
		return result, revisionMismatchError(expectedRevision, current.revision)
	}
	normalized, err := normalizeProvidersConfigForPersistence(cfg)
	if err != nil {
		return result, err
	}
	envelope, err := NewManagedProvidersConfigEnvelope(s.bootstrap.Source, normalized)
	if err != nil {
		return result, err
	}
	body, err := EncodeManagedProvidersConfigJSON(envelope)
	if err != nil {
		return result, err
	}
	if err := rejectManagedProvidersConfigDestinationAlias(s.Path(), s.bootstrap.Source.BootstrapPath); err != nil {
		return result, err
	}

	tmp, err := os.CreateTemp(filepath.Dir(s.Path()), ".providers-config-*.tmp")
	if err != nil {
		wrapped := fmt.Errorf("create managed providers config temporary file: %w", err)
		return result, NewConfigError(ConfigErrorManagedStore, "", wrapped.Error(), wrapped)
	}
	tmpPath := tmp.Name()
	removeTemp := true
	defer func() {
		if removeTemp {
			_ = os.Remove(tmpPath)
		}
	}()
	if err := hardenManagedProvidersConfigFile(tmp); err != nil {
		_ = tmp.Close()
		wrapped := fmt.Errorf("restrict managed providers config temporary file: %w", err)
		return result, NewConfigError(ConfigErrorManagedStore, "", wrapped.Error(), wrapped)
	}
	written, writeErr := io.Copy(tmp, bytes.NewReader(body))
	if writeErr != nil || written != int64(len(body)) {
		_ = tmp.Close()
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		wrapped := fmt.Errorf("write managed providers config temporary file: %w", writeErr)
		return result, NewConfigError(ConfigErrorManagedStore, "", wrapped.Error(), wrapped)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		wrapped := fmt.Errorf("sync managed providers config temporary file: %w", err)
		return result, NewConfigError(ConfigErrorManagedStore, "", wrapped.Error(), wrapped)
	}
	if err := tmp.Close(); err != nil {
		wrapped := fmt.Errorf("close managed providers config temporary file: %w", err)
		return result, NewConfigError(ConfigErrorManagedStore, "", wrapped.Error(), wrapped)
	}
	readbackBody, _, err := readRegularManagedProvidersConfigFile(tmpPath)
	if err != nil {
		return result, err
	}
	readback, err := DecodeManagedProvidersConfigJSON(readbackBody)
	if err != nil {
		return result, err
	}
	if readback.Source != envelope.Source || readback.Revision != envelope.Revision {
		err := fmt.Errorf("managed providers config readback metadata mismatch")
		return result, NewConfigError(ConfigErrorManagedStore, "", err.Error(), err)
	}

	if err := s.recheckBootstrapLocked(); err != nil {
		return result, err
	}
	rechecked, err := s.currentLocked()
	if err != nil {
		return result, err
	}
	if rechecked.revision != current.revision {
		return result, revisionMismatchError(current.revision, rechecked.revision)
	}
	if err := rejectManagedProvidersConfigDestinationAlias(s.Path(), s.bootstrap.Source.BootstrapPath); err != nil {
		return result, err
	}
	if s.beforeIrreversible != nil {
		s.beforeIrreversible()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if err := replaceManagedProvidersConfigFile(tmpPath, s.Path()); err != nil {
		wrapped := fmt.Errorf("replace managed providers config %q: %w", s.Path(), err)
		return result, NewConfigError(ConfigErrorManagedStore, "", wrapped.Error(), wrapped)
	}
	removeTemp = false
	result.Envelope = readback
	result.Path = s.Path()
	if syncErr := syncManagedProvidersConfigDirectory(filepath.Dir(s.Path())); syncErr != nil {
		result.DurabilityWarning = fmt.Errorf("managed providers config committed but directory sync failed: %w", syncErr)
	}
	return result, nil
}

// Reset removes the matching managed override after an optimistic revision and
// source-fingerprint check, returning the immutable bootstrap generation.
func (s *ManagedProvidersConfigStore) Reset(ctx context.Context, expectedRevision string) (result ManagedProvidersConfigResetResult, err error) {
	if s == nil {
		err := fmt.Errorf("managed providers config store is nil")
		return result, NewConfigError(ConfigErrorManagedStore, "", err.Error(), err)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !validSHA256Value(expectedRevision, "cfg_") {
		err := fmt.Errorf("expected revision must use cfg_ followed by a SHA-256 hex digest")
		return result, NewConfigError(ConfigErrorRevisionMismatch, "/revision", err.Error(), err)
	}
	if _, statErr := os.Stat(filepath.Dir(s.Path())); errors.Is(statErr, os.ErrNotExist) {
		if expectedRevision != s.bootstrap.Revision {
			return result, revisionMismatchError(expectedRevision, s.bootstrap.Revision)
		}
		return ManagedProvidersConfigResetResult{Config: s.bootstrap.Config, Revision: s.bootstrap.Revision, Path: s.Path()}, nil
	}
	if err := ensureManagedProvidersConfigDirectory(filepath.Dir(s.Path())); err != nil {
		return result, err
	}
	lock, err := acquireManagedProvidersConfigFileLock(ctx, s.Path()+".lock")
	if err != nil {
		return result, NewConfigError(ConfigErrorManagedStore, "", err.Error(), err)
	}
	defer func() {
		if releaseErr := lock.release(); releaseErr != nil {
			if err != nil {
				err = errors.Join(err, releaseErr)
			} else {
				result.DurabilityWarning = errors.Join(result.DurabilityWarning, releaseErr)
			}
		}
	}()
	if err := s.recheckBootstrapLocked(); err != nil {
		return result, err
	}
	current, err := s.currentLocked()
	if err != nil {
		return result, err
	}
	if current.revision != expectedRevision {
		return result, revisionMismatchError(expectedRevision, current.revision)
	}
	if s.beforeIrreversible != nil {
		s.beforeIrreversible()
	}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if current.envelope != nil {
		if err := os.Remove(s.Path()); err != nil {
			wrapped := fmt.Errorf("remove managed providers config %q: %w", s.Path(), err)
			return result, NewConfigError(ConfigErrorManagedStore, "", wrapped.Error(), wrapped)
		}
	}
	result = ManagedProvidersConfigResetResult{Config: s.bootstrap.Config, Revision: s.bootstrap.Revision, Path: s.Path()}
	if current.envelope != nil {
		if syncErr := syncManagedProvidersConfigDirectory(filepath.Dir(s.Path())); syncErr != nil {
			result.DurabilityWarning = fmt.Errorf("managed providers config reset but directory sync failed: %w", syncErr)
		}
	}
	return result, nil
}

type currentManagedProvidersConfig struct {
	revision string
	envelope *ManagedProvidersConfigEnvelope
}

func (s *ManagedProvidersConfigStore) currentLocked() (currentManagedProvidersConfig, error) {
	envelope, found, err := readManagedProvidersConfig(s.bootstrap)
	if err != nil {
		return currentManagedProvidersConfig{}, err
	}
	if !found {
		return currentManagedProvidersConfig{revision: s.bootstrap.Revision}, nil
	}
	return currentManagedProvidersConfig{revision: envelope.Revision, envelope: &envelope}, nil
}

func (s *ManagedProvidersConfigStore) recheckBootstrapLocked() error {
	cfg, kind, id, absolutePath, err := loadBootstrapProvidersConfig(s.bootstrap.Source.BootstrapPath)
	if err != nil {
		return managedSourceConflict("/source/bootstrap_digest", "reload bootstrap source", err)
	}
	if kind != s.bootstrap.Source.Kind || id != s.bootstrap.Source.ID || absolutePath != s.bootstrap.Source.BootstrapPath {
		return managedSourceConflict("/source/id", "bootstrap source identity changed", nil)
	}
	cfg, err = normalizeProvidersConfigForPersistence(cfg)
	if err != nil {
		return managedSourceConflict("/source/bootstrap_digest", "bootstrap source is no longer valid", err)
	}
	digest, err := ProvidersConfigDigest(cfg)
	if err != nil {
		return err
	}
	if digest != s.bootstrap.Source.BootstrapDigest {
		return managedSourceConflict(
			"/source/bootstrap_digest",
			fmt.Sprintf("bootstrap digest changed from %q to %q", s.bootstrap.Source.BootstrapDigest, digest),
			nil,
		)
	}
	return nil
}

func revisionMismatchError(expected, current string) error {
	err := fmt.Errorf("providers config revision mismatch: expected %q, current is %q", expected, current)
	return NewConfigError(ConfigErrorRevisionMismatch, "/revision", err.Error(), err)
}

func managedSourceConflict(pointer, message string, cause error) error {
	if cause != nil {
		message += ": " + cause.Error()
	}
	return NewConfigError(ConfigErrorManagedSourceConflict, pointer, message, cause)
}

func readManagedProvidersConfig(bootstrap ProvidersConfigBootstrap) (ManagedProvidersConfigEnvelope, bool, error) {
	var envelope ManagedProvidersConfigEnvelope
	if err := rejectManagedProvidersConfigDestinationAlias(bootstrap.Source.ManagedPath, bootstrap.Source.BootstrapPath); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return envelope, false, nil
		}
		return envelope, false, err
	}
	body, _, err := readRegularManagedProvidersConfigFile(bootstrap.Source.ManagedPath)
	if errors.Is(err, os.ErrNotExist) {
		return envelope, false, nil
	}
	if err != nil {
		return envelope, false, err
	}
	envelope, err = DecodeManagedProvidersConfigJSON(body)
	if err != nil {
		return ManagedProvidersConfigEnvelope{}, false, err
	}
	if envelope.Source.ID != bootstrap.Source.ID {
		return ManagedProvidersConfigEnvelope{}, false, managedSourceConflict(
			"/source/id",
			fmt.Sprintf("managed source id %q does not match bootstrap source id %q; use ignore-managed or reset-managed recovery", envelope.Source.ID, bootstrap.Source.ID),
			nil,
		)
	}
	if envelope.Source.BootstrapDigest != bootstrap.Source.BootstrapDigest {
		return ManagedProvidersConfigEnvelope{}, false, managedSourceConflict(
			"/source/bootstrap_digest",
			fmt.Sprintf("managed bootstrap digest %q does not match current bootstrap digest %q; use ignore-managed or reset-managed recovery", envelope.Source.BootstrapDigest, bootstrap.Source.BootstrapDigest),
			nil,
		)
	}
	return envelope, true, nil
}

func readRegularManagedProvidersConfigFile(path string) ([]byte, os.FileInfo, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if pathInfo.Mode()&os.ModeSymlink != 0 {
		err := fmt.Errorf("managed providers config %q must not be a symbolic link", path)
		return nil, nil, NewConfigError(ConfigErrorManagedStore, "", err.Error(), err)
	}
	if !pathInfo.Mode().IsRegular() {
		err := fmt.Errorf("managed providers config %q is not a regular file", path)
		return nil, nil, NewConfigError(ConfigErrorManagedStore, "", err.Error(), err)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer func() { _ = file.Close() }()
	openedInfo, err := file.Stat()
	if err != nil {
		return nil, nil, err
	}
	recheckedInfo, err := os.Lstat(path)
	if err != nil {
		return nil, nil, err
	}
	if recheckedInfo.Mode()&os.ModeSymlink != 0 || !os.SameFile(openedInfo, recheckedInfo) {
		err := fmt.Errorf("managed providers config %q changed while opening it", path)
		return nil, nil, NewConfigError(ConfigErrorManagedStore, "", err.Error(), err)
	}
	body, err := io.ReadAll(file)
	if err != nil {
		return nil, nil, err
	}
	return body, openedInfo, nil
}

func rejectManagedProvidersConfigDestinationAlias(managedPath, bootstrapPath string) error {
	managedPath = filepath.Clean(managedPath)
	if bootstrapPath != "" {
		bootstrapPath = filepath.Clean(bootstrapPath)
		if managedPath == bootstrapPath {
			return managedSourceConflict("/source/id", "managed destination aliases the bootstrap path", nil)
		}
	}
	managedInfo, err := os.Lstat(managedPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return NewConfigError(ConfigErrorManagedStore, "", fmt.Sprintf("inspect managed providers config %q: %v", managedPath, err), err)
	}
	if managedInfo.Mode()&os.ModeSymlink != 0 {
		err := fmt.Errorf("managed providers config %q must not be a symbolic link", managedPath)
		return NewConfigError(ConfigErrorManagedStore, "", err.Error(), err)
	}
	if !managedInfo.Mode().IsRegular() {
		err := fmt.Errorf("managed providers config %q is not a regular file", managedPath)
		return NewConfigError(ConfigErrorManagedStore, "", err.Error(), err)
	}
	if bootstrapPath == "" {
		return nil
	}
	bootstrapInfo, err := os.Stat(bootstrapPath)
	if err != nil {
		return managedSourceConflict("/source/bootstrap_digest", "cannot inspect bootstrap source", err)
	}
	if os.SameFile(managedInfo, bootstrapInfo) {
		return managedSourceConflict("/source/id", "managed destination is a hard link to the bootstrap file", nil)
	}
	return nil
}

func ensureManagedProvidersConfigDirectory(dir string) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		wrapped := fmt.Errorf("create managed providers config directory %q: %w", dir, err)
		return NewConfigError(ConfigErrorManagedStore, "", wrapped.Error(), wrapped)
	}
	info, err := os.Lstat(dir)
	if err != nil {
		return NewConfigError(ConfigErrorManagedStore, "", fmt.Sprintf("inspect managed providers config directory %q: %v", dir, err), err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		err := fmt.Errorf("managed providers config directory %q must be a real directory", dir)
		return NewConfigError(ConfigErrorManagedStore, "", err.Error(), err)
	}
	if err := hardenManagedProvidersConfigDirectory(dir); err != nil {
		wrapped := fmt.Errorf("restrict managed providers config directory %q: %w", dir, err)
		return NewConfigError(ConfigErrorManagedStore, "", wrapped.Error(), wrapped)
	}
	return nil
}

func resetManagedProvidersConfigUnconditionally(ctx context.Context, path string) (warning error, err error) {
	dir := filepath.Dir(path)
	info, statErr := os.Lstat(dir)
	if errors.Is(statErr, os.ErrNotExist) {
		return nil, nil
	}
	if statErr != nil {
		return nil, NewConfigError(ConfigErrorManagedStore, "", fmt.Sprintf("inspect managed providers config directory %q: %v", dir, statErr), statErr)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		err := fmt.Errorf("managed providers config directory %q must be a real directory", dir)
		return nil, NewConfigError(ConfigErrorManagedStore, "", err.Error(), err)
	}
	lock, lockErr := acquireManagedProvidersConfigFileLock(ctx, path+".lock")
	if lockErr != nil {
		return nil, NewConfigError(ConfigErrorManagedStore, "", lockErr.Error(), lockErr)
	}
	defer func() {
		if releaseErr := lock.release(); releaseErr != nil {
			if err != nil {
				err = errors.Join(err, releaseErr)
			} else {
				warning = errors.Join(warning, releaseErr)
			}
		}
	}()
	pathInfo, lstatErr := os.Lstat(path)
	if errors.Is(lstatErr, os.ErrNotExist) {
		return warning, nil
	}
	if lstatErr != nil {
		return nil, NewConfigError(ConfigErrorManagedStore, "", fmt.Sprintf("inspect managed providers config %q: %v", path, lstatErr), lstatErr)
	}
	if pathInfo.IsDir() {
		err := fmt.Errorf("managed providers config %q is a directory", path)
		return nil, NewConfigError(ConfigErrorManagedStore, "", err.Error(), err)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := os.Remove(path); err != nil {
		wrapped := fmt.Errorf("remove managed providers config %q: %w", path, err)
		return nil, NewConfigError(ConfigErrorManagedStore, "", wrapped.Error(), wrapped)
	}
	if syncErr := syncManagedProvidersConfigDirectory(dir); syncErr != nil {
		warning = fmt.Errorf("managed providers config reset but directory sync failed: %w", syncErr)
	}
	return warning, nil
}

// ProbeManagedProvidersConfigWritable verifies that the managed destination's
// real parent directory satisfies the same symlink and permission policy used
// by Commit, then creates, hardens, syncs, closes, and removes a harmless probe
// file in that directory. It never writes provider configuration data.
func ProbeManagedProvidersConfigWritable(path string) (err error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return NewConfigError(ConfigErrorManagedStore, "", "managed providers config path is empty", nil)
	}
	dir := filepath.Dir(path)
	if err := ensureManagedProvidersConfigDirectory(dir); err != nil {
		return err
	}
	probe, err := os.CreateTemp(dir, ".dashboard-config-write-probe-*")
	if err != nil {
		wrapped := fmt.Errorf("create managed providers config write probe in %q: %w", dir, err)
		return NewConfigError(ConfigErrorManagedStore, "", wrapped.Error(), wrapped)
	}
	probePath := probe.Name()
	removeProbe := true
	probeClosed := false
	defer func() {
		if !probeClosed {
			if closeErr := probe.Close(); closeErr != nil && err == nil {
				err = NewConfigError(ConfigErrorManagedStore, "", fmt.Sprintf("close managed providers config write probe: %v", closeErr), closeErr)
			}
		}
		if removeProbe {
			if removeErr := os.Remove(probePath); removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) && err == nil {
				err = NewConfigError(ConfigErrorManagedStore, "", fmt.Sprintf("remove managed providers config write probe: %v", removeErr), removeErr)
			}
		}
	}()
	if err := hardenManagedProvidersConfigFile(probe); err != nil {
		return NewConfigError(ConfigErrorManagedStore, "", fmt.Sprintf("restrict managed providers config write probe: %v", err), err)
	}
	if _, err := probe.Write([]byte("vekil")); err != nil {
		return NewConfigError(ConfigErrorManagedStore, "", fmt.Sprintf("write managed providers config probe: %v", err), err)
	}
	if err := probe.Sync(); err != nil {
		return NewConfigError(ConfigErrorManagedStore, "", fmt.Sprintf("sync managed providers config probe: %v", err), err)
	}
	if err := probe.Close(); err != nil {
		return NewConfigError(ConfigErrorManagedStore, "", fmt.Sprintf("close managed providers config probe: %v", err), err)
	}
	probeClosed = true
	if err := os.Remove(probePath); err != nil {
		return NewConfigError(ConfigErrorManagedStore, "", fmt.Sprintf("remove managed providers config probe: %v", err), err)
	}
	removeProbe = false
	return nil
}
