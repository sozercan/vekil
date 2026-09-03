# Agent Launchers

Vekil can start a short-lived loopback proxy and run a supported coding agent
against it in one command. The proxy uses an OS-selected port by default,
authenticates the child with a random per-launch token, prints a request/token
summary, and stops when the agent exits. Claude Code and Codex CLI can either
choose their normal CLI default through the global public model namespace or be
pinned to one validated model. GitHub Copilot CLI always requires a pinned
model.

## Supported targets

| Target | Model selection | Minimum tested contract | Compatible model endpoint |
|--------|-----------------|-------------------------|---------------------------|
| Claude Code | Optional `--model`; otherwise Claude's CLI default | Claude Code 2.1.83+ | `/v1/messages`, `/chat/completions`, or `/responses` |
| OpenAI Codex CLI | Optional `--model`; otherwise Codex's CLI default | Codex CLI 0.137.0+ | `/responses`, or a policy-owned `/chat/completions` model through bounded stateless compatibility |
| GitHub Copilot CLI | `--model` required | GitHub Copilot CLI 1.0.0+ | `/responses` or `/chat/completions` |

```bash
vekil launch claude
vekil launch codex
vekil launch claude --model claude-sonnet-4.5
vekil launch codex --model gpt-5.4-mini
vekil launch copilot --model gpt-5.4-mini
```

Use the same provider configuration accepted by the normal server:

```bash
vekil launch codex \
  --providers-config /path/to/providers.yaml \
  --model my-responses-model
```

Arguments after `--` are forwarded to the selected agent:

```bash
vekil launch claude --model claude-opus-5 -- \
  --resume 56d7498d-6b2e-47ca-92a6-92ddee84ab25

vekil launch codex -- resume --last

vekil launch copilot --model gpt-5.4-mini -- --continue

vekil launch codex -- \
  exec --ephemeral --skip-git-repo-check "Review this workspace"

vekil launch copilot --model gpt-5.4-mini -- \
  --allow-all-tools -p "Review this workspace" -s
```

Foreground resume and fork arguments remain attached to the supervised child
process and are forwarded. Each launcher still reserves model, provider,
remote-session, background, server, and other arguments that could replace the
validated routing layer or leave work running outside the ephemeral proxy
lifecycle. To pin Claude Code or Codex CLI, use the launcher's `--model` before
`--`; forwarded model overrides remain rejected.

The proxy and its local credential are ephemeral. Agent-owned conversation
history can persist across launches, and a resumed conversation uses the new
launch's provider routing and optional model pin.

## Model selection modes

Claude Code and Codex CLI support two launch modes:

- **Delegated CLI default**: omit `--model`. Vekil does not choose or pin a
  model; the agent resolves its normal configured or built-in default. The
  ephemeral proxy exposes the same global public model namespace as a normal
  Vekil server. Because there is no concrete model to inspect at startup, Vekil
  does not perform model-specific existence, endpoint-compatibility, or
  capability checks. The CLI's eventual model must exist in that public
  namespace and be compatible with the target's endpoint, or the request fails
  when the agent uses it.
- **Pinned model**: supply `--model ID`. Vekil validates that public model at
  startup, checks target compatibility, narrows the ephemeral proxy to that one
  model and its configured aliases, and pins the agent's model selection.

GitHub Copilot CLI has no delegated mode. Its custom-provider/BYOK contract
requires an explicit model, so `vekil launch copilot` rejects startup unless
`--model` is supplied.

Policy-owned Codex compatibility also requires launcher-level `--model`. A
delegated Codex session can use normal direct Responses models, but Vekil cannot
know its eventual CLI-selected model early enough to conditionally disable
hosted web search, remote compaction, freeform apply-patch, and inherited speed
tiers. If a delegated Codex default resolves to `owned_by: vekil-policy`, pin
that ID explicitly instead.

## Lifecycle

Every launcher:

1. loads Vekil provider configuration and credentials;
2. starts a proxy on `127.0.0.1:0` unless `--port` is provided;
3. completes startup authentication and provider-model validation;
4. initializes configured policy routing using the YAML profile mode or an explicit `--policy-routing` / `POLICY_ROUTING_MODE` safety ceiling;
5. waits for `/readyz` and, when `--model` is supplied, verifies that model in `/v1/models`;
6. validates the installed agent binary and, for a pinned model, endpoint compatibility;
7. starts the agent with target-specific temporary routing configuration and either delegated or pinned model selection;
8. forwards signals and supervises the managed child process group or Job Object;
9. prints a usage summary and stops the proxy when the agent exits.

