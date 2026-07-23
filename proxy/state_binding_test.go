package proxy

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type stateBindingTestClock struct {
	now time.Time
}

func (c *stateBindingTestClock) Now() time.Time {
	return c.now
}

func (c *stateBindingTestClock) Advance(d time.Duration) {
	c.now = c.now.Add(d)
}

func newTestStateBindingStore(t *testing.T, maxEntries int, ttl time.Duration, now func() time.Time) *stateBindingStore {
	t.Helper()
	store, err := newStateBindingStore(stateBindingStoreConfig{
		maxEntries: maxEntries,
		ttl:        ttl,
		now:        now,
		digestKey:  bytes.Repeat([]byte{0x5a}, stateBindingDigestKeySize),
	})
	if err != nil {
		t.Fatalf("newStateBindingStore() error = %v", err)
	}
	return store
}

func requireStateBindingResult(t *testing.T, got stateBindingLookupResult, wantOutcome stateBindingLookupOutcome, wantOwner stateBindingOwner) {
	t.Helper()
	if got.outcome != wantOutcome {
		t.Fatalf("outcome = %s, want %s", got.outcome, wantOutcome)
	}
	if got.owner != wantOwner {
		t.Fatalf("owner = %#v, want %#v", got.owner, wantOwner)
	}
	owner, ok := got.knownOwner()
	if wantOutcome == stateBindingLookupKnown {
		if !ok || owner != wantOwner {
			t.Fatalf("knownOwner() = %#v, %v; want %#v, true", owner, ok, wantOwner)
		}
		return
	}
	if ok || owner != (stateBindingOwner{}) {
		t.Fatalf("knownOwner() = %#v, %v; want zero, false", owner, ok)
	}
}

func TestNewStateBindingStoreDefaultsAndValidation(t *testing.T) {
	store, err := newStateBindingStore(stateBindingStoreConfig{
		now:       time.Now,
		digestKey: bytes.Repeat([]byte{0x11}, stateBindingDigestKeySize),
	})
	if err != nil {
		t.Fatalf("newStateBindingStore() error = %v", err)
	}
	if store.maxEntries != defaultStateBindingMaxEntries {
		t.Fatalf("maxEntries = %d, want %d", store.maxEntries, defaultStateBindingMaxEntries)
	}
	if store.ttl != defaultStateBindingTTL {
		t.Fatalf("ttl = %s, want %s", store.ttl, defaultStateBindingTTL)
	}

	tests := []struct {
		name   string
		config stateBindingStoreConfig
		want   error
	}{
		{
			name: "negative capacity",
			config: stateBindingStoreConfig{
				maxEntries: -1,
				digestKey:  bytes.Repeat([]byte{0x22}, stateBindingDigestKeySize),
			},
			want: errStateBindingMaxEntries,
		},
		{
			name: "negative ttl",
			config: stateBindingStoreConfig{
				ttl:       -time.Nanosecond,
				digestKey: bytes.Repeat([]byte{0x22}, stateBindingDigestKeySize),
			},
			want: errStateBindingTTL,
		},
		{
			name: "wrong digest key size",
			config: stateBindingStoreConfig{
				digestKey: []byte("too short"),
			},
			want: errStateBindingDigestKey,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			_, err := newStateBindingStore(tc.config)
			if !errors.Is(err, tc.want) {
				t.Fatalf("newStateBindingStore() error = %v, want %v", err, tc.want)
			}
		})
	}
}

func TestStateBindingStoreNamespacesIdenticalTokensByType(t *testing.T) {
	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	store := newTestStateBindingStore(t, 4, time.Hour, clock.Now)
	responseOwner := stateBindingOwner{routeID: "route-a", targetID: "target-1"}
	turnOwner := stateBindingOwner{routeID: "route-b", targetID: "target-2"}
	const token = "same-opaque-value"

	requireStateBindingResult(t, store.bind(stateBindingTypeResponseID, token, responseOwner), stateBindingLookupKnown, responseOwner)
	requireStateBindingResult(t, store.bind(stateBindingTypeTurnState, token, turnOwner), stateBindingLookupKnown, turnOwner)
	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, token), stateBindingLookupKnown, responseOwner)
	requireStateBindingResult(t, store.lookup(stateBindingTypeTurnState, token), stateBindingLookupKnown, turnOwner)

	stats := store.stats()
	if stats.entries != 2 || stats.tombstones != 0 || stats.collisions != 0 {
		t.Fatalf("stats = %#v, want two independent live bindings", stats)
	}
}

