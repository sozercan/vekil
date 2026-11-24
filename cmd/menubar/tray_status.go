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

func (l *menubarProxyLifecycle) trayStatus() trayStatus {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.shuttingDown {
		return trayStopped
	}
	if l.stopping {
		return trayStopping
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

var trayIconWarning sync.Once

func refreshTrayStatus() trayStatus {
	status := proxyLifecycle.trayStatus()
	if err := renderTrayStatus(status); err != nil {
		trayIconWarning.Do(func() {
			log.Warn("using fallback tray icon", logger.Err(err))
		})
	}
	systray.SetTooltip(status.tooltip())
	return status
}
