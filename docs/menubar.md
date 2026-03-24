# macOS Menubar App

The repo includes a native macOS menubar app for running the proxy without keeping a terminal open. The packaged app also includes Sparkle-based update checks.

## Build And Run

```bash
make build-app
open "Copilot Proxy.app"
```

`make build-app` downloads Sparkle 2.9.0 into `.build/sparkle/`, builds the menubar app with the `sparkle` build tag, embeds `Sparkle.framework`, and ad-hoc signs the finished app bundle.

If `SPARKLE_PUBLIC_ED_KEY` is not set, the app still builds, but `Check for Updates…` is disabled because Sparkle cannot start without a public EdDSA key.

To build a locally update-enabled app:

```bash
SPARKLE_PUBLIC_ED_KEY=your_public_key make build-app
open "Copilot Proxy.app"
```

## Features

- start/stop toggle from the menubar
- status icon: white robot when running, gray when stopped
- optional LaunchAgent integration for launch at login
- tooltip showing running/stopped state and port
- `Check for Updates…` menu item in packaged macOS app builds

## Release Assets

The release workflow publishes two macOS updater assets:

- `Copilot-Proxy-macos-arm64.zip`
- `appcast.xml`

It signs the appcast with `SPARKLE_PRIVATE_ED_KEY`, so repository releases need both `SPARKLE_PUBLIC_ED_KEY` and `SPARKLE_PRIVATE_ED_KEY` secrets configured.
