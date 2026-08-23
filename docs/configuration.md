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
| macOS shell configuration ownership and release status | [macOS Configuration Ownership](#macos-configuration-ownership) and [macOS App and Linux Tray](menubar.md) |
| Optional shell command rewrite and tool-output reduction config | [Tool Optimizers](tool-optimizers.md) |
| Codex-style `GET /v1/responses` websocket bridge and compaction tuning | [Responses WebSocket Bridge](responses-websocket.md) |

## macOS Configuration Ownership

The current Go tray stores only a selected provider-file path; that JSON/YAML file remains user-owned. The native source implements three explicit modes:

- **Legacy/zero-config** preserves built-in Copilot routing and existing authentication files. Users are not automatically converted to schema-v2 managed configuration.
- **External Configuration** selects a user-owned file. Vekil does not write, normalize, import, or silently adopt it. On-disk and active revisions are tracked separately; edits require explicit reload, and an unsafe, missing, oversized, or invalid replacement leaves the prior active configuration and run intent unchanged.
- **Managed Configuration** is explicit opt-in. The helper owns `~/Library/Application Support/vekil/providers.yaml`, versioned state in `menubar.json`, and the crash-recovery journal `managed-apply.json`; Swift owns UI preferences such as login/start behavior and window state.

The implemented helper uses secure bounded snapshot reads, ownership/revision checks, stable provider UUIDs, secret generations, private directory/file modes, and atomic apply/rollback. The protocol-level **Validate and Apply** transaction validates the exact staged YAML bytes with the matching in-memory secret generation. Applying while stopped stays stopped; applying while running restores running intent only after installation and readiness succeed. Interrupted transactions are resolved from the secret-free journal before new mutations are accepted. The current native Providers view keeps managed editing disabled until the signed cross-version Keychain continuity gate passes; External Configuration and zero-config mode are the enabled parity paths.

This source implementation does not add provider-schema fields or public endpoints. Shipping managed configuration remains gated by the cross-version signed Sparkle/Keychain continuity evidence described in [Development](development.md#native-macos-shell-and-release-gates). Advanced schema-v2 configuration remains available through External Configuration. See [Provider Routing](provider-routing.md#native-macos-guided-editor-scope) and [Provider API Keys](provider-api-keys.md#macos-managed-provider-secrets).

## Generic Flags

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--port` | `PORT` | `1337` | Listen port |
| `--host` | `HOST` | `127.0.0.1` | Listen host |
| `--token-dir` | `TOKEN_DIR` | `~/.config/vekil` | Token storage directory |
| `--providers-config` | `PROVIDERS_CONFIG` | unset | Local path or HTTP(S) URL to JSON or YAML provider configuration for explicit provider routing |
| `--policy-routing` | `POLICY_ROUTING_MODE` | `config` | Policy-routing ceiling: `config` follows each profile's YAML `mode`; `off`, `observe`, or `enforce` explicitly cap every profile. A profile cannot run above an explicit ceiling. |
| `--policy-routing-allow-remote-single-tenant` | `POLICY_ROUTING_ALLOW_REMOTE_SINGLE_TENANT` | `false` | Acknowledge running policy `observe`/`enforce` on a non-loopback bind for one trusted tenant. This adds no authentication or tenant isolation. |
| `--log-level` | `LOG_LEVEL` | `info` | Log level: `debug`, `info`, or `error` |
| `--streaming-upstream-timeout` | `STREAMING_UPSTREAM_TIMEOUT` | `1h0m0s` | Timeout for streaming upstream inference requests |

Native CLI and tray-app runs default to `127.0.0.1`. Container deployments that publish the proxy port must bind to `0.0.0.0`; the official image and sample Kubernetes manifest set `HOST=0.0.0.0` for that path.

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
