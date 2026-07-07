# Windows Tray App

Vekil includes a Windows tray build of the same local proxy controller used by the macOS/Linux tray app. It is intended for users who want to start, stop, authenticate, and monitor Vekil without keeping a terminal open.

## Build

From the repository root:

```bash
make build-windows-tray
```

This produces `VekilTray.exe`. The binary is built as a Windows GUI application (`-H=windowsgui`) so it does not open a console window when launched from Explorer or at sign-in.

## Use

1. Launch `VekilTray.exe`.
2. Open the tray icon menu.
3. Use **GitHub Auth → Sign In with GitHub** or **Use GitHub CLI Account** for Copilot-backed routing.
4. Choose **Start Vekil** to run the local proxy on `127.0.0.1:1337`.
5. Choose **Open Dashboard** to open `http://127.0.0.1:1337/dashboard`.

Provider configs work the same as the CLI and macOS tray app. Use **Choose Providers Config…** to select a JSON/YAML file, or **Use Default Copilot Routing** to return to zero-config Copilot routing.

## Launch at sign-in

The **Launch at Login** menu item writes the current executable path to:

```text
HKCU\Software\Microsoft\Windows\CurrentVersion\Run\Vekil
```

Disabling the menu item removes that value. No administrator privileges are required.

## Authentication storage

Vekil-managed credentials use Windows Credential Manager through the system keyring by default. Set `VEKIL_SECRET_STORE=file` only for test/headless environments that intentionally want legacy token files under `--token-dir`.

## Packaging status

The first Windows tray build is a portable `.exe`. An installer, code signing, and auto-update are intentionally left for a later release pipeline iteration.
