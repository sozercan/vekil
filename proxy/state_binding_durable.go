package proxy

import (
	"bytes"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

// DurableStateBindingsConfig enables local, single-writer ownership recovery.
// Path must name a private file in an existing owner-only directory. MaxEntries
// limits logical records (including tombstones), NOT database-file bytes.
// Records never expire automatically. Empty Path preserves memory-only mode.
type DurableStateBindingsConfig struct {
	Path       string
	MaxEntries int
}

func WithDurableStateBindings(config DurableStateBindingsConfig) Option {
	return func(h *ProxyHandler) { h.durableStateConfig = config }
}

var (
	errDurableStateConfig   = errors.New("invalid durable state configuration")
	errDurableStatePath     = errors.New("durable state requires a private regular file in an owner-only local directory")
	errDurableStatePlatform = errors.New("durable state is supported only on tested Linux local filesystems")
	errDurableStateLocked   = errors.New("durable state store is already in use")
	errDurableStateCorrupt  = errors.New("durable state store is corrupt or incompatible; preserve it for operator recovery")
	errDurableStateIO       = errors.New("durable state storage failed; state was not exposed; operator recovery is required")
	errDurableStateCapacity = errors.New("durable state record capacity reached; increase the limit or explicitly prune offline")
	errDurableStateClosed   = errors.New("durable state store is closed")
	errDurableStateIdentity = errors.New("provider state ownership identity changed or cannot be established")
	errDurableStateUnknown  = errors.New("unknown provider-bound state for explicit model route; durable ownership proof is missing or explicitly pruned")
	errDurableStateConflict = errors.New("conflicting provider-bound state for explicit model route")
)

var durableStateMetadata = []byte("metadata")
var durableStateRecords = []byte("bindings")
var durableStateFormat = []byte("vekil-state-v1")

// Fixed records contain outcome, three keyed owner components, issuance time,
// and a MAC over both the typed key and record. They retain no raw state or
// account/configuration labels. The MAC detects wrong keys and record corruption.
const durableStateRecordSize = 1 + 3*sha256.Size + 8 + sha256.Size

type durableStateBindings struct {
	mu         sync.Mutex
	db         *bolt.DB
	key        [32]byte
	maxEntries int
	now        func() time.Time
	failed     error
	// Test barriers surround the real, synchronously committed transaction.
	// Production never installs hooks or disables bbolt's data/meta syncs.
	beforeCommit func() error
	afterCommit  func() error
}

func newDurableStateBindingStore(config DurableStateBindingsConfig) (*stateBindingStore, error) {
	return openDurableStateBindingStore(config, true)
}

func openDurableStateBindingStore(config DurableStateBindingsConfig, allowCreate bool) (*stateBindingStore, error) {
	if config.MaxEntries == 0 {
		config.MaxEntries = defaultStateBindingMaxEntries
	}
	if config.Path == "" || config.MaxEntries < 1 {
		return nil, errDurableStateConfig
	}
	db, created, syncDirectory, err := openDurableStateDatabase(config.Path, allowCreate)
	if err != nil {
		return nil, err
	}
	d := &durableStateBindings{db: db, maxEntries: config.MaxEntries, now: time.Now}
	if err := d.initialize(created); err != nil {
		_ = syncDirectory()
		_ = db.Close()
		return nil, err
	}
	if err := syncDirectory(); err != nil {
		_ = db.Close()
		return nil, errDurableStateIO
	}
	return &stateBindingStore{durable: d, maxEntries: config.MaxEntries, digestKey: d.key, now: time.Now}, nil
}

// PruneDurableStateBindings explicitly retires records first issued (or marked
// conflicting) before the cutoff. It takes the same exclusive process lock as
// serving, never creates a missing store, and does not shrink the database file.
// Pruned continuation state becomes unknown. No credentials/providers are loaded.
// The cutoff must be a whole second, matching the persisted issuance precision.
func PruneDurableStateBindings(path string, before time.Time) (removed int, err error) {
	if before.IsZero() || before.Unix() <= 0 || before.Nanosecond() != 0 || before.After(time.Now()) {
		return 0, errDurableStateConfig
	}
	s, err := openDurableStateBindingStore(DurableStateBindingsConfig{Path: path, MaxEntries: math.MaxInt}, false)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, s.close()) }()
	d := s.durable
	err = d.db.Update(func(tx *bolt.Tx) error {
		records := tx.Bucket(durableStateRecords)
		cursor := records.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			_, issued, err := d.decode(key, value)
			if err != nil {
				return err
			}
			if issued.Before(before) {
				if err := cursor.Delete(); err != nil {
					return err
				}
				removed++
			}
		}
		return records.SetSequence(records.Sequence() - uint64(removed))
	})
	if err != nil {
		return 0, errDurableStateIO
	}
	return removed, nil
}

