# Client Usage Examples

These examples all target the same local proxy. Replace model IDs with public IDs from `/v1/models` in your deployment; client setup does not need to change when a model is backed by GitHub Copilot, Azure OpenAI, OpenAI Codex, or a generic compatible provider.

## Schema-v2 policy profile IDs

A semantic policy profile such as `coding-economy` is a narrower public model contract than a direct model ID. It accepts text/function-tool OpenAI Chat, translated Anthropic Messages and count-token probes, and bounded stateless OpenAI Responses—including structured text output—for Responses-only agents such as Codex. It is not accepted by Gemini routes/CLI, the Responses websocket bridge, compact/memory endpoints, images, hosted/custom tools, or stateful `previous_response_id` requests. Policy destinations may use native Chat or bounded Responses-backed Chat. `call_vekil_*` continuations are still best served by the process that owns their replay state: reasoning continuity lives there, and so does policy-tier pinning, whose route tag is keyed per process. For a policy profile that affinity is a requirement, not a preference, and on every surface: the tier is recovered from a carrier tag keyed per process, so on a replica holding none of that state no tier can be selected and the request fails with `responses_replay_state_missing` — in the planner, before Anthropic ingress could fall back. The ID-only rebuild that does survive a replica is a direct-model-route behaviour over Anthropic Messages, not a policy one. Policy continuations need sticky routing to the owning replica. Direct in-process Copilot targets satisfy the affinity requirement; an external downstream bridge, whose continuation IDs are opaque to Vekil and carry nothing, still requires a single instance or sticky ingress to the owning replica.

Chat clients and the supported translated agent surfaces may request a policy profile. The returned Chat JSON/SSE model identity remains the policy public ID; internal lightweight/powerful provider and route IDs are not a client contract. Exposed direct terminal routes and other public models remain independently requestable, so use `exposure: internal` when bypass must not be offered.

```bash
curl http://localhost:1337/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "coding-economy",
    "messages": [{"role": "user", "content": "Review this function"}]
  }'
```

The profile follows its YAML `mode` by default. `--policy-routing` / `POLICY_ROUTING_MODE` can explicitly lower every profile to `off` or `observe` for rollout and rollback. See [Semantic Policy Routing](policy-routing.md) before enabling observe or enforce.

## Responses-native models on Chat-compatible clients

A model whose native catalog endpoint is only `/responses` can still serve OpenAI Chat Completions, Claude Code through Anthropic Messages, and Gemini-compatible generation through Vekil's Chat-over-Responses adapter. Native Chat is preferred when a model supports both. The model catalog intentionally remains native, so a Responses-backed model can continue to show only `/responses` in `supported_endpoints`; this does not mean the documented Vekil compatibility routes are unavailable.