With `--model`, the ephemeral proxy scopes policy-controller construction and
live classifier preflight to the selected public model, including its configured
aliases. Unrelated policy profiles cannot require credentials, send classifier
preflights, or fail startup for that pinned session. Without `--model`, Claude
Code and Codex CLI use the normal global public namespace and therefore the
normal global policy-controller startup behavior.

## Common options

| Flag | Purpose |
|------|---------|
| `--model ID` | Public Vekil model ID to validate, scope, and pin. Optional for Claude Code and Codex CLI; required for GitHub Copilot CLI. |
| `--providers-config SOURCE` | Local path or HTTP(S) URL to JSON or YAML provider configuration. |
| `--token-dir PATH` | Copilot token storage directory used by Vekil. |
| `--port PORT` | Local proxy port. Default `0` lets the OS allocate one. |
| `--binary PATH` | Agent executable override. |
| `--startup-timeout DURATION` | Maximum startup/authentication/readiness time. |
| `--streaming-upstream-timeout DURATION` | Upstream streaming timeout. |
| `--policy-routing config|off|observe|enforce` | Policy-routing mode. `config` follows YAML profile modes and is the default; other values apply a process-wide safety ceiling. |
| `--proxy-log PATH` | New JSON proxy log path; existing files and links are rejected. |
| `--dry-run` | Print the child-process plan without starting a proxy or agent. Static model metadata is resolved; catalog-only metadata is marked unresolved. |
| `--no-summary` | Suppress the end-of-session request/token summary. |

```bash
vekil launch codex --dry-run
vekil launch codex --model gpt-5.4-mini --dry-run
```

For Claude Code and Codex CLI without `--model`, dry-run describes model
selection as `CLI default`. It does not invent a concrete model or perform
model-specific checks. GitHub Copilot CLI dry-run still requires `--model`.

Dry-run never contacts provider model catalogs. For a pinned model declared in
the providers file, Vekil uses its configured endpoint and capability metadata
and rejects an incompatible target exactly as a real launch would. For a pinned
catalog-discovered model, endpoint compatibility and endpoint-dependent child
settings are printed as `unresolved` instead of being guessed.

## Claude Code

```bash
vekil launch claude -- --allowed-tools Read

vekil launch claude --model claude-opus-5 -- \
  --resume 56d7498d-6b2e-47ca-92a6-92ddee84ab25

vekil launch claude --model claude-sonnet-4.5 -- \
  --allowed-tools "Read,Bash(git status:*)"
```

The launcher supplies an owner-only temporary `--settings` overlay so user or
project settings cannot replace its gateway or token. When `--model` is
supplied, the overlay also pins model selection. The file is removed during
cleanup. Its routing values include:

- `ANTHROPIC_BASE_URL`;
- a random `ANTHROPIC_AUTH_TOKEN`;
- an empty `ANTHROPIC_API_KEY` to avoid conflicting persisted API-key auth;
- when pinned, `ANTHROPIC_MODEL`, the default Haiku, Sonnet, Opus, and Fable
  aliases, `CLAUDE_CODE_SUBAGENT_MODEL`, and the custom model-picker entry;
- host-managed provider and subprocess-environment scrubbing flags;
- disabled background-task, agent-view, cron, agent-team, tmux, and remote-control surfaces.

Provider-specific Bedrock, Vertex, Foundry, Mantle, AWS, OAuth, and custom-header
values are removed from the child environment. Gateway catalog discovery is
disabled. With `--model`, the proxy independently rejects requests for any model
other than the selected public model. Without `--model`, Claude resolves its
normal configured or built-in default and the proxy accepts requests across the
global public model namespace.

Subprocess credential scrubbing keeps Claude in its default (`manual`)
permission mode. Current Claude Code releases ignore other explicit permission
modes when that hardening is enabled, so Vekil rejects
`--dangerously-skip-permissions`, `--allow-dangerously-skip-permissions`, and
non-`manual` `--permission-mode` values instead of silently weakening or
misrepresenting the requested policy. Use `--allowed-tools` and
`--disallowed-tools` for managed non-interactive tool permissions.

Policy-routing public model IDs (`owned_by: vekil-policy`) are supported in
pinned mode. Vekil translates Claude's Messages and count-token requests to the
same canonical Chat request used by OpenAI Chat policy planning, then translates
the selected terminal response back to Anthropic. The policy profile remains
advertised as native `/chat/completions` metadata; the launcher compatibility
path does not change terminal endpoint ownership.

Claude's `output_config.effort` is carried into canonical Chat
`reasoning_effort`. Direct routes preserve supported client effort. A policy
profile never advertises client-selectable reasoning effort: an incoming value
is accepted only when both tier objects configure `reasoning_effort`, and the
effective `lightweight` or `powerful` value then replaces it. If both
tier objects omit effort, a valid present effort is rejected as unsupported and
an unset effort proceeds without a policy-owned override. Malformed or blank
effort fails locally before policy execution. Current Claude releases
may emit effort even when the user did not pass `--effort`, so managed Claude
policy launches that need to accept that wire field should configure both tier
values.

