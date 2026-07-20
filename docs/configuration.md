# Configuration

Vekil supports two runtime patterns:

- **Zero-config mode**: no `--providers-config`; the proxy uses its built-in GitHub Copilot upstream.
- **Explicit provider routing**: pass `--providers-config` with any mix of `copilot`, `azure-openai`, `openai-codex`, `openai-compatible`, and `anthropic-compatible`. If the config omits Copilot, GitHub auth is not used.

A long-lived loopback CLI or menubar server can also stage and atomically publish provider, route, and policy edits through the [local dashboard configuration](dashboard-config.md) control plane. Those edits are stored in a source-scoped managed JSON file; the original JSON/YAML bootstrap is never modified.

Schema version 2 is the complete explicit-routing format: it supports public and internal routes, ordered failover, and optional [semantic policy profiles](policy-routing.md) that select one native-Chat terminal route per request. Existing route-only version-2 files remain valid. Policy routing is opt-in at runtime and defaults to globally off.

## Topic Map

| Need | Doc |
|------|-----|
| Runtime flags, env vars, and Copilot header overrides | This file |
| Provider auth, JSON/YAML routing examples, model ownership, and provider metadata | [Provider Routing](provider-routing.md) |
| Local editor, managed source precedence, optimistic revisions, apply/reset, and security | [Local Dashboard Configuration](dashboard-config.md) |
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
| `--providers-config` | `PROVIDERS_CONFIG` | unset | Path to JSON or YAML provider configuration for explicit provider routing |
| `--ignore-managed` | `PROVIDERS_CONFIG_IGNORE_MANAGED` | `false` | Start from the selected bootstrap source without reading or deleting its dashboard-managed override. This is a read-only recovery startup. |
| `--reset-managed` | `PROVIDERS_CONFIG_RESET_MANAGED` | `false` | Delete the selected source's dashboard-managed override before startup, then use the current bootstrap. Mutually exclusive with `--ignore-managed`. |
| `--policy-routing` | `POLICY_ROUTING_MODE` | `off` | Process-wide policy-routing ceiling: `off`, `observe`, or `enforce`. A profile cannot run above this ceiling. |
| `--policy-routing-allow-remote-single-tenant` | `POLICY_ROUTING_ALLOW_REMOTE_SINGLE_TENANT` | `false` | Acknowledge running policy `observe`/`enforce` on a non-loopback bind for one trusted tenant. This adds no authentication or tenant isolation. |
| `--log-level` | `LOG_LEVEL` | `info` | Log level: `debug`, `info`, or `error` |
| `--streaming-upstream-timeout` | `STREAMING_UPSTREAM_TIMEOUT` | `1h0m0s` | Timeout for streaming upstream inference requests |

Native CLI and tray-app runs default to `127.0.0.1`. Container deployments that publish the proxy port must bind to `0.0.0.0`; the official image and sample Kubernetes manifest set `HOST=0.0.0.0` for that path.

Policy `observe` and `enforce` are a v1 single-tenant feature. Loopback is the default supported topology. A non-loopback bind requires the explicit remote-single-tenant acknowledgement above, but that acknowledgement does not protect the proxy; put remote deployments behind a trusted external authentication and network boundary.

## Bootstrap and dashboard-managed configuration

The selected bootstrap has a stable source identity:

- no file: `implicit-copilot`;
- file selection: `file:<absolute-clean-path>`.

Vekil looks for a matching managed envelope below `<UserConfigDir>/vekil/dashboard-config/`. The implicit source uses `implicit-copilot.json`; file sources use a SHA-256-derived filename. Startup uses the managed document only when its source ID and canonical bootstrap digest match the current bootstrap. A semantic bootstrap change with an existing managed override is a conflict and stops startup; formatting/comment-only changes do not conflict.

Recovery options are intentionally different:

- `--ignore-managed` uses the bootstrap for this process, preserves the override, and disables managed writes.
- `--reset-managed` removes the override and returns to a normally writable bootstrap-backed startup when the config directory is writable.
- The tray app exposes **Reset Dashboard Override** for the currently selected bootstrap source.
- The running local editor exposes an optimistic, asynchronous **Reset managed override** action.

The original bootstrap file remains byte-for-byte unchanged across dashboard applies and resets. Inline secrets written through the editor live in plaintext in the owner-restricted managed file; prefer `api_key_env` where practical. See [Local Dashboard Configuration](dashboard-config.md#bootstrap-source-and-managed-override) for precedence, conflicts, paths, permissions, and container behavior.

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
- See [Semantic Policy Routing](policy-routing.md) for the schema-v2 `exposure`, `policy_profiles`, provider trust metadata, and classifier data-policy contract. A complete example is checked in at [`examples/policy-routing-coding-economy.yaml`](../examples/policy-routing-coding-economy.yaml).
- See [Provider API Keys](provider-api-keys.md) for provider console links and key-to-config mapping.
- See [Tool Optimizers](tool-optimizers.md) for the optional `tool_optimizers` block that can live alongside `providers` in the same config file.
- Set the optional top-level `insight_model` key to a public model ID the config serves to enable the dashboard's AI insights button. See [Traffic Dashboard](dashboard.md#ai-insights-optional).

## Config validation

The dashboard's `POST /dashboard/api/v1/config/validate` path performs the same strict JSON and semantic checks against the active base revision, including secret-operation and preserved-field rules. It is offline and does not publish a generation. The CLI commands below remain the file-oriented validation path.

Validate strict JSON/YAML decoding, references, collisions, route contracts, policy bounds, and local credential construction without starting the server or contacting inference endpoints:

```bash
vekil config validate --providers-config /path/to/providers.yaml
```

For a schema-v2 policy config, add `--live` to perform one fixed non-user classifier protocol preflight per distinct classifier route selected by the policy config:

```bash
vekil config validate --live --providers-config /path/to/providers.yaml
```

Live validation verifies authentication/reachability, the forced `emit_policy_signals` function contract, strict arguments, configured non-storage behavior, and the one-send limit. It proves protocol acceptance, not the provider's external retention behavior. Ordinary `config validate` remains offline.

At server startup, effective `enforce` profiles must pass live preflight before startup/readiness completes. An effective `observe` profile that fails preflight is kept off with a readiness/configuration diagnostic. Effective `off` profiles send no preflight or classifier request.

## Responses WebSocket Bridge

The Codex-style `GET /v1/responses` websocket bridge is disabled by default and remains a proxy-owned transport over upstream HTTP `/responses`. See [Responses WebSocket Bridge](responses-websocket.md) for websocket flags, auto-compaction settings, chunked compaction knobs, and a debug run example.
