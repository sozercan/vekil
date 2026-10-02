package proxy

import (
	"bytes"
	"crypto/hmac"
	"encoding/binary"
	"encoding/hex"
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
	errConversationStreamEnded      = errors.New("the response stream ended before the response completed")
)

var (
	conversationSnapshotsBucket = []byte("conversation-snapshots-v1")
	conversationIndexBucket     = []byte("conversation-index-v1")
	conversationPendingBucket   = []byte("conversation-pending-v1")
	conversationBlobsBucket     = []byte("conversation-blobs-v1")
)

const (
	maxConversationHistoryItems = 16384
	conversationPendingBytes    = 32 + 32 + 40 // key, MAC, timestamp and attempt digest

	// Records written before blobs carry their content inline. A distinct
	// integrity domain keeps those older versions from reading a newer record
	// as a snapshot with no content.
	conversationInlineRecordDomain = "conversation-record-v1"
	conversationRecordDomain       = "conversation-record-v2"
	conversationBlobDomain         = "conversation-blob-v1"
	conversationBlobKeyBytes       = 32
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

// conversationSnapshotRecord is a snapshot's stored form. Successive snapshots
// of a conversation repeat nearly all of its items and its whole tool catalog,
// so each distinct value is stored once as a blob. These fields shadow the
// snapshot's content fields in JSON and hold the keys of its blobs, in order.
type conversationSnapshotRecord struct {
	conversationSnapshot
	Input           []byte `json:"input"`
	Instructions    []byte `json:"instructions,omitempty"`
	Tools           []byte `json:"tools,omitempty"`
	AdditionalTools []byte `json:"additional_tools,omitempty"`

	// inline marks a record written before blobs, which holds its own content.
	inline bool
}

// encodedConversationSnapshot is a snapshot ready to store.
type encodedConversationSnapshot struct {
	value []byte
	// blobs holds each distinct value the record references, by blob key.
	blobs map[string][]byte
	// historyBytes counts the record and every value at each reference: the
	// size of the complete history the snapshot stands for.
	historyBytes int
}

type conversationHistoryStore struct {
	d      *durableStateBindings
	config ConversationMigrationConfig
	mu     sync.Mutex
	// active maps the root of each conversation with a turn in progress to
	// the client thread that sent it.
	active map[string]string
	// streaming maps the key of a response an active turn is streaming, and
	// has not saved yet, to that turn's root.
	streaming map[string]string
	// The single writer rebuilds counts while validating records at startup.
	// d.mu guards them, and updates publish only after the transaction commits.
	counts conversationHistoryCounts
}

type conversationHistoryCounts struct {
	snapshots int
	pending   int
}

func newConversationHistoryStore(d *durableStateBindings, config ConversationMigrationConfig) (*conversationHistoryStore, error) {
	if d == nil {
		return nil, configPathError("conversation_migration", "requires durable state_bindings, including process overrides")
	}
	s := &conversationHistoryStore{d: d, config: config.withDefaults(), active: make(map[string]string), streaming: make(map[string]string)}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed != nil || d.db == nil {
		return nil, errConversationHistoryStorage
	}
	buckets := [][]byte{conversationSnapshotsBucket, conversationIndexBucket, conversationPendingBucket}
	create := false
	err := d.db.View(func(tx *bolt.Tx) error {
		present := 0
		for _, name := range buckets {
			if tx.Bucket(name) != nil {
				present++
			}
		}
		// A store from before blobs gains the bucket at startup.
		create = present == 0 || tx.Bucket(conversationBlobsBucket) == nil
		if present == 0 {
			if tx.Bucket(conversationBlobsBucket) != nil {
				return errConversationHistoryStorage
			}
			return nil
		}
		if present != len(buckets) {
			return errConversationHistoryStorage
		}
		return s.validate(tx, &s.counts)
	})
	if err == nil && create {
		err = d.db.Update(func(tx *bolt.Tx) error {
			for _, name := range append(buckets, conversationBlobsBucket) {
				if _, err := tx.CreateBucketIfNotExists(name); err != nil {
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

func (s *conversationHistoryStore) validate(tx *bolt.Tx, counts *conversationHistoryCounts) error {
	snapshots := tx.Bucket(conversationSnapshotsBucket)
	index := tx.Bucket(conversationIndexBucket)
	pending := tx.Bucket(conversationPendingBucket)
	blobs := tx.Bucket(conversationBlobsBucket)
	*counts = conversationHistoryCounts{}
	var total uint64
	add := func(cost uint64) error {
		if math.MaxUint64-total < cost {
			return errConversationHistoryStorage
		}
		total += cost
		return nil
	}
	err := snapshots.ForEach(func(key, value []byte) error {
		record, err := s.decodeRecord(key, value)
		if err != nil {
			return err
		}
		historyBytes := len(value)
		if err := record.eachBlob(func(blobKey []byte) error {
			content := blobGet(blobs, blobKey)
			if content == nil {
				return errConversationHistoryStorage
			}
			historyBytes += len(content)
			return nil
		}); err != nil {
			return err
		}
		if historyBytes > s.config.MaxHistoryBytes {
			return errConversationHistoryCapacity
		}
		for _, anchor := range record.Indexes {
			ref := index.Get(anchor)
			if !bytes.Equal(ref, key) && !bytes.Equal(ref, []byte{0}) {
				return errConversationHistoryStorage
			}
		}
		counts.snapshots++
		return add(conversationSnapshotCost(&record.conversationSnapshot, value))
	})
	if err != nil {
		return err
	}
	if blobs != nil {
		if err := blobs.ForEach(func(key, content []byte) error {
			if !s.blobValid(key, content) {
				return errConversationHistoryStorage
			}
			return add(conversationBlobCost(content))
		}); err != nil {
			return err
		}
	}
	if total != snapshots.Sequence() {
		return errConversationHistoryStorage
	}
	if err := index.ForEach(func(key, value []byte) error {
		if len(key) != 32 || (len(value) != 32 && !bytes.Equal(value, []byte{0})) {
			return errConversationHistoryStorage
		}
		if len(value) == 32 {
			record, err := s.decodeRecord(value, snapshots.Get(value))
			if err != nil || !record.hasIndex(key) {
				return errConversationHistoryStorage
			}
		}
		return nil
	}); err != nil {
		return err
	}
	if err := pending.ForEach(func(key, value []byte) error {
		_, err := s.pendingCreated(key, value)
		if err == nil {
			counts.pending++
		}
		return err
	}); err != nil {
		return err
	}
	if counts.snapshots+counts.pending > s.config.MaxSnapshots ||
		total+uint64(counts.pending*conversationPendingBytes) > uint64(s.config.MaxTotalBytes) {
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

// branchRoot names the conversation a client thread branches into from root.
func (s *conversationHistoryStore) branchRoot(root, thread string) string {
	digest := s.d.digest("conversation-branch-v1", root, thread)
	return hex.EncodeToString(digest[:])
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

func (s *conversationHistoryStore) encode(key []byte, snapshot *conversationSnapshot) (*encodedConversationSnapshot, error) {
	encoded := &encodedConversationSnapshot{blobs: make(map[string][]byte)}
	blobKeys := func(values ...json.RawMessage) ([]byte, error) {
		var keys []byte
		for _, value := range values {
			// Marshal validates and compacts each value, as the inline form did.
			content, err := json.Marshal(value)
			if err != nil {
				return nil, errConversationHistoryStorage
			}
			blobKey := s.d.digest(conversationBlobDomain, string(content))
			encoded.blobs[string(blobKey[:])] = content
			encoded.historyBytes += len(content)
			keys = append(keys, blobKey[:]...)
		}
		return keys, nil
	}
	record := conversationSnapshotRecord{conversationSnapshot: *snapshot}
	var err error
	if record.Input, err = blobKeys(snapshot.Input...); err != nil {
		return nil, err
	}
	if record.AdditionalTools, err = blobKeys(snapshot.AdditionalTools...); err != nil {
		return nil, err
	}
	if len(snapshot.Instructions) > 0 {
		if record.Instructions, err = blobKeys(snapshot.Instructions); err != nil {
			return nil, err
		}
	}
	if len(snapshot.Tools) > 0 {
		if record.Tools, err = blobKeys(snapshot.Tools); err != nil {
			return nil, err
		}
	}
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, errConversationHistoryStorage
	}
	mac := s.d.digest(conversationRecordDomain, string(key), string(raw))
	encoded.value = append(mac[:], raw...)
	encoded.historyBytes += len(encoded.value)
	return encoded, nil
}

// decodeRecord authenticates a stored snapshot without reading its blobs.
func (s *conversationHistoryStore) decodeRecord(key, value []byte) (*conversationSnapshotRecord, error) {
	if len(key) != 32 || len(value) <= 32 {
		return nil, errConversationHistoryStorage
	}
	record := &conversationSnapshotRecord{}
	raw := string(value[32:])
	if mac := s.d.digest(conversationRecordDomain, string(key), raw); hmac.Equal(mac[:], value[:32]) {
		if json.Unmarshal(value[32:], record) != nil ||
			len(record.Input)%conversationBlobKeyBytes != 0 || len(record.AdditionalTools)%conversationBlobKeyBytes != 0 ||
			len(record.Instructions) != 0 && len(record.Instructions) != conversationBlobKeyBytes ||
			len(record.Tools) != 0 && len(record.Tools) != conversationBlobKeyBytes {
			return nil, errConversationHistoryStorage
		}
	} else if mac := s.d.digest(conversationInlineRecordDomain, string(key), raw); hmac.Equal(mac[:], value[:32]) {
		if json.Unmarshal(value[32:], &record.conversationSnapshot) != nil {
			return nil, errConversationHistoryStorage
		}
		record.inline = true
	} else {
		return nil, errConversationHistoryStorage
	}
	snapshot := &record.conversationSnapshot
	if snapshot.Root == "" || snapshot.ResponseID == "" || snapshot.RouteID == "" || snapshot.TargetID == "" ||
		snapshot.Identity == [32]byte{} || snapshot.Created <= 0 ||
		!bytes.Equal(s.responseKey(snapshot.RouteID, snapshot.ResponseID), key) {
		return nil, errConversationHistoryStorage
	}
	for _, index := range snapshot.Indexes {
		if len(index) != 32 {
			return nil, errConversationHistoryStorage
		}
	}
	return record, nil
}

// load returns a record's complete snapshot, reading every blob it references.
func (s *conversationHistoryStore) load(blobs *bolt.Bucket, record *conversationSnapshotRecord) (*conversationSnapshot, error) {
	snapshot := record.conversationSnapshot
	if record.inline {
		return &snapshot, nil
	}
	values := func(keys []byte) ([]json.RawMessage, error) {
		if len(keys) == 0 {
			return nil, nil
		}
		loaded := make([]json.RawMessage, 0, len(keys)/conversationBlobKeyBytes)
		for offset := 0; offset < len(keys); offset += conversationBlobKeyBytes {
			content := blobGet(blobs, keys[offset:offset+conversationBlobKeyBytes])
			if content == nil || !s.blobValid(keys[offset:offset+conversationBlobKeyBytes], content) {
				return nil, errConversationHistoryStorage
			}
			// bbolt values are valid only during the transaction.
			loaded = append(loaded, bytes.Clone(content))
		}
		return loaded, nil
	}
	// decodeRecord admits at most one key for instructions and tools.
	value := func(key []byte) (json.RawMessage, error) {
		loaded, err := values(key)
		if err != nil || len(loaded) == 0 {
			return nil, err
		}
		return loaded[0], nil
	}
	var err error
	if snapshot.Input, err = values(record.Input); err != nil {
		return nil, err
	}
	if snapshot.AdditionalTools, err = values(record.AdditionalTools); err != nil {
		return nil, err
	}
	if snapshot.Instructions, err = value(record.Instructions); err != nil {
		return nil, err
	}
	if snapshot.Tools, err = value(record.Tools); err != nil {
		return nil, err
	}
	return &snapshot, nil
}

func (s *conversationHistoryStore) decode(tx *bolt.Tx, key, value []byte) (*conversationSnapshot, error) {
	record, err := s.decodeRecord(key, value)
	if err != nil {
		return nil, err
	}
	return s.load(tx.Bucket(conversationBlobsBucket), record)
}

// put writes a snapshot's record and any blobs not stored yet, and returns the
// logical bytes they add. A failed capacity check after put rolls them back
// with the transaction.
func (s *conversationHistoryStore) put(tx *bolt.Tx, key []byte, snapshot *conversationSnapshot, encoded *encodedConversationSnapshot) (uint64, error) {
	blobs := tx.Bucket(conversationBlobsBucket)
	cost := conversationSnapshotCost(snapshot, encoded.value)
	for blobKey, content := range encoded.blobs {
		if blobs.Get([]byte(blobKey)) != nil {
			continue
		}
		if err := blobs.Put([]byte(blobKey), content); err != nil {
			return 0, err
		}
		cost += conversationBlobCost(content)
	}
	return cost, tx.Bucket(conversationSnapshotsBucket).Put(key, encoded.value)
}

// eachBlob calls fn with the key of every blob the record references, once
// per reference.
func (record *conversationSnapshotRecord) eachBlob(fn func([]byte) error) error {
	for _, keys := range [][]byte{record.Input, record.AdditionalTools, record.Instructions, record.Tools} {
		for offset := 0; offset < len(keys); offset += conversationBlobKeyBytes {
			if err := fn(keys[offset : offset+conversationBlobKeyBytes]); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *conversationHistoryStore) blobValid(key, content []byte) bool {
	sum := s.d.digest(conversationBlobDomain, string(content))
	return hmac.Equal(sum[:], key)
}

func blobGet(blobs *bolt.Bucket, key []byte) []byte {
	if blobs == nil {
		return nil
	}
	return blobs.Get(key)
}

func conversationSnapshotCost(snapshot *conversationSnapshot, encoded []byte) uint64 {
	// Account conservatively for every index, including shared/colliding keys.
	// These are logical bytes; bbolt pages and transaction overhead are extra.
	return uint64(len(encoded)+32) + uint64(len(snapshot.Indexes))*64
}

// conversationBlobCost counts a blob once, however many snapshots share it.
func conversationBlobCost(content []byte) uint64 {
	return uint64(len(content) + conversationBlobKeyBytes)
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
		snapshot, err = s.decode(tx, key, value)
		return err
	})
	return snapshot, err
}

func (s *conversationHistoryStore) lookupIndexes(keys [][]byte) (*conversationSnapshot, error) {
	var snapshot *conversationSnapshot
	err := s.view(func(tx *bolt.Tx) error {
		index, snapshots := tx.Bucket(conversationIndexBucket), tx.Bucket(conversationSnapshotsBucket)
		var chosen *conversationSnapshotRecord
		for _, key := range keys {
			ref := index.Get(key)
			if len(ref) != 32 {
				continue
			}
			candidate, err := s.decodeRecord(ref, snapshots.Get(ref))
			if err != nil {
				return err
			}
			if !candidate.hasIndex(key) {
				return errConversationHistoryStorage
			}
			// Input order, rather than issuance time, chooses the branch.
			chosen = candidate
		}
		if chosen == nil {
			return nil
		}
		var err error
		snapshot, err = s.load(tx.Bucket(conversationBlobsBucket), chosen)
		return err
	})
	return snapshot, err
}

// pendingReleasedTo reports whether a pending record admits a turn that
// continues from source. saveDelivered replaces the attempt digest with the key
// of the snapshot it saved, so only continuations of those delivered items pass.
func pendingReleasedTo(value, source []byte) bool {
	return len(source) == 32 && len(value) == 72 && bytes.Equal(value[40:], source)
}

func (s *conversationHistoryStore) acquire(root string) error {
	_, _, err := s.acquireFrom(root, "", nil, false)
	return err
}

// acquireFrom admits a turn sent by the named client thread, whose history
// continues from the snapshot keyed source, or from no snapshot when source is
// nil. A pending record admits only a continuation of the snapshot it was
// released to, unless trusted: the client is known to resend everything it
// received, so a turn whose history validated cannot hide delivered work.
// overrode reports that trust admitted the turn.
//
// One turn of a conversation runs at a time. While another client thread's
// turn holds root, a trusted client's turn branches into that thread's own
// root instead, such as a Codex side conversation or subagent forked from a
// thread that is still working. Its validated history cannot hide delivered
// work either, and its own snapshots carry its lineage from there. A retry
// from the thread before its branch saves history resolves to root again, and
// reaches the same branch, which is busy or holds that turn's pending record.
// admitted is the root the turn holds.
func (s *conversationHistoryStore) acquireFrom(root, thread string, source []byte, trusted bool) (admitted string, overrode bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if thread != "" {
		branch := s.branchRoot(root, thread)
		if _, busy := s.active[branch]; busy {
			// The thread's own turn that branched from root is still active.
			return "", false, errConversationHistoryBusy
		}
		if holder, busy := s.active[root]; busy && trusted && holder != "" && holder != thread {
			root = branch
		}
	}
	if _, busy := s.active[root]; busy {
		return "", false, errConversationHistoryBusy
	}
	if len(s.active) >= s.config.MaxSnapshots {
		return "", false, errConversationHistoryCapacity
	}
	if err := s.view(func(tx *bolt.Tx) error {
		key := s.rootKey(root)
		value := tx.Bucket(conversationPendingBucket).Get(key)
		if value == nil {
			return nil
		}
		// Neither the released pointer nor trust may read past a damaged record.
		if _, err := s.pendingCreated(key, value); err != nil {
			return err
		}
		if !pendingReleasedTo(value, source) {
			if !trusted {
				return errConversationHistoryUncertain
			}
			overrode = true
		}
		return nil
	}); err != nil {
		return "", false, err
	}
	s.active[root] = thread
	return root, overrode, nil
}

func (s *conversationHistoryStore) release(root string) {
	s.mu.Lock()
	delete(s.active, root)
	for key, streamingRoot := range s.streaming {
		if streamingRoot == root {
			delete(s.streaming, key)
		}
	}
	s.mu.Unlock()
}

// markStreaming records that the active turn rooted at root streams the
// response keyed key, until the turn is released.
func (s *conversationHistoryStore) markStreaming(root string, key []byte) {
	s.mu.Lock()
	if _, ok := s.active[root]; ok {
		s.streaming[string(key)] = root
	}
	s.mu.Unlock()
}

// turnActive reports whether a turn of the conversation rooted at root, or the
// turn streaming the response keyed key, is still active. Its outcome, and
// the history it saves, is not known yet.
func (s *conversationHistoryStore) turnActive(root string, key []byte) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.active[root]; ok && root != "" {
		return true
	}
	streamingRoot, ok := s.streaming[string(key)]
	if !ok {
		return false
	}
	_, ok = s.active[streamingRoot]
	return ok
}

func (s *conversationHistoryStore) update(fn func(*bolt.Tx, *conversationHistoryCounts) error) error {
	d := s.d
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.failed != nil || d.db == nil {
		return errConversationHistoryStorage
	}
	counts := s.counts
	err := d.db.Update(func(tx *bolt.Tx) error {
		if err := fn(tx, &counts); err != nil {
			return err
		}
		if d.beforeCommit != nil {
			return d.beforeCommit()
		}
		return nil
	})
	if err == nil {
		s.counts = counts
	}
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
	return s.beginAttemptFrom(root, operationID, nil, false)
}

// beginAttemptFrom records a turn's attempt. A pending record that admits the
// turn, as acquireFrom decides, is replaced; any other pending record refuses it.
func (s *conversationHistoryStore) beginAttemptFrom(root, operationID string, source []byte, trusted bool) error {
	return s.update(func(tx *bolt.Tx, counts *conversationHistoryCounts) error {
		pending, snapshots := tx.Bucket(conversationPendingBucket), tx.Bucket(conversationSnapshotsBucket)
		key := s.rootKey(root)
		added := 1
		if existing := pending.Get(key); existing != nil {
			if !trusted && !pendingReleasedTo(existing, source) {
				return errConversationHistoryUncertain
			}
			added = 0
		}
		if counts.snapshots+counts.pending+added > s.config.MaxSnapshots ||
			snapshots.Sequence()+uint64((counts.pending+added)*conversationPendingBytes) > uint64(s.config.MaxTotalBytes) {
			return errConversationHistoryCapacity
		}
		value := make([]byte, 40)
		binary.BigEndian.PutUint64(value[:8], uint64(s.d.now().Unix()))
		digest := s.d.digest("conversation-attempt-v1", operationID)
		copy(value[8:], digest[:])
		mac := s.d.digest("conversation-pending-record-v1", string(key), string(value))
		if err := pending.Put(key, append(mac[:], value...)); err != nil {
			return err
		}
		counts.pending += added
		return nil
	})
}

func (s *conversationHistoryStore) clearAttempt(root string) error {
	return s.update(func(tx *bolt.Tx, counts *conversationHistoryCounts) error {
		pending, key := tx.Bucket(conversationPendingBucket), s.rootKey(root)
		if pending.Get(key) == nil {
			return nil
		}
		if err := pending.Delete(key); err != nil {
			return err
		}
		counts.pending--
		return nil
	})
}

// save stores a completed turn's snapshot and clears its conversation's
// attempt marker in the same transaction.
func (s *conversationHistoryStore) save(snapshot *conversationSnapshot) error {
	return s.saveSnapshot(snapshot, false)
}

// saveDelivered stores the items an attempt delivered before the upstream ended
// it, and keeps the attempt marker pointed at that snapshot: only a turn that
// continues from those items clears it, never one that branches from earlier
// history and could repeat a delivered tool call.
func (s *conversationHistoryStore) saveDelivered(snapshot *conversationSnapshot) error {
	return s.saveSnapshot(snapshot, true)
}

func (s *conversationHistoryStore) saveSnapshot(snapshot *conversationSnapshot, delivered bool) error {
	key := s.responseKey(snapshot.RouteID, snapshot.ResponseID)
	encoded, err := s.encode(key, snapshot)
	if err != nil {
		return err
	}
	if encoded.historyBytes > s.config.MaxHistoryBytes {
		return errConversationHistoryCapacity
	}
	return s.update(func(tx *bolt.Tx, counts *conversationHistoryCounts) error {
		snapshots, index, pending := tx.Bucket(conversationSnapshotsBucket), tx.Bucket(conversationIndexBucket), tx.Bucket(conversationPendingBucket)
		if snapshots.Get(key) != nil {
			// Keep the pending turn and original history. An upstream ID collision
			// does not make the shared database unusable.
			return errConversationHistoryUncertain
		}
		rootKey := s.rootKey(snapshot.Root)
		existing := pending.Get(rootKey)
		if existing != nil && len(existing) != 72 {
			return errConversationHistoryStorage
		}
		// Copy the attempt's timestamp before later writes in this transaction.
		var created []byte
		if existing != nil {
			created = append([]byte(nil), existing[32:40]...)
		}
		pendingCount := counts.pending
		if existing != nil && !delivered {
			pendingCount--
		}
		cost, err := s.put(tx, key, snapshot, encoded)
		if err != nil {
			return err
		}
		if counts.snapshots+pendingCount+1 > s.config.MaxSnapshots ||
			cost > uint64(s.config.MaxTotalBytes) ||
			snapshots.Sequence()+cost+uint64(pendingCount*conversationPendingBytes) > uint64(s.config.MaxTotalBytes) {
			return errConversationHistoryCapacity
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
		switch {
		case delivered && created != nil:
			// Keep the attempt's timestamp for pruning; its digest becomes the key.
			value := append(created, key...)
			mac := s.d.digest("conversation-pending-record-v1", string(rootKey), string(value))
			if err := pending.Put(rootKey, append(mac[:], value...)); err != nil {
				return err
			}
		case !delivered:
			if err := pending.Delete(rootKey); err != nil {
				return err
			}
		}
		counts.snapshots++
		counts.pending = pendingCount
		return nil
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
	err = s.update(func(tx *bolt.Tx, counts *conversationHistoryCounts) error {
		snapshots := tx.Bucket(conversationSnapshotsBucket)
		if snapshots == nil {
			return nil
		}
		index, pending := tx.Bucket(conversationIndexBucket), tx.Bucket(conversationPendingBucket)
		if index == nil || pending == nil {
			return errConversationHistoryStorage
		}
		if err := s.validate(tx, counts); err != nil {
			return err
		}
		// An unresolved attempt and its source history must be retired together.
		// Process attempts first so newer pending roots retain all their history.
		retiredRoots := make(map[string]bool)
		cursor := pending.Cursor()
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
				counts.pending--
			}
		}
		var total uint64
		referenced := make(map[string]bool)
		cursor = snapshots.Cursor()
		for key, value := cursor.First(); key != nil; key, value = cursor.Next() {
			record, err := s.decodeRecord(key, value)
			if err != nil {
				return err
			}
			rootKey := s.rootKey(record.Root)
			if retiredRoots[string(rootKey)] || record.Created < before.Unix() && pending.Get(rootKey) == nil {
				if err := cursor.Delete(); err != nil {
					return err
				}
				counts.snapshots--
				removed++
				continue
			}
			total += conversationSnapshotCost(&record.conversationSnapshot, value)
			_ = record.eachBlob(func(blobKey []byte) error {
				referenced[string(blobKey)] = true
				return nil
			})
		}
		// Blobs live as long as any remaining snapshot references them.
		if blobs := tx.Bucket(conversationBlobsBucket); blobs != nil {
			cursor = blobs.Cursor()
			for key, content := cursor.First(); key != nil; key, content = cursor.Next() {
				if referenced[string(key)] {
					total += conversationBlobCost(content)
				} else if err := cursor.Delete(); err != nil {
					return err
				}
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
			snapshot, err := s.decodeRecord(key, value)
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
