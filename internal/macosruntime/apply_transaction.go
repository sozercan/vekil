package macosruntime

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"

	"github.com/sozercan/vekil/proxy"
)

// ManagedApplyTransaction is a durable install transaction. Runtime stop/start
// orchestration stays outside this type so the controller can preserve prior
// running intent.
type ManagedApplyTransaction struct {
	mu sync.Mutex

	manager   *ConfigManager
	journal   ApplyJournal
	candidate ManagedCandidate
	installed bool
	restored  bool
	finished  bool

	// Install persistence seams keep failure-path tests deterministic without
	// weakening the production filesystem implementation.
	saveInstalledState    func() error
	writeInstalledJournal func(string, ApplyJournal) error
}

// PrepareManagedApply verifies ownership and expected revision, stages the
// exact candidate bytes, and writes the durable prepared journal.
func (m *ConfigManager) PrepareManagedApply(operationID string, candidate ManagedCandidate, expectedRevision string, wasRunning bool) (*ManagedApplyTransaction, error) {
	operationID = strings.TrimSpace(operationID)
	if operationID == "" {
		return nil, errors.New("operation id is required")
	}
	if len(candidate.Bytes) == 0 || candidate.Revision == "" || candidate.SHA256 == "" {
		return nil, errors.New("managed candidate is incomplete")
	}
	if candidate.SecretGeneration == 0 {
		return nil, errors.New("managed candidate secret generation is required")
	}
	if _, err := proxyConfigFromCandidate(m.paths.Managed, candidate); err != nil {
		return nil, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.RecoveryState != "" {
		return nil, fmt.Errorf("managed recovery is required: %s", m.state.RecoveryState)
	}
	if m.state.ConfigMode != ConfigModeManaged || m.state.ManagedOwnershipID == "" {
		return nil, errors.New("managed configuration is not selected and owned")
	}
	if expectedRevision != "" && expectedRevision != m.state.CommittedConfigRevision {
		return nil, fmt.Errorf("expected managed revision %q, found %q", expectedRevision, m.state.CommittedConfigRevision)
	}
	if candidate.SecretGeneration <= m.state.SecretGeneration {
		return nil, fmt.Errorf("secret generation must advance beyond %d", m.state.SecretGeneration)
	}
	oldBody, _, err := readOwnedFile(m.paths.Managed, MaxConfigBytes)
	if err != nil {
		return nil, err
	}
	oldRevision, oldDigest := configRevision(oldBody)
	if oldRevision != m.state.CommittedConfigRevision || oldDigest != m.state.CommittedSHA256 {
		return nil, errors.New("managed configuration is drifted")
	}
	if _, err := os.Lstat(m.paths.Journal); err == nil {
		return nil, errors.New("managed apply journal already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect managed apply journal: %w", err)
	}
	_ = removePrivateFile(m.paths.Staged)
	_ = removePrivateFile(m.paths.Backup)
	if err := writeAtomicFile(m.paths.Staged, candidate.Bytes); err != nil {
		return nil, err
	}
	journal := ApplyJournal{
		Version:             ApplyJournalVersion,
		OperationID:         operationID,
		Phase:               ApplyPhasePrepared,
		OldRevision:         oldRevision,
		NewRevision:         candidate.Revision,
		OldSHA256:           oldDigest,
		NewSHA256:           candidate.SHA256,
		OldSecretGeneration: m.state.SecretGeneration,
		NewSecretGeneration: candidate.SecretGeneration,
		WasRunning:          wasRunning,
		OldProviders:        cloneProviderIdentities(m.state.Providers),
		NewProviders:        cloneProviderIdentities(candidate.Providers),
	}
	if err := writeApplyJournal(m.paths.Journal, journal); err != nil {
		_ = removePrivateFile(m.paths.Staged)
		return nil, err
	}
	return &ManagedApplyTransaction{manager: m, journal: journal, candidate: candidate}, nil
}

// Install atomically replaces the owned managed file after re-verifying the
// same open-once old and staged snapshots.
func (t *ManagedApplyTransaction) Install() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return errors.New("managed apply transaction is finished")
	}
	if t.installed {
		return nil
	}
	m := t.manager
	m.mu.Lock()
	defer m.mu.Unlock()

	journal, err := readApplyJournal(m.paths.Journal)
	if err != nil {
		return err
	}
	if journal.OperationID != t.journal.OperationID || journal.Phase != ApplyPhasePrepared {
		return errors.New("managed apply journal no longer matches prepared transaction")
	}
	oldBody, _, err := readOwnedFile(m.paths.Managed, MaxConfigBytes)
	if err != nil {
		return err
	}
	oldRevision, oldDigest := configRevision(oldBody)
	if oldRevision != journal.OldRevision || oldDigest != journal.OldSHA256 {
		return errors.New("managed configuration changed after validation")
	}
	stagedBody, _, err := readOwnedFile(m.paths.Staged, MaxConfigBytes)
	if err != nil {
		return err
	}
	newRevision, newDigest := configRevision(stagedBody)
	if newRevision != journal.NewRevision || newDigest != journal.NewSHA256 || !bytes.Equal(stagedBody, t.candidate.Bytes) {
		return errors.New("staged managed configuration changed after validation")
	}
	if err := writeExclusiveFile(m.paths.Backup, oldBody); err != nil {
		return err
	}
	if err := writeAtomicFile(m.paths.Managed, stagedBody); err != nil {
		_ = removePrivateFile(m.paths.Backup)
		return err
	}

	m.state.CommittedConfigRevision = journal.NewRevision
	m.state.CommittedSHA256 = journal.NewSHA256
	m.state.SelectedConfigRevision = journal.NewRevision
	m.state.SecretGeneration = journal.NewSecretGeneration
	m.state.Providers = cloneProviderIdentities(journal.NewProviders)
	saveInstalledState := m.saveStateLocked
	if t.saveInstalledState != nil {
		saveInstalledState = t.saveInstalledState
	}
	if err := saveInstalledState(); err != nil {
		return t.rollbackInstallFailureLocked(err, "state_install_failed")
	}
	journal.Phase = ApplyPhaseInstalled
	writeInstalledJournal := writeApplyJournal
	if t.writeInstalledJournal != nil {
		writeInstalledJournal = t.writeInstalledJournal
	}
	if err := writeInstalledJournal(m.paths.Journal, journal); err != nil {
		return t.rollbackInstallFailureLocked(err, "journal_install_failed")
	}
	t.journal = journal
	t.installed = true
	return nil
}

