//go:build !windows

package macosruntime

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestHelperStateLockBlocksReplacementUntilRelease(t *testing.T) {
	paths := PathsInDirectory(t.TempDir())
	first, err := AcquireHelperStateLock(t.Context(), paths)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })

	type lockResult struct {
		lock *HelperStateLock
		err  error
	}
	result := make(chan lockResult, 1)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	go func() {
		lock, acquireErr := AcquireHelperStateLock(ctx, paths)
		result <- lockResult{lock: lock, err: acquireErr}
	}()

	select {
	case acquired := <-result:
		if acquired.lock != nil {
			_ = acquired.lock.Close()
		}
		t.Fatalf("replacement acquired helper state early: %v", acquired.err)
	case <-time.After(3 * helperStateLockRetryInterval):
	}

	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case acquired := <-result:
		if acquired.err != nil {
			t.Fatal(acquired.err)
		}
		if acquired.lock == nil {
			t.Fatal("replacement returned without a helper state lock")
		}
		if err := acquired.lock.Close(); err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal("replacement did not acquire helper state after release")
	}
}

func TestHelperStateLockRejectsSymlink(t *testing.T) {
	paths := PathsInDirectory(t.TempDir())
	target := filepath.Join(t.TempDir(), "foreign-lock")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, paths.Lock); err != nil {
		t.Fatal(err)
	}

	lock, err := AcquireHelperStateLock(t.Context(), paths)
	if lock != nil {
		_ = lock.Close()
	}
	if err == nil {
		t.Fatal("symlinked helper state lock was accepted")
	}
}
