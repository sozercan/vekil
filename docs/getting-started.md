# Getting Started

Vekil commonly runs in one of two modes:

- **Zero-config mode**: start the proxy with no `--providers-config`; it uses the built-in GitHub Copilot upstream.
- **Explicit provider routing**: pass `--providers-config` to expose any mix of `copilot`, `azure-openai`, and `openai-codex` providers behind the same local API surface.

## Download From GitHub Releases

Download the latest binary for your platform from [GitHub Releases](https://github.com/sozercan/vekil/releases/latest).

Published binaries are available for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`, and `windows/arm64`. Windows release assets are published as `.exe` binaries. After downloading, make the binary executable if needed and run it locally.

On Apple Silicon Macs, you can use the native macOS tray app:

```bash
brew install --cask sozercan/repo/vekil
```

> **Note:** The app is not signed.
> Clear extended attributes, including quarantine, with:
> ```bash
> xattr -cr /Applications/Vekil.app
> ```

GitHub Releases also includes a `vekil-macos-arm64.zip` tray app bundle if you prefer a manual download.

## Build From Source

```bash
go build -o vekil .
./vekil
```

## Docker

Base container run:

```bash
docker pull ghcr.io/sozercan/vekil:latest
docker run -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  ghcr.io/sozercan/vekil:latest
```

If you use explicit provider routing, mount your JSON or YAML config file and pass `--providers-config`:

```bash
docker run -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  -v /path/to/providers.json:/config/providers.json:ro \
  ghcr.io/sozercan/vekil:latest \
  --providers-config /config/providers.json
```

If your provider config includes `type: "openai-codex"`, also mount the Codex home read-only so the proxy can read `auth.json`:

```bash
docker run -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  -v ~/.codex:/home/nonroot/.codex:ro \
  -v /path/to/providers.json:/config/providers.json:ro \
  ghcr.io/sozercan/vekil:latest \
  --providers-config /config/providers.json
```

If you customize `CODEX_HOME`, remember that the container reads the container-side path, not your host path. Mount your host Codex directory to the same path you set in `CODEX_HOME`, for example:

```bash
docker run -p 1337:1337 \
  -e CODEX_HOME=/codex-home \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  -v /path/to/custom-codex-home:/codex-home:ro \
  -v /path/to/providers.json:/config/providers.json:ro \
  ghcr.io/sozercan/vekil:latest \
  --providers-config /config/providers.json
```

To build a local image instead of pulling GHCR:

```bash
docker build -t vekil .
docker run -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  vekil
```

The same extra mounts and `--providers-config` flag shown above also apply to a locally built image.

The published image supports `linux/amd64` and `linux/arm64`.

## Kubernetes

A sample manifest is included at [`k8s/vekil.yaml`](../k8s/vekil.yaml).

```bash
kubectl apply -f k8s/vekil.yaml
```

## First Run And Authentication

Startup behavior depends on the providers that are active in your deployment.

### GitHub Copilot

If you are using zero-config startup or an explicit `type: "copilot"` provider, the proxy first looks for GitHub authentication in this order:

1. `COPILOT_GITHUB_TOKEN` when set explicitly for CI or another non-interactive environment.
2. Vekil's cached GitHub access token in `~/.config/vekil/`.
3. An authenticated GitHub CLI via `gh auth token --hostname github.com`, but only after you explicitly opt in with `vekil login --github-cli` or `vekil login --gh`.

If none of those sources is available, Vekil starts GitHub's device-code flow on first run:

1. Visit the URL shown in the terminal.
2. Enter the one-time code.
3. Authorize the application.

You can also run `vekil login` ahead of time to start the same device-code flow. If an existing Vekil login is still refreshable, `vekil login` reuses it and prints `Already logged in.`; use `vekil login --force` to skip that refresh check and force a new device-code sign-in.

To use the account that is already authenticated with the GitHub CLI, run `vekil login --github-cli` or the shorter `vekil login --gh`. This records an explicit preference to use `gh` for future Copilot token refreshes, but the GitHub CLI token itself is not copied into Vekil's `access-token` cache.

Tokens are cached in `~/.config/vekil/` and refreshed automatically before expiry. GitHub CLI-backed sign-in caches only the short-lived Copilot token and the explicit `gh` opt-in preference; it does not persist the GitHub CLI access token as a Vekil-managed token.

Signing out with `vekil logout` clears Vekil's cached credentials, disables GitHub CLI auto sign-in, and records a signed-out state so Vekil will not automatically borrow GitHub CLI credentials again. Run `vekil login --github-cli` to opt back into GitHub CLI auth, sign in with the device-code flow to use Vekil-managed OAuth, or set `COPILOT_GITHUB_TOKEN` explicitly for a non-interactive session.

If `HTTP_PROXY` or `HTTPS_PROXY` points at a local loopback proxy that is not running, the auth flow automatically retries GitHub requests directly.

### Azure OpenAI

Azure OpenAI credentials are configured per provider entry, usually with `api_key` or `api_key_env` in the providers config file. There is no separate interactive login flow in the proxy.

### OpenAI Codex

When your providers config includes `type: "openai-codex"`, first sign in with the OpenAI Codex CLI using ChatGPT auth so `~/.codex/auth.json` exists. The proxy reads that file directly and refreshes the access token as needed.

Codex API-key auth and OS keychain-backed credentials are not read by the proxy. If your Codex home is not `~/.codex`, set `CODEX_HOME` before starting the proxy. In Docker, `CODEX_HOME` is resolved inside the container, so your bind mount target must match the in-container `CODEX_HOME` path.

If your provider config does not include Copilot, startup skips GitHub authentication entirely. See [configuration.md](configuration.md) for provider routing examples and provider-specific knobs.

## Verify The Proxy Is Up

After the proxy is running and any first-run authentication has completed, check the basic health and model endpoints:

```bash
curl http://localhost:1337/healthz
curl http://localhost:1337/readyz
curl http://localhost:1337/v1/models
```

What each endpoint tells you:

- `/healthz` confirms the process is listening and serving HTTP. It should return `{"status":"ok"}`.
- `/readyz` verifies the proxy can authenticate to and probe the configured upstream providers. It should return `{"status":"ready"}`; `503` usually means auth or upstream reachability still needs attention.
- `/v1/models` shows the merged public model catalog that clients will see. Use it to confirm the models you expect are actually exposed before testing a client.
