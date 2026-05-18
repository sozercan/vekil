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

## Copilot Header Overrides

These overrides only affect Copilot-backed upstream requests.

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--copilot-editor-version` | `COPILOT_EDITOR_VERSION` | `vscode/1.95.0` | Upstream `editor-version` header |
| `--copilot-plugin-version` | `COPILOT_PLUGIN_VERSION` | `copilot-chat/0.26.7` | Upstream `editor-plugin-version` header |
| `--copilot-user-agent` | `COPILOT_USER_AGENT` | `GitHubCopilotChat/0.26.7` | Upstream `user-agent` header |
| `--copilot-integration-id` | `COPILOT_INTEGRATION_ID` | `vscode-chat` | Upstream `copilot-integration-id` header |
| `--copilot-github-api-version` | `COPILOT_GITHUB_API_VERSION` | `2025-05-01` | Upstream `x-github-api-version` header |
| `--copilot-openai-intent` | `COPILOT_OPENAI_INTENT` | unset (`conversation-panel` for chat/responses) | Upstream `openai-intent` header |

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

### Copilot Provider Header Profiles

`type: copilot` providers can define a `headers` block with a provider-wide `default` profile and endpoint-specific `chat_completions` and `responses` profiles. Endpoint-specific values override `headers.default`, which overrides the global Copilot header flags/environment variables, which then fall back to the built-in defaults. Omitted fields inherit from the lower-precedence profile. The built-in `openai-intent: conversation-panel` fallback is endpoint-aware: it is applied to upstream `/chat/completions` and `/responses` calls, while upstream `/models` calls send `openai-intent` only when you configure it explicitly through `--copilot-openai-intent`, `COPILOT_OPENAI_INTENT`, or a provider header profile.

```yaml
providers:
  - id: copilot
    type: copilot
    default: true
    headers:
      default:
        editor_version: vscode/1.95.0
        editor_plugin_version: copilot-chat/0.26.7
        user_agent: GitHubCopilotChat/0.26.7
        copilot_integration_id: vscode-chat
        github_api_version: "2025-05-01"
      chat_completions:
        openai_intent: conversation-panel
      responses:
        openai_intent: agent-mode
