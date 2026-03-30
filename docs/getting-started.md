# Getting Started

## Download From GitHub Releases

Download the latest binary for your platform from [GitHub Releases](https://github.com/sozercan/copilot-proxy/releases/latest).

Published binaries are available for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`. After downloading, make the binary executable if needed and run it locally.

## Build From Source

```bash
go build -o copilot-proxy .
./copilot-proxy
```

## Docker

```bash
docker pull docker.io/sozercan/copilot-proxy:latest
docker run -p 1337:1337 \
  -v ~/.config/copilot-proxy:/home/nonroot/.config/copilot-proxy \
  docker.io/sozercan/copilot-proxy:latest
```

Or build locally:

```bash
docker build -t copilot-proxy .
docker run -p 1337:1337 \
  -v ~/.config/copilot-proxy:/home/nonroot/.config/copilot-proxy \
  copilot-proxy
```

The published image supports `linux/amd64` and `linux/arm64`.

## Kubernetes

A sample manifest is included at [`k8s/copilot-proxy.yaml`](../k8s/copilot-proxy.yaml).

```bash
kubectl apply -f k8s/copilot-proxy.yaml
```

## First Run

On first run, the proxy starts GitHub's device code flow:

1. Visit the URL shown in the terminal.
2. Enter the one-time code.
3. Authorize the application.

Tokens are cached in `~/.config/copilot-proxy/` and refreshed automatically before expiry.
