# Configuration

Vekil supports two runtime patterns:

- **Zero-config mode**: no `--providers-config`; the proxy uses its built-in GitHub Copilot upstream.
- **Explicit provider routing**: pass `--providers-config` with any mix of `copilot`, `azure-openai`, `openai-codex`, `openai-compatible`, and `anthropic-compatible`. If the config omits Copilot, GitHub auth is not used.

Schema version 2 is the complete explicit-routing format: it supports public and internal routes, ordered failover, and optional [semantic policy profiles](policy-routing.md) that select one Chat-capable terminal route per request. Existing route-only version-2 files remain valid. By default, each profile's YAML `mode` is authoritative; an explicit process mode can still lower the effective mode as an operational override.

## Topic Map

| Need | Doc |
|------|-----|
| Runtime flags, env vars, and Copilot header overrides | This file |
| Provider auth, JSON/YAML routing examples, model ownership, and provider metadata | [Provider Routing](provider-routing.md) |
| Schema-v2 semantic policy profiles, privacy/trust acknowledgements, and rollout gates | [Semantic Policy Routing](policy-routing.md) |
| Provider console links and API-key setup patterns | [Provider API Keys](provider-api-keys.md) |
| Optional shell command rewrite and tool-output reduction config | [Tool Optimizers](tool-optimizers.md) |
| Codex-style `GET /v1/responses` websocket bridge and compaction tuning | [Responses WebSocket Bridge](responses-websocket.md) |

## Generic Flags

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--port` | `PORT` | `1337` | Listen port |
| `--host` | `HOST` | `127.0.0.1` | Listen host |
| `--token-dir` | `TOKEN_DIR` | `~/.config/vekil` | Token storage directory |
| `--providers-config` | `PROVIDERS_CONFIG` | unset | Local path or HTTP(S) URL to JSON or YAML provider configuration for explicit provider routing |
| `--policy-routing` | `POLICY_ROUTING_MODE` | `config` | Policy-routing ceiling: `config` follows each profile's YAML `mode`; `off`, `observe`, or `enforce` explicitly cap every profile. A profile cannot run above an explicit ceiling. |
| `--policy-routing-allow-remote-single-tenant` | `POLICY_ROUTING_ALLOW_REMOTE_SINGLE_TENANT` | `false` | Acknowledge running policy `observe`/`enforce` on a non-loopback bind for one trusted tenant. This adds no authentication or tenant isolation. |
| `--log-level` | `LOG_LEVEL` | `info` | Log level: `debug`, `info`, `warn`, or `error` |
| `--streaming-upstream-timeout` | `STREAMING_UPSTREAM_TIMEOUT` | `1h0m0s` | Timeout for streaming upstream inference requests |

Native CLI and tray-app runs default to `127.0.0.1`. Container deployments that publish the proxy port must bind to `0.0.0.0`; the official image and sample Kubernetes manifest set `HOST=0.0.0.0` for that path.

Serve mode defaults Go's garbage-collection target to `GOGC=200` to favor
throughput while retaining a bounded memory footprint. Whitespace-only `GOGC`
values are treated as unset. Set a standard non-whitespace value, including
`GOGC=off`, to override the default with the requested Go runtime behavior.

Policy `observe` and `enforce` are a v1 single-tenant feature. Loopback is the default supported topology. A non-loopback bind requires the explicit remote-single-tenant acknowledgement above, but that acknowledgement does not protect the proxy; put remote deployments behind a trusted external authentication and network boundary.

## Copilot Header Overrides

These overrides only affect Copilot-backed upstream requests. For provider-level header profiles, see [Copilot Provider Header Profiles](provider-routing.md#copilot-provider-header-profiles).

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--copilot-editor-version` | `COPILOT_EDITOR_VERSION` | `vscode/1.95.0` | Upstream `editor-version` header |
| `--copilot-plugin-version` | `COPILOT_PLUGIN_VERSION` | `copilot-chat/0.26.7` | Upstream `editor-plugin-version` header |
| `--copilot-user-agent` | `COPILOT_USER_AGENT` | `GitHubCopilotChat/0.26.7` | Upstream `user-agent` header |
| `--copilot-integration-id` | `COPILOT_INTEGRATION_ID` | credential-aware | Upstream `copilot-integration-id` header; direct `ghu_` catalog/Chat requests default to `copilot-language-server`, while its Responses fallback and other credentials default to `vscode-chat` |
| `--copilot-github-api-version` | `COPILOT_GITHUB_API_VERSION` | `2025-05-01` | Upstream `x-github-api-version` header |
| `--copilot-openai-intent` | `COPILOT_OPENAI_INTENT` | unset (`conversation-panel` for chat/responses) | Upstream `openai-intent` header |