func (d *durableStateBindings) initialize(created bool) (err error) {
	// Invalid bbolt pages may panic during validation. Never replace an existing
	// store with an empty one, or expose a partially validated index.
	defer func() {
		if recover() != nil {
			err = errDurableStateCorrupt
		}
	}()
	if created {
		if _, err := rand.Read(d.key[:]); err != nil {
			return errDurableStateIO
		}
		if err := d.db.Update(func(tx *bolt.Tx) error {
			meta, err := tx.CreateBucket(durableStateMetadata)
			if err != nil {
				return err
			}
			if err := meta.Put([]byte("format"), durableStateFormat); err != nil {
				return err
			}
			if err := meta.Put([]byte("key"), d.key[:]); err != nil {
				return err
			}
			_, err = tx.CreateBucket(durableStateRecords)
			return err
		}); err != nil {
			return errDurableStateIO
		}
	}
	err = d.db.View(func(tx *bolt.Tx) error {
		corrupt := false
		for checkErr := range tx.Check() {
			corrupt = corrupt || checkErr != nil
		}
		if corrupt {
			return errDurableStateCorrupt
		}
		meta, records := tx.Bucket(durableStateMetadata), tx.Bucket(durableStateRecords)
		if meta == nil || records == nil || !bytes.Equal(meta.Get([]byte("format")), durableStateFormat) || len(meta.Get([]byte("key"))) != 32 {
			return errDurableStateCorrupt
		}
		copy(d.key[:], meta.Get([]byte("key")))
		if records.Stats().KeyN > d.maxEntries {
			return errDurableStateCapacity
		}
		if uint64(records.Stats().KeyN) != records.Sequence() {
			return errDurableStateCorrupt
		}
		return records.ForEach(func(k, v []byte) error {
			_, _, err := d.decode(k, v)
			return err
		})
	})
	if err != nil && !errors.Is(err, errDurableStateCapacity) {
		return errDurableStateCorrupt
	}
	return err
}

func (d *durableStateBindings) digest(domain string, parts ...string) [32]byte {
	mac := hmac.New(sha256.New, d.key[:])
	for _, part := range append([]string{"vekil-state-v1", domain}, parts...) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = mac.Write(length[:])
		_, _ = io.WriteString(mac, part)
	}
	var result [32]byte
	copy(result[:], mac.Sum(nil))
	return result
}

func durableStateTypeByte(t stateBindingType) byte {
	switch t {
	case stateBindingTypeResponseID:
		return 1
	case stateBindingTypeEncryptedContent:
		return 2
	case stateBindingTypeTurnState:
		return 3
	case stateBindingTypeConversationID:
		return 4
	default:
		return 0
	}
}

func (d *durableStateBindings) tokenKeys(tokens []stateBindingToken) ([][]byte, error) {
	keys := make([][]byte, 0, len(tokens))
	seen := make(map[[33]byte]struct{}, len(tokens))
	for _, token := range tokens {
		kind := durableStateTypeByte(token.stateType)
		if kind == 0 || token.value == "" {
			return nil, errDurableStateCorrupt
		}
		digest := d.digest("token", string(token.stateType), token.value)
		var key [33]byte
		key[0] = kind
		copy(key[1:], digest[:])
		if _, ok := seen[key]; !ok {
			keys = append(keys, append([]byte(nil), key[:]...))
			seen[key] = struct{}{}
		}
	}
	return keys, nil
}

func (d *durableStateBindings) encodeOwner(o stateBindingOwner) stateBindingOwner {
	if o.routeID != "" {
		o.routeKey = d.digest("route", o.routeID)
		o.routeID = ""
	}
	if o.targetID != "" {
		o.targetKey = d.digest("target", o.targetID)
		o.targetID = ""
	}
	return o
}

