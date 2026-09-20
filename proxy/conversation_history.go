package proxy

import (
	"bytes"
	"crypto/hmac"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"sync"
	"time"

	bolt "go.etcd.io/bbolt"
)

var (
	errConversationHistoryStorage   = errors.New("conversation history storage failed; recovery is unavailable; preserve the database and restart after repairing storage")
	errConversationHistoryCapacity  = errors.New("conversation history capacity reached; increase conversation_migration limits or explicitly prune history offline")
	errConversationHistoryMissing   = errors.New("earlier conversation messages are unavailable or were explicitly deleted; resume with complete visible history and X-Vekil-History-Complete: true")
	errConversationHistoryPartial   = errors.New("input does not contain the complete saved conversation; send the full history or previous_response_id with only new input")
	errConversationHistoryBusy      = errors.New("another turn of this conversation is active; wait for it to complete before continuing or branching")
	errConversationHistoryUncertain = errors.New("an earlier attempt may have executed without a saved completion; automatic recovery is blocked to avoid duplicate work")
)

var (
	conversationSnapshotsBucket = []byte("conversation-snapshots-v1")
	conversationIndexBucket     = []byte("conversation-index-v1")
	conversationPendingBucket   = []byte("conversation-pending-v1")
)

const (
	maxConversationHistoryItems = 16384
	conversationPendingBytes    = 32 + 32 + 40 // key, MAC, timestamp and attempt digest
)

// Each immutable snapshot contains all visible history for one completed
// response. Branches never share a mutable head or redirect an old provider ID.
// Provider-private reasoning is deliberately absent.
type conversationSnapshot struct {
	ResponseID      string            `json:"response_id"`
	RouteID         string            `json:"route_id"`
	TargetID        string            `json:"target_id"`
	Identity        [32]byte          `json:"identity"`
	Root            string            `json:"root"`
	Scope           string            `json:"scope,omitempty"`
	Created         int64             `json:"created"`
	Input           []json.RawMessage `json:"input"`
	Instructions    json.RawMessage   `json:"instructions,omitempty"`
	Tools           json.RawMessage   `json:"tools,omitempty"`
	AdditionalTools []json.RawMessage `json:"additional_tools,omitempty"`
	Migrated        bool              `json:"migrated,omitempty"`
	Stored          bool              `json:"stored"`
	Indexes         [][]byte          `json:"indexes"`
}

type conversationHistoryStore struct {
	d      *durableStateBindings
	config ConversationMigrationConfig
	mu     sync.Mutex
	active map[string]bool
}

func newConversationHistoryStore(d *durableStateBindings, config ConversationMigrationConfig) (*conversationHistoryStore, error) {
	if d == nil {
		return nil, configPathError("conversation_migration", "requires durable state_bindings, including process overrides")
	}
	s := &conversationHistoryStore{d: d, config: config.withDefaults(), active: make(map[string]bool)}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed != nil || d.db == nil {
		return nil, errConversationHistoryStorage
	}
	created := false
	err := d.db.View(func(tx *bolt.Tx) error {
		present := 0
		for _, name := range [][]byte{conversationSnapshotsBucket, conversationIndexBucket, conversationPendingBucket} {
			if tx.Bucket(name) != nil {
				present++
			}
		}
		if present == 0 {
			created = true
			return nil
		}
		if present != 3 {
			return errConversationHistoryStorage
		}
		return s.validate(tx)
	})
	if err == nil && created {
		err = d.db.Update(func(tx *bolt.Tx) error {
			for _, name := range [][]byte{conversationSnapshotsBucket, conversationIndexBucket, conversationPendingBucket} {
				if _, err := tx.CreateBucket(name); err != nil {
					return err
				}
			}
			return nil
		})
	}
	if err != nil {
		if errors.Is(err, errConversationHistoryCapacity) {
			return nil, err
		}
		d.failed = errDurableStateIO
		return nil, errConversationHistoryStorage
	}
	return s, nil
}

