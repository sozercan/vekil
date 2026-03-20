# copilot-proxy

High-performance Go proxy that exposes Anthropic, Gemini, and OpenAI-compatible APIs, forwarding all requests to GitHub Copilot's backend (`api.githubcopilot.com`). This lets you use tools that speak the Anthropic, Gemini, or OpenAI protocol with your GitHub Copilot subscription.

## What It Supports

- Anthropic Messages API
- Gemini Generate Content and Count Tokens APIs
- OpenAI Chat Completions API
- OpenAI Responses API, including Codex websocket bridging
- Proxy-owned Codex compatibility endpoints for compaction and memory summarization
- Streaming, tool use, parallel tool calls, compressed request bodies, and OAuth token caching

## Quick Start

```bash
go build -o copilot-proxy .
./copilot-proxy
```

Or with Docker:

```bash
docker run -p 1337:1337 \
  -v ~/.config/copilot-proxy:/home/nonroot/.config/copilot-proxy \
  docker.io/sozercan/copilot-proxy:latest
```

On first run, authenticate with GitHub's device code flow. Tokens are cached in `~/.config/copilot-proxy/`.

## Docs

The full documentation now lives under [`docs/`](docs/README.md) in smaller, topic-focused files:

- [Docs Index](docs/README.md)
- [Getting Started](docs/getting-started.md)
- [Configuration](docs/configuration.md)
- [Client Usage Examples](docs/clients.md)
- [API Reference](docs/api.md)
- [Architecture](docs/architecture.md)
- [macOS Menubar App](docs/menubar.md)
- [Development](docs/development.md)

For non-interactive environments, the proxy also accepts a GitHub token from `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, or `GITHUB_TOKEN`. If one of these is set, it overrides any cached login state and is exchanged for a short-lived Copilot token at startup.

## GitHub Actions

The repository includes a manual `Live Copilot Smoke` workflow in [`.github/workflows/live-copilot-smoke.yaml`](.github/workflows/live-copilot-smoke.yaml). It builds the proxy, installs Codex, Claude Code, and Gemini CLI on a GitHub-hosted runner, and then runs [`scripts/live-cli-smoke.sh`](scripts/live-cli-smoke.sh).

The smoke script starts the proxy with a non-interactive GitHub token, waits for `/readyz`, selects currently available OpenAI, Anthropic, and Gemini models from `/v1/models`, and runs one file-reading headless check per CLI using isolated temp-home config directories.

To use it:

1. Create a GitHub token for a user that has GitHub Copilot access.
2. Grant that token the `Copilot Requests` permission.
3. Save it as the repository secret `COPILOT_GITHUB_TOKEN`.
4. Run the `Live Copilot Smoke` workflow from the Actions tab.

This is intentionally separate from the normal CI workflow so pull requests and forked builds remain deterministic and do not depend on Copilot credentials. You can also run the same smoke script locally after building `copilot-proxy` and installing those three CLIs.

## Most Common Client Setup

### Claude Code

```bash
export ANTHROPIC_BASE_URL=http://localhost:1337
```

### OpenAI Codex CLI

```bash
export OPENAI_BASE_URL=http://localhost:1337/v1
codex --model gpt-5.4
```

### Gemini CLI

```bash
env GEMINI_API_KEY=dummy \
  GOOGLE_GEMINI_BASE_URL=http://localhost:1337 \
  GOOGLE_GENAI_API_VERSION=v1beta \
  GEMINI_CLI_NO_RELAUNCH=true \
  gemini -m gemini-2.5-pro -p "Reply with exactly PROXY_OK" -o json
```

## Development

```bash
go test ./... -count=1
go test ./proxy/ -run '^$' -bench 'BenchmarkResponsesWebSocketRequestBuild' -benchmem -count=1
go vet ./...
```

More detail is in [docs/development.md](docs/development.md).

## License

MIT
