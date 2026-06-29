# Tool Optimizers

`tool_optimizers` is an optional top-level block in the JSON/YAML [providers config](provider-routing.md), alongside `providers`. It is disabled by default, and leaving it unset preserves the normal passthrough behavior.

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

When running Vekil in Docker, use the `ghcr.io/sozercan/vekil:latest-rtk` image variant or build one locally with `docker build --target runtime-rtk -t vekil:rtk .`. The default image intentionally does not include RTK. In the RTK variant, prefer `path: /usr/local/bin/rtk` so the config does not depend on `PATH` lookup.

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

## `exec_json` Provider Protocol

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

## `rtk_cli` Provider

`rtk_cli` defaults `path` to `rtk` and adapts optimizer calls to RTK commands:

- Command rewrite runs `rtk hook check -- <command>` and treats non-empty stdout as the rewritten command.
- Output reduction runs `rtk pipe`, passing the tool output on stdin. If Vekil resolved a supported hint, it adds `--filter <hint>`.

Vekil's built-in hint resolver may emit `git-diff`, `git-status`, `pytest`, `cargo-test`, `go-test`, or `vitest`. The `rtk_cli` adapter accepts those hints plus `cargo`, `go-build`, `tsc`, `grep`, `rg`, `find`, `fd`, `git-log`, `mypy`, `ruff-check`, `ruff-format`, and `prettier`.
