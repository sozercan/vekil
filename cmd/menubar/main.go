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
	"github.com/sozercan/vekil/aikit"
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

	// providersStateMu guards the saved providers selection and its most recent
	// load. Each start reloads them from its worker while the menu loop reads them.
	providersStateMu   sync.Mutex
	menubarCfg         menubarConfig
	providersCfg       proxy.ProvidersConfig
	providersConfigErr error
)

func main() {
	if err := initializeMenubarPATH(); err != nil {
		log.Warn("could not fully recover menubar PATH", logger.Err(err))
	}

	var err error
	authenticator, err = auth.NewAuthenticator("")
	if err != nil {
		log.Fatal("failed to initialize authenticator", logger.Err(err))
	}
	authenticator.DisableAutoDeviceFlow = true

	menubarCfg, providersCfg, providersConfigErr = loadProvidersConfigForMenubar(context.Background())
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
				go openDashboard()
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
	ctx, generation, stopPrevious, ok := proxyLifecycle.beginStartup(context.Background())
	if !ok {
		return
	}

	setProxyStartingUI()
	authn := authenticator
	go func() {
		if err := stopPrevious(); err != nil {
			log.Warn("failed to stop the previous proxy; its local model containers may still be running", logger.Err(err))
		}
		// Reload on every start so edits to the saved providers config apply
		// after Stop and Start without relaunching the app.
		cfg, configErr := reloadProvidersState(ctx)
		completeProxyStartup(generation, runProxyStartup(ctx, authn, cfg, configErr))
	}()
}

// reloadProvidersState re-reads the saved menubar config and the providers
// config it selects, then publishes the result for the menu. A canceled reload
// says nothing about the file, so it leaves the published state unchanged.
func reloadProvidersState(ctx context.Context) (proxy.ProvidersConfig, error) {
	cfg, loadedProvidersCfg, err := loadProvidersConfigForMenubar(ctx)
	if ctx.Err() == nil {
		setProvidersState(cfg, loadedProvidersCfg, err)
	}
	return loadedProvidersCfg, err
}

func providersState() (menubarConfig, proxy.ProvidersConfig, error) {
	providersStateMu.Lock()
	defer providersStateMu.Unlock()
	return menubarCfg, providersCfg, providersConfigErr
}

func setProvidersState(cfg menubarConfig, loadedProvidersCfg proxy.ProvidersConfig, err error) {
	providersStateMu.Lock()
	defer providersStateMu.Unlock()
	menubarCfg = cfg
	providersCfg = loadedProvidersCfg
	providersConfigErr = err
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
	return runProxyStartupAt(ctx, authn, cfg, configErr, proxyHost, proxyPort)
}

