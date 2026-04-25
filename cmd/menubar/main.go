package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
	"github.com/sozercan/vekil/server"
)

var log = logger.New(logger.ParseLevel("info"))

var (
	srv           *server.Server
	authenticator *auth.Authenticator

	// Menu items kept at package level so helpers can update them.
	mAuthMenu        *systray.MenuItem
	mToggle          *systray.MenuItem
	mSignInGitHub    *systray.MenuItem
	mUseGitHubCLI    *systray.MenuItem
	mSignOut         *systray.MenuItem
	mProvidersStatus *systray.MenuItem
	mProvidersChoose *systray.MenuItem
	mProvidersClear  *systray.MenuItem

	// signInMu guards signInCancel to prevent concurrent sign-in flows.
	signInMu     sync.Mutex
	signInCancel context.CancelFunc

	menubarCfg         menubarConfig
	providersCfg       proxy.ProvidersConfig
	providersConfigErr error
)

func main() {
	var err error
	authenticator, err = auth.NewAuthenticator("")
	if err != nil {
		log.Fatal("failed to initialize authenticator", logger.Err(err))
	}
	authenticator.DisableAutoDeviceFlow = true

	menubarCfg, providersCfg, providersConfigErr = loadProvidersConfigForMenubar()
	if providersConfigErr != nil {
		logProvidersConfigLoadError(providersConfigErr)
	}

	systray.Run(onReady, onExit)
}