func (d *durableStateBindings) encode(key []byte, owner stateBindingOwner, outcome stateBindingLookupOutcome, at time.Time) []byte {
	owner = d.encodeOwner(owner)
	value := make([]byte, durableStateRecordSize)
	value[0] = byte(outcome)
	copy(value[1:33], owner.routeKey[:])
	copy(value[33:65], owner.targetKey[:])
	copy(value[65:97], owner.identity[:])
	binary.BigEndian.PutUint64(value[97:105], uint64(at.Unix()))
	mac := d.digest("record", string(key), string(value[:105]))
	copy(value[105:], mac[:])
	return value
}

func (d *durableStateBindings) decode(key, value []byte) (stateBindingLookupResult, time.Time, error) {
	if len(key) != 33 || key[0] < 1 || key[0] > 4 || len(value) != durableStateRecordSize {
		return stateBindingLookupResult{}, time.Time{}, errDurableStateCorrupt
	}
	mac := d.digest("record", string(key), string(value[:105]))
	if !hmac.Equal(mac[:], value[105:]) {
		return stateBindingLookupResult{}, time.Time{}, errDurableStateCorrupt
	}
	result := stateBindingLookupResult{outcome: stateBindingLookupOutcome(value[0])}
	copy(result.owner.routeKey[:], value[1:33])
	copy(result.owner.targetKey[:], value[33:65])
	copy(result.owner.identity[:], value[65:97])
	if result.outcome != stateBindingLookupKnown && result.outcome != stateBindingLookupConflict {
		return stateBindingLookupResult{}, time.Time{}, errDurableStateCorrupt
	}
	if result.outcome == stateBindingLookupKnown && (result.owner.routeKey == [32]byte{} || result.owner.targetKey == [32]byte{} || result.owner.identity == [32]byte{}) {
		return stateBindingLookupResult{}, time.Time{}, errDurableStateCorrupt
	}
	return result, time.Unix(int64(binary.BigEndian.Uint64(value[97:105])), 0), nil
}

func (d *durableStateBindings) resolve(tokens []stateBindingToken) stateBindingLookupResult {
	return d.resolveWithRoute(tokens, "", "")
}

