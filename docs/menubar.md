# macOS App and Linux Tray

The tray app runs Vekil without a terminal. It supports macOS and Linux; published macOS app bundles also include Sparkle update checks.

## Implementation And Release Status

The currently published `Vekil.app` is the Go/systray shell in `cmd/menubar`. It runs the proxy in the same process, uses the legacy LaunchAgent integration for launch at login, opens the browser dashboard, and is packaged as an ad-hoc-signed Apple Silicon app.

The current source tree implements the native macOS 13 shell and its build-once pipeline: AppKit/SwiftUI application and status menu, singleton/window lifecycle, Swift `RuntimeController`, versioned JSONL Go helper, External Configuration switching, login-item migration, Keychain support services, native analytics, universal packaging, Sparkle 2.9.4 appcast tooling, and fail-closed release gates. This is implemented and locally testable source, not yet the published replacement. Production Developer ID signing/notarization, real N-1 Sparkle replacement, cross-version Keychain continuity, exact-artifact Homebrew installation, and forward-revert recovery remain external [release gates](development.md#native-macos-shell-and-release-gates).

## macOS Install

```bash
brew install --cask sozercan/repo/vekil
```

The app is not Developer ID signed. If macOS quarantines it, run:

```bash
xattr -cr /Applications/Vekil.app
```

You can also download `vekil-macos-arm64.zip` from [GitHub Releases](https://github.com/sozercan/vekil/releases/latest), unzip it, and open `Vekil.app`. Published app bundles are currently Apple Silicon (`arm64`) only; build from source on Intel Macs.

## Build From Source

```bash
make build-app
open "Vekil.app"
```

Quit any installed Vekil app before opening the source build. The published and native bundles both use `com.vekil.menubar`, so macOS otherwise activates the already-running installed app instead of launching the repository bundle.

`make build-app` builds the Swift product and Go helper for `arm64` and `x86_64`, combines both slices, embeds checksum-verified Sparkle 2.9.4, writes the shared release manifest/build ID, and ad-hoc signs the local development bundle. The existing Sparkle feed URL and public key come from `build-support/macos/app-config.json`.

Validate source and the assembled app with:

```bash
go test ./internal/appcontrol ./internal/macosruntime ./cmd/macos-runtime -count=1
swift test --package-path mac/VekilApp
make test-app
MACOS_SMOKE_ARCHES="arm64 x86_64" make smoke-app
```

`make build-legacy-app` retains the old Go/systray shell for forward-revert qualification; it is not the native release artifact. Production signing/notarization and exact-artifact promotion details live in [Development](development.md#native-macos-shell-and-release-gates).

## Published Menu Features

- start/stop the proxy; while authentication or policy preflight is running, the item changes to **Cancel Starting Vekil** so startup can be aborted without blocking Quit or other tray events
- status icon and tooltip for running/stopped state
- **Open Dashboard** — opens the live [traffic dashboard](dashboard.md) in your browser (enabled while Vekil is running)
- current app version
- choose and persist a providers config file
- return to default Copilot routing
- GitHub auth actions: sign in, use GitHub CLI account, sign out
- optional launch-at-login integration
- Sparkle `Check for Updates…` in packaged macOS builds

## Authentication And Providers

The `GitHub Auth` submenu exposes the same auth choices as the CLI:

- `Sign In with GitHub` uses Vekil-managed browser/device-code auth.
- `Use GitHub CLI Account` opts into the account from `gh auth login` and keeps the token in memory only.
- `Sign Out` clears Vekil's cached credentials and disables silent GitHub CLI reuse until you opt back in.

Copilot-backed configs require GitHub auth or `COPILOT_GITHUB_TOKEN`. Provider-only configs that omit Copilot do not require GitHub auth and can keep running after sign-out. See [Provider Routing](provider-routing.md) for provider-specific auth details.

Use `Choose Providers Config…` to select the same JSON/YAML file you would pass with `--providers-config`. The app saves the selected path for future launches and launch-at-login starts. `Use Default Copilot Routing` clears the saved path.

Startup authentication and semantic-policy classifier preflight run in a cancellable worker. Provider-config and authentication actions are temporarily disabled while that worker is active, while Quit and the other tray events remain responsive. Use **Cancel Starting Vekil** to abort startup; Vekil closes any listener opened by the canceled attempt before an automatic config restart can begin.

## Native Setup Assistant

On a true first run, the native source opens a guided setup assistant after the helper and recovered runtime state have initialized. A missing onboarding preference alone does not prove that an installation is new: an upgrade that recovers a signed-in account, a selected configuration, a running service, saved window/navigation state, or startup preferences records the current onboarding version as complete and continues without interrupting the user. The assistant can always be opened again with **Settings > Run Setup Assistant…**.

The assistant has five stable stages:

1. **Providers** — choose GitHub Copilot, OpenAI Codex, Azure OpenAI, OpenAI-compatible, Anthropic-compatible, or a multi-provider import. Copilot uses the built-in zero-config path. Other cards select an existing JSON/YAML configuration, which remains user-owned and can include several providers. GitHub authentication appears inline only when the selected configuration requires it.
2. **Models** — start the proxy, follow its real provider-validation phases, and review the discovered public catalog. Public model IDs are global and collisions fail startup rather than silently shadowing a provider.
3. **Verify** — confirm the authoritative configuration, authentication, readiness, endpoint, and model-catalog state.
4. **Client** — copy the local endpoint and a temporary Claude Code, Codex, GitHub Copilot CLI, or OpenAI-compatible setup command. The assistant does not edit client configuration.
5. **Ready** — review the source, authentication, proxy, and endpoint status, then optionally enable **Open Vekil at login** and **Start proxy when Vekil launches** independently.

**Skip Setup** closes the assistant and records the current onboarding version as handled; it does not repeatedly reopen on subsequent launches. Rerunning the assistant does not clear that completion state or change the active proxy until the user confirms an action.

Provider cards that need credentials use the existing configuration-file boundary. Vekil-managed provider-key entry remains gated on signed cross-version Keychain continuity; zero-config Copilot, Codex CLI file auth, and existing external configurations remain available meanwhile.

## Native Shell Behavior

The implemented native source keeps the proxy app-owned: the Swift shell owns one helper process and both pipes, and quitting Vekil shuts down the helper and listener. It does not install a persistent proxy daemon.

- **Open at Login** and **Start proxy when the app launches** are separate preferences and default off.
- Auto-start is evaluated once after helper/configuration initialization. Reopen, **Open Vekil**, and second-launch activation do not evaluate it again.
- Cold, login, and update relaunches remain menu-only. **Open Vekil**, application reopen, and a second launch reveal the durable main window. Sparkle's automatic checks stay disabled (`SUEnableAutomaticChecks` is `false`), so no update prompt or update window appears at launch; update checks are user-initiated from **Check for Updates…**, which activates the app so Sparkle's windows appear in front.
- Clicking the status item opens a transient 340×330 SwiftUI popover. It prioritizes status and the lifecycle action (**Start Proxy**, **Cancel Starting**, **Stop Proxy**, or disabled **Stopping…**), then any actionable warning, the active endpoint with **Copy Base URL**, one compact current-run activity summary, and **Open Vekil**, **Settings…**, and **Quit Vekil** actions.
- The durable main window has six stable destinations: **Overview**, **Activity**, **Connection**, **Clients**, **Settings**, and **About**. Activity groups traffic summary and recent requests; Connection groups providers and models. Settings owns login/startup preferences and **Run Setup Assistant…**; About owns version and update information. Persisted destinations from the earlier grouped shell migrate to the corresponding arranged destination.
- **Open Vekil…** restores the previously selected destination; **Settings…** opens that same durable window directly on Settings.
- Background auto-start does not open an interactive device flow; missing Copilot credentials leave the proxy stopped in an authentication-required state.
- Zero-config Copilot remains available. A selected **External Configuration** stays user-owned and is never written or normalized by Swift.
- Managed configuration changes require explicit **Validate and Apply** and preserve prior running/stopped intent through the helper transaction.

The source includes Overview, Traffic, Requests, Providers, and Settings. These views are not current release behavior until a native bundle passes the release gates above; the published Go app continues to use **Open Dashboard** for traffic and request analytics.

## Linux Tray

The Linux tray is not replaced by the native macOS shell. It remains the Go `cmd/menubar` target and uses the DBus StatusNotifierItem protocol, supported by Waybar, KDE Plasma, GNOME with the AppIndicator extension, and others.

```bash
make build-tray-linux
./vekil-tray
```

Cross-compile by setting `GOARCH`, for example:

```bash
GOARCH=arm64 make build-tray-linux
```

Linux preserves the Go-shell start/stop, status, auth, provider-config, and XDG autostart features. Keychain, SwiftUI, and Sparkle behavior is macOS-only.

Dialogs, notifications, and sign-in use `xdg-desktop-portal` D-Bus interfaces (`FileChooser.OpenFile`, `OpenURI.OpenURI`, `Notification.AddNotification`) rather than subprocess dialog tools. A portal backend (`xdg-desktop-portal-gtk`, `xdg-desktop-portal-kde`, or similar, matched to your X11 or Wayland desktop) is only strictly required for `Choose Providers Config…`, which has no other picker to fall back to. Sign-in confirmation and error/plain notifications prefer the portal but fall back to the legacy `org.freedesktop.Notifications` interface when it is unavailable.

Portal notifications are non-modal and may be grouped or suppressed by the desktop; there is no portal-provided generic modal dialog. Sign-in confirmation uses a notification with approve/decline action buttons and deliberately no default action, so only an explicit click on the approve button ever approves; clicking the notification body, dismissing it, letting it time out, or any other outcome declines. One two-minute budget covers the portal attempt and any legacy fallback together, so sign-in confirmation never shows more than one prompt. Error alerts and plain notifications are informational, non-interactive notifications with no buttons.

Once a confirmation notification may have been displayed by either mechanism, a failure is never treated as reason to retry: the legacy fallback runs only when the portal attempt is provably certain to have never been shown (for example, the portal service is unreachable); any other failure, including an ambiguous delivery error where the notification may already be on screen, declines outright rather than risking a duplicate prompt.

Fallback behavior when no portal is available:

| Feature | Behavior without a portal |
|---------|----------------------------|
| Sign-in confirmation | Falls back to a legacy `org.freedesktop.Notifications` action prompt with approve/decline buttons on a plain notification daemon. If no notification mechanism responds at all, sign-in confirmation defaults to decline rather than proceeding unattended. |
| Error alerts, notifications | Falls back to a plain legacy `org.freedesktop.Notifications` notification (no action buttons, no wait) on a plain notification daemon. |
| Choose Providers Config… | No fallback; the menu action reports an actionable error naming `xdg-desktop-portal`. Pass `--providers-config` or edit the saved menubar config file directly instead. |
| Open URLs (Open Dashboard, sign-in link) | Falls back to `xdg-open` when the portal method call cannot be made, the response wait fails or times out, or the portal reports an outcome other than success or a deliberate user cancellation. Both Open Dashboard and sign-in run this bounded portal wait on their own goroutine rather than the tray's single menu-dispatch loop, so other menu clicks stay responsive while it is pending. |

Clipboard support is unchanged and does not use the portal (the portal's clipboard interface requires an active RemoteDesktop/InputCapture session, not a standalone API): `wl-clipboard`, `xclip`, or `xsel`.
