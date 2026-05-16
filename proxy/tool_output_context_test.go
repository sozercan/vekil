package proxy

import (
	"testing"
	"time"
)

func TestToolExecutionContextStoreEvictsByInsertionUpdateOrder(t *testing.T) {
	store := NewToolExecutionContextStoreWithLimits(time.Hour, 2)
	createdAt := time.Now()

	store.Put("scope", ToolExecutionContext{CallID: "call-1", ToolName: "shell", OriginalCommand: "one", CreatedAt: createdAt})
	store.Put("scope", ToolExecutionContext{CallID: "call-2", ToolName: "shell", OriginalCommand: "two", CreatedAt: createdAt.Add(time.Second)})

	// Updating call-1 should make it the newest entry even though its CreatedAt is
	// still older than call-2. This keeps eviction O(1) by using store order.
	store.Put("scope", ToolExecutionContext{CallID: "call-1", ToolName: "shell", OriginalCommand: "one updated", CreatedAt: createdAt})
	store.Put("scope", ToolExecutionContext{CallID: "call-3", ToolName: "shell", OriginalCommand: "three", CreatedAt: createdAt.Add(2 * time.Second)})

	if _, ok := store.Get("scope", "call-2"); ok {
		t.Fatalf("expected call-2 to be evicted as the oldest insertion/update entry")
	}
	if got, ok := store.Get("scope", "call-1"); !ok || got.OriginalCommand != "one updated" {
		t.Fatalf("expected updated call-1 to remain, got %+v found=%v", got, ok)
	}
	if _, ok := store.Get("scope", "call-3"); !ok {
		t.Fatalf("expected newest call-3 to remain")
	}
}