## Provider Configs

Use `--providers-config` or `PROVIDERS_CONFIG` when you need explicit ownership of public model IDs across providers. The source may be a local path or an HTTP(S) URL. Configs can be JSON (`.json`) or YAML (`.yaml`/`.yml`); for a URL, the URL path extension selects YAML and the query string is ignored when detecting the format. Other extensions use JSON decoding, matching local-file behavior.

Remote configs are fetched once when the server, validator, or managed launcher loads them. Each fetch has a 15-second total timeout and a 4 MiB response-body limit; Vekil does not poll the URL or reload changes. Redirects are not followed. Prefer HTTPS because an HTTP response can be modified in transit, and keep credentials in `api_key_env` rather than embedding them in remotely hosted configuration. Startup and validation fail closed when the source cannot be fetched or returns a non-2xx status. User-visible diagnostics omit URL userinfo, query parameters, and fragments so credentials and signed query values are not printed.

### Environment interpolation in local provider configs

Any string value in a local JSON or YAML provider config can reference a non-empty environment variable with `${env:VAR_NAME}`. Interpolation happens after parsing and before validation, supports embedded or multiple references, and fails startup with the field and variable name when a variable is undefined or empty. Variable names must match the shell-compatible form `[A-Za-z_][A-Za-z0-9_]*`; default-value expressions such as `${env:VAR_NAME:-default}` are not supported.

```yaml
providers:
  - id: copilot
    type: copilot
    default: true
    exclude_models: [azure-model]
    headers:
      default:
        editor_version: ${env:COPILOT_EDITOR_VERSION}
  - id: azure
    type: azure-openai
    base_url: ${env:AZURE_BASE_URL}
    api_key: ${env:AZURE_API_KEY}
    models:
      - public_id: azure-model
        deployment: ${env:AZURE_DEPLOYMENT}
```

Use `\${env:VAR_NAME}` when the resulting value must literally be `${env:VAR_NAME}`. In a YAML double-quoted string or a JSON string, encode that backslash as `\\${env:VAR_NAME}`; YAML plain and single-quoted strings use one backslash.

Interpolation is intentionally disabled for configs loaded from HTTP(S) URLs: an unescaped reference is rejected without reading the host environment. This prevents a remotely controlled config from copying host values into provider requests. Escaped literal references remain valid.

`api_key_env` keeps its existing meaning and is excluded from interpolation: its value is already the name of an environment variable, such as `api_key_env: AZURE_API_KEY`. Prefer `api_key_env` for provider credentials; use `${env:...}` for other host-specific string values or where a direct string field must be populated.

- See [Provider Routing](provider-routing.md) for auth notes, generic-compatible provider fields, provider examples, routing rules, endpoint allowlists, and model metadata.
- See [Semantic Policy Routing](policy-routing.md) for the schema-v2 `exposure`, `policy_profiles`, provider trust metadata, and classifier data-policy contract. A complete example is checked in at [`examples/policy-routing-coding-economy.yaml`](../examples/policy-routing-coding-economy.yaml).
- See [Provider API Keys](provider-api-keys.md) for provider console links and key-to-config mapping.
- See [Tool Optimizers](tool-optimizers.md) for the optional `tool_optimizers` block that can live alongside `providers` in the same config file.
- Set the optional top-level `insight_model` key to a public model ID the config serves to enable the dashboard's AI insights button. See [Traffic Dashboard](dashboard.md#ai-insights-optional).

## Config validation

Validate strict JSON/YAML decoding, references, collisions, route contracts, policy bounds, and local credential construction without starting the server or contacting inference endpoints:

```bash
vekil config validate --providers-config /path/to/providers.yaml
```

For a schema-v2 policy config, add `--live` to perform one fixed non-user classifier protocol preflight per distinct classifier route selected by the policy config:

```bash
vekil config validate --live --providers-config /path/to/providers.yaml
```

When the config source is a URL, ordinary validation fetches that source but remains offline with respect to provider discovery and inference endpoints. Live validation verifies authentication/reachability, the forced `emit_policy_signals` function contract, strict arguments, configured non-storage behavior, and the one-send limit. It proves protocol acceptance, not the provider's external retention behavior.

At server startup, effective `enforce` profiles must pass live preflight before startup/readiness completes. An effective `observe` profile that fails preflight is kept off with a readiness/configuration diagnostic. Effective `off` profiles send no preflight or classifier request.

## Responses WebSocket Bridge

The Codex-style `GET /v1/responses` websocket bridge is disabled by default and remains a proxy-owned transport over upstream HTTP `/responses`. See [Responses WebSocket Bridge](responses-websocket.md) for websocket flags, auto-compaction settings, chunked compaction knobs, and a debug run example.
