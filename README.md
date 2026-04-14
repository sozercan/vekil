# vekil

High-performance Go proxy that exposes Anthropic, Gemini, and OpenAI-compatible APIs, forwarding all requests to GitHub Copilot's backend (`api.githubcopilot.com`). This lets you use tools that speak the Anthropic, Gemini, or OpenAI protocol with your GitHub Copilot subscription.

## What It Supports

- Anthropic Messages API
- Gemini Generate Content and Count Tokens APIs
- OpenAI Chat Completions API
- OpenAI Responses API, including Codex websocket bridging
- Proxy-owned Codex compatibility endpoints for compaction and memory summarization
- Streaming, tool use, parallel tool calls, compressed request bodies, and OAuth token caching

## Quick Start

Download the latest binary for your platform from [GitHub Releases](https://github.com/sozercan/vekil/releases/latest), then run it locally.

Or with Docker from GHCR:

```bash
docker run -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  ghcr.io/sozercan/vekil:latest
```

On Apple Silicon Macs, you can also use the native menubar app.

```bash
brew install --cask sozercan/repo/vekil
```

> **Note:** The app is not signed.
> Clear extended attributes, including quarantine, with:
> ```bash
> xattr -cr /Applications/Vekil.app
> ```

Manual downloads still work through the `vekil-macos-arm64.zip` asset on [GitHub Releases](https://github.com/sozercan/vekil/releases/latest). See [macOS Menubar App](docs/menubar.md).

On first run, authenticate with GitHub's device code flow. Tokens are cached in `~/.config/vekil/`.

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

## Most Common Client Setup

### Claude Code

```bash
env ANTHROPIC_BASE_URL=http://localhost:1337 \
  ANTHROPIC_API_KEY=dummy \
  claude --model claude-sonnet-4 --print --output-format text "Reply with exactly PROXY_OK"
```

### OpenAI Codex CLI

```bash
env OPENAI_API_KEY=dummy \
  OPENAI_BASE_URL=http://localhost:1337/v1 \
  codex exec --skip-git-repo-check -m gpt-5.4 "Reply with exactly PROXY_OK"
```

### Gemini CLI

```bash
env GEMINI_API_KEY=dummy \
  GOOGLE_GEMINI_BASE_URL=http://localhost:1337 \
  GOOGLE_GENAI_API_VERSION=v1beta \
  GEMINI_CLI_NO_RELAUNCH=true \
  gemini -m gemini-2.5-pro -p "Reply with exactly PROXY_OK" -o json
```

## License

MIT
