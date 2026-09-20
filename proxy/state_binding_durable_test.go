package proxy

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"syscall"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func newDurableStoreFixture(t *testing.T, maxEntries int) (*stateBindingStore, DurableStateBindingsConfig) {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip("durable filesystem support requires Linux or macOS")
	}
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := DurableStateBindingsConfig{Path: filepath.Join(dir, "bindings.db"), MaxEntries: maxEntries}
	store, err := newDurableStateBindingStore(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.close(); err != nil {
			t.Error(err)
		}
	})
	return store, config
}

func closeDurableStoreFixture(t testing.TB, s *stateBindingStore) {
	t.Helper()
	if err := s.close(); err != nil {
		t.Error(err)
	}
}

func durableFixtureOwner() stateBindingOwner {
	return stateBindingOwner{routeID: "synthetic-route-private", targetID: "synthetic-target-private", identity: [32]byte{7}}
}

func TestDurableStateStoreReopenWithoutExpiry(t *testing.T) {
	s, config := newDurableStoreFixture(t, 4)
	owner := durableFixtureOwner()
	tokens := []stateBindingToken{
		{stateBindingTypeResponseID, "synthetic-response-private"},
		{stateBindingTypeEncryptedContent, "synthetic-encrypted-private"},
		{stateBindingTypeTurnState, "synthetic-turn-private"},
		{stateBindingTypeConversationID, "synthetic-conversation-private"},
	}
	if result := s.bindAll(tokens, owner); result.err != nil || result.outcome != stateBindingLookupKnown {
		t.Fatalf("bind = %+v", result)
	}
	key := s.digestKey
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newDurableStateBindingStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDurableStoreFixture(t, reopened)
	reopened.durable.now = func() time.Time { return time.Now().Add(20 * 365 * 24 * time.Hour) }
	if key != reopened.digestKey {
		t.Fatal("store key changed across reopen")
	}
	if result := reopened.resolveForRoute(owner.routeID, "", tokens); result.err != nil || result.outcome != stateBindingLookupKnown || result.owner != reopened.durable.encodeOwner(owner) {
		t.Fatalf("reopen continuation = %+v", result)
	}
	if result := reopened.resolveForRoute("unrelated-route", "", tokens); result.outcome != stateBindingLookupConflict {
		t.Fatal("cross-route ownership accepted")
	}
	body, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{owner.routeID, owner.targetID, tokens[0].value, tokens[1].value, tokens[2].value, tokens[3].value} {
		if bytes.Contains(body, []byte(raw)) {
			t.Fatal("raw state or configuration label found in durable file")
		}
	}
}

