package main

import (
	"context"
	"errors"
	"sync"
	"testing"
)

type fakeMenubarProxyServer struct {
	mu          sync.Mutex
	running     bool
	usesCopilot bool
}

func (s *fakeMenubarProxyServer) Start() error { return nil }

func (s *fakeMenubarProxyServer) UsesCopilot() bool { return s.usesCopilot }

func (s *fakeMenubarProxyServer) ValidateDynamicProviderModels(context.Context) error { return nil }

func (s *fakeMenubarProxyServer) InitializePolicyRouting(context.Context) error { return nil }

func (s *fakeMenubarProxyServer) Stop(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.running = false
	return nil
}

func (s *fakeMenubarProxyServer) IsRunning() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

func TestMenubarProxyLifecycleCancelStartup(t *testing.T) {
	var lifecycle menubarProxyLifecycle
	ctx, generation, _, ok := lifecycle.beginStartup(t.Context())
	if !ok {
		t.Fatal("beginStartup() = false, want true")
	}
	if inFlight, canceled := lifecycle.startupState(); !inFlight || canceled {
		t.Fatalf("startupState() = (%v, %v), want (true, false)", inFlight, canceled)
	}

	if !lifecycle.cancelStartup(false) {
		t.Fatal("cancelStartup() = false, want true")
	}
	if inFlight, canceled := lifecycle.startupState(); !inFlight || !canceled {
		t.Fatalf("startupState() after cancel = (%v, %v), want (true, true)", inFlight, canceled)
	}
	select {
	case <-ctx.Done():
	case <-t.Context().Done():
		t.Fatal("startup context was not canceled")
	}
	if got, restart := lifecycle.finishStartup(generation, &fakeMenubarProxyServer{running: true}); got != proxyStartupCanceled || restart {
		t.Fatalf("finishStartup() = (%v, %v), want (canceled, false)", got, restart)
	}
	if lifecycle.isRunning() {
		t.Fatal("canceled startup published a running server")
	}
}

func TestMenubarProxyLifecycleUsesPublishedServerScope(t *testing.T) {
	var lifecycle menubarProxyLifecycle
	_, generation, _, ok := lifecycle.beginStartup(t.Context())
	if !ok {
		t.Fatal("beginStartup() = false, want true")
	}
	server := &fakeMenubarProxyServer{running: true, usesCopilot: true}
	if got, restart := lifecycle.finishStartup(generation, server); got != proxyStartupCurrent || restart {
		t.Fatalf("finishStartup() = (%v, %v), want (current, false)", got, restart)
	}
	if !lifecycle.usesCopilot() {
		t.Fatal("usesCopilot() = false, want published server scope")
	}
	server.usesCopilot = false
	if lifecycle.usesCopilot() {
		t.Fatal("usesCopilot() = true after runtime scope changed")
	}
}

func TestMenubarProxyLifecycleCanceledStartupBlocksReplacementUntilCleanup(t *testing.T) {
	var lifecycle menubarProxyLifecycle
	firstCtx, firstGeneration, _, ok := lifecycle.beginStartup(t.Context())
	if !ok {
		t.Fatal("first beginStartup() = false, want true")
	}
	if !lifecycle.cancelStartup(true) {
		t.Fatal("cancelStartup() = false, want true")
	}
	if _, _, _, ok := lifecycle.beginStartup(t.Context()); ok {
		t.Fatal("replacement startup began before canceled listener cleanup")
	}
	select {
	case <-firstCtx.Done():
	case <-t.Context().Done():
		t.Fatal("startup context was not canceled")
	}

	if got, restart := lifecycle.finishStartup(firstGeneration, &fakeMenubarProxyServer{running: true}); got != proxyStartupCanceled || !restart {
		t.Fatalf("first finishStartup() = (%v, %v), want (canceled, true)", got, restart)
	}
	if lifecycle.isRunning() {
		t.Fatal("canceled startup published a running server")
	}

	_, secondGeneration, _, ok := lifecycle.beginStartup(t.Context())
	if !ok {
		t.Fatal("replacement beginStartup() = false after cleanup, want true")
	}
	second := &fakeMenubarProxyServer{running: true}
	if got, restart := lifecycle.finishStartup(secondGeneration, second); got != proxyStartupCurrent || restart {
		t.Fatalf("second finishStartup() = (%v, %v), want (current, false)", got, restart)
	}
	if !lifecycle.isRunning() {
		t.Fatal("current startup did not publish the running server")
	}
	if got := lifecycle.detachServer(); got != second {
		t.Fatalf("detachServer() = %p, want %p", got, second)
	}
}