func TestStateBindingStoreLookupDoesNotExtendAbsoluteTTL(t *testing.T) {
	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	store := newTestStateBindingStore(t, 4, 10*time.Minute, clock.Now)
	owner := stateBindingOwner{routeID: "route-a", targetID: "target-1"}

	requireStateBindingResult(t, store.bind(stateBindingTypeResponseID, "resp-1", owner), stateBindingLookupKnown, owner)
	clock.Advance(9 * time.Minute)
	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, "resp-1"), stateBindingLookupKnown, owner)
	clock.Advance(time.Minute)
	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, "resp-1"), stateBindingLookupUnknown, stateBindingOwner{})

	stats := store.stats()
	if stats.entries != 0 || stats.expirations != 1 || stats.evictions != 0 {
		t.Fatalf("stats = %#v, want one expiration and no capacity eviction", stats)
	}
}

func TestStateBindingStoreSameOwnerRebindRefreshesTTL(t *testing.T) {
	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	store := newTestStateBindingStore(t, 4, 10*time.Minute, clock.Now)
	owner := stateBindingOwner{routeID: "route-a", targetID: "target-1"}

	requireStateBindingResult(t, store.bind(stateBindingTypeResponseID, "resp-1", owner), stateBindingLookupKnown, owner)
	clock.Advance(9 * time.Minute)
	requireStateBindingResult(t, store.bind(stateBindingTypeResponseID, "resp-1", owner), stateBindingLookupKnown, owner)
	clock.Advance(2 * time.Minute)
	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, "resp-1"), stateBindingLookupKnown, owner)
	clock.Advance(8 * time.Minute)
	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, "resp-1"), stateBindingLookupUnknown, stateBindingOwner{})
}

func TestStateBindingStoreCollisionCreatesTombstoneUntilCollisionExpiry(t *testing.T) {
	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	store := newTestStateBindingStore(t, 4, 10*time.Minute, clock.Now)
	ownerA := stateBindingOwner{routeID: "route-a", targetID: "target-1"}
	ownerB := stateBindingOwner{routeID: "route-a", targetID: "target-2"}

	requireStateBindingResult(t, store.bind(stateBindingTypeResponseID, "resp-collision", ownerA), stateBindingLookupKnown, ownerA)
	clock.Advance(9 * time.Minute)
	requireStateBindingResult(t, store.bind(stateBindingTypeResponseID, "resp-collision", ownerB), stateBindingLookupConflict, stateBindingOwner{})

	stats := store.stats()
	if stats.entries != 1 || stats.tombstones != 1 || stats.collisions != 1 {
		t.Fatalf("stats after collision = %#v, want one tombstone and one collision", stats)
	}

	// The collision starts a fresh fail-closed TTL, so the tombstone survives
	// beyond the original owner's expiry.
	clock.Advance(2 * time.Minute)
	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, "resp-collision"), stateBindingLookupConflict, stateBindingOwner{})
	requireStateBindingResult(t, store.bind(stateBindingTypeResponseID, "resp-collision", ownerA), stateBindingLookupConflict, stateBindingOwner{})
	requireStateBindingResult(t, store.bind(stateBindingTypeResponseID, "resp-collision", ownerB), stateBindingLookupConflict, stateBindingOwner{})
	if got := store.stats().collisions; got != 1 {
		t.Fatalf("collisions = %d, want 1 tombstone transition", got)
	}

	clock.Advance(8 * time.Minute)
	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, "resp-collision"), stateBindingLookupUnknown, stateBindingOwner{})
	requireStateBindingResult(t, store.bind(stateBindingTypeResponseID, "resp-collision", ownerB), stateBindingLookupKnown, ownerB)
}

