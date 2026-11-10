package proxy

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
)

func readStateBindingStats(t *testing.T, h *ProxyHandler) stateBindingStatsSnapshot {
	t.Helper()
	w := httptest.NewRecorder()
	h.HandleStatsJSON(w, httptest.NewRequest(http.MethodGet, "/stats.json", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("stats status = %d, body = %s", w.Code, w.Body.String())
	}
	var snapshot statsSnapshot
	if err := json.Unmarshal(w.Body.Bytes(), &snapshot); err != nil {
		t.Fatal(err)
	}
	return snapshot.StateBindings
}

func TestStateBindingStatsMemory(t *testing.T) {
	store, err := newStateBindingStore(stateBindingStoreConfig{maxEntries: 2})
	if err != nil {
		t.Fatal(err)
	}
	h := &ProxyHandler{stateBindings: store}
	check := func(entries uint64, capacity string) {
		t.Helper()
		snapshot := readStateBindingStats(t, h)
		if snapshot.Mode != "memory" || snapshot.Status != "ready" || snapshot.Entries == nil || *snapshot.Entries != entries || snapshot.MaxEntries != 2 {
			t.Fatalf("memory stats = %+v", snapshot)
		}
		if snapshot.DatabaseBytes == nil || *snapshot.DatabaseBytes != 0 || snapshot.CapacityStatus != capacity {
			t.Fatalf("memory storage/capacity = %+v", snapshot)
		}
	}
	check(0, "ok")
	owner := stateBindingOwner{routeID: "route", targetID: "first"}
	store.bind(stateBindingTypeResponseID, "response-one", owner)
	owner.targetID = "second"
	store.bind(stateBindingTypeResponseID, "response-one", owner)
	check(1, "ok")
	store.bind(stateBindingTypeResponseID, "response-two", owner)
	check(2, "exhausted")
	store.bind(stateBindingTypeResponseID, "response-three", owner)
	check(2, "exhausted")
}

func TestStateBindingStatsUnavailable(t *testing.T) {
	for _, h := range []*ProxyHandler{nil, {}} {
		snapshot := readStateBindingStats(t, h)
		if snapshot.Status != "unavailable" || snapshot.Entries != nil || snapshot.DatabaseBytes != nil || snapshot.CapacityUsagePercent != nil || snapshot.CapacityStatus != "unknown" {
			t.Fatalf("uninitialized stats = %+v", snapshot)
		}
	}
}

func TestDurableStateStatsCapacityAndTombstones(t *testing.T) {
	store, config := newDurableStoreFixture(t, 100)
	h := &ProxyHandler{stateBindings: store}
	owner := durableFixtureOwner()
	for entries := 0; entries <= 100; entries++ {
		if entries > 0 {
			if result := store.bind(stateBindingTypeResponseID, fmt.Sprintf("private-response-%d", entries), owner); result.err != nil {
				t.Fatal(result.err)
			}
		}
		wantStatus := "ok"
		switch {
		case entries == 100:
			wantStatus = "exhausted"
		case entries >= 95:
			wantStatus = "critical"
		case entries >= 80:
			wantStatus = "warning"
		}
		snapshot := readStateBindingStats(t, h)
		if snapshot.Mode != "durable" || snapshot.Status != "ready" || snapshot.Entries == nil || *snapshot.Entries != uint64(entries) || snapshot.MaxEntries != 100 || snapshot.CapacityStatus != wantStatus {
			t.Fatalf("entries=%d stats = %+v, want capacity=%s", entries, snapshot, wantStatus)
		}
		if snapshot.CapacityUsagePercent == nil || *snapshot.CapacityUsagePercent < float64(entries)-0.001 || *snapshot.CapacityUsagePercent > float64(entries)+0.001 {
			t.Fatalf("entries=%d usage percent = %v", entries, snapshot.CapacityUsagePercent)
		}
	}
	other := owner
	other.identity[0]++
	if result := store.bind(stateBindingTypeResponseID, "private-response-1", other); result.outcome != stateBindingLookupConflict || result.err != nil {
		t.Fatalf("conflict = %+v", result)
	}
	if snapshot := readStateBindingStats(t, h); snapshot.Entries == nil || *snapshot.Entries != 100 || snapshot.CapacityStatus != "exhausted" {
		t.Fatalf("tombstone removed from count: %+v", snapshot)
	}
	if result := store.bind(stateBindingTypeResponseID, "private-response-extra", owner); !errors.Is(result.err, errDurableStateCapacity) {
		t.Fatalf("capacity result = %+v", result)
	}
	if snapshot := readStateBindingStats(t, h); snapshot.Status != "ready" {
		t.Fatalf("capacity should not freeze storage: %+v", snapshot)
	}
	w := httptest.NewRecorder()
	h.HandleStatsJSON(w, httptest.NewRequest(http.MethodGet, "/stats.json", nil))
	for _, private := range []string{config.Path, owner.routeID, owner.targetID, "private-response-1"} {
		if strings.Contains(w.Body.String(), private) {
			t.Fatalf("stats exposed private storage data %q", private)
		}
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newDurableStateBindingStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDurableStoreFixture(t, reopened)
	if snapshot := readStateBindingStats(t, &ProxyHandler{stateBindings: reopened}); snapshot.Entries == nil || *snapshot.Entries != 100 || snapshot.CapacityStatus != "exhausted" {
		t.Fatalf("reopened stats = %+v", snapshot)
	}
}

func TestDurableStateStatsDatabaseSizeAndLargeCapacity(t *testing.T) {
	store, config := newDurableStoreFixture(t, 8388608)
	h := &ProxyHandler{stateBindings: store}
	initial := readStateBindingStats(t, h)
	if initial.DatabaseBytes == nil || *initial.DatabaseBytes <= 0 || *initial.DatabaseBytes > 1024*1024 {
		t.Fatalf("large capacity preallocated the file: %+v", initial)
	}
	initialSize := *initial.DatabaseBytes
	tokens := make([]stateBindingToken, 1024)
	for i := range tokens {
		tokens[i] = stateBindingToken{stateBindingTypeResponseID, fmt.Sprintf("size-probe-%d", i)}
	}
	if result := store.bindAll(tokens, durableFixtureOwner()); result.err != nil {
		t.Fatal(result.err)
	}
	snapshot := readStateBindingStats(t, h)
	info, err := os.Stat(config.Path)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.DatabaseBytes == nil || *snapshot.DatabaseBytes != info.Size() || *snapshot.DatabaseBytes <= initialSize {
		t.Fatalf("reported file size = %v, initial=%d actual=%d", snapshot.DatabaseBytes, initialSize, info.Size())
	}
	if snapshot.Entries == nil || *snapshot.Entries != uint64(len(tokens)) || snapshot.MaxEntries != 8388608 {
		t.Fatalf("logical capacity = %+v", snapshot)
	}
}

func TestDurableStateStatsFollowOpenFileAfterRename(t *testing.T) {
	for _, reopen := range []bool{false, true} {
		t.Run(fmt.Sprintf("reopened=%t", reopen), func(t *testing.T) {
			store, config := newDurableStoreFixture(t, 2048)
			owner := durableFixtureOwner()
			if result := store.bind(stateBindingTypeResponseID, "before-rename", owner); result.err != nil {
				t.Fatal(result.err)
			}
			if reopen {
				if err := store.close(); err != nil {
					t.Fatal(err)
				}
				reopened, err := newDurableStateBindingStore(config)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { closeDurableStoreFixture(t, reopened) })
				store = reopened
			}
			h := &ProxyHandler{stateBindings: store}
			initial := readStateBindingStats(t, h)
			if initial.DatabaseBytes == nil {
				t.Fatal("initial database size unavailable")
			}
			movedPath := config.Path + ".renamed"
			if err := os.Rename(config.Path, movedPath); err != nil {
				t.Fatal(err)
			}
			check := func(entries uint64) stateBindingStatsSnapshot {
				t.Helper()
				info, err := os.Stat(movedPath)
				if err != nil {
					t.Fatal(err)
				}
				snapshot := readStateBindingStats(t, h)
				if snapshot.Status != "ready" || snapshot.Entries == nil || *snapshot.Entries != entries || snapshot.DatabaseBytes == nil || *snapshot.DatabaseBytes != info.Size() {
					t.Fatalf("renamed database stats = %+v, want entries=%d bytes=%d", snapshot, entries, info.Size())
				}
				return snapshot
			}
			check(1)
			if err := os.WriteFile(config.Path, []byte("replacement"), 0o600); err != nil {
				t.Fatal(err)
			}
			check(1)
			tokens := make([]stateBindingToken, 1024)
			for i := range tokens {
				tokens[i] = stateBindingToken{stateBindingTypeResponseID, fmt.Sprintf("after-rename-%d", i)}
			}
			if result := store.bindAll(tokens, owner); result.err != nil {
				t.Fatal(result.err)
			}
			if grown := check(1025); *grown.DatabaseBytes <= *initial.DatabaseBytes {
				t.Fatal("size gauge did not track growth of the renamed open file")
			}
			if err := store.close(); err != nil {
				t.Fatal(err)
			}
			if snapshot := readStateBindingStats(t, h); snapshot.Status != "closed" || snapshot.DatabaseBytes != nil {
				t.Fatalf("closed database retained a size gauge: %+v", snapshot)
			}
			config.Path = movedPath
			reopened, err := newDurableStateBindingStore(config)
			if err != nil {
				t.Fatalf("renamed database retained its writer lock after close: %v", err)
			}
			defer closeDurableStoreFixture(t, reopened)
			if snapshot := readStateBindingStats(t, &ProxyHandler{stateBindings: reopened}); snapshot.Entries == nil || *snapshot.Entries != 1025 || snapshot.DatabaseBytes == nil {
				t.Fatalf("reopened renamed database = %+v", snapshot)
			}
		})
	}
}

func TestDurableStateStatsFrozenAndClosed(t *testing.T) {
	store, _ := newDurableStoreFixture(t, 10)
	h := &ProxyHandler{stateBindings: store}
	owner := durableFixtureOwner()
	if result := store.bind(stateBindingTypeResponseID, "committed", owner); result.err != nil {
		t.Fatal(result.err)
	}
	store.durable.beforeCommit = func() error { return errors.New("synthetic storage failure") }
	if result := store.bind(stateBindingTypeResponseID, "uncommitted", owner); !errors.Is(result.err, errDurableStateIO) {
		t.Fatalf("storage failure = %+v", result)
	}
	snapshot := readStateBindingStats(t, h)
	if snapshot.Status != "frozen" || snapshot.Entries == nil || *snapshot.Entries != 1 || snapshot.DatabaseBytes == nil {
		t.Fatalf("frozen stats = %+v", snapshot)
	}
	if result := store.lookup(stateBindingTypeResponseID, "committed"); !errors.Is(result.err, errDurableStateIO) {
		t.Fatalf("stats cleared storage failure: %+v", result)
	}
	if err := store.close(); err != nil {
		t.Fatal(err)
	}
	snapshot = readStateBindingStats(t, h)
	if snapshot.Mode != "durable" || snapshot.Status != "closed" || snapshot.Entries != nil || snapshot.DatabaseBytes != nil || snapshot.CapacityUsagePercent != nil || snapshot.CapacityStatus != "unknown" {
		t.Fatalf("closed stats = %+v", snapshot)
	}
}

func TestDurableStateStatsConcurrentClientsAndClose(t *testing.T) {
	const clients, perClient = 8, 16
	store, _ := newDurableStoreFixture(t, clients*perClient)
	h := &ProxyHandler{stateBindings: store}
	owner := durableFixtureOwner()
	var writers, readers sync.WaitGroup
	stop := make(chan struct{})
	for range 2 {
		readers.Add(1)
		go func() {
			defer readers.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				snapshot := readStateBindingStats(t, h)
				if snapshot.Status == "closed" {
					if snapshot.Entries != nil || snapshot.DatabaseBytes != nil {
						t.Error("closed store reported usable gauges")
					}
					continue
				}
				if snapshot.Status != "ready" || snapshot.Entries == nil || *snapshot.Entries > clients*perClient || snapshot.DatabaseBytes == nil || *snapshot.DatabaseBytes <= 0 {
					t.Errorf("concurrent stats = %+v", snapshot)
				}
			}
		}()
	}
	for client := range clients {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for request := range perClient {
				token := fmt.Sprintf("client-%d-response-%d", client, request)
				if result := store.bind(stateBindingTypeResponseID, token, owner); result.err != nil {
					t.Errorf("concurrent bind: %v", result.err)
				}
			}
		}()
	}
	writers.Wait()
	if snapshot := readStateBindingStats(t, h); snapshot.Entries == nil || *snapshot.Entries != clients*perClient {
		t.Errorf("final entries = %+v", snapshot)
	}
	if err := store.close(); err != nil {
		t.Error(err)
	}
	close(stop)
	readers.Wait()
}
