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
	// Graceful shutdown may use up ctx; removing containers needs its own time.
	cleanupCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return errors.Join(stopErr, s.group.Close(cleanupCtx))
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
