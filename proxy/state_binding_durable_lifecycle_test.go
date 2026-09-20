package proxy

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
)

func TestDurableStateLifecycleBackgroundFinalizationWaitsForWorker(t *testing.T) {
	s, config := newDurableStoreFixture(t, 4)
	h := &ProxyHandler{stateBindings: s}
	if !h.beginLifecycleWorker() {
		t.Fatal("worker registration failed")
	}
	h.BeginShutdown()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.WaitLifecycleWorkers(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("initial drain = %v", err)
	}
	finalized := make(chan error, 1)
	go func() { finalized <- h.WaitLifecycleWorkers(context.Background()) }()
	if second, err := newDurableStateBindingStore(config); !errors.Is(err, errDurableStateLocked) {
		if second != nil {
			_ = second.close()
		}
		t.Errorf("unfinished worker released lock: %v", err)
	}
	select {
	case err := <-finalized:
		t.Errorf("finalized before worker completion: %v", err)
	default:
	}
	h.endLifecycleWorker()
	select {
	case err := <-finalized:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("finalization did not finish after worker return")
	}
	second, err := newDurableStateBindingStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDurableStoreFixture(t, second)
}

func TestDurableStateLifecycleRetainsLockUntilDrain(t *testing.T) {
	s, config := newDurableStoreFixture(t, 4)
	h := &ProxyHandler{stateBindings: s}
	if !h.beginLifecycleWorker() {
		t.Fatal("worker registration failed")
	}
	h.BeginShutdown()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := h.WaitLifecycleWorkers(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("unfinished drain = %v", err)
	}
	if second, err := newDurableStateBindingStore(config); !errors.Is(err, errDurableStateLocked) {
		if second != nil {
			_ = second.close()
		}
		t.Fatalf("timed-out drain released lock: %v", err)
	}
	h.endLifecycleWorker()
	// An expired context is not proof that all HTTP handlers have drained.
	if err := h.WaitLifecycleWorkers(ctx); err != nil {
		t.Fatal(err)
	}
	if second, err := newDurableStateBindingStore(config); !errors.Is(err, errDurableStateLocked) {
		if second != nil {
			_ = second.close()
		}
		t.Fatalf("expired drain released lock: %v", err)
	}
	if err := h.WaitLifecycleWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := h.WaitLifecycleWorkers(context.Background()); err != nil {
		t.Fatal(err)
	}
	second, err := newDurableStateBindingStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDurableStoreFixture(t, second)
}

func TestDurableStateHandlerConstructionFailureReleasesLock(t *testing.T) {
	s, config := newDurableStoreFixture(t, 4)
	if err := s.close(); err != nil {
		t.Fatal(err)
	}
	_, err := NewProxyHandler(auth.NewTestAuthenticator("synthetic-token"), logger.New(logger.LevelFatal),
		WithDurableStateBindings(config), WithProvidersConfig(ProvidersConfig{SchemaVersion: 999}))
	if err == nil {
		t.Fatal("invalid providers accepted")
	}
	second, err := newDurableStateBindingStore(config)
	if err != nil {
		t.Fatal(err)
	}
	defer closeDurableStoreFixture(t, second)
}
