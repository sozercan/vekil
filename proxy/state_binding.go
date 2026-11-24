package proxy

import (
	"container/list"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"
)

const (
	defaultStateBindingMaxEntries = 256 * 1024
	defaultStateBindingTTL        = 24 * time.Hour
	stateBindingDigestKeySize     = sha256.Size
)

type stateBindingType string

const (
	stateBindingTypeResponseID       stateBindingType = "response_id"
	stateBindingTypeTurnState        stateBindingType = "codex_turn_state"
	stateBindingTypeEncryptedContent stateBindingType = "encrypted_content"
)

type stateBindingOwner struct {
	routeID   string
	targetID  string
	routeKey  [32]byte
	targetKey [32]byte
	identity  [32]byte
}

func (o stateBindingOwner) valid() bool {
	return o.routeID != "" && o.targetID != ""
}

type stateBindingLookupOutcome uint8

const (
	stateBindingLookupUnknown stateBindingLookupOutcome = iota
	stateBindingLookupKnown
	stateBindingLookupConflict
)

func (o stateBindingLookupOutcome) String() string {
	switch o {
	case stateBindingLookupUnknown:
		return "unknown"
	case stateBindingLookupKnown:
		return "known"
	case stateBindingLookupConflict:
		return "conflict"
	default:
		return "invalid"
	}
}

type stateBindingLookupResult struct {
	outcome stateBindingLookupOutcome
	owner   stateBindingOwner
	err     error
}

func (r stateBindingLookupResult) knownOwner() (stateBindingOwner, bool) {
	if r.outcome != stateBindingLookupKnown {
		return stateBindingOwner{}, false
	}
	return r.owner, true
}

type stateBindingToken struct {
	stateType stateBindingType
	value     string
}

type stateBindingStoreConfig struct {
	maxEntries int
	ttl        time.Duration
	now        func() time.Time

	digestKey []byte
}

type stateBindingStoreStats struct {
	entries     int
	tombstones  int
	evictions   uint64
	expirations uint64
	collisions  uint64
}

var (
	errStateBindingMaxEntries = errors.New("state binding max entries must be positive")
	errStateBindingTTL        = errors.New("state binding TTL must be positive")
	errStateBindingDigestKey  = fmt.Errorf("state binding digest key must be %d bytes", stateBindingDigestKeySize)
)

type stateBindingKey struct {
	stateType stateBindingType
	digest    [sha256.Size]byte
}

type stateBindingRecord struct {
	key       stateBindingKey
	owner     stateBindingOwner
	outcome   stateBindingLookupOutcome
	expiresAt time.Time
}

type stateBindingStore struct {
	mu      sync.Mutex
	durable *durableStateBindings

	maxEntries int
	ttl        time.Duration
	now        func() time.Time
	digestKey  [stateBindingDigestKeySize]byte

	entries map[stateBindingKey]*list.Element
	recency *list.List

	evictions   uint64
	expirations uint64
	collisions  uint64
}

func newStateBindingStore(config stateBindingStoreConfig) (*stateBindingStore, error) {
	maxEntries := config.maxEntries
	if maxEntries == 0 {
		maxEntries = defaultStateBindingMaxEntries
	}
	if maxEntries < 0 {
		return nil, errStateBindingMaxEntries
	}

	ttl := config.ttl
	if ttl == 0 {
		ttl = defaultStateBindingTTL
	}
	if ttl < 0 {
		return nil, errStateBindingTTL
	}

	now := config.now
	if now == nil {
		now = time.Now
	}

	store := &stateBindingStore{
		maxEntries: maxEntries,
		ttl:        ttl,
		now:        now,
		entries:    make(map[stateBindingKey]*list.Element),
		recency:    list.New(),
	}

	switch len(config.digestKey) {
	case 0:
		if _, err := rand.Read(store.digestKey[:]); err != nil {
			return nil, fmt.Errorf("initialize state binding digest key: %w", err)
		}
	case stateBindingDigestKeySize:
		copy(store.digestKey[:], config.digestKey)
	default:
		return nil, errStateBindingDigestKey
	}

	return store, nil
}

