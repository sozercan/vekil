# Agent Launchers

Vekil can start a short-lived loopback proxy and run a supported coding agent
against it in one command. The proxy uses an OS-selected port by default,
authenticates the child with a random per-launch token, restricts routing to the
selected public model, prints a request/token summary, and stops when the agent
exits.

## Supported targets

| Target | Command | Minimum tested contract | Required model endpoint |
|--------|---------|-------------------------|-------------------------|
| Claude Code | `vekil launch claude` | Claude Code 2.1.83+ | `/v1/messages`, `/chat/completions`, or `/responses` |
| OpenAI Codex CLI | `vekil launch codex` | Codex CLI 0.137.0+ | `/responses` |
| GitHub Copilot CLI | `vekil launch copilot` | GitHub Copilot CLI 1.0.0+ | `/responses` or `/chat/completions` |

```bash
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
vekil launch codex --model gpt-5.4-mini -- \
  exec --ephemeral --skip-git-repo-check "Review this workspace"

vekil launch copilot --model gpt-5.4-mini -- \
  --allow-all-tools -p "Review this workspace" -s
```

Each launcher reserves model, provider, remote-session, resume, and other
arguments that could replace the validated routing layer or leave work running
outside the ephemeral proxy lifecycle.

## Lifecycle

Every launcher:

1. loads Vekil provider configuration and credentials;
2. starts a proxy on `127.0.0.1:0` unless `--port` is provided;
3. completes startup authentication and provider-model validation;
4. initializes configured policy routing using the `--policy-routing` / `POLICY_ROUTING_MODE` safety ceiling;
5. waits for `/readyz` and verifies that `--model` appears in `/v1/models`;
6. validates the installed agent binary and model endpoint compatibility;
7. starts the agent with target-specific temporary routing configuration;
8. forwards signals and supervises the managed child process group or Job Object;
9. prints a usage summary and stops the proxy when the agent exits.

The ephemeral proxy scopes policy-controller construction and live classifier preflight to the selected public model (including its configured aliases). Unrelated policy profiles cannot require credentials, send classifier preflights, or fail startup for that managed session.

## Common options

| Flag | Purpose |
|------|---------|
| `--model ID` | Required public Vekil model ID. |
| `--providers-config PATH` | JSON or YAML provider configuration. |
| `--token-dir PATH` | Copilot token storage directory used by Vekil. |
| `--port PORT` | Local proxy port. Default `0` lets the OS allocate one. |
| `--binary PATH` | Agent executable override. |
| `--startup-timeout DURATION` | Maximum startup/authentication/readiness time. |
| `--streaming-upstream-timeout DURATION` | Upstream streaming timeout. |
| `--policy-routing off|observe|enforce` | Process-wide policy-routing safety ceiling. Defaults to `POLICY_ROUTING_MODE` or `off`. |
| `--proxy-log PATH` | New JSON proxy log path; existing files and links are rejected. |
| `--dry-run` | Print the child-process plan without starting a proxy or agent. Static model metadata is resolved; catalog-only metadata is marked unresolved. |
| `--no-summary` | Suppress the end-of-session request/token summary. |

```bash
vekil launch codex --model gpt-5.4-mini --dry-run
```

Dry-run never contacts provider model catalogs. When `--model` is declared in
the providers file, Vekil uses its configured endpoint and capability metadata
and rejects an incompatible target exactly as a real launch would. For
catalog-discovered models, endpoint compatibility and endpoint-dependent child
settings are printed as `unresolved` instead of being guessed.

## Claude Code

```bash
vekil launch claude --model claude-sonnet-4.5 -- \
  --dangerously-skip-permissions
```

The launcher supplies an owner-only temporary `--settings` overlay so user or
project settings cannot replace its gateway, token, or model. The file is
removed during cleanup. Its routing values include:

- `ANTHROPIC_BASE_URL`;
- a random `ANTHROPIC_AUTH_TOKEN`;
- an empty `ANTHROPIC_API_KEY` to avoid conflicting persisted API-key auth;
- `ANTHROPIC_MODEL` and the default Haiku, Sonnet, Opus, and Fable aliases;
- `CLAUDE_CODE_SUBAGENT_MODEL` and the custom model-picker entry;
- host-managed provider and subprocess-environment scrubbing flags;
- disabled background-task, agent-view, cron, agent-team, tmux, and remote-control surfaces.

Provider-specific Bedrock, Vertex, Foundry, Mantle, AWS, OAuth, and custom-header
values are removed from the child environment. Gateway catalog discovery is
disabled, and the proxy independently rejects requests for any model other than
the selected public model.

Policy-routing public model IDs (`owned_by: vekil-policy`) are rejected for
Claude Code even though they advertise `/chat/completions`: policy routing is
not supported on the Anthropic ingress used by this launcher. Use
`vekil launch copilot` for those Chat-completions policy models. Direct models
with `/v1/messages`, `/chat/completions`, or `/responses` remain supported.

Forwarded settings-source, detached-session, resume, model/fallback, and custom
agent overrides are rejected.

## OpenAI Codex CLI

```bash
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
- an additive `shell_environment_policy.set` override masks the local token in
  model-invoked shells without replacing user-configured exclusions; upstream
  credential names are removed from the Codex process environment.

A private temporary one-model Codex catalog is generated from the installed
CLI's bundled catalog, with Vekil model context, reasoning, vision, and tool
metadata overlaid when advertised by `/v1/models`. This keeps the picker and
reasoning controls scoped to the selected route without editing
`~/.codex/config.toml`. The catalog is removed during cleanup.

Forwarded `-m`/`--model`, `-c`/`--config`, profile, OSS/local-provider, remote,
resume/fork, app-server, remote-control, cloud, and non-agent command modes are
rejected.

## GitHub Copilot CLI

```bash
vekil launch copilot --model gpt-5.4-mini -- \
  --allow-all-tools -p "Reply with exactly OK" -s
```

The launcher uses Copilot CLI's custom-provider mode:

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

Forwarded model, custom-agent, secret-environment, connect/resume/session,
remote/export/share, ACP-server, and non-agent subcommands are rejected.

## Logs and statistics

Proxy JSON logs are written to:

```text
~/.config/vekil/logs/launch-<target>-<timestamp>-<pid>.jsonl
```

The startup banner prints the exact proxy URL and log path. Routes other than
`/healthz` and `/readyz` require the random session token, so the ordinary
browser dashboard is not exposed. The end-of-session summary is written to
stderr so non-interactive agent output on stdout remains pipeline-safe.

## Credential and process isolation

The child receives a sanitized copy of the parent environment. Vekil removes:

- every `api_key_env` named by the active provider configuration;
- common Azure Identity secret variables when Entra authentication is active;
- target-specific upstream, OAuth, GitHub, and provider credential variables.

Only a random loopback session token is passed to the agent. It is restricted
server-side to the selected public model and grants no direct upstream access.
Real provider credentials remain owned by the proxy process. Temporary files use
mode `0600` on Unix and an owner-only protected ACL on Windows.

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
