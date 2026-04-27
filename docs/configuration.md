# Configuration

Vekil supports two runtime patterns:

- **Zero-config mode**: no `--providers-config`; the proxy uses its built-in GitHub Copilot upstream.
- **Explicit provider routing**: pass `--providers-config` with any mix of `copilot`, `azure-openai`, and `openai-codex`. If the config omits Copilot, GitHub auth is not used.

## Generic Flags

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--port` | `PORT` | `1337` | Listen port |
| `--host` | `HOST` | `0.0.0.0` | Listen host |
| `--token-dir` | `TOKEN_DIR` | `~/.config/vekil` | Token storage directory |
| `--providers-config` | `PROVIDERS_CONFIG` | unset | Path to JSON or YAML provider configuration for explicit provider routing |
| `--log-level` | `LOG_LEVEL` | `info` | Log level: `debug`, `info`, or `error` |
| `--streaming-upstream-timeout` | `STREAMING_UPSTREAM_TIMEOUT` | `1h0m0s` | Timeout for streaming upstream inference requests |
| `--metrics` | `METRICS` | `true` | Enable the Prometheus-compatible `/metrics` endpoint |
| `--no-metrics` | `NO_METRICS` | `false` | Disable the Prometheus-compatible `/metrics` endpoint |

When enabled, `/metrics` exposes Prometheus text format from the main HTTP server, including Go runtime metrics, process metrics, `vekil_build_info{version="..."}`, and a bounded request counter `vekil_http_requests_total{handler,code,method}`. Handler labels come from registered route patterns, not raw request paths or query strings.

## Copilot Header Overrides

These overrides only affect Copilot-backed upstream requests.

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--copilot-editor-version` | `COPILOT_EDITOR_VERSION` | `vscode/1.95.0` | Upstream `editor-version` header |
| `--copilot-plugin-version` | `COPILOT_PLUGIN_VERSION` | `copilot-chat/0.26.7` | Upstream `editor-plugin-version` header |
| `--copilot-user-agent` | `COPILOT_USER_AGENT` | `GitHubCopilotChat/0.26.7` | Upstream `user-agent` header |
| `--copilot-github-api-version` | `COPILOT_GITHUB_API_VERSION` | `2025-04-01` | Upstream `x-github-api-version` header |

## Provider Authentication

### GitHub Copilot

For CI or other non-interactive environments, set `COPILOT_GITHUB_TOKEN` to a GitHub token for a user with GitHub Copilot access. This is the only GitHub token environment variable Vekil consumes directly; it overrides cached Vekil login state and is exchanged for a short-lived Copilot token at startup.

Vekil intentionally ignores generic GitHub token variables such as `GH_TOKEN` and `GITHUB_TOKEN`. If you want Vekil to use an authenticated GitHub CLI account, opt in explicitly with `vekil login --github-cli` or `vekil login --gh`; Vekil then runs `gh auth token --hostname github.com` for Copilot access and keeps that token in memory only, without copying it into Vekil's `access-token` or `api-key.json` caches.

Plain `vekil login` refreshes an existing Vekil-managed login when possible, otherwise starts GitHub's device-code flow. Use `vekil login --force` to force a new device-code flow even if an existing login can still refresh. A device-code sign-in disables GitHub CLI auto sign-in because the active account is then managed by Vekil rather than by `gh`.

After `vekil logout` or menubar Sign Out, Vekil clears its cached credentials, disables GitHub CLI auto sign-in, and suppresses automatic GitHub CLI reuse until you explicitly opt back in with `vekil login --github-cli` or `vekil login --gh`. `COPILOT_GITHUB_TOKEN` remains an explicit override and still works while signed out.

### OpenAI Codex

OpenAI Codex uses the ChatGPT/Codex CLI credentials in `~/.codex/auth.json` by default. Set `CODEX_HOME` if your Codex home lives elsewhere.

OpenAI Codex requires file-based ChatGPT auth from `codex login`; API-key auth and OS keychain-backed credentials are not read by the proxy.

### Azure OpenAI

Azure OpenAI credentials are configured in the provider entry, using either `api_key` or `api_key_env`.

## Provider Routing

Use `--providers-config` when you want explicit ownership of public model IDs across providers such as GitHub Copilot, Azure OpenAI, and OpenAI Codex. Provider config files can be JSON (`.json`) or YAML (`.yaml`/`.yml`).

You can run Azure-only or Codex-only configs, or mix those providers with Copilot behind the same local endpoint.

### Azure-Only Example

```json
{
  "providers": [
    {
      "id": "azure-openai",
      "type": "azure-openai",
      "default": true,
      "base_url": "https://myresource.cognitiveservices.azure.com/openai/v1",
      "api_key_env": "AZURE_OPENAI_API_KEY",
      "models": [
        {
          "public_id": "gpt-5.4-pro",
          "deployment": "gpt-5.4-pro",
          "endpoints": ["/responses"],
          "name": "GPT-5.4 Pro"
        }
      ]
    }
  ]
}
```

The same config can be written as YAML:

```yaml
providers:
  - id: azure-openai
    type: azure-openai
    default: true
    base_url: https://myresource.cognitiveservices.azure.com/openai/v1
    api_key_env: AZURE_OPENAI_API_KEY
    models:
      - public_id: gpt-5.4-pro
        deployment: gpt-5.4-pro
        endpoints:
          - /responses
        name: GPT-5.4 Pro
```

### Copilot + Azure Example