func TestStateBindingStoreEvictsLeastRecentlyUsedEntry(t *testing.T) {
	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	store := newTestStateBindingStore(t, 2, time.Hour, clock.Now)
	owner := stateBindingOwner{routeID: "route-a", targetID: "target-1"}

	store.bind(stateBindingTypeResponseID, "resp-1", owner)
	store.bind(stateBindingTypeResponseID, "resp-2", owner)
	store.lookup(stateBindingTypeResponseID, "resp-1") // resp-1 becomes most recent.
	store.bind(stateBindingTypeResponseID, "resp-3", owner)

	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, "resp-1"), stateBindingLookupKnown, owner)
	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, "resp-2"), stateBindingLookupUnknown, stateBindingOwner{})
	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, "resp-3"), stateBindingLookupKnown, owner)

	stats := store.stats()
	if stats.entries != 2 || stats.evictions != 1 || stats.expirations != 0 {
		t.Fatalf("stats = %#v, want one LRU capacity eviction", stats)
	}
}

func TestStateBindingStorePrunesExpiredBeforeCapacityEviction(t *testing.T) {
	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	store := newTestStateBindingStore(t, 2, 10*time.Minute, clock.Now)
	owner := stateBindingOwner{routeID: "route-a", targetID: "target-1"}

	store.bind(stateBindingTypeResponseID, "expired", owner)
	clock.Advance(5 * time.Minute)
	store.bind(stateBindingTypeResponseID, "live", owner)
	clock.Advance(6 * time.Minute)
	store.bind(stateBindingTypeResponseID, "new", owner)

	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, "expired"), stateBindingLookupUnknown, stateBindingOwner{})
	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, "live"), stateBindingLookupKnown, owner)
	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, "new"), stateBindingLookupKnown, owner)

	stats := store.stats()
	if stats.entries != 2 || stats.expirations != 1 || stats.evictions != 0 {
		t.Fatalf("stats = %#v, want expired entry pruned without capacity eviction", stats)
	}
}

func TestStateBindingStoreEvictedTombstoneBecomesUnknown(t *testing.T) {
	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	store := newTestStateBindingStore(t, 1, time.Hour, clock.Now)
	ownerA := stateBindingOwner{routeID: "route-a", targetID: "target-1"}
	ownerB := stateBindingOwner{routeID: "route-a", targetID: "target-2"}

	store.bind(stateBindingTypeResponseID, "ambiguous", ownerA)
	store.bind(stateBindingTypeResponseID, "ambiguous", ownerB)
	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, "ambiguous"), stateBindingLookupConflict, stateBindingOwner{})

	store.bind(stateBindingTypeResponseID, "replacement", ownerA)
	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, "ambiguous"), stateBindingLookupUnknown, stateBindingOwner{})

	stats := store.stats()
	if stats.entries != 1 || stats.tombstones != 0 || stats.collisions != 1 || stats.evictions != 1 {
		t.Fatalf("stats = %#v, want evicted tombstone accounted and degraded to unknown", stats)
	}
}

func TestStateBindingStoreResolveKnownTokensMustAgree(t *testing.T) {
	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	store := newTestStateBindingStore(t, 8, time.Hour, clock.Now)
	owner := stateBindingOwner{routeID: "route-a", targetID: "target-2"}
	tokens := []stateBindingToken{
		{stateType: stateBindingTypeResponseID, value: "resp-1"},
		{stateType: stateBindingTypeTurnState, value: "turn-1"},
		{stateType: stateBindingTypeEncryptedContent, value: "opaque-1"},
	}
	for _, token := range tokens {
		store.bind(token.stateType, token.value, owner)
	}

	requireStateBindingResult(t, store.resolve(tokens), stateBindingLookupKnown, owner)
	requireStateBindingResult(t, store.resolveForRoute("route-a", tokens), stateBindingLookupKnown, owner)
}

