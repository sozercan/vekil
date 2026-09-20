package proxy

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	bolt "go.etcd.io/bbolt"
)

func BenchmarkConversationHistoryAdmission(b *testing.B) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		b.Skip("conversation migration requires Linux or macOS durable storage")
	}
	for _, entries := range []int{0, 4096, 32768} {
		b.Run(fmt.Sprintf("snapshots=%d", entries), func(b *testing.B) {
			dir := b.TempDir()
			if err := os.Chmod(dir, 0o700); err != nil {
				b.Fatal(err)
			}
			bindings, err := newDurableStateBindingStore(DurableStateBindingsConfig{Path: filepath.Join(dir, "bindings.db"), MaxEntries: 1})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() {
				if err := bindings.close(); err != nil {
					b.Error(err)
				}
			})
			config := ConversationMigrationConfig{MaxSnapshots: entries + 1}
			history, err := newConversationHistoryStore(bindings.durable, config)
			if err != nil {
				b.Fatal(err)
			}
			// Seed valid snapshots in one transaction, then run normal startup
			// validation. Fixture construction is outside the measured hot path.
			err = bindings.durable.db.Update(func(tx *bolt.Tx) error {
				snapshots, index := tx.Bucket(conversationSnapshotsBucket), tx.Bucket(conversationIndexBucket)
				var total uint64
				for i := 0; i < entries; i++ {
					id := fmt.Sprintf("seed-%d", i)
					snapshot := conversationHistoryStorageSnapshot(history, id, id, time.Now())
					key := history.responseKey(snapshot.RouteID, snapshot.ResponseID)
					value, err := history.encode(key, snapshot)
					if err != nil {
						return err
					}
					if err := snapshots.Put(key, value); err != nil {
						return err
					}
					for _, anchor := range snapshot.Indexes {
						if err := index.Put(anchor, key); err != nil {
							return err
						}
					}
					total += conversationSnapshotCost(snapshot, value)
				}
				return snapshots.SetSequence(total)
			})
			if err != nil {
				b.Fatal(err)
			}
			history, err = newConversationHistoryStore(bindings.durable, config)
			if err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if err := history.beginAttempt("bench-root", "bench-operation"); err != nil {
					b.Fatal(err)
				}
				if err := history.clearAttempt("bench-root"); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