```json
{
  "providers": [
    {
      "id": "copilot",
      "type": "copilot",
      "default": true,
      "exclude_models": ["gpt-5.4-pro"]
    },
    {
      "id": "azure-openai",
      "type": "azure-openai",
      "base_url": "https://myresource.cognitiveservices.azure.com/openai/v1",
      "api_key_env": "AZURE_OPENAI_API_KEY",
      "models": [
        {
          "public_id": "gpt-5.4-pro",
          "deployment": "gpt-5.4-pro",
          "endpoints": ["/responses"],
          "name": "GPT-5.4 Pro"
        }
      ]
    }
  ]
}
```

### OpenAI Codex Subscription Example

```json
{
  "providers": [
    {
      "id": "copilot",
      "type": "copilot",
      "default": true
    },
    {
      "id": "openai-codex",
      "type": "openai-codex",
      "include_models": ["gpt-5.5"]
    }
  ]
}
```

The same Codex config can be written as YAML:

```yaml
providers:
  - id: copilot
    type: copilot
    default: true
  - id: openai-codex
    type: openai-codex
    include_models:
      - gpt-5.5
```

Routing rules:

- Clients keep using plain model IDs such as `gpt-5.4-pro`.
- Azure `deployment` is the upstream model name; the proxy rewrites the public ID before forwarding.
- Azure `models[]` remains the routing source of truth. The proxy does not autodiscover new Azure deployments for inference.
- OpenAI Codex discovers models dynamically from its upstream `/models` endpoint and exposes only models that are listed and supported in the API.
- OpenAI Codex models are `/responses`-only. The proxy rejects `/chat/completions` for those models instead of probing an unsupported route.
- Azure `base_url` must be an absolute URL whose path ends with either the OpenAI-compatible `/openai/v1` path or the legacy `/openai` path, with no query string or fragment.
- Azure AI Foundry inference URLs ending in `/models` are not supported in `type: "azure-openai"` configs. Use the corresponding OpenAI-compatible `.../openai/v1` endpoint instead.
- For `/openai/v1` base URLs, omit `api_version`; the proxy calls `/chat/completions`, `/responses`, and `/models` directly with no `api-version` query string.
- For legacy `/openai` base URLs, set `api_version`; the proxy appends `api-version=...` to upstream requests.
- Public model IDs are global across all providers. Startup fails if two providers expose the same ID.
- `include_models` is the recommended way to use dynamic providers without prefixes. It lets you opt into only the discovered model IDs that should belong to that provider.
- `exclude_models` lets one provider give ownership of a public ID to another provider.
- Only one Copilot provider is supported in a config today.
- `models[].endpoints` is an allowlist, not a guess. Keep it limited to the routes you have validated for that deployment.
- Static provider models can also advertise richer Codex `/v1/models` metadata via optional fields on each `models[]` entry: `model_picker_category`, `reasoning_effort`, `vision`, `parallel_tool_calls`, and `context_window`. Without those fields, the proxy exposes a minimal but valid model entry.
- For Azure OpenAI, `/v1/models` only does a best-effort metadata overlay for each configured `models[]` entry by probing Azure's upstream `/models` response. The proxy matches by `public_id` first, then by `deployment` for aliased models.
- Azure's upstream `/models` catalog can omit Codex-style fields entirely. The proxy only copies fields that Azure already returns; it does not derive reasoning levels, vision, parallel tool calls, model picker metadata, or context window from other Azure docs or capability hints.
- Explicit `models[]` metadata overrides Azure `/models` overlay metadata. Configured public IDs and endpoint allowlists always win, and the proxy falls back to the static entry if the Azure `/models` probe fails or returns a sparse payload.
- The example Azure `gpt-5.4-pro` model shown above is `/responses`-only. Do not advertise `/chat/completions` for that model unless you have verified native support.

Use the examples above as a starting point for your local providers config file. JSON and YAML use the same snake_case field names.

## WebSocket Session Tuning

These settings affect the Codex-style `GET /v1/responses` websocket bridge.

Important:

- This websocket bridge is proxy-owned and still forwards upstream over HTTP `/responses`.
- It is separate from Azure OpenAI's native `/realtime` websocket and WebRTC APIs.

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--responses-ws-turn-state-delta` | `RESPONSES_WS_TURN_STATE_DELTA` | `false` | Experimental: if upstream returns `X-Codex-Turn-State`, try replaying only the newest delta input on the next turn |
| `--responses-ws-disable-auto-compact` | `RESPONSES_WS_DISABLE_AUTO_COMPACT` | `false` | Disable automatic session-history compaction |
| `--responses-ws-auto-compact-max-items` | `RESPONSES_WS_AUTO_COMPACT_MAX_ITEMS` | `48` | Auto-compact when history item count exceeds this threshold |
| `--responses-ws-auto-compact-max-bytes` | `RESPONSES_WS_AUTO_COMPACT_MAX_BYTES` | `262144` | Auto-compact when raw history byte size exceeds this threshold |
| `--responses-ws-auto-compact-keep-tail` | `RESPONSES_WS_AUTO_COMPACT_KEEP_TAIL` | `12` | Keep this many most recent items verbatim after compaction |

## Suggested Debug Run

```bash
./vekil \
  --log-level debug \
  --responses-ws-turn-state-delta \
  --responses-ws-auto-compact-max-items 64 \
  --responses-ws-auto-compact-max-bytes 524288 \
  --responses-ws-auto-compact-keep-tail 16
```

With `--log-level debug`, websocket bridge logs include `delta_attempted`, `delta_fallback`, `auto_compacted`, `history_items`, `history_bytes`, and compaction before/after sizes.