func TestStateBindingStoreResolveFailsClosed(t *testing.T) {
	clockStart := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	ownerA1 := stateBindingOwner{routeID: "route-a", targetID: "target-1"}
	ownerA2 := stateBindingOwner{routeID: "route-a", targetID: "target-2"}
	ownerB1 := stateBindingOwner{routeID: "route-b", targetID: "target-1"}

	t.Run("no supplied state", func(t *testing.T) {
		clock := &stateBindingTestClock{now: clockStart}
		store := newTestStateBindingStore(t, 8, time.Hour, clock.Now)
		requireStateBindingResult(t, store.resolve(nil), stateBindingLookupUnknown, stateBindingOwner{})
	})

	t.Run("all unknown", func(t *testing.T) {
		clock := &stateBindingTestClock{now: clockStart}
		store := newTestStateBindingStore(t, 8, time.Hour, clock.Now)
		requireStateBindingResult(t, store.resolve([]stateBindingToken{
			{stateType: stateBindingTypeResponseID, value: "missing-response"},
			{stateType: stateBindingTypeTurnState, value: "missing-turn"},
		}), stateBindingLookupUnknown, stateBindingOwner{})
	})

	t.Run("mixed known and unknown", func(t *testing.T) {
		clock := &stateBindingTestClock{now: clockStart}
		store := newTestStateBindingStore(t, 8, time.Hour, clock.Now)
		store.bind(stateBindingTypeResponseID, "known", ownerA1)
		requireStateBindingResult(t, store.resolve([]stateBindingToken{
			{stateType: stateBindingTypeResponseID, value: "known"},
			{stateType: stateBindingTypeTurnState, value: "unknown"},
		}), stateBindingLookupConflict, stateBindingOwner{})
	})

	t.Run("same route different targets", func(t *testing.T) {
		clock := &stateBindingTestClock{now: clockStart}
		store := newTestStateBindingStore(t, 8, time.Hour, clock.Now)
		store.bind(stateBindingTypeResponseID, "resp", ownerA1)
		store.bind(stateBindingTypeTurnState, "turn", ownerA2)
		requireStateBindingResult(t, store.resolve([]stateBindingToken{
			{stateType: stateBindingTypeResponseID, value: "resp"},
			{stateType: stateBindingTypeTurnState, value: "turn"},
		}), stateBindingLookupConflict, stateBindingOwner{})
	})

	t.Run("different routes", func(t *testing.T) {
		clock := &stateBindingTestClock{now: clockStart}
		store := newTestStateBindingStore(t, 8, time.Hour, clock.Now)
		store.bind(stateBindingTypeResponseID, "resp", ownerA1)
		store.bind(stateBindingTypeTurnState, "turn", ownerB1)
		requireStateBindingResult(t, store.resolve([]stateBindingToken{
			{stateType: stateBindingTypeResponseID, value: "resp"},
			{stateType: stateBindingTypeTurnState, value: "turn"},
		}), stateBindingLookupConflict, stateBindingOwner{})
	})

	t.Run("collision tombstone", func(t *testing.T) {
		clock := &stateBindingTestClock{now: clockStart}
		store := newTestStateBindingStore(t, 8, time.Hour, clock.Now)
		store.bind(stateBindingTypeResponseID, "ambiguous", ownerA1)
		store.bind(stateBindingTypeResponseID, "ambiguous", ownerA2)
		requireStateBindingResult(t, store.resolve([]stateBindingToken{
			{stateType: stateBindingTypeResponseID, value: "ambiguous"},
		}), stateBindingLookupConflict, stateBindingOwner{})
	})

	t.Run("malformed token", func(t *testing.T) {
		clock := &stateBindingTestClock{now: clockStart}
		store := newTestStateBindingStore(t, 8, time.Hour, clock.Now)
		requireStateBindingResult(t, store.resolve([]stateBindingToken{
			{stateType: stateBindingTypeResponseID, value: ""},
		}), stateBindingLookupConflict, stateBindingOwner{})
		requireStateBindingResult(t, store.resolve([]stateBindingToken{
			{stateType: "", value: "value"},
		}), stateBindingLookupConflict, stateBindingOwner{})
	})
}

func TestStateBindingStoreResolveForRouteRejectsCrossRouteOwnership(t *testing.T) {
	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	store := newTestStateBindingStore(t, 4, time.Hour, clock.Now)
	owner := stateBindingOwner{routeID: "route-a", targetID: "target-1"}
	store.bind(stateBindingTypeResponseID, "resp-1", owner)

	requireStateBindingResult(t, store.resolveForRoute("route-b", []stateBindingToken{
		{stateType: stateBindingTypeResponseID, value: "resp-1"},
	}), stateBindingLookupConflict, stateBindingOwner{})
}

