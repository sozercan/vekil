package main

import (
	"context"
	"time"

	"fyne.io/systray"
	"github.com/sozercan/copilot-proxy/auth"
	"github.com/sozercan/copilot-proxy/logger"
	"github.com/sozercan/copilot-proxy/server"
)

var log = logger.New(logger.ParseLevel("info"))

var (
	srv           *server.Server
	authenticator *auth.Authenticator
)

func main() {
	authenticator = auth.NewAuthenticator("")
	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetIcon(iconOff)
	systray.SetTooltip("Copilot Proxy - Stopped")

	mToggle := systray.AddMenuItem("Start Proxy", "Start or stop the proxy")
	mLaunch := systray.AddMenuItemCheckbox("Launch at Login", "Launch at Login", false)
	if isLaunchAgentInstalled() {
		mLaunch.Check()
	}

	systray.AddSeparator()
	mQuit := systray.AddMenuItem("Quit", "Quit the application")

	go func() {
		for {
			select {
			case <-mToggle.ClickedCh:
				if srv != nil && srv.IsRunning() {
					stopProxy(mToggle)
				} else {
					startProxy(mToggle)
				}
			case <-mLaunch.ClickedCh:
				if mLaunch.Checked() {
					if err := removeLaunchAgent(); err != nil {
						log.Error("failed to remove launch agent", logger.Err(err))
					} else {
						mLaunch.Uncheck()
					}
				} else {
					if err := installLaunchAgent(); err != nil {
						log.Error("failed to install launch agent", logger.Err(err))
					} else {
						mLaunch.Check()
					}
				}
			case <-mQuit.ClickedCh:
				if srv != nil && srv.IsRunning() {
					stopProxy(mToggle)
				}
				systray.Quit()
				return
			}
		}
	}()
}

func startProxy(mToggle *systray.MenuItem) {
	if _, err := authenticator.GetToken(context.Background()); err != nil {
		log.Error("auth failed", logger.Err(err))
		return
	}

	srv = server.New(authenticator, log, "0.0.0.0", "1337")
	if err := srv.Start(); err != nil {
		log.Error("server start failed", logger.Err(err))
		return
	}

	mToggle.SetTitle("Stop Proxy")
	systray.SetIcon(iconOn)
	systray.SetTooltip("Copilot Proxy - Running on :1337")
	log.Info("proxy started")
}

func stopProxy(mToggle *systray.MenuItem) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Stop(ctx); err != nil {
		log.Error("server stop failed", logger.Err(err))
	}

	mToggle.SetTitle("Start Proxy")
	systray.SetIcon(iconOff)
	systray.SetTooltip("Copilot Proxy - Stopped")
	log.Info("proxy stopped")
}

func onExit() {
	if srv != nil && srv.IsRunning() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	}
}
