# Getting Started

Vekil commonly runs in one of two modes:

- **Zero-config mode**: start the proxy with no `--providers-config`; it uses the built-in GitHub Copilot upstream.
- **Explicit provider routing**: pass `--providers-config` to expose any mix of `copilot`, `azure-openai`, and `openai-codex` providers behind the same local API surface.

## Download From GitHub Releases

Download the latest binary for your platform from [GitHub Releases](https://github.com/sozercan/vekil/releases/latest).

Published binaries are available for `linux/amd64`, `linux/arm64`, `darwin/amd64`, `darwin/arm64`, `windows/amd64`, and `windows/arm64`. Windows release assets are published as `.exe` binaries. After downloading, make the binary executable if needed and run it locally.

On Apple Silicon Macs, you can use the native menubar app:

```bash
brew install --cask sozercan/repo/vekil
```

> **Note:** The app is not signed.
> Clear extended attributes, including quarantine, with:
> ```bash
> xattr -cr /Applications/Vekil.app
> ```

GitHub Releases also includes a `vekil-macos-arm64.zip` menubar app bundle if you prefer a manual download.

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

If you use explicit provider routing, mount your config file and pass `--providers-config`:

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

If you are using zero-config startup or an explicit `type: "copilot"` provider, the proxy starts GitHub's device code flow on first run:

1. Visit the URL shown in the terminal.
2. Enter the one-time code.
3. Authorize the application.

Tokens are cached in `~/.config/vekil/` and refreshed automatically before expiry.
If `HTTP_PROXY` or `HTTPS_PROXY` points at a local loopback proxy that is not running, the auth flow automatically retries GitHub requests directly.

### Azure OpenAI

Azure OpenAI credentials are configured per provider entry, usually with `api_key` or `api_key_env` in the providers JSON file. There is no separate interactive login flow in the proxy.

### OpenAI Codex

When your providers config includes `type: "openai-codex"`, first sign in with the OpenAI Codex CLI using ChatGPT auth so `~/.codex/auth.json` exists. The proxy reads that file directly and refreshes the access token as needed.

Codex API-key auth and OS keychain-backed credentials are not read by the proxy. If your Codex home is not `~/.codex`, set `CODEX_HOME` before starting the proxy.

If your provider config does not include Copilot, startup skips GitHub authentication entirely. See [configuration.md](configuration.md) for provider routing examples and provider-specific knobs.
