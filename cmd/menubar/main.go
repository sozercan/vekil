package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"fyne.io/systray"
	"github.com/sozercan/vekil/auth"
	"github.com/sozercan/vekil/logger"
	"github.com/sozercan/vekil/proxy"
	"github.com/sozercan/vekil/server"
)

var log = logger.New(logger.ParseLevel("info"))

const (
	proxyHost = "127.0.0.1"
	proxyPort = "1337"
)

func dashboardURL() string {
	return fmt.Sprintf("http://%s:%s/dashboard", proxyHost, proxyPort)
}

var (
	proxyLifecycle menubarProxyLifecycle
	authenticator  *auth.Authenticator

	// Menu items kept at package level so helpers can update them.
	mAuthMenu        *systray.MenuItem
	mToggle          *systray.MenuItem
	mDashboard       *systray.MenuItem
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
	mDashboard = systray.AddMenuItem("Open Dashboard", "Open the live traffic dashboard in your browser")
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
				if cancelProxyStartup() {
					continue
				}
				if proxyLifecycle.isRunning() {
					stopProxy()
				} else {
					startProxy()
				}
			case <-mDashboard.ClickedCh:
				openDashboard()
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
				_ = cancelProxyStartup()
				if proxyLifecycle.isRunning() {
					stopProxy()
				}
				systray.Quit()
				return
			}
		}
	}()
}

type proxyStartResult struct {
	server     menubarProxyServer
	err        error
	logMessage string
	title      string
	message    string
}

func startProxy() {
	ctx, generation, ok := proxyLifecycle.beginStartup(context.Background())
	if !ok {
		return
	}

	setProxyStartingUI()
	cfg := providersCfg
	configErr := providersConfigErr
	authn := authenticator
	go func() {
		completeProxyStartup(generation, runProxyStartup(ctx, authn, cfg, configErr))
	}()
}

// menubarPolicyRoutingMode follows the providers YAML unless a non-empty
// process override was explicitly supplied.
func menubarPolicyRoutingMode() (proxy.PolicyRoutingMode, error) {
	value, ok := os.LookupEnv("POLICY_ROUTING_MODE")
	if !ok || strings.TrimSpace(value) == "" {
		return proxy.PolicyRoutingModeConfig, nil
	}
	return proxy.ParsePolicyRoutingMode(value)
}

func runProxyStartup(
	ctx context.Context,
	authn *auth.Authenticator,
	cfg proxy.ProvidersConfig,
	configErr error,
) proxyStartResult {
	if configErr != nil {
		title, message := providersConfigStartDialog(configErr)
		return proxyStartFailure(
			"providers config unavailable",
			title,
			fmt.Sprintf("%s\n\n%v", message, configErr),
			configErr,
		)
	}

	if providersRequireGitHubAuth(cfg, configErr) {
		if _, err := authn.GetToken(ctx); err != nil {
			if ctx.Err() != nil {
				return proxyStartResult{err: ctx.Err()}
			}
			return proxyStartFailure(
				"auth failed",
				"GitHub Sign In Required",
				fmt.Sprintf("The active providers config uses GitHub Copilot, but Vekil could not refresh authentication.\n\nOpen ‘GitHub Auth’ and choose ‘Sign In with GitHub’ or ‘Use GitHub CLI Account’, then start Vekil again.\n\n%v", err),
				err,
			)
		}
	}

	if err := ctx.Err(); err != nil {
		return proxyStartResult{err: err}
	}

	policyMode, err := menubarPolicyRoutingMode()
	if err != nil {
		return proxyStartFailure(
			"invalid policy routing mode",
			"Vekil Start Failed",
			fmt.Sprintf("Invalid POLICY_ROUTING_MODE.\n\n%v", err),
			err,
		)
	}
	nextSrv, err := server.New(
		authn,
		log,
		proxyHost,
		proxyPort,
		server.WithProxyOptions(
			proxy.WithProvidersConfig(cfg),
			proxy.WithPolicyRoutingMode(policyMode),
		),
	)
	if err != nil {
		return proxyStartFailure(
			"server init failed",
			"Vekil Start Failed",
			fmt.Sprintf("Could not initialize Vekil.\n\n%v", err),
			err,
		)
	}
	if err := nextSrv.Start(); err != nil {
		return proxyStartFailure(
			"server start failed",
			"Vekil Start Failed",
			fmt.Sprintf("Could not start Vekil on port 1337.\n\n%v", err),
			err,
		)
	}

	// Each classifier route already has its own configured timeout. The startup
	// worker keeps the aggregate operation cancellable without imposing a second,
	// shorter deadline over a sequence of otherwise healthy routes.
	if err := initializeProxyPolicyRouting(ctx, nextSrv); err != nil {
		if ctx.Err() != nil {
			return proxyStartResult{err: ctx.Err()}
		}
		return proxyStartFailure(
			"policy routing initialization failed",
			"Vekil Start Failed",
			fmt.Sprintf("Policy routing preflight failed.\n\n%v", err),
			err,
		)
	}

	return proxyStartResult{server: nextSrv}
}

