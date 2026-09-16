//go:build !linux

package proxy

import bolt "go.etcd.io/bbolt"

func openDurableStateDatabase(string, bool) (*bolt.DB, bool, func() error, error) {
	return nil, false, nil, errDurableStatePlatform
}
