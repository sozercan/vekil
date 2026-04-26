# macOS/Linux Tray App

The repo includes a tray app for running the proxy without keeping a terminal open. It supports macOS and Linux. Published macOS app bundles also include Sparkle-based update checks.

## Download And Run

Install the macOS tray app from Homebrew:

```bash
brew install --cask sozercan/repo/vekil
```

> **Note:** The app is not Developer ID signed.
> Clear extended attributes, including quarantine, with:
> ```bash
> xattr -cr /Applications/Vekil.app
> ```

Or download `vekil-macos-arm64.zip` from [GitHub Releases](https://github.com/sozercan/vekil/releases/latest), unzip it, and open `Vekil.app`.

The published app bundle is currently available for Apple Silicon (`arm64`). On Intel Macs, build the app from source instead.

## Build From Source

```bash
make build-app
open "Vekil.app"
```

`make build-app` downloads Sparkle 2.9.0 into `.build/sparkle/`, builds the macOS tray app with the `sparkle` build tag, embeds `Sparkle.framework`, and ad-hoc signs the finished app bundle.

If `SPARKLE_PUBLIC_ED_KEY` is not set, the app still builds, but `Check for Updates…` is disabled because Sparkle cannot start without a public EdDSA key.

To build a locally update-enabled app:

```bash
SPARKLE_PUBLIC_ED_KEY=your_public_key make build-app
open "Vekil.app"
```

## Features

- start/stop toggle from the tray menu
- status icon: white robot when running, gray when stopped
- current app version shown in the menu
- choose and persist a `providers-config` JSON or YAML file from the menu
- `GitHub Auth` submenu with current status and actions for `Sign In with GitHub`, `Use GitHub CLI Account`, and `Sign Out`
- optional LaunchAgent integration for launch at login
- tooltip showing running/stopped state and port
- `Check for Updates…` in packaged macOS app builds

## Authentication

The tray menu shows a `GitHub Auth` parent item whose title reports the current auth state. Its submenu contains explicit actions:

- `Sign In with GitHub` starts Vekil's browser/device-code flow and stores Vekil-managed credentials in `~/.config/vekil/`.
- `Use GitHub CLI Account` opts in to the account already authenticated by `gh auth login`. Vekil uses `gh auth token --hostname github.com` for Copilot access and keeps that token in memory only; it does not copy the GitHub CLI token into Vekil's `access-token` or `api-key.json` caches.
- `Sign Out` clears Vekil's cached credentials, disables GitHub CLI auto sign-in, and records a signed-out state. The app will not silently fall back to GitHub CLI again until you choose `Use GitHub CLI Account` or run `vekil login --github-cli` / `vekil login --gh`.

When the active providers config uses Copilot, start-up needs one of those GitHub auth sources or an explicit `COPILOT_GITHUB_TOKEN`. If auth is missing or expired, the app asks you to open `GitHub Auth` and choose `Sign In with GitHub` or `Use GitHub CLI Account` instead of starting a sign-in flow automatically.

Provider-only configs that omit Copilot do not require GitHub auth; the proxy can keep running after `Sign Out` in that mode.

## Providers Config

Use `Choose Providers Config…` from the tray menu to select the same JSON or YAML file you would pass to the CLI with `--providers-config`.

- The app saves the selected path in its local app config so it is reused on the next launch and when started at login.
- `Use Default Copilot Routing` clears the saved path and returns to zero-config startup, which currently uses the built-in Copilot provider.
- If the selected config does not include a Copilot provider, the app no longer requires GitHub sign-in before starting the proxy.
- Provider-specific extra state still comes from the normal locations, for example `~/.codex/auth.json` for `type: "openai-codex"`.

## Release Assets

The release workflow publishes two macOS updater assets:

- `vekil-macos-arm64.zip`
- `appcast.xml`

It signs the appcast with `SPARKLE_PRIVATE_ED_KEY`, so repository releases need both `SPARKLE_PUBLIC_ED_KEY` and `SPARKLE_PRIVATE_ED_KEY` secrets configured.

## Linux System Tray

The same tray app runs on Linux using the DBus StatusNotifierItem protocol (supported by Waybar, KDE Plasma, GNOME with the AppIndicator extension, and others).

### Build

```bash
make build-tray-linux
./vekil-tray
```

No CGO or external libraries are required. To cross-compile for a different architecture:

```bash
GOARCH=arm64 make build-tray-linux
```

### Features

Same as macOS:

- start/stop toggle from the tray
- status icon: white robot when running, gray when stopped
- current app version shown in the menu
- optional XDG autostart for launch at login (`~/.config/autostart/vekil.desktop`)
- tooltip showing running/stopped state and port

The `Check for Updates...` menu item is not available on Linux.

### Optional Dependencies

Dialogs, notifications, and the sign-in flow use DBus (`org.freedesktop.Notifications`) directly -- no external tools are required when a notification daemon is running (GNOME, KDE, dunst, mako, swaync, etc.). If `zenity` or `kdialog` is installed, those are preferred for richer dialog windows.

The clipboard and URL opening still require external tools:

| Feature | Packages |
|---------|----------|
| Dialogs | Built-in via DBus; optionally `zenity` (GTK) or `kdialog` (KDE) for richer UI |
| Clipboard | `wl-clipboard` (Wayland), `xclip`, or `xsel` (X11) |
| Open URLs | `xdg-open` (usually pre-installed via `xdg-utils`) |
| Notifications | Built-in via DBus; falls back to `notify-send` |
