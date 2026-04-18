# Configuration

## Core Flags

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--port` | `PORT` | `1337` | Listen port |
| `--host` | `HOST` | `0.0.0.0` | Listen host |
| `--token-dir` | `TOKEN_DIR` | `~/.config/vekil` | Token storage directory |
| `--providers-config` | `PROVIDERS_CONFIG` | unset | Path to JSON provider configuration for multi-provider model routing |
| `--log-level` | `LOG_LEVEL` | `info` | Log level: `debug`, `info`, or `error` |
| `--streaming-upstream-timeout` | `STREAMING_UPSTREAM_TIMEOUT` | `1h0m0s` | Timeout for streaming upstream inference requests |
| `--copilot-editor-version` | `COPILOT_EDITOR_VERSION` | `vscode/1.95.0` | Upstream `editor-version` header |
| `--copilot-plugin-version` | `COPILOT_PLUGIN_VERSION` | `copilot-chat/0.26.7` | Upstream `editor-plugin-version` header |
| `--copilot-user-agent` | `COPILOT_USER_AGENT` | `GitHubCopilotChat/0.26.7` | Upstream `user-agent` header |
| `--copilot-github-api-version` | `COPILOT_GITHUB_API_VERSION` | `2025-04-01` | Upstream `x-github-api-version` header |

## Non-Interactive Authentication

For CI or other non-interactive environments, the proxy also accepts a GitHub token from `COPILOT_GITHUB_TOKEN`, `GH_TOKEN`, or `GITHUB_TOKEN`.

If one of these is set, it overrides any cached login state and is exchanged for a short-lived Copilot token at startup.

If your provider config does not include a Copilot provider, startup skips the GitHub auth check.

## Multi-Provider Routing

Use `--providers-config` to expose non-Copilot models such as Azure OpenAI deployments behind the same proxy endpoint.

Example:

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
      "api_version": "2025-04-01-preview",
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

Routing rules:

- Clients keep using plain model IDs such as `gpt-5.4-pro`.
- Azure `deployment` is the upstream model name; the proxy rewrites the public ID before forwarding.
- Azure `base_url` must include the OpenAI-compatible `/openai/v1` prefix because the proxy appends routes like `/chat/completions`, `/responses`, and `/models` directly.
- Public model IDs are global across all providers. Startup fails if two providers expose the same ID.
- `exclude_models` lets one provider give ownership of a public ID to another provider.
- When `api_version` is set for Azure OpenAI, the proxy appends `api-version=...` to upstream requests.
- Only one Copilot provider is supported in a config today.
- `models[].endpoints` is an allowlist, not a guess. Keep it limited to the routes you have validated for that deployment.
- The example Azure `gpt-5.4-pro` model shown above is `/responses`-only. Do not advertise `/chat/completions` for that model unless you have verified native support.

Use the JSON example above as a starting point for your local providers config file.

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