When a selected policy profile targets a configured `copilot` provider, the
ephemeral launch proxy authenticates and sends those terminal/classifier Chat
requests to Copilot in process. No separately started loopback bridge is
required.

The default policy mode follows the profile's YAML `mode`; an explicit
`--policy-routing off|observe|enforce` value can still lower the process-wide
ceiling. Locally owned `call_vekil_*` continuations retain their originating
route and tier, including in `enforce`. Opaque continuations from a downstream
Responses-backed bridge are accepted only for a single-target baseline in
`off` or `observe`, and that bridge must be one process (the normal loopback
launcher topology) or use sticky ingress to the replay-owning replica.

Foreground conversation selection, continuation, and fork arguments are
forwarded. Settings-source, detached or remote session, model/fallback,
incompatible permission-mode, and custom-agent overrides remain rejected. Use
the launcher-level `--model` to pin a model; otherwise model selection remains
delegated to Claude's normal default.

## OpenAI Codex CLI

```bash
vekil launch codex -- resume --last

vekil launch codex -- \
  exec --ephemeral --skip-git-repo-check "Reply with exactly OK"

vekil launch codex --model gpt-5.4-mini -- \
  exec --ephemeral --skip-git-repo-check "Reply with exactly OK"
```

Codex's built-in OpenAI provider does not use a generic base-URL override for
this flow. Vekil instead injects a transient, per-launch
`model_providers.<random-id>` definition through Codex `-c` arguments:

- `base_url` points to the ephemeral proxy's `/v1` root;
- `wire_api="responses"` selects `POST /v1/responses`;
- `env_key="VEKIL_CODEX_API_KEY"` reads the random local bearer token;
- `requires_openai_auth=false` prevents Codex/ChatGPT login fallback;
- `supports_websockets=false` keeps the launcher on deterministic HTTP Responses;
- for a policy-owned Chat model, Vekil disables hosted web search, remote
  compaction, Responses Lite, and code-only tool modes; removes Codex's freeform `apply_patch` declaration; and translates
  stateless Responses messages, bounded `text.format` structured-output schemas,
  and function/namespace tools through canonical Chat; namespace children receive
  deterministic Chat-safe aliases and are restored in Responses function-call
  output;
- an additive `shell_environment_policy.set` override masks the local token in
  model-invoked shells without replacing user-configured exclusions; upstream
  credential names are removed from the Codex process environment.


Pinned policy-owned Codex turns require `store: false` (Codex's launcher
default), no `previous_response_id`, text input, optional bounded structured
output via `codex exec --output-schema`, and function or namespace-child function
tools. Hosted/custom tools, images, the Responses websocket, compact/memory
routes, deferred tool discovery (`defer_loading` / `tool_search`), and provider-specific speed tiers remain unsupported. The launcher
disables the hosted/custom features it controls. As with Copilot and Claude,
opaque downstream replay IDs can continue only in `off` or `observe` with one
baseline target and one downstream bridge process (or sticky ingress to its
replay-owning replica); `enforce` tool continuations remain rejected without
affinity.
Large direct Codex tool catalogs require a downstream Chat bridge that accepts
more than the native OpenAI/Azure 128-function ceiling; operators using a
narrower terminal must reduce the enabled client catalog.

Policy-owned models never advertise client-selectable reasoning effort. If
Codex sends `reasoning.effort`, Vekil accepts it only when both profile tiers
configure `reasoning_effort`; the effective selected tier value then replaces
it. If both tiers omit effort, a valid present value is rejected as unsupported
and an unset value remains unset. Malformed or blank effort fails locally before
policy execution.

With `--model`, a private temporary one-model Codex catalog is generated from
the installed CLI's bundled catalog, with Vekil model context, reasoning,
vision, and tool metadata overlaid when advertised by `/v1/models`. This keeps
the picker and reasoning controls scoped to the selected route without editing
`~/.codex/config.toml`. The catalog is removed during cleanup.

Without `--model`, the launcher does not add a Codex `-m` override or narrow the
proxy namespace. Codex resolves the model from its normal configuration or
built-in default, and that public model ID is sent through the transient Vekil
provider. The selected default must exist in Vekil's global public namespace and
support `/responses`. Because the launcher cannot know the choice in advance,
an unknown or incompatible default fails when Codex requests it rather than
during launcher startup.

