# macOS Menubar App

The repo includes a native macOS menubar app for running the proxy without keeping a terminal open.

## Download And Run

Download `vekil-macos-arm64.zip` from [GitHub Releases](https://github.com/sozercan/vekil/releases/latest), unzip it, and open `Vekil.app`.

The published app bundle is currently available for Apple Silicon (`arm64`). On Intel Macs, build the app from source instead.

## Build From Source

```bash
make build-app
open "Vekil.app"
```

## Features

- start/stop toggle from the menubar
- status icon: white robot when running, gray when stopped
- current app version shown in the menu
- optional LaunchAgent integration for launch at login
- tooltip showing running/stopped state and port