func (d *durableStateBindings) resolveWithRoute(tokens []stateBindingToken, routeID, pinnedTargetID string) stateBindingLookupResult {
	keys, err := d.tokenKeys(tokens)
	if err != nil {
		return stateBindingLookupResult{outcome: stateBindingLookupConflict}
	}
	var routeKey, targetKey [32]byte
	if routeID != "" {
		routeKey = d.digest("route", routeID)
	}
	if pinnedTargetID != "" {
		targetKey = d.digest("target", pinnedTargetID)
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed != nil {
		return stateBindingLookupResult{err: d.failed}
	}
	if d.db == nil {
		return stateBindingLookupResult{err: errDurableStateClosed}
	}
	result := stateBindingLookupResult{outcome: stateBindingLookupUnknown}
	err = d.db.View(func(tx *bolt.Tx) error {
		known, unknown := false, false
		for _, key := range keys {
			value := tx.Bucket(durableStateRecords).Get(key)
			if value == nil {
				unknown = true
				continue
			}
			next, _, err := d.decode(key, value)
			if err != nil {
				return err
			}
			// Check persisted keyed owners inside this same snapshot, before
			// missing proof can erase a proven route/target disagreement.
			if next.outcome == stateBindingLookupConflict ||
				(routeID != "" && next.owner.routeKey != routeKey) ||
				(pinnedTargetID != "" && next.owner.targetKey != targetKey) ||
				(known && next.owner != result.owner) {
				result = stateBindingLookupResult{outcome: stateBindingLookupConflict}
				return nil
			}
			result, known = next, true
		}
		if unknown {
			result = stateBindingLookupResult{outcome: stateBindingLookupUnknown}
		}
		return nil
	})
	if err != nil {
		d.failed = errDurableStateIO
		return stateBindingLookupResult{err: d.failed}
	}
	return result
}

func (d *durableStateBindings) bind(tokens []stateBindingToken, owner stateBindingOwner, claim bool) stateBindingLookupResult {
	if !owner.valid() || owner.identity == [32]byte{} || len(tokens) == 0 {
		return stateBindingLookupResult{err: errDurableStateIdentity}
	}
	keys, err := d.tokenKeys(tokens)
	if err != nil {
		return stateBindingLookupResult{outcome: stateBindingLookupConflict}
	}
	owner = d.encodeOwner(owner)
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed != nil {
		return stateBindingLookupResult{err: d.failed}
	}
	if d.db == nil {
		return stateBindingLookupResult{err: errDurableStateClosed}
	}
	result := stateBindingLookupResult{outcome: stateBindingLookupKnown, owner: owner}
	// Replayed history commonly repeats already committed proof on every event.
	// With the process lock and d.mu held, a read-only match needs no transaction
	// write or fsync; only first issuance/collision can change durable authority.
	unchanged := true
	err = d.db.View(func(tx *bolt.Tx) error {
		for _, key := range keys {
			value := tx.Bucket(durableStateRecords).Get(key)
			if value == nil {
				unchanged = false
				return nil
			}
			prior, _, err := d.decode(key, value)
			if err != nil {
				return err
			}
			if claim {
				result = prior
				return nil
			}
			if prior.outcome != stateBindingLookupKnown || prior.owner != owner {
				unchanged = false
				return nil
			}
		}
		return nil
	})
	if err != nil {
		d.failed = errDurableStateIO
		return stateBindingLookupResult{err: d.failed}
	}
	if unchanged {
		return result
	}
	err = d.db.Update(func(tx *bolt.Tx) error {
		records := tx.Bucket(durableStateRecords)
		newKeys := 0
		conflicts := make([][]byte, 0)
		for _, key := range keys {
			value := records.Get(key)
			if value == nil {
				newKeys++
				continue
			}
			prior, _, err := d.decode(key, value)
			if err != nil {
				return err
			}
			// Conversation claiming returns the prior exact owner, never a
			// tombstone, when another request won the atomic bootstrap race.
			if claim {
				result = prior
				return nil
			}
			if prior.outcome == stateBindingLookupConflict || prior.owner != owner {
				conflicts = append(conflicts, key)
			}
		}
		if len(conflicts) > 0 {
			for _, key := range conflicts {
				if err := records.Put(key, d.encode(key, stateBindingOwner{}, stateBindingLookupConflict, d.now())); err != nil {
					return err
				}
			}
			result = stateBindingLookupResult{outcome: stateBindingLookupConflict}
		} else {
			if uint64(newKeys) > uint64(d.maxEntries)-records.Sequence() {
				return errDurableStateCapacity
			}
			for _, key := range keys {
				// No TTL refresh is needed; repeated observation preserves the
				// original issuance time for explicit offline pruning.
				if records.Get(key) == nil {
					if err := records.Put(key, d.encode(key, owner, stateBindingLookupKnown, d.now())); err != nil {
						return err
					}
				}
			}
			if err := records.SetSequence(records.Sequence() + uint64(newKeys)); err != nil {
				return err
			}
		}
		if d.beforeCommit != nil {
			return d.beforeCommit()
		}
		return nil
	})
	if err == nil && d.afterCommit != nil {
		err = d.afterCommit()
	}
	if errors.Is(err, errDurableStateCapacity) {
		return stateBindingLookupResult{err: errDurableStateCapacity}
	}
	if err != nil {
		d.failed = errDurableStateIO
		return stateBindingLookupResult{err: d.failed}
	}
	return result
}

func (d *durableStateBindings) stats() stateBindingStoreStats {
	d.mu.Lock()
	defer d.mu.Unlock()
	stats := stateBindingStoreStats{}
	if d.db != nil {
		_ = d.db.View(func(tx *bolt.Tx) error {
			return tx.Bucket(durableStateRecords).ForEach(func(_, value []byte) error {
				stats.entries++
				if len(value) > 0 && value[0] == byte(stateBindingLookupConflict) {
					stats.tombstones++
				}
				return nil
			})
		})
	}
	return stats
}

func (s *stateBindingStore) close() error {
	if s == nil || s.durable == nil {
		return nil
	}
	d := s.durable
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.db == nil {
		return nil
	}
	err := d.db.Close()
	d.db = nil
	if err != nil {
		return errDurableStateIO
	}
	return nil
}

func (s *stateBindingStore) ownerMatchesRoute(owner stateBindingOwner, routeID string) bool {
	if s.durable == nil {
		return owner.routeID == routeID
	}
	return owner.routeKey == s.durable.digest("route", routeID)
}

func (s *stateBindingStore) ownerTarget(owner stateBindingOwner, route *modelRoute) string {
	if s.durable == nil {
		return owner.targetID
	}
	for _, target := range route.targets {
		if owner.targetKey == s.durable.digest("target", target.id) {
			return target.id
		}
	}
	return ""
}