func onReady() {
	systray.SetIcon(iconOff)
	systray.SetTooltip("Vekil - Stopped")

	mAuthMenu = systray.AddMenuItem("GitHub Auth: Not Signed In", "GitHub authentication")
	mSignInGitHub = mAuthMenu.AddSubMenuItem("Sign In with GitHub", "Sign in through GitHub in your browser and let Vekil manage the token")
	mUseGitHubCLI = mAuthMenu.AddSubMenuItem("Use GitHub CLI Account", "Use the account already authenticated with gh auth login")
	mAuthMenu.AddSeparator()
	mSignOut = mAuthMenu.AddSubMenuItem("Sign Out", "Clear Vekil authentication")

	mVersion := systray.AddMenuItem(versionMenuTitle(), "Current app version")
	mVersion.Disable()
	systray.AddSeparator()

	mProvidersStatus = systray.AddMenuItem("Providers: Copilot default", "")
	mProvidersStatus.Disable()
	mProvidersChoose = systray.AddMenuItem("Choose Providers Config…", "Select a providers JSON or YAML file")
	mProvidersClear = systray.AddMenuItem("Use Default Copilot Routing", "Clear custom providers config")
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

	refreshSessionUI()
	if providersConfigErr != nil {
		title, message := providersConfigUnavailableDialog(providersConfigErr)
		showErrorDialog(title, fmt.Sprintf("%s\n\n%v", message, providersConfigErr))
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
			case <-mProvidersChoose.ClickedCh:
				selectProvidersConfig()
			case <-mProvidersClear.ClickedCh:
				clearProvidersConfig()
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
			case <-mSignInGitHub.ClickedCh:
				go signInWithGitHub()
			case <-mUseGitHubCLI.ClickedCh:
				go signInWithGitHubCLI()
			case <-mSignOut.ClickedCh:
				signOut()
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
	if providersConfigErr != nil {
		title, message := providersConfigStartDialog(providersConfigErr)
		showErrorDialog(title, fmt.Sprintf("%s\n\n%v", message, providersConfigErr))
		return
	}

	if providersRequireGitHubAuth(providersCfg, providersConfigErr) {
		if _, err := authenticator.GetToken(context.Background()); err != nil {
			log.Error("auth failed", logger.Err(err))
			showErrorDialog(
				"GitHub Sign In Required",
				fmt.Sprintf("The active providers config uses GitHub Copilot, but Vekil could not refresh authentication.\n\nOpen ‘GitHub Auth’ and choose ‘Sign In with GitHub’ or ‘Use GitHub CLI Account’, then start Vekil again.\n\n%v", err),
			)
			refreshSessionUI()
			return
		}
	}

	nextSrv, err := server.New(
		authenticator,
		log,
		"0.0.0.0",
		"1337",
		server.WithProxyOptions(proxy.WithProvidersConfig(providersCfg)),
	)
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

// signInWithGitHub drives the interactive GitHub device-code flow via native macOS
// dialogs. It is expected to be called in its own goroutine.
func signInWithGitHub() {
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

	setAuthActionsEnabled(false)
	mAuthMenu.SetTitle("GitHub Auth: Signing in with GitHub…")

	dcResp, err := authenticator.RequestDeviceCode(ctx)
	if err != nil {
		log.Error("device code request failed", logger.Err(err))
		showErrorDialog("Sign In Failed", fmt.Sprintf("Could not start sign-in: %v", err))
		refreshSessionUI()
		setAuthActionsEnabled(true)
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
		refreshSessionUI()
		setAuthActionsEnabled(true)
		return
	}

	openURL(dcResp.VerificationURI)
	mAuthMenu.SetTitle("GitHub Auth: Waiting for authorization…")

	if err := authenticator.PollForAuthorization(ctx, dcResp); err != nil {
		log.Error("authorization failed", logger.Err(err))
		if ctx.Err() == nil {
			// Only show error dialog if we weren't cancelled.
			showErrorDialog("Sign In Failed", fmt.Sprintf("Authorization failed: %v", err))
		}
		refreshSessionUI()
		setAuthActionsEnabled(true)
		return
	}

	refreshSessionUI()
	showNotification("Vekil", "Successfully signed in to GitHub.")
	log.Info("sign-in complete")
}

// signInWithGitHubCLI signs in using the currently authenticated GitHub CLI account.
// It is expected to be called in its own goroutine.
func signInWithGitHubCLI() {
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

	setAuthActionsEnabled(false)
	mAuthMenu.SetTitle("GitHub Auth: Signing in with GitHub CLI…")

	if err := authenticator.SignInWithGitHubCLI(ctx); err != nil {
		log.Error("github cli sign-in failed", logger.Err(err))
		if ctx.Err() == nil {
			showErrorDialog("GitHub CLI Sign In Failed", fmt.Sprintf("Could not sign in with GitHub CLI. Make sure gh is installed, authenticated with GitHub, and using an account with Copilot access.\n\n%v", err))
		}
		refreshSessionUI()
		setAuthActionsEnabled(true)
		return
	}

	refreshSessionUI()
	showNotification("Vekil", "Successfully signed in with GitHub CLI.")
	log.Info("github cli sign-in complete")
}

// signOut clears Vekil credentials and stops the proxy only when active
// providers require GitHub Copilot authentication.
func signOut() {
	// Cancel any in-progress sign-in.
	signInMu.Lock()
	if signInCancel != nil {
		signInCancel()
	}
	signInMu.Unlock()

	if providersRequireGitHubAuth(providersCfg, providersConfigErr) && srv != nil && srv.IsRunning() {
		stopProxy()
	}

	if err := authenticator.SignOut(); err != nil {
		log.Error("sign-out error", logger.Err(err))
	}

	refreshSessionUI()
	log.Info("signed out")
}

func selectProvidersConfig() {
	path, err := chooseProvidersConfigPath()
	if err != nil {
		if errors.Is(err, errDialogCanceled) {
			return
		}
		log.Error("providers config selection failed", logger.Err(err))
		showErrorDialog("Providers Config", fmt.Sprintf("Could not open the providers config picker.\n\n%v", err))
		return
	}

	if err := applyProvidersConfigPath(path); err != nil {
		log.Error("failed to apply providers config", logger.Err(err), logger.F("path", path))
		showErrorDialog("Providers Config", fmt.Sprintf("Could not use %s.\n\n%v", filepath.Base(path), err))
	}
}

func clearProvidersConfig() {
	if err := applyProvidersConfigPath(""); err != nil {
		log.Error("failed to clear providers config", logger.Err(err))
		showErrorDialog("Providers Config", fmt.Sprintf("Could not clear the saved providers config.\n\n%v", err))
	}
}

func applyProvidersConfigPath(path string) error {
	nextCfg := menubarConfig{ProvidersConfigPath: path}
	loadedProvidersCfg, err := proxy.LoadProvidersConfigFile(path)
	if err != nil {
		return err
	}
	if err := saveMenubarConfig(nextCfg); err != nil {
		return err
	}

	menubarCfg = nextCfg
	providersCfg = loadedProvidersCfg
	providersConfigErr = nil

	wasRunning := srv != nil && srv.IsRunning()
	if wasRunning {
		stopProxy()
	}

	refreshSessionUI()

	if wasRunning {
		startProxy()
	}

	return nil
}

func refreshSessionUI() {
	refreshProvidersMenu()

	status := auth.AuthStatus{Source: auth.AuthSourceNone}
	if authenticator != nil {
		status = authenticator.Status()
	}
	refreshAuthMenu(status)

	running := srv != nil && srv.IsRunning()
	switch {
	case providersConfigErr != nil:
		mToggle.Disable()
		if !running {
			mToggle.SetTitle("Start Vekil")
			systray.SetIcon(iconOff)
			systray.SetTooltip("Vekil - Stopped")
		}
	case !providersRequireGitHubAuth(providersCfg, providersConfigErr):
		mToggle.Enable()
		if !running {
			mToggle.SetTitle("Start Vekil")
			systray.SetIcon(iconOff)
			systray.SetTooltip("Vekil - Stopped")
		}
	case status.SignedIn:
		mToggle.Enable()
		if !running {
			mToggle.SetTitle("Start Vekil")
			systray.SetIcon(iconOff)
			systray.SetTooltip("Vekil - Stopped")
		}
	default:
		mToggle.Disable()
		if !running {
			mToggle.SetTitle("Start Vekil")
			systray.SetIcon(iconOff)
			systray.SetTooltip("Vekil - Stopped")
		}
	}
}

func refreshAuthMenu(status auth.AuthStatus) {
	if mAuthMenu != nil {
		mAuthMenu.SetTitle(authMenuTitle(status))
	}
	if mSignInGitHub != nil {
		mSignInGitHub.Enable()
	}
	if mUseGitHubCLI != nil {
		mUseGitHubCLI.Enable()
	}
	if mSignOut != nil {
		if status.SignedIn {
			mSignOut.Enable()
		} else {
			mSignOut.Disable()
		}
	}
}

func authMenuTitle(status auth.AuthStatus) string {
	if status.SignedIn {
		switch status.Source {
		case auth.AuthSourceEnv:
			return "GitHub Auth: Environment Token"
		case auth.AuthSourceVekil:
			return "GitHub Auth: Signed in with GitHub"
		case auth.AuthSourceGitHubCLI:
			return "GitHub Auth: Using GitHub CLI Account"
		default:
			return "GitHub Auth: Signed In"
		}
	}
	if status.SignedOut {
		return "GitHub Auth: Signed Out"
	}
	return "GitHub Auth: Not Signed In"
}

func setAuthActionsEnabled(enabled bool) {
	if !enabled {
		if mSignInGitHub != nil {
			mSignInGitHub.Disable()
		}
		if mUseGitHubCLI != nil {
			mUseGitHubCLI.Disable()
		}
		if mSignOut != nil {
			mSignOut.Disable()
		}
		return
	}

	status := auth.AuthStatus{Source: auth.AuthSourceNone}
	if authenticator != nil {
		status = authenticator.Status()
	}
	refreshAuthMenu(status)
}

func refreshProvidersMenu() {
	mProvidersStatus.SetTitle(providersMenuTitle())
	if menubarCfg.ProvidersConfigPath == "" {
		mProvidersClear.Disable()
		return
	}
	mProvidersClear.Enable()
}

func providersMenuTitle() string {
	switch {
	case isMenubarConfigLoadError(providersConfigErr):
		return "Providers: Config unavailable"
	case providersConfigErr != nil && menubarCfg.ProvidersConfigPath != "":
		return fmt.Sprintf("Providers: Invalid (%s)", filepath.Base(menubarCfg.ProvidersConfigPath))
	case providersConfigErr != nil:
		return "Providers: Invalid"
	case menubarCfg.ProvidersConfigPath == "":
		return "Providers: Copilot default"
	default:
		return fmt.Sprintf("Providers: %s", filepath.Base(menubarCfg.ProvidersConfigPath))
	}
}

func logProvidersConfigLoadError(err error) {
	if isMenubarConfigLoadError(err) {
		log.Error("failed to load menubar config", logger.Err(err))
		return
	}
	log.Error("failed to load providers config", logger.Err(err), logger.F("path", menubarCfg.ProvidersConfigPath))
}

func providersConfigUnavailableDialog(err error) (string, string) {
	if isMenubarConfigLoadError(err) {
		return "Menubar Config Unavailable", "Could not load the saved menubar config."
	}
	return "Providers Config Unavailable", "Could not load the saved providers config."
}

func providersConfigStartDialog(err error) (string, string) {
	if isMenubarConfigLoadError(err) {
		return "Menubar Config Unavailable", "Could not load the saved menubar config."
	}
	return "Invalid Providers Config", "Could not load the selected providers config."
}

func providersConfigStatusTitle(err error) string {
	if isMenubarConfigLoadError(err) {
		return "⚠ Config unavailable"
	}
	return "⚠ Invalid providers config"
}

func providersRequireGitHubAuth(cfg proxy.ProvidersConfig, err error) bool {
	return err == nil && cfg.UsesCopilot()
}

func onExit() {
	if srv != nil && srv.IsRunning() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Stop(ctx)
	}
}
