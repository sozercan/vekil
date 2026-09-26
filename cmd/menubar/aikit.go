package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/sozercan/vekil/aikit"
	"github.com/sozercan/vekil/logger"
)

// aikitProxyServer removes the server's AIKit containers when it stops.
type aikitProxyServer struct {
	menubarProxyServer
	group *aikit.Group
}

func (s aikitProxyServer) Stop(ctx context.Context) error {
	stopErr := s.menubarProxyServer.Stop(ctx)
	return errors.Join(stopErr, runAIKitCleanup(s.group.Close))
}

// pendingAIKitCleanups holds container removals that failed. Orphan reaping
// skips containers this still-running process owns, so the next start retries
// them instead of loading another model beside a leftover.
var pendingAIKitCleanups struct {
	mu       sync.Mutex
	cleanups []func(context.Context) error
}

// runAIKitCleanup runs cleanup with its own deadline, since graceful shutdown
// may have used up the caller's, and keeps it for a retry when it fails.
func runAIKitCleanup(cleanup func(context.Context) error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	err := cleanup(ctx)
	if err != nil {
		pendingAIKitCleanups.mu.Lock()
		pendingAIKitCleanups.cleanups = append(pendingAIKitCleanups.cleanups, cleanup)
		pendingAIKitCleanups.mu.Unlock()
	}
	return err
}

// retryAIKitCleanups retries removals that failed earlier.
func retryAIKitCleanups() error {
	pendingAIKitCleanups.mu.Lock()
	cleanups := pendingAIKitCleanups.cleanups
	pendingAIKitCleanups.cleanups = nil
	pendingAIKitCleanups.mu.Unlock()
	var errs []error
	for _, cleanup := range cleanups {
		errs = append(errs, runAIKitCleanup(cleanup))
	}
	return errors.Join(errs...)
}

// aikitStartupLog forwards AIKit start progress lines to the tray log.
type aikitStartupLog struct {
	mu      sync.Mutex
	pending bytes.Buffer
}

func (w *aikitStartupLog) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.pending.Write(p)
	for {
		line, err := w.pending.ReadString('\n')
		if err != nil {
			w.pending.Reset()
			w.pending.WriteString(line)
			return len(p), nil
		}
		if text := strings.TrimSpace(line); text != "" {
			log.Info("aikit", logger.F("status", text))
		}
	}
}
