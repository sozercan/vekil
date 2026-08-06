<p align="center">
  <img src="assets/macos/Vekil.png" alt="Vekil" width="128" height="128" />
</p>

<h1 align="center">vekil</h1>

<p align="center">
  <em>One local endpoint for Anthropic, Gemini, and OpenAI clients — backed by the provider of your choice.</em>
</p>

---

Vekil is a Go reverse proxy that exposes Anthropic, Gemini, and OpenAI-compatible APIs behind one local endpoint. Run it in zero-config mode against GitHub Copilot, or route selected models to providers like Azure OpenAI, OpenAI Codex, and generic OpenAI-compatible or Anthropic-compatible upstreams. The client-facing API surface stays the same while model ownership is configured behind the proxy. Vekil can also launch Claude Code, Codex CLI, or GitHub Copilot CLI through short-lived proxy sessions, optionally pinned to one model.

## Why Vekil?

Use your GitHub Copilot subscription with Claude Code, point the Codex CLI at Azure OpenAI, or send Gemini-CLI traffic through any OpenAI-compatible upstream — all without touching client config. Swap providers behind the proxy; your tools never notice.

> **Looking for free models?** Vekil can route to OpenCode Zen's free tier with no signup — [see how](docs/provider-routing.md#opencode-zen-free-tier). It is a shared, rate-limited trial tier (models rotate; not for sensitive prompts).

## Features

- **Anthropic Messages API** — drop-in compatible with Claude clients
- **Gemini API** — Generate Content, Stream Generate Content, and Count Tokens
- **OpenAI Chat Completions** and **Responses** APIs, including optional Codex websocket bridging
- **Multi-provider routing** across GitHub Copilot, Azure OpenAI, OpenAI Codex, and generic compatible providers; schema v2 provides explicit routes, ordered failover, internal routes, and semantic policy routing
- **Optional tool optimizers** for opt-in shell command rewrites and tool-output reduction across supported API surfaces; see [Tool Optimizers](docs/tool-optimizers.md)
- **Codex compatibility shims** for compaction and memory summarization
- **Streaming**, tool use, parallel tool calls, compressed request bodies, and auth/token caching
- **One-command Claude Code, Codex CLI, and GitHub Copilot CLI launchers** with ephemeral loopback proxies and no persistent client routing changes

## Quick Start

