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

## Responses WebSocket Bridge

The Codex-style `GET /v1/responses` websocket bridge is disabled by default and remains a proxy-owned transport over upstream HTTP `/responses`. See [Responses WebSocket Bridge](responses-websocket.md) for websocket flags, auto-compaction settings, chunked compaction knobs, and a debug run example.


## Prometheus metrics

Vekil exposes a Prometheus-compatible `/metrics` endpoint beside `/stats.json`. The endpoint is scrape-only and backed by the in-memory request statistics collector used by the dashboard. It includes request counters, non-streaming latency histogram buckets, token counters, retry counts, upstream error counts, an in-flight request gauge, build info, and minimal Go runtime gauges. Metrics are enabled by default; disable the route with `--metrics=false`, `--no-metrics`, `METRICS=false`, or `NO_METRICS=true`.

Important metric families include:

- `vekil_requests_total{provider,public_model,endpoint,status}`
- `vekil_request_duration_seconds_bucket{provider,public_model,endpoint,le}` plus `_sum` and `_count`
- `vekil_tokens_total{provider,public_model,direction}` for disjoint `prompt` and `completion` directions
- `vekil_tokens_reported_total{provider,public_model}` for upstream-reported total tokens, including total-only providers
- `vekil_tokens_cached_total{provider,public_model}` and `vekil_tokens_reasoning_total{provider,public_model}` for token sub-components
- `vekil_retries_total` and `vekil_retries_by_reason_total{provider,public_model,reason}`
- `vekil_upstream_errors_total{provider,public_model,code}`
- `vekil_inflight_requests{provider}`
- `vekil_build_info{version,go_version,commit}`

Request counters are emitted for each bounded provider/model/endpoint key that observed traffic, with disjoint `status="success"` and `status="error"` series. Use Prometheus aggregation (for example `sum by (provider)` or `sum without (public_model,endpoint,status)`) for rollups instead of relying on a separate blank-label aggregate row.

Latency is exported as a Prometheus histogram with bucket boundaries at `0.005`, `0.01`, `0.025`, `0.05`, `0.1`, `0.25`, `0.5`, `1`, `2.5`, `5`, and `10` seconds, plus the standard `+Inf` bucket. The `_sum` and `_count` series are cumulative over the same non-streaming latency observations; streamed requests are excluded because their wall-clock duration reflects connection lifetime rather than model latency.

A ready-to-import starter dashboard is available at [`docs/grafana-dashboard.json`](grafana-dashboard.json). Import it in Grafana and select your Prometheus data source for the `DS_PROMETHEUS` variable.