func TestMenubarProxyLifecycleShutdownCancelsStartupAndRejectsCompletion(t *testing.T) {
	var lifecycle menubarProxyLifecycle
	ctx, generation, _, ok := lifecycle.beginStartup(t.Context())
	if !ok {
		t.Fatal("beginStartup() = false, want true")
	}
	if got := lifecycle.shutdown(); got != nil {
		t.Fatalf("shutdown() server = %v, want nil", got)
	}
	select {
	case <-ctx.Done():
	case <-t.Context().Done():
		t.Fatal("shutdown did not cancel startup context")
	}
	if got, restart := lifecycle.finishStartup(generation, &fakeMenubarProxyServer{running: true}); got != proxyStartupSuperseded || restart {
		t.Fatalf("finishStartup() = (%v, %v), want (superseded, false)", got, restart)
	}
	if _, _, _, ok := lifecycle.beginStartup(t.Context()); ok {
		t.Fatal("beginStartup() succeeded after shutdown")
	}
}

type blockingPolicyMenubarServer struct {
	preflightStarted chan struct{}
	stopped          chan struct{}
}

func (s *blockingPolicyMenubarServer) Start() error { return nil }

func (s *blockingPolicyMenubarServer) UsesCopilot() bool { return false }

func (s *blockingPolicyMenubarServer) ValidateDynamicProviderModels(context.Context) error {
	return nil
}

func (s *blockingPolicyMenubarServer) InitializePolicyRouting(ctx context.Context) error {
	close(s.preflightStarted)
	<-ctx.Done()
	return ctx.Err()
}

func (s *blockingPolicyMenubarServer) Stop(context.Context) error {
	close(s.stopped)
	return nil
}

func (s *blockingPolicyMenubarServer) IsRunning() bool { return true }

func TestInitializeProxyPolicyRoutingCancellationStopsListener(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	current := &blockingPolicyMenubarServer{
		preflightStarted: make(chan struct{}),
		stopped:          make(chan struct{}),
	}
	done := make(chan error, 1)
	go func() {
		done <- initializeProxyPolicyRouting(ctx, current)
	}()

	select {
	case <-current.preflightStarted:
	case <-t.Context().Done():
		t.Fatal("policy preflight did not start")
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("initializeProxyPolicyRouting() error = %v, want context canceled", err)
		}
	case <-t.Context().Done():
		t.Fatal("policy preflight did not return after cancellation")
	}
	select {
	case <-current.stopped:
	case <-t.Context().Done():
		t.Fatal("canceled startup left the listener running")
	}
}

type stopRecordingServer struct {
	fakeMenubarProxyServer
	stopped chan struct{}
}

func (s *stopRecordingServer) Stop(ctx context.Context) error {
	err := s.fakeMenubarProxyServer.Stop(ctx)
	close(s.stopped)
	return err
}

func TestMenubarProxyLifecycleStopsExitedServerBeforeRestart(t *testing.T) {
	var lifecycle menubarProxyLifecycle
	_, generation, _, ok := lifecycle.beginStartup(t.Context())
	if !ok {
		t.Fatal("beginStartup() = false, want true")
	}
	exited := &stopRecordingServer{stopped: make(chan struct{})}
	if got, _ := lifecycle.finishStartup(generation, exited); got != proxyStartupCurrent {
		t.Fatalf("finishStartup() = %v, want current", got)
	}
	// The server exited on its own; a restart must Stop it, removing any AIKit
	// containers it owns, before the replacement starts.
	_, _, stopPrevious, ok := lifecycle.beginStartup(t.Context())
	if !ok {
		t.Fatal("restart beginStartup() = false, want true")
	}
	select {
	case <-exited.stopped:
		t.Fatal("exited server was stopped under the lifecycle lock")
	default:
	}
	if err := stopPrevious(); err != nil {
		t.Fatalf("stopPrevious() = %v", err)
	}
	select {
	case <-exited.stopped:
	default:
		t.Fatal("stopPrevious returned before the exited server stopped")
	}
}
