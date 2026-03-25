# macOS Menubar App

The repo includes a native macOS menubar app for running the proxy without keeping a terminal open.

## Build And Run

```bash
make build-app
open "Copilot Proxy.app"
```

## Features

- start/stop toggle from the menubar
- status icon: white robot when running, gray when stopped
- current app version shown in the menu
- optional LaunchAgent integration for launch at login
- tooltip showing running/stopped state and port
