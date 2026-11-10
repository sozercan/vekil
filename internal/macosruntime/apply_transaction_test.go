package macosruntime

import (
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func prepareApplyTransactionForTest(t *testing.T, operationID string, wasRunning bool) (*ConfigManager, *ManagedApplyTransaction, string, ManagedCandidate) {
	t.Helper()
	manager := newManagerForTest(t, "owner", "copilot-uuid", "local-uuid")
	description, err := manager.EnsureManagedConfiguration()
	if err != nil {
		t.Fatal(err)
	}
	candidate, err := manager.BuildManagedCandidate(managedApplyDraft(manager.State().Providers[0].UUID, true), 1)
	if err != nil {
		t.Fatal(err)
	}
	tx, err := manager.PrepareManagedApply(operationID, candidate, description.SelectedRevision, wasRunning)
	if err != nil {
		t.Fatal(err)
	}
	return manager, tx, description.SelectedRevision, candidate
}

func TestManagedApplyInstallPreservesPersistenceErrorAfterSuccessfulRollback(t *testing.T) {
	for _, test := range []struct {
		name   string
		inject func(*ManagedApplyTransaction, error)
	}{
		{
			name: "state save",
			inject: func(tx *ManagedApplyTransaction, primary error) {
				tx.saveInstalledState = func() error { return primary }
			},
		},
		{
			name: "installed journal",
			inject: func(tx *ManagedApplyTransaction, primary error) {
				tx.writeInstalledJournal = func(string, ApplyJournal) error { return primary }
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			manager, tx, oldRevision, _ := prepareApplyTransactionForTest(t, "op_"+strings.ReplaceAll(test.name, " ", "_"), false)
			primary := errors.New(test.name + " failed")
			test.inject(tx, primary)

			err := tx.Install()
			if !errors.Is(err, primary) {
				t.Fatalf("Install() error = %v, want primary error %v", err, primary)
			}
			if err.Error() != primary.Error() {
				t.Fatalf("Install() error = %q, want only primary error %q after successful rollback", err, primary)
			}
			if got := manager.State().CommittedConfigRevision; got != oldRevision {
				t.Fatalf("committed revision = %q, want restored %q", got, oldRevision)
			}
			body, readErr := os.ReadFile(manager.paths.Managed)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if revision, _ := configRevision(body); revision != oldRevision {
				t.Fatalf("managed file revision = %q, want restored %q", revision, oldRevision)
			}
			if _, statErr := os.Stat(manager.paths.Journal); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("journal remains after successful rollback: %v", statErr)
			}
		})
	}
}

func TestManagedApplyInstallPreservesPrimaryAndRollbackErrors(t *testing.T) {
	manager, tx, _, _ := prepareApplyTransactionForTest(t, "op_rollback_failure", false)
	primary := errors.New("state save failed")
	tx.saveInstalledState = func() error {
		if err := os.WriteFile(manager.paths.Backup, []byte("corrupt backup\n"), 0o600); err != nil {
			return err
		}
		return primary
	}

	err := tx.Install()
	if !errors.Is(err, primary) {
		t.Fatalf("Install() error = %v, want primary error %v", err, primary)
	}
	if !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("Install() error = %v, want combined rollback failure", err)
	}
	state := manager.State()
	if state.RecoveryState != string(ApplyPhaseRollbackFailed) || state.RecoveryPrimaryCode != "state_install_failed" || state.RecoveryRollbackCode == "" {
		t.Fatalf("recovery state = %+v", state)
	}
}

func TestManagedApplyInstallJournalFailureWhileRunningRetainsRuntimeRollback(t *testing.T) {
	manager, tx, oldRevision, _ := prepareApplyTransactionForTest(t, "op_running_state_failure", true)
	primary := errors.New("installed journal failed")
	tx.writeInstalledJournal = func(string, ApplyJournal) error { return primary }

	err := tx.Install()
	if !errors.Is(err, primary) || err.Error() != primary.Error() {
		t.Fatalf("Install() error = %v, want primary error %v", err, primary)
	}
	if got := manager.State().CommittedConfigRevision; got != oldRevision {
		t.Fatalf("committed revision = %q, want restored %q", got, oldRevision)
	}
	if !tx.restored || tx.finished {
		t.Fatalf("transaction state restored=%t finished=%t", tx.restored, tx.finished)
	}
	if err := tx.RestoreForRuntimeRollback("state_install_failed"); err != nil {
		t.Fatalf("idempotent RestoreForRuntimeRollback() error = %v", err)
	}
	if err := tx.FinishRuntimeRollback(); err != nil {
		t.Fatalf("FinishRuntimeRollback() error = %v", err)
	}
	if _, statErr := os.Stat(manager.paths.Journal); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("journal remains after runtime rollback finished: %v", statErr)
	}
}

func TestRemoveApplyArtifactsMakesJournalRemovalTheDurableBoundary(t *testing.T) {
	paths := PathsInDirectory("/test/state")
	var removed []string
	if err := removeApplyArtifactsWith(paths, true, func(path string) error {
		removed = append(removed, path)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	want := []string{paths.Journal, paths.Staged, paths.Backup}
	if !reflect.DeepEqual(removed, want) {
		t.Fatalf("removal order = %v, want %v", removed, want)
	}

	injected := errors.New("journal fsync failed")
	removed = nil
	err := removeApplyArtifactsWith(paths, true, func(path string) error {
		removed = append(removed, path)
		if path == paths.Journal {
			return injected
		}
		return nil
	})
	if !errors.Is(err, injected) {
		t.Fatalf("removeApplyArtifactsWith() error = %v, want %v", err, injected)
	}
	if !reflect.DeepEqual(removed, []string{paths.Journal}) {
		t.Fatalf("removals after journal failure = %v, want journal only", removed)
	}
}