Grab a binary from [GitHub Releases](https://github.com/sozercan/vekil/releases/latest), or run the container from GHCR:

```bash
mkdir -p ~/.config/vekil
docker run --user "$(id -u):$(id -g)" -e HOME=/home/nonroot -p 1337:1337 \
  -v ~/.config/vekil:/home/nonroot/.config/vekil \
  ghcr.io/sozercan/vekil:latest
```

On Apple Silicon Macs, install the native tray app via Homebrew:

```bash
brew install --cask sozercan/repo/vekil
```

> The app is not signed. Clear quarantine with `xattr -cr /Applications/Vekil.app`. Manual `vekil-macos-arm64.zip` downloads are also on [Releases](https://github.com/sozercan/vekil/releases/latest). See [Tray App (macOS/Linux)](docs/menubar.md).

For explicit provider routing, start the proxy with `--providers-config /path/to/providers.{json,yaml}`.

Schema-v2 policy routing follows each profile's YAML `mode` by default; an explicit process mode can still lower it for rollout or emergency rollback. Policy profiles use a text/function-tool canonical Chat contract with translated Anthropic and bounded stateless Responses ingress for managed agents, and support one trusted user/tenant per deployment; see [Semantic Policy Routing](docs/policy-routing.md) before enabling `observe` or `enforce`.

### Launch a coding agent

The native binary can start a loopback-only proxy, configure the agent for that
session, and clean everything up when the agent exits. Claude Code and Codex
CLI can use their normal configured or built-in default; Copilot CLI requires an
explicit model:

```bash
vekil launch claude
vekil launch codex
vekil launch copilot --model gpt-5.4-mini
```

Pass `--model` to Claude Code or Codex CLI when you want Vekil to validate,
scope, and pin the session to one public model.

Use `--providers-config` with the same JSON/YAML routing file accepted by the
server. Arguments after `--` are forwarded to the agent:

```bash
vekil launch codex \
  --providers-config /path/to/providers.yaml \
  --model my-responses-model -- \
  exec --ephemeral "Review this workspace"
```

The launcher keeps upstream credentials in the proxy, gives the child a random
session token, and supervises the child process tree. Without `--model`, Claude
Code and Codex CLI select their CLI default through the normal global public
model namespace; with `--model`, Vekil restricts the session to that model. Use
`--dry-run` to inspect the plan without starting the proxy or agent. See [Agent
Launchers](docs/agent-launchers.md) for supported CLI versions, endpoint
requirements, forwarded arguments, logs, and isolation details.

**First-run auth** depends on your providers:

- **Copilot** — `vekil login` uses Vekil-managed GitHub device-code sign-in; first proxy startup starts the same flow when needed. To use your current GitHub CLI account instead, opt in with `vekil login --github-cli` (or `--gh`). `vekil logout` clears cached auth and disables future silent `gh` reuse until you opt in again. `COPILOT_GITHUB_TOKEN` remains the explicit non-interactive override.
- **Azure OpenAI and generic hosted providers** — use `api_key` or `api_key_env` in your provider config.
- **OpenAI Codex** — requires `codex login` so `~/.codex/auth.json` exists. In Docker, mount your Codex home into `CODEX_HOME` (default `/home/nonroot/.codex`).
- **Local generic providers** — use `auth_type: none`.

For full setup details, see [Getting Started](docs/getting-started.md), [Configuration](docs/configuration.md), and [Provider Routing](docs/provider-routing.md).

## Docs

Documentation lives under [`docs/`](docs/README.md); start with these:

|                                                              |                                     |
| ------------------------------------------------------------ | ----------------------------------- |
| [Getting Started](docs/getting-started.md)                   | Install, run, first auth            |
| [Configuration](docs/configuration.md)                       | Config map and generic flags        |
| [Provider Routing](docs/provider-routing.md)                 | Provider auth and route failover    |
| [Semantic Policy Routing](docs/policy-routing.md)            | Schema-v2 policy selection and gates |
| [Provider API Keys](docs/provider-api-keys.md)               | Where to get provider keys          |
| [Tool Optimizers](docs/tool-optimizers.md)                   | Shell rewrite/output reduction      |
| [Responses WebSocket](docs/responses-websocket.md)           | Websocket bridge tuning             |
| [Client Examples](docs/clients.md)                           | Copy-paste snippets per client      |
| [Agent Launchers](docs/agent-launchers.md)                   | One-command coding-agent sessions   |
| [API Reference](docs/api.md)                                 | Endpoint behavior and compatibility |
| [Architecture](docs/architecture.md)                         | Package layout and design notes     |
| [Traffic Dashboard](docs/dashboard.md)                       | Live browser dashboard and stats    |
| [Tray App](docs/menubar.md)                                  | macOS/Linux menubar usage           |
| [Development](docs/development.md)                           | Build, test, benchmarks, CI         |

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
  codex exec --skip-git-repo-check -m gpt-5.5 \
  -c 'model_provider="openai"' \
  -c 'openai_base_url="http://localhost:1337/v1"' \
  "Reply with exactly PROXY_OK"
```

### GitHub Copilot CLI

```bash
env COPILOT_PROVIDER_BASE_URL=http://localhost:1337/v1 \
  COPILOT_PROVIDER_TYPE=openai \
  COPILOT_PROVIDER_WIRE_API=responses \
  COPILOT_MODEL=gpt-5.5 \
  COPILOT_OFFLINE=true \
  copilot -p "Reply with exactly PROXY_OK" -s
```

### Gemini CLI

```bash
env GEMINI_API_KEY=dummy \
  GOOGLE_GEMINI_BASE_URL=http://localhost:1337 \
  GOOGLE_GENAI_API_VERSION=v1beta \
  GEMINI_CLI_NO_RELAUNCH=true \
  gemini -m gemini-2.5-pro -p "Reply with exactly PROXY_OK" -o json
```