func TestStateBindingStoreRejectsMalformedBindAndLookup(t *testing.T) {
	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	store := newTestStateBindingStore(t, 4, time.Hour, clock.Now)
	validOwner := stateBindingOwner{routeID: "route-a", targetID: "target-1"}

	tests := []struct {
		name      string
		stateType stateBindingType
		token     string
		owner     stateBindingOwner
	}{
		{name: "missing state type", token: "value", owner: validOwner},
		{name: "missing token", stateType: stateBindingTypeResponseID, owner: validOwner},
		{name: "missing route", stateType: stateBindingTypeResponseID, token: "value", owner: stateBindingOwner{targetID: "target-1"}},
		{name: "missing target", stateType: stateBindingTypeResponseID, token: "value", owner: stateBindingOwner{routeID: "route-a"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requireStateBindingResult(t, store.bind(tc.stateType, tc.token, tc.owner), stateBindingLookupConflict, stateBindingOwner{})
		})
	}
	requireStateBindingResult(t, store.lookup("", "value"), stateBindingLookupConflict, stateBindingOwner{})
	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, ""), stateBindingLookupConflict, stateBindingOwner{})
	if stats := store.stats(); stats.entries != 0 {
		t.Fatalf("stats = %#v, malformed inputs must not create records", stats)
	}
}

func TestStateBindingStoreDoesNotRetainOrReturnRawTokens(t *testing.T) {
	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	store := newTestStateBindingStore(t, 4, time.Hour, clock.Now)
	owner := stateBindingOwner{routeID: "route-a", targetID: "target-1"}
	const rawToken = "raw-provider-state-MUST-NOT-BE-STORED"

	result := store.bind(stateBindingTypeEncryptedContent, rawToken, owner)
	requireStateBindingResult(t, result, stateBindingLookupKnown, owner)

	for _, rendered := range []string{
		fmt.Sprintf("%#v", store.entries),
		fmt.Sprintf("%#v", result),
		fmt.Sprintf("%#v", store.stats()),
	} {
		if strings.Contains(rendered, rawToken) {
			t.Fatalf("state binding output retained raw token: %s", rendered)
		}
	}
}

func TestStateBindingStoreConcurrentAccessIsBounded(t *testing.T) {
	const bindings = 128
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	store := newTestStateBindingStore(t, bindings, time.Hour, func() time.Time { return now })
	owner := stateBindingOwner{routeID: "route-a", targetID: "target-1"}

	var wg sync.WaitGroup
	errs := make(chan string, bindings)
	for i := 0; i < bindings; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			token := fmt.Sprintf("resp-%03d", i)
			if result := store.bind(stateBindingTypeResponseID, token, owner); result.outcome != stateBindingLookupKnown || result.owner != owner {
				errs <- fmt.Sprintf("bind %d = %#v", i, result)
				return
			}
			if result := store.lookup(stateBindingTypeResponseID, token); result.outcome != stateBindingLookupKnown || result.owner != owner {
				errs <- fmt.Sprintf("lookup %d = %#v", i, result)
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}

	stats := store.stats()
	if stats.entries != bindings || stats.evictions != 0 || stats.expirations != 0 {
		t.Fatalf("stats = %#v, want %d live bounded entries", stats, bindings)
	}
}

func TestStateBindingStoresAreProcessLocalAndIndependent(t *testing.T) {
	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	storeA := newTestStateBindingStore(t, 4, time.Hour, clock.Now)
	storeB := newTestStateBindingStore(t, 4, time.Hour, clock.Now)
	owner := stateBindingOwner{routeID: "route-a", targetID: "target-1"}

	storeA.bind(stateBindingTypeResponseID, "resp-process-local", owner)
	requireStateBindingResult(t, storeA.lookup(stateBindingTypeResponseID, "resp-process-local"), stateBindingLookupKnown, owner)
	requireStateBindingResult(t, storeB.lookup(stateBindingTypeResponseID, "resp-process-local"), stateBindingLookupUnknown, stateBindingOwner{})
}