func (s *stateBindingStore) bind(stateType stateBindingType, token string, owner stateBindingOwner) stateBindingLookupResult {
	if s != nil && s.durable != nil {
		return s.durable.bind([]stateBindingToken{{stateType: stateType, value: token}}, owner, false)
	}
	if s == nil || stateType == "" || token == "" || !owner.valid() {
		return stateBindingLookupResult{outcome: stateBindingLookupConflict}
	}

	key := s.bindingKey(stateType, token)
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	if elem, ok := s.entries[key]; ok {
		record := elem.Value.(*stateBindingRecord)
		if record.expired(now) {
			s.removeLocked(elem, stateBindingRemovalExpired)
		} else {
			s.recency.MoveToFront(elem)
			switch {
			case record.outcome == stateBindingLookupConflict:
				return stateBindingLookupResult{outcome: stateBindingLookupConflict}
			case record.owner == owner:
				record.expiresAt = now.Add(s.ttl)
				return stateBindingLookupResult{outcome: stateBindingLookupKnown, owner: owner}
			default:
				record.owner = stateBindingOwner{}
				record.outcome = stateBindingLookupConflict
				record.expiresAt = now.Add(s.ttl)
				s.collisions++
				return stateBindingLookupResult{outcome: stateBindingLookupConflict}
			}
		}
	}

	if len(s.entries) >= s.maxEntries {
		s.pruneExpiredLocked(now)
	}
	for len(s.entries) >= s.maxEntries {
		s.removeLocked(s.recency.Back(), stateBindingRemovalCapacity)
	}

	record := &stateBindingRecord{
		key:       key,
		owner:     owner,
		outcome:   stateBindingLookupKnown,
		expiresAt: now.Add(s.ttl),
	}
	elem := s.recency.PushFront(record)
	s.entries[key] = elem
	return stateBindingLookupResult{outcome: stateBindingLookupKnown, owner: owner}
}

func (s *stateBindingStore) lookup(stateType stateBindingType, token string) stateBindingLookupResult {
	if s != nil && s.durable != nil {
		return s.durable.resolve([]stateBindingToken{{stateType: stateType, value: token}})
	}
	if s == nil {
		return stateBindingLookupResult{outcome: stateBindingLookupUnknown}
	}
	if stateType == "" || token == "" {
		return stateBindingLookupResult{outcome: stateBindingLookupConflict}
	}

	key := s.bindingKey(stateType, token)
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lookupKeyLocked(key, now)
}

func (s *stateBindingStore) resolve(tokens []stateBindingToken) stateBindingLookupResult {
	return s.resolveWithRoute(tokens, "", "")
}

func (s *stateBindingStore) resolveWithRoute(tokens []stateBindingToken, routeID, pinnedTargetID string) stateBindingLookupResult {
	if s != nil && s.durable != nil {
		return s.durable.resolveWithRoute(tokens, routeID, pinnedTargetID)
	}
	if len(tokens) == 0 || s == nil {
		return stateBindingLookupResult{outcome: stateBindingLookupUnknown}
	}

	keys := make([]stateBindingKey, len(tokens))
	for i, token := range tokens {
		if token.stateType == "" || token.value == "" {
			return stateBindingLookupResult{outcome: stateBindingLookupConflict}
		}
		keys[i] = s.bindingKey(token.stateType, token.value)
	}
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	var owner stateBindingOwner
	haveKnown := false
	haveUnknown := false
	for _, key := range keys {
		result := s.lookupKeyLocked(key, now)
		switch result.outcome {
		case stateBindingLookupConflict:
			return stateBindingLookupResult{outcome: stateBindingLookupConflict}
		case stateBindingLookupUnknown:
			haveUnknown = true
		case stateBindingLookupKnown:
			if (routeID != "" && result.owner.routeID != routeID) ||
				(pinnedTargetID != "" && result.owner.targetID != pinnedTargetID) {
				return stateBindingLookupResult{outcome: stateBindingLookupConflict}
			}
			if !haveKnown {
				owner = result.owner
				haveKnown = true
				continue
			}
			if owner != result.owner {
				return stateBindingLookupResult{outcome: stateBindingLookupConflict}
			}
		default:
			return stateBindingLookupResult{outcome: stateBindingLookupConflict}
		}
	}

	if haveKnown && !haveUnknown {
		return stateBindingLookupResult{outcome: stateBindingLookupKnown, owner: owner}
	}
	return stateBindingLookupResult{outcome: stateBindingLookupUnknown}
}

func (s *stateBindingStore) resolveForRoute(routeID, pinnedTargetID string, tokens []stateBindingToken) stateBindingLookupResult {
	result := s.resolveWithRoute(tokens, routeID, pinnedTargetID)
	if result.outcome != stateBindingLookupKnown {
		return result
	}
	if routeID == "" || !s.ownerMatchesRoute(result.owner, routeID) {
		return stateBindingLookupResult{outcome: stateBindingLookupConflict}
	}
	return result
}

func (s *stateBindingStore) stats() stateBindingStoreStats {
	if s != nil && s.durable != nil {
		return s.durable.stats()
	}
	if s == nil {
		return stateBindingStoreStats{}
	}
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()
	s.pruneExpiredLocked(now)

	stats := stateBindingStoreStats{
		entries:     len(s.entries),
		evictions:   s.evictions,
		expirations: s.expirations,
		collisions:  s.collisions,
	}
	for elem := s.recency.Front(); elem != nil; elem = elem.Next() {
		if elem.Value.(*stateBindingRecord).outcome == stateBindingLookupConflict {
			stats.tombstones++
		}
	}
	return stats
}