Foreground `resume`, `fork`, and `exec resume` commands are forwarded.
Forwarded `-m`/`--model`, `-c`/`--config`, profile, OSS/local-provider, remote,
app-server, remote-control, cloud, and non-agent command modes remain rejected.
For a pinned policy-owned model, forwarded `--search`, equivalent web-search
feature toggles, and attempts to re-enable `code_mode`, `code_mode_only`, or
`remote_compaction_v2` are also rejected so the compatibility safeguards remain
authoritative. Use the launcher-level `--model` to pin a model; otherwise
selection remains delegated to Codex's normal default.

## GitHub Copilot CLI

```bash
vekil launch copilot --model gpt-5.4-mini -- --continue

vekil launch copilot --model gpt-5.4-mini -- \
  --allow-all-tools -p "Reply with exactly OK" -s
```

`--model` is required because Copilot CLI's custom-provider/BYOK mode needs an
explicit model ID. The launcher uses that mode as follows:

- `COPILOT_PROVIDER_BASE_URL` points to the ephemeral proxy's `/v1` root;
- `COPILOT_PROVIDER_TYPE=openai`;
- `COPILOT_PROVIDER_BEARER_TOKEN` carries the random local token;
- `COPILOT_PROVIDER_API_KEY` is empty;
- `COPILOT_MODEL`, provider model ID, and wire model are pinned to the selected route;
- `COPILOT_PROVIDER_WIRE_API=responses` when `/responses` is available,
  otherwise `completions` for `/chat/completions` models;
- advertised prompt/output limits are forwarded when available;
- `COPILOT_PROVIDER_TRANSPORT=http` prevents inherited websocket selection;
- `COPILOT_OFFLINE=true` disables GitHub authentication, telemetry, web tools,
  GitHub MCP, and auto-update network activity.

The command line also disables auto-update, remote control/export, and built-in
MCPs. `--secret-env-vars` removes the local token plus provider and GitHub
credential names from shell and MCP subprocesses. Inherited GitHub, OpenAI,
Anthropic, Azure, provider, and telemetry-header credentials are removed.

Foreground local resume and continue arguments are forwarded. Forwarded model,
custom-agent, connect/session-ID, remote/export/share, ACP-server, and non-agent
subcommands remain rejected. Copilot's offline launch mode cannot resume remote
task IDs even though `--resume` accepts task IDs as well as local session IDs.

## Logs and statistics

Proxy JSON logs are written to:

```text
~/.config/vekil/logs/launch-<target>-<timestamp>-<pid>.jsonl
```

The startup banner prints the exact proxy URL and log path. Routes other than
`/healthz` and `/readyz` require the random session token, so the ordinary
browser dashboard is not exposed. The end-of-session summary is written to
stderr so non-interactive agent output on stdout remains pipeline-safe.

If Codex reports an upstream `408 user_request_timeout`, see
[Troubleshooting](troubleshooting.md#408-user_request_timeout-timed-out-reading-request-body)
for retry and diagnosis guidance.

## Credential and process isolation

The child receives a sanitized copy of the parent environment. Vekil removes:

- every `api_key_env` named by the active provider configuration;
- common Azure Identity secret variables when Entra authentication is active;
- target-specific upstream, OAuth, GitHub, and provider credential variables.

Only a random loopback session token is passed to the agent. In pinned mode it
is restricted server-side to the selected public model. In delegated Claude or
Codex mode it can reach the proxy's normal global public model namespace. It
grants no direct upstream access in either mode; real provider credentials
remain owned by the proxy process. Temporary files use mode `0600` on Unix and
an owner-only protected ACL on Windows.

The launcher appends the loopback host, `localhost`, `127.0.0.1`, and `::1` to
both `NO_PROXY` and `no_proxy`. Existing proxy configuration is preserved, but
agent traffic to Vekil cannot be routed through an inherited corporate or
development proxy.

Interactive macOS and Linux launches hand the terminal to a dedicated agent
process group while Vekil mirrors stop/continue state to the shell-visible job,
so `Ctrl-Z` and `fg` continue to work. Non-interactive launches use the same
process-group containment. Windows uses a kill-on-close Job Object. Remaining
processes in the managed group or Job Object are terminated during cleanup.
On Unix, a descendant that deliberately creates a new session or process group
can leave that portable containment boundary; Vekil rejects known detached
agent modes but is not an operating-system sandbox.

This is environment and process isolation, not a filesystem sandbox. Agents
retain their normal workspace and home-directory access according to their own
permission and sandbox settings.

## Containers

Agent launchers are intended for the native Vekil binary. The distroless Vekil
container does not contain agent executables or a shell and is not a launcher
environment.

Launcher process containment is supported on macOS, Linux, and Windows. Other
operating-system targets reject launcher startup rather than running with only
immediate-child supervision.
