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

- start/stop the proxy
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

Dialogs, notifications, and sign-in use DBus directly when possible. Optional helpers improve desktop integration:

| Feature | Packages |
|---------|----------|
| Dialogs | `zenity` or `kdialog` for richer dialogs |
| Clipboard | `wl-clipboard`, `xclip`, or `xsel` |
| Open URLs | `xdg-open` |
| Notifications | DBus notification daemon; `notify-send` fallback |