func runProxyStartupAt(ctx context.Context, authn *auth.Authenticator, cfg proxy.ProvidersConfig, configErr error, host, port string) proxyStartResult {
	if configErr != nil {
		title, message := providersConfigStartDialog(configErr)
		return proxyStartFailure(
			"providers config unavailable",
			title,
			fmt.Sprintf("%s\n\n%v", message, configErr),
			configErr,
		)
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
	stateBindings, err := proxy.StateBindingsEnvironmentOverrides()
	if err != nil {
		return proxyStartFailure("invalid state bindings configuration", "Vekil Start Failed", fmt.Sprintf("Invalid state bindings override.\n\n%v", err), err)
	}
	// AIKit containers must be running before the server routes to them.
	cfg, aikitGroup, err := aikit.StartProviders(ctx, cfg, aikit.ProviderStartOptions{
		Progress:    &aikitStartupLog{},
		Environment: os.Environ(),
	})
	if err != nil {
		if ctx.Err() != nil {
			return proxyStartResult{err: ctx.Err()}
		}
		return proxyStartFailure(
			"aikit start failed",
			"Vekil Start Failed",
			fmt.Sprintf("Could not start the AIKit model container.\n\n%v", err),
			err,
		)
	}
	closeAIKit := func() {
		closeCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = aikitGroup.Close(closeCtx)
	}
	nextSrv, err := server.New(
		authn,
		log,
		host,
		port,
		server.WithProxyOptions(
			proxy.WithProvidersConfig(cfg),
			proxy.WithStateBindingsConfig(stateBindings),
			proxy.WithPolicyRoutingMode(policyMode),
			proxy.WithDeferredDynamicProviderModelValidation(cfg.UsesCopilot()),
		),
	)
	if err != nil {
		closeAIKit()
		return proxyStartFailure(
			"server init failed",
			"Vekil Start Failed",
			fmt.Sprintf("Could not initialize Vekil.\n\n%v", err),
			err,
		)
	}
	var current menubarProxyServer = nextSrv
	if aikitGroup != nil {
		current = aikitProxyServer{menubarProxyServer: nextSrv, group: aikitGroup}
	}
	if nextSrv.UsesCopilot() {
		if _, err := authn.GetToken(ctx); err != nil {
			_ = stopMenubarProxyServer(current, 10*time.Second)
			if ctx.Err() != nil {
				return proxyStartResult{err: ctx.Err()}
			}
			return proxyStartFailure(
				"auth failed",
				"GitHub Sign In Required",
				fmt.Sprintf("The active provider scope uses GitHub Copilot, but Vekil could not refresh authentication.\n\nOpen ‘GitHub Auth’ and choose ‘Sign In with GitHub’ or ‘Use GitHub CLI Account’, then start Vekil again.\n\n%v", err),
				err,
			)
		}
	}
	if cfg.UsesCopilot() {
		if err := nextSrv.ValidateDynamicProviderModels(ctx); err != nil {
			_ = stopMenubarProxyServer(current, 10*time.Second)
			if ctx.Err() != nil {
				return proxyStartResult{err: ctx.Err()}
			}
			return proxyStartFailure(
				"provider model validation failed",
				"Vekil Start Failed",
				fmt.Sprintf("Provider model validation failed.\n\n%v", err),
				err,
			)
		}
	}
	if err := ctx.Err(); err != nil {
		_ = stopMenubarProxyServer(current, 10*time.Second)
		return proxyStartResult{err: err}
	}
	if err := nextSrv.Start(); err != nil {
		closeAIKit()
		return proxyStartFailure(
			"server start failed",
			"Vekil Start Failed",
			fmt.Sprintf("Could not start Vekil on port %s.\n\n%v", port, err),
			err,
		)
	}

	// Each classifier route already has its own configured timeout. The startup
	// worker keeps the aggregate operation cancellable without imposing a second,
	// shorter deadline over a sequence of otherwise healthy routes.
	if err := initializeProxyPolicyRouting(ctx, current); err != nil {
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

	return proxyStartResult{server: current}
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
// reachable directly at the dashboard URL. On Linux this waits on the portal's
// bounded, asynchronous OpenURI response (see portalOpenURITimeout), so it is
// expected to be called on its own goroutine rather than the tray's single
// menu-dispatch loop, the same as GitHub sign-in.
func openDashboard() {
	if !proxyLifecycle.isRunning() {
		showErrorDialog("Vekil Not Running", "Start Vekil before opening the dashboard.")
		return
	}
	openURL(dashboardURL())
}

// signInWithGitHub drives the interactive GitHub device-code flow via the
// platform's native confirmation prompt. It is expected to be called in its
// own goroutine.
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

	// The menu title shows the device code only while confirmation is
	// awaited: refreshSessionUI (called on every exit path below, including
	// a decline) immediately overwrites it. Only the clipboard copy survives
	// a decline or refresh; retrying sign-in requests a fresh code.
	mAuthMenu.SetTitle(fmt.Sprintf("GitHub Auth: Confirm code %s to continue", dcResp.UserCode))

	approved := confirmAction(ctx, confirmationPrompt{
		Title:        "Sign in to GitHub Copilot",
		Message:      fmt.Sprintf("Your code has been copied to the clipboard.\n\nEnter this code on GitHub:\n\n%s", dcResp.UserCode),
		ApproveLabel: "Open GitHub",
		DeclineLabel: "Cancel",
	})

	if !approved {
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

	_, currentProvidersCfg, currentProvidersErr := providersState()
	if providersRequireGitHubAuth(currentProvidersCfg, currentProvidersErr) {
		_ = cancelProxyStartup()
	}
	if proxyLifecycle.usesCopilot() && proxyLifecycle.isRunning() {
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
		log.Error("failed to apply providers config", logger.Err(err), logger.F("path", proxy.ProvidersConfigSourceDisplay(path)))
		showErrorDialog("Providers Config", fmt.Sprintf("Could not use %s.\n\n%v", providersConfigDisplayName(path), err))
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

	setProvidersState(nextCfg, loadedProvidersCfg, nil)

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
	// Start stays available with an invalid config because it reloads the
	// config, so fixing the file and starting again recovers.
	mToggle.Enable()
	if !running {
		mToggle.SetTitle("Start Vekil")
		systray.SetIcon(iconOff)
		systray.SetTooltip("Vekil - Stopped")
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
	cfg, _, _ := providersState()
	if cfg.ProvidersConfigPath == "" {
		mProvidersClear.Disable()
		return
	}
	mProvidersClear.Enable()
}

func providersMenuTitle() string {
	cfg, _, err := providersState()
	switch {
	case isMenubarConfigLoadError(err):
		return "Providers: Config unavailable"
	case err != nil && cfg.ProvidersConfigPath != "":
		return fmt.Sprintf("Providers: Invalid (%s)", providersConfigDisplayName(cfg.ProvidersConfigPath))
	case err != nil:
		return "Providers: Invalid"
	case cfg.ProvidersConfigPath == "":
		return "Providers: Copilot default"
	default:
		return fmt.Sprintf("Providers: %s", providersConfigDisplayName(cfg.ProvidersConfigPath))
	}
}

func providersConfigDisplayName(source string) string {
	return filepath.Base(proxy.ProvidersConfigSourceDisplay(source))
}

func logProvidersConfigLoadError(err error) {
	if isMenubarConfigLoadError(err) {
		log.Error("failed to load menubar config", logger.Err(err))
		return
	}
	log.Error("failed to load providers config", logger.Err(err), logger.F("path", proxy.ProvidersConfigSourceDisplay(menubarCfg.ProvidersConfigPath)))
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
	// Stop even a server that already exited: it may still own AIKit
	// containers, and Stop is idempotent.
	if current := proxyLifecycle.shutdown(); current != nil {
		_ = stopMenubarProxyServer(current, 5*time.Second)
	}
}
