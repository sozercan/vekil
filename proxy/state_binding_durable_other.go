//go:build !linux && !darwin

package proxy

import (
	"os"

	bolt "go.etcd.io/bbolt"
)

func openDurableStateDatabase(string, bool) (*bolt.DB, *os.File, bool, func() error, error) {
	return nil, nil, false, nil, errDurableStatePlatform
}

func syncDurableStateDirectory(int) error { return errDurableStatePlatform }
