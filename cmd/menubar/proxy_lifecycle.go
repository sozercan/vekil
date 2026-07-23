package main

import (
	"context"
	"sync"
)

type menubarProxyServer interface {
	Start() error
	UsesCopilot() bool
	ValidateDynamicProviderModels(context.Context) error
	InitializePolicyRouting(context.Context) error
	Stop(context.Context) error
	IsRunning() bool
}

type proxyStartupCompletion uint8

const (
	proxyStartupCurrent proxyStartupCompletion = iota
	proxyStartupCanceled
	proxyStartupSuperseded
)

// menubarProxyLifecycle serializes the published server and cancellable startup
// attempt. Slow authentication or policy preflight work runs outside this lock.
// A canceled attempt remains in flight until its worker has stopped any listener,
// which prevents a replacement startup from racing the old listener for the port.
type menubarProxyLifecycle struct {
	mu sync.Mutex

	server              menubarProxyServer
	startupCancel       context.CancelFunc
	startupCanceled     bool
	restartAfterStartup bool
	startupGeneration   uint64
	shuttingDown        bool
}

func (l *menubarProxyLifecycle) beginStartup(parent context.Context) (context.Context, uint64, bool) {
	if parent == nil {
		parent = context.Background()
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	if l.shuttingDown || l.startupCancel != nil {
		return nil, 0, false
	}
	if l.server != nil {
		if l.server.IsRunning() {
			return nil, 0, false
		}
		l.server = nil
	}

	ctx, cancel := context.WithCancel(parent)
	l.startupGeneration++
	l.startupCancel = cancel
	l.startupCanceled = false
	l.restartAfterStartup = false
	return ctx, l.startupGeneration, true
}

func (l *menubarProxyLifecycle) cancelStartup(restart bool) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.startupCancel == nil {
		return false
	}
	if restart {
		l.restartAfterStartup = true
	}
	if !l.startupCanceled {
		l.startupCancel()
		l.startupCanceled = true
	}
	return true
}

func (l *menubarProxyLifecycle) finishStartup(generation uint64, started menubarProxyServer) (proxyStartupCompletion, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.shuttingDown || generation != l.startupGeneration {
		return proxyStartupSuperseded, false
	}
	if l.startupCancel == nil {
		return proxyStartupSuperseded, false
	}

	l.startupCancel()
	canceled := l.startupCanceled
	restart := l.restartAfterStartup
	l.startupCancel = nil
	l.startupCanceled = false
	l.restartAfterStartup = false
	if canceled {
		return proxyStartupCanceled, restart
	}

	l.server = started
	return proxyStartupCurrent, false
}

func (l *menubarProxyLifecycle) startupState() (inFlight, canceled bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.startupCancel != nil, l.startupCanceled
}

func (l *menubarProxyLifecycle) isRunning() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.server != nil && l.server.IsRunning()
}

func (l *menubarProxyLifecycle) usesCopilot() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.server != nil && l.server.UsesCopilot()
}

func (l *menubarProxyLifecycle) detachServer() menubarProxyServer {
	l.mu.Lock()
	defer l.mu.Unlock()

	current := l.server
	l.server = nil
	return current
}

func (l *menubarProxyLifecycle) shutdown() menubarProxyServer {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.shuttingDown = true
	if l.startupCancel != nil {
		l.startupCancel()
		l.startupCanceled = true
	}
	l.restartAfterStartup = false
	current := l.server
	l.server = nil
	return current
}