func (s *conversationHistoryStore) validate(tx *bolt.Tx) error {
	snapshots := tx.Bucket(conversationSnapshotsBucket)
	index := tx.Bucket(conversationIndexBucket)
	pending := tx.Bucket(conversationPendingBucket)
	var total uint64
	err := snapshots.ForEach(func(key, value []byte) error {
		snapshot, err := s.decode(key, value)
		if err != nil {
			return err
		}
		if len(value) > s.config.MaxHistoryBytes {
			return errConversationHistoryCapacity
		}
		for _, anchor := range snapshot.Indexes {
			ref := index.Get(anchor)
			if !bytes.Equal(ref, key) && !bytes.Equal(ref, []byte{0}) {
				return errConversationHistoryStorage
			}
		}
		cost := conversationSnapshotCost(snapshot, value)
		if math.MaxUint64-total < cost {
			return errConversationHistoryStorage
		}
		total += cost
		return nil
	})
	if err != nil {
		return err
	}
	if total != snapshots.Sequence() {
		return errConversationHistoryStorage
	}
	if err := index.ForEach(func(key, value []byte) error {
		if len(key) != 32 || (len(value) != 32 && !bytes.Equal(value, []byte{0})) {
			return errConversationHistoryStorage
		}
		if len(value) == 32 {
			snapshot, err := s.decode(value, snapshots.Get(value))
			if err != nil || !snapshot.hasIndex(key) {
				return errConversationHistoryStorage
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := pending.ForEach(func(key, value []byte) error {
		_, err := s.pendingCreated(key, value)
		return err
	}); err != nil {
		return err
	}
	if snapshots.Stats().KeyN+pending.Stats().KeyN > s.config.MaxSnapshots ||
		total+uint64(pending.Stats().KeyN*conversationPendingBytes) > uint64(s.config.MaxTotalBytes) {
		return errConversationHistoryCapacity
	}
	return nil
}

func (s *conversationHistoryStore) responseKey(routeID, responseID string) []byte {
	key := s.d.digest("conversation-response-v1", routeID, responseID)
	return key[:]
}

func (s *conversationHistoryStore) indexKey(routeID, kind, value string) []byte {
	key := s.d.digest("conversation-index-v1", routeID, kind, value)
	return key[:]
}

func (s *conversationHistoryStore) rootKey(root string) []byte {
	key := s.d.digest("conversation-root-v1", root)
	return key[:]
}

func (snapshot *conversationSnapshot) hasIndex(key []byte) bool {
	for _, index := range snapshot.Indexes {
		if bytes.Equal(index, key) {
			return true
		}
	}
	return false
}

func (s *conversationHistoryStore) pendingCreated(key, value []byte) (int64, error) {
	if len(key) != 32 || len(value) != 72 {
		return 0, errConversationHistoryStorage
	}
	mac := s.d.digest("conversation-pending-record-v1", string(key), string(value[32:]))
	created := int64(binary.BigEndian.Uint64(value[32:40]))
	if !hmac.Equal(mac[:], value[:32]) || created <= 0 {
		return 0, errConversationHistoryStorage
	}
	return created, nil
}

func (s *conversationHistoryStore) encode(key []byte, snapshot *conversationSnapshot) ([]byte, error) {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return nil, errConversationHistoryStorage
	}
	mac := s.d.digest("conversation-record-v1", string(key), string(raw))
	return append(mac[:], raw...), nil
}

func (s *conversationHistoryStore) decode(key, value []byte) (*conversationSnapshot, error) {
	if len(key) != 32 || len(value) <= 32 {
		return nil, errConversationHistoryStorage
	}
	mac := s.d.digest("conversation-record-v1", string(key), string(value[32:]))
	if !hmac.Equal(mac[:], value[:32]) {
		return nil, errConversationHistoryStorage
	}
	var snapshot conversationSnapshot
	if err := json.Unmarshal(value[32:], &snapshot); err != nil || snapshot.Root == "" ||
		snapshot.ResponseID == "" || snapshot.RouteID == "" || snapshot.TargetID == "" ||
		snapshot.Identity == [32]byte{} || snapshot.Created <= 0 ||
		!bytes.Equal(s.responseKey(snapshot.RouteID, snapshot.ResponseID), key) {
		return nil, errConversationHistoryStorage
	}
	for _, index := range snapshot.Indexes {
		if len(index) != 32 {
			return nil, errConversationHistoryStorage
		}
	}
	return &snapshot, nil
}

func conversationSnapshotCost(snapshot *conversationSnapshot, encoded []byte) uint64 {
	// Account conservatively for every index, including shared/colliding keys.
	// These are logical bytes; bbolt pages and transaction overhead are extra.
	return uint64(len(encoded)+32) + uint64(len(snapshot.Indexes))*64
}

func (s *conversationHistoryStore) view(fn func(*bolt.Tx) error) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed != nil || d.db == nil {
		return errConversationHistoryStorage
	}
	if err := d.db.View(fn); err != nil {
		if errors.Is(err, errConversationHistoryMissing) || errors.Is(err, errConversationHistoryUncertain) {
			return err
		}
		d.failed = errDurableStateIO
		return errConversationHistoryStorage
	}
	return nil
}

func (s *conversationHistoryStore) lookupResponse(routeID, responseID string) (*conversationSnapshot, error) {
	var snapshot *conversationSnapshot
	err := s.view(func(tx *bolt.Tx) error {
		key := s.responseKey(routeID, responseID)
		value := tx.Bucket(conversationSnapshotsBucket).Get(key)
		if value == nil {
			return errConversationHistoryMissing
		}
		var err error
		snapshot, err = s.decode(key, value)
		return err
	})
	return snapshot, err
}

func (s *conversationHistoryStore) lookupIndexes(keys [][]byte) (*conversationSnapshot, error) {
	var snapshot *conversationSnapshot
	err := s.view(func(tx *bolt.Tx) error {
		index, snapshots := tx.Bucket(conversationIndexBucket), tx.Bucket(conversationSnapshotsBucket)
		for _, key := range keys {
			ref := index.Get(key)
			if len(ref) != 32 {
				continue
			}
			candidate, err := s.decode(ref, snapshots.Get(ref))
			if err != nil {
				return err
			}
			if !candidate.hasIndex(key) {
				return errConversationHistoryStorage
			}
			// Input order, rather than issuance time, chooses the branch.
			snapshot = candidate
		}
		return nil
	})
	return snapshot, err
}

func (s *conversationHistoryStore) acquire(root string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active[root] {
		return errConversationHistoryBusy
	}
	if len(s.active) >= s.config.MaxSnapshots {
		return errConversationHistoryCapacity
	}
	if err := s.view(func(tx *bolt.Tx) error {
		if tx.Bucket(conversationPendingBucket).Get(s.rootKey(root)) != nil {
			return errConversationHistoryUncertain
		}
		return nil
	}); err != nil {
		return err
	}
	s.active[root] = true
	return nil
}

func (s *conversationHistoryStore) release(root string) {
	s.mu.Lock()
	delete(s.active, root)
	s.mu.Unlock()
}

func (s *conversationHistoryStore) update(fn func(*bolt.Tx) error) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed != nil || d.db == nil {
		return errConversationHistoryStorage
	}
	err := d.db.Update(func(tx *bolt.Tx) error {
		if err := fn(tx); err != nil {
			return err
		}
		if d.beforeCommit != nil {
			return d.beforeCommit()
		}
		return nil
	})
	if err == nil && d.afterCommit != nil {
		err = d.afterCommit()
	}
	if errors.Is(err, errConversationHistoryCapacity) || errors.Is(err, errConversationHistoryUncertain) {
		return err
	}
	if err != nil {
		d.failed = errDurableStateIO
		return errConversationHistoryStorage
	}
	return nil
}

func (s *conversationHistoryStore) beginAttempt(root, operationID string) error {
	return s.update(func(tx *bolt.Tx) error {
		pending, snapshots := tx.Bucket(conversationPendingBucket), tx.Bucket(conversationSnapshotsBucket)
		key := s.rootKey(root)
		if pending.Get(key) != nil {
			return errConversationHistoryUncertain
		}
		if snapshots.Stats().KeyN+pending.Stats().KeyN >= s.config.MaxSnapshots ||
			snapshots.Sequence()+uint64((pending.Stats().KeyN+1)*conversationPendingBytes) > uint64(s.config.MaxTotalBytes) {
			return errConversationHistoryCapacity
		}
		value := make([]byte, 40)
		binary.BigEndian.PutUint64(value[:8], uint64(s.d.now().Unix()))
		digest := s.d.digest("conversation-attempt-v1", operationID)
		copy(value[8:], digest[:])
		mac := s.d.digest("conversation-pending-record-v1", string(key), string(value))
		return pending.Put(key, append(mac[:], value...))
	})
}

func (s *conversationHistoryStore) clearAttempt(root string) error {
	return s.update(func(tx *bolt.Tx) error {
		return tx.Bucket(conversationPendingBucket).Delete(s.rootKey(root))
	})
}

func (s *conversationHistoryStore) save(snapshot *conversationSnapshot) error {
	key := s.responseKey(snapshot.RouteID, snapshot.ResponseID)
	value, err := s.encode(key, snapshot)
	if err != nil {
		return err
	}
	cost := conversationSnapshotCost(snapshot, value)
	if len(value) > s.config.MaxHistoryBytes {
		return errConversationHistoryCapacity
	}
	return s.update(func(tx *bolt.Tx) error {
		snapshots, index, pending := tx.Bucket(conversationSnapshotsBucket), tx.Bucket(conversationIndexBucket), tx.Bucket(conversationPendingBucket)
		if snapshots.Get(key) != nil {
			// Reusing an ID for a different turn must never overwrite history.
			return errConversationHistoryStorage
		}
		pendingCount := pending.Stats().KeyN
		if pending.Get(s.rootKey(snapshot.Root)) != nil {
			pendingCount--
		}
		if snapshots.Stats().KeyN+pendingCount+1 > s.config.MaxSnapshots ||
			cost > uint64(s.config.MaxTotalBytes) ||
			snapshots.Sequence()+cost+uint64(pendingCount*conversationPendingBytes) > uint64(s.config.MaxTotalBytes) {
			return errConversationHistoryCapacity
		}
		if err := snapshots.Put(key, value); err != nil {
			return err
		}
		for _, anchor := range snapshot.Indexes {
			ref := key
			if prior := index.Get(anchor); prior != nil && !bytes.Equal(prior, key) {
				ref = []byte{0}
			}
			if err := index.Put(anchor, ref); err != nil {
				return err
			}
		}
		if err := snapshots.SetSequence(snapshots.Sequence() + cost); err != nil {
			return err
		}
		return pending.Delete(s.rootKey(snapshot.Root))
	})
}

// PruneConversationHistory deletes selected history and unresolved attempts,
// without changing ownership proof. Serving must be stopped. Freed database
// pages remain reusable and are not a secure erase of conversation contents.
func PruneConversationHistory(path string, before time.Time) (removed int, err error) {
	if before.IsZero() || before.Unix() <= 0 || before.Nanosecond() != 0 || before.After(time.Now()) {
		return 0, errDurableStateConfig
	}
	bindings, err := openDurableStateBindingStore(DurableStateBindingsConfig{Path: path, MaxEntries: math.MaxInt}, false)
	if err != nil {
		return 0, err
	}
	defer func() { err = errors.Join(err, bindings.close()) }()
	s := &conversationHistoryStore{d: bindings.durable, config: ConversationMigrationConfig{
		MaxHistoryBytes: math.MaxInt, MaxTotalBytes: math.MaxInt64, MaxSnapshots: math.MaxInt,
	}}
	err = s.update(func(tx *bolt.Tx) error {
		snapshots := tx.Bucket(conversationSnapshotsBucket)
		if snapshots == nil {
			return nil
		}
		index, pending := tx.Bucket(conversationIndexBucket), tx.Bucket(conversationPendingBucket)
		if index == nil || pending == nil {
			return errConversationHistoryStorage
		}
		if err := s.validate(tx); err != nil {
			return err
		}
		cursor := snapshots.Cursor()
		var total uint64
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			snapshot, err := s.decode(key, value)
			if err != nil {
				return err
			}
			if snapshot.Created < before.Unix() {
				if err := cursor.Delete(); err != nil {
					return err
				}
				removed++
			} else {
				total += conversationSnapshotCost(snapshot, value)
			}
		}
		// An unresolved attempt and its source history must be retired together.
		// Otherwise deletion could silently authorize a duplicate attempt.
		retiredRoots := make(map[string]bool)
		cursor = pending.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			created, err := s.pendingCreated(key, value)
			if err != nil {
				return err
			}
			if created < before.Unix() {
				retiredRoots[string(key)] = true
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		cursor = snapshots.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			snapshot, err := s.decode(key, value)
			if err != nil {
				return err
			}
			if retiredRoots[string(s.rootKey(snapshot.Root))] {
				total -= conversationSnapshotCost(snapshot, value)
				if err := cursor.Delete(); err != nil {
					return err
				}
				removed++
			}
		}
		if err := tx.DeleteBucket(conversationIndexBucket); err != nil {
			return err
		}
		index, err := tx.CreateBucket(conversationIndexBucket)
		if err != nil {
			return err
		}
		if err := snapshots.ForEach(func(key, value []byte) error {
			snapshot, err := s.decode(key, value)
			if err != nil {
				return err
			}
			for _, anchor := range snapshot.Indexes {
				ref := key
				if prior := index.Get(anchor); prior != nil && !bytes.Equal(prior, key) {
					ref = []byte{0}
				}
				if err := index.Put(anchor, ref); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
		return snapshots.SetSequence(total)
	})
	if err != nil {
		return 0, err
	}
	return removed, nil
}

func conversationRequestError(err error) error {
	status, code := http.StatusBadRequest, "conversation_history_incomplete"
	switch {
	case errors.Is(err, errConversationHistoryStorage):
		status, code = http.StatusServiceUnavailable, "conversation_history_storage_unavailable"
	case errors.Is(err, errConversationHistoryCapacity):
		status, code = http.StatusServiceUnavailable, "conversation_history_capacity_exceeded"
	case errors.Is(err, errConversationHistoryBusy):
		status, code = http.StatusConflict, "conversation_turn_in_progress"
	case errors.Is(err, errConversationHistoryUncertain):
		status, code = http.StatusConflict, "conversation_execution_uncertain"
	case errors.Is(err, errConversationHistoryMissing):
		code = "conversation_history_unavailable"
	case errors.Is(err, errConversationHostedState):
		code = "conversation_state_unsupported"
	case errors.Is(err, errConversationCompaction):
		code = "conversation_compaction_unrecoverable"
	case errors.Is(err, errConversationPendingTool):
		code = "conversation_tools_pending"
	case errors.Is(err, errConversationIncomplete):
		status, code = http.StatusConflict, "conversation_execution_uncertain"
	}
	return &providerRequestError{statusCode: status, code: code, err: err}
}