func initializeProxyPolicyRouting(ctx context.Context, current menubarProxyServer) error {
	err := current.InitializePolicyRouting(ctx)
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		_ = stopMenubarProxyServer(current, 10*time.Second)
	}
	return err
}

func proxyStartFailure(logMessage, title, message string, err error) proxyStartResult {
	return proxyStartResult{
		err:        err,
		logMessage: logMessage,
		title:      title,
		message:    message,
	}
}

func completeProxyStartup(generation uint64, result proxyStartResult) {
	completion, restart := proxyLifecycle.finishStartup(generation, result.server)
	if completion != proxyStartupCurrent {
		if result.server != nil {
			_ = stopMenubarProxyServer(result.server, 10*time.Second)
		}
		if completion == proxyStartupCanceled {
			refreshSessionUI()
			log.Info("proxy startup canceled")
			if restart {
				startProxy()
			}
		}
		return
	}

	if result.err != nil {
		refreshSessionUI()
		if !errors.Is(result.err, context.Canceled) {
			log.Error(result.logMessage, logger.Err(result.err))
			showErrorDialog(result.title, result.message)
		}
		return
	}

	mToggle.SetTitle("Stop Vekil")
	systray.SetIcon(iconOn)
	systray.SetTooltip("Vekil - Running on :1337")
	refreshSessionUI()
	log.Info("proxy started")
}

func cancelProxyStartup() bool {
	return cancelProxyStartupWithRestart(false)
}

func cancelProxyStartupWithRestart(restart bool) bool {
	if !proxyLifecycle.cancelStartup(restart) {
		return false
	}
	if mToggle != nil {
		mToggle.SetTitle("Stopping Vekil…")
		mToggle.Disable()
	}
	if mDashboard != nil {
		mDashboard.Disable()
	}
	return true
}

func setProxyStartingUI() {
	mToggle.SetTitle("Cancel Starting Vekil")
	mToggle.Enable()
	systray.SetIcon(iconOff)
	systray.SetTooltip("Vekil - Starting")
	if mDashboard != nil {
		mDashboard.Disable()
	}
	if mProvidersChoose != nil {
		mProvidersChoose.Disable()
	}
	if mProvidersClear != nil {
		mProvidersClear.Disable()
	}
	setAuthActionsEnabled(false)
}

func stopProxy() {
	if cancelProxyStartup() {
		return
	}

	current := proxyLifecycle.detachServer()
	if current == nil {
		refreshSessionUI()
		return
	}
	if err := stopMenubarProxyServer(current, 10*time.Second); err != nil {
		log.Error("server stop failed", logger.Err(err))
	}

	refreshSessionUI()
	log.Info("proxy stopped")
}

func stopMenubarProxyServer(current menubarProxyServer, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return current.Stop(ctx)
}

// openDashboard opens the live traffic dashboard in the default browser. It is a
// convenience shortcut; the dashboard is served by the proxy itself and is also
// reachable directly at the dashboard URL.
func openDashboard() {
	if !proxyLifecycle.isRunning() {
		showErrorDialog("Vekil Not Running", "Start Vekil before opening the dashboard.")
		return
	}
	openURL(dashboardURL())
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

	if providersRequireGitHubAuth(providersCfg, providersConfigErr) {
		_ = cancelProxyStartup()
		if proxyLifecycle.isRunning() {
			stopProxy()
		}
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

	_ = cancelProxyStartupWithRestart(true)
	wasRunning := proxyLifecycle.isRunning()
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
	status := auth.AuthStatus{Source: auth.AuthSourceNone}
	if authenticator != nil {
		status = authenticator.Status()
	}

	starting, canceling := proxyLifecycle.startupState()
	if starting {
		if mDashboard != nil {
			mDashboard.Disable()
		}
		if mProvidersChoose != nil {
			mProvidersChoose.Disable()
		}
		if mProvidersClear != nil {
			mProvidersClear.Disable()
		}
		setAuthActionsEnabled(false)
		if canceling {
			mToggle.SetTitle("Stopping Vekil…")
			mToggle.Disable()
			systray.SetTooltip("Vekil - Stopping")
		} else {
			mToggle.SetTitle("Cancel Starting Vekil")
			mToggle.Enable()
			systray.SetTooltip("Vekil - Starting")
		}
		return
	}

	refreshAuthMenu(status)
	refreshProvidersMenu()
	running := proxyLifecycle.isRunning()
	if mDashboard != nil {
		if running {
			mDashboard.Enable()
		} else {
			mDashboard.Disable()
		}
	}
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
	if mProvidersChoose != nil {
		mProvidersChoose.Enable()
	}
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
	if current := proxyLifecycle.shutdown(); current != nil && current.IsRunning() {
		_ = stopMenubarProxyServer(current, 5*time.Second)
	}
}
