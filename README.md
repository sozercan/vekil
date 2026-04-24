# vekil

High-performance Go proxy that exposes Anthropic, Gemini, and OpenAI-compatible APIs behind one local endpoint. Vekil can run in zero-config mode against GitHub Copilot, or route selected models to configured providers such as Azure OpenAI and OpenAI Codex. The client-facing API surface stays the same while model ownership is configured behind the proxy.

## What It Supports

- Anthropic Messages API
- Gemini Generate Content and Count Tokens APIs
- OpenAI Chat Completions API
- OpenAI Responses API, including Codex websocket bridging
- Multi-provider model routing across GitHub Copilot, Azure OpenAI, and OpenAI Codex
- Proxy-owned Codex compatibility endpoints for compaction and memory summarization
- Streaming, tool use, parallel tool calls, compressed request bodies, and auth/token caching

## Quick Start

Download the latest binary for your platform from [GitHub Releases](https://github.com/sozercan/vekil/releases/latest), then run it locally.

Or with Docker from GHCR:

```bash
docker run -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  ghcr.io/sozercan/vekil:latest
```

For explicit provider routing, start the proxy with `--providers-config /path/to/providers.json`.

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

Depending on your active providers:

- Copilot-backed setups start GitHub's device code flow on first run.
- OpenAI Codex setups require `codex login` so `~/.codex/auth.json` exists.

For provider configuration, model routing, and provider-specific auth details, see [Getting Started](docs/getting-started.md) and [Configuration](docs/configuration.md).

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

## Common Client Setup

Use any public model ID exposed by `/v1/models`; the local client configuration stays the same even when a different upstream provider owns that model.

### Claude Code

```bash
env ANTHROPIC_BASE_URL=http://localhost:1337 \
  ANTHROPIC_API_KEY=dummy \
  claude --model claude-sonnet-4 --print --output-format text "Reply with exactly PROXY_OK"
```

### OpenAI Codex CLI

Use any public `/responses`-capable model ID exposed by your current setup. If you want the proxy to expose an OpenAI Codex subscription model, add an `openai-codex` provider first; model IDs still stay unprefixed for clients.

```bash
env OPENAI_API_KEY=dummy \
  OPENAI_BASE_URL=http://localhost:1337/v1 \
  codex exec --skip-git-repo-check -m gpt-5.5 "Reply with exactly PROXY_OK"
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
