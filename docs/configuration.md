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

Native CLI and tray-app runs default to `127.0.0.1`. Container deployments that publish the proxy port must bind to `0.0.0.0`; the official image and sample Kubernetes manifest set `HOST=0.0.0.0` for that path.

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

## Environment Variable Interpolation

Any string value in a provider config file (JSON or YAML) supports `${env:VAR_NAME}` interpolation. References are expanded at load time before parsing, so they work in any field position.

### Syntax

| Form | Behavior |
|------|----------|
| `${env:VAR_NAME}` | Replaced with the value of environment variable `VAR_NAME` |
| `\${env:VAR_NAME}` | Escaped — passes through as the literal `${env:VAR_NAME}` (backslash stripped) |

Variable names must match `[A-Za-z_][A-Za-z0-9_]*`.

### Error behavior

Missing environment variables produce a **hard startup error** that names every undefined variable referenced in the file. The proxy will not start with unresolved references.

### Example

```yaml
providers:
  - id: azure
    type: azure-openai
    base_url: ${env:AZURE_OPENAI_BASE_URL}
    api_key: ${env:AZURE_OPENAI_KEY}
    models:
      - public_id: gpt-5.4-pro
        deployment: gpt-5.4-pro
        endpoints:
          - /responses

  - id: copilot
    type: copilot
    default: true
    exclude_models:
      - gpt-5.4-pro
```

With environment variables set:

```bash
export AZURE_OPENAI_BASE_URL=https://myorg.openai.azure.com/openai/v1
export AZURE_OPENAI_KEY=sk-abc123...
vekil --providers-config providers.yaml
```

### Notes

- Interpolation applies before YAML/JSON parsing, so it works identically for both formats.
- An empty environment variable (set but empty string) is valid and substitutes as-is.
- Default values (`${env:VAR:-default}`) are not supported — keep variable definitions explicit.

## Responses WebSocket Bridge

The Codex-style `GET /v1/responses` websocket bridge is disabled by default and remains a proxy-owned transport over upstream HTTP `/responses`. See [Responses WebSocket Bridge](responses-websocket.md) for websocket flags, auto-compaction settings, chunked compaction knobs, and a debug run example.
