# Proxy-Mediated Code Execution

Proxy-mediated code execution is an optional, explicit proxy mode in which Vekil owns selected code-execution tool calls at the proxy layer instead of forwarding them to the downstream client. It is **disabled by default**; leaving it unset preserves Vekil's normal transparent-proxy behavior, and no tool call is ever intercepted.

When enabled, Vekil intercepts owned tool calls in a buffered (non-streaming) model response, executes them through a configured [`CodeExecutionBackend`](#backends), feeds the structured result back to the model over an internal conversation the client never sees, and returns only the final assistant response to the client.

## Why

Code execution has different security, durability, and scaling requirements from ordinary model proxying. This mode separates the proxy/control plane from the compute/execution environment:

- Vekil owns orchestration, policy, request/response handling, and model-protocol translation.
- A separate compute backend owns execution of model-generated commands.
- Model-generated code does not run inside the Vekil process or with Vekil's own credentials.

Vekil is not the harness. The downstream client may already have its own tool runtime; this mode lets Vekil optionally consume selected tool calls before the client sees them.

## Core invariant

```text
If Vekil executes a tool call, Vekil must not forward that tool call to the client.
```

Otherwise the client could execute the same command again. The internal loop fails closed on any unrecoverable state (see [Limitations](#limitations-mvp)) rather than surface an owned tool call.

## Flow

```text
client / harness
  → Vekil proxy
    → upstream model
      → model emits owned code-exec tool call
    ← Vekil intercepts the owned tool call (suppressed from client)
    → Vekil executes it through the compute backend
    → Vekil sends the tool_result back to the model internally
    → model returns final assistant response (loop repeats if more owned calls)
  ← Vekil returns only the final response to the client
```

## Enabling

The feature is configured through `VEKIL_CODE_EXEC_*` environment variables. It applies to the buffered chat paths: OpenAI `/v1/chat/completions` and Anthropic `/v1/messages` (both aggregate a forced-stream upstream response before returning). A working directory is required before any command will run.

```bash
export VEKIL_CODE_EXEC_ENABLED=true
export VEKIL_CODE_EXEC_OWNED_TOOLS=Bash          # comma-separated; case-insensitive
export VEKIL_CODE_EXEC_BACKEND=local             # only "local" today
export VEKIL_CODE_EXEC_WORKDIR=/srv/vekil-workspace
export VEKIL_CODE_EXEC_TIMEOUT_MS=30000
export VEKIL_CODE_EXEC_MAX_OUTPUT_BYTES=1048576
export VEKIL_CODE_EXEC_ENV_ALLOWLIST=PATH,HOME   # comma-separated; empty exports nothing
export VEKIL_CODE_EXEC_MAX_LOOP_DEPTH=8
```

| Variable | Default | Purpose |
|----------|---------|---------|
| `VEKIL_CODE_EXEC_ENABLED` | `false` | Master switch. The feature is inert unless this is `true`. |
| `VEKIL_CODE_EXEC_OWNED_TOOLS` | `Bash` | Tool names Vekil auto-executes. Matching is case-insensitive. |
| `VEKIL_CODE_EXEC_BACKEND` | `local` | Backend selector. Only the local process backend exists today. |
| `VEKIL_CODE_EXEC_WORKDIR` | _(none)_ | Working directory boundary. The local backend refuses to run without an existing directory. |
| `VEKIL_CODE_EXEC_TIMEOUT_MS` | `30000` | Per-command wall-clock timeout. |
| `VEKIL_CODE_EXEC_MAX_OUTPUT_BYTES` | `1048576` | Cap on captured stdout and stderr (each, independently). |
| `VEKIL_CODE_EXEC_ENV_ALLOWLIST` | _(empty)_ | Environment variable names passed through to executed commands. Empty means no variables are exposed. |
| `VEKIL_CODE_EXEC_MAX_LOOP_DEPTH` | `8` | Maximum internal model/tool turns before the loop fails closed. |

Invalid numeric or boolean values are ignored with the prior value kept, rather than failing startup. If the feature is enabled but the selected backend cannot be constructed, the feature disables itself with an error log instead of aborting the proxy.

## Distinguishing model-visible tools from owned tools

Vekil separates:

- **tools visible to the model** — the tools declared in the client request; Vekil forwards these upstream unchanged.
- **tools Vekil is allowed to auto-execute** — `VEKIL_CODE_EXEC_OWNED_TOOLS`, a subset that Vekil consumes.

A tool call for a name that is not owned is treated as a normal final response and forwarded to the client unchanged.

## Backends

Backends implement the `CodeExecutionBackend` interface (`RunCommand` plus `Name`). The interface is the extension point that lets execution route to different executors without the proxy loop hard-coding any single one. A future container, remote-sandbox, or HyperAgent-backed adapter attaches by satisfying the interface and registering a name; HyperAgent is possible as an adapter but is not the only execution model.

### Local process backend

The default `local` backend runs each command as a child process under `/bin/sh -c`. It enforces the policy on every request:

- **Timeout** — the command is killed after the configured wall-clock limit; the result is reported with `timed_out: true` and exit code `-1`.
- **Max output bytes** — stdout and stderr are captured independently up to the cap; overflow is dropped and the result is marked `truncated`.
- **Working directory** — execution refuses to start without an explicit, existing directory, so model-generated code never runs in the proxy's own working directory by default.
- **Environment allowlist** — only allowlisted variable names present in the proxy environment are exported. An empty allowlist yields an empty environment, so no proxy secrets leak into executed commands.

The local backend runs commands on the same host as the proxy. Operators who need stronger isolation should point the feature at a container or remote backend once available.

## Execution result shape

Each execution produces a structured result, serialized into the provider-specific tool result fed back to the model:

```json
{
  "tool_use_id": "toolu_123",
  "backend": "local",
  "command": "pytest",
  "exit_code": 1,
  "stdout": "...",
  "stderr": "...",
  "duration_ms": 18432,
  "timed_out": false,
  "truncated": false,
  "artifacts": []
}
```

A command that runs but exits non-zero is reported through `exit_code`, not as a backend error. A backend error (command could not be started at all) fails the loop closed.

## Security model

- Model-generated code has no implicit access to Vekil's process credentials or environment (empty environment unless allowlisted).
- No implicit access to the full host filesystem: an explicit working directory is required.
- Every executed command is audit-logged before execution, along with its exit code, timeout status, and duration.
- Code execution is never auto-enabled; it requires an explicit opt-in.

## Limitations (MVP)

- **Buffered / non-streaming only.** Interception runs on the aggregated (force-streamed then buffered) chat path. Client-requested streaming responses are not intercepted, so an owned tool call in a streamed response is forwarded normally. Streaming support would require synthesizing a final-only stream or careful SSE rewriting.
- **Single-request scope.** The hidden transcript that carries the internal model/tool loop lives only for the duration of one proxied request. There is no persistent shadow transcript across client turns.
- **No mixed ownership after execution starts.** If a response mixes owned and unowned tool calls, the loop fails closed rather than leak the owned call. A response with only unowned tool calls is forwarded unchanged.
- **Depth bound.** The internal loop is capped by `VEKIL_CODE_EXEC_MAX_LOOP_DEPTH`; exceeding it fails closed.
- **No local workspace mutation guarantees.** Execution is treated as isolated; results are returned as structured outputs rather than assumed to mutate the user's local project.

## Future extension points

- Container and remote-sandbox backends (and a HyperAgent adapter) behind the same `CodeExecutionBackend` interface.
- Richer workspace lifecycle (create/snapshot/restore/terminate) and artifact reporting.
- Optional command approval and network policy controls.
- Streaming support via final-only stream synthesis.
- Persistent shadow transcripts across client turns.
