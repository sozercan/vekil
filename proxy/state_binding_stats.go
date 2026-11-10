package proxy

import bolt "go.etcd.io/bbolt"

// stateBindingStatsSnapshot contains only storage gauges. Unavailable values
// stay null rather than suggesting an empty database after a read or close.
// Entries counts all retained logical records, including conflict tombstones.
type stateBindingStatsSnapshot struct {
	Mode                 string   `json:"mode"`
	Status               string   `json:"status"`
	Entries              *uint64  `json:"entries"`
	MaxEntries           int      `json:"max_entries"`
	DatabaseBytes        *int64   `json:"database_bytes"`
	CapacityUsagePercent *float64 `json:"capacity_usage_percent"`
	CapacityStatus       string   `json:"capacity_status"`
}

func (s *stateBindingStore) dashboardStats() stateBindingStatsSnapshot {
	if s == nil {
		return stateBindingStatsSnapshot{Status: "unavailable", CapacityStatus: "unknown"}
	}
	if s.durable != nil {
		return s.durable.dashboardStats()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	entries, databaseBytes := uint64(len(s.entries)), int64(0)
	snapshot := stateBindingStatsSnapshot{
		Mode:          "memory",
		Status:        "ready",
		Entries:       &entries,
		MaxEntries:    s.maxEntries,
		DatabaseBytes: &databaseBytes,
	}
	snapshot.setCapacity()
	return snapshot
}

func (d *durableStateBindings) dashboardStats() stateBindingStatsSnapshot {
	d.mu.Lock()
	defer d.mu.Unlock()
	snapshot := stateBindingStatsSnapshot{
		Mode:           "durable",
		Status:         "ready",
		MaxEntries:     d.maxEntries,
		CapacityStatus: "unknown",
	}
	if d.db == nil {
		snapshot.Status = "closed"
		return snapshot
	}
	if d.failed != nil {
		snapshot.Status = "frozen"
	}
	// The bucket sequence is the committed logical record count. Polling must
	// not walk records or hold up writers in proportion to database occupancy.
	err := d.db.View(func(tx *bolt.Tx) error {
		records := tx.Bucket(durableStateRecords)
		if records == nil {
			return errDurableStateCorrupt
		}
		entries := records.Sequence()
		snapshot.Entries = &entries
		return nil
	})
	if err != nil {
		if snapshot.Status != "frozen" {
			snapshot.Status = "unavailable"
		}
		return snapshot
	}
	snapshot.setCapacity()
	// Tx.Size reports used pages, not the file's current length. Stat the same
	// descriptor bbolt uses so a renamed or replaced path cannot change gauges.
	info, err := d.file.Stat()
	if err != nil {
		if snapshot.Status != "frozen" {
			snapshot.Status = "unavailable"
		}
		return snapshot
	}
	databaseBytes := info.Size()
	snapshot.DatabaseBytes = &databaseBytes
	return snapshot
}

func (s *stateBindingStatsSnapshot) setCapacity() {
	s.CapacityStatus = "unknown"
	if s.Entries == nil || s.MaxEntries <= 0 {
		return
	}
	percent := float64(*s.Entries) / float64(s.MaxEntries) * 100
	s.CapacityUsagePercent = &percent
	switch {
	case *s.Entries >= uint64(s.MaxEntries):
		s.CapacityStatus = "exhausted"
	case percent >= 95:
		s.CapacityStatus = "critical"
	case percent >= 80:
		s.CapacityStatus = "warning"
	default:
		s.CapacityStatus = "ok"
	}
}
