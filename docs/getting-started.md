# Getting Started

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

```bash
docker pull ghcr.io/sozercan/vekil:latest
docker run -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  ghcr.io/sozercan/vekil:latest
```

Or build locally:

```bash
docker build -t vekil .
docker run -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  vekil
```

The published image supports `linux/amd64` and `linux/arm64`.

## Kubernetes

A sample manifest is included at [`k8s/vekil.yaml`](../k8s/vekil.yaml).

```bash
kubectl apply -f k8s/vekil.yaml
```

## First Run

If your setup includes a Copilot provider, the proxy starts GitHub's device code flow on first run:

1. Visit the URL shown in the terminal.
2. Enter the one-time code.
3. Authorize the application.

Tokens are cached in `~/.config/vekil/` and refreshed automatically before expiry.
If `HTTP_PROXY` or `HTTPS_PROXY` points at a local loopback proxy that is not running, the auth flow automatically retries GitHub requests directly.

If your provider config does not include Copilot, startup skips GitHub authentication. See [configuration.md](configuration.md) for multi-provider setup details.
