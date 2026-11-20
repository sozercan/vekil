package main

import (
	"sync"

	"fyne.io/systray"
	"github.com/sozercan/vekil/logger"
)

type trayStatus uint8

const (
	trayStopped trayStatus = iota
	trayStarting
	trayStopping
	trayRunning
)

func (status trayStatus) tooltip() string {
	switch status {
	case trayStarting:
		return "Vekil - Starting"
	case trayStopping:
		return "Vekil - Stopping"
	case trayRunning:
		return "Vekil - Running on :1337"
	default:
		return "Vekil - Stopped"
	}
}

// trayStatus takes one lifecycle snapshot; authentication is independent of
// whether a provider-only proxy is running.
func (l *menubarProxyLifecycle) trayStatus() trayStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.shuttingDown {
		return trayStopped
	}
	if l.startupCancel != nil {
		if l.startupCanceled {
			return trayStopping
		}
		return trayStarting
	}
	if l.server != nil && l.server.IsRunning() {
		return trayRunning
	}
	return trayStopped
}

var trayStatusMu sync.Mutex
var trayIconWarning sync.Once

func refreshTrayStatus() {
	// snapshot after acquiring the rendering lock so an older caller cannot
	// overwrite the icon with a state captured before a newer transition.
	trayStatusMu.Lock()
	defer trayStatusMu.Unlock()
	status := proxyLifecycle.trayStatus()
	if err := renderTrayStatus(status); err != nil {
		trayIconWarning.Do(func() {
			log.Warn("using fallback tray icon", logger.Err(err))
		})
	}
	systray.SetTooltip(status.tooltip())
}