```

Supported header fields are `editor_version`, `editor_plugin_version`, `user_agent`, `copilot_integration_id`, `github_api_version`, and `openai_intent`. The `chat_completions` profile applies only to upstream `/chat/completions` calls, and the `responses` profile applies only to upstream `/responses` calls. Other Copilot upstream requests, including `/models` and readiness probes, use `headers.default` plus global/default fallback values for non-intent headers. `/models` omits `openai-intent` unless it is explicitly configured globally or in `headers.default`; put `openai_intent` in endpoint-specific profiles if you only want it on chat/response requests.

Copilot header profiles only apply to `type: copilot` providers. Non-Copilot providers do not receive configured Copilot headers, and the proxy strips Copilot-identifying client headers such as `authorization`, `editor-version`, `editor-plugin-version`, `user-agent`, `copilot-integration-id`, `x-github-api-version`, `x-request-id`, and `openai-intent` before applying provider-specific authentication.

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
- For Copilot-discovered models, Codex-compatible `/v1/models` metadata treats `capabilities.limits.max_prompt_tokens` as the active `context_window` and keeps `max_context_window_tokens` as `max_context_window`. If Copilot omits the prompt cap, the proxy falls back to the total context window.
- `models[].endpoints` is an allowlist, not a guess. Keep it limited to the routes you have validated for that deployment.
- Static provider models can also advertise richer Codex `/v1/models` metadata via optional fields on each `models[]` entry: `model_picker_category`, `reasoning_effort`, `vision`, `parallel_tool_calls`, and `context_window`. Without those fields, the proxy exposes a minimal but valid model entry.
- For Azure OpenAI, `/v1/models` only does a best-effort metadata overlay for each configured `models[]` entry by probing Azure's upstream `/models` response. The proxy matches by `public_id` first, then by `deployment` for aliased models.
- Azure's upstream `/models` catalog can omit Codex-style fields entirely. The proxy only copies fields that Azure already returns; it does not derive reasoning levels, vision, parallel tool calls, model picker metadata, or context window from other Azure docs or capability hints.
- Explicit `models[]` metadata overrides Azure `/models` overlay metadata. Configured public IDs and endpoint allowlists always win, and the proxy falls back to the static entry if the Azure `/models` probe fails or returns a sparse payload.
- The example Azure `gpt-5.4-pro` model shown above is `/responses`-only. Do not advertise `/chat/completions` for that model unless you have verified native support.

Use the examples above as a starting point for your local providers config file. JSON and YAML use the same snake_case field names.

## Tool Optimizers

`tool_optimizers` is an optional top-level block in the JSON/YAML providers config, alongside `providers`. It is disabled by default, and leaving it unset preserves the normal passthrough behavior.

Tool optimizers apply to all API surfaces: `/v1/responses` (HTTP and proxy-owned websocket bridge), `/v1/chat/completions`, Anthropic `/v1/messages`, and Gemini endpoints. They currently target shell-style function calls and can run two independent stages:

- `command_rewrite` rewrites matching shell function-call commands in non-streaming Responses responses before returning them to the client. Chat completions, Anthropic, and Gemini handlers capture tool calls for output reduction but do not rewrite commands today.
- `output_reduce` reduces captured `function_call_output` (or equivalent tool result) payloads in later requests after Vekil has seen the matching tool call in the same request/session scope.

Important behavior:

- The feature, `command_rewrite`, and `output_reduce` are all disabled by default.
- Shell matching defaults to the tool name `shell_command`.
- `tools.shell_function_calls.enabled` defaults to `true` once `tool_optimizers.enabled: true` is set, unless you explicitly set it to `false`.
- The shell command argument path is configurable with `tools.shell_function_calls.command_arg_path`. It defaults to `/command`, and may be any valid JSON Pointer to a string field in the function-call arguments (for example `/cmd` for an `exec_command` tool or `/shell/cmd` for nested arguments).
- `command_rewrite.streaming_mode` defaults to `disabled`, which is the only supported value today. Command rewrites are applied only when Vekil inspects non-streaming Responses response bodies; websocket/streaming paths capture command context for later output reduction but do not rewrite commands today.
- For `/v1/responses`, stable scope headers are preferred. Headerless clients can still correlate ordinary continuations through `previous_response_id`, and replay bodies that include a `function_call` before its output can be reduced within that same request.
- For chat-style APIs, cross-request output reduction requires a stable scope header such as `session_id` or Codex thread/window headers. `X-Client-Request-Id` is treated as per-request and is not used for chat-style scoping. Headerless chat requests are only reduced when the request replays the matching assistant `tool_calls` before the tool output in the same `messages` array; Vekil does not share chat tool-call context globally across conversations.
- Optimizers are fail-open. External errors, timeouts, invalid JSON, unsupported provider config, or invalid replacements are ignored and the original payload is used.
- `output_reduce.timeout_ms: 0` disables the per-provider optimizer timeout while still honoring the surrounding request context. `output_reduce.min_input_bytes: 0` disables the minimum-size gate, and `output_reduce.max_input_bytes: 0` disables the maximum-size gate.
- Command replacements must be changed, non-empty after trimming, at most `32768` bytes, and contain no NUL characters. Internal newlines and carriage returns are allowed for multi-line shell snippets.
- Output replacements must be changed and non-empty after trimming.

Recommended RTK example:

```yaml
tool_optimizers:
  enabled: true
  command_rewrite:
    enabled: true
  output_reduce:
    enabled: true
  providers:
    - type: rtk_cli
```

With this config, Vekil asks RTK to rewrite shell commands and reduce shell output where supported. The minimal `rtk_cli` provider relies on two defaults: `path` defaults to `rtk`, and omitted `stages` means the provider is eligible for both `command_rewrite` and `output_reduce`. For example, depending on RTK policy, command rewrite may replace common shell commands with RTK-aware equivalents such as `rtk ls`, `rtk read`, or `rtk grep`.

Advanced custom optimizer example:

```yaml
tool_optimizers:
  enabled: true
  command_rewrite:
    enabled: true
  output_reduce:
    enabled: true
  providers:
    - id: local-optimizer
      type: exec_json
      path: /usr/local/bin/vekil-tool-optimizer
      args:
        - --profile
        - default
      stages:
        - command_rewrite
        - output_reduce
