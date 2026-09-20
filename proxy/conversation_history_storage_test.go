package proxy

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func newConversationHistoryStorageFixture(t *testing.T, config ConversationMigrationConfig) (*stateBindingStore, *conversationHistoryStore, DurableStateBindingsConfig) {
	t.Helper()
	bindings, file := newDurableStoreFixture(t, 64)
	history, err := newConversationHistoryStore(bindings.durable, config)
	if err != nil {
		t.Fatal(err)
	}
	return bindings, history, file
}

func reopenConversationHistoryStorageFixture(t *testing.T, file DurableStateBindingsConfig, config ConversationMigrationConfig) (*stateBindingStore, *conversationHistoryStore) {
	t.Helper()
	bindings, err := newDurableStateBindingStore(file)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeDurableStoreFixture(t, bindings) })
	history, err := newConversationHistoryStore(bindings.durable, config)
	if err != nil {
		t.Fatal(err)
	}
	return bindings, history
}

func conversationHistoryStorageSnapshot(history *conversationHistoryStore, responseID, root string, created time.Time) *conversationSnapshot {
	owner := durableFixtureOwner()
	snapshot := &conversationSnapshot{
		ResponseID: responseID, RouteID: owner.routeID, TargetID: owner.targetID, Identity: owner.identity,
		Root: root, Scope: "storage-test-scope", Created: created.Unix(),
		Input: []json.RawMessage{
			json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"remember the full context"}]}`),
			json.RawMessage(fmt.Sprintf(`{"type":"message","role":"assistant","content":[{"type":"output_text","text":%q}]}`, responseID)),
		},
		Instructions: json.RawMessage(`"preserve these instructions"`),
		Tools:        json.RawMessage(`[{"type":"function","name":"read","parameters":{"type":"object"}}]`),
	}
	snapshot.Indexes = history.anchorIndexes(snapshot.RouteID, []conversationAnchor{{kind: "item", value: "message-" + responseID}})
	prefixes := history.prefixIndexes(snapshot.RouteID, snapshot.Scope, snapshot.Input)
	snapshot.Indexes = append(snapshot.Indexes, prefixes[len(prefixes)-1])
	return snapshot
}

func saveConversationHistoryStorageSnapshot(t *testing.T, history *conversationHistoryStore, snapshot *conversationSnapshot) {
	t.Helper()
	if err := history.acquire(snapshot.Root); err != nil {
		t.Fatal(err)
	}
	defer history.release(snapshot.Root)
	if err := history.beginAttempt(snapshot.Root, "operation-"+snapshot.ResponseID); err != nil {
		t.Fatal(err)
	}
	if err := history.save(snapshot); err != nil {
		t.Fatal(err)
	}
}

func requireConversationHistoryStorageSnapshot(t *testing.T, history *conversationHistoryStore, want *conversationSnapshot) {
	t.Helper()
	got, err := history.lookupResponse(want.RouteID, want.ResponseID)
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Fatalf("lookup %q = %+v, %v; want %+v", want.ResponseID, got, err, want)
	}
}

func TestConversationHistoryStorageReopenRetainsCompleteSnapshots(t *testing.T) {
	bindings, history, file := newConversationHistoryStorageFixture(t, ConversationMigrationConfig{})
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	first := conversationHistoryStorageSnapshot(history, "response-first", "shared-root", old)
	second := conversationHistoryStorageSnapshot(history, "response-second", first.Root, old.Add(time.Minute))
	second.Input = append(cloneRawMessages(first.Input), second.Input...)
	second.TargetID, second.Identity, second.Migrated = "secondary-target", [32]byte{9}, true
	for _, snapshot := range []*conversationSnapshot{first, second} {
		saveConversationHistoryStorageSnapshot(t, history, snapshot)
		owner := stateBindingOwner{routeID: snapshot.RouteID, targetID: snapshot.TargetID, identity: snapshot.Identity}
		if result := bindings.bindAll([]stateBindingToken{{stateBindingTypeResponseID, snapshot.ResponseID}}, owner); result.err != nil {
			t.Fatal(result.err)
		}
	}
	closeDurableStoreFixture(t, bindings)
	reopened, retained := reopenConversationHistoryStorageFixture(t, file, history.config)
	reopened.durable.now = func() time.Time { return old.Add(50 * 365 * 24 * time.Hour) }
	for _, snapshot := range []*conversationSnapshot{first, second} {
		requireConversationHistoryStorageSnapshot(t, retained, snapshot)
		byIndex, err := retained.lookupIndexes(snapshot.Indexes)
		if err != nil || !reflect.DeepEqual(byIndex, snapshot) {
			t.Fatalf("index lookup %q = %+v, %v", snapshot.ResponseID, byIndex, err)
		}
		owner := stateBindingOwner{routeID: snapshot.RouteID, targetID: snapshot.TargetID, identity: snapshot.Identity}
		if result := reopened.lookup(stateBindingTypeResponseID, snapshot.ResponseID); result.err != nil || result.owner != reopened.durable.encodeOwner(owner) {
			t.Fatalf("ownership after reopen = %+v", result)
		}
	}
	got, err := retained.lookupResponse(first.RouteID, first.ResponseID)
	if err != nil {
		t.Fatal(err)
	}
	got.Input[0][0] = '!'
	got.Indexes[0][0] ^= 1
	requireConversationHistoryStorageSnapshot(t, retained, first)
	if _, err := retained.lookupResponse("different-route", first.ResponseID); !errors.Is(err, errConversationHistoryMissing) {
		t.Fatalf("cross-route history = %v", err)
	}
	if err := retained.acquire(first.Root); err != nil {
		t.Fatalf("committed snapshot retained a pending attempt: %v", err)
	}
	retained.release(first.Root)
}

func TestConversationHistoryStorageCapacityNeverEvicts(t *testing.T) {
	for _, bound := range []string{"snapshots", "total bytes", "history bytes"} {
		t.Run(bound, func(t *testing.T) {
			bindings, history, file := newConversationHistoryStorageFixture(t, ConversationMigrationConfig{MaxSnapshots: 2})
			first := conversationHistoryStorageSnapshot(history, "capacity-first", "first-root", time.Now())
			saveConversationHistoryStorageSnapshot(t, history, first)
			second := conversationHistoryStorageSnapshot(history, "capacity-second", "second-root", time.Now())
			switch bound {
			case "snapshots":
				if err := history.beginAttempt(second.Root, "second-operation"); err != nil {
					t.Fatal(err)
				}
				if err := history.beginAttempt("third-root", "third-operation"); !errors.Is(err, errConversationHistoryCapacity) {
					t.Fatalf("pending attempt did not consume capacity: %v", err)
				}
				if err := history.save(second); err != nil {
					t.Fatalf("completion could not replace its pending reservation: %v", err)
				}
				third := conversationHistoryStorageSnapshot(history, "capacity-third", "third-root", time.Now())
				if err := history.save(third); !errors.Is(err, errConversationHistoryCapacity) {
					t.Fatalf("snapshot capacity = %v", err)
				}
				requireConversationHistoryStorageSnapshot(t, history, second)
			case "total bytes":
				encoded, err := history.encode(history.responseKey(first.RouteID, first.ResponseID), first)
				if err != nil {
					t.Fatal(err)
				}
				history.config.MaxTotalBytes = int64(conversationSnapshotCost(first, encoded) + conversationPendingBytes - 1)
				if err := history.beginAttempt(second.Root, "second-operation"); !errors.Is(err, errConversationHistoryCapacity) {
					t.Fatalf("pending byte capacity = %v", err)
				}
				if err := history.save(second); !errors.Is(err, errConversationHistoryCapacity) {
					t.Fatalf("snapshot byte capacity = %v", err)
				}
			case "history bytes":
				encoded, err := history.encode(history.responseKey(first.RouteID, first.ResponseID), first)
				if err != nil {
					t.Fatal(err)
				}
				history.config.MaxHistoryBytes = len(encoded)
				second.Instructions, _ = json.Marshal(strings.Repeat("large instructions ", len(encoded)))
				if err := history.beginAttempt(second.Root, "second-operation"); err != nil {
					t.Fatal(err)
				}
				if err := history.save(second); !errors.Is(err, errConversationHistoryCapacity) {
					t.Fatalf("history capacity = %v", err)
				}
				if err := history.acquire(second.Root); !errors.Is(err, errConversationHistoryUncertain) {
					t.Fatalf("failed save cleared pending execution: %v", err)
				}
			}
			requireConversationHistoryStorageSnapshot(t, history, first)
			if history.d.failed != nil {
				t.Fatal("capacity failure poisoned the durable store")
			}
			closeDurableStoreFixture(t, bindings)
			_, retained := reopenConversationHistoryStorageFixture(t, file, history.config)
			requireConversationHistoryStorageSnapshot(t, retained, first)
		})
	}
}

func TestConversationHistoryStorageFailureBarriers(t *testing.T) {
	for _, operation := range []string{"begin", "save", "clear"} {
		for _, after := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/after=%t", operation, after), func(t *testing.T) {
				bindings, history, file := newConversationHistoryStorageFixture(t, ConversationMigrationConfig{})
				first := conversationHistoryStorageSnapshot(history, "prior-committed", "fault-root", time.Now())
				saveConversationHistoryStorageSnapshot(t, history, first)
				if result := bindings.bindAll([]stateBindingToken{{stateBindingTypeResponseID, first.ResponseID}}, durableFixtureOwner()); result.err != nil {
					t.Fatal(result.err)
				}
				second := conversationHistoryStorageSnapshot(history, "fault-completion", first.Root, time.Now())
				if operation != "begin" {
					if err := history.beginAttempt(first.Root, "fault-operation"); err != nil {
						t.Fatal(err)
					}
				}
				fault := func() error { return syscall.ENOSPC }
				if after {
					bindings.durable.afterCommit = fault
				} else {
					bindings.durable.beforeCommit = fault
				}
				var err error
				switch operation {
				case "begin":
					err = history.beginAttempt(first.Root, "fault-operation")
				case "save":
					err = history.save(second)
				case "clear":
					err = history.clearAttempt(first.Root)
				}
				if !errors.Is(err, errConversationHistoryStorage) {
					t.Fatalf("failed transaction returned %v", err)
				}
				if _, err := history.lookupResponse(first.RouteID, first.ResponseID); !errors.Is(err, errConversationHistoryStorage) {
					t.Fatalf("failed history store remained readable: %v", err)
				}
				if result := bindings.lookup(stateBindingTypeResponseID, first.ResponseID); !errors.Is(result.err, errDurableStateIO) {
					t.Fatalf("shared ownership store did not fail closed: %+v", result)
				}
				closeDurableStoreFixture(t, bindings)
				_, retained := reopenConversationHistoryStorageFixture(t, file, history.config)
				requireConversationHistoryStorageSnapshot(t, retained, first)
				if operation == "save" && after {
					requireConversationHistoryStorageSnapshot(t, retained, second)
				} else if _, err := retained.lookupResponse(second.RouteID, second.ResponseID); !errors.Is(err, errConversationHistoryMissing) {
					t.Fatalf("uncommitted completion became visible: %v", err)
				}
				wantPending := !after
				if operation == "begin" {
					wantPending = after
				}
				err = retained.acquire(first.Root)
				if wantPending && !errors.Is(err, errConversationHistoryUncertain) || !wantPending && err != nil {
					t.Fatalf("pending after reopen = %v, want pending %t", err, wantPending)
				}
				retained.release(first.Root)
			})
		}
	}
}

func TestConversationHistoryStorageCountsFollowCommittedRecords(t *testing.T) {
	for _, operation := range []string{"begin", "save", "clear"} {
		t.Run(operation, func(t *testing.T) {
			bindings, history, file := newConversationHistoryStorageFixture(t, ConversationMigrationConfig{MaxSnapshots: 2})
			first := conversationHistoryStorageSnapshot(history, "count-first", "count-root", time.Now())
			saveConversationHistoryStorageSnapshot(t, history, first)
			second := conversationHistoryStorageSnapshot(history, "count-second", first.Root, time.Now())
			if operation != "begin" {
				if err := history.beginAttempt(first.Root, "count-operation"); err != nil {
					t.Fatal(err)
				}
			}
			before := history.counts
			bindings.durable.beforeCommit = func() error {
				if history.counts != before {
					t.Error("quota counts were published before commit")
				}
				return errConversationHistoryCapacity // abort without poisoning the store
			}
			mutate := func() error {
				switch operation {
				case "begin":
					return history.beginAttempt(first.Root, "count-operation")
				case "save":
					return history.save(second)
				default:
					return history.clearAttempt(first.Root)
				}
			}
			if err := mutate(); !errors.Is(err, errConversationHistoryCapacity) || history.counts != before {
				t.Fatalf("aborted transaction changed counts: before=%+v after=%+v err=%v", before, history.counts, err)
			}
			bindings.durable.beforeCommit = nil
			if err := mutate(); err != nil {
				t.Fatal(err)
			}
			if operation == "clear" {
				if err := history.clearAttempt(first.Root); err != nil {
					t.Fatal(err)
				}
			}
			var committed conversationHistoryCounts
			if err := bindings.durable.db.View(func(tx *bolt.Tx) error {
				committed.snapshots = tx.Bucket(conversationSnapshotsBucket).Stats().KeyN
				committed.pending = tx.Bucket(conversationPendingBucket).Stats().KeyN
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			if history.counts != committed {
				t.Fatalf("counts differ from committed records: cache=%+v records=%+v", history.counts, committed)
			}
			closeDurableStoreFixture(t, bindings)
			_, retained := reopenConversationHistoryStorageFixture(t, file, history.config)
			if retained.counts != committed {
				t.Fatalf("reopen lost quota counts: got=%+v want=%+v", retained.counts, committed)
			}
			for i := committed.snapshots + committed.pending; i < retained.config.MaxSnapshots; i++ {
				if err := retained.beginAttempt(fmt.Sprintf("fill-%d", i), "fill-operation"); err != nil {
					t.Fatal(err)
				}
			}
			if err := retained.beginAttempt("overflow", "overflow-operation"); !errors.Is(err, errConversationHistoryCapacity) {
				t.Fatalf("reopened counts did not enforce capacity: %v", err)
			}
		})
	}
}

func TestConversationHistoryStorageCorruptionIsPreserved(t *testing.T) {
	for _, corruption := range []string{"MAC", "payload", "count", "index", "missing index", "pending", "pending timestamp", "pending key", "missing index bucket", "missing pending bucket"} {
		t.Run(corruption, func(t *testing.T) {
			bindings, history, file := newConversationHistoryStorageFixture(t, ConversationMigrationConfig{})
			snapshot := conversationHistoryStorageSnapshot(history, "corruption-fixture", "corruption-root", time.Now())
			saveConversationHistoryStorageSnapshot(t, history, snapshot)
			if err := history.beginAttempt(snapshot.Root, "unresolved-operation"); err != nil {
				t.Fatal(err)
			}
			err := bindings.durable.db.Update(func(tx *bolt.Tx) error {
				snapshots := tx.Bucket(conversationSnapshotsBucket)
				switch corruption {
				case "count":
					return snapshots.SetSequence(0)
				case "index":
					return tx.Bucket(conversationIndexBucket).Put(snapshot.Indexes[0], []byte{1})
				case "missing index":
					return tx.Bucket(conversationIndexBucket).Delete(snapshot.Indexes[0])
				case "pending":
					return tx.Bucket(conversationPendingBucket).Put(history.rootKey(snapshot.Root), []byte{1})
				case "pending timestamp", "pending key":
					pending := tx.Bucket(conversationPendingBucket)
					key := history.rootKey(snapshot.Root)
					value := append([]byte(nil), pending.Get(key)...)
					if corruption == "pending timestamp" {
						value[39] ^= 1
					} else {
						key = history.rootKey("another-root")
					}
					return pending.Put(key, value)
				case "missing index bucket":
					return tx.DeleteBucket(conversationIndexBucket)
				case "missing pending bucket":
					return tx.DeleteBucket(conversationPendingBucket)
				default:
					key := history.responseKey(snapshot.RouteID, snapshot.ResponseID)
					value := append([]byte(nil), snapshots.Get(key)...)
					offset := 0
					if corruption == "payload" {
						offset = 33
					}
					value[offset] ^= 1
					return snapshots.Put(key, value)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			closeDurableStoreFixture(t, bindings)
			before, err := os.ReadFile(file.Path)
			if err != nil {
				t.Fatal(err)
			}
			reopened, err := newDurableStateBindingStore(file)
			if err != nil {
				t.Fatal(err)
			}
			_, err = newConversationHistoryStore(reopened.durable, history.config)
			if !errors.Is(err, errConversationHistoryStorage) {
				t.Errorf("corrupt history accepted: %v", err)
			}
			closeDurableStoreFixture(t, reopened)
			after, err := os.ReadFile(file.Path)
			if err != nil || !bytes.Equal(before, after) {
				t.Errorf("reopening corrupt history changed its file: %v", err)
			}
		})
	}
}

func TestConversationHistoryStorageAdmissionAndCancellation(t *testing.T) {
	_, history, _ := newConversationHistoryStorageFixture(t, ConversationMigrationConfig{})
	start := make(chan struct{})
	results := make(chan error, 16)
	var workers sync.WaitGroup
	for range 16 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			results <- history.acquire("contended-root")
		}()
	}
	close(start)
	workers.Wait()
	close(results)
	acquired := 0
	for err := range results {
		if err == nil {
			acquired++
		} else if !errors.Is(err, errConversationHistoryBusy) {
			t.Fatal(err)
		}
	}
	if acquired != 1 {
		t.Fatalf("concurrent admissions = %d, want 1", acquired)
	}
	history.release("contended-root")
	for _, dispatched := range []bool{false, true} {
		root := fmt.Sprintf("cancelled-%t", dispatched)
		if err := history.acquire(root); err != nil {
			t.Fatal(err)
		}
		turn := &conversationTurn{h: &ProxyHandler{}, store: history, root: root, operation: &routeOperation{id: "cancelled-operation"}}
		if err := turn.persistIntent(); err != nil {
			t.Fatal(err)
		}
		if dispatched {
			turn.dispatching()
		}
		turn.finish()
		turn.finish()
		if err := turn.persistIntent(); !errors.Is(err, context.Canceled) {
			t.Fatalf("closed turn persisted another intent: %v", err)
		}
		err := history.acquire(root)
		if dispatched && !errors.Is(err, errConversationHistoryUncertain) || !dispatched && err != nil {
			t.Fatalf("cancelled dispatched=%t admission = %v", dispatched, err)
		}
		history.release(root)
	}
}

func TestConversationHistoryStoragePrunePreservesOwnership(t *testing.T) {
	bindings, history, file := newConversationHistoryStorageFixture(t, ConversationMigrationConfig{})
	cutoff := time.Date(2020, 1, 1, 0, 0, 1, 0, time.UTC)
	old := conversationHistoryStorageSnapshot(history, "prune-old", "old-root", cutoff.Add(-time.Second))
	retired := conversationHistoryStorageSnapshot(history, "prune-uncertain", "uncertain-root", cutoff.Add(time.Second))
	kept := conversationHistoryStorageSnapshot(history, "prune-kept", "kept-root", cutoff)
	keptOlder := conversationHistoryStorageSnapshot(history, "prune-kept-old", kept.Root, cutoff.Add(-time.Second))
	snapshots := []*conversationSnapshot{old, retired, keptOlder, kept}
	for _, snapshot := range snapshots {
		saveConversationHistoryStorageSnapshot(t, history, snapshot)
		if result := bindings.bindAll([]stateBindingToken{{stateBindingTypeResponseID, snapshot.ResponseID}}, durableFixtureOwner()); result.err != nil {
			t.Fatal(result.err)
		}
	}
	// A clock regression must not let pruning an older intent retain its root.
	bindings.durable.now = func() time.Time { return cutoff.Add(-time.Second) }
	if err := history.beginAttempt(retired.Root, "old-unresolved-operation"); err != nil {
		t.Fatal(err)
	}
	bindings.durable.now = func() time.Time { return cutoff }
	if err := history.beginAttempt(kept.Root, "retained-unresolved-operation"); err != nil {
		t.Fatal(err)
	}
	if second, err := newDurableStateBindingStore(file); !errors.Is(err, errDurableStateLocked) {
		if second != nil {
			closeDurableStoreFixture(t, second)
		}
		t.Fatalf("second writer = %v", err)
	}
	if removed, err := PruneConversationHistory(file.Path, cutoff); removed != 0 || !errors.Is(err, errDurableStateLocked) {
		t.Fatalf("prune while serving = %d, %v", removed, err)
	}
	closeDurableStoreFixture(t, bindings)
	if removed, err := PruneConversationHistory(file.Path, cutoff); removed != 2 || err != nil {
		t.Fatalf("offline prune = %d, %v", removed, err)
	}
	reopened, retained := reopenConversationHistoryStorageFixture(t, file, history.config)
	for _, snapshot := range snapshots {
		if result := reopened.lookup(stateBindingTypeResponseID, snapshot.ResponseID); result.err != nil || result.outcome != stateBindingLookupKnown || result.owner != reopened.durable.encodeOwner(durableFixtureOwner()) {
			t.Fatalf("history pruning changed ownership: %+v", result)
		}
		if snapshot.Root == kept.Root {
			requireConversationHistoryStorageSnapshot(t, retained, snapshot)
			if got, err := retained.lookupIndexes(snapshot.Indexes); err != nil || !reflect.DeepEqual(got, snapshot) {
				t.Fatalf("prune lost an index for a pending conversation: %+v, %v", got, err)
			}
			continue
		}
		if _, err := retained.lookupResponse(snapshot.RouteID, snapshot.ResponseID); !errors.Is(err, errConversationHistoryMissing) {
			t.Fatalf("pruned response %q remained available: %v", snapshot.ResponseID, err)
		}
		if got, err := retained.lookupIndexes(snapshot.Indexes); got != nil || err != nil {
			t.Fatalf("pruned index %q remained available: %+v, %v", snapshot.ResponseID, got, err)
		}
	}
	if err := retained.acquire(retired.Root); err != nil {
		t.Fatalf("explicitly pruned intent remains active: %v", err)
	}
	retained.release(retired.Root)
	if err := retained.acquire(kept.Root); !errors.Is(err, errConversationHistoryUncertain) {
		t.Fatalf("prune removed an intent at the cutoff: %v", err)
	}
	retained.config.MaxSnapshots = 3 // two retained snapshots and one pending turn
	if err := retained.beginAttempt("after-prune", "after-prune-operation"); !errors.Is(err, errConversationHistoryCapacity) {
		t.Fatalf("pruned counts did not retain pending capacity: %v", err)
	}
	if err := retained.clearAttempt(kept.Root); err != nil {
		t.Fatal(err)
	}
	if err := retained.beginAttempt("after-prune", "after-prune-operation"); err != nil {
		t.Fatalf("pruned counts did not release cleared capacity: %v", err)
	}
	missing := filepath.Join(filepath.Dir(file.Path), "missing-history.db")
	if _, err := PruneConversationHistory(missing, cutoff); err == nil {
		t.Fatal("pruning accepted a missing database")
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("pruning created a missing database")
	}
}

// The parent kills only this disposable child. It inherits no provider secrets.
func TestConversationHistoryStorageChild(t *testing.T) {
	window := os.Getenv("VEKIL_TEST_CONVERSATION_HISTORY_CHILD")
	if window == "" {
		return
	}
	bindings, err := newDurableStateBindingStore(DurableStateBindingsConfig{Path: os.Getenv("VEKIL_TEST_CONVERSATION_HISTORY_FILE"), MaxEntries: 64})
	if err != nil {
		t.Fatal(err)
	}
	history, err := newConversationHistoryStore(bindings.durable, ConversationMigrationConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := history.beginAttempt("process-root", "process-operation"); err != nil {
		t.Fatal(err)
	}
	barrier := func() error {
		fmt.Println("history barrier")
		select {}
	}
	switch window {
	case "pending":
		_ = barrier()
	case "before":
		bindings.durable.beforeCommit = barrier
	case "after":
		bindings.durable.afterCommit = barrier
	default:
		t.Fatal("unknown process window")
	}
	snapshot := conversationHistoryStorageSnapshot(history, "process-completion", "process-root", time.Date(2020, 1, 1, 0, 1, 0, 0, time.UTC))
	if err := history.save(snapshot); err != nil {
		t.Fatal(err)
	}
	t.Fatal("completion returned before parent released the commit barrier")
}

func TestConversationHistoryStorageProcessKillWindows(t *testing.T) {
	for _, window := range []string{"pending", "before", "after"} {
		t.Run(window, func(t *testing.T) {
			bindings, history, file := newConversationHistoryStorageFixture(t, ConversationMigrationConfig{})
			prior := conversationHistoryStorageSnapshot(history, "process-prior", "process-root", time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC))
			saveConversationHistoryStorageSnapshot(t, history, prior)
			closeDurableStoreFixture(t, bindings)
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestConversationHistoryStorageChild$", "-test.timeout=12s")
			cmd.Env = []string{"GOMAXPROCS=2", "VEKIL_TEST_CONVERSATION_HISTORY_CHILD=" + window, "VEKIL_TEST_CONVERSATION_HISTORY_FILE=" + file.Path}
			cmd.Stderr = os.Stderr
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			done := make(chan error, 1)
			go func() { done <- cmd.Wait() }()
			waited := false
			defer func() {
				_ = cmd.Process.Kill()
				if !waited {
					select {
					case <-done:
					case <-time.After(time.Second):
						t.Error("owned child did not exit")
					}
				}
			}()
			lines := make(chan string, 4)
			go func() {
				defer close(lines)
				scanner := bufio.NewScanner(stdout)
				for scanner.Scan() {
					select {
					case lines <- scanner.Text():
					case <-ctx.Done():
						return
					}
				}
			}()
			barrier := false
			for !barrier {
				select {
				case line, ok := <-lines:
					if !ok {
						t.Fatal("child exited before the commit barrier")
					}
					barrier = line == "history barrier"
				case <-ctx.Done():
					t.Fatal("child did not reach the commit barrier")
				}
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			select {
			case err := <-done:
				waited = true
				if err == nil {
					t.Fatal("child exited successfully instead of being killed")
				}
			case <-ctx.Done():
				t.Fatal("killed child did not exit")
			}
			_, retained := reopenConversationHistoryStorageFixture(t, file, history.config)
			requireConversationHistoryStorageSnapshot(t, retained, prior)
			completion, err := retained.lookupResponse(prior.RouteID, "process-completion")
			if window == "after" {
				if err != nil || completion == nil {
					t.Fatalf("kill lost committed completion: %+v, %v", completion, err)
				}
			} else if !errors.Is(err, errConversationHistoryMissing) {
				t.Fatalf("kill published uncommitted completion: %+v, %v", completion, err)
			}
			err = retained.acquire(prior.Root)
			if window == "after" && err != nil || window != "after" && !errors.Is(err, errConversationHistoryUncertain) {
				t.Fatalf("pending after kill at %s = %v", window, err)
			}
			retained.release(prior.Root)
		})
	}
}