// Commit marks the installed generation durable and removes rollback material.
func (t *ManagedApplyTransaction) Commit() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return nil
	}
	if !t.installed {
		return errors.New("managed apply transaction is not installed")
	}
	m := t.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	journal, err := readApplyJournal(m.paths.Journal)
	if err != nil {
		return err
	}
	if journal.OperationID != t.journal.OperationID || journal.Phase != ApplyPhaseInstalled {
		return errors.New("managed apply journal no longer matches installed transaction")
	}
	journal.Phase = ApplyPhaseCommitted
	if err := writeApplyJournal(m.paths.Journal, journal); err != nil {
		return err
	}
	// The committed journal is the durable point of no return. Artifact cleanup
	// is idempotent and recoverApplyJournalLocked retries it on the next launch;
	// a cleanup failure must never make callers roll back a runtime that is
	// already serving the committed candidate.
	t.journal = journal
	t.finished = true
	_ = removeApplyArtifacts(m.paths, true)
	return nil
}

// Rollback restores previous bytes, identities, and secret generation.
func (t *ManagedApplyTransaction) Rollback(primaryFailureCode string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return nil
	}
	m := t.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	return t.rollbackLocked(primaryFailureCode, "")
}

// RestoreForRuntimeRollback restores the old file and state but intentionally
// retains the journal, backup, staged bytes, and both secret generations until
// the previous runtime has been proven ready again.
func (t *ManagedApplyTransaction) RestoreForRuntimeRollback(primaryFailureCode string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return errors.New("managed apply transaction is finished")
	}
	if t.restored {
		return nil
	}
	m := t.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	return t.restoreForRuntimeRollbackLocked(primaryFailureCode)
}