func TestDurableStateStoreCapacityPreservesProofAndConflicts(t *testing.T) {
	s, config := newDurableStoreFixture(t, 1)
	owner := durableFixtureOwner()
	known := stateBindingToken{stateBindingTypeResponseID, "known-fixture"}
	unseen := stateBindingToken{stateBindingTypeResponseID, "unseen-fixture"}
	if r := s.bindAll([]stateBindingToken{known}, owner); r.err != nil {
		t.Fatal(r.err)
	}
	if r := s.bindAll([]stateBindingToken{known, unseen}, owner); !errors.Is(r.err, errDurableStateCapacity) {
		t.Fatalf("cap = %+v", r)
	}
	if r := s.lookup(known.stateType, known.value); r.outcome != stateBindingLookupKnown {
		t.Fatal("capacity discarded existing proof")
	}
	if r := s.lookup(unseen.stateType, unseen.value); r.outcome != stateBindingLookupUnknown {
		t.Fatal("capacity partially inserted batch")
	}
	other := owner
	other.identity[0]++
	if r := s.bindAll([]stateBindingToken{known, unseen}, other); r.err != nil || r.outcome != stateBindingLookupConflict {
		t.Fatalf("conflict at cap = %+v", r)
	}
	if stats := s.stats(); stats.entries != 1 || stats.tombstones != 1 || stats.evictions != 0 {
		t.Fatalf("stats = %+v", stats)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newDurableStateBindingStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDurableStoreFixture(t, reopened)
	if r := reopened.bindAll([]stateBindingToken{known}, owner); r.err != nil || r.outcome != stateBindingLookupConflict {
		t.Fatal("reopen cleared conflict tombstone")
	}
}

func TestDurableStateStoreFailureBarriers(t *testing.T) {
	for _, after := range []bool{false, true} {
		t.Run(map[bool]string{false: "before_commit", true: "after_commit_before_exposure"}[after], func(t *testing.T) {
			s, config := newDurableStoreFixture(t, 4)
			owner := durableFixtureOwner()
			known := stateBindingToken{stateBindingTypeResponseID, "known-fixture"}
			unseen := stateBindingToken{stateBindingTypeResponseID, "unseen-fixture"}
			if r := s.bindAll([]stateBindingToken{known}, owner); r.err != nil {
				t.Fatal(r.err)
			}
			fault := func() error { return syscall.ENOSPC }
			if after {
				s.durable.afterCommit = fault
			} else {
				s.durable.beforeCommit = fault
			}
			if r := s.bindAll([]stateBindingToken{unseen}, owner); !errors.Is(r.err, errDurableStateIO) {
				t.Fatalf("fault = %+v", r)
			}
			if r := s.lookup(known.stateType, known.value); !errors.Is(r.err, errDurableStateIO) {
				t.Fatal("uncertain store did not fail closed")
			}
			if err := s.close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := newDurableStateBindingStore(config)
			if err != nil {
				t.Fatal(err)
			}
			defer closeDurableStoreFixture(t, reopened)
			if r := reopened.lookup(known.stateType, known.value); r.outcome != stateBindingLookupKnown {
				t.Fatal("failure lost old committed proof")
			}
			want := stateBindingLookupUnknown
			if after {
				want = stateBindingLookupKnown
			}
			if r := reopened.lookup(unseen.stateType, unseen.value); r.outcome != want {
				t.Fatalf("transaction window = %s, want %s", r.outcome, want)
			}
		})
	}
}

func TestDurableStateStoreCorruptionNeverReinitializes(t *testing.T) {
	for _, corruption := range []string{"key", "record", "format", "count"} {
		t.Run(corruption, func(t *testing.T) {
			s, config := newDurableStoreFixture(t, 4)
			if r := s.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "fixture"}}, durableFixtureOwner()); r.err != nil {
				t.Fatal(r.err)
			}
			err := s.durable.db.Update(func(tx *bolt.Tx) error {
				switch corruption {
				case "key":
					return tx.Bucket(durableStateMetadata).Put([]byte("key"), make([]byte, 32))
				case "format":
					return tx.Bucket(durableStateMetadata).Put([]byte("format"), []byte("future-unknown-format"))
				case "count":
					return tx.Bucket(durableStateRecords).SetSequence(0)
				default:
					b := tx.Bucket(durableStateRecords)
					key, value := b.Cursor().First()
					changed := append([]byte(nil), value...)
					changed[50] ^= 1
					return b.Put(key, changed)
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			if err := s.close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(config.Path)
			if err != nil {
				t.Fatal(err)
			}
			if reopened, err := newDurableStateBindingStore(config); err == nil {
				closeDurableStoreFixture(t, reopened)
				t.Fatal("corrupt store was accepted")
			}
			after, err := os.ReadFile(config.Path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("startup modified corrupt authority")
			}
		})
	}
}

func TestDurableStateStoreLockAndUnsafeFiles(t *testing.T) {
	s, config := newDurableStoreFixture(t, 4)
	if second, err := newDurableStateBindingStore(config); !errors.Is(err, errDurableStateLocked) {
		if second != nil {
			closeDurableStoreFixture(t, second)
		}
		t.Fatalf("second writer = %v", err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(config.Path, 0o644); err != nil {
		t.Fatal(err)
	}
	if second, err := newDurableStateBindingStore(config); err == nil {
		closeDurableStoreFixture(t, second)
		t.Fatal("world-readable file accepted")
	}
	if err := os.Chmod(config.Path, 0o600); err != nil {
		t.Fatal(err)
	}
	for _, link := range []string{"symlink", "hardlink"} {
		t.Run(link, func(t *testing.T) {
			path := filepath.Join(filepath.Dir(config.Path), link)
			var err error
			if link == "symlink" {
				err = os.Symlink(config.Path, path)
			} else {
				err = os.Link(config.Path, path)
			}
			if err != nil {
				t.Fatal(err)
			}
			if second, err := newDurableStateBindingStore(DurableStateBindingsConfig{Path: path}); err == nil {
				closeDurableStoreFixture(t, second)
				t.Fatal("linked authority accepted")
			}
		})
	}
}

func TestDurableStateStoreExplicitPruning(t *testing.T) {
	s, config := newDurableStoreFixture(t, 4)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	s.durable.now = func() time.Time { return old }
	owner := durableFixtureOwner()
	if r := s.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "old-fixture"}}, owner); r.err != nil {
		t.Fatal(r.err)
	}
	s.durable.now = time.Now
	if r := s.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "recent-fixture"}}, owner); r.err != nil {
		t.Fatal(r.err)
	}
	cutoff := old.Add(time.Hour)
	if _, err := PruneDurableStateBindings(config.Path, cutoff); !errors.Is(err, errDurableStateLocked) {
		t.Fatalf("live pruning = %v", err)
	}
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	if n, err := PruneDurableStateBindings(config.Path, cutoff); err != nil || n != 1 {
		t.Fatalf("prune = %d %v", n, err)
	}
	reopened, err := newDurableStateBindingStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDurableStoreFixture(t, reopened)
	if r := reopened.lookup(stateBindingTypeResponseID, "old-fixture"); r.outcome != stateBindingLookupUnknown {
		t.Fatal("pruned record still accepted")
	}
	if r := reopened.lookup(stateBindingTypeResponseID, "recent-fixture"); r.outcome != stateBindingLookupKnown {
		t.Fatal("prune lost retained authority")
	}
	missing := filepath.Join(filepath.Dir(config.Path), "missing.db")
	if _, err := PruneDurableStateBindings(missing, cutoff); err == nil {
		t.Fatal("prune accepted missing store")
	}
	if _, err := os.Lstat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("prune created a store")
	}
}

