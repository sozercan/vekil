package main

import (
	"context"
	"errors"
	"testing"
)

func TestFailedAIKitCleanupIsRetried(t *testing.T) {
	t.Cleanup(func() { pendingAIKitCleanups.cleanups = nil })
	attempts := 0
	cleanup := func(context.Context) error {
		attempts++
		if attempts < 3 {
			return errors.New("engine busy")
		}
		return nil
	}
	if err := runAIKitCleanup(cleanup); err == nil {
		t.Fatal("runAIKitCleanup() = nil, want the cleanup error")
	}
	if err := retryAIKitCleanups(); err == nil {
		t.Fatal("first retry = nil, want the cleanup error")
	}
	if err := retryAIKitCleanups(); err != nil {
		t.Fatalf("second retry = %v", err)
	}
	if err := retryAIKitCleanups(); err != nil || attempts != 3 {
		t.Fatalf("retry after success = %v, attempts = %d", err, attempts)
	}
}