func (t *ManagedApplyTransaction) restoreForRuntimeRollbackLocked(primaryFailureCode string) error {
	m := t.manager
	journal, err := readApplyJournal(m.paths.Journal)
	if err != nil {
		return err
	}
	backup, _, err := readOwnedFile(m.paths.Backup, MaxConfigBytes)
	if err == nil {
		revision, digest := configRevision(backup)
		if revision != journal.OldRevision || digest != journal.OldSHA256 {
			return errors.New("rollback backup revision mismatch")
		}
		if err := writeAtomicFile(m.paths.Managed, backup); err != nil {
			return err
		}
	} else if journal.Phase == ApplyPhasePrepared {
		current, _, currentErr := readOwnedFile(m.paths.Managed, MaxConfigBytes)
		if currentErr != nil {
			return currentErr
		}
		revision, digest := configRevision(current)
		if revision != journal.OldRevision || digest != journal.OldSHA256 {
			return errors.New("managed file changed before runtime rollback")
		}
	} else {
		return err
	}
	restoreStateFromJournal(&m.state, journal)
	if err := m.saveStateLocked(); err != nil {
		return err
	}
	journal.PrimaryFailureCode = sanitizeFailureCode(primaryFailureCode)
	if err := writeApplyJournal(m.paths.Journal, journal); err != nil {
		return err
	}
	t.journal = journal
	t.installed = false
	t.restored = true
	return nil
}

func (t *ManagedApplyTransaction) rollbackInstallFailureLocked(primary error, primaryFailureCode string) error {
	var rollbackErr error
	if t.journal.WasRunning {
		// Keep rollback material until the caller has restored the previously
		// running runtime and proven it ready again.
		rollbackErr = t.restoreForRuntimeRollbackLocked(primaryFailureCode)
	} else {
		rollbackErr = t.rollbackLocked(primaryFailureCode, "")
	}
	if rollbackErr != nil {
		return errors.Join(primary, rollbackErr)
	}
	return primary
}

// FinishRuntimeRollback removes retained transaction material after the old
// runtime has returned to readiness.
func (t *ManagedApplyTransaction) FinishRuntimeRollback() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.finished {
		return nil
	}
	if !t.restored {
		return errors.New("managed apply transaction has not restored the prior configuration")
	}
	t.manager.mu.Lock()
	defer t.manager.mu.Unlock()
	if err := removeApplyArtifacts(t.manager.paths, true); err != nil {
		return err
	}
	t.finished = true
	return nil
}

// MarkRollbackFailed retains the journal and both secret generations while
// exposing allowlisted primary and rollback failure codes in helper state.
func (t *ManagedApplyTransaction) MarkRollbackFailed(primaryFailureCode, rollbackFailureCode string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	m := t.manager
	m.mu.Lock()
	defer m.mu.Unlock()
	journal, err := readApplyJournal(m.paths.Journal)
	if err != nil {
		return err
	}
	journal.Phase = ApplyPhaseRollbackFailed
	journal.PrimaryFailureCode = sanitizeFailureCode(primaryFailureCode)
	journal.RollbackFailureCode = sanitizeFailureCode(rollbackFailureCode)
	if journal.RollbackFailureCode == "" {
		journal.RollbackFailureCode = "rollback_failed"
	}
	if err := writeApplyJournal(m.paths.Journal, journal); err != nil {
		return err
	}
	m.state.RecoveryState = string(ApplyPhaseRollbackFailed)
	m.state.RecoveryPrimaryCode = journal.PrimaryFailureCode
	m.state.RecoveryRollbackCode = journal.RollbackFailureCode
	return m.saveStateLocked()
}

func (t *ManagedApplyTransaction) rollbackLocked(primaryFailureCode, rollbackFailureCode string) error {
	m := t.manager
	journal, err := readApplyJournal(m.paths.Journal)
	if err != nil {
		return err
	}
	journal.PrimaryFailureCode = sanitizeFailureCode(primaryFailureCode)
	if backupBody, _, backupErr := readOwnedFile(m.paths.Backup, MaxConfigBytes); backupErr == nil {
		revision, digest := configRevision(backupBody)
		if revision != journal.OldRevision || digest != journal.OldSHA256 {
			err = errors.New("rollback backup revision mismatch")
		} else {
			err = writeAtomicFile(m.paths.Managed, backupBody)
		}
	} else if journal.Phase == ApplyPhasePrepared {
		// Installation did not create a backup; the original file is still active.
		current, _, currentErr := readOwnedFile(m.paths.Managed, MaxConfigBytes)
		if currentErr != nil {
			err = currentErr
		} else {
			revision, digest := configRevision(current)
			if revision != journal.OldRevision || digest != journal.OldSHA256 {
				err = errors.New("managed file changed before rollback")
			}
		}
	} else {
		err = backupErr
	}
	if err == nil {
		restoreStateFromJournal(&m.state, journal)
		err = m.saveStateLocked()
	}
	if err == nil {
		err = removeApplyArtifacts(m.paths, true)
	}
	if err == nil {
		t.finished = true
		t.installed = false
		return nil
	}

	journal.Phase = ApplyPhaseRollbackFailed
	journal.RollbackFailureCode = sanitizeFailureCode(rollbackFailureCode)
	if journal.RollbackFailureCode == "" {
		journal.RollbackFailureCode = "rollback_failed"
	}
	_ = writeApplyJournal(m.paths.Journal, journal)
	m.state.RecoveryState = string(ApplyPhaseRollbackFailed)
	m.state.RecoveryPrimaryCode = journal.PrimaryFailureCode
	m.state.RecoveryRollbackCode = journal.RollbackFailureCode
	_ = m.saveStateLocked()
	return fmt.Errorf("rollback failed: %w", err)
}