```

Defaults:

| Setting | Default |
|---------|---------|
| `tool_optimizers.enabled` | `false` |
| `tools.shell_function_calls.enabled` | `true` when tool optimizers are enabled and the field is omitted |
| `tools.shell_function_calls.names` | `["shell_command"]` |
| `tools.shell_function_calls.command_arg_path` | `/command` |
| `command_rewrite.enabled` | `false` |
| `command_rewrite.streaming_mode` | `disabled` |
| `command_rewrite.timeout_ms` | `200` |
| `output_reduce.enabled` | `false` |
| `output_reduce.timeout_ms` | `500` |
| `output_reduce.min_input_bytes` | `20000` |
| `output_reduce.max_input_bytes` | `500000` |
| `tool_optimizers.providers[].stages` | both `command_rewrite` and `output_reduce` when omitted or empty |
| `tool_optimizers.providers[].path` (`rtk_cli`) | `rtk` |
| `tool_optimizers.providers[].max_stdout_bytes` | `1048576` |
| `tool_optimizers.providers[].max_stderr_bytes` | `65536` |

Provider entries support these `type` values:

- `rtk_cli` runs the RTK command-line adapter and is the recommended local optimizer path. `path` defaults to `rtk`.
- `exec_json` is an advanced/custom protocol that runs a local executable, sends one JSON request on stdin, and expects one JSON response on stdout. `path` is required.
- `noop` accepts config and never changes payloads; it is useful for tests and dry runs.

Provider entries are enabled by default; set `enabled: false` to keep an entry in the file without using it. Each provider can set `stages`. If `stages` is omitted or empty, the provider is eligible for both `command_rewrite` and `output_reduce`; otherwise list only the stage names it should handle: `command_rewrite` and/or `output_reduce`. Providers run in config order, and the first valid changed result wins.

### `exec_json` Provider Protocol

`exec_json` requests and responses use `schema: "vekil.tool_optimizer.v1"`.

Command rewrite request:

```json
{
  "schema": "vekil.tool_optimizer.v1",
  "operation": "rewrite_command",
  "tool_name": "shell_command",
  "call_id": "call_123",
  "command": "go test ./...",
  "metadata": {}
}
```

Command rewrite response:

```json
{
  "changed": true,
  "command": "go test ./proxy/...",
  "reason": "narrowed package"
}
```

Output reduce request:

```json
{
  "schema": "vekil.tool_optimizer.v1",
  "operation": "reduce_output",
  "tool_name": "shell_command",
  "call_id": "call_123",
  "command": "go test ./...",
  "filter_hint": "go-test",
  "output": "...",
  "metadata": {}
}
```

Output reduce response:

```json
{
  "changed": true,
  "output": "...reduced output...",
  "reason": "kept failing tests"
}
```

Set `changed: false`, omit the replacement field, or return an invalid replacement when the original command/output should pass through unchanged.

### `rtk_cli` Provider

`rtk_cli` defaults `path` to `rtk` and adapts optimizer calls to RTK commands:

- Command rewrite runs `rtk hook check -- <command>` and treats non-empty stdout as the rewritten command.
- Output reduction runs `rtk pipe`, passing the tool output on stdin. If Vekil resolved a supported hint, it adds `--filter <hint>`.

Vekil's built-in hint resolver may emit `git-diff`, `git-status`, `pytest`, `cargo-test`, `go-test`, or `vitest`. The `rtk_cli` adapter accepts those hints plus `cargo`, `go-build`, `tsc`, `grep`, `rg`, `find`, `fd`, `git-log`, `mypy`, `ruff-check`, `ruff-format`, and `prettier`.

## WebSocket Session Tuning

These settings affect the Codex-style `GET /v1/responses` websocket bridge. The bridge is disabled by default; when disabled, websocket upgrade attempts receive `426 Upgrade Required` so Codex-style clients can fall back to HTTP `/v1/responses`.

Important:

- This websocket bridge is proxy-owned and still forwards upstream over HTTP `/responses`.
- It is separate from Azure OpenAI's native `/realtime` websocket and WebRTC APIs.
- Each websocket session is serialized: one active turn is processed at a time. Vekil does not multiplex turns or implement Copilot-style request superseding. Closing the websocket ends the session; once Vekil observes the disconnect, it stops relaying and closes the upstream response body while the upstream request context remains governed by the proxy streaming timeout.

| Flag | Env Var | Default | Description |
|------|---------|---------|-------------|
| `--responses-ws-enabled` | `RESPONSES_WS_ENABLED` | `false` | Enable the proxy-owned Codex websocket bridge on `GET /v1/responses` |
| `--responses-ws-turn-state-delta` | `RESPONSES_WS_TURN_STATE_DELTA` | `false` | Experimental: if upstream returns `X-Codex-Turn-State`, try replaying only the newest delta input on the next turn |
| `--responses-ws-disable-auto-compact` | `RESPONSES_WS_DISABLE_AUTO_COMPACT` | `false` | Disable automatic session-history compaction |
| `--responses-ws-auto-compact-max-items` | `RESPONSES_WS_AUTO_COMPACT_MAX_ITEMS` | `8` | Auto-compact when history item count exceeds this threshold |
| `--responses-ws-auto-compact-max-bytes` | `RESPONSES_WS_AUTO_COMPACT_MAX_BYTES` | `32768` | Auto-compact when raw history byte size exceeds this threshold |
| `--responses-ws-auto-compact-keep-tail` | `RESPONSES_WS_AUTO_COMPACT_KEEP_TAIL` | `4` | Keep this many most recent items verbatim after websocket auto-compaction and as the starting tail for replay-like `POST /v1/responses` 413 compaction; the replay fallback aligns tool/call boundaries and may halve this per request if compacted retries still return 413 |
| `--compact-upstream-chunk-bytes` | `COMPACT_UPSTREAM_CHUNK_BYTES` | `4194304` | Initial target body size for chunked compaction retries after an upstream `413` (`/v1/responses/compact` and `POST /v1/responses` replay compaction). Default sized below the empirically observed Copilot `/responses` payload cap of `5 MiB` (`5,242,880` bytes); `4 MiB` leaves headroom for proxy-owned overhead. Halved on each recursive `413` (and eagerly halved when a sub-target body still 413s) down to a `64 KiB` floor. Learned smaller targets are cached in memory for 30 minutes per provider/model/endpoint so later compaction fallbacks skip known-doomed larger chunks; tune up for providers like Azure that accept larger bodies, or down if your upstream's payload cap is below `4 MiB` |
| `--compact-upstream-chunk-concurrency` | `COMPACT_UPSTREAM_CHUNK_CONCURRENCY` | `4` | Maximum number of sibling chunk compaction calls to run concurrently after the first chunk succeeds. Lower this to reduce upstream burst pressure; raise it only when the upstream can safely handle more parallel `/responses` compaction calls |
| `--compact-upstream-max-attempts` | `COMPACT_UPSTREAM_MAX_ATTEMPTS` | `48` | Maximum logical compaction calls a compact/replay `413` fallback may issue per inbound request. Each call may add one extra HTTP POST when the configured model is unsupported (model-fallback) and is independently subject to the shared transport-retry policy on transient upstream failures (429/502/503/504). Sized to the documented 64 MiB inbound ceiling at the default 4 MiB chunk target with one round of recursive halving plus headroom; meant as a runaway-fanout safety net rather than gatekeeping legitimate large requests or duplicating the transport retry budget. On exhaustion the original `413` is returned |

## Suggested Debug Run

```bash
./vekil \
  --log-level debug \
  --responses-ws-enabled \
  --responses-ws-turn-state-delta \
  --responses-ws-auto-compact-max-items 64 \
  --responses-ws-auto-compact-max-bytes 524288 \
  --responses-ws-auto-compact-keep-tail 16
```

With `--log-level debug`, websocket bridge logs include `delta_attempted`, `delta_fallback`, `auto_compacted`, `history_items`, `history_bytes`, and compaction before/after sizes.