func TestDurableStateStorePruningWholeSecondBoundary(t *testing.T) {
	s, config := newDurableStoreFixture(t, 4)
	boundary := time.Date(2020, 1, 1, 0, 0, 1, 0, time.UTC)
	owner := durableFixtureOwner()
	for _, fixture := range []struct {
		token string
		at    time.Time
	}{
		{"before-second", boundary.Add(-time.Nanosecond)},
		{"at-second", boundary},
		{"after-second", boundary.Add(900 * time.Millisecond)},
		{"conflict-after-second", boundary.Add(900 * time.Millisecond)},
	} {
		s.durable.now = func() time.Time { return fixture.at }
		tokens := []stateBindingToken{{stateBindingTypeResponseID, fixture.token}}
		if r := s.bindAll(tokens, owner); r.err != nil {
			t.Fatal(r.err)
		}
		if fixture.token == "conflict-after-second" {
			other := owner
			other.targetID = "other-target"
			if r := s.bindAll(tokens, other); r.err != nil || r.outcome != stateBindingLookupConflict {
				t.Fatalf("tombstone = %+v", r)
			}
		}
	}
	closeDurableStoreFixture(t, s)
	original, err := os.ReadFile(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	for _, fraction := range []time.Duration{time.Nanosecond, 500 * time.Millisecond, time.Second - time.Nanosecond} {
		if n, err := PruneDurableStateBindings(config.Path, boundary.Add(fraction)); n != 0 || !errors.Is(err, errDurableStateConfig) {
			t.Fatalf("fractional cutoff %s: removed=%d err=%v", fraction, n, err)
		}
		unchanged, err := os.ReadFile(config.Path)
		if err != nil || !bytes.Equal(original, unchanged) {
			t.Fatalf("rejected prune changed database: %v", err)
		}
	}
	if n, err := PruneDurableStateBindings(config.Path, boundary); n != 1 || err != nil {
		t.Fatalf("whole-second cutoff: removed=%d err=%v", n, err)
	}
	reopened, err := newDurableStateBindingStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDurableStoreFixture(t, reopened)
	for token, want := range map[string]stateBindingLookupOutcome{
		"before-second": stateBindingLookupUnknown, "at-second": stateBindingLookupKnown,
		"after-second": stateBindingLookupKnown, "conflict-after-second": stateBindingLookupConflict,
	} {
		if got := reopened.lookup(stateBindingTypeResponseID, token); got.err != nil || got.outcome != want {
			t.Fatalf("retained boundary %s: %+v, want %v", token, got, want)
		}
	}
	closeDurableStoreFixture(t, reopened)
	if n, err := PruneDurableStateBindings(config.Path, boundary.Add(time.Second)); n != 3 || err != nil {
		t.Fatalf("next whole second: removed=%d err=%v", n, err)
	}
}

func TestDurableStateStorePruningPreservesFirstConflictTime(t *testing.T) {
	s, config := newDurableStoreFixture(t, 3)
	issued := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	firstConflict := issued.Add(time.Hour)
	cutoff := firstConflict.Add(time.Hour)
	repeated := cutoff.Add(time.Hour)
	s.durable.now = func() time.Time { return issued }
	owner := durableFixtureOwner()
	other := owner
	other.identity[0]++
	old := stateBindingToken{stateBindingTypeResponseID, "old-conflict"}
	late := stateBindingToken{stateBindingTypeResponseID, "late-conflict"}
	newToken := stateBindingToken{stateBindingTypeResponseID, "uncommitted-batch-member"}
	retained := stateBindingToken{stateBindingTypeResponseID, "retained-proof"}
	if r := s.bindAll([]stateBindingToken{old, late}, owner); r.err != nil || r.outcome != stateBindingLookupKnown {
		t.Fatalf("initial proof = %+v", r)
	}
	s.durable.now = func() time.Time { return firstConflict }
	if r := s.bindAll([]stateBindingToken{old}, other); r.err != nil || r.outcome != stateBindingLookupConflict {
		t.Fatalf("first conflict = %+v", r)
	}
	closeDurableStoreFixture(t, s)
	reopened, err := newDurableStateBindingStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDurableStoreFixture(t, reopened)
	reopened.durable.now = func() time.Time { return repeated }
	for _, candidate := range []stateBindingOwner{owner, other} {
		if r := reopened.bindAll([]stateBindingToken{old}, candidate); r.err != nil || r.outcome != stateBindingLookupConflict {
			t.Fatalf("repeated conflict = %+v", r)
		}
	}
	// A mixed batch must preserve the old tombstone, timestamp a newly detected
	// collision now, and withhold the previously unknown member atomically.
	if r := reopened.bindAll([]stateBindingToken{old, late, newToken}, other); r.err != nil || r.outcome != stateBindingLookupConflict {
		t.Fatalf("mixed collision batch = %+v", r)
	}
	if r := reopened.lookup(newToken.stateType, newToken.value); r.err != nil || r.outcome != stateBindingLookupUnknown {
		t.Fatalf("conflicting batch admitted new state: %+v", r)
	}
	if r := reopened.bindAll([]stateBindingToken{retained}, owner); r.err != nil || r.outcome != stateBindingLookupKnown {
		t.Fatalf("retained proof = %+v", r)
	}
	if stats := reopened.stats(); stats.entries != 3 || stats.tombstones != 2 {
		t.Fatalf("before pruning: %+v", stats)
	}
	if r := reopened.bindAll([]stateBindingToken{newToken}, owner); !errors.Is(r.err, errDurableStateCapacity) {
		t.Fatalf("capacity before pruning = %+v", r)
	}
	closeDurableStoreFixture(t, reopened)
	if n, err := PruneDurableStateBindings(config.Path, cutoff); err != nil || n != 1 {
		t.Fatalf("pruning first conflict time: removed=%d err=%v", n, err)
	}
	pruned, err := newDurableStateBindingStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDurableStoreFixture(t, pruned)
	for token, want := range map[stateBindingToken]stateBindingLookupOutcome{
		old: stateBindingLookupUnknown, late: stateBindingLookupConflict,
		retained: stateBindingLookupKnown, newToken: stateBindingLookupUnknown,
	} {
		if r := pruned.lookup(token.stateType, token.value); r.err != nil || r.outcome != want {
			t.Fatalf("pruned lookup %s = %+v, want %v", token.value, r, want)
		}
	}
	if r := pruned.bindAll([]stateBindingToken{newToken}, owner); r.err != nil || r.outcome != stateBindingLookupKnown {
		t.Fatalf("pruning did not reclaim capacity: %+v", r)
	}
}

func TestDurableStateStoreInvalidFilesPreserved(t *testing.T) {
	for _, damage := range []string{"empty", "truncated", "random", "foreign"} {
		t.Run(damage, func(t *testing.T) {
			s, config := newDurableStoreFixture(t, 4)
			if err := s.close(); err != nil {
				t.Fatal(err)
			}
			switch damage {
			case "empty":
				if err := os.Truncate(config.Path, 0); err != nil {
					t.Fatal(err)
				}
			case "truncated":
				if err := os.Truncate(config.Path, int64(os.Getpagesize()*2)); err != nil {
					t.Fatal(err)
				}
			case "random":
				if err := os.WriteFile(config.Path, bytes.Repeat([]byte{0xa5}, os.Getpagesize()*8), 0o600); err != nil {
					t.Fatal(err)
				}
			case "foreign":
				db, err := bolt.Open(config.Path, 0o600, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := db.Update(func(tx *bolt.Tx) error { return tx.DeleteBucket(durableStateMetadata) }); err != nil {
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			before, err := os.ReadFile(config.Path)
			if err != nil {
				t.Fatal(err)
			}
			if reopened, err := newDurableStateBindingStore(config); err == nil {
				_ = reopened.close()
				t.Fatal("invalid authority silently reinitialized")
			}
			after, err := os.ReadFile(config.Path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("invalid file changed during rejected startup")
			}
		})
	}
}

func TestDurableStateStoreMalformedPagesPreserved(t *testing.T) {
	for _, pageType := range []string{"leaf", "freelist"} {
		t.Run(pageType, func(t *testing.T) {
			s, config := newDurableStoreFixture(t, 4)
			if r := s.bindAll([]stateBindingToken{{stateBindingTypeResponseID, "page-fixture"}}, durableFixtureOwner()); r.err != nil {
				t.Fatal(r.err)
			}
			pageSize := s.durable.db.Info().PageSize
			offset := -1
			if err := s.durable.db.View(func(tx *bolt.Tx) error {
				for id := 2; int64(id*pageSize) < tx.Size(); id++ {
					page, err := tx.Page(id)
					if err != nil {
						return err
					}
					if page != nil && page.Type == pageType {
						// bbolt page flags follow the eight-byte page ID.
						offset = id*pageSize + 8
						break
					}
				}
				return nil
			}); err != nil || offset < 0 {
				t.Fatalf("find %s page: offset=%d err=%v", pageType, offset, err)
			}
			if err := s.close(); err != nil {
				t.Fatal(err)
			}
			before, err := os.ReadFile(config.Path)
			if err != nil {
				t.Fatal(err)
			}
			before[offset], before[offset+1] = 0, 0
			if err := os.WriteFile(config.Path, before, 0o600); err != nil {
				t.Fatal(err)
			}
			if reopened, err := newDurableStateBindingStore(config); !errors.Is(err, errDurableStateCorrupt) {
				if reopened != nil {
					_ = reopened.close()
				}
				t.Fatalf("malformed %s page = %v", pageType, err)
			}
			after, err := os.ReadFile(config.Path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(before, after) {
				t.Fatal("rejected page corruption changed store bytes")
			}
		})
	}
}

func TestDurableStateStoreRepeatedProofDoesNotWrite(t *testing.T) {
	s, _ := newDurableStoreFixture(t, 4)
	tokens := []stateBindingToken{{stateBindingTypeResponseID, "repeated-fixture"}}
	owner := durableFixtureOwner()
	if r := s.bindAll(tokens, owner); r.err != nil {
		t.Fatal(r.err)
	}
	before := s.durable.db.Stats()
	s.durable.beforeCommit = func() error { t.Error("repeated proof entered write transaction"); return syscall.EIO }
	for i := 0; i < 20; i++ {
		if r := s.bindAll(tokens, owner); r.err != nil || r.outcome != stateBindingLookupKnown {
			t.Fatalf("repeat = %+v", r)
		}
	}
	if after := s.durable.db.Stats(); after.TxStats.Write != before.TxStats.Write {
		t.Fatal("repeated proof wrote database pages")
	}
}

func TestDurableStateStoreConcurrentConversationClaims(t *testing.T) {
	s, _ := newDurableStoreFixture(t, 4)
	token := stateBindingToken{stateBindingTypeConversationID, "conversation-race-fixture"}
	const clients = 32
	results := make(chan stateBindingLookupResult, clients)
	start := make(chan struct{})
	var workers sync.WaitGroup
	for i := 0; i < clients; i++ {
		workers.Add(1)
		go func(i int) {
			defer workers.Done()
			owner := durableFixtureOwner()
			owner.identity[1] = byte(i)
			<-start
			results <- s.durable.bind([]stateBindingToken{token}, owner, true)
		}(i)
	}
	close(start)
	workers.Wait()
	close(results)
	var winner stateBindingOwner
	for result := range results {
		if result.err != nil || result.outcome != stateBindingLookupKnown {
			t.Fatalf("claim = %+v", result)
		}
		if winner == (stateBindingOwner{}) {
			winner = result.owner
		}
		if winner != result.owner {
			t.Fatal("bootstrap race changed owner")
		}
	}
	if r := s.lookup(token.stateType, token.value); r.owner != winner || s.stats().tombstones != 0 || s.stats().entries != 1 {
		t.Fatal("bootstrap poisoned winning proof")
	}
}
