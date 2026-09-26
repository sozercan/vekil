# Local AIKit Models

Vekil can run an [AIKit](https://github.com/kaito-project/aikit) model image as a
local container and route to it like any other provider. AIKit images bundle
LocalAI with model weights and serve an OpenAI-compatible API, so Vekil only
starts the container, points a provider at its loopback port, and removes the
container when it is done.

There are two ways to use it:

- `vekil launch <agent> --model aikit:<ref>` starts a model for one coding-agent
  session.
- A `type: aikit` provider in a providers config starts a model for `vekil
  serve`, the tray app, or a launch that uses that config.

The proxy never manages containers itself. The CLI starts them before the proxy
is constructed and rewrites each one into a static `openai-compatible` provider
with `upstream_dialect: localai`.

## References

| Form | Example | Served by |
|------|---------|-----------|
| Pre-made name | `qwen3.8:27b` | `ghcr.io/kaito-project/aikit/<name>:<tag>`, or its `applesilicon/` variant on a podman libkrun machine |
| Image reference | `ghcr.io/org/model:v1`, `localhost/model:dev` | that image |
| Image with several models | `ghcr.io/org/multi:v1#chat-model` | the named model from the image config |
| GGUF URL | `https://example.com/model-Q4_K_M.gguf` (https only; no query string, since the runner receives the URL on its command line; not `localhost`, which the runner container cannot reach) | a runner image that downloads the file at startup |
| Hugging Face GGUF file | `hf.co/unsloth/Qwen3.5-0.8B-GGUF/Qwen3.5-0.8B-Q3_K_S.gguf` | the same, on the repository's `main` revision |
| Hugging Face repository | `hf.co/Qwen/Qwen3-0.6B@<40-hex commit>` | the vllm.cpp runner; refused for now (see [Backends](#backends)) |

Pre-made names need a tag. A full image reference must start with a registry
host (`ghcr.io/…`, `localhost/…`, `registry:5000/…`).

## Launching an agent

```bash
vekil launch claude  --model aikit:qwen3.8:27b
vekil launch copilot --model aikit:devstral-small2:24b --context-size 98304
vekil launch codex   --model aikit:gpt-oss:20b -- exec "Summarize this repository"
vekil launch claude  --model aikit:hf.co/unsloth/Qwen3.5-0.8B-GGUF/Qwen3.5-0.8B-Q3_K_S.gguf
vekil launch claude  --model aikit:qwen3.8:27b --dry-run
```

| Flag | Purpose |
|------|---------|
| `--context-size N` | Context window. Default: 65536 capped at the model's trained context. |
| `--runtime auto\|docker\|podman` | Container engine. Default `auto`; see [Container engines](#container-engines). |
| `--backend llama-cpp\|vllm-cpp` | Backend for a runner reference. Default: `llama-cpp` for GGUF files, `vllm-cpp` for repositories. |
| `--keep` | Leave the container running after the agent exits and reuse it on the next launch with the same model, backend, and context. |
| `--load-timeout D` | Maximum time for the model to load, including out-of-memory retries. Default `10m`. Pulling is not counted. |

These flags require `--model aikit:<ref>`. Without a providers config the launch
serves only the local model, so no GitHub authentication is needed. With
`--providers-config`, the local model is added to that configuration as
provider `aikit` and must not collide with its public model IDs.

A launch:

1. detects a container engine and pulls the image if it is missing;
2. reads `/config.yaml` and the GGUF header out of an unstarted container (or,
   for a GGUF URL, reads the header with HTTP range requests) to learn the
   served model name and trained context;
3. chooses the context and starts the container with
   `LOCALAI_CONTEXT_SIZE` and `LOCALAI_LOAD_TO_MEMORY`, publishing port 8080 on
   `127.0.0.1` only;
4. waits until `/readyz` reports the model loaded, retrying at half the context
   if the load runs out of memory;
5. verifies the served context through `/api/models/config-json/<model>`;
6. starts the normal launcher flow pinned to the served model name;
7. removes the container when the agent exits, unless `--keep` was given.

Each agent is told the context actually served:

- Claude Code receives `CLAUDE_CODE_MAX_CONTEXT_TOKENS`, so it compacts before
  it overflows the local server.
- GitHub Copilot CLI receives the prompt limit through its provider settings.
- Codex receives `context_window` in its generated model catalog. Because
  LocalAI serves only function tools, the launcher also turns off hosted web
  search, code mode, and remote compaction, and removes the freeform
  `apply_patch` tool; Codex can still edit files through its shell tool.

Local prompt processing is much slower than a hosted model. Measured on
qwen3.5:2b on an Apple Silicon GPU through podman: Claude Code's first request
in a repository with a long `CLAUDE.md` was 39k tokens, a Codex session with no
MCP servers was 13k tokens, and a Codex session with 331 MCP and app tools was
118k tokens and took about 9.5 minutes before the first output token. Trim MCP
servers, or raise `--context-size` when a request is rejected as too long.

## Container engines

With `--runtime auto`:

- **macOS on Apple Silicon:** podman is used when a podman machine using the
  `libkrun` provider is running, because that is the only way AIKit images reach
  the GPU (`--device /dev/dri` and the `applesilicon/` image). Otherwise Vekil
  uses Docker without GPU acceleration and prints why, for example that podman
  is missing, no machine is running, or the machine uses `applehv`. Create a GPU
  machine with `CONTAINERS_MACHINE_PROVIDER=libkrun podman machine init --now`.
- **Linux and Windows:** Docker if it answers, otherwise podman. When
  `nvidia-smi` lists a GPU, containers get `--gpus all` (Docker) or
  `--device nvidia.com/gpu=all` (podman with CDI).

On macOS, where the engine runs in a VM (podman machine or Docker Desktop),
Vekil fails before starting a container whose weights alone exceed 90% of the
VM's memory and prints the command that raises it.

Pre-made models without an `applesilicon/` image fall back to the standard
image, which runs on the CPU. Runner images have no Apple Silicon build and
always run on the CPU there.

## Context size

LocalAI applies `LOCALAI_CONTEXT_SIZE` only when the model's config leaves
`context_size` unset, and otherwise caps an unset context at 8192 tokens.
llama.cpp can size a context to free memory only when it is given `0`, which
LocalAI never sends. Vekil therefore always chooses a value:

- `--context-size` (or `aikit.context_size`) if set; it must not exceed the
  trained context and is never reduced automatically.
- Otherwise 65536, capped at the model's trained context.
- Agent launches refuse anything below 49152 tokens, because the agent's first
  request would not fit. A model trained for less, such as phi-4 (16k), is
  refused before any container starts.
- If a default-sized load runs out of memory, the container is restarted at half
  the context, halving again on each failure down to that floor (8192 tokens
  for a `type: aikit` provider).
- An image whose config fixes `context_size` is used at that size, with a
  warning. A fixed size below the floor, or one that conflicts with
  `--context-size`, is refused; rebuild the image without `context_size`.

On a GPU, llama.cpp keeps an explicit context and moves layers to the CPU when
memory is short. Vekil reads the load log and warns when only some layers fit
on the GPU; a smaller `--context-size` keeps generation fast.

## Providers configuration

```yaml
schema_version: 2
providers:
  - id: copilot
    type: copilot
  - id: local
    type: aikit
    aikit:
      model: qwen3.8:27b
      context_size: 65536   # optional
      runtime: podman       # optional: auto, docker, podman
      backend: llama-cpp    # optional, runner references only
      keep: false           # optional
      load_timeout: 15m     # optional
    models:                 # optional
      - public_id: local-qwen
```

| Field | Purpose |
|-------|---------|
| `aikit.model` | Required reference, without the `aikit:` prefix. |
| `aikit.context_size` | Explicit context window. |
| `aikit.runtime` | Container engine. |
| `aikit.backend` | Runner backend. |
| `aikit.keep` | Leave the container running when Vekil exits. |
| `aikit.load_timeout` | Go duration bounding the model load. |
| `models` | Public IDs for the served model. `deployment` is always the model the container loads; omitted `endpoints` and `context_window` are filled from the running container. |

Vekil owns the connection, so `base_url`, authentication fields, endpoint
paths, `model_discovery`, model filters, `hosted_tools`, `upstream_dialect`, and
`models[].deployment` are rejected on an aikit provider. Without `models`, the provider exposes the served model name
(for example `qwen-3.8-27b`), unless routes reference the provider, in which
case the routes define its public contract; a route that leaves
`context_window` unset gets the served context. A route target's `upstream_model`
must be the served model name, or startup fails after the container loads. The agent context floor does not
apply here; set `context_size` for sessions that need more than the default.

- The server (`vekil --providers-config`), the tray app, and `vekil launch --providers-config` start aikit
  providers before the proxy and remove them on shutdown.
- `vekil config validate` checks aikit providers offline, including their model
  references. `--live` starts the containers for the duration of the check.
- A proxy constructed from a config that still contains `type: aikit` fails at
  startup instead of serving an unstarted provider.

## LocalAI dialect

`upstream_dialect: localai` on an `openai-compatible` provider corrects LocalAI
behaviors that would otherwise fail silently or confuse clients. AIKit
providers set it automatically; set it yourself for a LocalAI server you run,
declaring its models statically (`model_discovery: static`).

- **Function tools only.** LocalAI's Responses API drops non-function tools and
  unknown input items without an error. Vekil rejects hosted and custom tools,
  and items other than `message`, `function_call`, `function_call_output`,
  `reasoning`, and `item_reference`, with a `400`. Namespace tools are flattened
  into function tools named `<namespace>__<tool>`, and their calls are restored
  to the original namespace and name in responses and stream events.
- **One leading system message.** Many local chat templates, including Qwen's,
  reject a second system message or one after the first turn. Vekil merges
  leading system and developer messages into one, turns later ones into user
  messages wrapped in `<system-reminder>`, and folds Responses developer
  messages into `instructions`.
- **Context overflow.** LocalAI reports a prompt that exceeds the context as an
  HTTP `500`, and while streaming it sends the error text as assistant content
  before an error event. Vekil holds a stream until its first meaningful event
  and turns either form into a `400` with code `context_length_exceeded`.
  Anthropic clients receive `prompt is too long: N tokens > M maximum`, which
  Claude Code uses to compact.

## Local-first failover

A schema-version-2 route can fall back from a local model to a hosted one when
a prompt does not fit the local context:

```yaml
model_routes:
  - id: coder
    public_id: coder
    endpoints: [/chat/completions]
    targets:
      - {id: local, provider: local, upstream_model: qwen-3.8-27b}
      - {id: cloud, provider: copilot, upstream_model: gpt-5.4-mini}
    routing:
      mode: priority_failover
      max_target_attempts: 2
      max_upstream_sends: 3
      failover_on_context_overflow: true
```

See [Provider Routing](provider-routing.md#context-overflow-failover) for the
exact rule.

## Backends

- **llama-cpp** serves GGUF models, both in pre-made images and in the
  `runners/llama-cpp-cpu` and `runners/llama-cpp-cuda` runners.
- **vllm-cpp** sizes its KV cache pool with the `num_blocks` model option
  (default 256 blocks of 32 tokens, 8192 tokens total), which LocalAI reads only
  from a model config. `LOCALAI_CONTEXT_SIZE` raises the per-request limit but
  not the pool, so Vekil refuses vllm.cpp runner references and accepts a
  vllm.cpp image only when its config sets both `context_size` and a pool large
  enough to hold it.

## Containers, caches, and credentials

- Containers are named `vekil-aikit-<id>` and labeled `dev.vekil.aikit`. A later
  Vekil run removes containers left by a Vekil process on the same host that is
  no longer running, except kept ones, and does not start a model while one of
  them cannot be removed. A long-running process, such as the tray app, also
  retries its own failed removals before it starts another model. Remove a kept container with
  `docker rm -f` or `podman rm -f`. A start that finds a matching kept container
  running but not ready, for example still loading for another launch, fails
  instead of loading a second copy.
- Runner references mount a named volume `vekil-aikit-<hash>` at `/models`, so a
  downloaded model is reused. Remove it with `docker volume rm` or `podman
  volume rm`.
- `HF_TOKEN` is used only for Hugging Face sources (`hf.co/…` and
  `huggingface.co` URLs), for gated models. It is passed to runner containers
  by name, so its value never appears on a command line.
