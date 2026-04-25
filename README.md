<p align="center">
  <img src="assets/macos/Vekil.png" alt="Vekil" width="128" height="128" />
</p>

<h1 align="center">vekil</h1>

<p align="center">
  <em>One local endpoint for Anthropic, Gemini, and OpenAI clients — backed by the provider of your choice.</em>
</p>

---

Vekil is a Go reverse proxy that exposes Anthropic, Gemini, and OpenAI-compatible APIs behind one local endpoint. Run it in zero-config mode against GitHub Copilot, or route selected models to providers like Azure OpenAI and OpenAI Codex. The client-facing API surface stays the same while model ownership is configured behind the proxy.

## Why Vekil?

Use your GitHub Copilot subscription with Claude Code, point the Codex CLI at Azure OpenAI, or send Gemini-CLI traffic through any OpenAI-compatible upstream — all without touching client config. Swap providers behind the proxy; your tools never notice.

## Features

- **Anthropic Messages API** — drop-in compatible with Claude clients
- **Gemini API** — Generate Content, Stream Generate Content, and Count Tokens
- **OpenAI Chat Completions** and **Responses** APIs, including Codex websocket bridging
- **Multi-provider routing** across GitHub Copilot, Azure OpenAI, and OpenAI Codex
- **Codex compatibility shims** for compaction and memory summarization
- **Streaming**, tool use, parallel tool calls, compressed request bodies, and auth/token caching

## Quick Start

Grab a binary from [GitHub Releases](https://github.com/sozercan/vekil/releases/latest), or run the container from GHCR:

```bash
docker run -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  ghcr.io/sozercan/vekil:latest
```

On Apple Silicon Macs, install the native tray app via Homebrew:

```bash
brew install --cask sozercan/repo/vekil
```

> The app is not signed. Clear quarantine with `xattr -cr /Applications/Vekil.app`. Manual `vekil-macos-arm64.zip` downloads are also on [Releases](https://github.com/sozercan/vekil/releases/latest). See [Tray App (macOS/Linux)](docs/menubar.md).

For explicit provider routing, start the proxy with `--providers-config /path/to/providers.{json,yaml}`.

**First-run auth** depends on your providers:

- **Copilot** — `vekil login` uses Vekil-managed GitHub device-code sign-in; first proxy startup starts the same flow when needed. To use your current GitHub CLI account instead, opt in with `vekil login --github-cli` (or `--gh`). `vekil logout` clears cached auth and disables future silent `gh` reuse until you opt in again. `COPILOT_GITHUB_TOKEN` remains the explicit non-interactive override.
- **OpenAI Codex** — requires `codex login` so `~/.codex/auth.json` exists. In Docker, mount your Codex home into `CODEX_HOME` (default `/home/nonroot/.codex`).

For full configuration and routing details, see [Getting Started](docs/getting-started.md) and [Configuration](docs/configuration.md).

## Docs

Full documentation lives under [`docs/`](docs/README.md):

|                                            |                                     |
| ------------------------------------------ | ----------------------------------- |
| [Getting Started](docs/getting-started.md) | Install, run, first auth            |
| [Configuration](docs/configuration.md)     | Flags, env vars, provider routing   |
| [Client Examples](docs/clients.md)         | Copy-paste snippets per client      |
| [API Reference](docs/api.md)               | Endpoint behavior and compatibility |
| [Architecture](docs/architecture.md)       | Package layout and design notes     |
| [Tray App](docs/menubar.md)                | macOS/Linux menubar usage           |
| [Development](docs/development.md)         | Build, test, benchmarks, CI         |

## Client Examples

Use any public model ID exposed by `/v1/models` — your client config is the same regardless of which provider owns the model upstream.

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