func (m *ConfigManager) recoverApplyJournalLocked() error {
	journal, err := readApplyJournal(m.paths.Journal)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(strings.ToLower(err.Error()), "no such file") {
			return nil
		}
		return err
	}
	switch journal.Phase {
	case ApplyPhaseCreating:
		return m.recoverInitialManagedCreationLocked(journal)
	case ApplyPhaseCommitted:
		return removeApplyArtifacts(m.paths, true)
	case ApplyPhasePrepared, ApplyPhaseInstalled, ApplyPhaseRollbackFailed:
		backup, _, backupErr := readOwnedFile(m.paths.Backup, MaxConfigBytes)
		if backupErr == nil {
			revision, digest := configRevision(backup)
			if revision != journal.OldRevision || digest != journal.OldSHA256 {
				backupErr = errors.New("rollback backup revision mismatch")
			} else {
				backupErr = writeAtomicFile(m.paths.Managed, backup)
			}
		} else if journal.Phase == ApplyPhasePrepared {
			current, _, currentErr := readOwnedFile(m.paths.Managed, MaxConfigBytes)
			if currentErr == nil {
				revision, digest := configRevision(current)
				if revision == journal.OldRevision && digest == journal.OldSHA256 {
					backupErr = nil
				}
			}
		}
		if backupErr != nil {
			m.state.RecoveryState = string(ApplyPhaseRollbackFailed)
			m.state.RecoveryPrimaryCode = journal.PrimaryFailureCode
			m.state.RecoveryRollbackCode = "rollback_failed"
			_ = m.saveStateLocked()
			return nil
		}
		restoreStateFromJournal(&m.state, journal)
		if err := m.saveStateLocked(); err != nil {
			return err
		}
		return removeApplyArtifacts(m.paths, true)
	default:
		return fmt.Errorf("unsupported managed apply phase %q", journal.Phase)
	}
}

func initialManagedCreationJournal() ApplyJournal {
	revision, digest := configRevision(initialManagedCopilotYAML)
	return ApplyJournal{
		Version:     ApplyJournalVersion,
		OperationID: "initial_managed_create",
		Phase:       ApplyPhaseCreating,
		NewRevision: revision,
		NewSHA256:   digest,
	}
}

func (m *ConfigManager) recoverInitialManagedCreationIfPresentLocked() error {
	journal, err := readApplyJournal(m.paths.Journal)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(strings.ToLower(err.Error()), "no such file") {
			return nil
		}
		return err
	}
	if journal.Phase != ApplyPhaseCreating {
		return nil
	}
	return m.recoverInitialManagedCreationLocked(journal)
}

