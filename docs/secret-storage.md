# Secret Storage

Vekil stores authentication tokens (GitHub access tokens and Copilot API tokens) using a layered secret storage system that prefers the operating system's native secret store over plain-text files.

## Default Behavior

On startup, Vekil probes the system keyring:

| OS | Backend |
|----|---------|
| macOS | Keychain (via `security` CLI) |
| Linux | Secret Service over D-Bus (GNOME Keyring, KDE Wallet, etc.) |
| Windows | Windows Credential Manager |

If the probe succeeds, all tokens are stored in and retrieved from the keyring under the service name `vekil`. If the keyring is unavailable (headless servers, containers, CI), Vekil falls back to file-based storage in the token directory (`~/.config/vekil` by default) and logs a warning.

## Keys Stored

| Key | Content |
|-----|---------|
| `access-token` | GitHub OAuth access token |
| `copilot-token` | JSON-encoded Copilot API token with expiry |

## Forcing a Backend

Set the `VEKIL_SECRET_STORE` environment variable to override auto-detection:

| Value | Effect |
|-------|--------|
| `file` | Always use file-based storage regardless of keyring availability |
| `keyring` | Always use the keyring; fail hard if unavailable |
| _(empty/unset)_ | Auto-detect (default) |

Example:

```bash
VEKIL_SECRET_STORE=file vekil serve
```

## Migration from Older Versions

Previous Vekil versions stored tokens exclusively as plain-text files:

- `~/.config/vekil/access-token`
- `~/.config/vekil/api-key.json`

When the keyring backend is active and a token is successfully written to the keyring, Vekil automatically removes the corresponding plain-text file. The file-based backend reads from the legacy `api-key.json` filename if the new `copilot-token` file is not yet present, so upgrades are seamless in either direction.

No manual migration steps are required.

## CI and Containers

In environments without a desktop session or D-Bus secret service (Docker containers, CI runners, headless servers), Vekil automatically falls back to file-based storage. No configuration is needed. If you want to suppress the fallback log message, set `VEKIL_SECRET_STORE=file` explicitly.

## Security Notes

- Keyring-stored secrets are protected by the OS session lock (macOS Keychain access control, Linux session keyring, Windows user credential isolation).
- File-based secrets are written with `0600` permissions (owner read/write only) using atomic file operations to prevent partial writes.
- Tokens from the `COPILOT_GITHUB_TOKEN` environment variable bypass persistent storage entirely and are used directly for the session.
