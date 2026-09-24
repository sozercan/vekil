# macOS/Linux Tray App

The tray app runs Vekil without a terminal. It supports macOS and Linux; published macOS app bundles also include Sparkle update checks.

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

`make build-app` downloads Sparkle, builds with the `sparkle` tag, embeds `Sparkle.framework`, and ad-hoc signs the bundle. Without `SPARKLE_PUBLIC_ED_KEY`, the app still builds but disables `Check for Updates…`.

```bash
SPARKLE_PUBLIC_ED_KEY=your_public_key make build-app
```

Release asset and updater-secret details live in [Development](development.md#release).

## Menu Features

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

On macOS, the app adds directories from your interactive login shell's `PATH` at launch. This lets Azure CLI authentication and other configured commands work when the app starts through Finder or at login. The lookup imports only `PATH` and has a five-second timeout.

If shell startup fails or times out, the app appends `/opt/homebrew/bin` and `/usr/local/bin` as fallbacks and still logs the lookup warning. Shell recovery and fallback keep inherited directories first and only append missing absolute directories, never empty or relative entries. The app does not retry the shell lookup.

Use `Choose Providers Config…` to select the same JSON/YAML file you would pass with `--providers-config`. The app saves the selected path for future launches and launch-at-login starts. `Use Default Copilot Routing` clears the saved path.

Every **Start Vekil** reads the saved path and the providers file again, and fetches a remote URL again. To apply an edited file, choose **Stop Vekil** and then **Start Vekil**. You don't need to quit the app. If the edited file is invalid, Start shows the error and stays enabled, so you can fix the file and start again.

The top-level `state_bindings` configuration applies to both the CLI and tray
app. Schema-v2 explicit routes default to durable ownership with capacity
8,388,608; `mode: memory` opts out. The same private OS application-data file is
used when `file` is omitted, so a drained CLI can hand ownership recovery to the
tray and vice versa. No separate tray setting is needed. Concurrent clients can
share one running proxy. A second proxy cannot open its locked store; use a
distinct configured file for a separate instance. The dashboard shows retained
entries, capacity, database size, and capacity warnings. See
[State Recovery](state-recovery.md) for macOS/Linux storage and recovery details.

Providers-config loading, startup authentication, and semantic-policy classifier preflight run in a cancellable worker. Provider-config and authentication actions are temporarily disabled while that worker is active, while Quit and the other tray events remain responsive. Use **Cancel Starting Vekil** to abort startup; Vekil closes any listener opened by the canceled attempt before an automatic config restart can begin.

## Linux Tray

The same tray app runs on Linux using the DBus StatusNotifierItem protocol, supported by Waybar, KDE Plasma, GNOME with the AppIndicator extension, and others.

```bash
make build-tray-linux
./vekil-tray
```

Cross-compile by setting `GOARCH`, for example:

```bash
GOARCH=arm64 make build-tray-linux
```

Linux supports the same start/stop, status, auth, provider-config, and XDG autostart features as macOS. `Check for Updates…` is macOS-only.

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
