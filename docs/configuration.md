# Configuration

Vekil supports two runtime patterns:

- **Zero-config mode**: no `--providers-config`; the proxy uses its built-in GitHub Copilot upstream.
- **Explicit provider routing**: pass `--providers-config` with any mix of `copilot`, `azure-openai`, `openai-codex`, `openai-compatible`, and `anthropic-compatible`. If the config omits Copilot, GitHub auth is not used.

## Topic Map

| Need | Doc |
|------|-----|
| Runtime flags, env vars, and Copilot header overrides | This file |
| Provider auth, JSON/YAML routing examples, model ownership, and provider metadata | [Provider Routing](provider-routing.md) |
| Provider console links and API-key setup patterns | [Provider API Keys](provider-api-keys.md) |
| Optional shell command rewrite and tool-output reduction config | [Tool Optimizers](tool-optimizers.md) |
| Codex-style `GET /v1/responses` websocket bridge and compaction tuning | [Responses WebSocket Bridge](responses-websocket.md) |

## Generic Flags

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--port` | `PORT` | `1337` | Listen port |
| `--host` | `HOST` | `127.0.0.1` | Listen host |
| `--token-dir` | `TOKEN_DIR` | `~/.config/vekil` | Token storage directory |
| `--providers-config` | `PROVIDERS_CONFIG` | unset | Path to JSON or YAML provider configuration for explicit provider routing |
| `--log-level` | `LOG_LEVEL` | `info` | Log level: `debug`, `info`, or `error` |
| `--streaming-upstream-timeout` | `STREAMING_UPSTREAM_TIMEOUT` | `1h0m0s` | Timeout for streaming upstream inference requests |
| `--no-metrics` | `NO_METRICS` | `false` | Disable the Prometheus `/metrics` endpoint |

Native CLI and tray-app runs default to `127.0.0.1`. Container deployments that publish the proxy port must bind to `0.0.0.0`; the official image and sample Kubernetes manifest set `HOST=0.0.0.0` for that path.

### Prometheus histogram buckets

The enabled-by-default `/metrics` endpoint uses these non-streaming request-duration buckets (seconds): `.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 20, 30, 60, 120, 300`. See [Prometheus Metrics](metrics.md) for metric names, label bounds, and scrape examples.

## Copilot Header Overrides

These overrides only affect Copilot-backed upstream requests. For provider-level header profiles, see [Copilot Provider Header Profiles](provider-routing.md#copilot-provider-header-profiles).

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--copilot-editor-version` | `COPILOT_EDITOR_VERSION` | `vscode/1.95.0` | Upstream `editor-version` header |
| `--copilot-plugin-version` | `COPILOT_PLUGIN_VERSION` | `copilot-chat/0.26.7` | Upstream `editor-plugin-version` header |
| `--copilot-user-agent` | `COPILOT_USER_AGENT` | `GitHubCopilotChat/0.26.7` | Upstream `user-agent` header |
| `--copilot-integration-id` | `COPILOT_INTEGRATION_ID` | `vscode-chat` | Upstream `copilot-integration-id` header |
| `--copilot-github-api-version` | `COPILOT_GITHUB_API_VERSION` | `2025-05-01` | Upstream `x-github-api-version` header |
| `--copilot-openai-intent` | `COPILOT_OPENAI_INTENT` | unset (`conversation-panel` for chat/responses) | Upstream `openai-intent` header |

## Provider Configs

Use `--providers-config` or `PROVIDERS_CONFIG` when you need explicit ownership of public model IDs across providers. Provider config files can be JSON (`.json`) or YAML (`.yaml`/`.yml`).

- See [Provider Routing](provider-routing.md) for auth notes, generic-compatible provider fields, provider examples, routing rules, endpoint allowlists, and model metadata.
- See [Provider API Keys](provider-api-keys.md) for provider console links and key-to-config mapping.
- See [Tool Optimizers](tool-optimizers.md) for the optional `tool_optimizers` block that can live alongside `providers` in the same config file.
- Set the optional top-level `insight_model` key to a public model ID the config serves to enable the dashboard's AI insights button. See [Traffic Dashboard](dashboard.md#ai-insights-optional).

## Responses WebSocket Bridge

The Codex-style `GET /v1/responses` websocket bridge is disabled by default and remains a proxy-owned transport over upstream HTTP `/responses`. See [Responses WebSocket Bridge](responses-websocket.md) for websocket flags, auto-compaction settings, chunked compaction knobs, and a debug run example.