func (s *stateBindingStore) bindingKey(stateType stateBindingType, token string) stateBindingKey {
	mac := hmac.New(sha256.New, s.digestKey[:])
	_, _ = io.WriteString(mac, token)
	var digest [sha256.Size]byte
	copy(digest[:], mac.Sum(nil))
	return stateBindingKey{stateType: stateType, digest: digest}
}

func (s *stateBindingStore) lookupKeyLocked(key stateBindingKey, now time.Time) stateBindingLookupResult {
	elem, ok := s.entries[key]
	if !ok {
		return stateBindingLookupResult{outcome: stateBindingLookupUnknown}
	}
	record := elem.Value.(*stateBindingRecord)
	if record.expired(now) {
		s.removeLocked(elem, stateBindingRemovalExpired)
		return stateBindingLookupResult{outcome: stateBindingLookupUnknown}
	}

	s.recency.MoveToFront(elem)
	if record.outcome == stateBindingLookupConflict {
		return stateBindingLookupResult{outcome: stateBindingLookupConflict}
	}
	return stateBindingLookupResult{outcome: stateBindingLookupKnown, owner: record.owner}
}

func (s *stateBindingStore) pruneExpiredLocked(now time.Time) {
	for elem := s.recency.Back(); elem != nil; {
		previous := elem.Prev()
		if elem.Value.(*stateBindingRecord).expired(now) {
			s.removeLocked(elem, stateBindingRemovalExpired)
		}
		elem = previous
	}
}

type stateBindingRemovalReason uint8

const (
	stateBindingRemovalExpired stateBindingRemovalReason = iota
	stateBindingRemovalCapacity
)

func (s *stateBindingStore) removeLocked(elem *list.Element, reason stateBindingRemovalReason) {
	if elem == nil {
		return
	}
	record := elem.Value.(*stateBindingRecord)
	delete(s.entries, record.key)
	s.recency.Remove(elem)
	switch reason {
	case stateBindingRemovalExpired:
		s.expirations++
	case stateBindingRemovalCapacity:
		s.evictions++
	}
}

func (r *stateBindingRecord) expired(now time.Time) bool {
	return !now.Before(r.expiresAt)
}

func (s *stateBindingStore) bindAll(tokens []stateBindingToken, owner stateBindingOwner) stateBindingLookupResult {
	result, _ := s.bindAllWithEvictionDelta(tokens, owner)
	return result
}

func (s *stateBindingStore) bindAllWithEvictionDelta(tokens []stateBindingToken, owner stateBindingOwner) (stateBindingLookupResult, uint64) {
	if s != nil && s.durable != nil {
		return s.durable.bind(tokens, owner, false), 0
	}
	if s == nil || len(tokens) == 0 || !owner.valid() || len(tokens) > s.maxEntries {
		return stateBindingLookupResult{outcome: stateBindingLookupConflict}, 0
	}
	keys := make([]stateBindingKey, 0, len(tokens))
	seen := make(map[stateBindingKey]struct{}, len(tokens))
	for _, token := range tokens {
		if token.stateType == "" || token.value == "" {
			return stateBindingLookupResult{outcome: stateBindingLookupConflict}, 0
		}
		key := s.bindingKey(token.stateType, token.value)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		keys = append(keys, key)
	}
	if len(keys) == 0 {
		return stateBindingLookupResult{outcome: stateBindingLookupConflict}, 0
	}

	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	evictionsBefore := s.evictions
	finish := func(result stateBindingLookupResult) (stateBindingLookupResult, uint64) {
		return result, s.evictions - evictionsBefore
	}
	s.pruneExpiredLocked(now)

	conflicted := false
	for _, key := range keys {
		elem, ok := s.entries[key]
		if !ok {
			continue
		}
		record := elem.Value.(*stateBindingRecord)
		s.recency.MoveToFront(elem)
		if record.outcome == stateBindingLookupConflict {
			conflicted = true
			continue
		}
		if record.owner != owner {
			record.owner = stateBindingOwner{}
			record.outcome = stateBindingLookupConflict
			record.expiresAt = now.Add(s.ttl)
			s.collisions++
			conflicted = true
		}
	}
	if conflicted {
		return finish(stateBindingLookupResult{outcome: stateBindingLookupConflict})
	}

	for _, key := range keys {
		if elem, ok := s.entries[key]; ok {
			record := elem.Value.(*stateBindingRecord)
			record.owner = owner
			record.outcome = stateBindingLookupKnown
			record.expiresAt = now.Add(s.ttl)
			s.recency.MoveToFront(elem)
			continue
		}
		for len(s.entries) >= s.maxEntries {
			s.removeLocked(s.recency.Back(), stateBindingRemovalCapacity)
		}
		record := &stateBindingRecord{key: key, owner: owner, outcome: stateBindingLookupKnown, expiresAt: now.Add(s.ttl)}
		elem := s.recency.PushFront(record)
		s.entries[key] = elem
	}
	return finish(stateBindingLookupResult{outcome: stateBindingLookupKnown, owner: owner})
}