func (m *ConfigManager) recoverInitialManagedCreationLocked(journal ApplyJournal) error {
	expected := initialManagedCreationJournal()
	if journal.OperationID != expected.OperationID ||
		journal.NewRevision != expected.NewRevision ||
		journal.NewSHA256 != expected.NewSHA256 {
		return errors.New("initial managed creation journal does not match the built-in configuration")
	}

	// Saving ownership state is the commit boundary. If only journal cleanup
	// was interrupted, preserve that state and let ordinary drift checks handle
	// a subsequently changed or removed managed file.
	if m.state.ManagedOwnershipID != "" {
		if m.state.CommittedConfigRevision != expected.NewRevision ||
			m.state.CommittedSHA256 != expected.NewSHA256 {
			return errors.New("initial managed creation journal conflicts with ownership state")
		}
		return removePrivateFile(m.paths.Journal)
	}

	body, _, err := readOwnedFile(m.paths.Managed, MaxConfigBytes)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || strings.Contains(strings.ToLower(err.Error()), "no such file") {
			return removePrivateFile(m.paths.Journal)
		}
		return err
	}
	revision, digest := configRevision(body)
	if revision != expected.NewRevision || digest != expected.NewSHA256 {
		return removeUncommittedInitialManagedCreation(m.paths)
	}

	previous := clonePersistentState(m.state)
	m.state.ConfigMode = ConfigModeManaged
	m.state.SelectedPath = m.paths.Managed
	m.state.SelectedConfigRevision = expected.NewRevision
	m.state.ManagedOwnershipID = m.uuid()
	m.state.CommittedConfigRevision = expected.NewRevision
	m.state.CommittedSHA256 = expected.NewSHA256
	m.state.Providers = []ProviderIdentity{{UUID: m.uuid(), ProviderID: "copilot"}}
	if err := m.saveStateLocked(); err != nil {
		m.state = previous
		return err
	}
	return removePrivateFile(m.paths.Journal)
}

func removeUncommittedInitialManagedCreation(paths Paths) error {
	if err := removePrivateFile(paths.Managed); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove incomplete initial managed configuration: %w", err)
	}
	if err := removePrivateFile(paths.Journal); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove initial managed creation journal: %w", err)
	}
	return nil
}

func restoreStateFromJournal(state *PersistentState, journal ApplyJournal) {
	state.CommittedConfigRevision = journal.OldRevision
	state.CommittedSHA256 = journal.OldSHA256
	state.SelectedConfigRevision = journal.OldRevision
	state.SecretGeneration = journal.OldSecretGeneration
	state.Providers = cloneProviderIdentities(journal.OldProviders)
	state.RecoveryState = ""
	state.RecoveryPrimaryCode = ""
	state.RecoveryRollbackCode = ""
}

func writeApplyJournal(path string, journal ApplyJournal) error {
	journal.Version = ApplyJournalVersion
	body, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("encode managed apply journal: %w", err)
	}
	return writeAtomicFile(path, append(body, '\n'))
}

func readApplyJournal(path string) (ApplyJournal, error) {
	body, _, err := readOwnedFile(path, MaxStateBytes)
	if err != nil {
		return ApplyJournal{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	var journal ApplyJournal
	if err := decoder.Decode(&journal); err != nil {
		return ApplyJournal{}, fmt.Errorf("decode managed apply journal: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("trailing JSON value")
		}
		return ApplyJournal{}, fmt.Errorf("decode managed apply journal: %w", err)
	}
	if journal.Version != ApplyJournalVersion {
		return ApplyJournal{}, fmt.Errorf("unsupported managed apply journal version %d", journal.Version)
	}
	return journal, nil
}

func removeApplyArtifacts(paths Paths, includeJournal bool) error {
	return removeApplyArtifactsWith(paths, includeJournal, removePrivateFile)
}

func removeApplyArtifactsWith(paths Paths, includeJournal bool, remove func(string) error) error {
	if remove == nil {
		return errors.New("apply artifact remover is required")
	}
	if includeJournal {
		if err := remove(paths.Journal); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %q: %w", paths.Journal, err)
		}
	}

	var errs []error
	for _, path := range []string{paths.Staged, paths.Backup} {
		if err := remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			errs = append(errs, fmt.Errorf("remove %q: %w", path, err))
		}
	}
	return errors.Join(errs...)
}

func sanitizeFailureCode(code string) string {
	code = strings.TrimSpace(code)
	if len(code) > 128 {
		code = code[:128]
	}
	var builder strings.Builder
	for _, r := range code {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			builder.WriteRune(r)
		}
	}
	return builder.String()
}

func cloneProviderIdentities(values []ProviderIdentity) []ProviderIdentity {
	cloned := append([]ProviderIdentity(nil), values...)
	for index := range cloned {
		cloned[index].SecretRoles = append([]string(nil), cloned[index].SecretRoles...)
	}
	return cloned
}

func proxyConfigFromCandidate(name string, candidate ManagedCandidate) (any, error) {
	cfg, err := proxy.LoadProvidersConfigBytes(name, candidate.Bytes)
	if err != nil {
		return nil, err
	}
	revision, digest := configRevision(candidate.Bytes)
	if revision != candidate.Revision || digest != candidate.SHA256 {
		return nil, errors.New("managed candidate revision mismatch")
	}
	return cfg, nil
}
