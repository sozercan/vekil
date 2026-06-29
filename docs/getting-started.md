# Getting Started

Vekil commonly runs in one of two modes:

- **Zero-config mode**: no `--providers-config`; uses the built-in GitHub Copilot upstream.
- **Explicit provider routing**: pass `--providers-config` to expose any mix of `copilot`, `azure-openai`, `openai-codex`, `openai-compatible`, and `anthropic-compatible` providers behind the same local API surface.

## Install Or Build

Download a binary from [GitHub Releases](https://github.com/sozercan/vekil/releases/latest). Release binaries are published for Linux, macOS, and Windows on `amd64` and `arm64`.

On Apple Silicon Macs, you can also install the tray app:

```bash
brew install --cask sozercan/repo/vekil
xattr -cr /Applications/Vekil.app  # only if macOS quarantine blocks launch
```

For tray app details, see [macOS/Linux Tray App](menubar.md).

Build from source:

```bash
go build -o vekil .
./vekil
```

## Docker

Base run:

```bash
docker run -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  ghcr.io/sozercan/vekil:latest
```

The image sets `HOST=0.0.0.0` so published Docker ports work. Native binary and tray-app runs default to `127.0.0.1`; bind to `0.0.0.0` only when you intentionally need network access.

With explicit provider routing:

```bash
docker run -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  -v /path/to/providers.yaml:/config/providers.yaml:ro \
  ghcr.io/sozercan/vekil:latest \
  --providers-config /config/providers.yaml
```

If the config includes `type: "openai-codex"`, also mount the Codex home read-only:

```bash
-v ~/.codex:/home/nonroot/.codex:ro
```

If you customize `CODEX_HOME`, set the container-side path and mount your host directory there. The published image supports `linux/amd64` and `linux/arm64`.

### RTK image variant

Use the `-rtk` image variant when your providers config enables the optional [`rtk_cli` tool optimizer](tool-optimizers.md#rtk_cli-provider). The default image stays minimal; the RTK variant only adds the `rtk` binary and does not enable tool optimizers by itself.

```bash
docker run -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  -v /path/to/providers.yaml:/config/providers.yaml:ro \
  ghcr.io/sozercan/vekil:latest-rtk \
  --providers-config /config/providers.yaml
```

Inside the variant, set the optimizer path to `/usr/local/bin/rtk` for explicitness.

Build a local image:

```bash
docker build -t vekil .
# Optional RTK variant:
# docker build --target runtime-rtk -t vekil:rtk .
docker run -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  vekil
```

## Kubernetes

A sample manifest is included at [`k8s/vekil.yaml`](../k8s/vekil.yaml).

```bash
kubectl apply -f k8s/vekil.yaml
```

## First Run And Authentication

Startup behavior depends on active providers. For the full auth matrix, see [Provider Authentication](provider-routing.md#provider-authentication). For provider console links and key setup patterns, see [Provider API Keys](provider-api-keys.md).

### GitHub Copilot

Zero-config startup and explicit `type: "copilot"` providers need GitHub Copilot auth. The proxy checks, in order:

1. explicit `COPILOT_GITHUB_TOKEN`
2. Vekil-managed cached credentials in `~/.config/vekil/`
3. GitHub CLI auth, but only after `vekil login --github-cli` or `vekil login --gh`

If none are available, first startup starts GitHub's device-code flow after binding the HTTP listener. During that one-time login window, `/healthz` is available for liveness probes and `/readyz` remains `not_ready` until authentication and upstream probing succeed. You can also run `vekil login` ahead of time, `vekil login --force` for a fresh device-code sign-in, or `vekil logout` to clear Vekil-managed auth and disable silent GitHub CLI reuse.

### Azure OpenAI and Microsoft Foundry

Configure Azure credentials in the provider entry. Use `api_key`/`api_key_env` for key auth, or set `auth_mode: azure_identity` to use Microsoft Entra auth through the Azure SDK `DefaultAzureCredential` chain. Vekil does not run `az login` itself; for local SDK auth, sign in with Azure CLI or another supported Azure credential before starting the proxy.

### OpenAI Codex

Run `codex login` first so `~/.codex/auth.json` exists. Set `CODEX_HOME` if your Codex home lives elsewhere. API-key auth and OS keychain-backed credentials are not read by the proxy.

### Generic Compatible Providers

Generic `openai-compatible` and `anthropic-compatible` providers use the auth fields in the providers config. Use `auth_type: none` for local services, or set `api_key_env` with `auth_type: bearer` or `auth_type: api-key-header` for hosted services. There is no interactive login flow.

If your provider config omits Copilot, startup skips GitHub authentication entirely.

## Verify The Proxy Is Up

```bash
curl http://localhost:1337/healthz
curl http://localhost:1337/readyz
curl http://localhost:1337/v1/models
```

- `/healthz` confirms the process is serving HTTP.
- `/readyz` verifies provider auth and upstream probes.
- `/v1/models` shows the merged public model catalog clients will see.
