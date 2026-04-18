package main

import (
	"context"
	"fmt"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/server"
)

var log = logger.New(logger.ParseLevel("info"))

var (
	srv           *server.Server
	authenticator *auth.Authenticator

	// Menu items kept at package level so helpers can update them.
	mStatus *systray.MenuItem
	mToggle *systray.MenuItem
	mAuth   *systray.MenuItem

	// signInMu guards signInCancel to prevent concurrent sign-in flows.
	signInMu     sync.Mutex
	signInCancel context.CancelFunc
)

func main() {
	var err error
	authenticator, err = auth.NewAuthenticator("")
	if err != nil {
		log.Fatal("failed to initialize authenticator", logger.Err(err))
	}
	authenticator.DisableAutoDeviceFlow = true
	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetIcon(iconOff)
	systray.SetTooltip("Vekil - Stopped")

	mStatus = systray.AddMenuItem("○ Not signed in", "")
	mStatus.Disable()
	mVersion := systray.AddMenuItem(versionMenuTitle(), "Current app version")
	mVersion.Disable()
	systray.AddSeparator()

	mToggle = systray.AddMenuItem("Start Vekil", "Start or stop Vekil")
	systray.AddSeparator()

	mLaunch := systray.AddMenuItemCheckbox("Launch at Login", "Launch at Login", false)
	if isLaunchAgentInstalled() {
		if err := installLaunchAgent(); err != nil {
			log.Error("failed to refresh launch agent", logger.Err(err))
		}
		mLaunch.Check()
	}

	mAuth = systray.AddMenuItem("Sign In", "Sign in or out of GitHub")
	systray.AddSeparator()

	var mCheckUpdates *systray.MenuItem
	if updaterSupported() {
		mCheckUpdates = systray.AddMenuItem("Check for Updates…", "Check for Vekil updates")
		if err := startUpdater(); err != nil {
			log.Error("failed to start updater", logger.Err(err))
			mCheckUpdates.Disable()
		}
		systray.AddSeparator()
	}

	mQuit := systray.AddMenuItem("Quit", "Quit the application")

	// Set initial UI state based on whether we already have credentials.
	if authenticator.IsSignedIn() {
		setSignedInUI()
	} else {
		setSignedOutUI()
	}

	var mCheckUpdatesClicked <-chan struct{}
	if mCheckUpdates != nil {
		mCheckUpdatesClicked = mCheckUpdates.ClickedCh
	}

	go func() {
		for {
			select {
			case <-mToggle.ClickedCh:
				if srv != nil && srv.IsRunning() {
					stopProxy()
				} else {
					startProxy()
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
			case <-mAuth.ClickedCh:
				if authenticator.IsSignedIn() {
					signOut()
				} else {
					go signIn()
				}
			case <-mCheckUpdatesClicked:
				if err := checkForUpdates(); err != nil {
					log.Error("failed to check for updates", logger.Err(err))
					showErrorDialog("Update Check Failed", err.Error())
				}
			case <-mQuit.ClickedCh:
				if srv != nil && srv.IsRunning() {
					stopProxy()
				}
				systray.Quit()
				return
			}
		}
	}()
}

func startProxy() {
	if _, err := authenticator.GetToken(context.Background()); err != nil {
		log.Error("auth failed", logger.Err(err))
		// Token refresh failed — force re-authentication.
		_ = authenticator.SignOut()
		go signIn()
		return
	}

	nextSrv, err := server.New(authenticator, log, "0.0.0.0", "1337")
	if err != nil {
		log.Error("server init failed", logger.Err(err))
		showErrorDialog("Vekil Start Failed", fmt.Sprintf("Could not initialize Vekil.\n\n%v", err))
		return
	}
	if err := nextSrv.Start(); err != nil {
		log.Error("server start failed", logger.Err(err))
		showErrorDialog("Vekil Start Failed", fmt.Sprintf("Could not start Vekil on port 1337.\n\n%v", err))
		return
	}
	srv = nextSrv

	mToggle.SetTitle("Stop Vekil")
	systray.SetIcon(iconOn)
	systray.SetTooltip("Vekil - Running on :1337")
	log.Info("proxy started")
}

func stopProxy() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if err := srv.Stop(ctx); err != nil {
		log.Error("server stop failed", logger.Err(err))
	}

	mToggle.SetTitle("Start Vekil")
	systray.SetIcon(iconOff)
	systray.SetTooltip("Vekil - Stopped")
	log.Info("proxy stopped")
}

// signIn drives the interactive GitHub device-code flow via native macOS
// dialogs. It is expected to be called in its own goroutine.
func signIn() {
	// Guard against double sign-in.
	signInMu.Lock()
	if signInCancel != nil {
		signInMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	signInCancel = cancel
	signInMu.Unlock()

	defer func() {
		signInMu.Lock()
		signInCancel = nil
		signInMu.Unlock()
	}()

	mAuth.Disable()
	mStatus.SetTitle("⟳ Signing in…")

	dcResp, err := authenticator.RequestDeviceCode(ctx)
	if err != nil {
		log.Error("device code request failed", logger.Err(err))
		showErrorDialog("Sign In Failed", fmt.Sprintf("Could not start sign-in: %v", err))
		setSignedOutUI()
		mAuth.Enable()
		return
	}

	copyToClipboard(dcResp.UserCode)

	button := showOsascriptDialog(
		"Sign in to GitHub Copilot",
		fmt.Sprintf("Your code has been copied to the clipboard.\n\nEnter this code on GitHub:\n\n%s", dcResp.UserCode),
		"Open GitHub",
		"Cancel",
	)

	if button == "Cancel" {
		cancel()
		setSignedOutUI()
		mAuth.Enable()
		return
	}

	openURL(dcResp.VerificationURI)
	mStatus.SetTitle("⟳ Waiting for authorization…")

	if err := authenticator.PollForAuthorization(ctx, dcResp); err != nil {
		log.Error("authorization failed", logger.Err(err))
		if ctx.Err() == nil {
			// Only show error dialog if we weren't cancelled.
			showErrorDialog("Sign In Failed", fmt.Sprintf("Authorization failed: %v", err))
		}
		setSignedOutUI()
		mAuth.Enable()
		return
	}

	setSignedInUI()
	showNotification("Vekil", "Successfully signed in to GitHub.")
	log.Info("sign-in complete")
}

// signOut stops the proxy (if running) and clears all credentials.
func signOut() {
	// Cancel any in-progress sign-in.
	signInMu.Lock()
	if signInCancel != nil {
		signInCancel()
	}
	signInMu.Unlock()

	if srv != nil && srv.IsRunning() {
		stopProxy()
	}

	if err := authenticator.SignOut(); err != nil {
		log.Error("sign-out error", logger.Err(err))
	}

	setSignedOutUI()
	log.Info("signed out")
}

// setSignedInUI updates the menu to reflect an authenticated state.
func setSignedInUI() {
	mStatus.SetTitle("● Signed in to GitHub")
	mAuth.SetTitle("Sign Out")
	mAuth.Enable()
	mToggle.Enable()
}

// setSignedOutUI updates the menu to reflect an unauthenticated state.
func setSignedOutUI() {
	mStatus.SetTitle("○ Not signed in")
	mAuth.SetTitle("Sign In")
	mAuth.Enable()
	mToggle.Disable()
	systray.SetIcon(iconOff)
	systray.SetTooltip("Vekil - Stopped")
}

func onExit() {
	if srv != nil && srv.IsRunning() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	}
}