func TestNilStateBindingStoreFailsClosed(t *testing.T) {
	var store *stateBindingStore
	owner := stateBindingOwner{routeID: "route-a", targetID: "target-1"}

	requireStateBindingResult(t, store.bind(stateBindingTypeResponseID, "resp-1", owner), stateBindingLookupConflict, stateBindingOwner{})
	requireStateBindingResult(t, store.lookup(stateBindingTypeResponseID, "resp-1"), stateBindingLookupUnknown, stateBindingOwner{})
	requireStateBindingResult(t, store.resolve([]stateBindingToken{{stateType: stateBindingTypeResponseID, value: "resp-1"}}), stateBindingLookupUnknown, stateBindingOwner{})
	if stats := store.stats(); stats != (stateBindingStoreStats{}) {
		t.Fatalf("stats = %#v, want zero", stats)
	}
}

func TestStateBindingLookupOutcomeString(t *testing.T) {
	tests := []struct {
		outcome stateBindingLookupOutcome
		want    string
	}{
		{outcome: stateBindingLookupUnknown, want: "unknown"},
		{outcome: stateBindingLookupKnown, want: "known"},
		{outcome: stateBindingLookupConflict, want: "conflict"},
		{outcome: stateBindingLookupOutcome(255), want: "invalid"},
	}
	for _, tc := range tests {
		if got := tc.outcome.String(); got != tc.want {
			t.Fatalf("outcome %d String() = %q, want %q", tc.outcome, got, tc.want)
		}
	}
}

func TestStateBindingStoreBindAllDoesNotPartiallyPublishOnConflict(t *testing.T) {
	clock := &stateBindingTestClock{now: time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)}
	store := newTestStateBindingStore(t, 8, time.Hour, clock.Now)
	ownerA := stateBindingOwner{routeID: "route-a", targetID: "target-a"}
	ownerB := stateBindingOwner{routeID: "route-a", targetID: "target-b"}
	store.bind(stateBindingTypeResponseID, "existing", ownerA)

	result := store.bindAll([]stateBindingToken{
		{stateType: stateBindingTypeResponseID, value: "new-hidden"},
		{stateType: stateBindingTypeResponseID, value: "existing"},
	}, ownerB)
	if result.outcome != stateBindingLookupConflict {
		t.Fatalf("bindAll outcome = %s, want conflict", result.outcome)
	}
	if got := store.lookup(stateBindingTypeResponseID, "new-hidden"); got.outcome != stateBindingLookupUnknown {
		t.Fatalf("unexposed token outcome = %s, want unknown", got.outcome)
	}
	if got := store.lookup(stateBindingTypeResponseID, "existing"); got.outcome != stateBindingLookupConflict {
		t.Fatalf("colliding token outcome = %s, want conflict", got.outcome)
	}
}

func TestExtractExplicitResponsesRequestStateRejectsMalformedEncryptedContent(t *testing.T) {
	_, err := extractExplicitResponsesRequestState([]byte(`{"model":"route","input":[{"type":"reasoning","encrypted_content":42}]}`), nil)
	if err == nil || !strings.Contains(err.Error(), "encrypted_content") {
		t.Fatalf("error = %v, want encrypted_content validation", err)
	}
}

func TestStateBindingOwnerCredentialGenerationConflicts(t *testing.T) {
	store := newTestStateBindingStore(t, 16, time.Hour, time.Now)
	token := stateBindingToken{stateType: stateBindingTypeResponseID, value: "resp-credential"}
	ownerA := stateBindingOwner{routeID: "route", targetID: "target", credentialID: "credential-a"}
	ownerB := stateBindingOwner{routeID: "route", targetID: "target", credentialID: "credential-b"}
	if result := store.bind(token.stateType, token.value, ownerA); result.outcome != stateBindingLookupKnown {
		t.Fatalf("first bind outcome = %s, want known", result.outcome)
	}
	if result := store.bind(token.stateType, token.value, ownerB); result.outcome != stateBindingLookupConflict {
		t.Fatalf("cross-credential bind outcome = %s, want conflict", result.outcome)
	}
	if result := store.resolveForRoute("route", []stateBindingToken{token}); result.outcome != stateBindingLookupConflict {
		t.Fatalf("conflicted credential lookup outcome = %s, want conflict", result.outcome)
	}
}