The adapter is strict. It supports the mapped Chat field subset in [API Reference](api.md#responses-backed-chat-request-subset), rejects unknown fields instead of dropping them, does not emulate non-empty stop sequences, and accepts only function tools. Hosted tools such as web search, computer use, code execution, and image generation are not supported through Chat compatibility. Direct `/v1/responses` clients remain on the native path.

Tool continuations use one of two minted ID shapes, including Anthropic `tool_use.id` and Gemini `functionCall.id`: `call_vekil_v1_<15-character-nonce>_<upstream-call-id>_<4-character-checksum>`, a self-describing form whose total length never exceeds 64 characters, or `call_vekil_<22-character-base64url>`, the opaque fallback when the upstream ID cannot be embedded. The versioned form exposes the upstream provider's call ID. Its checksum distinguishes Vekil-minted IDs from plausible native IDs but is not an authorization key. Return either ID unchanged, and do not construct, edit, or parse it.

Preserve the complete assistant tool-call list in its original order; tool-result messages for a complete parallel group may be returned in any order. When only a non-empty subset of results is available, Vekil replays only the matching calls because the verified upstream rejected a complete call group paired with partial outputs; missing calls may be reissued.

Replay state is byte-bounded, process-local, and expires one hour after the tool-call response. It is lost on restart. If a continuation receives `responses_replay_state_missing`, restart that assistant tool-call turn rather than inventing or editing the call ID.

Claude Code and Gemini `countTokens` calls also use the selected model's native backend. Anthropic counting requires reported upstream usage; Gemini counting retains its existing cache and can use a local estimate for transient failures or missing usage.

## Claude Code

Use [`vekil launch claude`](agent-launchers.md) to start an ephemeral proxy and Claude Code together, or configure an already-running proxy manually:

```bash
env ANTHROPIC_BASE_URL=http://localhost:1337 \
  ANTHROPIC_API_KEY=dummy \
  claude --model claude-sonnet-5 --print --output-format text "Reply with exactly PROXY_OK"
```

When zero-config Copilot rejects `advisor-tool-2026-03-01` as an unsupported beta header, disable Claude Code's experimental Advisor Tool for that invocation:

```bash
env CLAUDE_CODE_DISABLE_ADVISOR_TOOL=1 \
  ANTHROPIC_BASE_URL=http://localhost:1337 \
  ANTHROPIC_API_KEY=dummy \
  claude --model claude-sonnet-5 --print --output-format text "Reply with exactly PROXY_OK"
```

## OpenAI Codex CLI

Use [`vekil launch codex`](agent-launchers.md) for an ephemeral supervised session, or configure an already-running proxy manually. The manual example below uses the Responses API, so select a direct Responses-capable model rather than a v1 policy profile:

```bash
env OPENAI_API_KEY=dummy \
  codex exec --skip-git-repo-check -m gpt-5.5 \
  -c 'model_provider="openai"' \
  -c 'openai_base_url="http://localhost:1337/v1"' \
  "Reply with exactly PROXY_OK"
```

Codex resolves the built-in `openai` provider's endpoint from the `openai_base_url` config key. Set both `model_provider` and `openai_base_url` with `-c` as above, or persist both in `~/.codex/config.toml`; pinning the provider prevents an existing custom provider from bypassing Vekil. `OPENAI_BASE_URL` is not read: exporting it leaves Codex calling the upstream provider directly, with no error to indicate the proxy was bypassed. [`vekil launch codex`](agent-launchers.md) applies the same explicit routing with a generated provider: it selects that provider, sets `model_providers.<id>.base_url`, and clears `OPENAI_BASE_URL` from the child environment.

For this manual configuration, Codex's built-in `openai` provider probes the [Responses WebSocket bridge](responses-websocket.md) before falling back to HTTP streaming. When the bridge is disabled — the default — that probe logs a single `426 Upgrade Required` at startup and the session proceeds normally over HTTP. The managed launcher disables this WebSocket probe.

## GitHub Copilot CLI

Use [`vekil launch copilot`](agent-launchers.md) for an ephemeral offline/BYOK session, or configure an already-running proxy manually. The manual example below sets `COPILOT_PROVIDER_WIRE_API=responses`, so select a direct Responses-capable model. A v1 policy profile instead requires the completions wire API and a request within the v1 text/function-tool Chat contract:

```bash
env COPILOT_PROVIDER_BASE_URL=http://localhost:1337/v1 \
  COPILOT_PROVIDER_TYPE=openai \
  COPILOT_PROVIDER_WIRE_API=responses \
  COPILOT_MODEL=gpt-5.5 \
  COPILOT_OFFLINE=true \
  copilot -p "Reply with exactly PROXY_OK" -s
```

## Gemini CLI

Gemini endpoints do not accept v1 policy profile IDs; select a direct public model/route.

```bash
env GEMINI_API_KEY=dummy \
  GOOGLE_GEMINI_BASE_URL=http://localhost:1337 \
  GOOGLE_GENAI_API_VERSION=v1beta \
  GEMINI_CLI_NO_RELAUNCH=true \
  gemini -m gemini-2.5-pro -p "Reply with exactly PROXY_OK" -o json
```

## Anthropic Messages API

```bash
curl http://localhost:1337/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4",
    "max_tokens": 1024,
    "messages": [
      {"role": "user", "content": "Hello, world!"}
    ]
  }'
```

## Anthropic Messages API (Streaming)

```bash
curl http://localhost:1337/v1/messages \
  -H "Content-Type: application/json" \
  -d '{
    "model": "claude-sonnet-4",
    "max_tokens": 1024,
    "stream": true,
    "messages": [
      {"role": "user", "content": "Hello, world!"}
    ]
  }'
```

## OpenAI Chat Completions API

```bash
curl http://localhost:1337/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "messages": [
      {"role": "user", "content": "Hello, world!"}
    ]
  }'
```

## OpenAI Responses API

Use a direct Responses-capable public model. V1 policy profile IDs fail locally on this endpoint.

```bash
curl http://localhost:1337/v1/responses \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4o",
    "input": "Hello, world!"
  }'
```

## Gemini Generate Content API

```bash
curl http://localhost:1337/v1beta/models/gemini-2.5-pro:generateContent \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [
      {
        "role": "user",
        "parts": [{"text": "Hello, world!"}]
      }
    ]
  }'
```

## Gemini Stream Generate Content API

```bash
curl -N http://localhost:1337/v1/models/gemini-2.5-pro:streamGenerateContent \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [
      {
        "role": "user",
        "parts": [{"text": "Stream a short answer"}]
      }
    ]
  }'
```

`-N` disables curl buffering so SSE chunks print as they arrive. The proxy accepts Gemini routes under `/v1beta/models`, `/v1/models`, and `/models`.

## Gemini Count Tokens API

```bash
curl http://localhost:1337/v1beta/models/gemini-2.5-pro:countTokens \
  -H "Content-Type: application/json" \
  -d '{
    "contents": [
      {
        "role": "user",
        "parts": [{"text": "Count these tokens"}]
      }
    ]
  }'
```
